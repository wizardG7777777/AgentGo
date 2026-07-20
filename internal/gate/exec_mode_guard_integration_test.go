package gate

import (
	"strings"
	"testing"

	"agentgo/internal/hook/builtin"
	"agentgo/internal/modes"
)

// TestExecModeGuard_ThroughRegistry 经 WrapToolHook 适配 + Registry 分发的
// 集成路径：readonly 模式下写类工具在 PreCall 被 Abort，normal 模式放行。
// 该路径与 bootstrap 装配（gate.WrapToolHook → gateReg.Register）一致。
func TestExecModeGuard_ThroughRegistry(t *testing.T) {
	store := modes.DefaultStore()
	reg := NewRegistry()
	if err := reg.Register(WrapToolHook(builtin.NewExecModeGuardHook(store))); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// normal 模式：写类工具放行。
	if d := reg.Dispatch(newToolCtx(PhaseToolPreCall, "write_file")); d.Action != Continue {
		t.Fatalf("normal 模式 Action = %v，期望 Continue", d.Action)
	}

	// 切到 readonly：写类工具 Abort，错误消息含 readonly。
	store.SetExec(modes.ExecReadonly)
	d := reg.Dispatch(newToolCtx(PhaseToolPreCall, "run_shell"))
	if d.Action != Abort {
		t.Fatalf("readonly 模式 Action = %v，期望 Abort", d.Action)
	}
	if !strings.Contains(d.AbortReason, "readonly") {
		t.Fatalf("AbortReason 应含 readonly: %q", d.AbortReason)
	}

	// readonly 下读类工具不受影响（Matches 过滤）。
	if d := reg.Dispatch(newToolCtx(PhaseToolPreCall, "read_file")); d.Action != Continue {
		t.Fatalf("readonly 模式 read_file Action = %v，期望 Continue", d.Action)
	}
}
