# L1 调用失败与 L4 恢复

当前边界：L1 Model Invocation Engineering + L4 Loop Engineering。实现分别位于 `internal/llm` 与 Loop；不存在独立 Invocation 基础层。旧验证事实见 [归档](../archived/invocation-failure-before-rebuild.md)，当前验证见 [实施记录](l1-l2-rebuild.md)。

## 调用与失败

唯一 L1 入口为 Invoker.Invoke(ctx, Request, EventSink)。Request 已由 L2 封存并通过必要的 Snapshot 持久化。HTTP 自动重试关闭；失败只返回一次，是否重试、重建上下文、创建 Attempt 或停止由 L4 决定。

HTTP 错误、取消/deadline、非 SSE 响应、协议不完整、字段超限与工具参数非法分别归类。HTTP 402/额度耗尽区别于 429 限速；前者不得消耗更多模型调用尝试恢复。协议和模型不按供应商名称特判。

Responses 必须收到完整输出项及 completed；Chat Completions 必须具有合法 finish_reason 与 [DONE]。EOF 不代表成功。部分工具参数不得执行；观察增量不授予行动权限。完整结果通过 L2 重放与记录门后，L3 仍执行 ToolRouter、required-action、首动作及 finalizing gate。

## 与 L4 的配合

L4 显式向单步执行器传递动作预算。L2 将其与 Context 输出预算求交，L1 只执行最终限额。取消/deadline 不确定的调用维持 unknown/partial 记账，不能伪造可靠 usage。既有 finalizing 优先级、Effect prepared/unknown 禁止重跑、fan-out 尾部 skipped receipt 规则不变。

## 观测

客户端时间事实在 L1 采集；Trace 负责持久化和展示。该横切观测面不进入 Prompt、Context digest 或控制策略。缺失字段保持缺席；不记录凭据、endpoint、IP 或正文。Provider 内部排队/推理时间不可由客户端时序推断。
