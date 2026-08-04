package builtin

import (
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/modes"
)

// readonlyStore 构造 exec 轴为 readonly 的模式 store（gate/topo 用默认值）。
func readonlyStore() *modes.Store {
	return modes.NewStore(modes.ExecReadonly, modes.TopoTeam)
}

// TestExecModeGuard_ReadonlyBlocksWriteTools readonly 模式下三个写类工具
// 全部被 Abort，且中文错误消息含 "readonly" 与切换指引。
func TestExecModeGuard_ReadonlyBlocksWriteTools(t *testing.T) {
	h := NewExecModeGuardHook(readonlyStore())
	for _, tool := range []string{"write_file", "edit_file", "run_shell"} {
		t.Run(tool, func(t *testing.T) {
			if !h.Matches(tool) {
				t.Fatalf("Matches(%s) = false，期望 true", tool)
			}
			d := h.Run(hook.ToolHookContext{Phase: hook.PhasePreCall, ToolName: tool})
			if d.Action != hook.Abort {
				t.Fatalf("Action = %v，期望 Abort", d.Action)
			}
			if d.HookName != "exec-mode-guard" {
				t.Fatalf("HookName = %q，期望 exec-mode-guard", d.HookName)
			}
			if !strings.Contains(d.AbortReason, "readonly") {
				t.Fatalf("AbortReason 应含 readonly: %q", d.AbortReason)
			}
			if !strings.Contains(d.AbortReason, tool) {
				t.Fatalf("AbortReason 应带出工具名 %s: %q", tool, d.AbortReason)
			}
			if !strings.Contains(d.AbortReason, "/mode exec normal") {
				t.Fatalf("AbortReason 应含切换指引: %q", d.AbortReason)
			}
		})
	}
}

// TestExecModeGuard_ReadonlyAllowsOtherTools readonly 模式只拦截写类工具，
// 读类 / Web / Meta 工具不匹配、不受影响。
func TestExecModeGuard_ReadonlyAllowsOtherTools(t *testing.T) {
	h := NewExecModeGuardHook(readonlyStore())
	for _, tool := range []string{
		"read_file", "list_dir", "grep_search", "glob_search",
		"web_search", "web_fetch", "publish_task", "send_message",
	} {
		if h.Matches(tool) {
			t.Errorf("Matches(%s) = true，期望 false（readonly 不应拦截该工具）", tool)
		}
	}
}

// TestExecModeGuard_OtherModesContinue normal / strict / yolo 模式下一律
// Continue——strict / yolo 的语义由后续切片实现，本 hook 不管。
func TestExecModeGuard_OtherModesContinue(t *testing.T) {
	for _, mode := range []modes.ExecMode{modes.ExecNormal, modes.ExecStrict, modes.ExecYolo} {
		t.Run(mode.String(), func(t *testing.T) {
			h := NewExecModeGuardHook(modes.NewStore(mode, modes.TopoTeam))
			for _, tool := range []string{"write_file", "edit_file", "run_shell"} {
				d := h.Run(hook.ToolHookContext{Phase: hook.PhasePreCall, ToolName: tool})
				if d.Action != hook.Continue {
					t.Errorf("%s 模式下 %s Action = %v，期望 Continue", mode, tool, d.Action)
				}
			}
		})
	}
}

// TestExecModeGuard_NilStoreSafe 未注入模式 store 时视为 normal，不拦截、不 panic。
func TestExecModeGuard_NilStoreSafe(t *testing.T) {
	h := NewExecModeGuardHook(nil)
	d := h.Run(hook.ToolHookContext{Phase: hook.PhasePreCall, ToolName: "write_file"})
	if d.Action != hook.Continue {
		t.Fatalf("nil store 下 Action = %v，期望 Continue", d.Action)
	}
}

// TestExecModeGuard_PhaseAndPriority 元信息：PreCall 阶段，Priority 5
// （早于 path-boundary 的 10 快速失败）。
func TestExecModeGuard_PhaseAndPriority(t *testing.T) {
	h := NewExecModeGuardHook(nil)
	if h.Phase() != hook.PhasePreCall {
		t.Fatalf("Phase = %v，期望 PhasePreCall", h.Phase())
	}
	if h.Priority() >= NewPathBoundaryHook(".").Priority() {
		t.Fatalf("Priority = %d，应早于 path-boundary(%d) 快速失败",
			h.Priority(), NewPathBoundaryHook(".").Priority())
	}
}
