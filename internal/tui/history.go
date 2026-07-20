package tui

// inputHistoryCap 是输入历史环形缓冲容量（满时丢最旧一条）。
const inputHistoryCap = 100

// inputHistory 是输入框的提交历史：Enter 提交的每一行（含斜杠命令）
// 入历史，连续重复行去重，容量 100。仅内存，不持久化。
//
// 导航语义模仿 Claude Code / REPL：cursor 是浏览游标，
// cursor == len(entries) 表示未在浏览（草稿态）；浏览中按 ↓ 越过最新
// 一条时恢复进入浏览前暂存的草稿（draft stash）。提交后游标重置。
type inputHistory struct {
	entries []string // 最旧在前
	cursor  int      // 浏览游标；== len(entries) 表示未在浏览
	draft   string   // 进入浏览前暂存的草稿
}

// push 记录一条提交行并重置浏览游标。空行不入历史；与上一条相同的
// 连续重复行去重（不重复入栈，只重置游标）。
func (h *inputHistory) push(line string) {
	if line == "" {
		return
	}
	if n := len(h.entries); n == 0 || h.entries[n-1] != line {
		h.entries = append(h.entries, line)
		if len(h.entries) > inputHistoryCap {
			h.entries = h.entries[len(h.entries)-inputHistoryCap:]
		}
	}
	h.cursor = len(h.entries)
	h.draft = ""
}

// prev 取更早一条历史。首次进入浏览时用 currentInput 暂存草稿；
// 已在最旧一条或无历史时返回 ok=false（调用方透传按键）。
func (h *inputHistory) prev(currentInput string) (string, bool) {
	if len(h.entries) == 0 || h.cursor <= 0 {
		return "", false
	}
	if h.cursor == len(h.entries) {
		h.draft = currentInput
	}
	h.cursor--
	return h.entries[h.cursor], true
}

// next 取更晚一条历史；越过最新一条时恢复草稿并退出浏览态。
// 未在浏览时返回 ok=false（调用方透传按键）。
func (h *inputHistory) next() (string, bool) {
	if h.cursor >= len(h.entries) {
		return "", false
	}
	h.cursor++
	if h.cursor == len(h.entries) {
		return h.draft, true
	}
	return h.entries[h.cursor], true
}
