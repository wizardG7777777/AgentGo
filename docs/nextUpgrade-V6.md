# AgentGo nextUpgrade V6：七个核心层的系统升级

> 版本：V6 Draft
> 日期：2026-07-29
> 范围：LLM 调用、Prompt、Context/Memory、Harness、Agent Loop、Graph、Eval/Trace

这份文档是一份面向产品行为和系统架构的升级说明，不是字段级协议、代码改造清单或测试手册。它只回答三个问题：AgentGo 目前还缺什么、V6 准备怎样升级、升级完成后会得到什么。

AgentGo 已经具备多 Agent、ReAct、工具调用、Memory、Gate、Reactor、动态 DAG、验收和 Trace 等基础。V6 不从头重写这些能力，而是让它们形成一条边界清楚、可以恢复、能够验证的执行链：

```text
LLM 调用 → Prompt → Context/Memory → Harness → Agent Loop → Graph
                                                    ↘ Eval/Trace
```

这不是一组互相替代的层。Graph 负责组织多个节点及其局部 Agent Loop；Loop 仍依赖 Harness、Context、Prompt 和 LLM 调用完成实际工作；Eval/Trace 则横跨所有层，提供验证与审计证据。

V6 统一遵守以下原则：

- Prompt 负责表达目标和策略，不承担权限、安全与状态机的最终约束。
- Context 中的每条重要信息都应当有来源、优先级、新鲜度和预算。
- Memory 是可验证、可治理的数据，不是模型可以自行提升权限的系统指令。
- Harness 负责工具权限、副作用、隔离、审批与恢复等硬边界。
- Agent Loop 必须明确结束为完成、阻塞、失败或取消，不能用自然语言含糊收尾。
- Graph 传递可验证的数据、产物和证据，而不是依赖 Agent 自行理解一段拼接文本。
- Trace 记录发生过什么以及为何发生，但不替代 Task、Graph、Memory、Effect 和 Usage 等权威状态。
- Live 模型测试属于显式的开发活动；正常 Release 不携带大批兼容性测试资产与逻辑。

---

## 1. LLM 调用

### 现有不足

1. 当前实际只依赖 OpenAI-compatible Chat Completions，YAML 中的 `llm.provider` 却仍用于选择少量历史请求变换。只有一种协议时，这个字段没有独立价值，还会把供应商身份、接口协议和模型兼容性混为一谈。

2. “配置中声明了某个模型”“端点真实返回过结果”“该模型通过三项关键能力验证”还没有成为三个清楚分离的事实。模型名或配置成功不应自动意味着上下文窗口、价格和行为能力已经得到确认。

3. 普通启动、静态配置检查、网络可达性检查和会消耗 token 的真实请求之间边界不够鲜明。密钥和模型只有经过真实请求才能验证，但普通启动不应为了证明它们有效而自动产生费用。

4. `thinking`、`tool_use`、`structure_output` 是 AgentGo 真正依赖的三项关键能力，但目前缺少统一的基线验证流程。历史上的特殊分支可能没有对应的失败证据，也可能在统一请求路径已经可用时继续存在。

5. 节点级 model override 主要是一个运行时字符串。它可以改变请求模型，却没有同时冻结本轮调用所依据的配置、兼容处理和预算语义，重试或恢复时可能难以准确解释实际调用契约。

6. token usage、费用估算、端点报告费用和真实账单费用尚未完全分开。未知费率如果被当成零，会让预算看起来比实际更安全。

### 升级思路

1. V6 从 YAML 和运行时配置中删除 `llm.provider`，并删除由它选择不同请求变换的主流程分支。现有配置升级到 V6 时直接移除该字段；V6 若读到旧字段，应给出明确的迁移诊断，不能静默忽略或回退。字段缺失不表示“默认选择 OpenAI 供应商”，而是表示 AgentGo 当前只实现 OpenAI-compatible Chat Completions。

2. 在只有一种接口协议时，不预留协议选择字段。未来如果真正支持 Anthropic Messages 等第二种原生接口，再引入明确的协议字段，例如 `api_protocol`；不建议重新使用 `provider: anthropic`，因为供应商身份与请求协议不是同一个概念，同一模型也可能经第三方网关继续使用 OpenAI-compatible 接口。该扩展不属于 V6。

3. 把 LLM 相关事实分为三层：

   - 配置声明：用户希望调用哪个 endpoint 和模型；
   - 运行观测：某次真实 Task 得到了什么响应或错误；
   - 兼容资格：开发阶段对关键能力完成了可重复验证。

   三者各自记录，不能互相冒充。

4. 普通启动不发送 Chat Completion。启动硬条件收窄为全局配置能够解析、Scheduler 所需引用与控制通道能够装配，并且系统至少可以在 `topo=solo` 下进入可接收任务的就绪状态；不要求其它 Agent 的 Prompt 与权限先通过语义审计。无法形成有效执行 route 的其它 Agent 应标记为 unavailable 并告警，不能反过来阻止 Scheduler 以 solo 模式运行。密钥、模型和请求格式仍由首次真实 Task 或其它显式 Live 操作产生观测。

5. 所有模型先走统一请求路径验证 `thinking`、`tool_use`、`structure_output`。只有某项能力出现稳定、可归因、可复现的失败时，才为精确目标研发最小兼容补丁。

6. 兼容补丁必须在开发阶段完成基线、补丁试验和资格发布。普通 Task 的一次失败只能形成候选证据，不能在进程内热切换请求契约；同一 Task 的重试继续使用开始时冻结的调用方式。

7. V6 只对 OpenAI GPT 系列和官方 DeepSeek V4 系列做正式兼容验证。删除 DeepSeek-R1 的历史兼容契约，AgentGo V6 不支持第三方托管或自部署 DeepSeek-R1。未来兼容 Kimi、Qwen、Seed、Gemini 等家族时，仍遵循“统一路径优先、失败证据、最小补丁、开发期验证”的流程。

8. 价格独立于模型调用能力管理。计价字段与 USD 单位对齐 OpenRouter 的语义，配置和计算使用非负有限浮点数；免费必须显式为零，缺失价格保持 unknown。

9. 本地推算的 `estimated cost`、端点报告的 cost 和账单凭证必须分列。预算计算避免重复计入 cache 与 reasoning token；无法可靠分类或缺少费率时，不猜测、不按零处理。

### 预计结果

1. V6 配置不再包含冗余的 `provider` 字段；所有 endpoint 都明确使用同一种 OpenAI-compatible 调用协议。

2. 增加新模型时不再复制一套“供应商主流程”；只有真正增加第二种接口协议时，才新增对应的协议边界。

3. 系统能够准确区分“用户配置过”“真实调用过”和“开发阶段验证通过”。

4. 普通启动不会意外消耗 token，显式 Live 验证的成本和请求范围也可预期。

5. 兼容补丁只处理有证据的真实差异，不会因模型名称猜测而长期堆积。

6. 每次调用、重试和恢复都能说明自己使用了哪套冻结契约，以及 usage 和费用为何是已知或未知。

---

## 2. Prompt

### 现有不足

1. 当前 Prompt 由基础说明、Agent 角色、工具说明、Plan 规则和运行时片段逐步拼接，整体仍偏向手写长文本。内容来源、组合顺序和版本变化不够容易审阅。

2. Prompt 中承诺的工具或控制能力可能与某条实际运行 route 的 allowlist 不一致。模型被要求调用某个工具，不代表该节点真实拥有它。

3. 路径安全、写入规则、预算、审批和任务终止仍有一部分通过自然语言提醒模型遵守。模型忘记或误解这些规则时，框架不应随之失去约束。

4. Scheduler、普通 Agent、验收节点和不同运行模式需要的指令并不完全相同，但目前缺少对所有有效组合的一致检查，容易出现重复、矛盾或遗漏。

5. Trace 可以看到调用发生，却不总能准确说明这一轮使用了哪组 Prompt 组成部分。完整保存 Prompt 又可能带来用户数据和秘密泄露风险。

### 升级思路

1. 将 Prompt 视为一个有序编译结果，而不是一份不断增长的模板。它由少量职责明确的部分组成：基础协作契约、Agent 角色、任务目标、控制协议、工具说明、输出要求和必要的安全提醒。

2. 每个组成部分都具有稳定身份、版本和内容摘要。Task 开始时冻结最终 Prompt；执行过程中如果需要改变核心指令，应当通过新 Task 或 replan 明确发生，而不是静默替换。

3. 框架生成的工具说明必须来自节点真实可用的工具集，不能根据 Agent 名称或 Prompt 猜测权限。自由文本中的角色定位、工作方式和能力承诺不在启动期做不可靠的语义解析，而是在启动后的显式 Scheduler 审计中与实际运行能力比较。

