package tui

import "testing"

func TestCalcLayout_NormalWidth(t *testing.T) {
	l := calcLayout(120, 40, ViewDashboard)

	if l.Compact {
		t.Error("expected normal mode at width=120, got compact")
	}
	if l.SidebarW != sidebarMinWidth {
		t.Errorf("SidebarW = %d, want %d", l.SidebarW, sidebarMinWidth)
	}
	if l.HeaderH != headerHeight {
		t.Errorf("HeaderH = %d, want %d", l.HeaderH, headerHeight)
	}
	if l.StatusH != statusBarHeight {
		t.Errorf("StatusH = %d, want %d", l.StatusH, statusBarHeight)
	}
	if l.MainX != l.SidebarW+1 {
		t.Errorf("MainX = %d, want %d", l.MainX, l.SidebarW+1)
	}
	if l.MainW <= 0 {
		t.Errorf("MainW = %d, should be positive", l.MainW)
	}
	if l.MainH <= 0 {
		t.Errorf("MainH = %d, should be positive", l.MainH)
	}
}

func TestCalcLayout_CompactMode(t *testing.T) {
	l := calcLayout(60, 30, ViewDashboard)

	if !l.Compact {
		t.Error("expected compact mode at width=60")
	}
	if l.SidebarW != 0 {
		t.Errorf("SidebarW = %d, want 0 in compact mode", l.SidebarW)
	}
	if l.MainX != 0 {
		t.Errorf("MainX = %d, want 0 in compact mode", l.MainX)
	}
	if l.MainW != 60 {
		t.Errorf("MainW = %d, want 60 in compact mode", l.MainW)
	}
}

func TestCalcLayout_CompactThreshold(t *testing.T) {
	justBelow := calcLayout(compactThreshold-1, 30, ViewDashboard)
	if !justBelow.Compact {
		t.Errorf("width=%d should be compact", compactThreshold-1)
	}

	atThreshold := calcLayout(compactThreshold, 30, ViewDashboard)
	if atThreshold.Compact {
		t.Errorf("width=%d should NOT be compact", compactThreshold)
	}
}

func TestCalcLayout_WideSidebarExpansion(t *testing.T) {
	l := calcLayout(150, 40, ViewDashboard)
	if l.SidebarW != sidebarMaxWidth {
		t.Errorf("SidebarW = %d, want %d at width=150", l.SidebarW, sidebarMaxWidth)
	}
}

func TestCalcLayout_VerticalDistribution(t *testing.T) {
	l := calcLayout(120, 40, ViewDashboard)

	// header(1) + body + input(3) + status(1) = 40
	expectedBody := 40 - headerHeight - inputHeight - statusBarHeight
	if l.MainH != expectedBody {
		t.Errorf("MainH = %d, want %d", l.MainH, expectedBody)
	}
	if l.InputY+l.InputH != l.StatusY {
		t.Error("input should be directly above status bar")
	}
}

func TestCalcLayout_TinyTerminal(t *testing.T) {
	l := calcLayout(10, 5, ViewDashboard)

	if l.MainW < 1 {
		t.Errorf("MainW = %d, should be at least 1", l.MainW)
	}
	if l.MainH < 1 {
		t.Errorf("MainH = %d, should be at least 1", l.MainH)
	}
}

func TestCalcLayout_ApprovalOverlapsInput(t *testing.T) {
	l := calcLayout(120, 40, ViewDashboard)
	if l.ApprovalY != l.InputY {
		t.Errorf("ApprovalY = %d, want %d (should overlap input)", l.ApprovalY, l.InputY)
	}
}
