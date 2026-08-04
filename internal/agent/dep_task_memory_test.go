// dep_task_memory_test.go 覆盖 V6 CM4 的依赖任务 Task Memory 交接注入：
// 下游任务在 processTask 入口加载各前置任务封存的 Task Memory，以
// <dep-task-memory> 块注入 history（取代已删除的 <upstream-transfer-notes>
// TransferNote 注入），Manifest 登记 dep_task_memory 段。
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// TestProcessTask_DepTaskMemoryHandoff：dep 有已封存的 Task Memory 时，
// 下游任务首轮 LLM 调用的 messages 含 <dep-task-memory> 渲染块（目标/已完成
// 动作），Manifest 登记 dep_task_memory 段（Source=task-memory，
// informational，snapshot）；同时全链路不出现 transfer-note 系标记。
func TestProcessTask_DepTaskMemoryHandoff(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()
	tmStore := taskmem.NewStore(t.TempDir())

	// --- 上游任务：一轮工具调用后完成，Task Memory 终态封存 ---
	dep := &model.Task{Description: "调查认证模块", EventType: "code"}
	if err := s.PublishTask(dep); err != nil {
		t.Fatalf("PublishTask dep: %v", err)
	}
	if err := s.ClaimTask("agent-up", dep.ID); err != nil {
		t.Fatalf("ClaimTask dep: %v", err)
	}
	upMock := &mockLLMClient{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "auth/login.go"}},
		}},
		{Content: "调查完成"},
	}}
	upTools := NewToolRegistry()
	upTools.Register("read_file", "读文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "认证逻辑", nil
	})
	upExec := NewLLMExecutor(upMock, upTools, nil, nil,
		func(taskID string, rec store.ToolCallRecord) { _ = s.AppendToolCall(taskID, rec) }, "")
	upAgent := NewAgent("agent-up", "code", s, r, upExec)
	upAgent.TaskMemStore = tmStore
	upAgent.processTask(context.Background(), dep.ID)

	depMem, err := tmStore.Load(dep.ID)
	if err != nil || depMem == nil || !depMem.Sealed {
		t.Fatalf("dep Task Memory 应已封存: mem=%+v err=%v", depMem, err)
	}

	// --- 下游任务：声明依赖，首轮即应看到上游 Task Memory ---
	down := &model.Task{Description: "基于调查结果重构", EventType: "code", Dependencies: []string{dep.ID}}
	if err := s.PublishTask(down); err != nil {
		t.Fatalf("PublishTask down: %v", err)
	}
	if err := s.ClaimTask("agent-down", down.ID); err != nil {
		t.Fatalf("ClaimTask down: %v", err)
	}
	downMock := &mockLLMClient{responses: []llm.Response{{Content: "重构完成"}}}
	downExec := NewLLMExecutor(downMock, NewToolRegistry(), nil, nil, nil, "")
	downAgent := NewAgent("agent-down", "code", s, r, downExec)
	downAgent.TaskMemStore = tmStore
	downAgent.processTask(context.Background(), down.ID)

	if len(downMock.captured) == 0 {
		t.Fatal("下游任务未发起 LLM 调用")
	}
	var joined strings.Builder
	for _, msgs := range downMock.captured {
		for _, m := range msgs {
			joined.WriteString(m.Content)
			joined.WriteString("\n")
		}
	}
	all := joined.String()
	if !strings.Contains(all, "<dep-task-memory>") {
		t.Errorf("下游 messages 应含 <dep-task-memory> 交接块:\n%s", all)
	}
	if !strings.Contains(all, "调查认证模块") {
		t.Errorf("交接块应含上游任务目标, got:\n%s", all)
	}
	if !strings.Contains(all, "read_file auth/login.go") {
		t.Errorf("交接块应含上游已完成动作, got:\n%s", all)
	}
	// absence：TransferNote 系标记全链路清零（V6 CM4）。
	for _, marker := range []string{"<transfer-note", "<upstream-transfer-notes>"} {
		if strings.Contains(all, marker) {
			t.Errorf("messages 不应出现已删除的 %s 标记", marker)
		}
	}

	// --- Manifest：dep_task_memory 段登记 ---
	foundSection := false
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind != trace.KindContextManifestBuilt || ev.TaskID != down.ID {
			continue
		}
		var items []manifestItemSummary
		if err := json.Unmarshal([]byte(ev.Description), &items); err != nil {
			t.Fatalf("Manifest 摘要非合法 JSON: %v", err)
		}
		for _, item := range items {
			if item.ID == ManifestSectionDepTaskMemory {
				foundSection = true
				if item.Source != SourceTaskMemory || item.Authority != AuthorityInformational ||
					item.Freshness != FreshnessSnapshot || item.Disposition != DispositionIncluded {
					t.Errorf("dep_task_memory 段属性不符: %+v", item)
				}
			}
		}
	}
	if !foundSection {
		t.Error("Context Manifest 未登记 dep_task_memory 段")
	}
}

