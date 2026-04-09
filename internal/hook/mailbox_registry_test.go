package hook

import (
	"errors"
	"sync/atomic"
	"testing"

	"agentgo/internal/mailbox"
)

// ---- 单测专用 mock ----

// mockMailboxHook 是一个最小可配置 MailboxHook 实现。
type mockMailboxHook struct {
	name     string
	phase    MailboxHookPhase
	priority int
	runFn    func(hctx MailboxHookContext) MailboxHookDecision
	decision MailboxHookDecision
	callN    *atomic.Int32
}

func (m *mockMailboxHook) Name() string             { return m.name }
func (m *mockMailboxHook) Phase() MailboxHookPhase  { return m.phase }
func (m *mockMailboxHook) Priority() int            { return m.priority }
func (m *mockMailboxHook) Run(hctx MailboxHookContext) MailboxHookDecision {
	if m.callN != nil {
		m.callN.Add(1)
	}
	if m.runFn != nil {
		return m.runFn(hctx)
	}
	return m.decision
}

// panickingMailboxHook 永远 panic，验证 recover 机制。
type panickingMailboxHook struct {
	name     string
	phase    MailboxHookPhase
	priority int
}

func (p *panickingMailboxHook) Name() string                                            { return p.name }
func (p *panickingMailboxHook) Phase() MailboxHookPhase                                 { return p.phase }
func (p *panickingMailboxHook) Priority() int                                           { return p.priority }
func (p *panickingMailboxHook) Run(hctx MailboxHookContext) MailboxHookDecision {
	panic("测试用 panic")
}

// ---- Phase 常量 / Decision 字段 ----

func TestMailboxHookPhase_StringValues(t *testing.T) {
	if string(PhaseBeforeSend) != "beforeSend" {
		t.Errorf("PhaseBeforeSend = %q, want beforeSend", PhaseBeforeSend)
	}
	if string(PhaseBeforeDeliver) != "beforeDeliver" {
		t.Errorf("PhaseBeforeDeliver = %q, want beforeDeliver", PhaseBeforeDeliver)
	}
	if string(PhaseBeforeWake) != "beforeWake" {
		t.Errorf("PhaseBeforeWake = %q, want beforeWake", PhaseBeforeWake)
	}
}

// ---- Register ----

func TestMailboxRegister_Success(t *testing.T) {
	r := NewMailboxHookRegistry()
	h := &mockMailboxHook{name: "a", phase: PhaseBeforeSend, priority: 500}
	if err := r.Register(h); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
}

func TestMailboxRegister_RejectsDuplicate(t *testing.T) {
	r := NewMailboxHookRegistry()
	r.Register(&mockMailboxHook{name: "dup", phase: PhaseBeforeSend, priority: 100})
	err := r.Register(&mockMailboxHook{name: "dup", phase: PhaseBeforeWake, priority: 200})
	if !errors.Is(err, ErrMailboxHookNameConflict) {
		t.Errorf("expected ErrMailboxHookNameConflict, got %v", err)
	}
}

