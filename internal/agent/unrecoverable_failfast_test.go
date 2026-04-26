package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// TestUnrecoverable_FailsFastWithoutRetry 是 §9.3 核心不变量的端到端护栏：
//
// **当 LLM 返回不可恢复错误（401/400/404 等）时：**
//   - 任务必须直接转 failed 终态
//   - **绝不**进入 RetryRollback / 不递增 RetryCount
//   - trace 必须 emit KindTaskFailed，且 reason 含诊断信息（不是裸 SDK 错误串）
//
// **历史背景**：spec §9.3 立项触发自 2026-04-20 实战事故——LLM 服务关机时 scheduler
// 无限重试 166+ 次（~25 分钟空转）。当时 ErrUnrecoverable 已分类正确，但 handleFailure
// 在某些路径下仍然回到 RetryRollback。
//
// 当前 handleFailure（agent.go:723-814）通过 errors.As(execErr, &ErrRecoverable) 二分
// 判定，凡不可恢复的（含 ErrUnrecoverable + 任何其他类型）一律走 terminateTask；
// 此前的 §9.3 修复早已生效，§9.4 诊断映射叠加在终止 reason 之上。
//
// 本测试是该路径的端到端不变量护栏：未来若有人重构 handleFailure 把
// errors.As 顺序写反、或在不可恢复分支增加 retry 早返点，本测试会立即变红。
//
// 单测层（diagnose_test.go）只验诊断字符串生成；客户端层（client_test.go）只验
// classifySDKError 状态码分类。**只有这条端到端测试守的是"装配握手位"——
// 三段联动起来必须仍然 fail-fast**。
//
// 负向自检：把 handleFailure 不可恢复分支的 terminateTask 临时替换为 RetryRollback，
// 本测试应红（task 状态变 pending、RetryCount 变 1）。
func TestUnrecoverable_FailsFastWithoutRetry(t *testing.T) {
	traceDir := setupTraceWriter(t)
	s, r, _ := setup()

	task := &model.Task{Description: "should fail fast", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-failfast", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	beforeStatus, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask before: %v", err)
	}
	if beforeStatus.RetryCount != 0 {
		t.Fatalf("前置条件不满足：RetryCount = %d, want 0", beforeStatus.RetryCount)
	}

	// 让 executor 返回 §9.3 列表里的不可恢复错误（401 invalid_api_key 是经典案例）。
	executor := func(ctx context.Context, tk *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, &llm.ErrUnrecoverable{
			Err:        errors.New("401 unauthorized"),
			StatusCode: 401,
			Code:       "invalid_api_key",
			Message:    "Incorrect API key provided",
			Endpoint:   "https://api.deepseek.com/v1",
		}
	}

	// MaxRetries=5 是个干扰项——若 fail-fast 失效退化到重试路径，RetryCount 会被
	// 推到 5 才终止；正确行为是 RetryCount 保持 0 直接 failed。
	ag := NewAgent("agent-failfast", "code", s, r, executor, 5)
	ag.MaxRetries = 5
	ag.processTask(context.Background(), task.ID)

	// 不变量 1：任务终态 = failed（不是 pending = RetryRollback 后的状态）
	after, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask after: %v", err)
	}
	if after.Status != model.TaskStatusFailed {
		t.Errorf("Status = %q, want %q（fail-fast 退化为 RetryRollback 的迹象）",
			after.Status, model.TaskStatusFailed)
	}

	// 不变量 2：RetryCount 未递增（RetryRollback 才会递增）
	if after.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0（不可恢复错误不应触发重试）", after.RetryCount)
	}

	// 不变量 3：trace 含 KindTaskFailed 事件
	events := p1fixesReadTraceEvents(t, traceDir)
	var failed *trace.Event
	var retried *trace.Event
	for i, ev := range events {
		if ev.TaskID != task.ID {
			continue
		}
		switch ev.Kind {
		case trace.KindTaskFailed:
			if failed == nil {
				failed = &events[i]
			}
		case trace.KindTaskRetry:
			if retried == nil {
				retried = &events[i]
			}
		}
	}
	if failed == nil {
		t.Fatalf("未 emit KindTaskFailed，事件: %s", eventKinds(events))
	}
	// 不变量 4：未 emit KindTaskRetry（出现该事件即说明走了 retry 路径）
	if retried != nil {
		t.Errorf("不应 emit KindTaskRetry——不可恢复错误的 retry 痕迹：%+v", retried)
	}

	// 不变量 5：reason 含 §9.4 诊断字符串（不是裸 SDK 错误串）。
	// invalid_api_key 路径下 diagnoseLLMError 应返回"API key 无效"开头的中文消息。
	if !strings.Contains(failed.Reason, "API key 无效") {
		t.Errorf("Reason 不含 §9.4 诊断字符串（说明 diagnoseLLMError 未被调用或 reason 透传裸错误）；\n  reason = %q",
			failed.Reason)
	}
	if strings.Contains(failed.Reason, "401 unauthorized") {
		t.Errorf("Reason 含裸 SDK 错误串——说明 reason 没经过 diagnoseLLMError 转换；\n  reason = %q",
			failed.Reason)
	}
}
