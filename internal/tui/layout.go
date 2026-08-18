package tui

// calcLayout computes panel dimensions from terminal size and view state.
// rows[0] is the input area height; rows[1], when present, is the Interaction
// panel height. The two lower panels stack instead of overlapping.
func calcLayout(w, h int, rows ...int) Layout {
	l := Layout{Width: w, Height: h}
	l.Compact = w < compactThreshold
	inputH := inputMinHeight
	if len(rows) > 0 {
		inputH = rows[0]
	}
	if inputH < inputMinHeight {
		inputH = inputMinHeight
	}
	interactionH := 0
	if len(rows) > 1 && rows[1] > 0 {
		interactionH = rows[1]
	}

	// Vertical split: body | interaction? | input | status（顶栏已并入状态栏）。
	l.StatusY = h - statusBarHeight
	l.StatusH = statusBarHeight
	l.InputY = l.StatusY - inputH
	l.InputH = inputH
	l.InteractionY = l.InputY - interactionH
	l.InteractionH = interactionH

	bodyY := 0
	bodyH := l.InteractionY - bodyY
	if bodyH < 1 {
		bodyH = 1
	}

	// 无侧边栏：主面板始终全宽。
	l.MainX = 0
	l.MainY = bodyY
	l.MainW = w
	l.MainH = bodyH

	if l.MainW < 1 {
		l.MainW = 1
	}

	return l
}
