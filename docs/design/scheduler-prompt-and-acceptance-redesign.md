# Scheduler 提示词重构与验收机制重设计

> 状态：**已实施（2026-08-19）**。核心切片全部落地：数据流引擎（activation 级
> Result Store + TransitionRecord.Input → Execution.Input + 证据组装 + 任务注入）、
> 输入端口 barrier（target_input / required_inputs）、验收结算重写（data-ready 门控 +
> required_evidence + 谱系判定矩阵 + verifier 去 Shell）、Schema 清理（删
> Capability.Budget 与跨任务 activation 上限）、Scheduler prompt 全量重写
> （embedded:v7.5-unified-graph-terminal-report）+ 统一 Graph 收官生命周期 + 文档同步。
> 本文保留为设计决议档案；文中所有架构事实均标注了代码位置。

Schema 同步删除了从未绑定 registry、也没有 Runtime 消费者的
`task.output_schema` 占位。结构化路由事实统一由
`submit_task_result(result={...})` 产生；不宣称当前不存在的 schema 强校验。

## 1. 背景与问题

### 1.1 触发点

改造前 Scheduler system prompt（历史版本 `embedded:v6.5-kind-paradigms`）的
组织轴是**机制**（十种节点 kind 怎么路由、边条件怎么写），不是**认知**
（节点在认识世界还是改变世界）。当时可观测的病症：

- **总分形态倾向**：Scheduler 普遍把任务拍成"并行 fan-out + join + 汇总"。
  - prompt 层根因：`scheduler.go:371` 明确指令"调查/研究类请求应按子方向并行拆分"；
    `scheduler.go:233-236` probe_directory 文件量拆分启发式（"20+ 文件就并行拆"）纯机械、
    不看依赖方向。
  - 旧机制层根因：普通 fan-in 曾按 first-arrival/OR 激活，并行 fan-out 必须经
    join barrier 汇合且 join→下游只能 completed——"扇出+汇总"是最容易写对的
    形态；条件边要求互斥穷举、router 无匹配出路即整图 failed，写错代价高。
    本轮安全基线已禁止普通节点多静态入边与共享端口 OR（见 §2.6），不再把
    first-arrival 当成 Scheduler 可依赖的 authoring 能力。
- 现实任务不都是总分形态：有前置依赖的修改链、大场景整体修改等被压成总分时，
  下游拿不到上游产出（图内节点任务描述建图时冻结，无数据流织入，见 §5.1）。

### 1.2 现行验收机制（本轮安全基线）

- `acceptance` 是发任务型节点，默认路由 `acceptance.verify`；节点必须提供非空
  title 与逐项验收标准，`required_inputs` 端口齐备后才发布任务；
- `builtin/verifier@1` 的工具面固定为 read/list/grep/glob/web +
  `submit_task_result` 的正向闭集，无写工具、Shell、消息、发任务、用户交互或
  `request_replan`；当前不含 MCP，只能结合上游数据流绑定的 Result/Evidence
  做独立判断；
- 源 activation 的完整 Result/Evidence 先落 durable Result Store，实际生效边再把
  稳定 ResultRef、目标端口与 EvidenceRef 绑定给 acceptance activation；
- `cited_evidence` 只做输入谱系核验：越谱系引用由 Runtime 标记 `disputed`，
  节点 failed 并唤醒 Scheduler；不引用不判死。所需输入或证据不足时 verifier
  提交 `status=blocked`，旧 `unverifiable` 与逐字 evidence_items 契约已经删除；
- completed 结果必须省略 `event`，只提交 `verdict=pass|fixable|failed`，并由
  `$.verdict eq ...` 精确选择业务出边。Runtime `failed` / `blocked` 事件只作兜底。

## 2. 设计原则（用户定调）

### 2.1 节点语义三分法（认知轴）

任何工作节点都可归类（正交于机制轴 kind）：

- **调查（诊断）**：认识世界——调查、测量、理解、诊断。只读工具面。
- **变更**：改变世界中的编辑——编写、修改。带 write_file/edit_file。
- **执行**：改变世界中的命令执行——部署、发送、跑测试。带 run_shell。
  从变更中单独划出，因为 shell 日志极易撑爆执行者上下文——
  拆分解决"别让改代码的 agent 亲自跑长日志命令"，
  不解决"执行 agent 自己被日志撑爆"（后者是 runner 上下文治理问题）。

纪律：**不要机械地给所有任务加"研究"阶段**（过度研究）；
最终图中所有节点与行动都必须可归入认知或改变。

