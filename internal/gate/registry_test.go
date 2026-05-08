package gate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubGate 是测试用 Gate，按指定参数返回固定 Decision。
type stubGate struct {
	name     string
	phase    Phase
	priority int
	matches  func(c Context) bool
	run      func(c Context) Decision
}

func (g *stubGate) Name() string  { return g.name }
func (g *stubGate) Phase() Phase  { return g.phase }
func (g *stubGate) Priority() int { return g.priority }
func (g *stubGate) Matches(c Context) bool {
	if g.matches == nil {
		return true
	}
	return g.matches(c)
}
func (g *stubGate) Run(c Context) Decision { return g.run(c) }

func newToolCtx(phase Phase, tool string) *ToolContext {
	return &ToolContext{
		PhaseField:   phase,
		AgentIDField: "agent-1",
		TaskIDField:  "task-1",
		CtxField:     context.Background(),
		ToolName:     tool,
	}
}

func TestRegistry_RegisterAndDispatchSinglePhase(t *testing.T) {
	r := NewRegistry()
	called := false
	g := &stubGate{
		name: "g1", phase: PhaseToolPreCall, priority: 100,
		run: func(c Context) Decision {
			called = true
			return Decision{Action: Continue}
		},
	}
	if err := r.Register(g); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := r.Dispatch(newToolCtx(PhaseToolPreCall, "read_file"))
	if !called {
		t.Error("gate not called")
	}
	if d.Action != Continue {
		t.Errorf("Action=%v want Continue", d.Action)
	}
}

func TestRegistry_AbortShortCircuits(t *testing.T) {
	r := NewRegistry()
	called2 := false
	r.Register(&stubGate{
		name: "g1", phase: PhaseToolPreCall, priority: 100,
		run: func(c Context) Decision {
			return Decision{Action: Abort, AbortReason: "deny", HookName: "g1"}
		},
	})
	r.Register(&stubGate{
		name: "g2", phase: PhaseToolPreCall, priority: 200,
		run: func(c Context) Decision {
			called2 = true
			return Decision{Action: Continue}
		},
	})
	d := r.Dispatch(newToolCtx(PhaseToolPreCall, "x"))
	if d.Action != Abort {
		t.Errorf("expected Abort got %v", d.Action)
	}
	if d.AbortReason != "deny" {
		t.Errorf("AbortReason=%q", d.AbortReason)
	}
	if called2 {
		t.Error("g2 should not run after g1 Abort")
	}
}

func TestRegistry_PriorityOrdering(t *testing.T) {
	r := NewRegistry()
	var order []string
	mkRun := func(tag string) func(c Context) Decision {
		return func(c Context) Decision {
			order = append(order, tag)
			return Decision{Action: Continue}
		}
	}
	// Register in reverse priority order; expect execution low-prio first.
	r.Register(&stubGate{name: "high", phase: PhaseToolPreCall, priority: 900, run: mkRun("high")})
	r.Register(&stubGate{name: "mid", phase: PhaseToolPreCall, priority: 500, run: mkRun("mid")})
	r.Register(&stubGate{name: "low", phase: PhaseToolPreCall, priority: 50, run: mkRun("low")})

	r.Dispatch(newToolCtx(PhaseToolPreCall, "x"))
	if strings.Join(order, ",") != "low,mid,high" {
		t.Errorf("priority order wrong: %v", order)
	}
}

func TestRegistry_PhaseIsolation(t *testing.T) {
	r := NewRegistry()
	preCalled := false
	postCalled := false
	r.Register(&stubGate{name: "pre", phase: PhaseToolPreCall, priority: 100,
		run: func(c Context) Decision { preCalled = true; return Decision{Action: Continue} }})
	r.Register(&stubGate{name: "post", phase: PhaseToolPostCall, priority: 100,
		run: func(c Context) Decision { postCalled = true; return Decision{Action: Continue} }})

	r.Dispatch(newToolCtx(PhaseToolPreCall, "x"))
	if !preCalled || postCalled {
		t.Errorf("phase isolation broken: pre=%v post=%v", preCalled, postCalled)
	}
}

func TestRegistry_MatchesFiltersOut(t *testing.T) {
	r := NewRegistry()
	called := false
	r.Register(&stubGate{
		name: "g", phase: PhaseToolPreCall, priority: 100,
		matches: func(c Context) bool {
			tc, ok := c.(*ToolContext)
			return ok && tc.ToolName == "write_file"
		},
		run: func(c Context) Decision {
			called = true
			return Decision{Action: Continue}
		},
	})
	r.Dispatch(newToolCtx(PhaseToolPreCall, "read_file"))
	if called {
		t.Error("Matches=false should skip Run")
	}
	r.Dispatch(newToolCtx(PhaseToolPreCall, "write_file"))
	if !called {
		t.Error("Matches=true should call Run")
	}
}