4. 明确区分业务工具与控制工具。业务工具用于读写、搜索和执行；控制工具用于提交结果、声明阻塞、请求 replan、验收和结束任务。每种节点只获得完成职责所需的最小控制能力。

5. 将可强制执行的规则下沉到 Harness、Gate、状态机、预算和结构化结果校验中。Prompt 仍会向模型解释这些边界，但不再是唯一防线。

6. V6 增加启动后的 Scheduler 审计 slash 命令，暂定为 `/doctor agents`。该命令由 TUI/Web 的命令入口显式触发，为 Scheduler 创建一次只读审计任务，并向它提供每个 Agent 的身份描述、最终系统 Prompt、真实工具 allowlist、控制工具、运行模式和 route 状态。Scheduler 检查“Prompt 给予的身份和职责”是否与“实际拥有或缺少的权限”明显冲突，并向用户返回 warning，而不是修改配置或授予工具。

7. Scheduler 审计是显式、可选且可能消耗 token 的语义检查，不是启动门槛，也不冒充确定性的证明。每条 warning 应指出 Agent、冲突类型、相关 Prompt 摘要或片段以及实际权限证据；审计结果允许因模型判断不同而变化。它不能检查或修复 Scheduler 自身能否启动，Scheduler 与 solo 路径的最小装配仍由普通代码和开发期测试保证。

8. 离线评测负责验证审计快照、命令路由、warning 结构和权限不被自动修改；真实模型能否稳定识别身份—权限冲突则由开发阶段的 Live Behavior 评测观察。二者不阻止普通用户启动 AgentGo。

9. Trace 默认只记录 Prompt 组成、版本和摘要；完整正文只在显式、受控的开发诊断中保存，并进行敏感信息处理。Scheduler 审计记录命令发起、被检查 Agent、warning 摘要和所依据的能力快照摘要，不默认复制所有 Agent 的完整 Prompt。

### 预计结果

1. Prompt 会更短、更模块化，角色、工具和控制协议之间的关系更容易审阅。

2. 每次 LLM 调用都能回溯到明确的 Prompt 版本，修改前后也可以可靠比较。

3. 用户可以在系统启动后显式要求 Scheduler 审计其它 Agent，获得“身份要求高于实际权限”或“权限明显超出身份职责”等带证据 warning，而不必逐份人工对照 Prompt 和 allowlist。

4. 即使模型没有完全遵守文字说明，权限、安全、预算和任务终态仍由运行时保证。

5. 其它 Agent 的语义配置问题不会让整个系统无法启动；只要 Scheduler 的最小运行契约能够装配，AgentGo 至少可以使用 solo 模式接收和完成任务。

6. 审计只提供诊断，不会让 Scheduler 根据一次模型判断自动改写 Prompt、扩大权限、启用不可用 route 或停止正常运行。

---

## 3. Context / Memory

### 现有不足

1. Task 描述、依赖结果、历史、工具结果、Mailbox、Team snapshot、文件状态和 Memory 分别注入上下文，缺少一份统一清单说明模型本轮究竟看到了什么。

2. 不同来源的信息没有始终携带明确的权威等级、新鲜度和裁剪记录。旧 snapshot、未经验证的 Memory 或外部网页内容可能在文本上看起来和系统事实同样可信。

3. token 预算往往基于历史或估算值，而最终请求还包含系统说明、工具 schema 和动态上下文。只有对最终实际请求计量，才能真正避免上下文溢出。

4. 当前生产路径主要使用 Process 级 Context，承载 team snapshot 和 file awareness 等运行快照；这些内容更接近临时上下文，不是任务工作记忆。系统尚未形成短小、持续更新的 Task Memory，Session Memory 虽有持久化基础，也尚未形成稳定的学习、验证、召回和效果评估闭环。

5. Memory 缺少统一的候选、证据、过期、替代和遗忘机制。模型推测、过时经验或低可信信息一旦长期保留，可能反复污染后续任务。

6. 大型工具结果、历史压缩内容和 Skill 资料可能被重复复制进上下文，既消耗 token，也让真正重要的任务约束被淹没。历史压缩目前主要在缩短文本，没有把旧轮次稳定沉淀为下一轮可复用的 Task 工作状态。

### 升级思路

1. 每次 LLM 调用生成 Context Manifest。它记录各段内容的来源、作用域、权威等级、新鲜度、内容摘要、token 预算以及是否被裁剪、压缩或脱敏。

2. 建立稳定的上下文装配顺序和优先级。安全与控制契约、当前任务、有效约束和必要工具协议优先保留；历史、Memory、团队状态和外部资料按相关性与新鲜度进入剩余预算。

3. 以最终实际请求作为预算对象，工具 schema 和动态注入内容也必须计入。超出预算时采用可解释的裁剪策略，并在 Trace 中记录被保留和被舍弃的类别。

4. ReAct 历史保持真实时间顺序，assistant tool call 与对应 tool result 不拆散。已发生的历史只追加或形成带来源的压缩结果，不在后台悄然改写。

5. 建立完整 Memory 闭环：原始记录产生、Task Memory 更新、证据验证、Session 晋升、检索注入和效果评估。只有通过来源和证据检查的内容才能进入 Session 长期记忆。

6. V6 的语义记忆主链固定为“原始记录 → Task Memory → Session Memory”。Process 级 team/file/mode snapshot 继续作为可刷新的运行上下文，不参与记忆晋升；Project Memory 保持未来扩展，不在 V6 中自动接收 Session 内容。所有可晋升条目支持 confirmed、inferred、stale、superseded 和 forget 等状态。

7. 大型产物和工具输出优先保存为可验证引用，在上下文中提供摘要和定位信息。只有当前步骤确实需要时才展开正文。

8. 静态 Skill 指令属于 Prompt，按轮读取的参考资料属于 Context；两者都不能自动扩大工具权限。Memory 内容始终以带来源的数据出现，不能伪装成 system 指令。

#### 记忆架构与晋升机制

持久化归组采用以下层次：

```text
Session
├─ Session Memory
└─ Task
   ├─ Task Memory
   └─ Execution Attempt / Lease
      └─ Turn
         ├─ LLM Invocation
         ├─ Tool Call / Tool Result
         ├─ Effect / Artifact / Observation
         └─ Usage / Error / State Transition
```

Agent Loop 是产生多个 Turn 的执行过程，不单独充当记忆层；Execution Attempt / Lease 用于区分不同 Agent 接手、重试、恢复和冻结契约。原始记录保持不可变，Task Memory 是有界的滚动工作状态，Session Memory 是经过筛选的跨 Task 长期记忆。

| 层级 | 记录什么 | 什么时候写入或晋升 | 预计频次 |
|---|---|---|---|
| 原始记录层 | 用户输入；Prompt 与 Context Manifest 引用；模型公开文本与 tool call；ToolResult、Effect、Artifact、Usage、错误和状态迁移。模型私有 reasoning 不保存，超大内容保存受控引用 | 每次 LLM 调用、工具调用或副作用进入 settled/unknown 等稳定状态时追加；每个 settled Turn 结束后，从本轮记录提取 Task Memory 候选 | 原始记录每次调用或 Effect 一条或多条；Task 候选提取通常每个 settled Turn 最多一次，不按流式 token 或 UI snapshot 写入 |
| Task Memory | 当前目标与约束、工作阶段、已完成动作、confirmed 事实及证据引用、文件与产物版本、失败尝试、当前阻塞、待解决问题和下一步候选 | Task 开始时创建；settled Turn 出现实质变化时滚动更新；用户决定、重要 Effect、Artifact 变化、状态迁移、历史压缩前和 Attempt 结束前立即 checkpoint。重复读取或无新增证据的轮次不扩写 | 正常为每个 settled Turn 0–1 次更新；关键事件可立即更新；每个 Task 终态最多形成一次面向 Session 的晋升候选包 |
| Session Memory | 跨 Task 仍有价值的用户决定、稳定偏好、会话级约束、已验证 Task 结果、Artifact 引用、阻塞条件、后续 Task 需要继续处理的问题，以及旧结论的 supersede 关系 | 主要在 Task 进入 completed、blocked、failed 或 cancelled 终态后，从最终 Task Memory 和权威记录中筛选晋升；明确影响后续 Task 的用户决定可以立即晋升，不必等待当前 Task 结束 | 通常每个 Task 终态 0–1 次；跨 Task 用户决定每次一次；不因普通 Agent Loop 自动追加，也不在 V6 中自动晋升为 Project Memory |

Task Memory 必须有独立于 Loop 次数的固定预算。更新以替换、合并和 supersede 为主，不把每轮摘要继续追加到尾部。默认更新应直接使用结构化 Tool、Effect、Artifact 和状态事实，不为了“记忆一下”额外调用 LLM；只有超过预算、需要形成终态交接或开发期评测明确证明有价值时，才允许进行受预算约束的摘要整理。