// TestProcessTask_DepTaskMemoryStoreMissing：TaskMemStore 未装配（nil）时
// 带依赖的任务照常执行、不注入交接块，Manifest 记 dep_task_memory
// dropped:store_unavailable。
func TestProcessTask_DepTaskMemoryStoreMissing(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()

	dep := &model.Task{Description: "前置任务", EventType: "code"}
	if err := s.PublishTask(dep); err != nil {
		t.Fatalf("PublishTask dep: %v", err)
	}
	if err := s.ClaimTask("agent-up", dep.ID); err != nil {
		t.Fatalf("ClaimTask dep: %v", err)
	}
	upMock := &mockLLMClient{responses: []llm.Response{{Content: "完成"}}}
	upAgent := NewAgent("agent-up", "code", s, r, NewLLMExecutor(upMock, NewToolRegistry(), nil, nil, nil, ""))
	upAgent.processTask(context.Background(), dep.ID)

	down := &model.Task{Description: "下游任务", EventType: "code", Dependencies: []string{dep.ID}}
	if err := s.PublishTask(down); err != nil {
		t.Fatalf("PublishTask down: %v", err)
	}
	if err := s.ClaimTask("agent-down", down.ID); err != nil {
		t.Fatalf("ClaimTask down: %v", err)
	}
	downMock := &mockLLMClient{responses: []llm.Response{{Content: "完成"}}}
	downAgent := NewAgent("agent-down", "code", s, r, NewLLMExecutor(downMock, NewToolRegistry(), nil, nil, nil, ""))
	// 刻意不装配 TaskMemStore（nil）——交接注入整链关闭。
	downAgent.processTask(context.Background(), down.ID)

	cur, err := s.GetTask(down.ID)
	if err != nil || cur.Status != model.TaskStatusCompleted {
		t.Fatalf("store 缺失不应阻断任务, status=%v err=%v", cur.Status, err)
	}
	for _, msgs := range downMock.captured {
		for _, m := range msgs {
			if strings.Contains(m.Content, "<dep-task-memory>") {
				t.Error("store 未装配时不应注入 <dep-task-memory> 块")
			}
		}
	}
	foundDropped := false
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind != trace.KindContextManifestBuilt || ev.TaskID != down.ID {
			continue
		}
		if strings.Contains(ev.Description, `"id":"dep_task_memory"`) &&
			strings.Contains(ev.Description, "dropped:store_unavailable") {
			foundDropped = true
		}
	}
	if !foundDropped {
		t.Error("store 未装配时 Manifest 应登记 dep_task_memory dropped:store_unavailable")
	}
}

// TestProcessTask_RetryHandoffWithoutTransferNote：重试接手回归——
// RetryCount>0 的任务无 transfer-note 注入（机制已删除），但既有
// Task Memory 照常恢复注入，任务可完成。
func TestProcessTask_RetryHandoffWithoutTransferNote(t *testing.T) {
	s, r, _ := setup()
	tmStore := taskmem.NewStore(t.TempDir())

	task := &model.Task{Description: "重试接手任务", EventType: "code", RetryCount: 1}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	// 预置上一 attempt 的 Task Memory（接手者应看到滚动工作状态）。
	prev := taskmem.New(task.ID)
	prev.Goal = "重试接手任务"
	taskmem.ApplyTurn(prev, taskmem.TurnFacts{FilesWritten: []taskmem.FileWrittenFact{{Path: "partial.go", Hash: "h-p"}}})
	if err := tmStore.Save(prev); err != nil {
		t.Fatalf("预置 Save: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	mock := &mockLLMClient{responses: []llm.Response{{Content: "完成"}}}
	ag := NewAgent("agent-1", "code", s, r, NewLLMExecutor(mock, NewToolRegistry(), nil, nil, nil, ""))
	ag.TaskMemStore = tmStore
	ag.processTask(context.Background(), task.ID)

	cur, err := s.GetTask(task.ID)
	if err != nil || cur.Status != model.TaskStatusCompleted {
		t.Fatalf("重试任务应正常完成, status=%v err=%v", cur.Status, err)
	}
	if len(mock.captured) == 0 {
		t.Fatal("未发起 LLM 调用")
	}
	var joined strings.Builder
	for _, msgs := range mock.captured {
		for _, m := range msgs {
			joined.WriteString(m.Content)
			joined.WriteString("\n")
		}
	}
	all := joined.String()
	if !strings.Contains(all, "<task-memory") || !strings.Contains(all, "partial.go") {
		t.Errorf("重试接手应注入恢复的 Task Memory（含上一 attempt 文件版本）:\n%s", all)
	}
	for _, marker := range []string{"<transfer-note", "<upstream-transfer-notes>"} {
		if strings.Contains(all, marker) {
			t.Errorf("重试接手不应出现已删除的 %s 标记", marker)
		}
	}
}
