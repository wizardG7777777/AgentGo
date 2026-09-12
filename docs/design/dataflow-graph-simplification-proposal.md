# agentTask 数据流图重建实施计划

日期：2026-09-11。状态：**代码已切换，验证与最终交付对账见第 15 章及验证记录**。文件名保留，避免既有讨论链接失效。

用户已确认：唯一节点为 `agentTask`，表示 Agent 执行一个确定任务并交付一个确定结果；完成与交付属于图级能力，删除强制 Proposal Acceptance；迭代通过更新同一图、追加新节点实例表达。其余章节是为落实这三项决定补齐的工程契约。以下保留实施依据；当前接口与实际验证以末尾对账和现行冻结基线为准。

## 1. 调查结论

当前实现是状态转移驱动、附带 Result→Input 数据传递的混合图，并非只按输入数据就绪执行任务的数据流图。

证据入口：

| 区域 | 当前实现及问题 |
|---|---|
| 节点与边 | `internal/graph/types.go::Node/NodeDefinition/Transition`：10 种 kind、Next/when、Wait/Tool/Subgraph/EndOutcome 等专用字段 |
| 执行 | `internal/graph/runtime.go::activateLockedWithReplay` 按 kind 分派；router、end、tool、approval、wait_event、subgraph 各有独立执行链 |
| 数据传递 | `types.go::InputBinding/ActivationResult` 已有稳定结果、证据、Delivery 引用；但普通节点不能像 join/acceptance 那样多输入就绪 |
| 创建与更新 | `minimum_validate.go` 要求 end、success end、所有节点可达 end 和各种出口；`contract_validate.go` 要求交付要求预先绑定且 success path 不绕过 |
| 模型准入 | `definition_compiler.go::Compile` 在机械校验后还强制调用独立 Proposal Acceptance；没有该服务也不允许提交 |
| 交付 | `acceptance.go::settleAcceptanceLocked` 负责调用 CommitDelivery；`runtime.go::deliveryCommitRefFor` 在图成功时要求提交引用 |
| 工作区 | `runtime.go::taskSpecFor/bindDeliveryWorkspace` 与 `bootstrap/graph_runtime.go::prepareDeliveryTask` 按 kind 区分验收、修复和隔离 |
| 任务身份与权限 | `model/task.go`、`agent/execution_lease.go`、`tools/submit_result.go` 仍携带 GraphNodeKind/controller role/verdict 特殊规则 |
| UI 与恢复 | `graph/runtime_suspend.go`、`bootstrap/graph_approval.go`、`ui/service.go/control.go/commands.go`、TUI `/event` 保留节点专属等待与审批语义 |

运行时 10 种，当前工具 schema 开放 9 种；subgraph 已禁止新建但执行代码仍保留。只删 schema 枚举不能完成此次精简。

本次真实 SWE 的 37 次 apply_graph_change 全是 create，29 次拒绝、8 次成功创建；不是 37 次运行中 update。四题成功写入候选但没有正式 acceptance，最终未提交 Delivery。详见 [原始运行调查](../test-issues/2026-09-11-swe-candidate-delivery-audit.md)。

## 2. 已确定模型：唯一 agentTask 节点

公开名称和 JSON 类型值固定为 `agentTask`，Go 定义使用 `AgentTaskNode`。`kind` 仅接受常量 `agentTask`，用于边界拒绝旧类型，不用于运行时多类型分派。删除旧 `NodeKind` 多类型枚举与分支；不保留 `agent`、`task` 或 `acceptance` 别名。Go 层 AgentTaskNode 是图定义，model.Task 是派发后的任务身份，二者不得混用。

节点表示“消费指定输入、由有能力的 Agent 完成一项任务、产生版本化结果”。调查、实现、测试、复核只是任务内容或授权差异，不是节点种类。