本轮只落地 **prompt 认知纪律**：先定性，再按性质收窄 per-node capability；
`submit_graph` 的 route/capability 校验继续 fail-closed。已拍板不引入
`metadata.category`，也不把三分法升格为 Runtime 字段或 Gate 规则；当前通用
`Metadata` 仅因 `metadata.route` 仍承担历史路由职责而暂留，后续单独迁移。

### 2.2 停止拆分的判据（节点粒度）

一个节点可以停止拆分，当且仅当执行者能够：

1. 在有限上下文中理解其局部目标；
2. 获得完成任务所需的输入和工具；
3. 在一次有界执行中产生明确输出或副作用；
4. 使用明确验收条件判断成功与否；
5. 在失败时局部重试，而不必重做大量无关工作；
6. 将结果交给下游，而不需要传递完整内部对话。

**原子节点 ≠ 单次 LLM 调用**：一个节点可含多个工具调用甚至一个完整
Codex 式循环（架构天然成立——agent 节点认领后即一个完整 ReAct 循环）。
这六条**取代** probe_directory 文件量启发式（删除 `scheduler.go:233-236` 拆分表）。

### 2.3 从目标出发建图、执行中更新图

- 建图从用户目标出发；调查/执行过程中可更新图；
- 节点不可无限拆分，也不推荐过分拆分（判据见 §2.2）；
- 动态更新的三条合法通路（现有机制，prompt 要教对）：
  1. 预铺条件边（运行时才知道的分支，建图时穷举）；
  2. patch_graph 未激活节点（base_revision CAS 纪律）；
  3. 覆盖度裁决节点 + gap 条件边（结果到齐后判断缺口）。
- 硬约束：**在途 activation 的 next 已冻结**（定义快照，`types.go:71-77`），
  patch 在途节点路由静默无效。

### 2.4 模糊输入不追问

默认用调查消解模糊；`request_user_input` 是最后手段
（仅当答案真正依赖用户偏好且无法自查）。并非所有任务都需要严格验收——
验收按价值挂载，不是图形标配装饰。

### 2.5 验收机制总原则

- 验收节点由**无领域角色的 verifier agent** 驱动，判据全部来自节点的
  task.description（completion contract 形状，对齐 Kimi Code /goal 语义：
  终态 / 证明方式 / 边界 / 止损规则）；
- **信任 verifier 的判断力**，不引入 git/正则/行校验/内容校验等机械产物校验；
- 但系统保留对**行为事实**的核验（与判断质量无关，见 §4 的判定矩阵）；
- verifier 不写报告：业务结论只经 `submit_task_result.verdict` 交给图；
  `event` 不再承载 pass/fixable/failed 业务 verdict。

### 2.6 当前单赋值安全基线与 future token

本轮数据流 Runtime **没有 flow generation/correlation token**。为避免不同分支、
不同循环代次的数据错误拼接，authoring 收缩为以下可执行子集：

1. 所有非 barrier 节点最多一条静态入边；条件分支必须各自保留后续与 `end`，
   不能再汇入同一个普通节点形成 OR mux；
2. join / acceptance 的每个 `target_input` 只有一条生产边。并行 AND 使用不同
   端口；互斥候选不得共享端口；
3. 循环体可直接作为 root：root 有一次隐式初始 activation，再由唯一回边以
   `activation:"new"` 重入。不要额外添加 start→root，否则形成第二条静态入边；
4. 复杂 OR mux、共享端口候选与跨 generation 汇流留到 flow generation /
   correlation token 落地后开放。当前基础设施不是表达不了分支，而是刻意拒绝
   无法证明同代关联的数据合并。

Acceptance 节点必须有非空 `task.title` 与写明逐项验收标准的非空
`task.description`。completed 业务结论统一通过 `$.verdict` 精确 `eq` 求值；
verdict 枚举固定为 `pass` / `fixable` / `failed`，completed 结果必须省略
`event`。acceptance 出边禁止无条件、`always`、`completed`、`pass`/`fixable`
事件条件，只允许 verdict 业务分支与 Runtime `failed` / `blocked` 兜底事件。
证据或能力不足时 verifier 提交 `status=blocked`；`disputed` 是 Runtime 核验
状态，不是 verifier 可提交的 verdict。

## 3. Scheduler prompt 重构方案

### 3.1 统一 Graph 路径

删除 D（直接回答）/ G（Graph）双路径。**每个新用户请求都必须先形成
Graph**；最终自然语言回答是 Graph 执行结果的呈现，不再是一条与 Graph
并列的执行路径。

