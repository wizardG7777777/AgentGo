package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// 这些测试从旧的 internal/scheduler/scheduler_test.go::TestScheduler_BuildBoardJSON_*
// 迁移而来。原测试调用 sched.buildBoardJSON 私有方法，现在直接测公开 helper
// BuildBoardJSON。

func TestBuildBoardJSON_Resources(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 4}

	// 一个 processing worker task，agent worker-1 持有
	t1 := &model.Task{Description: "in flight"}
	if err := s.PublishTask(t1); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.ClaimTask("worker-1", t1.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventTickerWakeup}, SnapshotSources{})

	// resources 应当反映：worker_count=4, busy=1, available=3
	if !strings.Contains(out, `"worker_count": 4`) {
		t.Errorf("expected worker_count=4 in output, got: %s", out)
	}
	if !strings.Contains(out, `"busy_workers": 1`) {
		t.Errorf("expected busy_workers=1, got: %s", out)
	}
	if !strings.Contains(out, `"available_workers": 3`) {
		t.Errorf("expected available_workers=3, got: %s", out)
	}
}

func TestBuildBoardJSON_ResourcesDefault(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{} // WorkerCount=0 → 默认 1

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput}, SnapshotSources{})

	if !strings.Contains(out, `"worker_count": 1`) {
		t.Errorf("expected worker_count default 1, got: %s", out)
	}
	if !strings.Contains(out, `"available_workers": 1`) {
		t.Errorf("expected available_workers=1, got: %s", out)
	}
}

func TestBuildBoardJSON_TriggerFields(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	out := BuildBoardJSON(s, cfg, "plan", model.Event{
		Type:    model.EventUserInput,
		Payload: map[string]string{"text": "hello world"},
	}, SnapshotSources{})

	if !strings.Contains(out, `"mode": "plan"`) {
		t.Errorf("expected mode=plan, got: %s", out)
	}
	if !strings.Contains(out, `"type": "user_input"`) {
		t.Errorf("expected trigger.type=user_input, got: %s", out)
	}
	if !strings.Contains(out, `"text": "hello world"`) {
		t.Errorf("expected trigger.text=hello world, got: %s", out)
	}
}

func TestBuildBoardJSON_TaskWithArtifacts(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	task := &model.Task{Description: "writes a file"}
	s.PublishTask(task)
	s.ClaimTask("worker-1", task.ID)
	s.AppendArtifact(task.ID, "docs/result.md")
	s.SubmitResult("worker-1", task.ID, "done")

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventTaskCompleted}, SnapshotSources{})

	if !strings.Contains(out, "docs/result.md") {
		t.Errorf("expected artifact in snapshot, got: %s", out)
	}
}

func TestBuildBoardJSON_EmptyStore(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 2}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput}, SnapshotSources{})

	if !strings.Contains(out, `"worker_count": 2`) {
		t.Errorf("expected worker_count=2 in empty store snapshot, got: %s", out)
	}
}

// ---- Phase 3.1：Resources.Agents 与 SessionHistory 测试 ----

// parseSnapshot 解析 BuildBoardJSON 输出为可断言的结构体。
// 仅供测试使用，避免大量字符串包含断言。
func parseSnapshot(t *testing.T, s string) boardSnapshot {
	t.Helper()
	var bs boardSnapshot
	if err := json.Unmarshal([]byte(s), &bs); err != nil {
		t.Fatalf("snapshot 解析失败: %v\n%s", err, s)
	}
	return bs
}

func TestBuildBoardJSON_AgentsFromMailboxRegistry(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 2}

	mb := mailbox.NewRegistry(8)
	mb.Register("worker-1", "")
	mb.Register("worker-2", "")
	mb.Register("explorer-1", "explore")
	mb.Register("scheduler-x", "__scheduler__")

	// worker-2 收件箱有 1 条消息
	_ = mb.Send(mailbox.Message{From: "scheduler-x", To: "worker-2", Content: "hi"})

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventTickerWakeup}, SnapshotSources{
		MBRegistry: mb,
	})
	bs := parseSnapshot(t, out)

	if len(bs.Resources.Agents) != 4 {
		t.Fatalf("agents 数量=%d, want 4", len(bs.Resources.Agents))
	}

	byID := make(map[string]agentSnapshot)
	for _, a := range bs.Resources.Agents {
		byID[a.ID] = a
	}

	if byID["worker-1"].Type != "worker" {
		t.Errorf("worker-1.Type=%q, want worker", byID["worker-1"].Type)
	}
	if byID["explorer-1"].Type != "explorer" {
		t.Errorf("explorer-1.Type=%q, want explorer", byID["explorer-1"].Type)
	}
	if byID["scheduler-x"].Type != "scheduler" {
		t.Errorf("scheduler-x.Type=%q, want scheduler", byID["scheduler-x"].Type)
	}
	if byID["worker-2"].MailboxPending != 1 {
		t.Errorf("worker-2.MailboxPending=%d, want 1", byID["worker-2"].MailboxPending)
	}
	if byID["worker-1"].MailboxPending != 0 {
		t.Errorf("worker-1.MailboxPending=%d, want 0", byID["worker-1"].MailboxPending)
	}
}