| 旧类型 | 推荐处置 | 保留的实际能力 |
|---|---|---|
| agent | 重写为 agentTask 节点 | Agent Loop 执行任务 |
| controller | 删除类型及图内专属控制阶段 | Scheduler 在图外维护定义；编排权限由 L3 授权，不靠 node kind 获得 |
| router | 删除节点和 Next/when 跳转机 | 需要模型判断时输出普通结果，由 Scheduler 根据结果扩展图；首版不增加任意条件边 DSL |
| tool | 删除图直接调工具的旁路 | 工具由任务内的统一 L3 分发与记账执行 |
| approval | 删除图节点与专用桥 | 必需用户交互使用现有 Interaction/工具；不删除基础授权服务 |
| acceptance | 删除类型、专属 verdict 路由和提交特权 | 可选复核是普通任务；文件交付是运行时事务 |
| subgraph | 删除模型、递归校验、派生图、父子终态回填 | 当前用同一图的节点集合表达；显示分组可为纯标签，不可隐含执行语义 |
| join | 删除节点 | 所有任务都支持多输入，输入齐备后运行 |
| wait_event | 删除节点 | 尚未到达的数据表现为输入未就绪；外部数据只经显式授权接入口注入 |
| end | 删除节点 | 图完成是图级动作与状态，不是占位任务 |

不把旧类型换成 agentTask.mode=controller/acceptance 等隐藏分派。数据流仍需要调度、取消、权限和事务，但这些由运行时承担，不必成为图里的控制节点。

## 3. 数据与就绪契约

### 3.1 输入和结果

- 输入槽显式引用已存在的上游输出槽，或图级外部输入槽；不允许拼错的悬空 node ID 混成“未来节点”。未知后续不提前声明边，待知道后再添加。
- 未到达的已声明输入合法，节点保持 waiting_inputs；必需输入齐备、类型可表示、执行面可用才产生一次 Activation。
- 多上游直接绑定不同输入槽，去掉普通节点最多一条入边的限制。保留“同一执行实例的同一输入槽单赋值”，不引入多个来源抢同一个槽的隐含 OR。
- 一份上游结果可供多个下游引用；引用保持来源 Task/Activation/Result/Evidence 身份，不复制自然语言当作权威。
- 开始执行时冻结输入与定义；后来的图变更和新结果不得替换该次执行正在读取的数据。完成结果不可覆写。
- 普通业务结论，如需要更多调查、发现缺陷、复核不通过，是结果内容；工具或任务失败则是可检视的执行事实。失败不能伪装为下游需要的成功输出。

### 3.2 候选代码是数据的一部分

结果如包含文件修改，必须带有可解引用的候选版本引用。运行时将该版本解析成检查/修改任务实际使用的工作区，不要求模型填写内部目录、哈希或猜测 Delivery ID。

读取同一候选的任务可以并行；后续写入从选定候选建立新的隔离版本，不能改写已被其他任务引用的版本。若多个输入包含不同候选，必须明确选择工作基线；不以第一个 DeliveryRef 为准，也不在本次增加多候选自动合并。

初期保持单一最终候选的交付范围，避免把节点精简扩大成多工作区合并系统。产物检查只证明引用、版本和事务一致性；不把 exit=0 等同于测试通过。

### 3.3 迭代与新执行

迭代固定通过新增 agentTask 实例表达下一轮：调查→修改→检查，结果需要返工时，Scheduler 添加下一次修改任务，引用已有候选与检查结果。没有默认轮数上限；没有自动复制旧任务或重开已结算实例。

删除旧 Next/when 回边状态机和同节点反复 Activation。一个 node_id 表示一项工作实例，失败/完成后返工必须追加新 node_id。每个已提交 revision 的数据依赖必须无环，但整个项目不是预先规划完的固定 DAG：同一图可以在运行中持续增长。L4 对尚未终结的同一任务进行已有错误重试不等于图迭代；保留 Attempt 身份和副作用核验，不允许重试覆写已发布结果。

## 4. 不完整图与调度

创建时可以只有一个调查任务，不需要 end、成功路径、修复节点、复核节点或预先分配所有交付要求。空图可保存但不可启动；启动至少有一个输入可就绪、执行能力可满足的入口。

启动后按输入就绪调度，不再要求唯一 root。外部初始材料是图输入，不需要 input 类型节点。初期没有必需上游输入的任务也必须有目标和合法执行规格。

当当前任务完成、已无可执行任务，但图还没被明确结束时，表示图仍开放，需要继续规划；不能自动成功、自动失败或保持无说明的 running。

保留简洁的图生命周期，单列派生运行情况（有工作执行、等输入、等规划、结算中）。这些不是新节点类型。Runtime 以持久化、去重的图事件通知图外 Scheduler；Scheduler 根据结果追加任务或请求完成。普通 send_message 仍只传信息，不因此恢复唤醒能力。调度器自己不作为被编排图的前驱节点，避免循环依赖。

