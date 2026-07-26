package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// capMockClient 捕获每次 Chat 调用收到的工具定义，用于断言 LLM 视野被
// 节点能力过滤视图收窄。
type capMockClient struct {
	toolDefs [][]llm.ToolDef
	calls    int
}

func (m *capMockClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	m.calls++
	m.toolDefs = append(m.toolDefs, append([]llm.ToolDef(nil), tools...))
	return llm.Response{Content: "done"}, nil
}

func newCapToolRegistry() *ToolRegistry {
	tools := NewToolRegistry()
	noop := func(ctx context.Context, args map[string]any) (string, error) { return "ok", nil }
	tools.Register("read_file", "读取文件", nil, noop)
	tools.Register("write_file", "写入文件", nil, noop)
	tools.Register("run_shell", "执行命令", nil, noop)
	return tools
}

// publishAndClaim 发布并认领一个带节点能力的任务，返回 processing 态任务 ID。
func publishAndClaim(t *testing.T, s store.TaskStore, agentID string, ncap *model.NodeCapability) string {
	t.Helper()
	task := &model.Task{Description: "节点能力任务", EventType: "code", Capability: ncap}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	return task.ID
}

// 过滤视图收窄生效：任务声明工具子集时，LLM 只见到子集 defs；
// 任务结束后 executor 恢复原 registry。
func TestProcessTask_CapabilityToolFilterNarrowsLLMView(t *testing.T) {
	s, r, _ := setup()
	mock := &capMockClient{}
	full := newCapToolRegistry()
	exec := NewSwappableLLMExecutor(mock, full, nil, nil, nil, "")

	const agentID = "agent-cap"
	taskID := publishAndClaim(t, s, agentID, &model.NodeCapability{Tools: []string{"read_file"}})

	ag := NewAgent(agentID, "code", s, r, exec.Execute, 5)
	ag.ToolSwapper = exec
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", task.Status, task.Error)
	}
	if mock.calls != 1 {
		t.Fatalf("LLM 调用次数 = %d，want 1", mock.calls)
	}
	defs := mock.toolDefs[0]
	if len(defs) != 1 || defs[0].Name != "read_file" {
		names := make([]string, 0, len(defs))
		for _, d := range defs {
			names = append(names, d.Name)
		}
		t.Fatalf("LLM 见到的工具 = %v，want [read_file]", names)
	}
	// 任务结束后恢复原 registry：当前生效 registry 回到完整注册集
	if got := exec.ToolRegistry(); got != full {
		t.Fatalf("任务结束后 registry 未恢复：got %p，want 原 registry %p", got, full)
	}
	if exec.ToolRegistry().RegisteredCount() != 3 {
		t.Fatalf("恢复后注册数 = %d，want 3", exec.ToolRegistry().RegisteredCount())
	}
}

// 越界 fail-closed：节点工具 ⊄ executor 注册全集时任务直接失败，
// 错误列明缺失工具，LLM 从未被调用，registry 不被替换。
func TestProcessTask_CapabilityToolFilterFailClosed(t *testing.T) {
	s, r, _ := setup()
	mock := &capMockClient{}
	full := newCapToolRegistry()
	exec := NewSwappableLLMExecutor(mock, full, nil, nil, nil, "")

	const agentID = "agent-cap"
	taskID := publishAndClaim(t, s, agentID,
		&model.NodeCapability{Tools: []string{"read_file", "nonexistent_tool"}})

	ag := NewAgent(agentID, "code", s, r, exec.Execute, 5)
	ag.ToolSwapper = exec
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", task.Status)
	}
	if !strings.Contains(task.Error, "nonexistent_tool") {
		t.Fatalf("失败原因应列明缺失工具 nonexistent_tool，实际: %s", task.Error)
	}
	if mock.calls != 0 {
		t.Fatalf("fail-closed 不应调用 LLM，实际 %d 次", mock.calls)
	}
	if got := exec.ToolRegistry(); got != full {
		t.Fatal("fail-closed 路径不应替换 registry")
	}
}

// executor 不支持按任务过滤（ToolSwapper 未装配）时同样 fail-closed——
// 无法用超集工具集降级执行一个声明了子集的任务。
func TestProcessTask_CapabilityNilSwapperFailClosed(t *testing.T) {
	s, r, _ := setup()
	plainExec := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}

	const agentID = "agent-cap"
	taskID := publishAndClaim(t, s, agentID, &model.NodeCapability{Tools: []string{"read_file"}})

	ag := NewAgent(agentID, "code", s, r, plainExec, 5)
	ag.processTask(context.Background(), taskID)

	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusFailed {
		t.Fatalf("status = %s，want failed", task.Status)
	}
	if !strings.Contains(task.Error, "ToolSwapper") {
		t.Fatalf("失败原因应指出 ToolSwapper 未装配，实际: %s", task.Error)
	}
}

