package scheduler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/probe"
	"agentgo/internal/roster"
	"agentgo/internal/store"

	"pgregory.net/rapid"
)

// 这些测试从旧的 internal/scheduler/scheduler_test.go::TestScheduler_BuildBoardJSON_*
// 迁移而来。原测试调用 sched.buildBoardJSON 私有方法，现在直接测公开 helper
// BuildBoardJSON。

func TestBuildBoardJSON_Resources(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 4}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{})
	if strings.Contains(out, `"agents"`) {
		t.Errorf("agents field should be omitted when MBRegistry is nil, got: %s", out)
	}
}

func TestBuildBoardJSON_SessionHistoryFromHistory(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

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
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

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

// ---- Phase agent-capability-declaration：agent_capabilities 测试 ----

func TestBuildBoardJSON_AgentCapabilities_WorkerAndSpecialized(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	reg := NewAgentRegistry()
	reg.Register(SpecializedAgent{
		EventType:    "explore",
		Count:        1,
		Role:         "read-only investigator",
		Capabilities: []string{"codebase_read", "web_search"},
	})

	workerCaps := &AgentCapabilityInfo{
		Capabilities: []string{"code_edit", "shell_exec"},
		Description:  "general worker",
	}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput}, SnapshotSources{
		AgentRegistry:      reg,
		WorkerCapabilities: workerCaps,
	})
	bs := parseSnapshot(t, out)

	if len(bs.Resources.AgentCapabilities) != 2 {
		t.Fatalf("agent_capabilities len=%d, want 2", len(bs.Resources.AgentCapabilities))
	}
	// Worker 排第一
	if bs.Resources.AgentCapabilities[0].AgentType != "worker" {
		t.Errorf("first entry agent_type=%q, want worker", bs.Resources.AgentCapabilities[0].AgentType)
	}
	if bs.Resources.AgentCapabilities[0].Description != "general worker" {
		t.Errorf("worker description=%q, want 'general worker'", bs.Resources.AgentCapabilities[0].Description)
	}
	if bs.Resources.AgentCapabilities[1].AgentType != "explore" {
		t.Errorf("second entry agent_type=%q, want explore", bs.Resources.AgentCapabilities[1].AgentType)
	}
}

func TestBuildBoardJSON_AgentCapabilities_NilWorker(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	reg := NewAgentRegistry()
	reg.Register(SpecializedAgent{
		EventType:    "explore",
		Count:        1,
		Role:         "explorer",
		Capabilities: []string{"read"},
	})

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{
		AgentRegistry:      reg,
		WorkerCapabilities: nil,
	})
	bs := parseSnapshot(t, out)

	if len(bs.Resources.AgentCapabilities) != 1 {
		t.Fatalf("agent_capabilities len=%d, want 1", len(bs.Resources.AgentCapabilities))
	}
	if bs.Resources.AgentCapabilities[0].AgentType != "explore" {
		t.Errorf("agent_type=%q, want explore", bs.Resources.AgentCapabilities[0].AgentType)
	}
}

func TestBuildBoardJSON_AgentCapabilities_BothNil_OmitsField(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{})
	if strings.Contains(out, `"agent_capabilities"`) {
		t.Errorf("agent_capabilities should be omitted when both nil, got: %s", out)
	}
}

func TestBuildBoardJSON_AgentCapabilities_EmptyCapsAndDesc(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	workerCaps := &AgentCapabilityInfo{
		Capabilities: []string{},
		Description:  "",
	}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{}, SnapshotSources{
		WorkerCapabilities: workerCaps,
	})
	bs := parseSnapshot(t, out)

	if len(bs.Resources.AgentCapabilities) != 1 {
		t.Fatalf("agent_capabilities len=%d, want 1", len(bs.Resources.AgentCapabilities))
	}
	// Empty capabilities should appear as empty array, not nil
	if bs.Resources.AgentCapabilities[0].Capabilities == nil {
		t.Errorf("empty capabilities should be [] not nil in JSON")
	}
	if len(bs.Resources.AgentCapabilities[0].Capabilities) != 0 {
		t.Errorf("capabilities len=%d, want 0", len(bs.Resources.AgentCapabilities[0].Capabilities))
	}
	if bs.Resources.AgentCapabilities[0].Description != "" {
		t.Errorf("description=%q, want empty", bs.Resources.AgentCapabilities[0].Description)
	}
}

