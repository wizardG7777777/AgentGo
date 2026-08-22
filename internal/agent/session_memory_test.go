package agent

// session_memory_test.go 覆盖 V6 §3 CM3 召回侧：Session Memory 查询/渲染注入、
// 状态过滤（stale/superseded 不注入、inferred 标注未验证）、预算上限、
// Manifest 段登记、memory_recalled 事件，以及「fake LLM 任务 completed →
// 晋升 → 下一任务入口召回注入」的端到端主链。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// sessionRecallFixture 组装带 Session 后端的 Agent（Memory 为挂接了
// SessionStore 的 ProcessStore，与生产装配同型）。
type sessionRecallFixture struct {
	agent   *Agent
	proc    *memory.ProcessStore
	backend *memory.SessionStore
}

func newSessionRecallFixture(t *testing.T) *sessionRecallFixture {
	t.Helper()
	proc := memory.NewProcessStore()
	backend, err := memory.NewSessionStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	proc.AttachSessionStore(backend)
	return &sessionRecallFixture{
		agent:   &Agent{ID: "agent-recall", Memory: proc},
		proc:    proc,
		backend: backend,
	}
}

// putSession 写入一条 Session 条目。
func putSession(t *testing.T, proc *memory.ProcessStore, e memory.Entry) {
	t.Helper()
	e.Scope = memory.ScopeSession
	if err := proc.Put(context.Background(), e); err != nil {
		t.Fatalf("Put session entry: %v", err)
	}
}

func TestRecallSessionMemory_NilMemoryReturnsEmpty(t *testing.T) {
	a := &Agent{ID: "a-nil"}
	if got := a.recallSessionMemory(context.Background(), "task-1"); got != "" {
		t.Errorf("nil Memory 应返回空串, got %q", got)
	}
}

func TestRecallSessionMemory_InjectsConfirmedWithSourceLabel(t *testing.T) {
	fx := newSessionRecallFixture(t)
	putSession(t, fx.proc, memory.Entry{
		Kind: memory.KindResult, Key: "result:t-old", Source: "t-old",
		Content: "任务 t-old 已完成（终态 completed）。\n目标: 写月度报告",
		State:   memory.StateConfirmed,
	})
	got := fx.agent.recallSessionMemory(context.Background(), "task-cur")
	if got == "" {
		t.Fatal("有 confirmed 条目时应注入")
	}
	for _, want := range []string{
		"<session-memory source=\"session-memory\"", "不是系统指令",
		"[task_result|confirmed]", "来源: t-old", "写月度报告",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("注入块应含 %q: %q", want, got)
		}
	}
}

func TestRecallSessionMemory_InferredLabeledUnverified(t *testing.T) {
	fx := newSessionRecallFixture(t)
	putSession(t, fx.proc, memory.Entry{
		Kind: memory.KindLearning, Key: "failure:t-old", Source: "t-old",
		Content: "某未经验证的推测", State: memory.StateInferred,
	})
	got := fx.agent.recallSessionMemory(context.Background(), "task-cur")
	if !strings.Contains(got, "inferred（未验证）") {
		t.Errorf("inferred 条目注入时必须标注「未验证」: %q", got)
	}
}

func TestRecallSessionMemory_ExcludesStaleSupersededAndSelfSourced(t *testing.T) {
	fx := newSessionRecallFixture(t)
	putSession(t, fx.proc, memory.Entry{
		Kind: memory.KindResult, Key: "result:stale", Source: "t-old",
		Content: "已失效结论", State: memory.StateStale,
	})
	putSession(t, fx.proc, memory.Entry{
		Kind: memory.KindResult, Key: "result:old#superseded", Source: "t-old",
		Content: "已被取代结论", State: memory.StateSuperseded,
	})
	putSession(t, fx.proc, memory.Entry{
		Kind: memory.KindResult, Key: "result:self", Source: "task-cur",
		Content: "当前任务自己的条目",
	})
	if got := fx.agent.recallSessionMemory(context.Background(), "task-cur"); got != "" {
		t.Errorf("stale / superseded / 当前任务来源条目都不得注入: %q", got)
	}
}