LLM 文本只是原始记忆材料之一，不能自动成为 confirmed 事实。模型声称“文件已修改”或“测试通过”时，Task Memory 必须通过对应 File Effect、Artifact、命令结果或状态记录验证；没有证据的内容保持 inferred，不能晋升为 Session 的权威结论。

不同 Task 终态采用不同晋升规则：

- completed：晋升已验证结果、产物、用户决定和仍然适用的约束；
- blocked：晋升阻塞原因、已经尝试的方案、现有证据和恢复条件，不宣称任务完成；
- failed：只晋升可复现的失败证据、已排除方案和避免重复失败所需的信息；
- cancelled：默认不晋升中间推断，只保留已经发生的权威 Effect、明确用户决定和必要审计引用。

每轮 Context Assembler 优先注入完整但有界的当前 Task Memory，再选择与当前步骤相关的 Session Memory，并保留最近少量原始 Turn。更早的调用、工具结果和大型产物只提供引用，模型确实需要细节时再按需展开。这样循环轮数不再要求原始历史同步线性增长，但 no-progress、token、费用、工具和副作用预算仍然独立生效。

### 预计结果

1. 任一次 LLM Invocation 都可以关联到准确的 Context Manifest、Task Memory 版本、采用的 Session Memory 条目、保留的近期 Turn 和裁剪记录，系统能够回答“这一轮模型看到了什么、来自哪里、为何被采用”。

2. Task Memory 的长度由独立预算限制，不随 Turn 数量线性增长；较早的调用、工具结果和大型产物逐步转为可验证引用，使 Agent 可以继续更多有效循环轮次而不必反复携带全部历史。

3. Task 在重试、Agent 接手、Attempt 恢复或进程重启后，可以从 Task Memory、checkpoint 和原始记录引用恢复到最近一次已确认状态，减少重复读取、重复工具调用和重复副作用。

4. 每个 Task 终态最多形成一次 Session 晋升候选；明确影响后续 Task 的用户决定可以立即晋升。只有经过证据验证的结果、产物、约束和恢复条件进入 Session Memory，普通模型推测不会因多次出现而自动变成长期事实。

5. 正常 settled Turn 最多更新一次 Task Memory，流式片段、UI snapshot、重复读取和无新增证据的观察不会触发记忆晋升；默认更新使用结构化运行事实，不为每轮记忆维护额外发起 LLM 调用。

6. 原始记录、Task Memory 和 Session Memory 的权威关系清楚：原始记录不可变，Task Memory 可以版本化更新和 supersede，Session Memory 是经过筛选的跨 Task 视图；Process snapshot 不参与晋升，V6 也不会把 Session 内容自动写入 Project Memory。

7. Memory 条目始终携带来源、证据、状态和生命周期，支持 stale、supersede 与 forget；它作为带来源的数据进入 Context，不能伪装成 system 指令、修改 Prompt 或扩大 Harness 权限。

---

## 4. Harness

### 现有不足

1. AgentGo 已有 Agent kind allowlist、节点级 `NodeCapability.Tools`、Gate、路径检查、Roster、Interaction 和按 Task workspace 隔离，但这些能力仍分散在配置、路由、认领和执行环节。当前 Agent 工具列表既充当 Route 能力天花板，又会在节点未声明工具时退化为本次实际权限，尚未形成统一、冻结的“节点最小能力”执行契约。

2. 节点工具声明目前是可选的；缺省时继承 Agent 完整工具集。业务工具、任务完成协议和 Graph 控制能力也尚未完全按节点角色分离，可能出现普通节点控制能力过宽、Scheduler 通过工具名间接授予控制权，或节点缺少必要收尾通道。

3. Agent 身份、Prompt、执行 Route 和工具权限仍有较强绑定。为了得到不同工具组合而新增多个 Agent kind 会造成配置膨胀，但若彻底删除执行器能力上限，又会让 Scheduler 生成的节点声明同时变成不受约束的自我授权。

4. 路径和 workspace 约束主要保护 AgentGo 自身工具。当前实现已统一 canonical `project_root` 并在文件工具路径上解析现存 symlink/路径别名后做真实 containment；竞争替换（TOCTOU）、平台特有 junction/reparse point 以及 host Shell 命令正文仍需要更强的 OS 边界。用户批准命令也不等同于命令已经处于安全沙箱。

5. workspace 的本质是写时复制与冲突隔离，不是 OS 级安全沙箱。任务读取自己的修改、跨文件一致性和多文件合并仍需要更清晰的事务语义。

6. 文件写入、Shell、消息发送和 workspace 合并等副作用缺少统一、持久的执行账本。进程崩溃后，系统可能不知道操作尚未执行、已经完成，还是处于无法确认的中间状态。

7. Trace 能记录大量事件，但仅靠日志不能安全决定是否重放副作用。未知状态的 Shell 或外部消息如果被自动重试，可能造成重复执行。

8. 当前未知工具和错误路径已经有基础的 Did-You-Mean，但它主要解决名称相似问题。Gate、状态机、预算和结果校验产生的语义拒绝仍多为自由文本；有些错误会手写“请先读取文件”之类的提示，却没有统一的原因代码、可重试语义和安全下一步。模型只能自行猜测如何恢复，容易浪费循环和 token，也会因模型能力不同产生不一致行为。

### 升级思路

1. 保留 Agent 声明，但收窄其职责。Agent 继续声明 kind、Prompt、默认模型、replicas、循环参数和可承载的 Route；它不再拥有当前 Task 的实际工具权限。迁移期旧 `agents[].tools`、`agents[].profile` 和 `tool_profiles` 只解释为 Route/Executor Capability Ceiling，后续可迁移到名称更明确的 executor profile。

2. Graph 的节点、依赖、验收和 replan 流程保持不变。每个节点必须显式声明，或由可信的节点编译器确定性生成本次所需的业务工具、读写范围、workspace、模型、预算、输入输出和验收要求；节点工具缺省不再表示继承 Agent/Route 的全部能力。非 Graph Task 和 `topo=solo` 下的 Scheduler Task 视为单节点执行，同样生成 synthetic Node Capability，不能绕过节点权限模型。

3. Route/Executor Capability Ceiling 只回答“这个执行器最多支持什么”，节点声明只回答“本次任务需要什么”。节点声明不是权限授予；认领时由 Harness 计算：

   ```text
   ExecutionLease
     = Node Requirement
     ∩ Route / Executor Capability Ceiling
     ∩ System Policy
     ∩ Graph Role / Task State
     ∩ Mode / Approval / Gate
   ```

   任一必需能力无法满足时 fail-closed；没有匹配 Route 的节点进入明确的 blocked/replan 流程，不能长期隐藏在 pending 队列中。

4. ExecutionLease 在 Task 被认领时按 Execution Attempt 冻结，至少包含精确业务工具、由节点角色派生的控制工具、逻辑/物理 workspace 视图、读写范围、隔离与 Shell 策略、审批要求、预算、模型和终态协议。Lease 具有稳定摘要并进入 Trace；重试与恢复复用原 Lease，核心权限变化必须通过新节点或 replan。Task 进入 finalizing 或终态后立即撤销业务副作用能力。

5. 将业务工具、协调工具和 Graph 控制工具分开授权。业务工具来自 Node Requirement；`submit_task_result`、`request_replan`、验收和 Controller 工具等控制通道由 Graph Role 与 Task 状态派生，Scheduler 不能仅通过在工具列表中写入名称就把普通节点提升为 Controller。

6. Gate 和运行时校验成为 ExecutionLease 的权威执行者。Prompt 只解释允许做什么，不能扩大 Lease，也不能绕过路径、模式、预算、审批、Graph 角色和终态规则。

7. 在 Harness 内建立统一的 Suggestions 机制，将拒绝转换为结构化的恢复提示。提示至少说明稳定的拒绝原因、是否可以重试、可选的合法下一步，以及是否必须交给用户、replan 或终态处理。Suggestions 直接作为工具观察返回给 Agent Loop，不需要额外调用一次 LLM，也不自动执行任何建议。

8. 具体约束的执行者负责提供候选建议：例如 read-before-write 可以建议读取准确路径，版本冲突可以建议重新读取，缺少预期产物可以指出准确产物，普通节点越权修改 Graph 可以建议 `request_replan`。Harness 再根据当前 ExecutionLease、Task 状态和预算过滤建议；模型采纳后的动作仍需重新经过全部 Gate，Suggestion 本身不能扩大权限。