// Feature: agent-capability-declaration, Property 3: snapshot agent_capabilities completeness
// **Validates: Requirements 3.1, 3.2, 3.3, 3.4, 6.3, 6.4**
//
// 使用 rapid 生成随机 AgentRegistry 内容和 WorkerCapabilities，
// 验证 buildAgentCapabilities 输出的完整性和一致性。
func TestProperty_BoardSnapshotAgentCapabilities(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// --- 生成随机 AgentRegistry ---
		reg := NewAgentRegistry()
		numSpecialized := rapid.IntRange(0, 5).Draw(t, "numSpecialized")

		type specEntry struct {
			eventType string
			caps      []string
			role      string
		}
		registeredEntries := make([]specEntry, 0, numSpecialized)

		for i := range numSpecialized {
			et := rapid.StringMatching(`[a-z]{2,8}`).Draw(t, fmt.Sprintf("et_%d", i))

			// Random capabilities: nil / empty / non-empty
			var caps []string
			capsChoice := rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("capsChoice_%d", i))
			switch capsChoice {
			case 0:
				caps = nil
			case 1:
				caps = []string{}
			case 2:
				n := rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("capsLen_%d", i))
				caps = make([]string, n)
				for j := range n {
					caps[j] = rapid.StringMatching(`[a-z_]{1,12}`).Draw(t, fmt.Sprintf("cap_%d_%d", i, j))
				}
			}

			role := rapid.StringMatching(`[a-zA-Z ]{0,30}`).Draw(t, fmt.Sprintf("role_%d", i))

			reg.Register(SpecializedAgent{
				EventType:    et,
				Count:        1,
				Capabilities: caps,
				Role:         role,
			})
			registeredEntries = append(registeredEntries, specEntry{
				eventType: et,
				caps:      caps,
				role:      role,
			})
		}

		// --- 生成随机 WorkerCapabilities (nil / non-nil) ---
		var workerCaps *AgentCapabilityInfo
		hasWorker := rapid.Bool().Draw(t, "hasWorker")
		var expectedWorkerCaps []string
		var expectedWorkerDesc string
		if hasWorker {
			capsChoice := rapid.IntRange(0, 2).Draw(t, "workerCapsChoice")
			switch capsChoice {
			case 0:
				expectedWorkerCaps = nil
			case 1:
				expectedWorkerCaps = []string{}
			case 2:
				n := rapid.IntRange(1, 5).Draw(t, "workerCapsLen")
				expectedWorkerCaps = make([]string, n)
				for j := range n {
					expectedWorkerCaps[j] = rapid.StringMatching(`[a-z_]{1,12}`).Draw(t, fmt.Sprintf("workerCap_%d", j))
				}
			}
			expectedWorkerDesc = rapid.StringMatching(`[a-zA-Z ]{0,40}`).Draw(t, "workerDesc")
			workerCaps = &AgentCapabilityInfo{
				Capabilities: expectedWorkerCaps,
				Description:  expectedWorkerDesc,
			}
		}

		// --- 调用被测函数 ---
		result := buildAgentCapabilities(reg, workerCaps, nil)

		// --- 验证 ---
		specializedEntries := reg.Specialized()

		// Property: 当 workerCaps 为 nil 且 registry 为空时，返回 nil
		if workerCaps == nil && len(specializedEntries) == 0 {
			if result != nil {
				t.Fatalf("expected nil when both worker and registry empty, got %d entries", len(result))
			}
			return
		}

		// Property: 结果长度 = (worker ? 1 : 0) + len(specializedEntries)
		expectedLen := len(specializedEntries)
		if hasWorker {
			expectedLen++
		}
		if len(result) != expectedLen {
			t.Fatalf("result len=%d, want %d (hasWorker=%v, specialized=%d)",
				len(result), expectedLen, hasWorker, len(specializedEntries))
		}

		idx := 0

		// Property: Worker 记录始终排在第一位（当 workerCaps 非 nil 时）
		if hasWorker {
			w := result[0]
			if w.AgentType != "worker" {
				t.Errorf("first entry agent_type=%q, want worker", w.AgentType)
			}
			if w.Description != expectedWorkerDesc {
				t.Errorf("worker description=%q, want %q", w.Description, expectedWorkerDesc)
			}
			// Capabilities 一致性
			if expectedWorkerCaps == nil {
				if w.Capabilities != nil {
					t.Errorf("worker capabilities should be nil, got %v", w.Capabilities)
				}
			} else {
				if len(w.Capabilities) != len(expectedWorkerCaps) {
					t.Errorf("worker capabilities len=%d, want %d", len(w.Capabilities), len(expectedWorkerCaps))
				}
				for j, c := range w.Capabilities {
					if j < len(expectedWorkerCaps) && c != expectedWorkerCaps[j] {
						t.Errorf("worker capabilities[%d]=%q, want %q", j, c, expectedWorkerCaps[j])
					}
				}
			}
			idx = 1
		}

		// Property: 对 AgentRegistry 中每个特化代理，结果中包含对应记录
		for i, se := range specializedEntries {
			r := result[idx+i]
			if r.AgentType != se.EventType {
				t.Errorf("entry[%d] agent_type=%q, want %q", idx+i, r.AgentType, se.EventType)
			}
			if r.Description != se.Role {
				t.Errorf("entry[%d] description=%q, want %q", idx+i, r.Description, se.Role)
			}
			// Capabilities 一致性
			if se.Capabilities == nil {
				if r.Capabilities != nil {
					t.Errorf("entry[%d] capabilities should be nil, got %v", idx+i, r.Capabilities)
				}
			} else {
				if len(r.Capabilities) != len(se.Capabilities) {
					t.Errorf("entry[%d] capabilities len=%d, want %d", idx+i, len(r.Capabilities), len(se.Capabilities))
				}
				for j, c := range r.Capabilities {
					if j < len(se.Capabilities) && c != se.Capabilities[j] {
						t.Errorf("entry[%d] capabilities[%d]=%q, want %q", idx+i, j, c, se.Capabilities[j])
					}
				}
			}
		}

		// Property: 结果中不包含未注册且非 worker 的代理类型
		validTypes := make(map[string]bool)
		if hasWorker {
			validTypes["worker"] = true
		}
		for _, se := range specializedEntries {
			validTypes[se.EventType] = true
		}
		for _, r := range result {
			if !validTypes[r.AgentType] {
				t.Errorf("unexpected agent_type %q in result", r.AgentType)
			}
		}
	})
}

