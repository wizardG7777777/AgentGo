package tui

import (
	"strings"
	"testing"

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

// 焦点在侧栏/主面板时粘贴不能静默丢弃：粘贴的意图永远是输入，
// 应切回输入框焦点并写入文本。
func TestHandleKey_PasteMsgRetargetsFocusToInput(t *testing.T) {
	m := newAppModel(testDeps())
	m.setFocus(FocusSidebar)
	m.agents = []AgentInfo{{ID: "a1"}}

	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello\nworld"), Paste: true})
	updated := result.(AppModel)

	if updated.focus != FocusInput {
		t.Fatalf("粘贴应把焦点切回输入框，实际 %v", updated.focus)
	}
	if got := updated.input.Value(); got != "hello\nworld" {
		t.Fatalf("焦点在侧栏时粘贴不得丢弃，得到 %q", got)
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

// 焦点在侧栏时 Ctrl+V 同样生效：粘贴的意图永远是输入，应切回
// 输入框焦点再读剪贴板。
func TestHandleKey_CtrlVRetargetsFocusToInput(t *testing.T) {
	m := newAppModel(testDeps())
	m.setFocus(FocusSidebar)
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
	updated = firePendingSubmit(t, result.(AppModel))
	_ = updated

	sent := fakeOf(deps).sentTexts
	if len(sent) != 1 {
		t.Fatalf("Enter 应提交且仅提交一次，实际 %d 次", len(sent))
	}
	if !strings.Contains(sent[0], "第一行") || !strings.Contains(sent[0], "第二行") {
		t.Fatalf("提交文本应包含完整多行内容，得到 %q", sent[0])
	}
}

// ── Enter 提交防抖（Windows Terminal 逐键注入粘贴的兜底）──

// 模拟终端逐键注入的粘贴爆发流：line1 <Enter> line2 <Enter> 在防抖
// 窗口内连续到达，必须合并为一次提交，文本保留换行。
func TestEnterDebounce_MergesKeystrokePasteBurst(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	m.input.SetValue("第一行")
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // 爆发流第一个换行
	m = result.(AppModel)
	// 窗口内第二行字符到达（普通按键路径，直接落入输入框）
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第二行")})
	m = result.(AppModel)
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter}) // 爆发流第二个换行
	m = result.(AppModel)

	// 旧的 tick 到期（seq 已刷新）：不得提交
	result, _ = m.Update(submitTimeoutMsg{seq: m.submitSeq - 1})
	m = result.(AppModel)
	if sent := fakeOf(deps).sentTexts; len(sent) != 0 {
		t.Fatalf("过期 tick 不得触发提交，实际 %v", sent)
	}

	// 最新 tick 到期：整段合并为一次提交
	m = firePendingSubmit(t, m)
	sent := fakeOf(deps).sentTexts
	if len(sent) != 1 {
		t.Fatalf("粘贴爆发流应合并为一次提交，实际 %d 次", len(sent))
	}
	if sent[0] != "第一行\n第二行" {
		t.Fatalf("提交文本应保留换行，得到 %q", sent[0])
	}
}

// 真人单次 Enter：防抖到期后按原语义提交（末尾换行被 TrimSpace 去掉）。
func TestEnterDebounce_SingleEnterSubmitsAfterTick(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	m.input.SetValue("hello")
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	if sent := fakeOf(deps).sentTexts; len(sent) != 0 {
		t.Fatal("Enter 后 tick 到期前不得提交")
	}
	m = firePendingSubmit(t, m)
	sent := fakeOf(deps).sentTexts
	if len(sent) != 1 || sent[0] != "hello" {
		t.Fatalf("防抖到期应提交原样文本，得到 %v", sent)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("提交后输入框应清空，得到 %q", got)
	}
}

// Enter 后立即 Esc（取消意图）：pending 提交必须作废，tick 到期不再
// 把输入框内容发出去。
func TestEnterDebounce_EscInvalidatesPendingSubmit(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	m.input.SetValue("不想发了")
	result, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = result.(AppModel)
	result, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(AppModel)
	// 用 Enter 时的旧 seq 投递 tick：代数已被 Esc 刷新，不得提交
	result, _ = m.Update(submitTimeoutMsg{seq: m.submitSeq - 1})
	m = result.(AppModel)
	if sent := fakeOf(deps).sentTexts; len(sent) != 0 {
		t.Fatalf("Esc 后 pending 提交应作废，实际提交 %v", sent)
	}
}
