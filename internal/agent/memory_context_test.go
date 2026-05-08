package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/roster"
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