9. Suggestions 只在安全恢复路径能够确定时生成。readonly、待人工审批、任务已经 finalizing、外部副作用状态未知或不存在唯一安全方案时，不给出可执行动作，而是明确标记为需要用户处理、blocked、replan 或 terminal。建议数量保持有界；同一原因和目标连续失败时转入 no-progress 处理，不能形成“拒绝—建议—再次拒绝”的无限循环。

10. 明确不同隔离等级的真实保证：普通执行、workspace 冲突隔离、未来的 OS sandbox 分别描述。V6 不把 workspace 宣称为安全沙箱，也不因用户批准而夸大隔离能力。

11. 强化真实路径校验，并让读取、搜索、写入、产物检查和合并始终使用 ExecutionLease 中的同一个任务视图。任务可以看见自己的修改，其他任务在正式提交前看不见。

12. workspace 合并采用“完整预检、统一提交、失败可恢复”的流程。blocked 节点的 workspace 和产物保持未提交状态，不能被普通下游当成已完成输入。

13. 建立统一 Effect Journal。副作用执行前记录意图，执行后记录结果，并声明该操作是否可安全重放、需要核验、需要人工裁决或禁止自动重放。

14. 崩溃恢复以冻结的 ExecutionLease、Effect Journal 和实际外部状态共同裁决。无法证明幂等的未知 Shell、消息或外部操作不得静默重跑。

15. Interaction 继续承载高风险操作和人工审批；产物、证据与引用使用结构化记录。日志、Suggestions、ExecutionLease 和 Trace 默认脱敏，避免为了可观测性泄露密钥、Prompt 私密内容、工具参数或结果中的秘密。

### 预计结果

1. Scheduler 仍按正常 Graph 安排节点、依赖、验收和 replan；Agent 声明继续作为身份、路由和执行器定义存在，不需要为每种任务工具组合创建新的 Agent kind。

2. 每个 Task/Attempt 只使用其冻结 ExecutionLease 中的工具、路径、workspace、控制通道和预算；节点缺省不会继承整个 Route ceiling，权限错误会在认领或执行前暴露。

3. Route ceiling 继续提供路由资格与纵深防御，Scheduler 生成的节点能力声明不会变成不受约束的自我授权；控制工具也不能脱离 Graph Role 被任意授予。

4. `topo=solo` 和非 Graph Task 使用 synthetic Node Capability 与 ExecutionLease，不需要执行图也能工作，但不会绕过 Harness 权限模型。

5. workspace 冲突不会演变成未记录的部分提交，隔离等级也不会被误解或夸大。

6. 崩溃恢复不会盲目重复发送消息、覆盖文件或再次执行状态不明的 Shell；所有重要副作用都有可审计的意图、结果和恢复结论。

7. Harness 成为 Prompt、Agent 和 Scheduler 都无法绕过的执行边界，为上层 Loop 和 Graph 提供可靠基础。

8. 可恢复的拒绝会携带明确、合法且有界的下一步，减少模型猜测和无效重试；不可恢复的拒绝则准确升级到用户、blocked、replan 或 terminal，而不会伪造一条看似可行的建议。

9. Suggestions 不会扩大节点权限、自动执行副作用或形成无限恢复循环，并可以通过 Trace 与离线评测验证其实际效果。

---

## 5. Agent Loop

### 现有不足

1. 当前 ReAct Loop 已具备工具调用、最大轮数、取消、重试和历史压缩，但“模型说完成”与“运行时进入不可再产生副作用的终态”还不是同一个动作。

2. Agent 调用结果提交工具后，同一模型响应中排在后面的写文件或 Shell 调用仍可能继续执行。完成结果尚未成为严格的 finalization fence。

3. `completed` 与 `blocked` 的表达仍可能混杂在自由文本中。节点虽然报告了阻塞原因，最终却可能被记录为完成并错误放行下游。

4. replan 请求与 Task 终态没有形成完全持久的闭环。进程重启后，阻塞事实、待发送的 replan 信号和 Scheduler 的处理进度可能难以一致恢复。

5. Loop 缺少覆盖整轮调用的 durable checkpoint。崩溃后可能无法确定请求是否发出、响应是否落盘、工具是否执行、usage 是否结算或最终结果是否已提交。

6. `MaxLoops` 用固定轮数同时承担正常任务预算和失控保护。它既不能识别连续重复相同动作、反复读取同一内容或没有新增证据的语义空转，也会截断仍在取得有效进展的复杂任务。达到上限后的 TransferNote、回滚和重试还可能重复推理与消耗，而没有真正扩大任务的完成能力。

7. token、美元成本、工具调用、副作用、超时和重试尚未完全进入统一预算，最后一次 LLM 调用或 unknown usage 也可能使终态账目不完整。

### 升级思路

1. 使用统一的结构化任务结果，明确区分 `completed`、`blocked`、`failed` 和 `cancelled`，并携带摘要、产物、检查结果、证据、剩余风险和 replan 需要。

2. 增加持久化 finalizing 阶段。结构化结果一旦被接受，立即冻结业务工具；同一响应中剩余的副作用调用全部跳过，每个 Task 只允许一个终态提交者。

3. blocked 与 replan 意图作为同一收尾事务持久化。blocked 永远不满足普通依赖；Scheduler 即使在进程重启后，也能继续处理尚未消费的 replan 请求。

4. 为每轮建立 durable checkpoint，覆盖请求准备、可能已发送、响应记录、工具执行、Effect 引用、usage 结算和轮次关闭。恢复优先重放已记录事实，不重新询问 LLM 猜测之前发生了什么。

5. V6 预备删除面向用户的固定循环上限，包括 Scheduler、普通 Agent、Agent Template 和临时 Agent 上的 `agent_max_loops` 配置。Loop 不再因为到达预先猜测的轮数而终止，也不再执行“MaxLoops 耗尽 → 生成 TransferNote → 回滚同一 Task”的正常流程。旧配置迁移时应给出明确的弃用诊断，而不能继续静默影响运行。

6. 删除固定轮数不等于允许无边界运行。Loop 由结构化终态、用户或系统取消、Task deadline、token 与美元成本、LLM 调用、工具调用、副作用、重试和 no-progress 共同约束。每次模型调用先预留、再结算或释放；unknown usage 不能记为零，最后一轮必须先记账再进入终态。ExecutionLease 必须至少提供一种可强制执行的时间或资源边界。

7. Agent Loop 将 Harness 返回的 Suggestion 作为一种结构化观察，而不是必须执行的命令。模型可以采纳合法建议或选择其他允许动作；重复出现相同拒绝原因与目标时计入 no-progress。系统先提示 Agent 收敛，再限制高成本探索，最终要求提交完成或明确阻塞；持续无进展时由控制面结束本轮并请求 replan。

8. 循环次数继续作为 Trace、Eval 和周期性 checkpoint 的观测指标，但不再作为正常终止条件。系统可以每隔若干轮刷新 Task Memory、持久化 checkpoint 并检查进展；这里的轮数只是检查间隔。实现层可保留一个不可由普通配置调低的极高 emergency fuse，用于防御程序缺陷造成的真正死循环；触发时记录运行时异常并进入 `blocked/replan`，不得自动重跑相同 Task。

9. 重试必须复用 Task 开始时冻结的 Prompt、调用契约、兼容补丁和关键上下文策略。若这些契约确需改变，应当创建新节点或通过 replan 进入新 revision。

10. Loop 只负责单节点内部的思考、行动、观察和收敛。跨节点拓扑改变、依赖裁决和人工批准交给 Plan/Graph 控制面。

### 预计结果

1. 完成、阻塞、失败和取消语义互斥，Graph 可以可靠消费每个节点的终态。

2. 结果提交后不再发生新的业务副作用，blocked 节点也不会错误放行下游。

3. 崩溃后可以从 checkpoint 和 Effect Journal 恢复，减少重复 LLM 调用、重复副作用和重复计费。

4. 仍在产生有效进展的复杂任务不会仅因固定轮数耗尽而被截断或回滚；Task Memory、上下文压缩和 checkpoint 可以真正支持更长的 Agent 工作过程。

5. Loop 会在时间与资源预算内完成、明确阻塞并请求 replan，或由控制面终止。无进展循环和程序性死循环仍有明确的分级保护，不会因为删除 `MaxLoops` 而无限运行。

6. 用户最终看到的是明确、持久的任务结果，而不是需要自己从日志中猜测任务是否完成。

---

## 6. Graph

### 现有不足

1. 当前 `Plan` 同时表示 Scheduler 的规划过程、`gate=plan` 的执行前审阅模式、动态 DAG、运行状态、验收和恢复控制面。名称和职责混在一起，使“是否需要用户先审阅”被误认为一种图拓扑。

2. 当前图固定为 DAG，而 DAG 只是图的一种形态。它适合并行任务和一次性依赖，却难以自然表达条件路由、实现与验证之间的反馈、外部事件等待、人工决策和可复用子流程。

