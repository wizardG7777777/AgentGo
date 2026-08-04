package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// finalizing fence（V6 §5 升级思路 1）：submit_task_result 被接受
//（MarkTaskFinalized）后，同一 LLM 响应中排在其后的工具调用一律跳过——
// 不 dispatch、不产生副作用，返回「已跳过」提示文本并逐个 emit
// tool_call_skipped；排在其前的调用不受影响。

// fenceTestTools 注册三件套假工具：会把 holder 标记 finalized 的
// submit_task_result、真实落盘的 write_file、置标志位的 run_shell。
func fenceTestTools(t *testing.T, holder *FinalizationHolder, writeTarget string, shellRan *bool) *ToolRegistry {
	t.Helper()
	tools := NewToolRegistry()
	tools.Register("submit_task_result", "结构化提交", nil, func(_ context.Context, _ map[string]any) (string, error) {
		holder.MarkTaskFinalized()
		return "结构化结果已提交", nil
	})
	tools.Register("write_file", "写文件", nil, func(_ context.Context, args map[string]any) (string, error) {
		path, _ := args["path"].(string)
		if err := os.WriteFile(path, []byte("fence 不应写入"), 0o644); err != nil {
			return "", err
		}
		return "写入成功: " + path, nil
	})
	tools.Register("run_shell", "跑命令", nil, func(_ context.Context, _ map[string]any) (string, error) {
		*shellRan = true
		return "exit_code: 0", nil
	})
	tools.Register("edit_file", "改文件", nil, func(_ context.Context, _ map[string]any) (string, error) {
		return "", os.WriteFile(filepath.Join(filepath.Dir(writeTarget), "fence_edit.txt"), []byte("x"), 0o644)
	})
	return tools
}

func skippedEvents(events []trace.Event) []trace.Event {
	var out []trace.Event
	for _, ev := range events {
		if ev.Kind == trace.KindToolCallSkipped {
			out = append(out, ev)
		}
	}
	return out
}

// [submit_task_result, write_file, run_shell] 同一响应：后两者不执行
//（磁盘无产物、shell 未跑、ToolCallRecord 只记真实执行的提交调用），
// 各自收到「已跳过」提示并产生一条 tool_call_skipped。
func TestFinalizingFence_SkipsTrailingToolCalls(t *testing.T) {
	traceDir := setupTraceWriter(t)
	holder := NewFinalizationHolder()
	holder.Set("task-fence")
	writeTarget := filepath.Join(t.TempDir(), "fence_write.txt")
	shellRan := false
	tools := fenceTestTools(t, holder, writeTarget, &shellRan)

	var records []string
	mock := &mockLLMClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "submit_task_result", Arguments: map[string]any{"summary": "done"}},
			{ID: "c2", Name: "write_file", Arguments: map[string]any{"path": writeTarget}},
			{ID: "c3", Name: "run_shell", Arguments: map[string]any{"command": "echo hi"}},
		},
	}}}
	exec := NewSwappableLLMExecutor(mock, tools, nil, nil,
		func(_ string, rec store.ToolCallRecord) { records = append(records, rec.ToolName) }, "")
	exec.SetFinalizationChecker(holder)

	result, err := exec.Execute(context.Background(), &model.Task{ID: "task-fence", Description: "fence"}, nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.ToolCalled || len(result.ToolResults) != 3 {
		t.Fatalf("ToolCalled=%v ToolResults=%d，期望 true/3", result.ToolCalled, len(result.ToolResults))
	}

	// 副作用断言：write_file 未落盘、run_shell 未执行。
	if _, err := os.Stat(writeTarget); !os.IsNotExist(err) {
		t.Errorf("fence 后 write_file 不应落盘，stat err=%v", err)
	}
	if shellRan {
		t.Error("fence 后 run_shell 不应执行")
	}
	// ToolCallRecord 只记真实执行的 submit_task_result（跳过的调用从未发生）。
	if len(records) != 1 || records[0] != "submit_task_result" {
		t.Errorf("ToolCallRecord = %v，期望仅 [submit_task_result]", records)
	}

	// 跳过的两个调用收到结构化提示文本（protocol 上每个 tool_call 都有对应 tool 消息）。
	for i, id := range []string{"c2", "c3"} {
		tr := result.ToolResults[i+1]
		if tr.ToolCallID != id {
			t.Errorf("ToolResults[%d].ToolCallID = %q，期望 %q", i+1, tr.ToolCallID, id)
		}
		if !strings.Contains(tr.Content, "已跳过：任务已进入收尾（finalizing），本次调用未执行") {
			t.Errorf("ToolResults[%d].Content = %q，期望跳过提示", i+1, tr.Content)
		}
	}
	// 第一个调用（submit_task_result）正常执行。
	if !strings.Contains(result.ToolResults[0].Content, "结构化结果已提交") {
		t.Errorf("submit_task_result 应正常执行，实际：%q", result.ToolResults[0].Content)
	}

	// trace：两条 tool_call_skipped，携带 tool 名 / call_id / 原因。
	skipped := skippedEvents(p1fixesReadTraceEvents(t, traceDir))
	if len(skipped) != 2 {
		t.Fatalf("tool_call_skipped 应有 2 条，实际 %d", len(skipped))
	}
	wantTools := map[string]string{"write_file": "c2", "run_shell": "c3"}
	for _, ev := range skipped {
		wantCall, ok := wantTools[ev.Tool]
		if !ok {
			t.Errorf("意外被跳过的工具：%s", ev.Tool)
			continue
		}
		if ev.CallID != wantCall {
			t.Errorf("%s 的 CallID = %q，期望 %q", ev.Tool, ev.CallID, wantCall)
		}
		if ev.Reason == "" {
			t.Errorf("%s 的 tool_call_skipped 缺少原因", ev.Tool)
		}
		delete(wantTools, ev.Tool)
	}
	if len(wantTools) != 0 {
		t.Errorf("以下工具的 tool_call_skipped 缺失：%v", wantTools)
	}
}

