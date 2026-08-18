package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// bracketed paste：bubbletea 把整段粘贴作为一个 KeyRunes{Paste:true}
// 事件投递，换行是 rune 而非 Enter 键——必须整体落入输入框，不得
// 触发逐行提交。
func TestHandleKey_PasteMsgInsertsMultilineWithoutSubmit(t *testing.T) {
	m := newAppModel(testDeps())

	pasted := "第一行内容\n第二行内容\n第三行内容"
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})
	updated := result.(AppModel)

	if got := updated.input.Value(); got != pasted {
		t.Fatalf("粘贴文本应整体保留多行，得到 %q", got)
	}
	// 没有任何提交：输入历史为空（提交路径会 push 历史）。
	if len(updated.history.entries) != 0 {
		t.Fatalf("粘贴不得触发提交，历史应为空，实际 %d 条", len(updated.history.entries))
	}
}

// Windows 剪贴板文本常带 CRLF；textarea 只按 '\n' 分行，'\r' 必须
// 在入口被规范化掉，否则行尾残留不可见字符。
func TestHandleKey_PasteMsgNormalizesCRLF(t *testing.T) {
	m := newAppModel(testDeps())

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\r\nb\rc"), Paste: true})
	updated := result.(AppModel)

	if got := updated.input.Value(); got != "a\nb\nc" {
		t.Fatalf("CRLF/CR 应规范化为 LF，得到 %q", got)
	}
}

// 焦点在主面板时粘贴不能静默丢弃：粘贴的意图永远是输入，
// 应切回输入框焦点并写入文本。
func TestHandleKey_PasteMsgRetargetsFocusToInput(t *testing.T) {
	m := newAppModel(testDeps())
	m.setFocus(FocusMain)
	m.agents = []AgentInfo{{ID: "a1"}}

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello\nworld"), Paste: true})
	updated := result.(AppModel)

	if updated.focus != FocusInput {
		t.Fatalf("粘贴应把焦点切回输入框，实际 %v", updated.focus)
	}
	if got := updated.input.Value(); got != "hello\nworld" {
		t.Fatalf("焦点在主面板时粘贴不得丢弃，得到 %q", got)
	}
}

// 粘贴内容恰好像按键名（例如单行 "enter"）也不能被当成按键分发。
func TestHandleKey_PasteMsgNeverMatchesKeyBindings(t *testing.T) {
	m := newAppModel(testDeps())

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("enter"), Paste: true})
	updated := result.(AppModel)

	if got := updated.input.Value(); got != "enter" {
		t.Fatalf("粘贴文本 enter 应原样进入输入框，得到 %q", got)
	}
	if len(updated.history.entries) != 0 {
		t.Fatal("粘贴文本不得触发提交路径")
	}
}

// 空粘贴（只有换行/回车）不应污染输入框已有内容。
func TestInsertPastedText_EmptyAfterNormalize(t *testing.T) {
	m := newAppModel(testDeps())
	m.input.SetValue("已有内容")

	result, _ := m.insertPastedText("")
	updated := result.(AppModel)
	if got := updated.input.Value(); got != "已有内容" {
		t.Fatalf("空粘贴不得改动输入框，得到 %q", got)
	}
}

// Ctrl+V 不退出应用、不经过提交路径：textarea 内置 Paste 绑定返回
// 剪贴板读取 cmd，读回的内容以 pasteMsg 整体插入输入框。
func TestHandleKey_CtrlVReturnsClipboardCmd(t *testing.T) {
	m := newAppModel(testDeps())

	result, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlV})
	updated := result.(AppModel)

	if cmd == nil {
		t.Fatal("Ctrl+V 应返回剪贴板读取命令")
	}
	if len(updated.history.entries) != 0 {
		t.Fatal("Ctrl+V 不得触发提交路径")
	}
}

// 焦点在主面板时 Ctrl+V 同样生效：粘贴的意图永远是输入，应切回
// 输入框焦点再读剪贴板。
func TestHandleKey_CtrlVRetargetsFocusToInput(t *testing.T) {
	m := newAppModel(testDeps())
	m.setFocus(FocusMain)
	m.agents = []AgentInfo{{ID: "a1"}}

	result, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlV})
	updated := result.(AppModel)

	if updated.focus != FocusInput {
		t.Fatalf("Ctrl+V 应把焦点切回输入框，实际 %v", updated.focus)
	}
	if cmd == nil {
		t.Fatal("Ctrl+V 应返回剪贴板读取命令")
	}
}

