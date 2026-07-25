package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/trace"
)

// v5 Phase 1 Memory System 取代 TeamAwarenessHook 的逻辑覆盖（MemoryManageSystem.md MM5）。
// 主流程接入点 Agent.injectMemoryContext 需要保证：
//  1. nil Memory：直接返回 ""，等价于不启用
//  2. 首轮（loopIdx=-1）：注入 team_snapshot + file_awareness 并 write-through 到 Memory
//  3. RetryCount > 0：首轮跳过（沿袭 v4 TeamAwarenessHook 短路）
//  4. loopIdx==0：返回 ""（首轮注入由 -1 路径承担，避免双重）
//  5. 刷新间隔：loopIdx % TeamRefreshInterval == 0 时刷新
//  6. hasNewMail：强制刷新（绕过间隔）
//  7. 非刷新轮：返回 ""

func memCtxSetup(t *testing.T) (*Agent, *memory.ProcessStore, *mailbox.Registry) {
	t.Helper()
	s, r, _ := setup()
	mbReg := mailbox.NewRegistry(64)
	mem := memory.NewProcessStore()

	a := NewAgent("agent-test", "code", s, r, nil, 5)
	a.Mailbox = mbReg.Register(a.ID, a.EventType)
	a.MailRegistry = mbReg
	a.Memory = mem
	a.TeamRefreshInterval = 5

	// 注册一个 peer agent 让 BuildTeamSnapshot 有非空输出（idle peer 出现在 snapshot 里）。
	// 让 a 自身占用一个文件，让 renderFileAwareness 也输出内容。
	mbReg.Register("agent-peer", "code")
	if ok, err := r.TryClaim(a.ID, "src/foo.go"); !ok || err != nil {
		t.Fatalf("setup TryClaim self: %v", err)
	}
	return a, mem, mbReg
}

func TestInjectMemoryContext_NilMemoryReturnsEmpty(t *testing.T) {
	a, _, _ := memCtxSetup(t)
	a.Memory = nil
	got := a.injectMemoryContext(context.Background(), "task-x", -1, false)
	if got != "" {
		t.Errorf("nil Memory should return empty, got %q", got)
	}
}

func TestInjectMemoryContext_FirstRoundInjectsAndWriteThrough(t *testing.T) {
	a, mem, _ := memCtxSetup(t)
	task := &model.Task{Description: "test", EventType: "code"}
	if err := a.Store.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	got := a.injectMemoryContext(context.Background(), task.ID, -1, false)
	if got == "" {
		t.Fatalf("first-round inject should be non-empty")
	}
	if !strings.Contains(got, "<team-snapshot>") && !strings.Contains(got, "<board>") {
		// BuildTeamSnapshot 输出含 <team-snapshot> 或 board 子元素；至少一个出现
		t.Errorf("expected team_snapshot content, got %q", got[:minInt(200, len(got))])
	}

	// 验证 write-through：Memory 中应有 team_snapshot:<id> 条目
	es, _ := mem.Query(context.Background(), memory.ScopeProcess, memory.KindContext,
		teamSnapshotKey(a.ID), 1)
	if len(es) == 0 {
		t.Errorf("Memory should contain team_snapshot:%s entry after first inject", a.ID)
	}
}

func TestInjectMemoryContext_ReadsExistingMemoryEntries(t *testing.T) {
	mem := memory.NewProcessStore()
	a := &Agent{
		ID:                  "agent-read",
		Memory:              mem,
		TeamRefreshInterval: 5,
	}
	if err := mem.Put(context.Background(), memory.Entry{
		Scope:   memory.ScopeProcess,
		Kind:    memory.KindContext,
		Key:     teamSnapshotKey(a.ID),
		Content: "<team-snapshot>cached team</team-snapshot>",
	}); err != nil {
		t.Fatalf("Put team snapshot: %v", err)
	}
	if err := mem.Put(context.Background(), memory.Entry{
		Scope:   memory.ScopeProcess,
		Kind:    memory.KindContext,
		Key:     fileAwarenessKey(a.ID),
		Content: "<file-awareness>cached files</file-awareness>",
	}); err != nil {
		t.Fatalf("Put file awareness: %v", err)
	}

	got := a.injectMemoryContext(context.Background(), "task-read", -1, false)
	if !strings.Contains(got, "cached team") {
		t.Fatalf("expected cached team snapshot from Memory, got %q", got)
	}
	if !strings.Contains(got, "cached files") {
		t.Fatalf("expected cached file awareness from Memory, got %q", got)
	}
}