通知丢失、Scheduler 失败、外部输入未到达必须显示明确原因；恢复通知不能靠无限复制任务。既有 Session 恢复不自动续跑的约束继续适用。

### 三个时点的检查

| 时点 | 应检查 | 不再要求 |
|---|---|---|
| 应用图定义/增量 | schema、身份、权限、已声明引用、输入类型、版本冲突及在途影响 | 所有未来节点、成功出口、所有交付要求已绑定、强制独立模型审批 |
| 派发某个任务 | 输入版本齐备、可解引用；route 对应真实可用执行者；工具和 WorkspaceManager 等执行组件满足需求 | 未来任务必须已经有执行者 |
| 请求整个图完成 | 选定输出和候选有效；在途执行及副作用已结算；交付事务完成 | 某种特殊节点已运行、固定数量复核或特定 pytest 命令 |

未来任务的执行能力不足应在预检中清晰展示，但不阻止与其无关的就绪任务调查。启动时的硬拒绝和 waiting_reason 必须区分，不能让任务认领后才发现依赖为空。

## 5. 图级完成与交付

复用编排类 control_graph，增加显式 complete 请求，携带 graph revision、request_id 和选定输出引用。此动作是本计划待实现的接口，当前不能直接使用；start/cancel 既有语义仍保留。完成请求与并发图更新的事务规则见第 10 章。

运行时在同一收口流程中校验引用，冻结选定候选，结算文件交付，最后写唯一图完成事实。文件提交失败或 Effect unknown 时报告明确结果，不能返回成功，也不能默认重跑。正在执行的任务不因 complete 请求被悄悄取消；须先结算或显式取消并完成清理。

图中若仍有已声明但未完成的工作，完成请求必须拒绝或要求调用者明确调整范围，不能静默丢弃。未来未规划的工作不是虚构的 pending 节点；任务目标是否充分完成由用户要求和业务判断负责，Runtime 不通过图形状或固定测试策略猜测。

业务需要复核时，用普通任务产生检查结果，并由 Scheduler/用户据此决定是否完成。不默认增加 mandatory_review、reviewer kind 或隐藏 acceptance 模型调用。最终交付仍保留实际文件版本、Effect 与回执，删除的是节点特权，不是写盘核验。

完整响应呈现由终态事件和现有输出服务提供。若需要模型整理交付说明，应是有明确身份和正常契约的请求，不再带旧 report_done/read_graph/get_task_result 指令。本次调查在 `bootstrap/graph_runtime.go` 的终态通知文本中仍发现这些旧工具名，需要一并清理。

## 6. 编排工具与运行时边界

- 四类工具目录不因删节点重新膨胀。read_graph_definition、apply_graph_change、control_graph、request_replan 继续承担图读写与生命周期；参数按新数据流契约重建。
- read_graph_definition 的辅助信息应返回可用执行能力/路由引用，避免模型把 Agent 名称当作 event_type。不能靠重试 worker/worker-1 猜测。
- Scheduler 负责依据新结果决定如何扩图；Runtime 负责就绪判定和唯一派发；Agent 负责节点任务；L3 负责权限、工具和执行环境。
- 独立 Proposal Acceptance 不是 acceptance 节点，但同样是每次建图的强制模型关卡。本轮整条退役，以机械增量校验替代。需要额外模型讨论时作为明确业务工作，不藏在提交 API 内。
- L1/L2 保持当前请求与 SSE 权责；只适配新 Task 输入、结果引用和上下文来源，不在其中实现图控制逻辑。watchdog 不承担补图、模型复核或数据绑定补救。

## 7. 删除、重写与保留清单

