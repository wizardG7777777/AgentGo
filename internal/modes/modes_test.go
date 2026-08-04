package modes

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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

// TestStore_IndependentAxes 验证两轴 Set/Get 完全独立：
// 拨动任意一轴，另一轴保持不动（各轴取值是并行关系而非互斥枚举）。
func TestStore_IndependentAxes(t *testing.T) {
	s := NewStore(ExecNormal, TopoTeam)

	s.SetExec(ExecReadonly)
	if s.GetExec() != ExecReadonly || s.GetTopo() != TopoTeam {
		t.Fatalf("SetExec 后两轴 = %v/%v，期望 readonly/team", s.GetExec(), s.GetTopo())
	}
	s.SetTopo(TopoSolo)
	if s.GetExec() != ExecReadonly || s.GetTopo() != TopoSolo {
		t.Fatalf("SetTopo 后两轴 = %v/%v，期望 readonly/solo", s.GetExec(), s.GetTopo())
	}
}

// TestStore_Snapshot 验证 Snapshot 返回两轴字符串瞬时值，且随后续 Set 更新。
func TestStore_Snapshot(t *testing.T) {
	s := DefaultStore()
	snap := s.Snapshot()
	if snap != (Snapshot{Exec: "normal", Topo: "team"}) {
		t.Fatalf("默认 Snapshot = %+v，期望 normal/team", snap)
	}

	s.SetExec(ExecYolo)
	s.SetTopo(TopoSolo)
	snap = s.Snapshot()
	if snap != (Snapshot{Exec: "yolo", Topo: "solo"}) {
		t.Fatalf("Set 后 Snapshot = %+v，期望 yolo/solo", snap)
	}
}

func TestStore_WithSnapshotRejectsDriftWithoutRunningEffect(t *testing.T) {
	s := DefaultStore()
	called := false
	err := s.WithSnapshot(Snapshot{Exec: "strict", Topo: "team"}, func() error {
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
	s := NewStore(ExecStrict, TopoSolo)
	if s.GetExec() != ExecStrict || s.GetTopo() != TopoSolo {
		t.Fatalf("NewStore 初值 = %v/%v，期望 strict/solo", s.GetExec(), s.GetTopo())
	}
}
