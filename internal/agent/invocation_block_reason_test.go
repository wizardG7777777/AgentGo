package agent

import (
	"errors"
	"strings"
	"testing"

	"agentgo/internal/invocation"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestBlockedInvocationTerminalPreservesFailureAuthority(t *testing.T) {
	contextFailure := invocation.NewFailure(
		invocation.FailureContextAssembly,
		invocation.PhaseRequestBuild,
		invocation.OriginRuntime,
		errors.New("fragment_limit_exceeded"),
	)
	reason, code := blockedInvocationTerminal(contextFailure, contextFailure)
	if code != "context_assembly_rejected" ||
		!strings.Contains(reason, "Context 装配被 L2 policy 拒绝") ||
		strings.Contains(reason, "deadline 阻断") {
		t.Fatalf("Context failure 被错误归因为 deadline: code=%q reason=%q", code, reason)
	}

	deadlineFailure := invocation.NewFailure(
		invocation.FailureAttemptDeadline,
		invocation.PhaseRequestSend,
		invocation.OriginCaller,
		invocation.ErrAttemptDeadline,
	)
	deadlineFailure.TimeoutScope = invocation.TimeoutAttempt
	reason, code = blockedInvocationTerminal(deadlineFailure, deadlineFailure)
	if code != "invocation_deadline" || !strings.Contains(reason, "deadline 阻断") ||
		!strings.Contains(reason, string(invocation.TimeoutAttempt)) {
		t.Fatalf("真正 deadline 未保留作用域: code=%q reason=%q", code, reason)
	}

	quotaFailure := invocation.NewFailure(
		invocation.FailureProviderQuotaExhausted,
		invocation.PhaseResponseHeaders,
		invocation.OriginProvider,
		errors.New("quota exhausted"),
	)
	quotaFailure.HTTPStatus = 402
	quotaFailure.ProviderCode = "invalid_request_error"
	reason, code = blockedInvocationTerminal(quotaFailure, quotaFailure)
	if code != "provider_quota_exhausted" || !strings.Contains(reason, "计费额度或余额已耗尽") ||
		!strings.Contains(reason, "status=402") {
		t.Fatalf("quota failure 未形成外部资源 blocked 原因: code=%q reason=%q", code, reason)
	}
}

func TestBlockedInvocationTerminalNilFailureIsGeneric(t *testing.T) {
	reason, code := blockedInvocationTerminal(nil, errors.New("unknown"))
	if code != "invocation_blocked" || !strings.Contains(reason, "恢复策略阻断") {
		t.Fatalf("nil failure generic fallback 错误: code=%q reason=%q", code, reason)
	}
}

func TestHandleFailurePersistsContextAssemblyWithoutDeadlineLabel(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{ID: "context-blocked", Description: "测试", EventType: "worker"}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{ID: "worker-1", Store: tasks}
	failure := invocation.NewFailure(
		invocation.FailureContextAssembly,
		invocation.PhaseRequestBuild,
		invocation.OriginRuntime,
		errors.New("fragment_limit_exceeded"),
	)
	agent.handleFailure(task, task.ID, failure, nil, nil)
	stored, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.TaskStatusBlocked ||
		!strings.Contains(stored.Error, "Context 装配被 L2 policy 拒绝") ||
		strings.Contains(stored.Error, "deadline 阻断") {
		t.Fatalf("持久化终态丢失 Context authority: %+v", stored)
	}
}

func TestHandleFailureBlocksProviderQuotaWithoutRetry(t *testing.T) {
	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := &model.Task{ID: "quota-blocked", Description: "测试", EventType: "worker"}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{ID: "worker-1", Store: tasks}
	failure := invocation.NewFailure(invocation.FailureProviderQuotaExhausted,
		invocation.PhaseResponseHeaders, invocation.OriginProvider, errors.New("quota exhausted"))
	failure.HTTPStatus, failure.ProviderCode = 402, "invalid_request_error"
	agent.handleFailure(task, task.ID, failure, nil, nil)
	stored, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.TaskStatusBlocked || stored.RetryCount != 0 ||
		!strings.Contains(stored.Error, "计费额度或余额已耗尽") {
		t.Fatalf("quota failure 必须 blocked 等待外部资源: %+v", stored)
	}
}