3. 当前节点以 Task 为中心，通过 `Dependencies` 和 Task UUID 建立关系。`publish_task` 会同时创建 Task 和登记图节点，图尚未完整时便可能已有部分任务进入队列，无法先对整张图进行原子校验。

4. 节点定义与运行状态混在同一 Plan 模型中，但两者变化频率和写入者不同。Scheduler 修改任务或拓扑、Runner 更新执行状态、Interaction 写入用户决定时，容易形成所有权不清或版本漂移。

5. 上游结果仍主要通过字符串和 Prompt 传递，缺少稳定的结构化结果、artifact/evidence 引用和明确的转移条件。增加更多图形态之前，如果不解决数据和状态语义，只会得到更复杂的依赖列表。

6. 一个 Task 终态后不能重新进入 processing，因此现有“一个节点等于一个 Task”的关系无法表达图中的回边。若直接重开旧 Task，还会破坏 Task Memory、Trace、Effect 和权限租约的审计边界。

7. 当前缺少面向整张图的 JSON 输入边界：语法、重复字段、根节点、引用、可达性、节点类型、状态迁移、扩展字段和写权限尚未形成一条统一校验链。

### 升级思路

1. V6 删除既有 Plan 设定，不再把 `Plan`、`PlanNode`、`PlanStore` 和 `gate=plan` 作为面向用户的核心概念。规划仍然是 Scheduler 的能力；执行前审阅改为独立的 review policy 或显式 Approval 节点。现有 revision、CAS、验收、暂停、恢复和用户决定能力迁移到 Graph，而不是随 Plan 名称一起删除。

2. 使用版本化 JSON `GraphDocument` 直接代表一张图。Scheduler 提交的 JSON 经过验证后就是 Graph Runtime 的执行契约，不再建立 `GraphDraft → GraphIR` 的第二条转换链。Markdown 和 Mermaid 只能由 JSON 生成并用于展示，不能反向解析为运行事实。

3. 每张图有且只有一个 `root`。root 必须指向真实节点，只有 root 能在图创建时自动激活，其他节点只能由边、事件或人工决定激活；所有有效节点必须能从 root 到达。root 可以直接完成 solo 任务，也可以由 Scheduler 从它向外建立团队执行图，因此“单 Agent 或多 Agent”与“图是什么形态”相互独立。

4. JSON Graph 的基本形态如下。节点通过 ID 引用，因此同一结构既能表达 DAG，也能表达条件分支和回边：

```json
{
  "schema": "agentgo.graph/v1",
  "graph_id": "graph-123",
  "revision": 1,
  "state_version": 8,
  "root": "root",
  "status": "running",
  "nodes": {
    "root": {
      "kind": "controller",
      "task": { "title": "完成用户请求" },
      "status": "running",
      "executor": { "type": "agent", "agent_id": "scheduler" },
      "execution": { "phase": "planning", "task_id": "task-root" },
      "next": [{ "to": "implement", "when": { "event": "ready" } }]
    },
    "implement": {
      "kind": "agent",
      "task": { "title": "实施修改" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": [{ "to": "verify", "when": { "event": "completed" } }]
    },
    "verify": {
      "kind": "agent",
      "task": { "title": "验证修改" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": [
        { "to": "finish", "when": { "path": "$.verdict", "operator": "eq", "value": "pass" } },
        { "to": "implement", "activation": "new", "when": { "path": "$.verdict", "operator": "eq", "value": "fixable" } }
      ]
    },
    "finish": {
      "kind": "end",
      "task": { "title": "形成最终结果" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": []
    }
  }
}
```

5. 每个节点至少描述：节点类型、任务目标、输入与输出、当前状态、执行者、执行阶段、能力需求、后续转移和结果引用。V6 首批支持 `controller`、`agent`、`tool`、`router`、`join`、`approval`、`wait_event`、`acceptance`、`subgraph` 和 `end`。其中 `end` 专门表示图的结束节点，避免与命令行终端混淆。拓扑可以自由组合，但未知节点类型、任意脚本条件或无法解释的执行语义必须拒绝。

6. Scheduler 通过结构化 `submit_graph` 提交初始 JSON，通过携带 `base_revision` 的 `patch_graph` 原子修改节点定义和连接。自然语言说明只供用户阅读，不参与执行。Graph 验证并提交成功前不得创建部分 Task；revision 不匹配时返回冲突和可执行的 Suggestions，由 Scheduler 基于最新图重新修改。

7. 不引入独立 IR Compiler。Graph 服务使用 Go 标准库检查 JSON 语法并解码为有类型的 `GraphDocument`，随后依次检查输入大小与嵌套深度、重复 object key、未知核心字段、唯一 root、节点 ID 与引用、从 root 的可达性、条件表达式、节点能力、状态迁移和调用者写权限。`json.Valid` 只能证明语法正确，不能替代这些图语义校验。

8. 核心字段保持严格，扩展能力放入节点的 `metadata` 与 `extensions`。`metadata` 只用于标签、展示和检索；已登记 extension 由对应 Registry 验证并执行；未知可选 extension 原样保留但不参与运行，未知必需 extension 直接拒绝。新增核心执行语义时升级 `schema`，避免把拼错的 `status` 一类字段误当成合法扩展。

9. 同一个节点对象可以同时展示定义和运行状态，但字段所有权必须固定：Scheduler 只写 `kind`、`task`、能力需求和 `next`；Graph Runtime 写 `status`；调度与认领系统写 `executor`；Agent Loop 和 Harness 写 `execution`、结果与 Effect 引用。所有写入经过 GraphStore CAS，Scheduler 不能通过整图覆盖伪造 `completed` 或占用者身份。

10. GraphStore 以内存中的有类型 `GraphDocument` 作为活跃 Graph 的主要读写对象。调度、状态查询、节点激活和 Graph 修改都访问内存，不在热路径中反复读取、解析或整体改写硬盘 JSON。JSON 是对外契约和持久化格式；进入进程后解码为等价的 Go 结构，必要时缓存序列化结果。

11. 硬盘上的 Graph 是恢复备份，不是正常运行时的查询数据库。GraphStore 使用每张图独立的锁或 CAS，在内存副本上应用、验证并形成新状态，再交给持久化机制同步。普通读取始终从已提交的内存快照获得；并发 Runner 不得各自读取文件后覆盖整张图。

12. 持久化采用“周期性完整 JSON snapshot + append-only 变更日志”。临时进度、流式文本和频繁 phase 更新可以合并、延迟同步；Graph revision、节点认领、Activation 创建、边选择、Join 结果、审批决定、节点终态、结果引用和副作用意图属于恢复所需的关键事实。关键事实虽然先在内存候选副本中计算，但必须在对外确认成功或触发下一步副作用前完成一次 durable append；否则崩溃后可能丢失已确认状态或重复执行。snapshot 使用临时文件、同步和原子替换，变更日志达到阈值后再压缩，不为每次读取触碰硬盘。

13. 启动恢复时先读取最近一份完整 snapshot，再按顺序重放其后的变更日志，重建内存 `GraphDocument`，校验 schema、revision、state_version 和摘要后才恢复调度。硬盘同步失败时 Graph 必须进入明确的 persistence-degraded 状态并向用户告警；系统不能一边声称可恢复，一边继续静默产生无法落盘的外部副作用。

14. `revision` 只在任务定义、节点、连接或执行要求变化时增加；`state_version` 在认领、进度、结果、审批和边选择等运行事实变化时增加。Graph digest 只覆盖会改变执行语义的字段，状态刷新不会制造新 revision，也不会使有效证据无故失效。

15. Graph Runtime 直接解释 JSON：激活 root，判断 `next` 条件，将目标节点置为 ready，并按节点类型创建 Task、确定性工具执行、Interaction、事件等待或子 Graph。Agent 节点获得 Harness 生成的 ExecutionLease，再由匹配的 Runner 认领；节点产生结构化结果后，Runtime 持久化边选择并继续激活后继节点。

16. 图中的回边不会重开旧 Task。每次重新进入节点都创建新的 `activation_id`、Task、Task Memory、ExecutionLease 和 Trace，例如 `implement@1`、`implement@2`。Agent Loop 负责一次 Agent Activation 内的连续工作；Graph 回边只表达跨节点的业务反馈。循环由结果条件、no-progress、blocked、用户取消或外部失败收敛，不重新使用固定轮数或默认累计 Token 硬上限截断正常进展。

17. 边选择、Join 结果、审批决定和节点激活都必须持久化。相同来源 Activation 的同一条边最多生效一次，可以使用 `graph_id + source_activation_id + transition_id` 作为幂等身份；恢复时重放已记录事实，不重新让 Scheduler 或模型猜测已经发生的路由决定。