// Feature: tool-health-probe, Property 6: Board Snapshot unavailable_tools rendering correctness
// *For any* valid ToolHealthStatus and board snapshot inputs, when unavailable tools exist,
// parsed JSON resources.unavailable_tools equals ToolHealthStatus.UnavailableTools();
// when nil or all available, JSON does not contain unavailable_tools key.
// **Validates: Requirements 4.1, 4.2, 5.4, 7.1, 7.5**
func TestProperty_SnapshotUnavailableTools(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// --- Build a minimal task store + config ---
		ch := make(chan model.Event, 64)
		s := store.NewMemoryTaskStore(ch, 100, 2, 300)
		cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

		// --- Generate random ToolHealthStatus: nil, empty, all-available, or mixed ---
		choice := rapid.IntRange(0, 3).Draw(t, "toolHealthChoice")

		var toolHealth *probe.ToolHealthStatus

		switch choice {
		case 0:
			// nil ToolHealthStatus
			toolHealth = nil

		case 1:
			// Empty ToolHealthStatus (no results recorded)
			toolHealth = probe.NewToolHealthStatus()

		case 2:
			// All tools available
			toolHealth = probe.NewToolHealthStatus()
			numTools := rapid.IntRange(1, 6).Draw(t, "numAvailable")
			for i := range numTools {
				name := rapid.StringMatching(`[a-z_]{2,12}`).Draw(t, fmt.Sprintf("availTool_%d", i))
				toolHealth.Record(probe.ProbeResult{
					Tool:      name,
					Available: true,
					Latency:   time.Duration(rapid.IntRange(1, 5000).Draw(t, fmt.Sprintf("lat_%d", i))) * time.Millisecond,
				})
			}

		case 3:
			// Mixed: some available, some unavailable
			toolHealth = probe.NewToolHealthStatus()
			numTools := rapid.IntRange(1, 8).Draw(t, "numMixed")
			for i := range numTools {
				name := rapid.StringMatching(`[a-z_]{2,12}`).Draw(t, fmt.Sprintf("mixTool_%d", i))
				avail := rapid.Bool().Draw(t, fmt.Sprintf("mixAvail_%d", i))
				errMsg := ""
				if !avail {
					errMsg = rapid.StringMatching(`[a-zA-Z0-9 ]{5,30}`).Draw(t, fmt.Sprintf("mixErr_%d", i))
				}
				toolHealth.Record(probe.ProbeResult{
					Tool:      name,
					Available: avail,
					Error:     errMsg,
					Latency:   time.Duration(rapid.IntRange(1, 5000).Draw(t, fmt.Sprintf("mixLat_%d", i))) * time.Millisecond,
				})
			}
		}

		// --- Call BuildBoardJSON ---
		out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventTickerWakeup}, SnapshotSources{
			ToolHealth: toolHealth,
		})

		// --- Parse and verify ---
		var bs boardSnapshot
		if err := json.Unmarshal([]byte(out), &bs); err != nil {
			t.Fatalf("snapshot JSON parse failed: %v\n%s", err, out)
		}

		expectedUnavailable := toolHealth.UnavailableTools() // nil-safe

		if len(expectedUnavailable) > 0 {
			// Property: when unavailable tools exist, parsed field matches UnavailableTools()
			if len(bs.Resources.UnavailableTools) != len(expectedUnavailable) {
				t.Fatalf("unavailable_tools len=%d, want %d\nJSON: %s",
					len(bs.Resources.UnavailableTools), len(expectedUnavailable), out)
			}
			for i, name := range expectedUnavailable {
				if bs.Resources.UnavailableTools[i] != name {
					t.Errorf("unavailable_tools[%d]=%q, want %q", i, bs.Resources.UnavailableTools[i], name)
				}
			}
		} else {
			// Property: when nil or all available, JSON must NOT contain "unavailable_tools" key
			if len(bs.Resources.UnavailableTools) != 0 {
				t.Errorf("expected no unavailable_tools, got %v", bs.Resources.UnavailableTools)
			}
			// Also verify at the raw JSON level that the key is absent (omitempty)
			if strings.Contains(out, `"unavailable_tools"`) {
				t.Errorf("unavailable_tools key should be absent from JSON when all tools available or nil\nJSON: %s", out)
			}
		}
	})
}

