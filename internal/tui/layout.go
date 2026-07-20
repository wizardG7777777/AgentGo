package tui

// calcLayout computes panel dimensions from terminal size and view state.
// rows[0] is the input area height; rows[1], when present, is the Interaction
// panel height. The two lower panels stack instead of overlapping.
func calcLayout(w, h int, view ViewState, rows ...int) Layout {
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

	// Vertical split: header | body | interaction? | input | status.
	l.HeaderY = 0
	l.HeaderH = headerHeight
	l.StatusY = h - statusBarHeight
	l.StatusH = statusBarHeight
	l.InputY = l.StatusY - inputH
	l.InputH = inputH
	l.InteractionY = l.InputY - interactionH
	l.InteractionH = interactionH

	bodyY := l.HeaderY + l.HeaderH
	bodyH := l.InteractionY - bodyY
	if bodyH < 1 {
		bodyH = 1
	}

	if l.Compact {
		// No sidebar in compact mode
		l.SidebarW = 0
		l.MainX = 0
		l.MainY = bodyY
		l.MainW = w
		l.MainH = bodyH
	} else {
		// Sidebar on the left
		l.SidebarW = sidebarMinWidth
		if w > 140 {
			l.SidebarW = sidebarMaxWidth
		}
		l.SidebarX = 0
		l.SidebarY = bodyY
		l.SidebarH = bodyH

		l.MainX = l.SidebarW + 1 // +1 for border
		l.MainY = bodyY
		l.MainW = w - l.MainX
		l.MainH = bodyH
	}

	if l.MainW < 1 {
		l.MainW = 1
	}

	return l
}