统一生命周期：

1. 初始 Scheduler 只负责理解目标并制定、提交 Graph，不在建图前完成主体工作；
2. controller / agent 等工作节点执行请求，并以结构化结果推动图转移；
3. Graph 到达 `end` 并发出 `graph_ended` 后，Scheduler 读取权威终态与节点结果，
   再向用户呈现最终回答。

由此删除“一次原子只读查询可直接回答”“第二次工具调用时从 D 切换到 G”等
判定。是否建图不再是决策项；Scheduler 只需要决定**图的节点粒度、拓扑、
能力、路由和验收强度**。闲聊、状态回答和简单查询也使用最小 Graph，接受
一次固定的图提交与收官成本，避免直接路径以例外形式重新出现。

### 3.2 统一决策序

收到请求后按以下顺序制定 Graph：

1. **给工作定性**：按 §2.1 判断是调查、变更还是执行；一个请求可包含多种
   性质，但不得机械地添加“先研究”阶段；
2. **按六判据确定节点粒度**：使用 §2.2 判断工作是否能在一次有界执行中
   完成。工具调用次数和文件数量都不是拆分依据；
3. **按真实依赖选择拓扑**：先判断先后依赖、输入输出交接、失败隔离和上下文
   耦合，再选择单节点、依赖链、条件分支、fan-out / join 或回边；依赖优先于
   并行；
   拓扑必须落在 §2.6 的单赋值安全子集内：非 barrier 单入边、barrier 端口
   单生产者、条件分支各自收官；
   当前阶段 Scheduler 只生成单层 Graph，不主动使用 `subgraph`；Runtime 保留并
   验证嵌套图基础能力，待单层图规划、日志审阅与治理稳定后再开放；
4. **按性质配置 capability 与路由**：决定使用 controller 还是 agent，收窄
   tools / model / isolation，并确认目标 route 真实存在且能力足够；
5. **按价值决定是否挂验收**：依据 §2.4 和 §4 判断是否需要 acceptance，
   不把验收当成所有图的固定装饰。

### 3.3 最小图、节点内循环与拆分边界

最简单的请求退化为一个可执行工作节点加 `end`：

```text
controller（Scheduler 自己完成） → end
```

`end` 只是收官节点，不算业务工作节点。**原子节点不等于一次 LLM 调用或一次
工具调用**：controller / agent 节点可以包含多次读写、命令调用和完整的有界
ReAct 循环。只要仍满足 §2.2 的六项判据，就不应为了表现“有规划”而继续拆分。

只有出现真实边界时才增加节点：

- 后一步必须消费前一步的明确产物，或两步需要不同能力、权限、上下文与局部
  重试边界时，使用依赖链；
- 先探明条件、达标后才能变更时，使用前置条件门；
- 子问题相互独立且并行收益明确时，使用 fan-out，经 join barrier 汇合；
- 运行时结果决定下一步时，使用互斥穷举的条件边或 router；
- router 只按已送达输入的结构化字段分流，激活后以自身 `completed` 终态求值
  `next`，不会继承上游 `failed` / `blocked` 事件；上游失败应在源节点出边直接
  绕过 router 到 repair。若有意把失败 Result 送入 router，则按
  `$.status eq "failed"` 分流，禁止在 `router.next` 写不可达的
  `event=failed`；
- 实现结果需要独立裁决时，增加 acceptance 与必要的修复回边；
- 单个执行者可以在有限上下文内整体完成的大场景修改，仍可保留为单节点。

节点内 ReAct 循环用于同一局部目标的有界探索与执行；Graph 回边用于跨
activation 的返工、复验或局部重试。二者不要因为都表现为“循环”而混用。

### 3.4 制定图与更新图

每个请求都必须**制定图**，但并非每张图都必须发生 `patch_graph`。只有执行中
出现的新事实证明原图覆盖不足时，才按 §2.3 更新图；能够由当前节点在有界执行
中消解的问题继续留在节点内部完成。

动态图更新遵守以下边界：

- 在途 activation 的 `next` 已冻结，当前节点不能通过 patch 临时长出新的后继；
- `patch_graph` 只修改尚未激活的节点，并遵守 `base_revision` CAS 纪律；
- 建图时已知结果可能暴露信息缺口，就预铺覆盖度条件边、router 或扩展节点，
  让后续 controller 能修改仍未激活的结构；
- 运行时才能确定的分支在建图时穷举，不能等当前节点结束前再修改自己的出边；
- 不得为了体现“动态图”而无事实依据地 patch，也不得先由 Scheduler 完成主体
  工作，再补交一张装饰性 Graph。