func TestRegistry_WakeDescriptionAccumulates(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubGate{name: "a", phase: PhaseMailboxBeforeWake, priority: 100,
		run: func(c Context) Decision { return Decision{Action: Continue, WakeDescription: "first"} }})
	r.Register(&stubGate{name: "b", phase: PhaseMailboxBeforeWake, priority: 200,
		run: func(c Context) Decision { return Decision{Action: Continue, WakeDescription: "second"} }})
	r.Register(&stubGate{name: "c", phase: PhaseMailboxBeforeWake, priority: 300,
		run: func(c Context) Decision { return Decision{Action: Continue} }}) // 不贡献 desc

	mc := &MailboxContext{
		PhaseField:   PhaseMailboxBeforeWake,
		AgentIDField: "agent-1",
		CtxField:     context.Background(),
	}
	d := r.Dispatch(mc)
	if d.Action != Continue {
		t.Errorf("Action=%v want Continue", d.Action)
	}
	if d.WakeDescription != "first\n\nsecond" {
		t.Errorf("WakeDescription=%q want %q", d.WakeDescription, "first\n\nsecond")
	}
}

func TestRegistry_NameConflictRejected(t *testing.T) {
	r := NewRegistry()
	g1 := &stubGate{name: "dup", phase: PhaseToolPreCall, priority: 100,
		run: func(c Context) Decision { return Decision{Action: Continue} }}
	g2 := &stubGate{name: "dup", phase: PhaseMailboxBeforeWake, priority: 100,
		run: func(c Context) Decision { return Decision{Action: Continue} }}
	if err := r.Register(g1); err != nil {
		t.Fatalf("first Register failed: %v", err)
	}
	if err := r.Register(g2); !errors.Is(err, ErrGateNameConflict) {
		t.Errorf("duplicate name should be rejected with ErrGateNameConflict, got %v", err)
	}
}

func TestRegistry_PriorityOutOfRangeRejected(t *testing.T) {
	r := NewRegistry()
	for _, p := range []int{-1, 1001} {
		err := r.Register(&stubGate{name: "g", phase: PhaseToolPreCall, priority: p,
			run: func(c Context) Decision { return Decision{Action: Continue} }})
		if !errors.Is(err, ErrGatePriorityInvalid) {
			t.Errorf("priority=%d should fail with ErrGatePriorityInvalid, got %v", p, err)
		}
	}
}

func TestRegistry_NilRegistrySafe(t *testing.T) {
	var r *Registry
	d := r.Dispatch(newToolCtx(PhaseToolPreCall, "x"))
	if d.Action != Continue {
		t.Errorf("nil Registry Dispatch should be Continue")
	}
}

func TestRegistry_PanicRecoveryContinues(t *testing.T) {
	r := NewRegistry()
	called2 := false
	r.Register(&stubGate{name: "panicky", phase: PhaseToolPreCall, priority: 100,
		run: func(c Context) Decision { panic("boom") }})
	r.Register(&stubGate{name: "next", phase: PhaseToolPreCall, priority: 200,
		run: func(c Context) Decision {
			called2 = true
			return Decision{Action: Continue}
		}})
	d := r.Dispatch(newToolCtx(PhaseToolPreCall, "x"))
	if d.Action != Continue {
		t.Errorf("after panic recovery Action=%v want Continue", d.Action)
	}
	if !called2 {
		t.Error("panic in g1 should not stop g2 from running")
	}
}

func TestRegistry_ArgsCopyOnWriteIsolation(t *testing.T) {
	r := NewRegistry()
	original := map[string]any{"k": "orig"}
	tc := newToolCtx(PhaseToolPreCall, "x")
	tc.Args = original
	r.Register(&stubGate{name: "mut", phase: PhaseToolPreCall, priority: 100,
		run: func(c Context) Decision {
			ctx := c.(*ToolContext)
			ctx.Args["k"] = "mutated"
			return Decision{Action: Continue}
		}})
	r.Dispatch(tc)
	if original["k"] != "orig" {
		t.Errorf("Gate mutation leaked to caller's Args: %v", original)
	}
}

func TestRegistry_ArgsCopyOnWriteIsolationInMatches(t *testing.T) {
	r := NewRegistry()
	original := map[string]any{"k": "orig"}
	tc := newToolCtx(PhaseToolPreCall, "x")
	tc.Args = original

	r.Register(&stubGate{
		name: "mut-matches", phase: PhaseToolPreCall, priority: 100,
		matches: func(c Context) bool {
			ctx := c.(*ToolContext)
			ctx.Args["k"] = "mutated-in-matches"
			return true
		},
		run: func(c Context) Decision {
			ctx := c.(*ToolContext)
			if ctx.Args["k"] != "orig" {
				t.Fatalf("Run should receive a fresh Args copy, got %v", ctx.Args)
			}
			return Decision{Action: Continue}
		},
	})

	r.Dispatch(tc)
	if original["k"] != "orig" {
		t.Errorf("Gate Matches mutation leaked to caller's Args: %v", original)
	}
}
