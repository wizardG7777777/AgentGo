package tui

import "testing"

func TestCalcLayout_NormalWidth(t *testing.T) {
	l := calcLayout(120, 40)

	if l.Compact {
		t.Error("expected normal mode at width=120, got compact")
	}
	if l.StatusH != statusBarHeight {
		t.Errorf("StatusH = %d, want %d", l.StatusH, statusBarHeight)
	}
	if l.MainY != 0 {
		t.Errorf("MainY = %d, want 0（顶栏已并入状态栏，body 从第 0 行起）", l.MainY)
	}
	// 无侧边栏：主面板始终全宽。
	if l.MainX != 0 {
		t.Errorf("MainX = %d, want 0", l.MainX)
	}
	if l.MainW != 120 {
		t.Errorf("MainW = %d, want 120（全宽）", l.MainW)
	}
	if l.MainH <= 0 {
		t.Errorf("MainH = %d, should be positive", l.MainH)
	}
}

func TestCalcLayout_CompactMode(t *testing.T) {
	l := calcLayout(60, 30)

	if !l.Compact {
		t.Error("expected compact mode at width=60")
	}
	if l.MainX != 0 {
		t.Errorf("MainX = %d, want 0 in compact mode", l.MainX)
	}
	if l.MainW != 60 {
		t.Errorf("MainW = %d, want 60 in compact mode", l.MainW)
	}
}

func TestCalcLayout_CompactThreshold(t *testing.T) {
	justBelow := calcLayout(compactThreshold-1, 30)
	if !justBelow.Compact {
		t.Errorf("width=%d should be compact", compactThreshold-1)
	}

	atThreshold := calcLayout(compactThreshold, 30)
	if atThreshold.Compact {
		t.Errorf("width=%d should NOT be compact", compactThreshold)
	}
}

func TestCalcLayout_VerticalDistribution(t *testing.T) {
	l := calcLayout(120, 40)

	// body + input(3) + status(1) = 40（顶栏已并入状态栏）
	expectedBody := 40 - inputHeight - statusBarHeight
	if l.MainH != expectedBody {
		t.Errorf("MainH = %d, want %d", l.MainH, expectedBody)
	}
	if l.InputY+l.InputH != l.StatusY {
		t.Error("input should be directly above status bar")
	}
}

func TestCalcLayout_DynamicInputHeight(t *testing.T) {
	l := calcLayout(120, 40, 8)

	expectedBody := 40 - 8 - statusBarHeight
	if l.InputH != 8 {
		t.Errorf("InputH = %d, want 8", l.InputH)
	}
	if l.MainH != expectedBody {
		t.Errorf("MainH = %d, want %d", l.MainH, expectedBody)
	}
	if l.InputY+l.InputH != l.StatusY {
		t.Error("dynamic input should stay directly above status bar")
	}
}

func TestCalcLayout_TinyTerminal(t *testing.T) {
	l := calcLayout(10, 5)

	if l.MainW < 1 {
		t.Errorf("MainW = %d, should be at least 1", l.MainW)
	}
	if l.MainH < 1 {
		t.Errorf("MainH = %d, should be at least 1", l.MainH)
	}
}

func TestCalcLayout_InteractionStacksAboveInput(t *testing.T) {
	l := calcLayout(120, 40, 4, 7)
	if l.InteractionH != 7 {
		t.Fatalf("InteractionH = %d, want 7", l.InteractionH)
	}
	if l.InteractionY+l.InteractionH != l.InputY {
		t.Fatalf("Interaction should end where input starts: interactionY=%d interactionH=%d inputY=%d",
			l.InteractionY, l.InteractionH, l.InputY)
	}
	if l.InputY+l.InputH != l.StatusY {
		t.Error("input should remain directly above status bar")
	}
}
