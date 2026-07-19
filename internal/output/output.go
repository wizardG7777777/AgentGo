// Package output 定义代理用户可见输出通道（outputCh）的类型化事件。
//
// 输出通道不再传输裸字符串：每条消息在产生处就完成分类（普通文本 / 任务最终结果），
// 消费方（TUI、结果快照记录）按 Kind 分发，不再做 "=== 任务完成 ===" 之类的
// 魔法字符串子串匹配——措辞调整不会再静默击穿分类逻辑。
package output

// Kind 标记一条输出事件的类别。
type Kind int

const (
	// KindText 是普通代理输出（进度汇报、提示等），TUI 渲染为 MsgAgent。
	KindText Kind = iota
	// KindResult 是任务最终结果块，TUI 渲染为 MsgResult（result 卡片），
	// 并被记录到 ResultSnapshot 供重启恢复。
	KindResult
)

// Event 是输出通道上的一条消息。Text 保留完整渲染文本（含 "=== 任务完成 ==="
// 等展示标记）——标记只是呈现层内容，不再承担分类职责。
type Event struct {
	Kind    Kind
	AgentID string
	Text    string
}