### 3.5 新 prompt 的章节结构（草案）

1. 身份、统一 Graph 生命周期与最终回答呈现；
2. 你能看见什么（board snapshot 说明，保留现状）；
3. **统一 Graph 决策序**（§3.1–§3.4，重写核心）；
4. 图语义参考（十种 kind、边条件、join barrier、单赋值拓扑安全基线——
   **从“主教材”降级为“参考手册”**；条件边互斥穷举、router 无匹配即整图
   failed、当前不支持 OR mux 等边界保留警示）；
5. **非总分形态图例库**：
   - 最小图（单个 controller / agent 工作节点 → end）；
   - 依赖链（前置修改 → 依赖其产出的后续修改）；
   - 前置条件门（先探明/准备，达标才进入变更）；
   - 大场景整体修改（单节点内含完整 ReAct 循环，不拆）；
   - 增量扩展（覆盖度裁决 + patch 尚未激活的节点）。
6. 验收章节（重写，见 §4.6）；
7. 用户澄清（收缩为最后手段，§2.4）；
8. 节点能力声明 / 路由指引 / 动态组队纪律（保留现状，小修）；
9. 工作模式两轴 / 与代理协作 / 否定约束保留铁律 / 反模式（保留现状）；
10. **删除**：D/G 路径与一次工具调用阈值、probe_directory 文件量拆分表
    （`scheduler.go:233-236`）、“系统性调查按子方向并行拆分”的机械指令
    （`scheduler.go:371` 改写为统一决策序引导）。

### 3.6 必须保留的既有内容

- 验收红线（`scheduler.go:184`，07-21 事故产物：禁止为通过验收修改
  被验收对象/环境状态；不通过只有修复实现/修正口径/问用户三条出路）；
- 保留用户否定性约束铁律（`scheduler.go:395-402`）；
- patch_graph CAS 纪律与在途冻结警示；
- Graph controller 控制面硬绑定（`scheduler.go:180`）；
- 反模式清单（`scheduler.go:404-410`）。

### 3.7 工程纪律

- prompt 是反引号原始字符串常量，正文**禁止出现反引号**；
- 每次正文变更递增 `schedulerPromptVersion`（`scheduler.go:42`）；
- 验证手段：运行 Scheduler、Graph 与 acceptance 的定向测试及全仓测试；涉及
  跨包装配时，再用真实二进制完成 implement → acceptance 冒烟并核对 Trace 事实。

## 4. 验收机制重设计（核心变更）

### 4.1 verifier 新形态（用户已定稿）

- **改为正向工具闭集**（`builtins.go:50-64`、`graph_control.go` 修改）：
  高智能 agent 持 shell 即存在"自行推断出通过方案"的失控空间；
  verifier 只保留 read / web 与 `submit_task_result`，写入、Shell、消息、发任务、
  用户交互、`request_replan` 及其它未知工具全部拒绝，
  **欺诈空间从"能做任何事但会被发现"降为"物理上几乎无能做恶"**。
  顺手关闭 KNOWN_ISSUES:78 的"verifier 可经 shell 污染被验收对象"有意接受风险。
- **保留 web 工具**（核验外部声明类认知判据需要，用户已拍板）；
- 保留 read 四件套 + web 两件套 + `submit_task_result`；
- **不新增 `inspect_task_calls` 专用旁路工具**（见 §4.2）；
- 不写报告（无写工具，现状已如此；prompt 再明写禁止声明 expected_artifacts）；
- `verifier.md` 重写：职责 = 读交付物 + 消费上游 Result / EvidenceRefs +
  用 web 核验公开事实 + 独立判断 + 诚实报告；基于 CLI 的独立检查交给实现节点
  下游 checker agent 执行，结果再经数据流进入验收；整段 evidence_items 格式
  契约删除；当前不开放 MCP，未来须有工具只读元数据后再扩闭集；
- 注意：模板变更改变 digest，旧动态 verifier team 重启恢复时标 stopped
  （07-21 起既有行为，冒烟时确认即可）。

### 4.2 验收证据数据流与外部能力边界（已拍板）

`inspect_task_calls` 专用工具提案作废：verifier 不应绕过 Graph 数据流，按
task_id 旁路枚举另一个节点的调用历史。验收证据按以下职责边界产生和消费：

1. **领域能力提供者 = MCP 或外部 CLI**：UE5 场景、数据库、云平台、交易环境
   等权威状态的查询与操作由对应 MCP / CLI 实现，AgentGo 不内建领域审计能力；