func TestBuildBoardJSON_AgentsCurrentTaskMapping(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 2}

	mb := mailbox.NewRegistry(8)
	mb.Register("worker-1", "")
	mb.Register("worker-2", "")

	// worker-1 正在处理 t1
	t1 := &model.Task{Description: "task one"}
	s.PublishTask(t1)
	s.ClaimTask("worker-1", t1.ID)

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{MBRegistry: mb})
	bs := parseSnapshot(t, out)

	byID := make(map[string]agentSnapshot)
	for _, a := range bs.Resources.Agents {
		byID[a.ID] = a
	}

	if byID["worker-1"].CurrentTaskID != t1.ID {
		t.Errorf("worker-1.CurrentTaskID=%q, want %q", byID["worker-1"].CurrentTaskID, t1.ID)
	}
	if byID["worker-1"].CurrentTaskDesc != "task one" {
		t.Errorf("worker-1.CurrentTaskDesc=%q, want 'task one'", byID["worker-1"].CurrentTaskDesc)
	}
	if byID["worker-2"].CurrentTaskID != "" {
		t.Errorf("worker-2 should have no current task, got %q", byID["worker-2"].CurrentTaskID)
	}
}

func TestBuildBoardJSON_AgentsRosterLockedFiles(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	mb := mailbox.NewRegistry(8)
	mb.Register("worker-1", "")

	r := roster.NewMemoryRoster()
	if _, err := r.TryClaim("worker-1", "main.go"); err != nil {
		t.Fatalf("TryClaim main.go: %v", err)
	}
	if _, err := r.TryClaim("worker-1", "internal/foo.go"); err != nil {
		t.Fatalf("TryClaim internal/foo.go: %v", err)
	}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{MBRegistry: mb, Roster: r})
	bs := parseSnapshot(t, out)

	if len(bs.Resources.Agents) != 1 {
		t.Fatalf("agents=%d, want 1", len(bs.Resources.Agents))
	}
	got := bs.Resources.Agents[0].LockedFiles
	if len(got) != 2 {
		t.Fatalf("LockedFiles=%v, want 2 entries", got)
	}
	// 顺序由 roster 内部决定，断言"包含"即可
	wantSet := map[string]bool{"main.go": true, "internal/foo.go": true}
	for _, f := range got {
		if !wantSet[f] {
			t.Errorf("unexpected locked file: %q", f)
		}
	}
}

func TestBuildBoardJSON_AgentsNilMBRegistry_OmitsField(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{})
	if strings.Contains(out, `"agents"`) {
		t.Errorf("agents field should be omitted when MBRegistry is nil, got: %s", out)
	}
}

func TestBuildBoardJSON_SessionHistoryFromHistory(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	hist := NewSessionHistory(8)

	// 模拟两条用户输入
	hist.Append(SessionInput{
		Text:            "你好",
		SchedulerTaskID: "tid-1",
		SubmittedAt:     time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	})
	hist.Append(SessionInput{
		Text:            "读 main.go",
		SchedulerTaskID: "tid-2",
		SubmittedAt:     time.Date(2026, 4, 10, 12, 1, 0, 0, time.UTC),
	})

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{History: hist})
	bs := parseSnapshot(t, out)

	if len(bs.SessionHistory) != 2 {
		t.Fatalf("session_history len=%d, want 2", len(bs.SessionHistory))
	}
	if bs.SessionHistory[0].Text != "你好" {
		t.Errorf("entry[0].Text=%q, want '你好'", bs.SessionHistory[0].Text)
	}
	if bs.SessionHistory[1].SchedulerTaskID != "tid-2" {
		t.Errorf("entry[1].SchedulerTaskID=%q, want tid-2", bs.SessionHistory[1].SchedulerTaskID)
	}
	if !strings.Contains(bs.SessionHistory[0].SubmittedAt, "2026-04-10") {
		t.Errorf("SubmittedAt should be RFC3339, got %q", bs.SessionHistory[0].SubmittedAt)
	}
	// Outcome 应当为空（tid-1 / tid-2 在 store 中不存在）
	if bs.SessionHistory[0].Outcome != "" {
		t.Errorf("Outcome with no matching task should be empty, got %q", bs.SessionHistory[0].Outcome)
	}
}

func TestBuildBoardJSON_SessionHistoryOutcomeFromStore(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	// 真实发布一个 task 到 store，让 outcome 能被查到
	task := &model.Task{Description: "real", EventType: "__scheduler__"}
	s.PublishTask(task)
	s.ClaimTask("scheduler-1", task.ID)

	hist := NewSessionHistory(4)
	hist.Append(SessionInput{
		Text:            "real input",
		SchedulerTaskID: task.ID,
		SubmittedAt:     time.Now(),
	})

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{History: hist})
	bs := parseSnapshot(t, out)

	if len(bs.SessionHistory) != 1 {
		t.Fatalf("session_history len=%d, want 1", len(bs.SessionHistory))
	}
	if bs.SessionHistory[0].Outcome != "processing" {
		t.Errorf("Outcome=%q, want 'processing'", bs.SessionHistory[0].Outcome)
	}
}

func TestBuildBoardJSON_SessionHistoryNil_OmitsField(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{WorkerCount: 1}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{})
	if strings.Contains(out, `"session_history"`) {
		t.Errorf("session_history should be omitted when History is nil, got: %s", out)
	}
}

func TestAgentTypeFromEventType(t *testing.T) {
	cases := map[string]string{
		"":              "worker",
		"explore":       "explorer",
		"__scheduler__": "scheduler",
		"random":        "unknown",
	}
	for in, want := range cases {
		if got := agentTypeFromEventType(in); got != want {
			t.Errorf("agentTypeFromEventType(%q)=%q, want %q", in, got, want)
		}
	}
}
