# Context Snapshot 与请求预算

当前实现：L2 `contextruntime`，ContextSnapshot `agentgo.context/v2`，Context `context:default/v11`，Replay `provider-replay:openai-compatible/v5`。重建验证状态见 [实施记录](l1-l2-rebuild.md)，旧实验记录在 [归档](../archived/context-snapshot-item-budget-before-rebuild.md)。

## 唯一编译路径

L3 明确给出工具、权限与调用选项，L2 装配角色、目标、输出要求、记忆、typed 邮件/上游输入与历史；有序材料进入 ContextCompiler。所有选项在此之前确定，不能在 Snapshot 封存后继续补入模型、tool choice、reasoning 或预算。

Snapshot 的 instruction_ref 标识本轮指令材料；encoded_request_digest 覆盖规范化语义请求，包括 options、消息、工具 strict/schema。它不是 HTTP 包的抓包摘要。L1 Request 的摘要另外覆盖封存对象与关联身份，避免 Snapshot 与 Request 的身份互相计算形成循环。L1 使用与 L2 尺寸检查同源的协议编码函数发送。

## 预算与历史

普通指令、任务、历史和工具片段继承原 v10 的有效上限；模型容量只调节总输入预算、completion reserve 与 RequiredExact 容器。历史裁剪不修改 Raw History；工具调用与结果按 call_id 原子关联。Observation 控制调用与业务重放隔离；阶段由 L3 选择，投影由 L2 执行。

图片/文件位于独立 user_media / input_media 分区，按显式 InputCapability 的字节和 token 预算计费，不扩大普通 Fragment 上限。二进制内容不裁剪、不伪装成摘要；实际协议编码后再次检查字节预算。未声明能力或预算不足时在 API 前拒绝。引用数据通过 L3 InputContentReader 读取，L1 不读取业务文件。

## 返回门

L1 只交付完整 Result 或明确失败。L2 检查 ProtocolReplay；RequiredExact 不可表示时阻止工具执行，Optional 可按策略从下一轮投影移除。完整响应先记录，工具由 L3 执行并记录结果，下一轮仅接收完整交换。流式展示不成为执行或记忆权威。

## 版本与存储

旧 Context/Replay policy、Prompt Build、Binding 与数组式历史均拒绝，不做转换。新 Context 使用 `.agentgo/state/context-snapshots-v2`，旧目录保留。历史使用 `agentgo.model-history/v1` 信封；Session v6 必须携带相同 model_history_contract。恢复会话仍不自动续跑。