| 分类 | 实际范围 |
|---|---|
| 删除 | graph 节点专属枚举/DTO/分派；WaitSpec/ToolSpec/SubgraphSpec/EndOutcome；approval 网关；subgraph 父子回填；acceptance 专属 verdict/提交流程；独立 Proposal Acceptance；旧 controller/recovery 节点通路与新运行兼容入口 |
| 重写 | `graph/runtime.go/types.go/validate.go/minimum_validate.go/contract_validate.go/definition_compiler.go/authoring_runtime.go`；输入就绪、图开放状态、图完成；authoring_store/journal/recover 对应新数据 |
| 重写跨层接缝 | `bootstrap/graph_runtime.go/bootstrap.go`；`agent/execution_lease.go/agent.go` 的图角色判断；`model/task.go` Graph 字段；`tools/graph_schema.go/graph_authoring.go/graph_routes.go/submit_result.go` |
| 保留并迁移 | Activation、结果引用、来源证据、幂等派发和唯一终态算法；workspace 内容版本与隔离；Delivery/Effect 持久化与实际提交；Interaction 通用能力 |
| UI/Trace 适配 | 去节点特有字段与 `/event` 旧桥；展示任务输入、候选版本、等待原因、图完成请求与提交结果；已写历史 Trace 不重新解释 |
| 配置与提示词 | AGENTS、五层规范、工具契约、冻结基线、Scheduler/Worker/Verifier 模板、Team 路由与 SWE YAML；去强制验收节点与旧工具名 |

以新版本独立拒绝旧执行图和旧 node kind，不做 agent/acceptance→agentTask 自动转换，不创建兼容 wrapper。历史磁盘保留；只读历史展示不得与新执行解码共用 fallback。目标版本和目录见第 12 章，这些不是当前已经上线的版本。

`join` 等共享的输入/证据算法必须先迁移再删除旧入口。`runtime_suspend.go` 中 Session 挂起/恢复职责与 approval 专属逻辑混合，不能整文件盲删。同样不得整包删除 Interaction、Effect、Workspace 或 UI。

## 8. 实施顺序与验收

1. 按已确认产品语义更新职责规范为待实施状态，落地新输入/结果/完成请求契约；不得再次引入多节点类型或强制模型准入。
2. 建立统一任务及多输入就绪运行时，保留幂等、来源和在途冻结，先用确定性数据流测试验证。
3. 将候选版本解析与提交从 acceptance kind 中移出，接通图级收口；验证多 Agent 实际读取同一版本。
4. 切换 Scheduler、Agent、Team、普通检查任务、UI 和恢复；同步移除独立 Proposal Acceptance 与旧提示词控制约束。
5. 删除旧类型、执行链、兼容入口及旧专用测试；按“删除/迁移/重写/新增”对账。
6. 重写 SWE 审计和本地二进制 fixture，三平台完整验证；真实模型复测另行按用户授权执行，不修改旧题结果冒充修复后成绩。

最低回归：

- 只创建调查任务就能启动；无 end/acceptance 合法；调查完成后追加实现、再追加检查，无重复创建顶层图。
- 两个来源向同一任务的不同槽供数；就绪才启动；fan-out 不复制权威；重复事件不重复执行。
- 边和新节点引用历史已完成输出时可执行；在途变更不能替换已冻结输入。
- 调查阶段无后续任务显示等待规划；无轮数关卡；Scheduler 事件恢复与失败有明确状态。
- 候选修改在多个 Agent 间版本一致；未交付主根仍旧但不会被错当候选；最终完成请求实际提交后 Python Judge 测到同一版本。
- 没有特殊验收节点也能完成交付；需要复核的业务可以添加普通检查任务；没有强制模型单工具报告。
- route 权威可发现、执行组件缺失不会伪造可执行；多行 Shell 事实身份合法。
- 在途任务、未知副作用、版本冲突、提交失败不能被 complete 忽略；旧图明确拒绝且历史不删。
- 本地 Responses/Chat Completions 二进制、Go/Python/race 与 Windows/Linux/macOS CI；SWE 仍由 Python 判断真实测试结果。

## 9. 公开数据契约

### 9.1 图定义和节点

图定义只含 schema、graph_id、revision、目标与约束、图级输入声明及 nodes。nodes 为包含唯一 node_id 的列表，Runtime 可建立索引；不同时在对象键和字段中维护两份节点 ID。删除 root、next、when、end_outcome、requires_acceptance、控制角色、恢复专用字段和预先覆盖所有成功路径的 contract_bindings。

每个 AgentTaskNode 包含：

