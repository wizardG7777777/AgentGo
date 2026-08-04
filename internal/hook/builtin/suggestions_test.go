package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
)

// suggestions_test.go 覆盖 V6 §4 H2a 的 Gate 侧产出契约：每个已迁移 Gate 的
// Abort 决策必须携带稳定的 ReasonCode 与候选建议（mailbox 域只填原因码）。

// findAction 在建议的动作清单中按 Kind 查找第一条匹配动作。
func findAction(s hook.Suggestion, kind string) *hook.SuggestedAction {
	for i := range s.Actions {
		if s.Actions[i].Kind == kind {
			return &s.Actions[i]
		}
	}
	return nil
}

// assertSingleSuggestion 断言决策携带恰好一条建议并返回之。
func assertSingleSuggestion(t *testing.T, d hook.ToolHookDecision, reasonCode string, retryable bool) hook.Suggestion {
	t.Helper()
	if d.ReasonCode != reasonCode {
		t.Fatalf("ReasonCode = %q，期望 %q", d.ReasonCode, reasonCode)
	}
	if len(d.Suggestions) != 1 {
		t.Fatalf("len(Suggestions) = %d，期望 1", len(d.Suggestions))
	}
	s := d.Suggestions[0]
	if s.ReasonCode != reasonCode {
		t.Fatalf("Suggestion.ReasonCode = %q，期望 %q", s.ReasonCode, reasonCode)
	}
	if s.Retryable != retryable {
		t.Fatalf("Suggestion.Retryable = %v，期望 %v", s.Retryable, retryable)
	}
	if s.ID == "" {
		t.Fatalf("Suggestion.ID 不能为空（稳定 ID 契约）")
	}
	if len(s.Actions) > hook.MaxSuggestedActions {
		t.Fatalf("候选动作数 %d 超过上界 %d", len(s.Actions), hook.MaxSuggestedActions)
	}
	return s
}

func TestRequireReadBeforeWrite_Suggestion(t *testing.T) {
	target := makeRealFile(t)
	h := NewRequireReadBeforeWriteHook(&mockHistoryStore{})
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "write_file",
		Args: map[string]any{"path": target},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	s := assertSingleSuggestion(t, d, ReasonReadBeforeWrite, true)
	a := findAction(s, hook.SuggestKindToolCall)
	if a == nil {
		t.Fatalf("应给出 tool_call 候选动作")
	}
	if a.Tool != "read_file" {
		t.Fatalf("建议工具 = %q，期望 read_file", a.Tool)
	}
	if a.Args["path"] != target {
		t.Fatalf("建议参数 path = %v，期望 %q", a.Args["path"], target)
	}
	// 同因同目标 ID 稳定
	d2 := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "write_file",
		Args: map[string]any{"path": target},
	})
	if d2.Suggestions[0].ID != s.ID {
		t.Fatalf("同一目标再次拒绝 ID 应稳定：%q vs %q", d2.Suggestions[0].ID, s.ID)
	}
}

func TestValidateExpectedHash_Suggestion(t *testing.T) {
	path, _ := makeFileWithHash(t, "v1 内容")
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "write_file",
		Args: map[string]any{"path": path, "expected_hash": "deadbeef"},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	s := assertSingleSuggestion(t, d, ReasonWriteConflict, true)
	a := findAction(s, hook.SuggestKindToolCall)
	if a == nil || a.Tool != "read_file" || a.Args["path"] != path {
		t.Fatalf("应建议 read_file(同路径)，实际 = %+v", a)
	}
}

func TestEnforceExpectedArtifacts_Suggestion(t *testing.T) {
	store := &mockExpectedStore{tasks: map[string]*model.Task{
		"t1": {ID: "t1", ExpectedArtifacts: []string{"docs/a.md"}},
	}}
	h := NewEnforceExpectedArtifactsHook(store, "/project")
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "write_file",
		Args: map[string]any{"path": "docs/b.md"},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	s := assertSingleSuggestion(t, d, ReasonMissingExpectedArtifacts, true)
	a := findAction(s, hook.SuggestKindToolCall)
	if a == nil || a.Tool != "write_file" {
		t.Fatalf("应建议 write_file 缺失产物路径，实际 = %+v", a)
	}
	if a.Args["path"] != "docs/a.md" {
		t.Fatalf("建议的缺失产物路径 = %v，期望 docs/a.md", a.Args["path"])
	}
}