// ---- Task 7.5: BuildBoardJSON backward compatibility ----

func TestBuildBoardJSON_BackwardCompat_NilToolHealth(t *testing.T) {
	// When ToolHealth is nil (default zero value of SnapshotSources),
	// the JSON output must NOT contain "unavailable_tools" — identical
	// to the output before the tool-health-probe feature was introduced.
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput}, SnapshotSources{})
	if strings.Contains(out, "unavailable_tools") {
		t.Errorf("unavailable_tools must be absent when ToolHealth is nil (backward compat)\nJSON: %s", out)
	}

	bs := parseSnapshot(t, out)
	if len(bs.Resources.UnavailableTools) != 0 {
		t.Errorf("parsed UnavailableTools should be empty, got %v", bs.Resources.UnavailableTools)
	}
}

func TestBuildBoardJSON_BackwardCompat_AllToolsAvailable(t *testing.T) {
	// When ToolHealth is non-nil but every recorded tool is available,
	// UnavailableTools() returns nil → omitempty omits the field.
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}

	th := probe.NewToolHealthStatus()
	th.Record(probe.ProbeResult{Tool: "web_search", Available: true, Latency: 100 * time.Millisecond})
	th.Record(probe.ProbeResult{Tool: "web_fetch", Available: true, Latency: 200 * time.Millisecond})

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventTickerWakeup}, SnapshotSources{
		ToolHealth: th,
	})
	if strings.Contains(out, "unavailable_tools") {
		t.Errorf("unavailable_tools must be absent when all tools are available (omitempty)\nJSON: %s", out)
	}

	bs := parseSnapshot(t, out)
	if len(bs.Resources.UnavailableTools) != 0 {
		t.Errorf("parsed UnavailableTools should be empty, got %v", bs.Resources.UnavailableTools)
	}
}