| 字段 | 含义 |
|---|---|
| node_id、kind=agentTask | 稳定实例身份；节点 ID 不能在删除后复用 |
| title、objective | 确定任务与所需结果；调查结果本身可以是待验证假设或后续建议 |
| inputs | 命名输入槽到 graph_input 或 node_result 引用的映射；同时新增的节点可相互引用，但整次变更必须无环 |
| result_schema | 本任务唯一业务结果对象的类型契约；允许只要求简短 summary，不强迫模型填写框架观察报告 |
| execution | 已注册 route 引用、所需工具与执行环境；默认项由运行时解析成显式有效规格 |
| labels | 可选展示标签；不得通过 labels/metadata 决定控制权限或执行算法 |

依赖边从 inputs 推导并显示，不同时维护可单独修改的 edges 和 next，避免两个结构权威。一个 node_result 既可引用整份业务结果，也可选取其声明字段；缺字段或类型不符时不派发下游。

### 9.2 唯一结果与失败事实

submit_task_result 提交一个符合 result_schema 的对象。Runtime 生成不可变结果信封，包含 ResultRef、Run/Graph/Node/Task/Activation/Attempt 身份、实际候选引用、证据引用和内容摘要；这些身份由框架填写。正文与工具摘要从该信封派生，不维护可独立修改的第二份权威结果。

失败、取消或 blocked 仍产生终态事实，但不生成可冒充成功的业务输出。Scheduler 可检视该事实并将其作为明确的诊断输入传给新增节点，不能让下游依赖自动跳过或读取上一轮旧结果。一个“检查发现缺陷”的任务可以成功完成，结果表示发现的问题；业务判断与执行失败分开。

### 9.3 编排接口

- apply_graph_change(create)：只提交当前已知目标、图输入和 agentTask 集合；不自动启动。
- apply_graph_change(update)：expected_revision、request_id、add/update/remove 集合；同一次变更原子生效，在运行图中新增的就绪节点随后自动派发，无需再次 start。
- update/remove 只允许尚未创建 Activation 的节点，且不得留下悬空消费者；删除保留审计记录。已激活或终态节点不修改，需求变化通过追加实例表达。
- read_graph_definition：同时提供结构版本及可用 route/能力目录的引用。无图时支持显式读取能力目录视图，不伪造图 ID；不泄露凭据或框架内部路径。
- request_replan：保持请求/回执语义，不直接改图。Graph Runtime 的规划事件与普通 send_message 分离。
- control_graph：start/cancel/complete。结束后拒绝 update，不隐式复活图。

请求幂等键绑定调用主体、Session、Graph 和 request_id；相同身份相同内容返回原回执，相同身份换内容拒绝。创建重试不新建图，更新冲突返回当前 revision 和可定位的冲突，不要求模型重建顶层图。

### 9.4 外部输入与等待

Graph Runtime 提供统一 ProvideInput 端口，由已授权的用户输入/Interaction/外部集成适配器调用；输入含 graph_id、已声明 port、request_id、预期图版本及类型化数据或 ContentRef。沿用现有用户输入与授权体系，不新增 wait_event 节点、任意事件匹配器或隐式广播。

每个图输入版本不可覆盖：重复身份同内容返回原回执，换内容拒绝；需要修订时创建新输入版本，新增任务显式引用它。已激活任务仍读取旧版本。未知端口、越作用域候选或完成中的图拒绝输入；内容持久化与就绪通知应可恢复且幂等。

外部输入尚未到达时只等待，不自动生成反复询问用户的任务。只有节点本身需要交互时才使用 request_user_input。CLI/Web 的旧 /event 接口退役，应用级新输入适配到 ProvideInput；模型工具目录不因此增加事件工具。

## 10. 调度、并发与完成事务

### 10.1 唯一派发

就绪判定、冻结定义/输入、创建 Activation 和派发意图必须形成持久化事务边界。Task ID 从稳定执行身份派生；重复扫描、事件重放和崩溃恢复不得生成第二个任务。一次 AgentTask 实例只有一次 Activation，可含 L4 的多个 Attempt；终态后新增实例才可继续工作。

派发前复核真实执行能力，包括 workspace manager/activator 和 route 所有权。未知工具/伪造 route 在定义边界拒绝；声明有效但暂时没有可用执行者时显示 waiting_executor，并允许其他工作推进。空图和所有入口都不能启动时，start 返回具体阻塞事实。

### 10.2 图外 Scheduler

节点终态、外部输入到达、显式重规划请求和执行阻塞变化产生带身份的规划事件。按图串行处理规划请求，合并尚未消费的事件；一次规划期间到达的新事实在下一次继续处理，不丢失也不并发重建图。

