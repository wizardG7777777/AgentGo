package scheduler

import (
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/testmodel"
	"context"
	"strings"
	"testing"
)

func newSoloTestBundle(t *testing.T, modeStore *modes.Store, mockLLM llm.Invoker) (*Bundle, *store.MemoryTaskStore, *model.Task) {
	t.Helper()
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	cfg.Agents = []config.// newSoloTestBundle 构造一个 scheduler Bundle 用于 solo 单测：
	// modeStore 由调用方给定（nil 时走 New 内部的 DefaultStore 回落，等价 team）。
	// 返回的 scheduler task 已发布并被认领，holder 已通过 OnTaskStart 设置，
	// 使 publish_task / report_done 等依赖当前任务上下文的工具可直接执行。
	// executeOneRound 用脚本化 LLM 跑一轮 Execute，返回唯一工具调用的 result content。
	// TestSoloPublishTaskBlocked_Solo 验证 topo=solo 时 scheduler 的 publish_task
	// 被硬拦截，且错误消息明确告知"solo 模式禁止派发子任务，请直接执行"。
	// TestSoloPublishTaskBlocked_TeamAllows 验证 topo=team 时 publish_task 正常放行。
	// TestSoloPublishTaskBlocked_NilStoreAllows 验证 modeStore 为 nil 时
	// （New 内部回落 DefaultStore=team，包装器本身也 nil 安全）publish_task 不受拦截。
	// TestSoloPublishTaskBlocked_RunnerUnaffected 验证拦截只作用于 scheduler 自己的
	// ToolRegistry：按 runner 方式装配的 registry（不经 scheduler.New 的包装）即使在
	// solo 模式存在的环境下，publish_task 也不受影响。
	// New 注册的稳定别名，保证路由存在
	AgentKind{{Kind: "worker", Replicas: 1}}
	bundle := newTestScheduler(t, s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, modeStore, nil, nil, nil)
	task := &model.Task{Description: "solo 测试任务", EventType: "__scheduler__", GraphID: "g-solo", NodeID: "work", ActivationID: "work@1"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("发布 scheduler 任务失败: %v", err)
	}
	if err := s.ClaimTask(bundle.Agent.ID, task.ID); err != nil {
		t.Fatalf("认领 scheduler 任务失败: %v", err)
	}
	bundle.Agent.OnTaskStart(task.ID)
	t.Cleanup(func() {
		bundle.Agent.OnTaskEnd(task.ID, true)
	})
	return bundle, s, task
}

func executeOneRound(t *testing.T, bundle *Bundle, task *model.Task) string {
	t.Helper()
	result, err := bundle.SchedulerExec.Execute(context.Background(), task, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if len(result.ToolResults) != 1 {
		t.Fatalf("期望 1 个工具结果，实际 %d: %+v", len(result.ToolResults), result.ToolResults)
	}
	return result.ToolResults[0].Content
}
func mustTask(t *testing.T, s *store.MemoryTaskStore) *model.Task {
	t.Helper()
	tasks, err := s.ScanAll()
	if err != nil || len(tasks) == 0 {
		t.Fatalf("读取 scheduler 任务失败: tasks=%d err=%v", len(tasks), err)
	}
	return tasks[0]
}
func TestSoloPublishTaskBlocked_SendMessageAllowed(t *testing.T) {
	mockLLM := &scriptedLLM{responses: []testmodel.Fixture{{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "send_message", Arguments: map[string]any{"to": "scheduler", "content": "solo 下的普通邮件"}}}}}}
	bundle, _, task := newSoloTestBundle(t, modes.NewStore(modes.ExecNormal, modes.TopoSolo), mockLLM)
	content := executeOneRound(t, bundle, task)
	if strings.Contains(content, "solo 编排模式禁止") {
		t.Errorf("send_message 不应被 solo 拦截，实际: %s", content)
	}
	if strings.Contains(content, "错误") {
		t.Errorf("send_message 应成功送达 scheduler 别名，实际: %s", content)
	}
}