func TestInjectMemoryContext_RetryTaskSkipped(t *testing.T) {
	a, mem, _ := memCtxSetup(t)
	task := &model.Task{Description: "retry test", EventType: "code", RetryCount: 1}
	if err := a.Store.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	got := a.injectMemoryContext(context.Background(), task.ID, -1, false)
	if got != "" {
		t.Errorf("retry task should skip injection, got %q", got)
	}
	// 也不应 write-through
	es, _ := mem.Query(context.Background(), memory.ScopeProcess, memory.KindContext,
		teamSnapshotKey(a.ID), 1)
	if len(es) != 0 {
		t.Errorf("retry skip should not Put memory, got %d entries", len(es))
	}
}

func TestInjectMemoryContext_LoopZeroReturnsEmpty(t *testing.T) {
	a, _, _ := memCtxSetup(t)
	task := &model.Task{Description: "x", EventType: "code"}
	_ = a.Store.PublishTask(task)
	got := a.injectMemoryContext(context.Background(), task.ID, 0, false)
	if got != "" {
		t.Errorf("loopIdx==0 should return empty (TaskStart already injected), got non-empty")
	}
}

func TestInjectMemoryContext_RefreshInterval(t *testing.T) {
	a, _, _ := memCtxSetup(t)
	a.TeamRefreshInterval = 5
	task := &model.Task{Description: "x", EventType: "code"}
	_ = a.Store.PublishTask(task)

	cases := []struct {
		loopIdx int
		want    bool // 是否应注入
	}{
		{1, false}, {2, false}, {3, false}, {4, false},
		{5, true}, {6, false}, {10, true},
	}
	for _, tc := range cases {
		got := a.injectMemoryContext(context.Background(), task.ID, tc.loopIdx, false)
		hasContent := got != ""
		if hasContent != tc.want {
			t.Errorf("loopIdx=%d: got non-empty=%v want=%v", tc.loopIdx, hasContent, tc.want)
		}
	}
}

func TestInjectMemoryContext_HasNewMailForcesRefresh(t *testing.T) {
	a, _, _ := memCtxSetup(t)
	a.TeamRefreshInterval = 5
	task := &model.Task{Description: "x", EventType: "code"}
	_ = a.Store.PublishTask(task)

	// loopIdx=2 不在刷新点；hasNewMail=true 应强制刷新
	got := a.injectMemoryContext(context.Background(), task.ID, 2, true)
	if got == "" {
		t.Errorf("hasNewMail=true should force refresh even off-interval, got empty")
	}
}

func TestRenderFileAwareness_PrefixForSelfVsOthers(t *testing.T) {
	r := roster.NewMemoryRoster()
	if ok, err := r.TryClaim("agent-self", "src/foo.go"); !ok || err != nil {
		t.Fatalf("setup: TryClaim self ok=%v err=%v", ok, err)
	}
	if ok, err := r.TryClaim("agent-other", "src/bar.go"); !ok || err != nil {
		t.Fatalf("setup: TryClaim other ok=%v err=%v", ok, err)
	}

	got := renderFileAwareness("agent-self", r)
	if !strings.Contains(got, "你（agent-self）已占用: src/foo.go") {
		t.Errorf("self prefix wrong: %q", got)
	}
	if !strings.Contains(got, "agent-other 正在修改: src/bar.go") {
		t.Errorf("other prefix wrong: %q", got)
	}
}

func TestRenderFileAwareness_NilRoster(t *testing.T) {
	got := renderFileAwareness("a", nil)
	if got != "" {
		t.Errorf("nil roster should return empty, got %q", got)
	}
}