2. **编排与审计者 = AgentGo**：只记录自身可观察到的调用身份、状态、退出码、
   有界摘要及 artifact / evidence 引用，并在源 activation 结算时随完整 Result
   一并持久化；`ToolCallRecord` 保留为内部审计事实，不直接暴露成 verifier 工具；
3. **传递者 = Runtime 数据流**：实际生效的转移把源 activation 的 ResultRef、
   EvidenceEntry 与目标输入端口以持久化 InputBinding 绑定给 acceptance
   activation，恢复后绑定不变；
4. **裁判 = verifier**：消费自己的上游 Input，读取交付物并独立判断；公开网络
   事实用 web 工具核验，领域权威状态消费 checker agent 通过 CLI 生成的结构化
   结果与证据引用。当前 verifier 闭集不含 MCP；未来只有工具元数据可证明
   read-only 后才扩展。

引用身份统一使用稳定 EvidenceRef（内部记录可关联 CallID）；序数只作展示排序，
不得成为验收契约。结构化 EvidenceEntry 随 InputBinding 冻结并直接注入下游；
本轮不新增按 task_id 枚举或按 EvidenceRef 旁路读取的工具。超过注入上限或历史
绑定只有引用而没有内容时必须显式标为 unresolved，由 verifier blocked，不能猜测。

### 4.3 谁产生验收证据——三个合法来源

1. **实现者自己（仅证明调用事实）**：跑验证命令可以是实现工作的一部分，命令、
   退出码与 artifact/evidence 引用进入自己的 Result/Evidence；但 Evidence 不携带
   可作为验收权威的节点内 happens-before，不能证明该命令晚于同一节点最后一次
   写入，verifier 不得从展示顺序、CallID 或时间戳猜测新鲜度。
2. **verifier 的独立只读核验**：文件与代码事实由 read 工具核验，公开网络事实
   由 web 工具核验；当前不直接承载领域 MCP。
3. **独立 checker agent（需要可证明新鲜度时必用）**：需要 CLI / shell 执行，
   或判据要求证明检查发生在实现完成之后时，图必须建成
   `implement → checker → acceptance`。checker 是实现节点下游的普通 agent，
   无 `write_file` / `edit_file`，capability 只保留逐字执行指定检查所需的
   `run_shell` 与收尾工具；由 Graph 因果边证明先后，再把结构化 Result /
   EvidenceRefs 经数据流交给 verifier。

Runtime 直调声明式命令仍属二期候选（见 §5.3），本轮不把它作为正式验收的
依赖路径。

**git-clean 类判据：废除**。它从不是用户目标（07-21 系 Scheduler 过度解读），
verifier 碰不了 shell 后结构性不可执行；真有需要走声明式检查（二期）。
判据写作规范（§4.6）负责在写作阶段暴露"这条命令的成功到底证明什么"。

### 4.4 结算判定矩阵重写（acceptance.go / graph_acceptance.go）

新证据契约：verdict + summary 必填；verifier 可引用自己实际消费的稳定
EvidenceRef。服务端只核对引用是否来自该 acceptance activation 的上游 Input
谱系，或来自 verifier 自己获准执行的只读核验；不要求 Agent 逐字复述命令，
也不接受易漂移的展示序数作为身份。

业务 verdict 词表统一为 `pass` / `fixable` / `failed`。completed acceptance
结果必须省略 `event`，只通过 `$.verdict eq ...` 选择业务出边。
`status=blocked` 表示证据/能力不足，Runtime `failed` / `blocked` 事件只承担兜底，
不与业务 verdict 混用。`disputed` 由 Runtime 谱系核验产生，不是 verifier verdict。

| 情形 | 处置 | 对应历史 |
|------|------|----------|
| 引用的 EvidenceRef 越出当前 Input 谱系（引用身份造假） | **disputed 判死**（保留） | 07-20 伪造引用被抓的路径 |
| `required_inputs` 声明的目标输入端口尚未绑定 | acceptance 不进入 data-ready，不创建执行任务 | 数据流结构性约束 |
| `required_evidence` 要求的种类缺失或无法解引用 | 缺口随任务显式注入，verifier 应 blocked；即使自报 pass，Runtime 仍强制 blocked 并唤醒 Scheduler | 07-20/07-28 格式事故改由数据契约吸收 |
| 输入与证据充分，但 verifier 未额外调用 read / web | verdict 正常采信，不挂起、不附加专门审计标记 | 取消"零行为报 pass"错误维度 |
| verifier 任务出现 file_write/file_edit 账 | 规则退役——无写工具无 shell 后物理不可能 | KNOWN_ISSUES:78 关闭 |