18. 图可以呈现为顺序图、DAG、条件图、状态机式图、包含受控回边的反馈图、事件等待图或层级子图，但底层只保留一个 JSON Graph Runtime。系统扩展的是节点与转移语义，而不是为每种图分别建设一套执行引擎。

### 预计结果

1. 用户不再需要理解或选择 Plan 模式；每个请求都可以自然落在一张单根 Graph 上，简单任务保持单节点，复杂任务按需向外扩展。

2. Scheduler、Runner、Interaction 和恢复系统围绕同一份 JSON Graph 协作，不再同时维护 Markdown 计划、隐式 Task 依赖和另一份 IR。

3. 活跃 Graph 的读取、修改和调度都在内存完成，避免文件 I/O 成为执行热路径；硬盘 snapshot 与变更日志提供可校验、可重放的恢复备份，关键状态也不会因异步同步窗口而丢失。

4. DAG 不再是唯一拓扑。条件路由、并行汇聚、实现与验证反馈、人工批准、事件等待和子图都能得到直接表达。

5. 每个节点都能回答“任务是什么、当前状态是什么、谁在执行、执行到哪一步、结果在哪里、下一步为何被激活”。

6. 图定义变化和运行状态变化不会互相污染；旧 revision、旧 Activation、旧结果和旧验收证据都有稳定归属，崩溃恢复也不会重复触发已经生效的边。

7. JSON 核心协议严格而可诊断，额外展示信息和未来能力可以通过 `metadata`、`extensions` 与 schema 升级持续扩展。

8. Graph 只负责组织不同节点及其局部 Loop，不取代 Prompt、Context、Harness 和 Agent Loop 的职责；节点级能力仍由 Harness 转换为可强制执行的 ExecutionLease。

---

## 7. Eval / Trace

Eval 用来回答“升级是否真的改善了系统”，Trace 用来回答“一次运行实际发生了什么，以及为什么发生”。二者横跨前六章，但都不能成为第二套运行状态。V6 对本章设置一条明确的完成标准：

> 前六章新增的稳定事实，如果没有可关联、可脱敏、可查询、可测试的 Trace，就不算完成；已经删除的机制，如果仍在新 Trace、CLI、Dashboard、Reactor 订阅或 Eval 指标中出现，也不算删除完成。

### 现有不足

1. 当前 Trace 是按 Task 分片的 JSONL，Writer 遇到空 `TaskID` 会直接丢弃事件。Graph 创建、Graph 校验失败、等待外部事件、Join、恢复和 Graph 结束等事实不一定属于某个 Task，因此现有存储模型无法完整承载第 6 章的 Graph。

2. 当前 `trace.Event` 是一份持续扩张的可选字段大结构，主要依靠 `TaskID + AgentID + Loop` 关联。它没有 Session、Graph revision、Node、Activation、Attempt、ExecutionLease、Turn、Invocation、Context Manifest、Effect、Interaction 和 Usage settlement 等稳定身份；重试后 Loop 序号还会重新开始。

3. 当前 LLM Trace 主要只有 `llm_call_start/end`、历史条目数、工具数、耗时和 prompt/completion token。它不能证明请求模型、冻结调用契约、Prompt 编译结果、Context Manifest、兼容补丁、响应观察和 Usage 结算属于同一次 Invocation；错误调用的未知 usage 也容易表现成零。

4. 当前 Context/Memory 侧主要只有历史压缩、截断和 `memory_context_inject`。后者只覆盖 team snapshot、file awareness 等 Process 级临时上下文，尚不能观察 Task Memory 的版本、checkpoint、证据状态、Session 晋升、召回、stale、supersede 和 forget。

5. Harness 侧可以看到部分工具、文件、workspace 和 Shell 结果，却看不到实际冻结的 ExecutionLease、Gate 的稳定拒绝码、Suggestions 及其后续处理，也没有统一的 Effect `prepared → settled | unknown` 事实链。只根据执行后日志无法安全判断未知副作用能否重放。

6. 当前 `trace.Emit` 同时负责写审计 JSONL 和向 Reactor 派发业务事件。ReadSet、Artifact、Task 收尾、Team 和 Spawn 等运行逻辑会消费这些事件；如果 V6 直接删除旧字段或对参数脱敏，可能反过来破坏控制流程。这说明“领域事件”和“审计投影”目前还没有真正分开。

7. 工具 Trace 可以保存完整 `Args`，Shell Trace 可以保存命令和输出摘录，显式 Prompt dump 还会保存完整 messages 与 response。它们对开发排障有用，但不适合作为默认生产 Trace，更不能与普通脱敏审计记录共用相同保留和访问策略。

8. 当前事件和 CLI 仍内置 `replan_*`、`plan_revision_changed`、`plan_paused`、`plan_terminal`、`PlanTraceContext`、`trace plan` 和 `stats plan`；Loop Trace 也仍把 MaxLoops 作为 retry 与失败原因。这些都与第 5、6 章已经决定删除的固定循环上限和 Plan 控制面冲突。

9. 当前 `agentgo eval` 直接编入普通 AgentGo 二进制，`run/record` 会先探测真实端点并使用真实模型，尚没有不联网的 fake-LLM Offline E2E 主链。与此同时，suite、fixture、baseline 和报告整体位于被忽略的 `eval/` 目录，无秘密的行为契约也无法随代码版本审阅。

10. 当前 baseline 只按模型、Prompt hash、配置 hash 和 Git commit 区分，每个 case 主要是单次运行与固定比例带。Eval 还用最大 Loop 序号近似循环数，用 `task_published` 统计 DAG Subtasks，用 `replan_requested` 统计 Replans；这些指标既不精确，也延续了 V5 的 Plan/DAG/MaxLoops 语义。

11. Trace 写入失败目前只告警，Eval 收割又允许跳过损坏文件。若没有独立的完整性状态，`event_absent` 可能只是证据丢失，却被误判为“禁止行为没有发生”。

### 升级思路

#### 7.1 先分开领域事件与 Trace

V6 将 Trace 定义为“权威事实的脱敏审计投影”，不再把 Trace 事件本身当成业务状态或 Reactor 的唯一输入。

```text
权威 Store / Journal 成功提交
                │
                ├─ 完整 Domain Event / Outbox → Reactor 与运行时响应
                │
                └─ 脱敏 Trace Projection      → CLI / UI / Eval / 排障
```

1. Task/Attempt 状态以 TaskStore 和 checkpoint 为准；Graph JSON、revision 与 `state_version` 以 GraphStore 的内存状态、snapshot 和变更日志为准。

2. Prompt 与 Context 以各自 Manifest 为准；Memory 以 MemoryStore 为准；权限以 ExecutionLease 为准；副作用以 Effect Journal 为准；Usage 与费用以 Usage Ledger 为准；人工决定以 Interaction 为准；公开回复以 Turn 账本为准。

3. Trace 只保存这些权威记录的稳定 ID、版本、摘要、状态变化和受控引用，不能用一条日志伪造完成、恢复 Graph、重放 Effect 或晋升 Memory。

4. Graph revision、Activation、边选择、Join、审批、节点结果和 Effect intent 等关键事实，必须先完成对应权威日志的 durable commit，再产生 Trace 投影。Trace 的时间戳不取代 Graph `state_version`、Attempt/Turn 序号和 Effect 状态顺序。

5. Trace 写入失败不回滚已经提交的业务事实，但必须将运行标记为 `trace_degraded`，向用户显示告警，并让依赖 Trace 的 Eval 结果进入 `trace_incomplete`。Reactor 仍从完整 Domain Event 接收事实，不能依赖脱敏后的 JSONL。

#### 7.2 统一事件信封与存储范围

V6 不再让所有事件共享一份不断扩张的宽结构，而是使用小型版本化 Envelope 加有类型的 payload：

```text
trace_schema / payload_schema
event_id / kind / occurred_at / recorded_at / scope_sequence
session_id
graph_id / graph_revision / graph_state_version
node_id / activation_id
task_id / attempt_id / lease_id
turn_id / invocation_id
tool_call_id / effect_id / interaction_id
prompt_build_id / context_manifest_id / memory_ref / usage_settlement_id
actor_type / actor_id
causation_id / correlation_id
authority_ref / authority_version
payload_digest / redaction_policy / payload
```

并非每条事件都要填写所有 ID，但每条事件必须声明自己的作用域，准备、执行、结算、恢复等阶段必须复用同一组稳定身份。Writer 不再因为缺少 `TaskID` 丢弃 Graph、Session、Interaction 或 Invocation 事件。

