package tui

// calcLayout computes panel dimensions from terminal size and view state.
// Borrows crush's responsive breakpoint pattern: sidebar hidden below compactThreshold.
func calcLayout(w, h int, view ViewState) Layout {
	l := Layout{Width: w, Height: h}
	l.Compact = w < compactThreshold

	// Vertical split: header(1) | body | input(3) | status(1)
	l.HeaderY = 0
	l.HeaderH = headerHeight
	l.StatusY = h - statusBarHeight
	l.StatusH = statusBarHeight
	l.InputY = l.StatusY - inputHeight
	l.InputH = inputHeight

	bodyY := l.HeaderY + l.HeaderH
	bodyH := l.InputY - bodyY
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

	// Approval overlays the input area
	l.ApprovalY = l.InputY
	l.ApprovalH = l.InputH

	return l
}
