package bootstrap

// sessionPromotionReactor（V6 §3 CM3）的行为测试：终态事件 → Task Memory
// 筛选 → SessionStore 写入 + PromotedAt 幂等（重复事件 / 进程重启不重复晋升）。
// trace 事件经临时目录 writer 捕获断言。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/memory"
	"agentgo/internal/reactor"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// promotionFixture 组装晋升测试环境：Task Memory Store + ProcessStore（挂接
// SessionStore 后端）+ 晋升 Reactor + trace 捕获目录。
type promotionFixture struct {
	tmStore  *taskmem.Store
	proc     *memory.ProcessStore
	backend  *memory.SessionStore
	reactor  *sessionPromotionReactor
	traceDir string
}

func newPromotionFixture(t *testing.T) *promotionFixture {
	t.Helper()
	traceDir := capturePromotionTrace(t)
	tmStore := taskmem.NewStore(t.TempDir())
	proc := memory.NewProcessStore()
	backend, err := memory.NewSessionStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	proc.AttachSessionStore(backend)
	fx := &promotionFixture{
		tmStore:  tmStore,
		proc:     proc,
		backend:  backend,
		traceDir: traceDir,
	}
	fx.reactor = newSessionPromotionReactor(tmStore, proc.SessionBackend)
	return fx
}

// capturePromotionTrace 把包级默认 trace.Writer 切到临时目录（Cleanup 恢复原
// writer 再 Close——Windows 句柄纪律）。
func capturePromotionTrace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	w, err := trace.NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	orig := trace.Default()
	trace.SetDefault(w)
	t.Cleanup(func() { trace.SetDefault(orig) })
	return dir
}

// readPromotionTrace 读取捕获目录中的全部 trace 事件。
func readPromotionTrace(t *testing.T, dir string) []trace.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var evs []trace.Event
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev trace.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			evs = append(evs, ev)
		}
	}
	return evs
}

// seedSealedMem 造一份终态 Task Memory 并落盘（含 confirmed 事实 / 用户决定 /
// 文件版本 / inferred 事实）。
func seedSealedMem(t *testing.T, tmStore *taskmem.Store, taskID, decision string) {
	t.Helper()
	m := taskmem.New(taskID)
	m.Goal = "写月度报告"
	m.Facts = []taskmem.Fact{
		{Text: "报告已写入 docs/report.md", Confirmed: true,
			Evidence: []taskmem.EvidenceRef{{Kind: taskmem.EvidenceFileEffect, Ref: "docs/report.md"}}},
		{Text: decision, Confirmed: true,
			Evidence: []taskmem.EvidenceRef{{Kind: taskmem.EvidenceUser, Ref: "request_user_input"}}},
		{Text: "测试应该通过了", Confirmed: false}, // inferred：不得晋升
	}
	m.Files = []taskmem.FileVersion{{Path: "docs/report.md", Hash: "abcdef0123456789"}}
	m.Sealed = true
	if err := tmStore.Save(m); err != nil {
		t.Fatalf("Save Task Memory: %v", err)
	}
}

// querySession 查全部 ScopeSession 某 Kind 条目。
func querySession(t *testing.T, proc *memory.ProcessStore, kind memory.Kind) []memory.Entry {
	t.Helper()
	entries, err := proc.Query(context.Background(), memory.ScopeSession, kind, "", 50)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return entries
}

// decidedEvents 过滤某任务的晋升 decided 事件。
func decidedEvents(evs []trace.Event, taskID string) []trace.Event {
	var out []trace.Event
	for _, ev := range evs {
		if ev.Kind == trace.KindSessionMemoryPromotionDecided && ev.TaskID == taskID {
			out = append(out, ev)
		}
	}
	return out
}

func decidedPayload(t *testing.T, ev trace.Event) promotionDecidedPayload {
	t.Helper()
	var p promotionDecidedPayload
	if err := json.Unmarshal([]byte(ev.Description), &p); err != nil {
		t.Fatalf("decided Description 非合法 JSON: %v (%q)", err, ev.Description)
	}
	return p
}