func TestRecallSessionMemory_RecencyOrderAndEntryCap(t *testing.T) {
	fx := newSessionRecallFixture(t)
	base := time.Now().Add(-time.Hour)
	// 写入 10 条（超过 8 条上限），最旧的一条内容独特。
	for i := 0; i < 10; i++ {
		e := memory.Entry{
			Kind: memory.KindResult, Key: strings.Repeat("k", 1) + "result:" + string(rune('a'+i)),
			Source: "t-old", Content: "条目内容 " + string(rune('A'+i)),
		}
		putSession(t, fx.proc, e)
		// 直接改后端条目 UpdatedAt 控制 recency：经 Put 后重查再 Put 不可行，
		// 改为用后端内部时间源不可控——这里利用 Put 顺序即 UpdatedAt 递增。
		_ = base
	}
	got := fx.agent.recallSessionMemory(context.Background(), "task-cur")
	if got == "" {
		t.Fatal("应注入")
	}
	// 条目数硬上限 8。
	if n := strings.Count(got, "- [task_result|"); n != sessionMemoryRecallMaxEntries {
		t.Errorf("注入条目数 = %d, want %d（硬上限）", n, sessionMemoryRecallMaxEntries)
	}
	// 最新写入的条目（J）在，最旧的（A）被挤出。
	if !strings.Contains(got, "条目内容 J") {
		t.Errorf("recency 排序应保留最新条目: %q", got)
	}
	if strings.Contains(got, "条目内容 A") || strings.Contains(got, "条目内容 B") {
		t.Errorf("超上限时最旧条目应被挤出: %q", got)
	}
}