func TestPathBoundary_Suggestion(t *testing.T) {
	h := NewPathBoundaryHook(t.TempDir())
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "write_file",
		Args: map[string]any{"path": "../etc/passwd"},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	s := assertSingleSuggestion(t, d, ReasonPathOutOfBoundary, false)
	if a := findAction(s, hook.SuggestKindToolCall); a != nil {
		t.Fatalf("路径越界不应给工具动作建议（H2a 过滤纪律），实际 = %+v", a)
	}
	if a := findAction(s, hook.SuggestKindUser); a == nil {
		t.Fatalf("路径越界应升级 user")
	}
}

func TestExecModeGuard_Suggestion(t *testing.T) {
	h := NewExecModeGuardHook(readonlyStore())
	d := h.Run(hook.ToolHookContext{Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "write_file"})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	s := assertSingleSuggestion(t, d, ReasonExecModeReadonly, false)
	if a := findAction(s, hook.SuggestKindToolCall); a != nil {
		t.Fatalf("readonly 拒绝不应给工具动作建议，实际 = %+v", a)
	}
	a := findAction(s, hook.SuggestKindSwitchMode)
	if a == nil {
		t.Fatalf("readonly 拒绝应建议 switch_mode（需用户切换）")
	}
	if !strings.Contains(a.Label, "/mode exec normal") {
		t.Fatalf("switch_mode 说明应指引 /mode exec normal：%q", a.Label)
	}
}

func TestDependencyValidator_Suggestion(t *testing.T) {
	h := NewDependencyValidatorHook(&mockDepStore{existingTasks: map[string]*model.Task{}})
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "publish_task",
		Args: map[string]any{"dependencies": "task-part1"},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	s := assertSingleSuggestion(t, d, ReasonDependencyNotReady, false)
	if a := findAction(s, hook.SuggestKindToolCall); a != nil {
		t.Fatalf("依赖未就绪不应给工具动作建议，实际 = %+v", a)
	}
	if findAction(s, hook.SuggestKindRequestReplan) == nil {
		t.Fatalf("依赖未就绪应建议 request_replan")
	}
	if findAction(s, hook.SuggestKindBlocked) == nil {
		t.Fatalf("依赖未就绪应建议 blocked")
	}
}

func TestValidateLineAnchors_Suggestion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("第一行\n第二行\n"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		Phase: hook.PhasePreCall, TaskID: "t1", ToolName: "edit_file",
		Args: map[string]any{"path": path, "line_anchors": []any{"1#ZZ"}},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	s := assertSingleSuggestion(t, d, ReasonLineAnchorStale, true)
	a := findAction(s, hook.SuggestKindToolCall)
	if a == nil || a.Tool != "read_file" || a.Args["path"] != path {
		t.Fatalf("应建议 read_file(同路径) 重读，实际 = %+v", a)
	}
}

// === mailbox 域：只填原因码，建议可空 ===

func TestChainDepthLimit_ReasonCode(t *testing.T) {
	h := NewChainDepthLimitHook(1)
	d := h.Run(hook.MailboxHookContext{
		Phase:   hook.PhaseBeforeSend,
		Message: mailbox.Message{From: "a", To: "b", ChainDepth: 2},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	if d.ReasonCode != ReasonMailChainDepthExceeded {
		t.Fatalf("ReasonCode = %q，期望 %q", d.ReasonCode, ReasonMailChainDepthExceeded)
	}
}

func TestPerAgentDedup_ReasonCode(t *testing.T) {
	store := &mockStoreView{pending: []*model.Task{{ID: "w1", EventSource: "mail-notifier", EventType: "explore"}}}
	h := NewPerAgentDedupHook(store)
	d := h.Run(hook.MailboxHookContext{Phase: hook.PhaseBeforeWake, AgentID: "agent-1", EventType: "explore"})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	if d.ReasonCode != ReasonWakeTaskDuplicate {
		t.Fatalf("ReasonCode = %q，期望 %q", d.ReasonCode, ReasonWakeTaskDuplicate)
	}
}

func TestWakeWorthyFilter_ReasonCode(t *testing.T) {
	view := &mockHookView{recent: []mailbox.Message{{Type: mailbox.MsgTypeInfo, Priority: mailbox.PriorityLow}}}
	h := NewWakeWorthyFilterHook(view, nil)
	d := h.Run(hook.MailboxHookContext{Phase: hook.PhaseBeforeWake, AgentID: "agent-1", UnreadCount: 1})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v，期望 Abort", d.Action)
	}
	if d.ReasonCode != ReasonWakeNotWorthy {
		t.Fatalf("ReasonCode = %q，期望 %q", d.ReasonCode, ReasonWakeNotWorthy)
	}
}