func TestMailboxRegister_RejectsInvalidPriority(t *testing.T) {
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
			r := NewMailboxHookRegistry()
			err := r.Register(&mockMailboxHook{name: tt.name, phase: PhaseBeforeSend, priority: tt.priority})
			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestMailboxRegister_RejectsNilAndEmptyName(t *testing.T) {
	r := NewMailboxHookRegistry()
	if err := r.Register(nil); err == nil {
		t.Error("expected error for nil hook")
	}
	if err := r.Register(&mockMailboxHook{name: "", phase: PhaseBeforeSend}); err == nil {
		t.Error("expected error for empty name")
	}
}

// ---- RunBeforeSend ----

func TestRunBeforeSend_PriorityAscendingOrder(t *testing.T) {
	r := NewMailboxHookRegistry()
	var order []string
	makeRecorder := func(name string, prio int) *mockMailboxHook {
		return &mockMailboxHook{
			name: name, phase: PhaseBeforeSend, priority: prio,
			runFn: func(MailboxHookContext) MailboxHookDecision {
				order = append(order, name)
				return MailboxHookDecision{Action: Continue}
			},
		}
	}
	r.Register(makeRecorder("mid", 50))
	r.Register(makeRecorder("low", 100))
	r.Register(makeRecorder("high", 10))

	r.RunBeforeSend(MailboxHookContext{Phase: PhaseBeforeSend})

	if len(order) != 3 || order[0] != "high" || order[1] != "mid" || order[2] != "low" {
		t.Errorf("order = %v, want [high mid low]", order)
	}
}

func TestRunBeforeSend_AbortShortCircuits(t *testing.T) {
	r := NewMailboxHookRegistry()
	var secondCalled bool
	r.Register(&mockMailboxHook{
		name: "aborter", phase: PhaseBeforeSend, priority: 10,
		decision: MailboxHookDecision{Action: Abort, AbortReason: "停止", HookName: "aborter"},
	})
	r.Register(&mockMailboxHook{
		name: "never", phase: PhaseBeforeSend, priority: 20,
		runFn: func(MailboxHookContext) MailboxHookDecision {
			secondCalled = true
			return MailboxHookDecision{Action: Continue}
		},
	})

	d := r.RunBeforeSend(MailboxHookContext{Phase: PhaseBeforeSend})
	if d.Action != Abort {
		t.Errorf("Action = %v, want Abort", d.Action)
	}
	if d.AbortReason != "停止" {
		t.Errorf("AbortReason = %q, want 停止", d.AbortReason)
	}
	if secondCalled {
		t.Error("short-circuit broken")
	}
}

func TestRunBeforeSend_EmptyRegistryReturnsContinue(t *testing.T) {
	r := NewMailboxHookRegistry()
	d := r.RunBeforeSend(MailboxHookContext{Phase: PhaseBeforeSend})
	if d.Action != Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
}

func TestRunBeforeSend_PhaseFiltering(t *testing.T) {
	// 注册多个不同 phase 的 hook，只有 BeforeSend 应当被触发
	r := NewMailboxHookRegistry()
	var sendCalled, deliverCalled, wakeCalled bool

	r.Register(&mockMailboxHook{
		name: "send", phase: PhaseBeforeSend, priority: 10,
		runFn: func(MailboxHookContext) MailboxHookDecision {
			sendCalled = true
			return MailboxHookDecision{Action: Continue}
		},
	})
	r.Register(&mockMailboxHook{
		name: "deliver", phase: PhaseBeforeDeliver, priority: 10,
		runFn: func(MailboxHookContext) MailboxHookDecision {
			deliverCalled = true
			return MailboxHookDecision{Action: Continue}
		},
	})
	r.Register(&mockMailboxHook{
		name: "wake", phase: PhaseBeforeWake, priority: 10,
		runFn: func(MailboxHookContext) MailboxHookDecision {
			wakeCalled = true
			return MailboxHookDecision{Action: Continue}
		},
	})

	r.RunBeforeSend(MailboxHookContext{Phase: PhaseBeforeSend})
	if !sendCalled || deliverCalled || wakeCalled {
		t.Errorf("phase filtering broken: send=%v deliver=%v wake=%v", sendCalled, deliverCalled, wakeCalled)
	}
}

// ---- RunBeforeDeliver ----

func TestRunBeforeDeliver_AbortStopsOneRecipient(t *testing.T) {
	r := NewMailboxHookRegistry()
	r.Register(&mockMailboxHook{
		name: "aborter", phase: PhaseBeforeDeliver, priority: 10,
		decision: MailboxHookDecision{Action: Abort, AbortReason: "skip"},
	})
	d := r.RunBeforeDeliver(MailboxHookContext{
		Phase:     PhaseBeforeDeliver,
		Message:   mailbox.Message{From: "a", To: "b"},
		DeliverTo: "b",
	})
	if d.Action != Abort {
		t.Errorf("Action = %v, want Abort", d.Action)
	}
}

func TestRunBeforeDeliver_DeliverToFieldVisible(t *testing.T) {
	r := NewMailboxHookRegistry()
	var seen string
	r.Register(&mockMailboxHook{
		name: "peeker", phase: PhaseBeforeDeliver, priority: 10,
		runFn: func(hctx MailboxHookContext) MailboxHookDecision {
			seen = hctx.DeliverTo
			return MailboxHookDecision{Action: Continue}
		},
	})
	r.RunBeforeDeliver(MailboxHookContext{Phase: PhaseBeforeDeliver, DeliverTo: "worker-3"})
	if seen != "worker-3" {
		t.Errorf("DeliverTo = %q, want worker-3", seen)
	}
}

// ---- RunBeforeWake (核心：累加语义) ----

func TestRunBeforeWake_EmptyRegistryReturnsContinue(t *testing.T) {
	r := NewMailboxHookRegistry()
	d := r.RunBeforeWake(MailboxHookContext{Phase: PhaseBeforeWake})
	if d.Action != Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
	if d.WakeDescription != "" {
		t.Errorf("WakeDescription should be empty, got %q", d.WakeDescription)
	}
}

func TestRunBeforeWake_AccumulatesDescriptions(t *testing.T) {
	r := NewMailboxHookRegistry()
	r.Register(&mockMailboxHook{
		name: "first", phase: PhaseBeforeWake, priority: 10,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: "片段一"},
	})
	r.Register(&mockMailboxHook{
		name: "second", phase: PhaseBeforeWake, priority: 20,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: "片段二"},
	})

	d := r.RunBeforeWake(MailboxHookContext{Phase: PhaseBeforeWake})
	if d.Action != Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
	want := "片段一\n\n片段二"
	if d.WakeDescription != want {
		t.Errorf("WakeDescription = %q, want %q", d.WakeDescription, want)
	}
}