func TestSessionPromotion_CompletedTaskPromotesEntries(t *testing.T) {
	fx := newPromotionFixture(t)
	seedSealedMem(t, fx.tmStore, "task-1", "用户决定: 采用简洁版格式")

	if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "task-1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Session 中出现正确条目：结果（不含 inferred）+ 用户决定。
	results := querySession(t, fx.proc, memory.KindResult)
	if len(results) != 1 {
		t.Fatalf("KindResult 应有 1 条: %+v", results)
	}
	if !strings.Contains(results[0].Content, "报告已写入 docs/report.md") {
		t.Errorf("结果条目应含 confirmed 事实: %q", results[0].Content)
	}
	if strings.Contains(results[0].Content, "测试应该通过了") {
		t.Errorf("结果条目不得含 inferred 事实: %q", results[0].Content)
	}
	if results[0].EffectiveState() != memory.StateConfirmed {
		t.Errorf("结果条目应为 confirmed: %+v", results[0])
	}
	decisions := querySession(t, fx.proc, memory.KindDecision)
	if len(decisions) != 1 || !strings.Contains(decisions[0].Content, "采用简洁版格式") {
		t.Errorf("用户决定应晋升: %+v", decisions)
	}

	// PromotedAt 幂等标记已置位并落盘。
	mem, err := fx.tmStore.Load("task-1")
	if err != nil || mem == nil {
		t.Fatalf("Load Task Memory: %v", err)
	}
	if mem.PromotedAt.IsZero() {
		t.Error("晋升后 PromotedAt 应置位")
	}

	// trace：proposed + decided(promoted) 各一条。
	evs := readPromotionTrace(t, fx.traceDir)
	var proposed int
	for _, ev := range evs {
		if ev.Kind == trace.KindSessionMemoryPromotionProposed && ev.TaskID == "task-1" {
			proposed++
			if ev.Reason != memory.TerminalCompleted {
				t.Errorf("proposed Reason = %q, want completed", ev.Reason)
			}
		}
	}
	if proposed != 1 {
		t.Errorf("proposed 事件应有 1 条, got %d", proposed)
	}
	decided := decidedEvents(evs, "task-1")
	if len(decided) != 1 {
		t.Fatalf("decided 事件应有 1 条, got %d", len(decided))
	}
	payload := decidedPayload(t, decided[0])
	if payload.Decided != "promoted" || payload.Entries != 2 {
		t.Errorf("decided payload = %+v, want promoted entries=2", payload)
	}
}

func TestSessionPromotion_IdempotentOnDuplicateEvent(t *testing.T) {
	fx := newPromotionFixture(t)
	seedSealedMem(t, fx.tmStore, "task-2", "用户决定: A")

	for i := 0; i < 2; i++ {
		if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "task-2"}); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}
	// 第二次：already_promoted，Session 条目数不变。
	if got := len(querySession(t, fx.proc, memory.KindResult)); got != 1 {
		t.Errorf("重复终态事件后 KindResult 仍应为 1 条, got %d", got)
	}
	decided := decidedEvents(readPromotionTrace(t, fx.traceDir), "task-2")
	if len(decided) != 2 {
		t.Fatalf("decided 应有 2 条（每次事件一条）, got %d", len(decided))
	}
	if p := decidedPayload(t, decided[1]); p.Decided != "already_promoted" {
		t.Errorf("第二次 decided = %q, want already_promoted", p.Decided)
	}
}

func TestSessionPromotion_IdempotentAcrossRestart(t *testing.T) {
	fx := newPromotionFixture(t)
	seedSealedMem(t, fx.tmStore, "task-3", "用户决定: B")
	if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "task-3"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	before := len(querySession(t, fx.proc, memory.KindResult))
	if before != 1 {
		t.Fatalf("首次晋升后 KindResult 应为 1 条, got %d", before)
	}

	// 模拟进程重启：同一 taskmem 目录开新 Store（内存索引为空，从磁盘恢复），
	// 新 Reactor 实例重放同一终态事件。
	restartedStore := taskmem.NewStore(fx.tmStore.Dir())
	restarted := newSessionPromotionReactor(restartedStore, fx.proc.SessionBackend)
	if err := restarted.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "task-3"}); err != nil {
		t.Fatalf("重启后 Run: %v", err)
	}
	if got := len(querySession(t, fx.proc, memory.KindResult)); got != before {
		t.Errorf("重启重复事件不得重复晋升: got %d, want %d", got, before)
	}
	mem, err := restartedStore.Load("task-3")
	if err != nil || mem == nil || mem.PromotedAt.IsZero() {
		t.Errorf("重启后 PromotedAt 应从磁盘恢复: mem=%+v err=%v", mem, err)
	}
}

