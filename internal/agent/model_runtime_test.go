package agent

import (
	"agentgo/internal/runcontract"
	"context"
	"fmt"
	"testing"
	"time"

	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/gate"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/store"
	"agentgo/internal/testmodel"
	"github.com/google/uuid"
)

func newAgentTestContextRuntime(t *testing.T) contextruntime.Runtime { return testmodel.Runtime(t) }
func newTestSwappableLLMExecutor(t *testing.T, client llm.Invoker, tools *ToolRegistry, gates *gate.Registry, view store.StoreHookView, recorder func(string, store.ToolCallRecord), team string, prompt ...string) *LLMExecutor {
	system := ""
	if len(prompt) > 0 {
		system = prompt[0]
	}
	e := NewTurnExecutor(client, tools, gates, recorder, testmodel.Runtime(t), contextruntime.Instructions{System: system, Team: team})
	e.SetContextRuntime(testmodel.Runtime(t))
	e.SetPromptVersion("test-instructions")
	return e
}
func newTestLLMExecutor(t *testing.T, client llm.Invoker, tools *ToolRegistry, gates *gate.Registry, view store.StoreHookView, recorder func(string, store.ToolCallRecord), team string, prompt ...string) TaskExecutor {
	e := newTestSwappableLLMExecutor(t, client, tools, gates, view, recorder, team, prompt...)
	return func(ctx context.Context, task *model.Task, deps map[string]string, history []contextcontract.HistoryEntry, budget llm.OutputBudget) (ExecuteResult, error) {
		return executeTestStep(t, e.Execute, ctx, task, deps, history, budget)
	}
}

// executeTestStep 显式构造测试的 L3 权限及调用身份，不向生产代码添加缺省通路。
func executeTestStep(t *testing.T, executor TaskExecutor, ctx context.Context, task *model.Task, deps map[string]string, history []contextcontract.HistoryEntry, budget llm.OutputBudget) (ExecuteResult, error) {
	t.Helper()
	if task.ID == "" {
		task.ID = uuid.NewString()
	}
	if task.ContextPolicyRef == "" {
		task.ContextPolicyRef = policycatalog.ContextDefaultCurrent
	}
	if task.Lease == nil {
		task.Lease = &model.ExecutionLease{Schema: model.ExecutionLeaseSchemaCurrent, Model: "test-model", ModelCapabilityDigest: "test-capability", Digest: "test-lease"}
	}
	_, attempt, turn := executionIdentityFromContext(ctx)
	if attempt == "" {
		attempt = "test-attempt:" + task.ID
	}
	if turn == "" {
		turn = "test-turn:" + uuid.NewString()
	}
	ctx = WithExecutionIdentity(ctx, string(task.RunID), attempt, turn)
	if AgentIDFromContext(ctx) == "" {
		ctx = WithAgentContext(ctx, "test-agent", task.ID, 0)
	}
	for i := range history {
		if history[i].TurnID == "" {
			history[i].TurnID = fmt.Sprintf("fixture-turn-%d", i)
		}
	}
	return executor(ctx, task, deps, history, budget)
}

func replayGateTask(id string, tools []string) *model.Task {
	now := time.Now().UTC()
	return &model.Task{
		ID: id, RunID: runcontract.RunID("run-" + id), Description: "验证 replay gate",
		AttemptID: id + "/attempt-1", AttemptNo: 1,
		ContextPolicyRef: policycatalog.ContextDefaultCurrent,
		RunContract: &runcontract.RunContract{
			Schema: runcontract.SchemaCurrent, RunID: runcontract.RunID("run-" + id), CreatedAt: now,
			DeadlineAt:    now.Add(time.Hour),
			BudgetProfile: "test/v1",
		},
		Lease: &model.ExecutionLease{Schema: model.ExecutionLeaseSchemaCurrent,
			TaskID: id, Attempt: 1, FrozenAt: now, BusinessTools: tools, Digest: "lease-" + id,
		},
	}
}

func compileTestMessages(t *testing.T, system string, task *model.Task, deps map[string]string, history []contextcontract.HistoryEntry, team string) []llm.Message {
	t.Helper()
	r := testmodel.Runtime(t)
	c, err := r.Compile(context.Background(), contextruntime.Input{Identity: llm.Identity{InvocationID: uuid.NewString(), AttemptID: "test-attempt", ContextPolicyID: policycatalog.ContextDefaultCurrent, SessionID: "test-session"}, Instructions: contextruntime.Instructions{ProfileID: "test", System: system, Team: team, Objective: task.Description, Control: renderTaskContextBlock(task)}, Dependencies: deps, History: history, ToolRouter: contextruntime.ToolRouterBinding{SnapshotID: "test-tools"}, ExecutionLeaseRef: "test-lease", Options: testmodel.Options()})
	if err != nil {
		t.Fatal(err)
	}
	return c.Request().Spec().Messages
}

func testExecutor(t *testing.T, executor TaskExecutor) TaskExecutor {
	return func(ctx context.Context, task *model.Task, deps map[string]string, history []contextcontract.HistoryEntry, budget llm.OutputBudget) (ExecuteResult, error) {
		return executeTestStep(t, executor, ctx, task, deps, history, budget)
	}
}

func decodeTestHistory(raw []byte, out *[]contextcontract.HistoryEntry) error {
	entries, err := contextcontract.DecodeHistory(raw)
	if err == nil {
		*out = entries
	}
	return err
}