func TestRenderFileAwareness_NoClaims(t *testing.T) {
	r := roster.NewMemoryRoster()
	got := renderFileAwareness("a", r)
	if got != "" {
		t.Errorf("empty roster should return empty, got %q", got)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ===== Memory 上下文注入审计埋点（memory_context_inject 事件）=====
//
// 验证点：
//  1. 注入时每个非空 section 各发一条事件，字段（TaskID/AgentID/Loop/
//     NotifyType=来源 / Path=实际命中键 / OutputLen=rune 数 / Description）正确
//  2. 埋点反映的是 Query 回读、真正拼进 history 的内容，而非 write-through
//     计算值——用 Memory 中条目内容核对 OutputLen，并核对注入文本包含该内容
//  3. 未注入时（非刷新轮 / loopIdx==0 / 重试任务跳过 / nil Memory）不发事件
//  4. 循环刷新注入的事件 Loop 字段等于实际轮次（区别于任务入口的 -1）
//
// 事件捕获方式与 progress_notify_test.go 同型：包级默认 Writer 切到临时目录，
// 测试结束恢复原 Writer。

// captureTraceToDir 把包级默认 trace.Writer 切到 t.TempDir() 以捕获测试期间
// emit 的全部事件，返回目录路径。Cleanup 顺序（LIFO）：先恢复原 Writer，
// 再 Close 临时 Writer，最后 TempDir 清理——满足 Windows 下"文件句柄必须先于
// 目录清理关闭"的约束（见 AGENTS.md Cross-platform constraints）。
func captureTraceToDir(t *testing.T) string {
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

// readTraceEventsFromDir 读取目录下全部 .jsonl trace 分片并逐行解析为 Event。
// 读取发生在断言阶段（Writer.Emit 每次 append 直接落盘），无需先 Close。
func readTraceEventsFromDir(t *testing.T, dir string) []trace.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir trace dir: %v", err)
	}
	var evs []trace.Event
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev trace.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("unmarshal trace event: %v (line=%q)", err, line)
			}
			evs = append(evs, ev)
		}
	}
	return evs
}

