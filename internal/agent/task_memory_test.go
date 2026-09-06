package agent

import (
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/testmodel"
	"context"
	"errors"

	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// TestProcessTask_TaskMemoryLifecycle 是 CM2 的端到端主测试：两工具轮 +
// 自然完成轮，断言 Task Memory 创建 → 逐轮更新 → 终态 Sealed 全链路，
// 以及注入文本与 Manifest 段、trace 三事件。
func TestProcessTask_TaskMemoryResumeAcrossAttempts(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()
	tmStore := taskmem.NewStore(t.TempDir())

	task := &model.Task{Description: "续跑任务", EventType: "code", RetryCount: 1}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	// 预置上一 attempt 的 Task Memory（模拟进程重启后的恢复：经磁盘往返）。
	prev := taskmem.New(task.ID)
	prev.Goal = "续跑任务"
	taskmem.ApplyTurn(prev, taskmem.TurnFacts{FilesWritten: []taskmem.FileWrittenFact{{Path: "old.go", Hash: "h-old"}}})
	if err := tmStore.Save(prev); err != nil {
		t.Fatalf("预置 Save: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	mock := &mockLLMClient{responses: []testmodel.Fixture{{Content: "完成"}}}
	runtime := testmodel.Runtime(t)
	runtime.TaskMemory = tmStore
	step := NewTurnExecutor(mock, NewToolRegistry(), nil, nil, runtime, contextruntime.Instructions{ProfileID: "memory-test"})
	executor := testExecutor(t, step.Execute)
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.TaskMemStore = tmStore
	ag.processTask(context.Background(), task.ID)

	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind == trace.KindTaskMemoryCreated && ev.TaskID == task.ID {
			t.Error("恢复既有 Task Memory 不应再发 created")
		}
	}
	mem, _, err := tmStore.LoadOrCreate(task.ID)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !mem.Sealed {
		t.Error("终态应封存")
	}
	// 上一 attempt 的文件版本在恢复后保留（版本继续滚动而非重来）。
	foundOld := false
	for _, f := range mem.Files {
		if f.Path == "old.go" && f.Hash == "h-old" {
			foundOld = true
		}
	}
	if !foundOld {
		t.Errorf("恢复后上一 attempt 的文件版本应保留: %+v", mem.Files)
	}
	// 恢复版本的注入应带 version（来自持久化）。
	if !containsMemoryMessage(mock.captured[0]) {
		t.Errorf("恢复任务首轮即应注入 Task Memory: %+v", mock.captured[0])
	}
}

// TestProcessTask_TaskMemoryNoLongerCompactsFromRepeatedPromptSpend：完整
// prompt token 的重复计费不得再触发 Raw History 压缩/checkpoint。
func TestProcessTask_TaskMemoryNoLongerCompactsFromRepeatedPromptSpend(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()
	tmStore := taskmem.NewStore(t.TempDir())

	task := &model.Task{Description: "压缩检查点", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	toolTurn := testmodel.Fixture{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}}}
	toolTurn.Usage.PromptTokens = 100
	textTurn := testmodel.Fixture{Content: "完成"}
	textTurn.Usage.PromptTokens = 100
	mock := &mockLLMClient{responses: []testmodel.Fixture{toolTurn, textTurn}}
	tools := NewToolRegistry()
	tools.Register("read_file", "读文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "ok", nil
	})
	executor := newTestLLMExecutor(t, mock, tools, nil, nil,
		func(taskID string, rec store.ToolCallRecord) { _ = s.AppendToolCall(taskID, rec) }, "")
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.TaskMemStore = tmStore
	ag.processTask(context.Background(), task.ID)

	found := false
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind == trace.KindTaskMemoryCheckpointed && ev.Reason == "history_compaction" && ev.TaskID == task.ID {
			found = true
		}
	}
	if found {
		t.Error("重复 prompt spend 不得再触发 history_compaction checkpoint")
	}
}

