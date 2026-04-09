package hook

import (
	"errors"
	"sync/atomic"
	"testing"
)

// ---- 单测专用 mock ----

// mockHook 是一个最小可配置 ToolHook 实现，按值传递语义设计。
// 与并行测试代理的 mockToolHook 无关（后者用指针传递且 Run 字段定义非法，
// 由 build tag 屏蔽）。
type mockHook struct {
	name     string
	phase    ToolHookPhase
	matchStr string // 为 "*" 时通配；否则精确匹配
	priority int
	// runFn 允许测试注入自定义 Run 行为；nil 时返回 decision 字段
	runFn    func(hctx ToolHookContext) ToolHookDecision
	decision ToolHookDecision
	callN    *atomic.Int32
}

func (m *mockHook) Name() string              { return m.name }
func (m *mockHook) Phase() ToolHookPhase      { return m.phase }
func (m *mockHook) Priority() int             { return m.priority }
func (m *mockHook) Matches(toolName string) bool {
	if m.matchStr == "*" {
		return true
	}
	return m.matchStr == toolName
}
func (m *mockHook) Run(hctx ToolHookContext) ToolHookDecision {
	if m.callN != nil {
		m.callN.Add(1)
	}
	if m.runFn != nil {
		return m.runFn(hctx)
	}
	return m.decision
}

// panickingHook 永远 panic，用于验证 recover 机制
type panickingHook struct {
	name     string
	phase    ToolHookPhase
	priority int
}

func (p *panickingHook) Name() string                       { return p.name }
func (p *panickingHook) Phase() ToolHookPhase               { return p.phase }
func (p *panickingHook) Priority() int                      { return p.priority }
func (p *panickingHook) Matches(toolName string) bool       { return true }
func (p *panickingHook) Run(hctx ToolHookContext) ToolHookDecision {
	panic("测试用 panic")
}

// ---- 基础常量 ----

func TestToolHookPhase_Strings(t *testing.T) {
	if string(PhasePreCall) != "preCall" {
		t.Errorf("PhasePreCall = %q, want preCall", PhasePreCall)
	}
	if string(PhasePostCall) != "postCall" {
		t.Errorf("PhasePostCall = %q, want postCall", PhasePostCall)
	}
}

func TestHookAction_Enum(t *testing.T) {
	if Continue != 0 {
		t.Errorf("Continue = %d, want 0", Continue)
	}
	if Abort != 1 {
		t.Errorf("Abort = %d, want 1", Abort)
	}
}

// ---- Registry.Register ----

func TestRegister_Success(t *testing.T) {
	r := NewToolHookRegistry()
	h := &mockHook{name: "a", phase: PhasePreCall, matchStr: "*", priority: 500}
	if err := r.Register(h); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
}

func TestRegister_RejectsDuplicate(t *testing.T) {
	r := NewToolHookRegistry()
	r.Register(&mockHook{name: "dup", phase: PhasePreCall, matchStr: "*", priority: 100})
	err := r.Register(&mockHook{name: "dup", phase: PhasePostCall, matchStr: "*", priority: 200})
	if !errors.Is(err, ErrHookNameConflict) {
		t.Errorf("expected ErrHookNameConflict, got %v", err)
	}
}