// filterTraceEvents 过滤出指定 Kind 的事件。
func filterTraceEvents(evs []trace.Event, kind trace.EventKind) []trace.Event {
	var out []trace.Event
	for _, ev := range evs {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func TestInjectMemoryContext_TraceEventFieldsOnInject(t *testing.T) {
	dir := captureTraceToDir(t)
	a, mem, _ := memCtxSetup(t)
	task := &model.Task{Description: "trace inject", EventType: "code"}
	if err := a.Store.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	injected := a.injectMemoryContext(context.Background(), task.ID, -1, false)
	if injected == "" {
		t.Fatalf("first-round inject should be non-empty")
	}

	// team_snapshot + file_awareness 两个 section 各一条事件
	evs := filterTraceEvents(readTraceEventsFromDir(t, dir), trace.KindMemoryContextInject)
	if len(evs) != 2 {
		t.Fatalf("expected 2 %s events (team_snapshot + file_awareness), got %d: %+v",
			trace.KindMemoryContextInject, len(evs), evs)
	}

	bySource := map[string]trace.Event{}
	for _, ev := range evs {
		if ev.TaskID != task.ID {
			t.Errorf("TaskID=%q want %q", ev.TaskID, task.ID)
		}
		if ev.AgentID != a.ID {
			t.Errorf("AgentID=%q want %q", ev.AgentID, a.ID)
		}
		if ev.Loop != -1 {
			t.Errorf("Loop=%d want -1（任务入口注入）", ev.Loop)
		}
		if ev.Description == "" {
			t.Errorf("Description 不应为空（trace CLI 默认分支展示 desc=）")
		}
		bySource[ev.NotifyType] = ev
	}

	// 逐 section 校验来源 / 命中键 / rune 数
	for _, tc := range []struct {
		source string
		key    string
	}{
		{"team_snapshot", teamSnapshotKey(a.ID)},
		{"file_awareness", fileAwarenessKey(a.ID)},
	} {
		ev, ok := bySource[tc.source]
		if !ok {
			t.Errorf("缺少 source=%s 的注入事件", tc.source)
			continue
		}
		if ev.Path != tc.key {
			t.Errorf("source=%s Path=%q want %q（实际命中的 Memory 键）", tc.source, ev.Path, tc.key)
		}
		// OutputLen 必须等于实际注入内容（Query 回读值，即 Memory 中条目
		// 去空白后的文本）的 rune 数，而不是 write-through 计算值的缓存镜像
		es, err := mem.Query(context.Background(), memory.ScopeProcess, memory.KindContext, tc.key, 1)
		if err != nil || len(es) == 0 {
			t.Fatalf("Query %s: err=%v n=%d", tc.key, err, len(es))
		}
		content := strings.TrimSpace(es[0].Content)
		if want := utf8.RuneCountInString(content); ev.OutputLen != want {
			t.Errorf("source=%s OutputLen=%d want %d（实际注入内容的 rune 数）", tc.source, ev.OutputLen, want)
		}
		if !strings.Contains(injected, content) {
			t.Errorf("source=%s 的 Memory 内容未出现在注入文本中——埋点与实际注入分叉", tc.source)
		}
		if !strings.Contains(ev.Description, tc.source) || !strings.Contains(ev.Description, tc.key) {
			t.Errorf("source=%s Description=%q 应包含来源与命中键", tc.source, ev.Description)
		}
	}
}

func TestInjectMemoryContext_NoTraceEventWhenNotInjected(t *testing.T) {
	dir := captureTraceToDir(t)
	a, _, _ := memCtxSetup(t)
	a.TeamRefreshInterval = 5
	task := &model.Task{Description: "x", EventType: "code"}
	_ = a.Store.PublishTask(task)

	// 非刷新轮（loopIdx=1 且无新邮件）：不注入
	if got := a.injectMemoryContext(context.Background(), task.ID, 1, false); got != "" {
		t.Fatalf("loopIdx=1 非刷新轮应返回空, got %q", got)
	}
	// loopIdx==0：首轮注入由 -1 路径承担，不注入
	if got := a.injectMemoryContext(context.Background(), task.ID, 0, false); got != "" {
		t.Fatalf("loopIdx==0 应返回空, got %q", got)
	}
	// 重试任务：TaskStart 阶段跳过注入
	retryTask := &model.Task{Description: "retry", EventType: "code", RetryCount: 1}
	_ = a.Store.PublishTask(retryTask)
	if got := a.injectMemoryContext(context.Background(), retryTask.ID, -1, false); got != "" {
		t.Fatalf("重试任务应跳过注入, got %q", got)
	}
	// nil Memory：特性禁用，不注入
	a.Memory = nil
	if got := a.injectMemoryContext(context.Background(), task.ID, -1, false); got != "" {
		t.Fatalf("nil Memory 应返回空, got %q", got)
	}

	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.Kind == trace.KindMemoryContextInject {
			t.Errorf("未注入场景不应发射 %s 事件: %+v", trace.KindMemoryContextInject, ev)
		}
	}
}

func TestInjectMemoryContext_TraceEventLoopFieldOnRefresh(t *testing.T) {
	dir := captureTraceToDir(t)
	a, _, _ := memCtxSetup(t)
	a.TeamRefreshInterval = 5
	task := &model.Task{Description: "x", EventType: "code"}
	_ = a.Store.PublishTask(task)

	// 任务入口注入（loopIdx=-1）后，模拟第 5 轮刷新点注入
	_ = a.injectMemoryContext(context.Background(), task.ID, -1, false)
	if got := a.injectMemoryContext(context.Background(), task.ID, 5, false); got == "" {
		t.Fatalf("loopIdx=5 刷新点应注入")
	}

	var entryEvents, loopEvents int
	for _, ev := range filterTraceEvents(readTraceEventsFromDir(t, dir), trace.KindMemoryContextInject) {
		switch ev.Loop {
		case -1:
			entryEvents++
		case 5:
			loopEvents++
		default:
			t.Errorf("意外 Loop 值 %d（应只有 -1=任务入口 与 5=刷新轮）", ev.Loop)
		}
	}
	if entryEvents == 0 || loopEvents == 0 {
		t.Errorf("任务入口与第 5 轮刷新都应有注入事件: entry=%d loop=%d", entryEvents, loopEvents)
	}
}