// [write_file, submit_task_result, edit_file] 同一响应：排在提交前的
// write_file 正常执行（产物落盘），排在提交后的 edit_file 被 fence 跳过。
func TestFinalizingFence_CallsBeforeSubmitExecute(t *testing.T) {
	traceDir := setupTraceWriter(t)
	holder := NewFinalizationHolder()
	holder.Set("task-fence-2")
	dir := t.TempDir()
	writeTarget := filepath.Join(dir, "before.txt")
	shellRan := false
	tools := fenceTestTools(t, holder, writeTarget, &shellRan)

	mock := &mockLLMClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "write_file", Arguments: map[string]any{"path": writeTarget}},
			{ID: "c2", Name: "submit_task_result", Arguments: map[string]any{"summary": "done"}},
			{ID: "c3", Name: "edit_file", Arguments: map[string]any{"path": filepath.Join(dir, "x.txt")}},
		},
	}}}
	exec := NewSwappableLLMExecutor(mock, tools, nil, nil, nil, "")
	exec.SetFinalizationChecker(holder)

	result, err := exec.Execute(context.Background(), &model.Task{ID: "task-fence-2", Description: "fence"}, nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 提交前的 write_file 真实执行。
	data, err := os.ReadFile(writeTarget)
	if err != nil || string(data) != "fence 不应写入" {
		t.Errorf("提交前的 write_file 应落盘: data=%q err=%v", data, err)
	}
	if !strings.Contains(result.ToolResults[0].Content, "写入成功") {
		t.Errorf("write_file 应返回真实结果，实际：%q", result.ToolResults[0].Content)
	}
	// 提交后的 edit_file 被跳过：无产物、有跳过提示。
	if _, err := os.Stat(filepath.Join(dir, "fence_edit.txt")); !os.IsNotExist(err) {
		t.Errorf("edit_file 不应落盘，stat err=%v", err)
	}
	if !strings.Contains(result.ToolResults[2].Content, "已跳过") {
		t.Errorf("edit_file 应收到跳过提示，实际：%q", result.ToolResults[2].Content)
	}
	skipped := skippedEvents(p1fixesReadTraceEvents(t, traceDir))
	if len(skipped) != 1 || skipped[0].Tool != "edit_file" || skipped[0].CallID != "c3" {
		t.Errorf("tool_call_skipped 应仅 edit_file/c3 一条，实际 %+v", skipped)
	}
}

// 未装配 FinalizationChecker（旧装配形态）：无 fence，全部调用照常执行。
func TestFinalizingFence_DisabledWithoutChecker(t *testing.T) {
	holder := NewFinalizationHolder()
	holder.Set("task-nofence")
	dir := t.TempDir()
	writeTarget := filepath.Join(dir, "all.txt")
	shellRan := false
	tools := fenceTestTools(t, holder, writeTarget, &shellRan)

	mock := &mockLLMClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "submit_task_result", Arguments: map[string]any{"summary": "done"}},
			{ID: "c2", Name: "write_file", Arguments: map[string]any{"path": writeTarget}},
			{ID: "c3", Name: "run_shell", Arguments: map[string]any{"command": "echo hi"}},
		},
	}}}
	exec := NewSwappableLLMExecutor(mock, tools, nil, nil, nil, "")
	// 刻意不调用 SetFinalizationChecker。

	if _, err := exec.Execute(context.Background(), &model.Task{ID: "task-nofence", Description: "no fence"}, nil, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(writeTarget); err != nil {
		t.Error("未装配 checker 时 write_file 应照常执行")
	}
	if !shellRan {
		t.Error("未装配 checker 时 run_shell 应照常执行")
	}
}