红线纪律：**unverifiable 判死删除**是行为变更，KNOWN_ISSUES:90 的
"有意的保守默认"条目要同步改写。

### 4.5 程序性同步死循环保险丝（非业务预算）

`fixable → implement` 等跨任务回边不设 activation 次数上限；合法的长目标可以
产生任意多次 activation，次数只用于 Trace 观测。`Capability.Budget` 与
`budget.max_activations` 均不进入 schema。

Runtime 只保留不可配置的 **同步机械级联保险丝**：它限制一次 Runtime 调用内
router/join 等不经 Task 让出控制权的连续转移步数，防止定义错误造成进程栈递归
或 CPU 自旋；控制权一旦返回 Task、外部事件或等待点，计数即结束。触发时必须把
Graph 以明确原因 durable 结算并发 graph change 唤醒，不能留下无法恢复的
running 僵尸图。它是程序缺陷防线，不得被描述为验收重试或目标执行预算。

### 4.6 scheduler prompt 验收章节重写要点

- 判据三分法（写作规范）：
  1. **读工具可核验**（文件内容、代码结构——verifier 亲自读）；
  2. **上游证据可核验**（实现者或 checker 的结构化 Result / EvidenceRefs
     经 InputRef 数据流到达 verifier）；
  3. **外部权威状态可核验**（公开事实由 verifier 的 web 工具核验；领域 CLI /
     shell 由 checker agent 执行后回传证据；当前 verifier 闭集不含 MCP）。
- 每条判据必须写清证据提供者、EvidenceRef / artifact 形状和 verifier 的消费
  方法；写不清证明方式的判据不许建；
- acceptance 的 `task.title` 和 `task.description` 都必须非空，description 必须
  逐项写出上述验收标准；
- verifier verdict 只允许 pass / fixable / failed，业务分支一律使用
  `$.verdict eq ...`；completed 结果必须省略 event，也禁止 acceptance 的无条件、
  always、completed、pass/fixable 事件出边；
- 证据或能力不足时使用 `status=blocked`；`disputed` 仅由 Runtime 核验产生，
  verifier 不得把它当 verdict 提交；
- 核验失败处置指引补回（07-20 修复、V6 丢失）：disputed 先定位证据
  引用问题，不要机械重开验收；
- Evidence 只证明调用事实，不证明节点内最后写入顺序；需要可证明新鲜度时写成
  `implement → checker → acceptance`，以 Graph 因果边承担时序证明；
- 验收红线原样保留（§3.2）。

## 5. tool 节点语义反转

### 5.1 用户质疑（成立）

"既然只是一个命令，为什么不交由上游/下游节点由 Agent 执行？单独 tool 节点
执行完，结果都不知道是谁在消费。"

改造前的架构事实是：`taskSpecFor` 只从冻结节点定义取 Title/Description，
没有把上游 Result 织进下游任务描述。因而当时 tool 节点 Result 的消费路只有：

1. 边条件（eq/ne/in/exists 标量算子，对自由文本 content 近乎摆设）；
2. 下游 agent 在静态描述里被告知"先 read_graph 查 probe 结果"（笨拙间接）。

而 agent 在自己 ReAct 循环里调同一工具，结果直接落在自己上下文，信息流更直接。
本轮新增通用 Result→Input 绑定解决了跨节点交接，但没有改变“普通探测无需单列
tool 节点”的规划结论。

### 5.2 反转后的语义角色

tool 节点存在的理由不是"省一轮 LLM"，而是"**执行者不能是 Agent**"：

- 探测类用途**退场**：折进调查节点的 ReAct 循环（§2.1 调查节点的内部行为）；
  scheduler prompt 中 tool 节点的"探测范例"删除，明确反对探测用途；
- 语义角色重写为"**契约声明的确定性检查点**"，三权分立：
  - 契约作者 = Scheduler（建图时写明什么命令/查询的成功算证据）；
  - 执行臂 = Runtime（零 LLM、逐字执行、结果落账，物理上无法造假）；
  - 裁判 = verifier（不碰 shell，读交付物 + 读检查结果 + 审计记录，下 verdict）；
- 消费关系显式化：消费者是机械边条件（`$.exit_code eq 0`）或 verifier；
- 不做“孤儿 Result lint”：每条实际生效边由 Runtime 结构性绑定 ResultRef、
  输入端口与 Evidence，不再猜测下游是否可能消费；
