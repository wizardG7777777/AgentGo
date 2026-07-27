package tui

import (
	"testing"
	"time"
)

func TestPasteBurstState_SingleASCIIFlushesAsTyped(t *testing.T) {
	var state pasteBurstState
	now := time.Unix(100, 0)

	if got := state.onRunes([]rune("a"), now); got != "" {
		t.Fatalf("首个 ASCII 候选不应立即返回，得到 %q", got)
	}
	flush := state.flushIfDue(now.Add(pasteBurstCharInterval + pasteBurstTickSlack))
	if flush.kind != pasteBurstFlushTyped || flush.text != "a" {
		t.Fatalf("单字符应按普通输入刷出，得到 kind=%v text=%q", flush.kind, flush.text)
	}
}

func TestPasteBurstState_RapidASCIIBuffersNewlineAsPaste(t *testing.T) {
	var state pasteBurstState
	now := time.Unix(200, 0)

	if got := state.onRunes([]rune("abc"), now); got != "" {
		t.Fatalf("高速 ASCII 应进入缓冲，得到 immediate=%q", got)
	}
	if !state.appendNewlineIfActive(now.Add(time.Millisecond)) {
		t.Fatal("活跃 burst 中的 Enter 应作为换行缓冲")
	}
	state.onRunes([]rune("def"), now.Add(2*time.Millisecond))

	flush := state.flushIfDue(now.Add(pasteBurstActiveIdleTimeout + 3*time.Millisecond))
	if flush.kind != pasteBurstFlushPaste || flush.text != "abc\ndef" {
		t.Fatalf("应按一次完整粘贴刷出，得到 kind=%v text=%q", flush.kind, flush.text)
	}
}

func TestPasteBurstState_SingleCharacterFollowedByImmediateEnterStartsBurst(t *testing.T) {
	tests := []struct {
		name  string
		runes []rune
		want  string
	}{
		{name: "ASCII", runes: []rune("x"), want: "x\n"},
		{name: "非 ASCII", runes: []rune("中"), want: "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var state pasteBurstState
			now := time.Unix(250, 0)
			state.onRunes(tc.runes, now)
			if !state.appendNewlineIfActive(now.Add(time.Millisecond)) {
				t.Fatal("紧随字符的 Enter 应确认粘贴 burst")
			}
			flush := state.flushIfDue(now.Add(pasteBurstActiveIdleTimeout + 2*time.Millisecond))
			if flush.kind != pasteBurstFlushPaste || flush.text != tc.want {
				t.Fatalf("burst 内容错误：kind=%v text=%q want=%q", flush.kind, flush.text, tc.want)
			}
		})
	}
}

func TestPasteBurstState_WindowOutlivesBuffer(t *testing.T) {
	var state pasteBurstState
	now := time.Unix(300, 0)
	state.onRunes([]rune("chunk1"), now)
	flushAt := now.Add(pasteBurstActiveIdleTimeout + pasteBurstTickSlack)

	if flush := state.flushIfDue(flushAt); flush.kind != pasteBurstFlushPaste {
		t.Fatalf("第一块应先刷出，得到 kind=%v", flush.kind)
	}
	if !state.shouldSuppressEnter(now.Add(300 * time.Millisecond)) {
		t.Fatal("超过 active idle 但仍在保护窗口时 Enter 应继续作为换行")
	}
	if state.shouldSuppressEnter(now.Add(pasteBurstEnterSuppressWindow + time.Millisecond)) {
		t.Fatal("保护窗口到期后 Enter 应恢复普通提交")
	}
}

func TestPasteBurstState_ChunkGapOver100msDoesNotSplitSubmission(t *testing.T) {
	var state pasteBurstState
	now := time.Unix(400, 0)
	state.onRunes([]rune("first"), now)

	// 模拟 ConPTY 分块间隙已超过旧实现的 100ms，并让首块先刷入 textarea。
	flushAt := now.Add(pasteBurstActiveIdleTimeout + pasteBurstTickSlack)
	if flush := state.flushIfDue(flushAt); flush.text != "first" {
		t.Fatalf("首块刷出错误：%q", flush.text)
	}
	secondAt := now.Add(250 * time.Millisecond)
	if got := state.onRunes([]rune("second"), secondAt); got != "" {
		t.Fatalf("保护窗口内的后续块应重新进入缓冲，得到 %q", got)
	}
	if !state.appendNewlineIfActive(secondAt.Add(time.Millisecond)) {
		t.Fatal("后续块的 Enter 不得走提交路径")
	}
	if flush := state.flushIfDue(secondAt.Add(pasteBurstActiveIdleTimeout + 2*time.Millisecond)); flush.text != "second\n" {
		t.Fatalf("后续块应完整保留，得到 %q", flush.text)
	}
}

func TestPasteBurstState_IMETextIsImmediate(t *testing.T) {
	var state pasteBurstState
	now := time.Unix(500, 0)

	if got := state.onRunes([]rune("中文输入"), now); got != "中文输入" {
		t.Fatalf("单个 IME 事件应立即插入，得到 %q", got)
	}
	if state.hasTimedState() {
		t.Fatal("IME 词组不应留下定时状态")
	}
}

func TestPasteBurstState_OnlyOneTickAndOldTickExpires(t *testing.T) {
	var state pasteBurstState
	now := time.Unix(600, 0)
	state.onRunes([]rune("a"), now)

	_, seq, ok := state.armTimer(now)
	if !ok {
		t.Fatal("有 ASCII 候选时应创建 tick")
	}
	if _, _, duplicate := state.armTimer(now); duplicate {
		t.Fatal("已有 tick 在途时不得重复创建")
	}

	state.reset()
	if state.acceptTick(seq) {
		t.Fatal("reset 后晚到的旧 tick 必须失效")
	}
}

func TestPasteBurstState_BoundaryFlushesWithoutSubmissionSemantics(t *testing.T) {
	var state pasteBurstState
	now := time.Unix(700, 0)
	state.onRunes([]rune("rapid"), now)

	flush := state.flushBeforeBoundary()
	if flush.kind != pasteBurstFlushPaste || flush.text != "rapid" {
		t.Fatalf("边界应完整刷出 burst，得到 kind=%v text=%q", flush.kind, flush.text)
	}
	if state.hasTimedState() || !state.windowUntil.IsZero() {
		t.Fatal("边界后分类与保护窗口都应清空")
	}
}
