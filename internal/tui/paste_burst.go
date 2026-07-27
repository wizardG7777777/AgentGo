package tui

import (
	"strings"
	"time"
	"unicode"
)

// Windows Terminal / ConPTY 在无法透传 bracketed paste 时，会把剪贴板内容
// 退化为高速 KeyRunes + Enter 事件流。pasteBurstState 把这类事件先缓冲为
// 一次完整粘贴，避免其中的 Enter 走普通提交路径。
//
// 三个时间窗口分别承担不同职责：
//   - charInterval：首个 ASCII 字符的候选窗口；第二个高速字符到达即判为 burst；
//   - activeIdleTimeout：burst 停顿多久后把缓冲整体插入输入框；
//   - enterSuppressWindow：缓冲刷出后继续短暂把 Enter 解释为换行，容忍
//     ConPTY 分块注入时超过 activeIdleTimeout 的间隙。
//
// 这些值有意比 Codex CLI 的 Windows 启发式更宽松：本项目已经真实观察到
// 100ms 提交防抖被长粘贴分块停顿击穿。误判时的最坏结果是 Enter 插入换行、
// 用户需短暂停顿后再提交；不能反向冒险把不完整提示词提交给 Scheduler。
const (
	pasteBurstCharInterval        = 12 * time.Millisecond
	pasteBurstActiveIdleTimeout   = 180 * time.Millisecond
	pasteBurstEnterSuppressWindow = 500 * time.Millisecond
	pasteBurstTickSlack           = time.Millisecond
)

type pasteBurstFlushKind uint8

const (
	pasteBurstFlushNone pasteBurstFlushKind = iota
	pasteBurstFlushTyped
	pasteBurstFlushPaste
)

type pasteBurstFlush struct {
	kind pasteBurstFlushKind
	text string
}

type pendingPasteRune struct {
	r  rune
	at time.Time
}

// pasteBurstState 是纯输入分类状态，不直接修改 textarea。
// timerArmed/seq 只用于保证整个 burst 同时最多有一个 Bubble Tea tick。
type pasteBurstState struct {
	lastPlainAt     time.Time
	consecutiveFast int
	windowUntil     time.Time
	buffer          string
	active          bool
	pendingASCII    *pendingPasteRune

	timerArmed bool
	seq        uint64
}

// onRunes 消费一个未带 Alt/Paste 标记的 KeyRunes 事件。
// immediate 是应立即按普通输入插入的文本（主要是非 ASCII/IME，或已过期的
// ASCII 候选）；其余内容已被状态机暂存。
func (s *pasteBurstState) onRunes(runes []rune, now time.Time) (immediate string) {
	if len(runes) == 0 {
		return ""
	}

	// IME 常把一整个非 ASCII 词组作为单个 KeyRunes 事件提交。不能在同一
	// 事件内部把它误判为高速逐键粘贴；已有 burst 时则仍应完整并入缓冲。
	if len(runes) > 1 && containsNonASCII(runes) && !s.active {
		if s.pendingASCII != nil {
			immediate += string(s.pendingASCII.r)
			s.pendingASCII = nil
		}
		s.clearClassification()
		return immediate + string(runes)
	}

	for _, r := range runes {
		if r > unicode.MaxASCII {
			immediate += s.onNonASCII(r, now)
			continue
		}
		immediate += s.onASCII(r, now)
	}
	return immediate
}

func (s *pasteBurstState) onASCII(r rune, now time.Time) string {
	s.notePlain(now)
	if s.active {
		s.buffer += string(r)
		s.extendWindow(now)
		return ""
	}
	if s.inEnterSuppressWindow(now) {
		s.active = true
		s.buffer += string(r)
		s.extendWindow(now)
		return ""
	}
	if s.pendingASCII != nil {
		pending := s.pendingASCII
		if now.Sub(pending.at) <= pasteBurstCharInterval {
			s.active = true
			s.buffer += string(pending.r)
			s.buffer += string(r)
			s.pendingASCII = nil
			s.extendWindow(now)
			return ""
		}
		s.pendingASCII = &pendingPasteRune{r: r, at: now}
		return string(pending.r)
	}
	s.pendingASCII = &pendingPasteRune{r: r, at: now}
	return ""
}

func (s *pasteBurstState) onNonASCII(r rune, now time.Time) string {
	s.notePlain(now)
	if s.active {
		s.buffer += string(r)
		s.extendWindow(now)
		return ""
	}
	// 非 ASCII/IME 不持有首字符，避免中文输入出现“漏字”观感；连续三个
	// 极高速事件才从当前字符开始缓冲，前缀已经按原顺序进入 textarea。
	if s.consecutiveFast >= 3 {
		s.active = true
		s.buffer += string(r)
		s.extendWindow(now)
		return ""
	}
	return string(r)
}

func (s *pasteBurstState) notePlain(now time.Time) {
	if !s.lastPlainAt.IsZero() && now.Sub(s.lastPlainAt) <= pasteBurstCharInterval {
		s.consecutiveFast++
	} else {
		s.consecutiveFast = 1
	}
	s.lastPlainAt = now
}

func (s *pasteBurstState) appendNewlineIfActive(now time.Time) bool {
	return s.appendControlIfBurst("\n", now)
}

