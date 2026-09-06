# AgentGo 当前运行契约与冻结边界

2026-09-07 L1/L2 重建修订。旧基线及验证记录见 [归档](../archived/contract-freeze-before-l1-l2-rebuild.md)，不能作为当前 L1/L2 实现指南。

| 责任域 | 当前契约 | 规则 |
|---|---|---|
| L1 | agentgo.model-request/v1、agentgo.model-result/v1 | 完整请求、SSE-only、唯一调用入口、无自动 HTTP 重试 |
| L2 | agentgo.context/v2、context:default/v11、provider-replay:openai-compatible/v5 | 全量装配与封存、旧版本明确拒绝 |
| 历史/输出 | agentgo.model-history/v1、agentgo.model-output/v1、Session v6 | 不转换旧模型历史，不删除旧磁盘数据；不自动续跑 |
| L3 | ExecutionLease v2、RunContract v2、RecoveryDelta v5、Effect/Store/workspace | 原有权限、状态与旧业务版本定义保持不变 |
| L4 | code-change/v12、investigation/v6 及冻结历史 Progress | finalizing、预算、收敛、重试与停止仍由 Loop 决定 |
| L5 | Graph v4、simple-task/v4、Delivery v1 | 编排、验收、候选交付和 promotion 不由模型 API/UI 决定 |
| Trace | agentgo.llm-invocation-timing/v1 | 横切观测，旧 Trace 按历史事实读取，不重新解释 |

## 重建例外与长期纪律

本次明确退役 L1/L2 的旧请求、Prompt Build、Binding、builder、非 SSE、配置 stream 与历史转换入口。L3–L5 自身的历史业务契约未被取消；若其引用旧 L2 版本，在 L2 接入边界拒绝。禁止为旧格式增加 wrapper、alias 或隐藏执行分支。

后续改变请求、预算或状态语义仍须发布明确版本。Prompt 不写经验阈值；机械上限进入 policy/schema。接口依赖注入，无新的全局状态。配置必须显式声明 llm.request_contract=agentgo.model-request/v1；模型输入能力默认仅文本，图片/文件须声明能力与预算。

完整业务不变量与程序文件索引见 [AGENTS](../../AGENTS.md) 和 [五层规范](five-layer-engineering-architecture.md)。当前完成状态与验证缺口见 [实施记录](l1-l2-rebuild.md)，不得以旧版本的测试结果宣称新链路已完成。