同一事件投递重试沿用稳定请求身份。Scheduler 成功处理但选择等待时，记录等待原因，只有新事实或用户请求才再次激活；不以空闲轮询、累计轮数或默认“新知识阈值”反复调用模型。Scheduler 调用通过现有 L2/L1 契约并记录真实 operation/task 身份，不在业务图里制造 controller 节点。

### 10.3 完成请求

complete 输入包含 outcome（success/failed/blocked）、expected_revision、request_id、summary，以及 success 所需的 outputs/result_refs 和可选 selected_candidate_ref。选定候选优先从明确输出解析；存在多个不同候选时必须显式选择。cancel 继续使用独立动作。

失败节点可作为历史保留，迭代成功不要求历史全部变绿。若其工作已被替代，完成请求附 superseded_by 等明确处置引用；没有后续替代的失败/阻塞工作必须明确说明为何不再需要，不能被隐式忽略。尚未派发且不再需要的任务通过 update/remove 审计移除，活跃任务必须先结束或显式取消结算。

完成流程：

1. 复核调用权限、revision、当前任务和副作用事实、输出作用域与结果类型。
2. 同一图写入幂等 CompletionIntent，进入 finalizing，冻结输出、候选及 state_version；拒绝后续结构变更、外部输入和新派发。
3. 无文件变更时只核验输出；有文件变更时执行 Delivery 提交：候选完整性、主根基线、文件冲突和 Effect 结算。
4. 持久化 Delivery/输出回执后，写唯一图终态及通知；success 不能早于必要提交成功。

预检拒绝不改变运行图。进入 finalizing 后的失败保留完成事务状态：已证实未执行的操作才可依契约重试，Effect unknown 不自动重放；检查状态和人工处置入口通过统一检视/控制接口暴露，不新增隐藏模型工具。

主根内容与候选基线冲突时拒绝覆盖，不能悄悄合并。跨文件写盘沿用可追溯提交和恢复，不把它宣传为操作系统层的全目录原子事务；部分提交或无法确认的状态不得发布 success。

## 11. 候选、交付与既有故障修复

候选与交付事务拆开：候选是某份不可变代码状态；Delivery 是选定候选提交到目标根的操作。普通检查任务读取候选不触发 BeginRepair，不因执行角色变更候选状态；修复任务从候选建立新版本，保留 parent_candidate_ref。

删除按 Graph execution_class 或是否有 apply_change 工具猜测修改性质的路径。run_shell 也能写文件，其改动必须进入同一候选生成和版本核验链；所有 Shell 操作继续保存完整执行事实。无候选输入时，由运行时提供明确的项目基线，不能由模型填写任意内部工作区路径。

复用工作区快照、manifest、内容存储和 Effect 算法，但 Delivery DTO 删除 acceptance_outcome_ref/producer role 特权；按完成事务和选定结果绑定。禁止共享可变 Delivery workspace 使后续修改污染已发布结果。

本次将三类已证实的故障纳入重构验收：

- 四题候选已改而主根未交付：用无特殊验收节点的完整数据流和图级完成验证修复。
- WorkspaceManager/WorkspaceActivator 缺失：派发前能力校验，覆盖实际 Runner/Scheduler 装配，不只 mock route 名称。
- 多行 Shell 导致 fingerprint.identity 非法：使用真实 CallID/结果或证据引用作为身份，命令正文仅为内容；删除依赖进展分类决定正常调查是否可继续的残留路径。必要持久化失败仍明确阻断，不通过吞错放行工具。

不改 watchdog 生产代码，不恢复 Observation/record、固定轮数或知识预算关卡。

## 12. 一次性版本切换与配置

以下为目标版本；实施时创建对应常量和严格解码器，不代表当前版本已变化：

| 数据域 | 目标 |
|---|---|
| Graph 定义与执行 | agentgo.graph/v7，唯一 kind=agentTask |
| Authoring 摘要 | agentgo.graph-authoring-definition-digest/v3 |
| 图结果与完成事务 | agentgo.graph-result/v1、agentgo.graph-completion/v1 |
| agentTask 结果信封 | agentgo.agent-task-result/v1 |
| TaskOutcome / TerminalIntent | agentgo.task-outcome/v4、agentgo.terminal-intent/v2，统一新运行终态记录，清除节点类型专属字段 |
| 候选与交付 | agentgo.candidate/v1、agentgo.delivery/v2 |
| ExecutionLease / Session | agentgo.execution-lease/v4、Session 8 |
| SWE 结果 | swe-result/v5；Judge v2 与 pytest-phase-report/v2、swe-test-execution/v1 保持判题语义 |

