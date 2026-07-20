package modes

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestGateMode_ParseStringRoundtrip 验证 gate 轴 String→Parse 往返一致。
func TestGateMode_ParseStringRoundtrip(t *testing.T) {
	for _, g := range []GateMode{GateImmediate, GatePlan} {
		got, err := ParseGateMode(g.String())
		if err != nil {
			t.Fatalf("ParseGateMode(%q) 报错: %v", g.String(), err)
		}
		if got != g {
			t.Errorf("往返不一致: %v → %q → %v", g, g.String(), got)
		}
	}
}

// TestExecMode_ParseStringRoundtrip 验证 exec 轴 String→Parse 往返一致。
func TestExecMode_ParseStringRoundtrip(t *testing.T) {
	for _, e := range []ExecMode{ExecNormal, ExecStrict, ExecReadonly, ExecYolo} {
		got, err := ParseExecMode(e.String())
		if err != nil {
			t.Fatalf("ParseExecMode(%q) 报错: %v", e.String(), err)
		}
		if got != e {
			t.Errorf("往返不一致: %v → %q → %v", e, e.String(), got)
		}
	}
}

// TestTopoMode_ParseStringRoundtrip 验证 topo 轴 String→Parse 往返一致。
func TestTopoMode_ParseStringRoundtrip(t *testing.T) {
	for _, m := range []TopoMode{TopoTeam, TopoSolo} {
		got, err := ParseTopoMode(m.String())
		if err != nil {
			t.Fatalf("ParseTopoMode(%q) 报错: %v", m.String(), err)
		}
		if got != m {
			t.Errorf("往返不一致: %v → %q → %v", m, m.String(), got)
		}
	}
}

// TestParse_CaseAndSpaceTolerant 验证解析容错大小写与首尾空白。
func TestParse_CaseAndSpaceTolerant(t *testing.T) {
	g, err := ParseGateMode("  PLAN ")
	if err != nil || g != GatePlan {
		t.Errorf("ParseGateMode(\"  PLAN \") = %v, %v，期望 GatePlan, nil", g, err)
	}
	e, err := ParseExecMode("ReadOnly")
	if err != nil || e != ExecReadonly {
		t.Errorf("ParseExecMode(\"ReadOnly\") = %v, %v，期望 ExecReadonly, nil", e, err)
	}
	m, err := ParseTopoMode("SOLO")
	if err != nil || m != TopoSolo {
		t.Errorf("ParseTopoMode(\"SOLO\") = %v, %v，期望 TopoSolo, nil", m, err)
	}
}

// TestParse_UnknownValueRejected 验证未知值与空串一律报错。
func TestParse_UnknownValueRejected(t *testing.T) {
	if _, err := ParseGateMode("fast"); err == nil {
		t.Error("ParseGateMode(\"fast\") 应报错")
	}
	if _, err := ParseExecMode(""); err == nil {
		t.Error("ParseExecMode(\"\") 应报错（空串走默认值，不交给 Parse）")
	}
	if _, err := ParseTopoMode("pair"); err == nil {
		t.Error("ParseTopoMode(\"pair\") 应报错")
	}
	// 错误消息应列出可选值，方便用户改配置
	if _, err := ParseExecMode("super"); err == nil || !strings.Contains(err.Error(), "normal") {
		t.Errorf("ParseExecMode 错误消息应含可选值，got: %v", err)
	}
}

// TestStore_IndependentAxes 验证三轴 Set/Get 完全独立：
// 拨动任意一轴，另外两轴保持不动（plan 与 readonly/solo 是并行关系）。
func TestStore_IndependentAxes(t *testing.T) {
	s := NewStore(GateImmediate, ExecNormal, TopoTeam)

	s.SetGate(GatePlan)
	if s.GetGate() != GatePlan || s.GetExec() != ExecNormal || s.GetTopo() != TopoTeam {
		t.Fatalf("SetGate 后三轴 = %v/%v/%v，期望 plan/normal/team", s.GetGate(), s.GetExec(), s.GetTopo())
	}
	s.SetExec(ExecReadonly)
	if s.GetGate() != GatePlan || s.GetExec() != ExecReadonly || s.GetTopo() != TopoTeam {
		t.Fatalf("SetExec 后三轴 = %v/%v/%v，期望 plan/readonly/team", s.GetGate(), s.GetExec(), s.GetTopo())
	}
	s.SetTopo(TopoSolo)
	if s.GetGate() != GatePlan || s.GetExec() != ExecReadonly || s.GetTopo() != TopoSolo {
		t.Fatalf("SetTopo 后三轴 = %v/%v/%v，期望 plan/readonly/solo", s.GetGate(), s.GetExec(), s.GetTopo())
	}
}

// TestStore_Snapshot 验证 Snapshot 返回三轴字符串瞬时值，且随后续 Set 更新。
func TestStore_Snapshot(t *testing.T) {
	s := DefaultStore()
	snap := s.Snapshot()
	if snap != (Snapshot{Gate: "immediate", Exec: "normal", Topo: "team"}) {
		t.Fatalf("默认 Snapshot = %+v，期望 immediate/normal/team", snap)
	}

	s.SetGate(GatePlan)
	s.SetExec(ExecYolo)
	s.SetTopo(TopoSolo)
	snap = s.Snapshot()
	if snap != (Snapshot{Gate: "plan", Exec: "yolo", Topo: "solo"}) {
		t.Fatalf("Set 后 Snapshot = %+v，期望 plan/yolo/solo", snap)
	}
}

func TestStore_WithSnapshotRejectsDriftWithoutRunningEffect(t *testing.T) {
	s := DefaultStore()
	called := false
	err := s.WithSnapshot(Snapshot{Gate: "plan", Exec: "normal", Topo: "team"}, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrSnapshotChanged) {
		t.Fatalf("WithSnapshot drift error = %v", err)
	}
	if called {
		t.Fatal("模式快照不匹配时不得运行 effect")
	}
}

func TestStore_WithSnapshotSerializesConcurrentModeChange(t *testing.T) {
	s := DefaultStore()
	entered := make(chan struct{})
	release := make(chan struct{})
	effectDone := make(chan error, 1)
	go func() {
		effectDone <- s.WithSnapshot(s.Snapshot(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	setDone := make(chan struct{})
	go func() {
		s.SetTopo(TopoSolo)
		close(setDone)
	}()
	select {
	case <-setDone:
		t.Fatal("模式修改不得穿过已绑定快照的 effect 窗口")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-effectDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-setDone:
	case <-time.After(time.Second):
		t.Fatal("effect 结束后模式修改仍未完成")
	}
	if got := s.GetTopo(); got != TopoSolo {
		t.Fatalf("topo = %s, want solo", got)
	}
}

// TestStore_NewStoreInitialValues 验证 NewStore 初值按参数生效。
func TestStore_NewStoreInitialValues(t *testing.T) {
	s := NewStore(GatePlan, ExecStrict, TopoSolo)
	if s.GetGate() != GatePlan || s.GetExec() != ExecStrict || s.GetTopo() != TopoSolo {
		t.Fatalf("NewStore 初值 = %v/%v/%v，期望 plan/strict/solo", s.GetGate(), s.GetExec(), s.GetTopo())
	}
}