func TestSessionPromotion_SameKeySupersedesAcrossTasks(t *testing.T) {
	fx := newPromotionFixture(t)
	// 两个任务晋升同一条用户决定（内容寻址同 Key）→ 旧条目 superseded。
	seedSealedMem(t, fx.tmStore, "task-4a", "用户决定: 采用简洁版格式")
	seedSealedMem(t, fx.tmStore, "task-4b", "用户决定: 采用简洁版格式")

	for _, id := range []string{"task-4a", "task-4b"} {
		if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: id}); err != nil {
			t.Fatalf("Run %s: %v", id, err)
		}
	}
	decisions := querySession(t, fx.proc, memory.KindDecision)
	if len(decisions) != 2 {
		t.Fatalf("同 Key 决定应保留新旧两条（审计链）: %+v", decisions)
	}
	var active, superseded int
	for _, e := range decisions {
		switch e.EffectiveState() {
		case memory.StateConfirmed:
			active++
			if e.Source != "task-4b" {
				t.Errorf("活跃条目应来自后晋升的 task-4b: %+v", e)
			}
		case memory.StateSuperseded:
			superseded++
			if e.SupersededBy == "" {
				t.Errorf("superseded 条目应记 SupersededBy: %+v", e)
			}
		}
	}
	if active != 1 || superseded != 1 {
		t.Errorf("同 Key 晋升后应 1 活跃 + 1 superseded: active=%d superseded=%d", active, superseded)
	}
	// memory_entry_state_changed 事件已发出。
	var stateChanged int
	for _, ev := range readPromotionTrace(t, fx.traceDir) {
		if ev.Kind == trace.KindMemoryEntryStateChanged {
			stateChanged++
		}
	}
	if stateChanged != 1 {
		t.Errorf("memory_entry_state_changed 应有 1 条, got %d", stateChanged)
	}
}

func TestSessionPromotion_NoBackendSkipsWithoutMarking(t *testing.T) {
	traceDir := capturePromotionTrace(t)
	tmStore := taskmem.NewStore(t.TempDir())
	seedSealedMem(t, tmStore, "task-5", "用户决定: C")
	r := newSessionPromotionReactor(tmStore, func() *memory.SessionStore { return nil })

	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "task-5"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 后端缺失：不置 PromotedAt（允许后端就绪后的重复事件重试）。
	mem, _ := tmStore.Load("task-5")
	if mem == nil || !mem.PromotedAt.IsZero() {
		t.Error("后端未挂接时不得置 PromotedAt")
	}
	decided := decidedEvents(readPromotionTrace(t, traceDir), "task-5")
	if len(decided) != 1 || decidedPayload(t, decided[0]).Decided != "session_store_unavailable" {
		t.Errorf("decided 应为 session_store_unavailable: %+v", decided)
	}
}

func TestSessionPromotion_WaitsForSealedCheckpoint(t *testing.T) {
	fx := newPromotionFixture(t)
	m := taskmem.New("task-wait-sealed")
	m.Goal = "等待终态"
	if err := fx.tmStore.Save(m); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- fx.reactor.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: m.TaskID})
	}()
	select {
	case err := <-done:
		t.Fatalf("晋升器不得在 sealed checkpoint 前返回: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	m.Facts = []taskmem.Fact{{
		Text: "终态快照中的已验证结果", Confirmed: true,
		Evidence: []taskmem.EvidenceRef{{Kind: taskmem.EvidenceStatus, Ref: "completed"}},
	}}
	m.Sealed = true
	if err := fx.tmStore.Save(m); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	results := querySession(t, fx.proc, memory.KindResult)
	if len(results) != 1 || !strings.Contains(results[0].Content, "终态快照中的已验证结果") {
		t.Fatalf("晋升必须读取 sealed checkpoint 的一致快照: %+v", results)
	}
}

