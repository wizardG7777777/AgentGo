package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
func TestProcessTask_TaskMemoryLifecycle(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()
	tmDir := t.TempDir()
	tmStore := taskmem.NewStore(tmDir)

	task := &model.Task{Description: "写笔记并验证", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	content := "hello task memory"
	mock := &mockLLMClient{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "notes/x.md", "content": content}},
			{ID: "c2", Name: "run_shell", Arguments: map[string]any{"command": "go test ./..."}},
		}},
		{ToolCalls: []llm.ToolCall{
			{ID: "c3", Name: "read_file", Arguments: map[string]any{"path": "notes/x.md"}},
		}},
		{Content: "全部完成"},
	}}
	tools := NewToolRegistry()
	tools.Register("write_file", "写文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "文件已写入: notes/x.md", nil
	})
	tools.Register("run_shell", "跑命令", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "exit_code: 1\nFAIL", nil // 命令级失败（工具本身成功）
	})
	tools.Register("read_file", "读文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return content, nil
	})
	executor := NewLLMExecutor(mock, tools, nil, nil,
		func(taskID string, rec store.ToolCallRecord) { _ = s.AppendToolCall(taskID, rec) }, "")
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.TaskMemStore = tmStore
	ag.processTask(context.Background(), task.ID)

	// --- 持久化与终态封存 ---
	mem, created, err := tmStore.LoadOrCreate(task.ID)
	if err != nil || created {
		t.Fatalf("LoadOrCreate = (created=%v, err=%v), want 已存在", created, err)
	}
	if !mem.Sealed {
		t.Error("任务完成后 Task Memory 应 Sealed 封存")
	}
	if mem.Phase != "终态:completed" {
		t.Errorf("Phase = %q, want 终态:completed", mem.Phase)
	}
	if !strings.Contains(mem.Goal, "写笔记并验证") {
		t.Errorf("Goal 应来自任务描述, got %q", mem.Goal)
	}
	// 落盘文件存在（恢复语义由 store 单测覆盖，这里确认文件确实写了）。
	if _, err := os.Stat(filepath.Join(tmDir, task.ID+".json")); err != nil {
		t.Errorf("Task Memory 未落盘: %v", err)
	}

	// --- 证据纪律：file_written → 文件版本（含 hash）---
	sum := sha256.Sum256([]byte(content))
	wantHash := hex.EncodeToString(sum[:])
	var fv *taskmem.FileVersion
	for i := range mem.Files {
		if mem.Files[i].Path == "notes/x.md" {
			fv = &mem.Files[i]
		}
	}
	if fv == nil || fv.Hash != wantHash {
		t.Errorf("文件版本 = %+v, want notes/x.md hash=%s", fv, wantHash)
	}
	// shell 非零 → 失败尝试；write_file → 动作。
	foundFailure := false
	for _, f := range mem.Failures {
		if strings.Contains(f, "exit=1") {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Errorf("Failures 应含 exit=1 失败尝试: %+v", mem.Failures)
	}
	foundWrite, foundRead := false, false
	for _, ac := range mem.Actions {
		if strings.HasPrefix(ac.Caption, "write_file notes/x.md") {
			foundWrite = true
		}
		if strings.HasPrefix(ac.Caption, "read_file notes/x.md") {
			foundRead = true
		}
	}
	if !foundWrite || !foundRead {
		t.Errorf("Actions 应含 write_file 与 read_file 记录: %+v", mem.Actions)
	}

	// --- trace 三事件：created×1、updated 仅实质变化轮、checkpointed 终态 ---
	var createdEvs, updatedEvs, checkpointedEvs []trace.Event
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.TaskID != task.ID {
			continue
		}
		switch ev.Kind {
		case trace.KindTaskMemoryCreated:
			createdEvs = append(createdEvs, ev)
		case trace.KindTaskMemoryUpdated:
			updatedEvs = append(updatedEvs, ev)
		case trace.KindTaskMemoryCheckpointed:
			checkpointedEvs = append(checkpointedEvs, ev)
		}
	}
	if len(createdEvs) != 1 {
		t.Errorf("task_memory_created = %d 条, want 1", len(createdEvs))
	}
	// 第 0、1 轮有实质变化（写文件+shell 失败 / 首次读取），第 2 轮纯文本
	// 自然完成不走 applySettledTurn。
	if len(updatedEvs) != 2 {
		t.Errorf("task_memory_updated = %d 条, want 2（仅实质变化轮）: %+v", len(updatedEvs), updatedEvs)
	}
	if len(checkpointedEvs) != 1 || checkpointedEvs[0].Reason != "terminal:completed" {
		t.Errorf("task_memory_checkpointed = %+v, want 1 条 terminal:completed", checkpointedEvs)
	}
	// updated payload：版本递增、段计数随更新增长，不含正文。
	var payload struct {
		Version int64 `json:"version"`
		Actions int   `json:"actions"`
	}
	if err := json.Unmarshal([]byte(updatedEvs[len(updatedEvs)-1].Description), &payload); err != nil {
		t.Fatalf("updated Description 非合法 JSON: %v", err)
	}
	if payload.Version < 3 || payload.Actions < 2 {
		t.Errorf("updated 段计数不符: %s", updatedEvs[len(updatedEvs)-1].Description)
	}

	// --- 注入：第二轮起 messages 含 <task-memory>，且紧随 user 首条 ---
	if len(mock.captured) != 3 {
		t.Fatalf("LLM 调用 = %d 次, want 3", len(mock.captured))
	}
	for callIdx, msgs := range mock.captured {
		foundIdx := -1
		firstUserIdx := -1
		for i, m := range msgs {
			if m.Role == "user" && firstUserIdx < 0 {
				firstUserIdx = i
			}
			if strings.Contains(m.Content, "<task-memory") {
				foundIdx = i
			}
		}
		if foundIdx < 0 {
			t.Errorf("第 %d 次 LLM 调用未注入 <task-memory>", callIdx)
			continue
		}
		if foundIdx != firstUserIdx+1 {
			t.Errorf("第 %d 次调用 <task-memory> 位置 = %d, want 紧随 user 首条（%d）", callIdx, foundIdx, firstUserIdx+1)
		}
	}
	// 第二轮的注入应反映第一轮的工作状态（write_file 已进入已完成动作）。
	if !strings.Contains(mock.captured[1][1].Content, "write_file notes/x.md") &&
		!strings.Contains(mock.captured[2][1].Content, "write_file notes/x.md") {
		t.Errorf("后续轮注入应含已完成动作, 第 2 轮注入: %q", mock.captured[1][1].Content)
	}

	// --- Manifest：task_memory 段登记（Source=task-memory，informational）---
	manifestFound := false
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind != trace.KindContextManifestBuilt || ev.TaskID != task.ID {
			continue
		}
		var items []manifestItemSummary
		if err := json.Unmarshal([]byte(ev.Description), &items); err != nil {
			t.Fatalf("Manifest 摘要非合法 JSON: %v", err)
		}
		for _, item := range items {
			if item.ID == ManifestSectionTaskMemory {
				manifestFound = true
				if item.Source != SourceTaskMemory || item.Authority != AuthorityInformational ||
					item.Freshness != FreshnessLive || item.Disposition != DispositionIncluded {
					t.Errorf("task_memory 段属性不符: %+v", item)
				}
			}
		}
	}
	if !manifestFound {
		t.Error("Context Manifest 未登记 task_memory 段")
	}
}