// 模型覆盖：任务声明 Capability.Model 时执行期间 a.Model 被替换，
// 任务结束恢复原值。
func TestProcessTask_CapabilityModelOverrideAndRestore(t *testing.T) {
	s, r, _ := setup()

	const agentID = "agent-cap"
	taskID := publishAndClaim(t, s, agentID, &model.NodeCapability{Model: "m-override"})

	var ag *Agent
	var seenModel string
	exec := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		seenModel = ag.Model // 执行期间读到的模型
		return ExecuteResult{Output: "done"}, nil
	}
	ag = NewAgent(agentID, "code", s, r, exec, 5)
	ag.Model = "m-base"
	ag.processTask(context.Background(), taskID)

	if seenModel != "m-override" {
		t.Fatalf("执行期间 a.Model = %q，want m-override", seenModel)
	}
	if ag.Model != "m-base" {
		t.Fatalf("任务结束后 a.Model = %q，want 恢复为 m-base", ag.Model)
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != model.TaskStatusCompleted {
		t.Fatalf("status = %s，want completed（error: %s）", task.Status, task.Error)
	}
}

// Filtered 视图单元行为：defs 收窄、Dispatch 拒绝视图外工具（第二道防线）、
// Missing 报告越界名、原 registry 不被修改。
func TestToolRegistry_FilteredView(t *testing.T) {
	full := newCapToolRegistry()

	if missing := full.Missing([]string{"read_file", "no_such", "no_such", "write_file"}); len(missing) != 1 || missing[0] != "no_such" {
		t.Fatalf("Missing = %v，want [no_such]（去重、保序）", missing)
	}
	if missing := full.Missing([]string{"read_file"}); len(missing) != 0 {
		t.Fatalf("Missing = %v，want 空", missing)
	}

	view := full.Filtered([]string{"read_file"})
	if got := len(view.Defs()); got != 1 || view.Defs()[0].Name != "read_file" {
		t.Fatalf("过滤视图 defs 数 = %d，want 1 (read_file)", got)
	}
	if view.RegisteredCount() != 1 {
		t.Fatalf("过滤视图注册数 = %d，want 1", view.RegisteredCount())
	}
	// 视图外工具名即使被 LLM 幻觉出来，Dispatch 也拒绝
	if _, err := view.Dispatch(context.Background(), llm.ToolCall{Name: "write_file", Arguments: nil}); err == nil ||
		!strings.Contains(err.Error(), "未知工具") {
		t.Fatalf("视图 Dispatch 视图外工具应报「未知工具」，实际: %v", err)
	}
	// 视图内工具正常分发
	if out, err := view.Dispatch(context.Background(), llm.ToolCall{Name: "read_file", Arguments: nil}); err != nil || out != "ok" {
		t.Fatalf("视图 Dispatch 视图内工具失败: out=%q err=%v", out, err)
	}
	// 原 registry 不被修改
	if full.RegisteredCount() != 3 || len(full.Defs()) != 3 {
		t.Fatal("Filtered 修改了原 registry")
	}
}

// SwapToolRegistry 原子换入/换出语义。
func TestLLMExecutor_SwapToolRegistry(t *testing.T) {
	mock := &capMockClient{}
	full := newCapToolRegistry()
	exec := NewSwappableLLMExecutor(mock, full, nil, nil, nil, "")

	if exec.ToolRegistry() != full {
		t.Fatal("初始 registry 应为构造入参")
	}
	view := full.Filtered([]string{"read_file"})
	old := exec.SwapToolRegistry(view)
	if old != full {
		t.Fatal("SwapToolRegistry 应返回被替换的旧 registry")
	}
	if exec.ToolRegistry() != view {
		t.Fatal("换入后当前 registry 应为过滤视图")
	}
	old = exec.SwapToolRegistry(full)
	if old != view {
		t.Fatal("恢复时应返回过滤视图")
	}
	if exec.ToolRegistry() != full {
		t.Fatal("恢复后当前 registry 应为完整注册集")
	}
	// nil 入参被拒绝（防御编程错误），当前 registry 不变
	if got := exec.SwapToolRegistry(nil); got != full || exec.ToolRegistry() != full {
		t.Fatal("SwapToolRegistry(nil) 应保持原 registry")
	}
}