func TestSessionPromotion_WriteFailureDoesNotMarkPromoted(t *testing.T) {
	fx := newPromotionFixture(t)
	seedSealedMem(t, fx.tmStore, "task-write-failed", "用户决定: retry")
	if err := fx.backend.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "task-write-failed"}); err != nil {
		t.Fatal(err)
	}
	m, err := fx.tmStore.Load("task-write-failed")
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || !m.PromotedAt.IsZero() {
		t.Fatalf("任一候选写失败时不得置 PromotedAt: %+v", m)
	}
	events := decidedEvents(readPromotionTrace(t, fx.traceDir), "task-write-failed")
	if len(events) != 1 || decidedPayload(t, events[0]).Decided != "write_failed" {
		t.Fatalf("写入失败应记录 write_failed: %+v", events)
	}
}

func TestSessionPromotion_NoTaskMemory(t *testing.T) {
	fx := newPromotionFixture(t)
	if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "never-existed"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	decided := decidedEvents(readPromotionTrace(t, fx.traceDir), "never-existed")
	if len(decided) != 1 || decidedPayload(t, decided[0]).Decided != "no_task_memory" {
		t.Errorf("decided 应为 no_task_memory: %+v", decided)
	}
}

func TestSessionPromotion_TerminalRulesApplied(t *testing.T) {
	// blocked / failed / cancelled 经 Reactor 走各自规则（规则正确性由
	// internal/memory/promotion_test.go 钉死，这里验证事件→规则的映射）。
	fx := newPromotionFixture(t)

	blockedMem := taskmem.New("task-6b")
	blockedMem.Blockers = []string{"write_file a.go — 文件被占用"}
	blockedMem.Failures = []string{"write_file 调用失败: a.go"}
	blockedMem.NextCandidates = []string{"解除占用后重写"}
	blockedMem.Sealed = true
	if err := fx.tmStore.Save(blockedMem); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: "task-6b"}); err != nil {
		t.Fatalf("Run blocked: %v", err)
	}
	blockers := querySession(t, fx.proc, memory.KindBlocker)
	if len(blockers) != 1 || !strings.Contains(blockers[0].Content, "未完成") {
		t.Errorf("blocked 应晋升 1 条 KindBlocker 且标注未完成: %+v", blockers)
	}

	failedMem := taskmem.New("task-6f")
	failedMem.Failures = []string{"run_shell 命令失败 (exit=2): go build"}
	failedMem.Sealed = true
	if err := fx.tmStore.Save(failedMem); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-6f"}); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	learnings := querySession(t, fx.proc, memory.KindLearning)
	if len(learnings) != 1 || !strings.Contains(learnings[0].Content, "exit=2") {
		t.Errorf("failed 应晋升 1 条 KindLearning 失败证据: %+v", learnings)
	}

	cancelledMem := taskmem.New("task-6c")
	cancelledMem.Files = []taskmem.FileVersion{{Path: "docs/draft.md", Hash: "0123456789abcdef"}}
	cancelledMem.Failures = []string{"某中间失败"} // cancelled 不晋升过程记录
	cancelledMem.Sealed = true
	if err := fx.tmStore.Save(cancelledMem); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := fx.reactor.Run(trace.Event{Kind: trace.KindTaskCancelled, TaskID: "task-6c"}); err != nil {
		t.Fatalf("Run cancelled: %v", err)
	}
	effects := querySession(t, fx.proc, memory.KindResult)
	if len(effects) != 1 || !strings.Contains(effects[0].Content, "docs/draft.md") {
		t.Fatalf("cancelled 应保留权威 Effect: %+v", effects)
	}
	if strings.Contains(effects[0].Content, "某中间失败") {
		t.Errorf("cancelled 不得晋升中间过程记录: %q", effects[0].Content)
	}
}

func TestSessionPromotion_RegistrationWiring(t *testing.T) {
	// 装配缝：Reactor 注册进 Registry 后订阅四种终态事件（与 bootstrap.go
	// Step 8.1.5 的注册路径同型）。
	reg := reactor.NewRegistry()
	tmStore := taskmem.NewStore(t.TempDir())
	proc := memory.NewProcessStore()
	if err := reg.Register(newSessionPromotionReactor(tmStore, proc.SessionBackend)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, kind := range []trace.EventKind{
		trace.KindTaskCompleted, trace.KindTaskFailed, trace.KindTaskBlocked, trace.KindTaskCancelled,
	} {
		subs := reg.Subscribers(kind)
		found := false
		for _, s := range subs {
			if s.Name() == "session-memory-promotion" {
				found = true
			}
		}
		if !found {
			t.Errorf("session-memory-promotion 应订阅 %s", kind)
		}
	}
}