func TestRunBeforeWake_SkipsEmptyDescriptions(t *testing.T) {
	r := NewMailboxHookRegistry()
	r.Register(&mockMailboxHook{
		name: "first", phase: PhaseBeforeWake, priority: 10,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: "有内容"},
	})
	r.Register(&mockMailboxHook{
		name: "empty", phase: PhaseBeforeWake, priority: 20,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: ""},
	})
	r.Register(&mockMailboxHook{
		name: "third", phase: PhaseBeforeWake, priority: 30,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: "更多"},
	})

	d := r.RunBeforeWake(MailboxHookContext{Phase: PhaseBeforeWake})
	want := "有内容\n\n更多"
	if d.WakeDescription != want {
		t.Errorf("WakeDescription = %q, want %q", d.WakeDescription, want)
	}
}

func TestRunBeforeWake_AbortShortCircuitsAndDiscardsAccumulated(t *testing.T) {
	r := NewMailboxHookRegistry()
	r.Register(&mockMailboxHook{
		name: "first", phase: PhaseBeforeWake, priority: 10,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: "已写入但会被丢弃"},
	})
	r.Register(&mockMailboxHook{
		name: "aborter", phase: PhaseBeforeWake, priority: 20,
		decision: MailboxHookDecision{Action: Abort, AbortReason: "停止唤醒"},
	})
	var thirdCalled bool
	r.Register(&mockMailboxHook{
		name: "third", phase: PhaseBeforeWake, priority: 30,
		runFn: func(MailboxHookContext) MailboxHookDecision {
			thirdCalled = true
			return MailboxHookDecision{Action: Continue}
		},
	})

	d := r.RunBeforeWake(MailboxHookContext{Phase: PhaseBeforeWake})
	if d.Action != Abort {
		t.Errorf("Action = %v, want Abort", d.Action)
	}
	if d.AbortReason != "停止唤醒" {
		t.Errorf("AbortReason = %q", d.AbortReason)
	}
	if d.WakeDescription != "" {
		t.Errorf("WakeDescription should be discarded on Abort, got %q", d.WakeDescription)
	}
	if thirdCalled {
		t.Error("third hook should not run after Abort")
	}
}