- 二期放行 run_shell 时强制消费者声明：检查节点 task.description
  必须引用其服务的 acceptance 判据名，无消费者不准建。

### 5.3 声明式命令检查（二期，本次不实施）

tool 节点放行 run_shell 的治理问题（记录备查）：

- exec_mode 联动：readonly 模式 submit 时即拒；strict 审批 Interaction
  在 Runtime 结算路径同步执行时的弹出/等待语义需专门决策；
- Effect Journal 归属：账本按 task ID 记账，tool 节点无认领任务，
  需按 graph/node/activation 派生合成身份；
- 超时/输出截断/shell 方言：handler 复用 ShellGroup，接线是真改动。

## 6. 诚实的能力上限

- 机械层只能验证必需 Input / EvidenceRefs 已绑定、属于当前 activation 的输入
  谱系且可解引用，永远验证不了"**判断是对的**"
  （verifier 真跑了 go test 但把失败输出解读成 pass，任何机械层抓不到）。
  判断质量靠 verifier 模型本身 + Scheduler 在 graph_ended 前 read_graph 核对；
- AgentGo 只对自身可观察的工具调用和证据消费事实负责，领域状态是否真实由外部
  CLI（未来也可由带只读元数据的 MCP）提供；ToolCallRecord 与 trace 只作事后诊断，不为
  verifier 暴露专用查询工具，也不参与 verdict 门控；
- verifier 无 shell 后，CLI 型独立检查由 checker agent 执行；当前 MCP 未实现且
  工具名为闭集，未来必须先有 capability 元数据证明 read-only 才能扩展 verifier。

## 7. 实施影响面清单（定稿后执行）

| # | 改动 | 位置 |
|---|------|------|
| 1 | verifier 模板改为 read/web/submit 正向闭集，不新增 inspect_task_calls | `internal/agenttemplate/builtins.go:50-64`、`internal/tools/graph_control.go` |
| 2 | verifier.md 重写（消费 Result / EvidenceRefs + 独立只读核验） | `internal/agenttemplate/prompts/verifier.md` |
| 3 | 结算先写 activation 级完整 Result/Evidence，再让 Transition 持久化 ResultRef/InputBinding | `internal/graph/types.go`、`runtime.go`、`store.go`、`journal.go`、`recover.go` |
| 4 | Graph Task 上下文自动注入上游 Input、端口、完整小结果/ResultRef 与结构化证据 | `internal/graph/runtime.go`、`internal/bootstrap/graph_runtime.go`、`internal/agent/` |
| 5 | 结算判定矩阵重写（Input / EvidenceRef 充分性与谱系核验 / 删 unverifiable 判死） | `internal/graph/acceptance.go`、`internal/bootstrap/graph_acceptance.go` |
| 6 | Scheduler prompt 全面重写（§3 结构 + 验收证据数据流） | `internal/scheduler/scheduler.go:64` + promptVersion 递增 |
| 7 | tool 节点语义章节重写（探测退场、检查点定位） | 同上一并 |
| 8 | KNOWN_ISSUES 更新：:78 风险关闭、:90 保守默认改写 | `docs/activate/KNOWN_ISSUES.md` |
| 9 | 测试：Result→Input、结构化路由、输入端口 barrier、EvidenceRef 谱系、大结果与恢复、同步级联保险丝 | 各对应 _test.go |
| 10 | 交付冒烟：真二进制走 implement → acceptance 闭环并核对 Trace 事实 | 交付约定（AGENTS.md） |

跨平台纪律提醒（AGENTS.md 硬约束）：测试文件句柄先 Close 再 TempDir 清理；
路径只用 filepath.Join；行尾 LF；新输入通路建在 Bubble Tea MVU 内（本方案不涉及）。

## 8. 拍板记录

1. **metadata.category / Metadata（已拍板）**：本轮不引入
   `metadata.category`，§2.1 的可选标签提案作废，调查/变更/执行三分法只保留为
   Scheduler prompt 的认知纪律；现有 `Metadata` 暂不直接删除，因为
   `metadata.route` 仍承担节点认领路由，后续应将 `route` 升格为进入 digest 与
   正式校验的一等字段，删除无价值的 `metadata.title` 回退并迁移已有持久化图，
   最终删除通用 `Metadata` 字典以收缩模糊扩展面。