func (s *pasteBurstState) appendTabIfActive(now time.Time) bool {
	return s.appendControlIfBurst("\t", now)
}

func (s *pasteBurstState) appendControlIfBurst(control string, now time.Time) bool {
	if !s.active && s.buffer == "" && s.pendingASCII != nil &&
		now.Sub(s.pendingASCII.at) <= pasteBurstCharInterval {
		s.active = true
		s.buffer = string(s.pendingASCII.r)
		s.pendingASCII = nil
	}
	// ConPTY 粘贴的首行可能只有一个非 ASCII 字符。该字符已直接写入
	// textarea，但紧随其后的 Enter/Tab 仍可凭极短间隔确认这是 burst。
	if !s.active && s.buffer == "" && !s.lastPlainAt.IsZero() &&
		now.Sub(s.lastPlainAt) <= pasteBurstCharInterval {
		s.active = true
	}
	if !s.active && s.buffer == "" {
		return false
	}
	s.active = true
	s.buffer += control
	s.lastPlainAt = now
	s.extendWindow(now)
	return true
}

func (s *pasteBurstState) shouldSuppressEnter(now time.Time) bool {
	return s.active || s.buffer != "" || s.inEnterSuppressWindow(now)
}

func (s *pasteBurstState) inEnterSuppressWindow(now time.Time) bool {
	return !s.windowUntil.IsZero() && !now.After(s.windowUntil)
}

func (s *pasteBurstState) extendWindow(now time.Time) {
	s.windowUntil = now.Add(pasteBurstEnterSuppressWindow)
}

// flushIfDue 由 tick 或下一按键调用。ASCII 候选按普通输入刷出；active
// buffer 按显式粘贴刷出，并保留 Enter 抑制窗口。
func (s *pasteBurstState) flushIfDue(now time.Time) pasteBurstFlush {
	if s.active || s.buffer != "" {
		if s.lastPlainAt.IsZero() || now.Sub(s.lastPlainAt) <= pasteBurstActiveIdleTimeout {
			return pasteBurstFlush{}
		}
		out := s.buffer
		s.buffer = ""
		s.active = false
		s.pendingASCII = nil
		s.timerArmed = false
		s.seq++
		return pasteBurstFlush{kind: pasteBurstFlushPaste, text: out}
	}
	if s.pendingASCII != nil && now.Sub(s.pendingASCII.at) > pasteBurstCharInterval {
		out := string(s.pendingASCII.r)
		s.pendingASCII = nil
		s.timerArmed = false
		s.seq++
		return pasteBurstFlush{kind: pasteBurstFlushTyped, text: out}
	}
	return pasteBurstFlush{}
}

// flushBeforeBoundary 在方向键、Ctrl/Alt、焦点切换等非文本输入前清空暂存，
// 防止状态泄漏到下一段输入。buffer 走 paste，单个候选走 typed。
func (s *pasteBurstState) flushBeforeBoundary() pasteBurstFlush {
	var out pasteBurstFlush
	switch {
	case s.buffer != "":
		out = pasteBurstFlush{kind: pasteBurstFlushPaste, text: s.buffer}
		if s.pendingASCII != nil {
			out.text += string(s.pendingASCII.r)
		}
	case s.pendingASCII != nil:
		out = pasteBurstFlush{kind: pasteBurstFlushTyped, text: string(s.pendingASCII.r)}
	}
	s.reset()
	return out
}

func (s *pasteBurstState) clearAfterExplicitPaste() {
	s.reset()
}

func (s *pasteBurstState) clearClassification() {
	s.lastPlainAt = time.Time{}
	s.consecutiveFast = 0
	s.windowUntil = time.Time{}
	s.active = false
	s.buffer = ""
}

func (s *pasteBurstState) reset() {
	s.clearClassification()
	s.pendingASCII = nil
	s.timerArmed = false
	s.seq++
}

func (s *pasteBurstState) hasTimedState() bool {
	return s.active || s.buffer != "" || s.pendingASCII != nil
}

// armTimer 返回下一次状态检查所需延迟；已有 tick 在途时不重复创建，
// 长粘贴因此不会为每个字符生成一个 goroutine/timer。
func (s *pasteBurstState) armTimer(now time.Time) (delay time.Duration, seq uint64, ok bool) {
	if s.timerArmed || !s.hasTimedState() {
		return 0, 0, false
	}
	var deadline time.Time
	if s.active || s.buffer != "" {
		deadline = s.lastPlainAt.Add(pasteBurstActiveIdleTimeout)
	} else {
		deadline = s.pendingASCII.at.Add(pasteBurstCharInterval)
	}
	delay = deadline.Sub(now) + pasteBurstTickSlack
	if delay < pasteBurstTickSlack {
		delay = pasteBurstTickSlack
	}
	s.timerArmed = true
	return delay, s.seq, true
}

func (s *pasteBurstState) acceptTick(seq uint64) bool {
	if seq != s.seq {
		return false
	}
	s.timerArmed = false
	return true
}

func containsNonASCII(runes []rune) bool {
	return strings.IndexFunc(string(runes), func(r rune) bool { return r > unicode.MaxASCII }) >= 0
}