// ---- panic recovery (各 phase) ----

func TestMailboxRunBeforeSend_PanicRecoveredAsContinue(t *testing.T) {
	r := NewMailboxHookRegistry()
	var afterCalled bool
	r.Register(&panickingMailboxHook{name: "boom", phase: PhaseBeforeSend, priority: 10})
	r.Register(&mockMailboxHook{
		name: "after", phase: PhaseBeforeSend, priority: 20,
		runFn: func(MailboxHookContext) MailboxHookDecision {
			afterCalled = true
			return MailboxHookDecision{Action: Continue}
		},
	})

	d := r.RunBeforeSend(MailboxHookContext{Phase: PhaseBeforeSend})
	if d.Action != Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
	if !afterCalled {
		t.Error("hook after panic should run")
	}
}

func TestMailboxRunBeforeWake_PanicRecoveredContinuesAccumulation(t *testing.T) {
	r := NewMailboxHookRegistry()
	r.Register(&mockMailboxHook{
		name: "first", phase: PhaseBeforeWake, priority: 10,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: "前段"},
	})
	r.Register(&panickingMailboxHook{name: "boom", phase: PhaseBeforeWake, priority: 20})
	r.Register(&mockMailboxHook{
		name: "third", phase: PhaseBeforeWake, priority: 30,
		decision: MailboxHookDecision{Action: Continue, WakeDescription: "后段"},
	})

	d := r.RunBeforeWake(MailboxHookContext{Phase: PhaseBeforeWake})
	if d.Action != Continue {
		t.Errorf("Action = %v, want Continue", d.Action)
	}
	want := "前段\n\n后段"
	if d.WakeDescription != want {
		t.Errorf("WakeDescription = %q, want %q (panic should not break accumulation)", d.WakeDescription, want)
	}
}

// ---- nil registry safety ----

func TestNilMailboxRegistry_AllRunsReturnContinue(t *testing.T) {
	var r *MailboxHookRegistry
	if r.RunBeforeSend(MailboxHookContext{}).Action != Continue {
		t.Error("nil RunBeforeSend should return Continue")
	}
	if r.RunBeforeDeliver(MailboxHookContext{}).Action != Continue {
		t.Error("nil RunBeforeDeliver should return Continue")
	}
	if r.RunBeforeWake(MailboxHookContext{}).Action != Continue {
		t.Error("nil RunBeforeWake should return Continue")
	}
}

func TestNilMailboxRegistry_DoesNotPanic(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("nil receiver methods panicked: %v", rec)
		}
	}()
	var r *MailboxHookRegistry
	r.RunBeforeSend(MailboxHookContext{})
	r.RunBeforeDeliver(MailboxHookContext{})
	r.RunBeforeWake(MailboxHookContext{})
}

// ---- ToolHookRegistry independence ----

func TestMailboxRegistry_IndependentFromToolRegistry(t *testing.T) {
	// 注册一个 mailbox hook 不应当影响 ToolHookRegistry
	mr := NewMailboxHookRegistry()
	mr.Register(&mockMailboxHook{name: "m1", phase: PhaseBeforeSend, priority: 10})

	tr := NewToolHookRegistry()
	tr.Register(&mockHook{name: "t1", phase: PhasePreCall, matchStr: "*", priority: 10})

	// 各自独立运行
	mDec := mr.RunBeforeSend(MailboxHookContext{Phase: PhaseBeforeSend})
	tDec := tr.RunPre(ToolHookContext{ToolName: "x"})

	if mDec.Action != Continue {
		t.Error("mailbox run should be Continue")
	}
	if tDec.Action != Continue {
		t.Error("tool run should be Continue")
	}
}

// ---- joinFragments helper ----

func TestJoinFragments(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"single", []string{"a"}, "a"},
		{"two", []string{"a", "b"}, "a\n\nb"},
		{"three", []string{"a", "b", "c"}, "a\n\nb\n\nc"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := joinFragments(tt.in)
			if got != tt.want {
				t.Errorf("joinFragments(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