// 粘贴后的多行文本按 Enter 应作为一条完整请求提交（换行保留在
// 提交的文本里，而不是逐行拆开）。
func TestPasteThenEnterSubmitsWholeMultilineText(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第一行\n第二行"), Paste: true})
	updated := result.(AppModel)
	result, _ = updated.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	updated = result.(AppModel)
	_ = updated

	sent := fakeOf(deps).sentTexts
	if len(sent) != 1 {
		t.Fatalf("Enter 应提交且仅提交一次，实际 %d 次", len(sent))
	}
	if !strings.Contains(sent[0], "第一行") || !strings.Contains(sent[0], "第二行") {
		t.Fatalf("提交文本应包含完整多行内容，得到 %q", sent[0])
	}
}

// ── Windows ConPTY 粘贴突发状态机 ──

// 模拟终端逐键注入的粘贴流：快速字符和真实 Enter 必须先重组为完整
// 多行文本，不能在任一换行处向 Scheduler 提交残片。
func TestPasteBurst_MergesKeystrokeStreamWithoutPartialSubmit(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("line1")})
	m = result.(AppModel)
	// 第一块停顿超过旧实现的 100ms，状态机只把它刷入 textarea，
	// 不把它提交给 Scheduler；500ms 保护窗口仍继续等待下一块。
	firstSeq := m.pasteBurst.seq
	firstFlushAt := m.pasteBurst.lastPlainAt.Add(pasteBurstActiveIdleTimeout + pasteBurstTickSlack)
	result, _ = m.Update(pasteBurstTickMsg{seq: firstSeq, at: firstFlushAt})
	m = result.(AppModel)
	if got := m.input.Value(); got != "line1" {
		t.Fatalf("首块应先完整刷入输入框，得到 %q", got)
	}
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第二行")})
	m = result.(AppModel)

	if sent := fakeOf(deps).sentTexts; len(sent) != 0 {
		t.Fatalf("粘贴仍在注入时不得提交残片，实际 %v", sent)
	}

	seq := m.pasteBurst.seq
	flushAt := m.pasteBurst.lastPlainAt.Add(pasteBurstActiveIdleTimeout + pasteBurstTickSlack)
	result, _ = m.Update(pasteBurstTickMsg{seq: seq, at: flushAt})
	m = result.(AppModel)
	if got := m.input.Value(); got != "line1\n第二行" {
		t.Fatalf("状态机应完整重组多行文本，得到 %q", got)
	}
	if sent := fakeOf(deps).sentTexts; len(sent) != 0 {
		t.Fatalf("状态机刷入输入框不得自动提交，实际 %v", sent)
	}

	// 用户检查完整文本后再按 Enter；测试显式越过保护窗口，不等待墙钟。
	m.pasteBurst.windowUntil = time.Now().Add(-time.Second)
	m.pasteBurst.lastPlainAt = time.Now().Add(-time.Second)
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	sent := fakeOf(deps).sentTexts
	if len(sent) != 1 {
		t.Fatalf("完整文本应仅提交一次，实际 %d 次", len(sent))
	}
	if sent[0] != "line1\n第二行" {
		t.Fatalf("提交文本应保留换行，得到 %q", sent[0])
	}
}

// 真人单次 Enter 保持原有即时提交语义，不再引入固定 100ms 延迟。
func TestPasteBurst_SingleEnterSubmitsImmediately(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	m.input.SetValue("hello")
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	sent := fakeOf(deps).sentTexts
	if len(sent) != 1 || sent[0] != "hello" {
		t.Fatalf("普通 Enter 应立即提交原样文本，得到 %v", sent)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("提交后输入框应清空，得到 %q", got)
	}
}

// 状态被 Esc 边界清空后，晚到的旧 tick 不得重新刷入或提交内容。
func TestPasteBurst_EscInvalidatesOldTick(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("burst")})
	m = result.(AppModel)
	oldSeq := m.pasteBurst.seq
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(AppModel)
	before := m.input.Value()
	result, _ = m.Update(pasteBurstTickMsg{seq: oldSeq, at: time.Now().Add(time.Second)})
	m = result.(AppModel)
	if sent := fakeOf(deps).sentTexts; len(sent) != 0 {
		t.Fatalf("Esc 后旧 tick 不得提交，实际 %v", sent)
	}
	if got := m.input.Value(); got != before {
		t.Fatalf("旧 tick 不得改动输入框，原为 %q，得到 %q", before, got)
	}
}