物理存储仍可使用 Session 下的分段 JSONL，但分片只是一种落盘方式，不再代表逻辑归属。CLI 通过 Envelope 的关联 ID 聚合，不依赖文件名猜测一张 Graph 或一次 Attempt。每个分片记录 schema、序号范围、校验摘要和关闭状态；Eval 只读取当前 `run_id + suite_case_id + graph_id` 的完整 bundle，不再扫描并混合所有 Session 与 legacy JSONL。

流式 token、UI snapshot、普通进度刷新和尚未提交的 Memory 候选不逐条进入 durable Trace；它们在形成 settled Turn、Memory checkpoint、Effect 状态或 Graph 状态变化后再记录稳定事实，避免 Trace 自身成为新的高频负担。

#### 7.3 前六章的 Trace 覆盖

| 层 | V6 需要记录的稳定事件 | 必须能够回答的问题 |
|---|---|---|
| LLM 调用 | `llm_invocation_prepared/dispatched/observed/settled/failed`；`usage_reserved/settled/unknown` | 哪个 endpoint hash、requested model 和冻结 Chat contract 被使用；绑定了哪个 Prompt/Context；请求是否可能已发送；响应形态、错误分类、finish reason、usage 和费用状态是什么 |
| Prompt | `prompt_compiled`、`prompt_bound`、`agent_audit_started/warning/completed` | Scheduler 或某类 Agent 的有序组件、版本、摘要和最终 Prompt digest 是什么；`/doctor agents` 根据哪份能力快照提出了什么 warning；审计是否仅告警而没有修改权限 |
| Context / Memory | `context_manifest_built`；`task_memory_created/updated/checkpointed`；`session_memory_promotion_proposed/decided`；`memory_recalled`、`memory_entry_state_changed` | 一次 Invocation 实际采用、裁剪或转为引用了哪些来源；Task Memory 是哪个版本；某条 Session Memory 为何晋升、拒绝、stale、supersede 或 forget |
| Harness | `execution_lease_frozen/rejected/reused/revoked`；`gate_decision`；`suggestions_returned`、`suggestion_disposition`；`effect_prepared/dispatched/settled/unknown/recovery_decided`；Interaction 与 workspace 状态事件 | Node Requirement、Route ceiling、Graph role、模式与系统策略如何形成实际权限；操作为何被拒绝；给了哪些合法建议；建议是否被显式采用；副作用是否发生、是否未知、恢复时为何重放或不重放 |
| Agent Loop | `attempt_started/resumed/ended`；`turn_started/settled`；`loop_checkpoint_committed`；`task_finalizing`、`tool_call_skipped`、`task_result_committed`；`no_progress_observed/escalated`、`runtime_loop_fuse_triggered` | 当前是哪个 Attempt、Lease、Turn 和 checkpoint；完成声明后哪些尾随工具被 fence 拦截；completed、blocked、failed 或 cancelled 是由哪次权威收尾事务提交；是否持续有进展 |
| Graph | `graph_submitted/submission_rejected`；`graph_revision_committed/patch_rejected`；`graph_change_requested/decided`；`node_activation_created/state_changed`；`graph_transition_selected`、`graph_join_resolved`、`graph_approval_decided`、`graph_wait_started/resumed`；`acceptance_completed`；`graph_snapshot_written/recovered`、`graph_persistence_degraded/recovered`、`graph_ended` | 哪一版 JSON Graph 被接受；哪个 Node 的哪次 Activation 创建了 Task/Interaction/Effect；哪条边为何生效；Join、Approval、Acceptance 与恢复如何裁决；Graph 是否到达 `end` |

进一步约束如下：

1. LLM 事件只记录 endpoint 的稳定引用或 hash、requested model、OpenAI-compatible Chat contract digest 和 CompatibilityPatch digest，不记录或推断 `provider`。Prompt 正文、模型私有 reasoning 和原始敏感响应不进入默认 Trace。

2. Usage 以 Invocation 为单位记录 prompt、completion、cache、reasoning 等实际可类型化类别。`estimated_cost`、`provider_reported_cost` 和 `billing_receipt_cost` 分列；未知 usage、未知货币或未知价格保留 unknown，不得写成零。累计统计由 settlement 推导，不再额外维护可能漂移的 Agent 累计值。

3. `prompt_compiled` 保存有序 component ID、version、digest 和最终摘要；Invocation 只引用冻结的 `prompt_build_id`。工具说明摘要必须来自实际 ExecutionLease，不能用 Agent 声明的上限冒充本次权限。

4. `context_manifest_built` 保存每类信息的 source、scope、authority、freshness、digest、included/dropped、裁剪原因和 token 估算。Process snapshot 作为 Context item 记录，不进入 Task → Session 的记忆晋升链。

5. Suggestion 必须有稳定 ID、拒绝原因码、retryable、候选 action 与过滤结果。`suggestion_disposition` 只记录模型通过结构化引用明确采纳、放弃或再次触发相同拒绝的事实，不能根据自然语言相似度猜测“模型大概采纳了”。

6. Effect 使用同一 `effect_id` 贯穿 prepared、possibly dispatched、settled、unknown 和 recovery decision。默认 Trace 保存操作类别、目标摘要、幂等策略和证据引用，不保存完整工具参数、Shell 命令或秘密。

7. `task_result_committed` 必须由 Task 的同一收尾事务直接给出结构化状态，不能从 `tool_result` 或自由文本推断 blocked。Graph 只消费这份已提交结果，旧完成节点和旧 Activation 始终保持原状态。

8. 每次 Graph 回边创建新的 `activation_id`、Task、Task Memory、ExecutionLease 和 Attempt。边事件携带 `graph_id + source_activation_id + transition_id` 作为幂等身份；Graph 恢复后不得重复触发已经生效的边。

9. Graph 关键事件只保存 patch 摘要、revision、`state_version`、结果引用和权威 journal record，不复制整份 Graph JSON。硬盘 Graph 日志负责恢复，Trace 只负责解释恢复前后发生了什么。

#### 7.4 明确停止跟踪的旧内容

V6 的新 Writer、CLI、Dashboard、Reactor 订阅与 Eval 聚合必须同时停止使用下列旧语义：

| V5 内容 | V6 处理 |
|---|---|
| `llm.provider`、供应商 cohort、DeepSeek-R1 兼容身份或资格 | 不新增任何生产 Trace 字段或事件；只记录 endpoint hash、requested model、调用契约与已经发布的最小 patch digest。GPT/DeepSeek V4 的资格证据只存在于开发工具 |
| `replan_requested/coalesced/decided`、`plan_revision_changed`、`plan_paused`、`plan_terminal`、`PlanTraceContext` | 停止新发射；分别由 Graph change、revision、wait/approval、`graph_ended` 和 Graph Envelope 取代。Acceptance 保留，但改为 Graph/revision/Activation 归属 |
| `trace plan`、`stats plan` 和 Dashboard 的 Plan 维度 | 删除当前入口，改为 `trace graph`、`trace activation` 与 Graph 统计 |
| 图节点或事件中的 `terminal` 命名 | Graph 节点只使用 `end`，结束事件只使用 `graph_ended` |
| MaxLoops 导致的 `task_retry`、`react_loop_exit:max_loops`、`max_loops_exceeded` 和 TransferNote 回滚 | 停止发射和统计；Turn 数只作为观察和 checkpoint 间隔。极高 emergency fuse 触发时记录 `runtime_loop_fuse_triggered` 并进入 blocked/Graph change，不自动重跑原 Task |
| 默认累计 token 上限导致的普通 Loop/Graph 截断 | 停止作为默认结束原因；usage 和费用仍完整记录。只有用户显式启用的保护策略才能产生对应 policy decision |
| `memory_context_inject` 独立事件 | 其信息并入 Context Manifest；Process snapshot 仍可见，但不再被误认为长期 Memory |
| 绑定固定 `context_limit` 的 `history_truncated` | 改为 Context Manifest 中的选择、压缩、引用和排除原因；上下文适配仍可观察，但不再表达已删除的固定配置硬限 |
| Agent 级 `token_stats` 累计事件 | 改为 Invocation 级 Usage reservation/settlement/unknown；Task、Node、Graph 与 Session 汇总从账本推导 |
| Task `Dependencies`、`Subtasks` 和 `Replans` 作为整张图的拓扑与核心 Eval 指标 | Graph 拓扑只认 Node、Transition、Join、Router 与 Activation；指标改为 Activation、Graph change、committed revision 和 transition。Task 前置关系若仍用于执行，不得冒充完整 Graph |
| 默认 Trace 中的完整工具 `Args`、Shell command/output 和完整 Prompt dump | 改为 schema-aware redaction、摘要与受控引用；完整诊断资料只进入显式开发通道，使用独立开关、目录、权限与保留策略 |