func TestRegister_RejectsPriorityOutOfRange(t *testing.T) {
	tests := []struct {
		name     string
		priority int
		wantErr  bool
	}{
		{"negative", -1, true},
		{"zero", 0, false},
		{"normal", 500, false},
		{"max", 1000, false},
		{"overmax", 1001, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewToolHookRegistry()
			err := r.Register(&mockHook{
				name: tt.name, phase: PhasePreCall, matchStr: "*", priority: tt.priority,
			})
			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRegister_RejectsNilAndEmptyName(t *testing.T) {
	r := NewToolHookRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("expected error for nil hook")
	}
	if err := r.Register(&mockHook{name: "", phase: PhasePreCall, matchStr: "*"}); err == nil {
		t.Error("expected error for empty name")
	}
}

// ---- Registry.RunPre ordering and short-circuit ----

func TestRunPre_PriorityAscendingOrder(t *testing.T) {
	r := NewToolHookRegistry()
	var order []string
	makeRecorder := func(name string, prio int) *mockHook {
		return &mockHook{
			name: name, phase: PhasePreCall, matchStr: "*", priority: prio,
			runFn: func(ToolHookContext) ToolHookDecision {
				order = append(order, name)
				return ToolHookDecision{Action: Continue}
			},
		}
	}
	// 故意乱序注册
	r.Register(makeRecorder("mid", 50))
	r.Register(makeRecorder("low", 100))
	r.Register(makeRecorder("high", 10))

	r.RunPre(ToolHookContext{ToolName: "x"})

	if len(order) != 3 || order[0] != "high" || order[1] != "mid" || order[2] != "low" {
		t.Errorf("order = %v, want [high mid low]", order)
	}
}

func TestRunPre_AbortShortCircuits(t *testing.T) {
	r := NewToolHookRegistry()
	var secondCalled bool
	r.Register(&mockHook{
		name: "aborter", phase: PhasePreCall, matchStr: "*", priority: 10,
		decision: ToolHookDecision{Action: Abort, AbortReason: "停止", HookName: "aborter"},
	})
	r.Register(&mockHook{
		name: "never", phase: PhasePreCall, matchStr: "*", priority: 20,
		runFn: func(ToolHookContext) ToolHookDecision {
			secondCalled = true
			return ToolHookDecision{Action: Continue}
		},
	})

	d := r.RunPre(ToolHookContext{ToolName: "x"})
	if d.Action != Abort {
		t.Errorf("Action = %v, want Abort", d.Action)
	}
	if d.AbortReason != "停止" {
		t.Errorf("AbortReason = %q, want 停止", d.AbortReason)
	}
	if secondCalled {
		t.Error("short-circuit broken: second hook ran after Abort")
	}
}

func TestRunPre_EmptyRegistryReturnsContinue(t *testing.T) {
	r := NewToolHookRegistry()
	d := r.RunPre(ToolHookContext{ToolName: "x"})
	if d.Action != Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
}

func TestRunPre_MatchesFiltering(t *testing.T) {
	r := NewToolHookRegistry()
	var writeCalled, readCalled bool
	r.Register(&mockHook{
		name: "w", phase: PhasePreCall, matchStr: "write_file", priority: 10,
		runFn: func(ToolHookContext) ToolHookDecision {
			writeCalled = true
			return ToolHookDecision{Action: Continue}
		},
	})
	r.Register(&mockHook{
		name: "re", phase: PhasePreCall, matchStr: "read_file", priority: 10,
		runFn: func(ToolHookContext) ToolHookDecision {
			readCalled = true
			return ToolHookDecision{Action: Continue}
		},
	})

	r.RunPre(ToolHookContext{ToolName: "write_file"})
	if !writeCalled || readCalled {
		t.Errorf("write_file path: writeCalled=%v readCalled=%v", writeCalled, readCalled)
	}
}

func TestRunPre_WildcardMatchesAnyTool(t *testing.T) {
	r := NewToolHookRegistry()
	calls := atomic.Int32{}
	r.Register(&mockHook{
		name: "any", phase: PhasePreCall, matchStr: "*", priority: 10,
		callN:    &calls,
		decision: ToolHookDecision{Action: Continue},
	})
	for _, tool := range []string{"read_file", "write_file", "list_dir", "unknown_tool"} {
		r.RunPre(ToolHookContext{ToolName: tool})
	}
	if calls.Load() != 4 {
		t.Errorf("wildcard matched %d times, want 4", calls.Load())
	}
}

// ---- Args shallow copy isolation ----

func TestRunPre_ArgsShallowCopyIsolatesHook(t *testing.T) {
	r := NewToolHookRegistry()
	r.Register(&mockHook{
		name: "mutator", phase: PhasePreCall, matchStr: "*", priority: 10,
		runFn: func(hctx ToolHookContext) ToolHookDecision {
			// hook 尝试通过 map 引用修改 — 这应当只影响自己的副本
			hctx.Args["injected"] = true
			return ToolHookDecision{Action: Continue}
		},
	})

	originalArgs := map[string]any{"path": "docs/foo.md"}
	r.RunPre(ToolHookContext{ToolName: "x", Args: originalArgs})

	if _, exists := originalArgs["injected"]; exists {
		t.Error("原始 Args 被 hook 污染 — 浅拷贝隔离失效")
	}
	if _, exists := originalArgs["path"]; !exists {
		t.Error("原始 Args 丢失了 path 字段")
	}
}

func TestRunPre_ArgsCopyIsolatesBetweenHooks(t *testing.T) {
	// hook A 往 Args 写 key → hook B 应当看不到（因为 B 收到的是独立副本）
	r := NewToolHookRegistry()
	var bSawA bool
	r.Register(&mockHook{
		name: "a", phase: PhasePreCall, matchStr: "*", priority: 10,
		runFn: func(hctx ToolHookContext) ToolHookDecision {
			hctx.Args["from_a"] = true
			return ToolHookDecision{Action: Continue}
		},
	})
	r.Register(&mockHook{
		name: "b", phase: PhasePreCall, matchStr: "*", priority: 20,
		runFn: func(hctx ToolHookContext) ToolHookDecision {
			_, bSawA = hctx.Args["from_a"]
			return ToolHookDecision{Action: Continue}
		},
	})

	r.RunPre(ToolHookContext{ToolName: "x", Args: map[string]any{}})

	if bSawA {
		t.Error("hook B 看到了 hook A 写入的 key — hook 间副本隔离失效")
	}
}

// ---- Panic recovery ----

func TestRunPre_PanicRecoveredAsContinue(t *testing.T) {
	r := NewToolHookRegistry()
	var afterCalled bool
	r.Register(&panickingHook{name: "boom", phase: PhasePreCall, priority: 10})
	r.Register(&mockHook{
		name: "after", phase: PhasePreCall, matchStr: "*", priority: 20,
		runFn: func(ToolHookContext) ToolHookDecision {
			afterCalled = true
			return ToolHookDecision{Action: Continue}
		},
	})

	d := r.RunPre(ToolHookContext{ToolName: "x"})
	if d.Action != Continue {
		t.Errorf("panic 恢复后 Action = %v, want Continue", d.Action)
	}
	if !afterCalled {
		t.Error("panic 之后的 hook 应当继续执行")
	}
}

func TestRunPost_NoReturnNoShortCircuit(t *testing.T) {
	r := NewToolHookRegistry()
	var aCalled, bCalled bool
	// a 返回 Abort，但 post 阶段应当忽略该返回值
	r.Register(&mockHook{
		name: "a", phase: PhasePostCall, matchStr: "*", priority: 10,
		runFn: func(ToolHookContext) ToolHookDecision {
			aCalled = true
			return ToolHookDecision{Action: Abort, AbortReason: "应当被忽略"}
		},
	})
	r.Register(&mockHook{
		name: "b", phase: PhasePostCall, matchStr: "*", priority: 20,
		runFn: func(ToolHookContext) ToolHookDecision {
			bCalled = true
			return ToolHookDecision{Action: Continue}
		},
	})

	r.RunPost(ToolHookContext{ToolName: "x", Result: "ok"})
	if !aCalled || !bCalled {
		t.Errorf("post 阶段应不短路：a=%v b=%v", aCalled, bCalled)
	}
}

func TestRunPost_PanicRecoveredContinues(t *testing.T) {
	r := NewToolHookRegistry()
	var afterCalled bool
	r.Register(&panickingHook{name: "boom-post", phase: PhasePostCall, priority: 10})
	r.Register(&mockHook{
		name: "after-post", phase: PhasePostCall, matchStr: "*", priority: 20,
		runFn: func(ToolHookContext) ToolHookDecision {
			afterCalled = true
			return ToolHookDecision{Action: Continue}
		},
	})
	r.RunPost(ToolHookContext{ToolName: "x"})
	if !afterCalled {
		t.Error("post hook panic 后续 hook 应继续执行")
	}
}

// ---- Nil registry safety ----

func TestNilRegistry_RunPreReturnsContinue(t *testing.T) {
	var r *ToolHookRegistry // 显式 nil
	d := r.RunPre(ToolHookContext{ToolName: "x"})
	if d.Action != Continue {
		t.Errorf("nil registry RunPre Action = %v, want Continue", d.Action)
	}
}

func TestNilRegistry_RunPostNoPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("nil registry RunPost panicked: %v", rec)
		}
	}()
	var r *ToolHookRegistry
	r.RunPost(ToolHookContext{ToolName: "x"})
}

// ---- Mixed phase isolation ----

func TestRunPre_IgnoresPostHooks(t *testing.T) {
	r := NewToolHookRegistry()
	var postCalled bool
	r.Register(&mockHook{
		name: "post", phase: PhasePostCall, matchStr: "*", priority: 10,
		runFn: func(ToolHookContext) ToolHookDecision {
			postCalled = true
			return ToolHookDecision{Action: Continue}
		},
	})
	r.RunPre(ToolHookContext{ToolName: "x"})
	if postCalled {
		t.Error("RunPre 不应触发 post 阶段的 hook")
	}
}
