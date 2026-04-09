package mailbox

// MailboxHookRunner 是 mailbox.Registry 在 Send 路径上调用的最小 hook 接口。
//
// 之所以定义在 mailbox 包内部（而不是直接 import internal/hook 的
// MailboxHookRegistry），是因为 internal/hook 已经 import internal/mailbox
// 来取 Message 类型 —— 反向 import 会形成循环。这个接口让 mailbox 包不
// 依赖 hook 包，hook 包通过 AsMailboxRunner 适配器满足本接口。
//
// 调用语义：
//   - BeforeSend 在 Send 入口被调用一次。abort=true 时整条消息发送被拒绝
//     （Registry.Send 返回 error）。
//   - BeforeDeliver 在每个具体收件人的 TrySend 之前被调用。abort=true 时
//     该收件人被跳过；广播路径下其他收件人不受影响；单点路径下整条消息
//     被拒绝（Registry.Send 返回 error）。
//
// nil runner 等价于"未挂接 hook 系统"，所有方法被 Registry 直接跳过，
// 永远不会触发 nil dereference。这与既有 mailbox 测试用例向后兼容 ——
// 既有测试都没有 attach runner，跑起来等同于 hook 全部禁用。
type MailboxHookRunner interface {
	// BeforeSend 在 Registry.Send 入口被调用。
	// 返回值：(是否拒绝, 拒绝原因, 触发拒绝的 hook 名称)
	BeforeSend(msg Message) (abort bool, reason string, hookName string)

	// BeforeDeliver 在每个具体收件人的 TrySend 之前被调用。
	// deliverTo 是当前正在投递的具体 agentID（广播展开时一对一调用）。
	// 返回值：(是否拒绝, 拒绝原因, 触发拒绝的 hook 名称)
	BeforeDeliver(msg Message, deliverTo string) (abort bool, reason string, hookName string)
}