V6 可以保留只读 legacy decoder 以查看旧 JSONL，但必须显示 `legacy / coverage=partial`，不能把旧 Plan 事件自动解释成新的 Graph 事实，也不能将它们并入 V6 Dashboard、Reactor 白名单、baseline 或 Release gate。能读取历史不等于继续跟踪旧机制。

#### 7.5 Trace 查询入口

生产 Release 保留轻量、脱敏的 Trace 查询能力：

```text
agentgo trace list
agentgo trace show <task_id>
agentgo trace graph <graph_id>
agentgo trace node <graph_id>/<node_id>
agentgo trace activation <activation_id>
agentgo trace invocation <invocation_id>
agentgo trace explain <event_id|activation_id|task_id>
agentgo trace stats [session|graph|node|agent|model]
```

`trace graph` 按 revision 与 `state_version` 展示 Node/Activation/Transition/Join/Approval/Acceptance/恢复时间线；`trace explain` 沿 `causation_id`、`authority_ref` 和相关 Manifest 说明“为何路由、为何拒绝、为何裁剪、为何恢复”。每个视图都显示 `complete / partial / degraded`，避免把缺失证据误当成没有发生。

#### 7.6 分层 Eval 与 Release 边界

| 评测层 | 主要验证内容 | 是否联网或调用真实模型 | 普通 Release 是否携带 |
|---|---|---:|---:|
| Contract Tests | schema、状态机、字段所有权、Prompt 顺序、Context/Memory 晋升、Lease/Gate/Effect、Graph CAS 与恢复不变量 | 否 | 不携带测试二进制与 fixture |
| Offline fake-LLM E2E | 用 scripted/fake endpoint 驱动真实 Prompt → Context → Harness → Loop → Graph 主链，验证 Trace 事实与禁止行为 | 否 | 否 |
| Recovery / Platform / Security | Effect 崩溃窗口、Graph journal 重放、Trace 缺口、race、symlink/junction、Windows/macOS/Linux 差异 | 否 | 否 |
| Live Behavior | 真实模型完成代表性任务的成功率、里程碑、无进展、重复副作用、usage、时延与费用 | 是 | 否 |
| Live Compatibility | 仅验证 `thinking`、`tool_use`、`structure_output` 及确有失败证据的最小补丁 | 是 | 否 |

1. 从普通 AgentGo 二进制删除 `eval preflight/run/record` 路由。Contract 与 Offline E2E 由开发/CI 测试运行；Live Behavior 与 Compatibility Lab 进入独立开发工具 `agentgo-eval`，不作为正常 Release 产物发布。

2. 正常 Release 只保留生产 Trace 查询器、权威运行账本和已经资格化的最小兼容补丁；不包含 compatibility suite、sentinel tool、judge、fixture、trial 状态机、baseline 录制器或真实模型跑批命令。普通启动不得为了评测产生 LLM Invocation。

3. 无秘密的 suite、fixture、fake-LLM 脚本、judge schema 和 accepted baseline manifest 进入开发/CI 的版本管理，但由构建和打包规则排除在 Release 外。密钥、原始模型响应、用户数据、完整 Prompt、完整 Trace 和机器缓存继续保存在仓库外或明确忽略的运行目录。

4. Offline case 使用确定性结果和精确断言。Live Behavior 对同一 case 运行多个 rollout，报告成功率、语义里程碑、禁止行为、结构化结束、重复 Effect、no-progress 以及中位/P95 的调用、usage、时延和费用；不再用单次结果与固定 ±30% 掩盖随机性。

5. Live 入口在执行前展示请求上限、completion token 上限、预算与可能费用。GPT 与官方 DeepSeek V4 的三功能兼容性只在开发阶段验证；缺少凭证、未运行、目标不在资格范围或证据过期都不能默认为 pass。

6. `record` 只生成 baseline candidate。只有必需 case 全部运行、Trace 完整、结果满足准入条件并经过显式 review/promote 后，才能成为 accepted baseline；失败或不完整运行不能覆盖现有基线。

#### 7.7 Fingerprint、比较与指标

完整 Run Fingerprint 用于复现来源，至少记录：

```text
suite/case/judge digest
build provenance + dirty state
OS / architecture
api_protocol=openai-compatible-chat-completions
endpoint hash + requested model + typed request parameters
Chat contract digest + CompatibilityPatch digest
Prompt build digest
Context policy + Memory policy digest
ExecutionLease / effective tool contract digest
Graph schema + relevant Graph digest
Usage policy + AccountingProfile digest
```

`provider` 不进入 fingerprint。Git commit 是 provenance，不应因为候选版本与 baseline 的 commit 不同就直接跳过回归比较。

比较时再从 Run Fingerprint 中选择当前指标必须相同的 cohort，并显式声明本次有意改变的 variant。例如：行为正确性不因价格表变化失去可比性；时延要求平台和运行器接近；token 要求模型 usage 口径与调用契约一致；美元成本要求 AccountingProfile 一致。未声明的关键差异报告为 `not_comparable`，而不是强行混组或静默跳过。

评测结果使用有类型状态：

```text
pass / fail / skipped / blocked / unqualified / not_comparable / trace_incomplete
```

`skipped`、`unqualified` 和 `trace_incomplete` 都不能满足对应 Release gate。基于 Trace 的 judge 必须先验证 schema、事件 ID、scope sequence、分片摘要、开始/关闭状态和 writer health；证据不完整时不得用 `event_absent` 判定通过。

核心准入指标关注：

- 结构化 completed/blocked/failed/cancelled 是否正确闭合；
- 必需 Artifact、Effect 和 Acceptance evidence 是否存在；
- 禁止工具、越权路径和 finalizing 后副作用是否为零；
- settled Effect 与已生效 Graph transition 在恢复后是否恰好一次；
- Graph 是否按预期到达 `end`，Join、Approval、wait 和回边是否正确；
- Memory 晋升是否有证据，inferred 是否没有越权变成 confirmed；
- Suggestions 是否不扩大权限、不自动执行，并避免重复拒绝循环。

LLM 调用数、Turn 数、wall time、usage、费用、Suggestion 采用率、no-progress、Graph revision、Activation、transition 和等待时长保留为观察与同 cohort 回归指标。Turn/Loop 数不再是默认结束条件或成功判据；旧 `Replans`、`Subtasks` 和 Plan 维度不再进入 V6 指标。

#### 7.8 Trace 覆盖作为升级验收项

每项 V6 新能力合并前必须同时具备：

1. 有版本的事件 payload、稳定关联 ID 与默认脱敏策略；
2. 在权威事实提交后产生 Trace 投影，且不会因 Trace 失败改变业务结果；
3. 至少一个 CLI/UI 查询入口能够沿关联 ID 找到该事实；
4. Contract 或 Offline E2E 验证事件存在、顺序、引用、恢复行为和敏感字段缺失；
5. Eval 能区分事实没有发生、Trace 不完整和旧 schema 无法证明三种情况。

删除旧机制时必须同时完成：

1. 删除生产 emitter 和 Domain Event 依赖；
2. 删除新 schema 中的旧字段、事件常量与 Reactor 订阅；
3. 删除 CLI/Dashboard 的当前视图和 Eval 聚合指标；
4. 增加“V6 不再发出该事件”的 absence test；
5. 如需读取历史，只保留隔离的 legacy decoder，不把旧事实迁入当前指标。

### 预计结果

1. 从一次用户输入可以沿 Session → Graph → Node/Activation → Task/Attempt → Turn/Invocation → Tool/Effect → Result/Acceptance 找到完整且脱敏的因果链。

2. Prompt 编译、Context 裁剪、Memory 晋升、ExecutionLease、Suggestions、Loop checkpoint、Graph 路由和恢复都具备稳定证据；同时 Trace 不替代任何权威 Store 或 Journal。

3. V6 新运行不会再产生 Plan、MaxLoops、provider、DeepSeek-R1 资格和旧 DAG 指标；历史记录仍可隔离查看，但不会污染当前 Dashboard、Reactor、Eval 或 Release gate。

4. 大多数回归可以在不联网、不消耗 token 的 Contract 与 Offline E2E 中发现；Live Behavior 和 Compatibility 成为显式、限额、可说明费用的开发活动。

5. 正常 AgentGo Release 不再携带真实模型跑批、compatibility suite、sentinel、judge、fixture、trial 状态机和 baseline 录制逻辑，生产程序只保留必要运行能力与脱敏排障入口。

6. 报告能够区分真实行为回归、随机波动、受控 variant、环境漂移、资格缺失和 Trace 不完整，不再把 skipped、unknown 或证据丢失误报为 pass。

7. 删除机制与新增机制采用同一套 Trace 覆盖验收：该跟踪的事实可以查到，不该继续存在的旧概念在新事件、查询和指标中彻底消失。
