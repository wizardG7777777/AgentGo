# 当前契约冻结基线（2026-09-09）

文件路径保持稳定供现有入口引用。此前冻结表已存入 [历史基线](../archived/contract-freeze-before-four-categories.md)。四类工具重建改变了新运行的工具、配置和进展语义；历史描述不能用于恢复被删除的执行路径。

| 责任域 | 当前契约 | 新运行约束 |
|---|---|---|
| L1 | model-request/v1、model-result/v1 | 完整请求，显式协议，SSE-only，不自动重试或补上下文 |
| L2 | context/v2、context:default/v11、provider-replay:openai-compatible/v5 | 完整装配、不可变封存、Raw History 保留；无强制观察历史模式 |
| 模型历史/输出 | model-history/v1、model-output/v1 | 完整响应与逐次工具事实分别保存；不依赖 UI |
| Session / Lease | Session 7、execution-lease/v3 | 拒绝旧会话/旧执行租约；角色与工具视图冻结，无 Observation 模型 |
| Run / Progress | run-contract/v3、progress-contract/v2 | 未设 deadline 时不注入阶段预留；进展记录不按默认轮数强制停止或恢复 |
| 当前 Progress | code-change/v13、investigation/v8、verification/v4、coordination/v3、final-report/v2 | 事实与角色分类，不要求 record/decision 工具 |
| Graph | graph/v5 | apply_graph_change 初建/更新，内部校验提交；在途 preserve；单 mutable producer |
| 产物与交付 | fulfillment/v2、delivery/v1 | 通用工作区/产物/Effect 权威，无 CheckContract/CheckRef |
| Shell 事实 | shell-execution/v2 | 启动状态、可选退出码/作用域、失败/取消/超时与输出 |
| Python 评测 | swe-result/v4、swe-judge/v2、pytest-phase-report/v2、swe-test-execution/v1 | 外部测试身份与判题，不回填 AgentGo 检查 gate |

没有前缀的 schema 名称在代码中均以 agentgo. 开头；policy ref 按表中原样使用。当前 TaskOutcome 按职责原生使用 v1（非 Graph 入口/final-report）或 v2/v3（图节点），不因存储目录换代而统一重命名。

## 存储与拒绝边界

新数据目录在 .agentgo/state 下使用 graphs-v5、graph-authoring-v2、loop-facts-v2、run-usage-v2、task-outcomes-v2、taskmem-v2、deliveries-v2、context-snapshots-v2。目录代次和内部 JSON schema 分别验证。旧目录不删除、不自动迁移，不因旧目录缺失或损坏回退为新运行输入。

文件配置显式声明 llm.request_contract=agentgo.model-request/v1；llm.stream、agents[*].observation_model、max_subtask_depth 被拒绝。旧工具无 wrapper/alias；L2 旧 control/investigation 强制历史模式已删除，旧观察锚点明确拒绝。新 Session 不能承接旧 Session snapshot 继续运行。

启动始终新建 Session。进入可接受版本的历史会话只恢复上下文，不自动续跑；非终态 Task 阻断、图停驻。旧 Trace/历史业务记录仅作为原版本事实读取，不重新解释为新工具调用。

## 不变的事务纪律

- L3 权限和实际执行来源必须可关联；模型调用完成不等于工具已执行。
- finalizing 是唯一终态通道，后续工具不执行；Effect prepared/unknown 不静默重放。
- Graph 单赋值端口、角色能力闭集和 Delivery 产物冻结继续生效。Graph v5 不代表支持任意 OR 汇合或多候选联合 promotion。
- 取消、用户明确 deadline/预算、HTTP/进程超时与真实 provider 配额仍有各自语义；不得借这些名义重建已删除的经验进展关卡。
- Prompt 不硬编码经验轮数，watchdog 不接管模型观察/决策。测试判题由 Python 完成。

后续修改权限、状态或协议含义须显式发布版本；字段新增也要核对持久化、恢复、调用方与测试，不通过缺省值扩大权限。五层职责及文件索引见 [五层规范](five-layer-engineering-architecture.md)，工具行为与阶段证据见 [工具契约](tool-taxonomy-and-contracts.md)。本次用户暂缓真实 SWE；不能用历史批次成绩证明当前版本已通过。