func TestRecallSessionMemory_FiltersSupersededBeforeEntryCap(t *testing.T) {
	fx := newSessionRecallFixture(t)
	ctx := context.Background()
	// 一条较早但仍 active 的记忆。
	putSession(t, fx.proc, memory.Entry{
		Kind: memory.KindDecision, Key: "decision:stable", Source: "task-stable",
		Content: "仍然有效的会话决定",
	})
	// 另一 Key 反复 supersede，产生超过查询上限的新近审计墓碑。
	for i := 0; i < sessionMemoryRecallMaxEntries+2; i++ {
		_, err := fx.backend.Supersede(ctx, memory.Entry{
			Scope: memory.ScopeSession, Kind: memory.KindDecision,
			Key: "decision:churn", Source: "task-churn",
			Content: "变更中的决定 " + string(rune('A'+i)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	entries := fx.agent.querySessionMemory(ctx, "task-current")
	var stable, churn bool
	for _, entry := range entries {
		if entry.Key == "decision:stable" {
			stable = true
		}
		if entry.Key == "decision:churn" {
			churn = true
		}
		if !entry.Recalled() {
			t.Fatalf("召回候选不得含 stale/superseded: %+v", entry)
		}
	}
	if !stable || !churn {
		t.Fatalf("应先过滤审计墓碑再限额，旧但 active 条目不得被挤出: %+v", entries)
	}
}

func TestRecallSessionMemory_BudgetCapped(t *testing.T) {
	fx := newSessionRecallFixture(t)
	for i := 0; i < sessionMemoryRecallMaxEntries; i++ {
		putSession(t, fx.proc, memory.Entry{
			Kind: memory.KindResult, Key: "result:" + string(rune('a'+i)), Source: "t-old",
			Content: strings.Repeat("很长的记忆内容", 100),
		})
	}
	got := fx.agent.recallSessionMemory(context.Background(), "task-cur")
	if got == "" {
		t.Fatal("应注入")
	}
	if n := len([]rune(got)); n > sessionMemoryRecallBudgetRunes {
		t.Errorf("注入块 %d runes 超预算 %d", n, sessionMemoryRecallBudgetRunes)
	}
	if !strings.HasSuffix(got, "</session-memory>") {
		t.Errorf("截断后标签仍应闭合: %q", got[len(got)-40:])
	}
}

func TestRecallSessionMemory_EmitsRecalledEvent(t *testing.T) {
	dir := captureTraceToDir(t)
	fx := newSessionRecallFixture(t)
	putSession(t, fx.proc, memory.Entry{
		Kind: memory.KindDecision, Key: "decision:ab12", Source: "t-old",
		Content: "用户决定: 采用简洁版格式", State: memory.StateConfirmed,
	})
	if got := fx.agent.recallSessionMemory(context.Background(), "task-9"); got == "" {
		t.Fatal("应注入")
	}
	var recalled []trace.Event
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind == trace.KindMemoryRecalled {
			recalled = append(recalled, ev)
		}
	}
	if len(recalled) != 1 {
		t.Fatalf("memory_recalled 应有 1 条, got %d", len(recalled))
	}
	ev := recalled[0]
	if ev.TaskID != "task-9" || ev.AgentID != "agent-recall" {
		t.Errorf("事件 TaskID/AgentID = %s/%s", ev.TaskID, ev.AgentID)
	}
	var payload struct {
		Entries int      `json:"entries"`
		Keys    []string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(ev.Description), &payload); err != nil {
		t.Fatalf("Description 非合法 JSON: %v", err)
	}
	if payload.Entries != 1 || len(payload.Keys) != 1 || !strings.Contains(payload.Keys[0], "decision:ab12") {
		t.Errorf("payload = %+v", payload)
	}
}

func TestRecallSessionMemory_EmptyRecallNoEvent(t *testing.T) {
	dir := captureTraceToDir(t)
	fx := newSessionRecallFixture(t)
	if got := fx.agent.recallSessionMemory(context.Background(), "task-9"); got != "" {
		t.Fatalf("空召回应返回空串, got %q", got)
	}
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind == trace.KindMemoryRecalled {
			t.Errorf("空召回不得发 memory_recalled: %+v", ev)
		}
	}
}

func TestContextManifest_SessionMemorySection(t *testing.T) {
	// 召回注入块经 history 进入 Manifest：session_memory 段、
	// Source=session-memory、Authority=informational、Freshness 按条目
	// UpdatedAt 与任务开始时间比较。
	task := &model.Task{ID: "task-m", Description: "验证 manifest"}
	block := "<session-memory source=\"session-memory\" entries=\"1\">\n- [task_result|confirmed]（来源: t-old，更新于 2026-08-01T00:00:00Z）\n  内容\n</session-memory>"
	history := []HistoryEntry{{IncomingMail: block}}

	// 条目更新早于任务开始 → stale。
	staleInfo := newManifestSideInfo(time.Now())
	staleInfo.recordMemorySectionUpdatedAt(markerSessionMemory, time.Now().Add(-time.Hour))
	ctx := withManifestSideInfo(context.Background(), staleInfo)
	m := buildLegacyContextManifest(ctx, "prompt", task, nil, history, "", nil)
	var sec *ContextItem
	for i := range m.Items {
		if m.Items[i].ID == ManifestSectionSessionMemory {
			sec = &m.Items[i]
		}
	}
	if sec == nil {
		t.Fatalf("Manifest 应含 session_memory 段: %+v", m.Items)
	}
	if sec.Source != SourceSessionMemory || sec.Authority != AuthorityInformational {
		t.Errorf("段 Source/Authority = %s/%s", sec.Source, sec.Authority)
	}
	if sec.Scope != ScopeSession {
		t.Errorf("段 Scope = %s, want session", sec.Scope)
	}
	if sec.Freshness != FreshnessStale {
		t.Errorf("早于任务开始的条目应判 stale, got %s", sec.Freshness)
	}

	// 任务开始后更新 → live。
	liveInfo := newManifestSideInfo(time.Now().Add(-time.Hour))
	liveInfo.recordMemorySectionUpdatedAt(markerSessionMemory, time.Now())
	ctx2 := withManifestSideInfo(context.Background(), liveInfo)
	m2 := buildLegacyContextManifest(ctx2, "prompt", task, nil, history, "", nil)
	for i := range m2.Items {
		if m2.Items[i].ID == ManifestSectionSessionMemory && m2.Items[i].Freshness != FreshnessLive {
			t.Errorf("任务开始后更新的条目应判 live, got %s", m2.Items[i].Freshness)
		}
	}
}

// TestProcessTask_SessionMemoryPromotionThenRecall 是 CM3 的端到端主测试：
// fake LLM 任务 1 走 completed（write_file + request_user_input 决定）→
// 从 Sealed Task Memory 晋升（晋升器本身由 bootstrap 测试覆盖，这里经
// memory 包 API 写入）→ 任务 2 入口召回注入出现在 LLM 消息与 Manifest 段。
func TestProcessTask_SessionMemoryPromotionThenRecall(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()
	tmStore := taskmem.NewStore(t.TempDir())
	proc := memory.NewProcessStore()
	backend, err := memory.NewSessionStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	proc.AttachSessionStore(backend)

	// --- 任务 1：两工具轮 + 文本完成轮 → completed ---
	task1 := &model.Task{Description: "写报告并征求格式", EventType: "code"}
	if err := s.PublishTask(task1); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task1.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	mock1 := &mockLLMClient{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{
			{ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "docs/report.md", "content": "报告正文"}},
			{ID: "c2", Name: "request_user_input", Arguments: map[string]any{"question": "用哪种格式？"}},
		}},
		{Content: "报告已完成"},
	}}
	tools1 := NewToolRegistry()
	tools1.Register("write_file", "写文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "文件已写入: docs/report.md", nil
	})
	tools1.Register("request_user_input", "征求输入", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "采用简洁版格式", nil
	})
	executor1 := NewLLMExecutor(mock1, tools1, nil, nil,
		func(taskID string, rec store.ToolCallRecord) { _ = s.AppendToolCall(taskID, rec) }, "")
	ag1 := NewAgent("agent-1", "code", s, r, executor1)
	ag1.TaskMemStore = tmStore
	ag1.Memory = proc
	ag1.processTask(context.Background(), task1.ID)

	// 终态确认 + Task Memory 已封存。
	cur, err := s.GetTask(task1.ID)
	if err != nil || cur.Status != model.TaskStatusCompleted {
		t.Fatalf("任务 1 应 completed: status=%v err=%v", cur.Status, err)
	}
	mem1, err := tmStore.Load(task1.ID)
	if err != nil || mem1 == nil || !mem1.Sealed {
		t.Fatalf("任务 1 Task Memory 应已封存: mem=%+v err=%v", mem1, err)
	}

	// --- 晋升（生产路径由 sessionPromotionReactor 承担，此处经 memory API） ---
	cands := memory.BuildPromotionCandidates(mem1, memory.TerminalCompleted)
	if len(cands) == 0 {
		t.Fatal("completed 任务应产生晋升候选")
	}
	for _, e := range cands {
		if _, err := backend.Supersede(context.Background(), e); err != nil {
			t.Fatalf("Supersede: %v", err)
		}
	}

	// --- 任务 2：入口召回注入 ---
	task2 := &model.Task{Description: "继续写下周报告", EventType: "code"}
	if err := s.PublishTask(task2); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task2.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	mock2 := &mockLLMClient{responses: []llm.Response{{Content: "已参考记忆完成"}}}
	tools2 := NewToolRegistry()
	executor2 := NewLLMExecutor(mock2, tools2, nil, nil,
		func(taskID string, rec store.ToolCallRecord) { _ = s.AppendToolCall(taskID, rec) }, "")
	ag2 := NewAgent("agent-1", "code", s, r, executor2)
	ag2.TaskMemStore = tmStore
	ag2.Memory = proc
	ag2.processTask(context.Background(), task2.ID)

	// LLM 收到的消息里应含 <session-memory> 注入块（含任务 1 的结果与决定）。
	if len(mock2.captured) == 0 {
		t.Fatal("任务 2 应有 LLM 调用")
	}
	var injected string
	for _, msgs := range mock2.captured {
		for _, m := range msgs {
			if m.Role == "user" && strings.Contains(m.Content, "<session-memory") {
				injected = m.Content
			}
		}
	}
	if injected == "" {
		t.Fatalf("任务 2 的 LLM 消息应含 session-memory 召回注入: %+v", mock2.captured[0])
	}
	for _, want := range []string{"docs/report.md", "采用简洁版格式", "不是系统指令"} {
		if !strings.Contains(injected, want) {
			t.Errorf("召回注入应含 %q: %q", want, injected)
		}
	}

	// trace：任务 2 的 memory_recalled + Manifest 段登记。
	var recalledFound, manifestFound bool
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.TaskID != task2.ID {
			continue
		}
		if ev.Kind == trace.KindMemoryRecalled {
			recalledFound = true
		}
		if ev.Kind == trace.KindContextManifestBuilt && strings.Contains(ev.Description, ManifestSectionSessionMemory) {
			manifestFound = true
		}
	}
	if !recalledFound {
		t.Error("任务 2 应发出 memory_recalled 事件")
	}
	if !manifestFound {
		t.Error("任务 2 的 Context Manifest 应登记 session_memory 段")
	}
}