2. **数据流图语义 / 孤儿 Result（已拍板）**：目标是建立数据流图而非只有激活
   关系的控制流图，§5.2 的“孤儿 Result lint”提案作废；每条实际生效的转移都
   必须把源 activation 的 Result 以持久化 InputRef 绑定为目标 activation 的
   Input，controller / agent / acceptance 的执行上下文自动获得来源标识、目标端口、
   完整小结果或稳定 ResultRef 与结构化证据，router / end 直接消费输入，fan-out
   向各命中下游共享同一来源引用。join / acceptance 以 `required_inputs` 声明目标
   端口：并行必需来源使用不同端口，每个端口只有一条生产边；互斥分支各自保留
   后续与 `end`，不做共享端口 OR。大结果保存在 activation 级 durable Result Store，Runtime
   的 router / join / 恢复路径按引用精确解引用；需要 Agent 消费的超大正文以
   artifact 传递。恢复后输入来源、端口和证据谱系必须保持不变。消费关系由
   Runtime 与 schema 结构性保证，不再用 lint 猜测某个下游是否可能读取结果。
3. **预算护栏（已拍板删除）**：测试阶段不引入 `budget.max_activations`、累计
   token 或成本等正常业务终止上限，§4.5 的验收循环计数方案与实施清单中的对应
   改动作废，并删除当前只有形状校验、没有 Runtime 消费者的
   `Capability.Budget` 占位，避免形成预算已经生效的虚假契约；Graph 回边与节点
   activation 只记录用量供评测，不因预算自动终止；不可配置的 emergency fuse
   只限制一次 Runtime 调用内不让出控制权的同步机械级联，绝不按跨任务
   activation 总数熔断。生产阶段再依据测试数据决定是否引入默认关闭的显式预算
   策略。
4. **验收证据数据流 / inspect_task_calls（已拍板取消）**：不新增
   `inspect_task_calls` 专用旁路工具，也不再单独设计序数 / CallID 双标识和分页
   口径；AgentGo 只把经其编排通道可观察到的 Agent 工具、MCP / CLI 调用事实以稳定
   EvidenceRef 随源 activation 的 Result 持久化，并通过 Result→Input 数据流交给
   acceptance。领域证据与权威状态由外部 CLI（未来可含带只读元数据的 MCP）提供，
   verifier 消费 checker agent 通过 CLI 生成的结构化证据；
   CallID 可作为内部记录关联，序数只用于展示，大记录分页属于通用 Evidence
   Store 的读取纪律，不进入 Scheduler prompt。
5. **验收数据充分性 / "零行为报 pass"（已拍板取消）**：取消基于 verifier
   工具调用数量的 pass 护栏，既不挂起 verdict，也不因零调用添加专门审计标记；
   acceptance 只检查判据要求的输入端口与 `required_evidence` 是否完整绑定、属于
   当前 activation 的输入谱系且可解引用。必需端口尚未绑定时 acceptance 不进入
   data-ready；证据缺口随任务注入，verifier 应以 blocked 结算，若仍自报 pass，
   Runtime 强制 blocked；输入证据充分时，
   即使没有额外工具调用也允许提交 pass。ToolCallRecord 与 trace 仅用于事后诊断，
   不参与 verdict 门控。
6. **Scheduler prompt 重写（已拍板全量重写）**：以 §3.1 的统一 Graph 生命周期
   为骨架，并纳入已拍板的数据流语义、平面拓扑、验收证据、MCP / CLI 能力边界、
   tool 节点限制和无预算测试策略，整体重写 Scheduler prompt；不采用只修改决策
   序、验收和 tool 三节的最小补丁方案。现有 prompt 只保留 §3.6 所列安全红线、
   `patch_graph` revision CAS、在途 activation 定义冻结、Graph controller 控制面
   绑定、route / capability fail-closed 校验、Effect Journal 副作用恢复纪律和用户
   否定性约束，删除与新设计冲突的 D / G 双路径及旧范例。
7. **MCP（已拍板延后）**：本轮不实现任何 MCP 设施（现状：`internal/` 无 mcp
   包，`.mcp.json` 为空壳），acceptance 工具名按 read/list/grep/glob/web/submit
   正向闭集校验，不能仅凭自定义工具名声称“只读 MCP”而放行。"外部权威状态可
   核验"判据在本版的唯一承载是 checker agent（CLI / shell 执行后回传结构化
   证据）；verifier 独立核验覆盖文件/代码事实（read 工具）与公开网络事实
   （web 工具）。未来 MCP 落地时先增加统一 capability/effect 元数据，能够机械
   证明 read-only 后再显式扩展 acceptance allowlist 与 prompt。