// Feature: per-worker-tool-profiles, Property: per-profile agent_capabilities output correctness
// **Validates: Requirements 3.2**
//
// 使用 rapid 生成随机 WorkerCapabilitiesByProfile（1-5 个不同 profile，每个有随机 capabilities），
// 验证 BuildBoardJSON 输出的 JSON 中 agent_capabilities 数组包含每个 profile 的记录，
// 且每条记录的 agent_type 为 "worker"、profile 字段正确、capabilities 匹配。
func TestProperty_PerProfileAgentCapabilitiesOutput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// --- 构造最小 task store + config ---
		ch := make(chan model.Event, 64)
		s := store.NewMemoryTaskStore(ch, 100, 2, 300)
		cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

		// --- 生成随机 WorkerCapabilitiesByProfile（1-5 个 profile）---
		numProfiles := rapid.IntRange(1, 5).Draw(t, "numProfiles")
		capsByProfile := make(map[string]*AgentCapabilityInfo, numProfiles)

		type expectedEntry struct {
			profile      string
			capabilities []string
			description  string
		}
		expected := make([]expectedEntry, 0, numProfiles)

		for i := range numProfiles {
			profileName := rapid.StringMatching(`[a-z_]{3,12}`).Draw(t, fmt.Sprintf("profile_%d", i))
			// Ensure unique profile names by appending index if collision
			if _, exists := capsByProfile[profileName]; exists {
				profileName = fmt.Sprintf("%s_%d", profileName, i)
			}

			// Generate random capabilities (0-5 items)
			numCaps := rapid.IntRange(0, 5).Draw(t, fmt.Sprintf("numCaps_%d", i))
			caps := make([]string, numCaps)
			for j := range numCaps {
				caps[j] = rapid.StringMatching(`[a-z_]{2,15}`).Draw(t, fmt.Sprintf("cap_%d_%d", i, j))
			}

			desc := rapid.StringMatching(`[a-zA-Z ]{0,30}`).Draw(t, fmt.Sprintf("desc_%d", i))

			capsByProfile[profileName] = &AgentCapabilityInfo{
				Capabilities: caps,
				Description:  desc,
			}
			expected = append(expected, expectedEntry{
				profile:      profileName,
				capabilities: caps,
				description:  desc,
			})
		}

		// --- 调用 BuildBoardJSON ---
		out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput}, SnapshotSources{
			WorkerCapabilitiesByProfile: capsByProfile,
		})

		// --- 解析 JSON ---
		var bs boardSnapshot
		if err := json.Unmarshal([]byte(out), &bs); err != nil {
			t.Fatalf("snapshot JSON parse failed: %v\n%s", err, out)
		}

		// --- Property: agent_capabilities 数组包含每个 profile 的记录 ---
		if len(bs.Resources.AgentCapabilities) != len(capsByProfile) {
			t.Fatalf("agent_capabilities len=%d, want %d (one per profile)",
				len(bs.Resources.AgentCapabilities), len(capsByProfile))
		}

		// 构建 profile → agentCapabilitySnapshot 映射，方便查找
		byProfile := make(map[string]agentCapabilitySnapshot, len(bs.Resources.AgentCapabilities))
		for _, ac := range bs.Resources.AgentCapabilities {
			byProfile[ac.Profile] = ac
		}

		// --- Property: 每条记录的 agent_type 为 "worker"、profile 字段正确、capabilities 匹配 ---
		for profileName, info := range capsByProfile {
			ac, ok := byProfile[profileName]
			if !ok {
				t.Fatalf("missing agent_capabilities record for profile %q", profileName)
			}

			// agent_type must be "worker"
			if ac.AgentType != "worker" {
				t.Errorf("profile %q: agent_type=%q, want \"worker\"", profileName, ac.AgentType)
			}

			// profile field must match
			if ac.Profile != profileName {
				t.Errorf("profile %q: profile field=%q, want %q", profileName, ac.Profile, profileName)
			}

			// capabilities must match
			if len(ac.Capabilities) != len(info.Capabilities) {
				t.Errorf("profile %q: capabilities len=%d, want %d",
					profileName, len(ac.Capabilities), len(info.Capabilities))
			} else {
				for j, c := range ac.Capabilities {
					if c != info.Capabilities[j] {
						t.Errorf("profile %q: capabilities[%d]=%q, want %q",
							profileName, j, c, info.Capabilities[j])
					}
				}
			}

			// description must match
			if ac.Description != info.Description {
				t.Errorf("profile %q: description=%q, want %q",
					profileName, ac.Description, info.Description)
			}
		}
	})
}

