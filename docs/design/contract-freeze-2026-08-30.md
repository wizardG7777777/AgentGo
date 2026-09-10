# 当前契约冻结基线（2026-09-11）

文件路径保持稳定。旧表仅为历史记录，不能指导新执行。agentTask 实施来源见 [计划](dataflow-graph-simplification-proposal.md)。

| 域 | 当前契约 | 规则 |
|---|---|---|
| L1 | model-request/v1、model-result/v1 | 完整请求；两协议均 SSE-only |
| L2 | context/v2、context:default/v11、provider-replay:openai-compatible/v5 | L2 装配封存，原始历史不改写 |
| 模型历史/输出 | model-history/v1、model-output/v1 | UI 仅消费输出 |
| Graph | graph/v6 | 唯一 agentTask；inputs 数据依赖；追加实例迭代 |
| 节点结果/图完成 | agent-task-result/v1、graph-completion/v1 | 不可变结果与图级完成事务 |
| TaskOutcome / TerminalIntent | task-outcome/v4、terminal-intent/v2 | 新运行统一严格版本，不保留旧控制结果转换 |
| Candidate / Delivery | candidate/v1、delivery/v2 | 候选版本与图级提交分离 |
| Session / Lease | Session 8、execution-lease/v4 | 旧版本不恢复执行 |
| Run / Progress / Shell | run-contract/v3、progress-contract/v2、shell-execution/v2 | 通用事实、显式预算、Shell 实际执行；无默认观察/恢复控制命令 |
| SWE | swe-result/v5、swe-judge/v2、pytest-phase-report/v2、swe-test-execution/v1 | Python 判题与输入身份独立保存 |

schema 以 agentgo. 开头；策略 ref 保持完整原名。

新图在 `.agentgo/state/graphs-v6` 保存版本化定义、运行、输入、回执与完成意图；不再有独立旧 authoring 控制链。终态、Loop、TaskMemory、Delivery 分别使用 task-outcomes-v3、loop-facts-v3、taskmem-v3、deliveries-v3。候选完整目录与元数据位于 `.agentgo/candidates-v1`。L1/L2、run-usage 未变目录保持原契约。

文件必须显式声明 llm.request_contract=agentgo.model-request/v1、graph.request_contract=agentgo.graph/v6。旧 node kind、root/next/when、Proposal Acceptance 参数和旧历史拒绝执行；不增加别名、迁移器或 fallback，不删除磁盘历史。

图更新仅影响未激活任务；已经冻结的输入与结果不可覆写。图可以只有调查任务，不预要求成功路径或验收节点。所有就绪输入有效且执行面可用时才派发；最终 success 必须有真实选定结果、全部必要结算及交付回执。主根冲突或 Effect unknown 不默认重放。

进入历史 Session 只恢复可接受版本的上下文，不自动续跑。Trace 的历史展示与执行解码分离。真实 SWE automatic-options 已完整通过；这不代表其它七题或所有模型已通过。