新目录使用 graphs-v6、graph-authoring-v3、graph-completions-v1、candidates-v1、deliveries-v3、task-outcomes-v3、loop-facts-v3、taskmem-v3。保持 run-usage 等未变数据域原契约；L1 请求、L2 Snapshot/策略、SSE eventCursor 不因节点精简盲目升版。必须排查这些存储是否嵌入旧 Graph/Outcome 引用，并在新执行边界拒绝旧引用。

Session 7 及以前不能恢复执行；历史目录不删除、不迁移，不把 agent/acceptance 自动翻译成 agentTask。旧图字段和节点名显式报退役错误，不能靠忽略未知字段继续执行。

配置新增 graph 配置块，显式声明 `graph.request_contract: agentgo.graph/v7`；文件缺失不由默认配置补齐。这是新建的配置域，不假称当前已有 runtime.graph_contract。旧图创建 schema、旧特殊节点配置、Proposal Acceptance 专属模型/提示词配置全部拒绝。原 verifier 名称可以继续作为普通 Agent 的显示身份，但移除保留 route 和节点类型授权语义；任何保留 route 都必须出现在能力目录中。

## 13. 实际文件与清理任务

实施前按下表建立删除对账；混合文件保留通用职责，旧链路不得用新 wrapper 留下：

| 文件/包 | 必须执行的改动 |
|---|---|
| internal/graph/types.go、runtime.go | 重建 AgentTaskNode/结果依赖；删除旧 activateRouter/Tool/Approval/WaitEvent/Subgraph/Acceptance、evaluateJoin 和 commitEndOutcome 分派；迁移就绪与持久化算法 |
| graph/acceptance.go、proposal_acceptance.go、recovery_delta.go | 退役专属机制；通用证据解引用和 Scheduler 事件移至职责清晰的实现，再删除旧文件入口 |
| graph/validate.go、minimum_validate.go、contract_validate.go、outlet_check.go、output_contract*.go | 删除根节点、成功路径覆盖、状态出口、verdict 特例；重建引用、DAG、结果 schema 与增量影响校验 |
| graph/definition_compiler.go、authoring_types/store/runtime.go、journal.go、recover.go、store.go | 新版本不可变定义、增量/CAS、派发和完成恢复；删除旧版本执行分支 |
| internal/proposalacceptance | 调用方切断后整包删除，连同独立模型工具、配置与专用测试 |
| bootstrap/graph_approval.go、bootstrap.go、graph_runtime.go、loop_intervention.go | 删除审批/子图/控制节点桥；重建候选解析、图外 Scheduler 和完成交付接线 |
| agent、model、runner、scheduler 的图接缝 | 去 GraphNodeKind/GraphControllerRole 特权、旧自动回边和验收专属工具筛选；统一实际能力授权、任务终态与结果生成 |
| tools/graph_schema.go、graph_authoring.go、graph_routes.go、submit_result.go、plan_control.go | 统一 agentTask 数据输入、增量、能力目录、图级 complete 和普通结果；删除验收 verdict 前置判断 |
| delivery、workspace、outcome、session、fulfillment、loopprogress/taskmem | 解耦候选版本与提交事务；统一新版本，剥离旧节点/验收假设及进展控制残留 |
| UI/TUI/Trace、Reactors、模板与文档 | 新等待状态/完成回执/输入候选来源；删除 /event 旧节点接口和 report_done 等旧指令，不恢复旧展示页面 |

同步追踪非直接引用，例如字符串 agent_type/acceptance.verify、最终报告 Prompt、Reactor YAML 的旧事件和仅测试可达构造器。不能只用 KindAcceptance 搜索结果为零宣称删除完成。

## 14. 测试与最终完成标准