// ---- Task 5.8: Board Snapshot backward compatibility (per-worker-tool-profiles) ----

func TestBuildBoardJSON_BackwardCompat_NoProfileFields(t *testing.T) {
	// When WorkerProfiles=nil and WorkerCapabilitiesByProfile=nil,
	// the output must be identical to pre-modification behavior:
	// - agents entries must NOT contain "profile" field
	// - agent_capabilities entries must NOT contain "profile" field
	// - Only the old-style single worker record appears in agent_capabilities
	//
	// Validates: Requirements 3.3
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}

	// Set up mailbox registry so agents section is populated
	mb := mailbox.NewRegistry(8)
	mb.Register("worker-1", "")
	mb.Register("worker-2", "")
	mb.Register("explorer-1", "explore")

	// Set up old-style single WorkerCapabilities (no per-profile)
	workerCaps := &AgentCapabilityInfo{
		Capabilities: []string{"code_edit", "shell_exec", "web_search"},
		Description:  "general worker",
	}

	// Specialized agent in registry
	reg := NewAgentRegistry()
	reg.Register(SpecializedAgent{
		EventType:    "explore",
		Count:        1,
		Role:         "read-only investigator",
		Capabilities: []string{"codebase_read", "web_search"},
	})

	// Construct SnapshotSources with ONLY old fields — no WorkerProfiles, no WorkerCapabilitiesByProfile
	sources := SnapshotSources{
		MBRegistry:         mb,
		AgentRegistry:      reg,
		WorkerCapabilities: workerCaps,
		// WorkerProfiles:              nil  (zero value)
		// WorkerCapabilitiesByProfile: nil  (zero value)
	}

	out := BuildBoardJSON(s, cfg, "immediate", model.Event{Type: model.EventUserInput}, sources)
	bs := parseSnapshot(t, out)

	// --- Verify agents do NOT have "profile" field ---
	// Since Profile is omitempty, nil/empty means it won't appear in JSON.
	// Check both parsed struct and raw JSON.
	for _, a := range bs.Resources.Agents {
		if a.Profile != "" {
			t.Errorf("agent %q has profile=%q, want empty (backward compat)", a.ID, a.Profile)
		}
	}
	// Raw JSON check: "profile" should not appear inside any agent object
	// We check that no agent block contains a profile key
	if strings.Contains(out, `"profile"`) {
		t.Errorf("JSON output should not contain \"profile\" key when WorkerProfiles=nil and WorkerCapabilitiesByProfile=nil\nJSON: %s", out)
	}

	// --- Verify agent_capabilities uses old-style single worker record (no profile field) ---
	if len(bs.Resources.AgentCapabilities) != 2 {
		t.Fatalf("agent_capabilities len=%d, want 2 (1 worker + 1 explorer)", len(bs.Resources.AgentCapabilities))
	}

	// Worker record should be first, with no profile
	wCap := bs.Resources.AgentCapabilities[0]
	if wCap.AgentType != "worker" {
		t.Errorf("first agent_capabilities entry agent_type=%q, want \"worker\"", wCap.AgentType)
	}
	if wCap.Profile != "" {
		t.Errorf("worker agent_capabilities has profile=%q, want empty (backward compat)", wCap.Profile)
	}
	if wCap.Description != "general worker" {
		t.Errorf("worker description=%q, want \"general worker\"", wCap.Description)
	}
	if len(wCap.Capabilities) != 3 {
		t.Errorf("worker capabilities len=%d, want 3", len(wCap.Capabilities))
	}

	// Explorer record should be second, also no profile
	eCap := bs.Resources.AgentCapabilities[1]
	if eCap.AgentType != "explore" {
		t.Errorf("second agent_capabilities entry agent_type=%q, want \"explore\"", eCap.AgentType)
	}
	if eCap.Profile != "" {
		t.Errorf("explorer agent_capabilities has profile=%q, want empty", eCap.Profile)
	}
}