// TestProcessTask_TaskMemoryResumeAcrossAttempts：重试接手时加载既有
// Task Memory（不发 created），滚动继续，终态封存。
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

	mock := &mockLLMClient{responses: []llm.Response{{Content: "完成"}}}
	executor := NewLLMExecutor(mock, NewToolRegistry(), nil, nil, nil, "")
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
	if !strings.Contains(mock.captured[0][1].Content, "<task-memory") {
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

	toolTurn := llm.Response{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}}}
	toolTurn.Usage.PromptTokens = 100
	textTurn := llm.Response{Content: "完成"}
	textTurn.Usage.PromptTokens = 100
	mock := &mockLLMClient{responses: []llm.Response{toolTurn, textTurn}}
	tools := NewToolRegistry()
	tools.Register("read_file", "读文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "ok", nil
	})
	executor := NewLLMExecutor(mock, tools, nil, nil,
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
func TestProcessTask_TaskMemoryStoreDegraded(t *testing.T) {
	dir := captureTraceToDir(t)
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

	mock := &mockLLMClient{responses: []llm.Response{{Content: "完成"}}}
	executor := NewLLMExecutor(mock, NewToolRegistry(), nil, nil, nil, "")
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.TaskMemStore = tmStore
	ag.processTask(context.Background(), task.ID)

	// 任务照常完成。
	cur, err := s.GetTask(task.ID)
	if err != nil || cur.Status != model.TaskStatusCompleted {
		t.Fatalf("降级不应阻断任务, status=%v err=%v", cur.Status, err)
	}
	// 不注入正文。
	for _, msgs := range mock.captured {
		for _, m := range msgs {
			if strings.Contains(m.Content, "<task-memory") {
				t.Error("降级时不应注入 Task Memory 正文")
			}
		}
	}
	// Manifest 记 dropped:store_unavailable。
	foundDropped := false
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind != trace.KindContextManifestBuilt || ev.TaskID != task.ID {
			continue
		}
		if strings.Contains(ev.Description, `"id":"task_memory"`) &&
			strings.Contains(ev.Description, "dropped:store_unavailable") {
			foundDropped = true
		}
	}
	if !foundDropped {
		t.Error("降级时 Manifest 应登记 task_memory dropped:store_unavailable")
	}
}

// TestInsertTaskMemMessage：注入位置紧随 user 首条；无 user 消息时追加尾部。
func TestInsertTaskMemMessage(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "a"},
	}
	out := insertTaskMemMessage(msgs, "<task-memory>x</task-memory>")
	if len(out) != 4 || out[2].Content != "<task-memory>x</task-memory>" {
		t.Fatalf("注入位置不符: %+v", out)
	}
	// 原切片不被修改（调用方可能复用）。
	if len(msgs) != 3 {
		t.Error("原 messages 不应被修改")
	}
	noUser := []llm.Message{{Role: "system", Content: "sys"}}
	out2 := insertTaskMemMessage(noUser, "<task-memory>x</task-memory>")
	if len(out2) != 2 || out2[1].Content != "<task-memory>x</task-memory>" {
		t.Errorf("无 user 消息时应追加尾部: %+v", out2)
	}
}

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
		ToolResults: []ToolResult{
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
		ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "采用简洁版格式"}},
	}
	if got := matchTaskMemToolRecordContent(legacy, unique); got[0] != "采用简洁版格式" {
		t.Fatalf("unique legacy match = %+v", got)
	}

	ambiguous := unique
	ambiguous.ToolCalls = append(ambiguous.ToolCalls,
		llm.ToolCall{ID: "call-2", Name: "request_user_input", Arguments: args})
	ambiguous.ToolResults = append(ambiguous.ToolResults,
		ToolResult{ToolCallID: "call-2", Content: "另一个回答"})
	if got := matchTaskMemToolRecordContent(legacy, ambiguous); len(got) != 0 {
		t.Fatalf("ambiguous legacy records must not guess: %+v", got)
	}
}