分层测试：契约深拷贝与摘要、DAG/多输入、增量 CAS、唯一派发、完成并发、失败实例替代、候选不可变与实际绑定、Shell 写入、冲突/崩溃/Effect unknown、Session 拒绝与历史保留。新模型测试必须从公开接口走完整接线，禁止为 fixture 保留旧节点入口。

本地二进制 fixture 重写为真实 SSE provider，覆盖以下完整链：

1. 创建只有调查的图并启动，当前工作结束后等待规划。
2. Scheduler 在同一图追加两个可并行调查节点，再追加消费两份结果的实现节点。
3. 实现输出候选 A，普通检查节点读取 A 并返回问题；追加实现 B，引用 A 和问题结果。
4. 检查 B，图级 complete 提交 B，断言最终主根、结果引用、Delivery 回执和用户最终输出一致。
5. 验证无特殊节点、无 Proposal Acceptance API 调用、无 end/next/when、无自动回边、无默认轮数关卡。

SWE Test Runner 同步更换 runtime_audit 对新图、agentTask、候选和完成事务的读取，摘要区分节点执行/业务修复/图开放等待/交付结算。保留四个变量预检、模型去重、SSE 结束校验、Python 测试身份与阶段失败比较。正式 Judge 只测最终被交付的主根，不偷偷改为候选目录使失败变绿。

针对本次六题保留原数据；用脱敏且有界的故障 fixture 重放错误路由、无验收图、验证旧主根、多行 Shell 和缺执行组件，不将历史 trace 转换为可执行新图。真实批测在获得执行授权后使用独立新 run 目录验证，不覆盖原证据。

验收命令包含 go test ./...、go vet ./...、go build、Python 离线全套、两协议本地二进制和 Team 用例；Linux 对图/调度/输出等并发域跑 race，Windows/Linux/macOS CI 均需实际通过。真实端点协议和 SWE 成绩独立报告，不能以 fixture 代替。

完成标准：单一 agentTask 链路可从调查增量扩图直到实际交付；旧节点/强制 Proposal Acceptance/状态转移旁路不可执行；所有正式调用方、配置、恢复、测试与文档已切换；删除/迁移/重写/新增清单与测试证据齐全。过程中不设置双轨兼容开关，不以局部编译通过宣布重构完成。

交付物：更新后的架构与 AGENTS 实现索引、正式接口说明、删除对账、配置示例、测试与 CI 记录、真实复测结果或明确未执行项。完成证明另由实际测试与删除对账提供。

失败反馈使用显式 node_outcome 输入引用已持久化终态事实；它与 node_result 成功业务结果分开。追加的诊断/修复仍为 agentTask，不是恢复控制节点。

## 15. 当前实施对账

已实现唯一 agentTask、数据输入就绪、无 root/next/when 的增量图、图外 Scheduler、普通检查任务及候选版本传递、图级完成与交付。旧图引擎、Proposal Acceptance、审批/子图/工具节点桥、旧 L4 intervention 命令生产和投递已删除，不用 wrapper 兼容。

实现中收敛了两处存储安排：定义、执行、请求回执与 CompletionIntent 共用 graphs-v6 的摘要链日志，不再各建 authoring/completion 影子账本；候选目录与元数据共置于 .agentgo/candidates-v1。定义摘要由 graph/v6 schema 约束，不增加单独 authoring 摘要版本或第二份 GraphResult 权威。

失败诊断增加显式 node_outcome 输入，与成功 node_result 和外部 graph_input 分开；不增加节点类型。图取消等待真实任务回执，包括未开始 Attempt 的取消。没有新事实时不重复产生空闲规划事件。

数据版本：Session 8、Lease v4、TaskOutcome v4/TerminalIntent v2、Candidate v1/Delivery v2、SWE result v5。其余未变 L1/L2/Run 契约保持原版本。所有旧图和旧控制字段拒绝执行，历史目录原样保留。

验证记录见 [agentTask 验证](../test-issues/2026-09-11-agenttask-validation.md)，文件删除清单见 [删除对账](agenttask-deletion-ledger.md)。真实 SWE 单题成功不代表整个 Flask-8 已通过；CI 和剩余限制分别列明。

2026-09-12 后续修复：模型工作基线字段已删除，Graph v7 / graphs-v8 使用运行时候选谱系解析；等待事实按节点持久化去重。此前 v6 实施验证仍为历史记录。见 [剩余问题修复与复测](swe-remaining-repairs.md)。