// TestProcessTask_TaskMemoryStoreDegraded：taskmem 目录不可写时任务继续，
// 不注入正文，Manifest 记 dropped:<原因>。
func TestProcessTask_TaskMemoryReadFailureBlocksBeforeLLM(t *testing.T) {
	_ = captureTraceToDir(t)
	s, r, _ := setup()

	// 构造不可写目录：父路径是文件 → MkdirAll/读取失败。
	blocker := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmStore := taskmem.NewStore(filepath.Join(blocker, "taskmem"))

	task := &model.Task{Description: "降级任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	mock := &mockLLMClient{responses: []testmodel.Fixture{{Content: "完成"}}}
	runtime := testmodel.Runtime(t)
	runtime.TaskMemory = failedTaskMemoryReader{}
	step := NewTurnExecutor(mock, NewToolRegistry(), nil, nil, runtime, contextruntime.Instructions{ProfileID: "memory-test"})
	executor := testExecutor(t, step.Execute)
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.TaskMemStore = tmStore
	ag.processTask(context.Background(), task.ID)

	cur, err := s.GetTask(task.ID)
	if err != nil || cur.Status != model.TaskStatusBlocked {
		t.Fatalf("必需记忆读取失败应阻断: %+v %v", cur, err)
	}
	if mock.callIndex != 0 {
		t.Fatal("记忆读取失败后仍调用 LLM")
	}
}

// TestInsertTaskMemMessage：注入位置紧随 user 首条；无 user 消息时追加尾部。

func TestTaskMemToolRecordDeltaIgnoresEqualTimestampReordering(t *testing.T) {
	stamp := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	oldRecord := store.ToolCallRecord{
		Timestamp: stamp, CallID: "call-old", ToolName: "write_file",
		Args: map[string]any{"path": "old.txt"}, Success: true,
	}
	newRecord := store.ToolCallRecord{
		Timestamp: stamp, CallID: "call-new", ToolName: "request_user_input",
		Args: map[string]any{"question": "格式？"}, Success: true,
	}
	rt := &taskMemRuntime{toolRecordsSeen: taskMemToolRecordMultiset([]store.ToolCallRecord{oldRecord})}

	// The new record sorts before the old prefix. A length/index cursor would
	// incorrectly replay oldRecord; the identity multiset must return newRecord.
	delta := rt.takeUnseenToolRecords([]store.ToolCallRecord{newRecord, oldRecord})
	if len(delta) != 1 || delta[0].CallID != "call-new" {
		t.Fatalf("delta = %+v, want only call-new", delta)
	}
	if again := rt.takeUnseenToolRecords([]store.ToolCallRecord{oldRecord, newRecord}); len(again) != 0 {
		t.Fatalf("reordering consumed records must not replay them: %+v", again)
	}

	// Multiset counts preserve a genuinely appended duplicate identity.
	duplicate := rt.takeUnseenToolRecords([]store.ToolCallRecord{newRecord, oldRecord, newRecord})
	if len(duplicate) != 1 || duplicate[0].CallID != "call-new" {
		t.Fatalf("duplicate delta = %+v, want one additional call-new", duplicate)
	}
}

func TestMatchTaskMemToolRecordContentUsesCallIDNotRecordOrder(t *testing.T) {
	stamp := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	records := []store.ToolCallRecord{
		{Timestamp: stamp, CallID: "call-input", ToolName: "request_user_input", Success: true},
		{Timestamp: stamp, CallID: "call-write", ToolName: "write_file", Success: true},
	}
	result := ExecuteResult{
		ToolCalls: []llm.ToolCall{
			{ID: "call-write", Name: "write_file"},
			{ID: "call-input", Name: "request_user_input"},
		},
		ToolResults: []contextcontract.ToolResult{
			{ToolCallID: "call-write", Content: "文件已写入"},
			{ToolCallID: "call-input", Content: "采用简洁版格式"},
		},
	}

	matched := matchTaskMemToolRecordContent(records, result)
	if matched[0] != "采用简洁版格式" || matched[1] != "文件已写入" {
		t.Fatalf("CallID join mismatch: %+v", matched)
	}
}

func TestMatchTaskMemToolRecordContentLegacyFallbackRequiresUniqueMatch(t *testing.T) {
	args := map[string]any{"question": "格式？"}
	legacy := []store.ToolCallRecord{{ToolName: "request_user_input", Args: args, Success: true}}
	unique := ExecuteResult{
		ToolCalls:   []llm.ToolCall{{ID: "call-1", Name: "request_user_input", Arguments: args}},
		ToolResults: []contextcontract.ToolResult{{ToolCallID: "call-1", Content: "采用简洁版格式"}},
	}
	if got := matchTaskMemToolRecordContent(legacy, unique); got[0] != "采用简洁版格式" {
		t.Fatalf("unique legacy match = %+v", got)
	}

	ambiguous := unique
	ambiguous.ToolCalls = append(ambiguous.ToolCalls,
		llm.ToolCall{ID: "call-2", Name: "request_user_input", Arguments: args})
	ambiguous.ToolResults = append(ambiguous.ToolResults,
		contextcontract.ToolResult{ToolCallID: "call-2", Content: "另一个回答"})
	if got := matchTaskMemToolRecordContent(legacy, ambiguous); len(got) != 0 {
		t.Fatalf("ambiguous legacy records must not guess: %+v", got)
	}
}

func containsMemoryMessage(messages []llm.Message) bool {
	for _, m := range messages {
		if strings.Contains(m.Content, "<task-memory") {
			return true
		}
	}
	return false
}

type failedTaskMemoryReader struct{}

func (failedTaskMemoryReader) Load(string) (*taskmem.TaskMemory, error) {
	return nil, errors.New("记忆读取故障")
}
