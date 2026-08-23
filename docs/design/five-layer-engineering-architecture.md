# AgentGo 五层工程架构规范

> 状态：Accepted Architecture，Five-layer repair implementation complete / single-task architecture passed / 8-task validation pending<br>
> 日期：2026-08-21<br>
> 性质：长期目标架构与责任边界，不是当前实现已经完全满足的声明<br>
> 历史母本：[`docs/nextUpgrade-V6.md`](../nextUpgrade-V6.md)

## 0.1 2026-08-22 实施快照

本规范的主体边界已经用于第五轮 SWE 修复，当前不能再按“尚未开始”理解；但
也不能把单题架构门误报为多题 closure。当前事实如下：

| 范围 | 已进入生产主链 | 仍开放 |
|---|---|---|
| Model Invocation 基础层 | typed failure、ContextBinding 唯一入口、L2/L4 动态 OutputBudget、SSE字段/Tool批次硬限、partial no-dispatch | 真实 provider/SWE 多 rollout |
| L1 Prompt | 冻结 Scheduler core prompt + 每 Invocation phase task-control prompt；Graph-first 保持 | 固定模型长期 cohort 指标 |
| L2 Context | Context v8/Replay v3、Responses typed-item representability、32K completion、Raw History projection、ContentStore/ToolResultRef | 真实 tokenizer/model capability profile 精细化 |
| L3 Harness | 仓库 SWE harness、双层 function-call probe、真实 Lease、phase ToolRouter、typed terminal/snapshot | Effect unknown 仍需人工裁决；workspace 仍非 OS sandbox |
| L4 Loop | 6 Attempts、统一 Deadline Compiler、Invocation-failure 中性进展、typed intervention scope、Progress/Terminal 主链 | 多题长时统计 |
| L5 Graph | framework simple Graph、current validate/commit/start、Change、typed Outcome/Result/Evidence | 复杂 OR/generation token 仍关闭；legacy submit/patch 未删 |
| Validation / Trace | contract/full/race/vet/build、真实二进制与单题 `architecture_ok=true` | 单题 task resolved、8题批测与三平台 CI |

本轮明确没有修改 `AGENTS.md`。

## 1. 背景

AgentGo 在 V5、V6 的连续演进中已经落地 Prompt Build、Context Manifest、
Task/Session Memory、Execution Lease、Effect Journal、结构化任务终态和持久化
Graph Runtime。与此同时，多次问题修补也让部分责任集中到
`internal/agent`、`internal/bootstrap` 和 Graph 桥接代码中：同一个文件可能
同时处理 Prompt、Context、工具执行、Loop 生命周期和 Graph 终态协议。

本规范把已经存在但未长期固化的工程主旨整理为五个主体层：

| 层级 | 工程对象 | 核心问题 |
|---|---|---|
| **L1 Prompt Engineering** | 一次指令及其契约 | 我怎么告诉模型做什么？ |
| **L2 Context Engineering** | 一次模型调用可见的完整信息 | 它这一次应该知道什么？ |
| **L3 Harness Engineering** | Tool / Memory Store / State / Effect / Execution Environment | 模型怎样获得受控能力并真正行动？ |
| **L4 Loop Engineering** | 一个 Agent Activation 的持续运行 | 怎样观察、检查、重试、收敛和停止？ |
| **L5 Graph Engineering** | 多个 Agent / Loop / Tool / Evaluator 的协调 | 谁做什么、什么时候做、结果流向哪里？ |

五层之外保留两个明确平面：

- **Model Invocation 基础层**：OpenAI-compatible API、SDK、SSE、响应解析、
  调用错误与 usage。它是五层共同依赖的模型传输底座，不计入五层。
- **Validation / Trace 横切面**：验证各层不变量并投影权威事实。它不拥有
  Task、Memory、Effect 或 Graph 状态，也不计入五层。

## 2. 目标与非目标

### 2.1 目标

1. 为新功能和缺陷修复提供唯一、可操作的责任归属。
2. 明确每层拥有的状态、输入、输出、错误和禁止行为。
3. 保留当前接口驱动和依赖注入原则，避免以包移动制造新的全局状态。
4. 允许当前代码按可验证的小切片逐步迁移，不进行一次性大爆炸重构。
5. 让每个稳定事实都有权威 Store/Journal、可查询 Trace 和对应测试。
6. 让 Graph 继续成为产品主控制面，同时不侵占局部 Loop 和 Harness 职责。

### 2.2 非目标

1. 本规范不立即规定所有 Go 包的最终名称，也不要求五层与目录一一对应。
2. 本规范不改变当前 Graph v2、ExecutionLease、Session、Effect Journal 等已
   生效契约。
3. 本规范不以文档重命名代替真实代码迁移和端到端验证。
4. 本规范不把 workspace 宣称为 OS sandbox，也不扩大任何运行时权限。
5. 本规范不恢复已删除的 Plan Runtime、固定 MaxLoops 或 provider 分支。

## 3. 总体模型

五层是逐级扩大的工程作用域，而不是必须形成同名 Go 包的线性调用栈：

```text
                         L5 Graph
              协调 Node / Activation / Result / Evidence
                              │
                    驱动一个或多个 L4 Loop
                              │
                         L4 Agent Loop
             Attempt / Turn / retry / finalizing / stop
                              │
                    每轮使用 L3 Harness
                              │
                           L3 Harness
      ExecutionLease / Tool / Memory Store / Effect / Workspace / State
                              │
                  每次调用编译 L2 Context
                              │
                         L2 Context
       Prompt + task + memory + history + tool schema + upstream input
                              │
                     绑定冻结的 L1 Prompt
                              │
                         L1 Prompt
                role / instruction / policy explanation / output contract
                              │
                    Model Invocation 基础层
             OpenAI-compatible SDK / SSE / response / usage

Validation / Trace 横跨以上全部作用域，只投影、不夺取权威状态。
```

控制方向通常为 `L5 → L4 → L3 → L2 → L1`；数据和结果向上返回。实现依赖
可以通过端口倒置，但不得形成下层直接操纵上层状态的捷径。

## 4. 全局架构不变量

1. **单一权威**：同一稳定事实只能有一个权威 Store/Journal；Trace、Prompt
   和自由文本都不是第二套状态。
2. **冻结后执行**：Prompt Build、Context 策略、ExecutionLease 和 Graph
   Activation 的执行契约在规定边界冻结；变更通过新 Attempt、Activation 或
   Graph revision 显式发生。
3. **结构化收口**：Graph 节点只通过结构化终态提交完成、阻塞或失败事实；
   自由文本不直接推进 Graph。
4. **权限不由文本授予**：Prompt、Memory、邮件、上游结果和 Suggestion 均
   不能扩大 ExecutionLease。
5. **副作用先记账**：有副作用的操作先进入 Effect Journal，再执行和结算；
   prepared 未 settled 的未知 Effect 不得静默重跑。
6. **Context 可解释**：模型本轮看到的主要信息都能回答来源、权威、新鲜度、
   预算和处置方式。
7. **Graph 不做局部 Loop**：Graph 组织 Activation 和数据流，不替 Agent
   执行多轮思考或直接调用业务工具。
8. **Loop 不改拓扑**：Loop 可以提交结果或请求 replan，但不能直接修改
   Graph 状态、边或其它节点终态。
9. **Harness 不自主循环**：Harness 执行一次受控动作并返回观察，不自行决定
   再试一次、继续工作或宣布任务完成。
10. **验证是交付的一部分**：跨层功能只有在单测、真实二进制路径和预期产物
    均被断言后才能报告完成。

## 5. Model Invocation 基础层

Model Invocation 不计入五层，但必须有独立边界，否则 API/SDK 问题会被错误
归类到 Prompt、Harness 或 Loop。

### 5.1 拥有

- OpenAI Responses typed-item 请求/响应/SSE；Chat Completions 仅作显式兼容适配。
- openai-go SDK 配置、HTTP timeout、SSE 聚合和连接错误。
- 模型返回的 message、reasoning、function/custom tool item、终态和 usage 解析；
  自由正文不得提升为行动。
- 单次请求的模型覆盖、输出上限、流式累计上限和协议兼容处理。
- API 错误的传输级分类：recoverable、unrecoverable、bad response。

### 5.2 不拥有

- Task 是否应该重试或何时耗尽重试预算。
- Context 应保留哪些历史或 Memory。
- 工具是否有权限执行。
- Agent、Task、Node 或 Graph 的终态。
- 供应商名称推断出的隐式行为分支。

### 5.3 当前映射

- `internal/llm/client.go`、`internal/llm/responses_client.go`、`internal/llm/protocol.go`
- `internal/config` 的 `llm:` 配置
- `internal/bootstrap/runtime_builder.go` 的客户端构造

### 5.4 当前不足与提升要点

当前已由 `llm.protocol` 冻结 Responses/Chat wire，Responses 主链以 typed output
item 作为行动身份，并由 ContextBinding 冻结 completion/output/tool batch 预算；
SDK 流式累计阶段早夭，L4 action reservation 进一步收紧本次 completion，typed
failure 端到端保持。后续提升点是继续扩充真实 model capability/tokenizer profile，
并在 compatibility 调用归零后删除 Chat adapter，而不是恢复正文解析或 provider
名称分支。

## 6. L1 Prompt Engineering

L1 负责稳定指令，不负责动态事实和权限执行。

### 6.1 拥有

- Agent 角色、任务方法、协作原则和输出契约。
- Scheduler 的 Graph authoring doctrine。
- worker、explorer、verifier 等角色 Prompt。
- Prompt component 的稳定 ID、版本、顺序、digest 和冻结规则。
- 对硬边界的解释性文字，但不拥有硬边界本身。

### 6.2 输入与输出

输入：角色定义、系统策略说明、节点输出契约、工具能力的只读描述。<br>
输出：不可变 `PromptBuild` 及其稳定引用。

### 6.3 不拥有

- 当前任务事实、Memory 召回、工具结果、邮件或上游 Result。
- 工具 allowlist、路径范围、审批结果或副作用权限。
- retry、deadline、Loop 终止和 Graph 路由。
- 通过自然语言修改 Task、Lease 或 Graph 状态。

### 6.4 当前映射

- `internal/prompt`
- `internal/agent/prompt_build.go`
- `prompts/`、`internal/agenttemplate/prompts/`
- `internal/scheduler/scheduler.go` 的 Scheduler system prompt
- Graph Runtime 派生并注入的输出契约

### 6.5 当前混乱

1. `internal/prompt` 主要提供 Build 身份，实际渲染仍复用
   `agent.buildMessages`，Prompt 与 Context 没有真正独立的编译入口。
2. Scheduler Prompt、普通 Agent Prompt、Graph 输出契约分布在不同包中，缺少
   统一 component catalog 和 ownership 表。
3. 部分运行时恢复纪律只存在于 Prompt，自身缺少对应 Harness/Loop 机械边界。
4. Prompt 变更的行为有效性主要靠手工 SWE 批测，没有稳定的 Prompt cohort
   和多 rollout 对比约定。

### 6.6 提升要点

1. 建立唯一 Prompt Compiler，输入有序 component，不再通过多处字符串拼接
   得到逻辑上相同的 Prompt。
2. 将角色、控制协议、输出契约和工具说明分成独立组件；工具说明只接受 L3
   提供的有效能力快照。
3. Prompt Build 在 Attempt 开始时冻结；核心指令变化必须产生新 Attempt、
   Node 或 Graph revision。
4. 为每类 Prompt 建立 contract tests：组件顺序、版本、digest、禁止承诺的
   工具以及结构化收口语句。
5. L1 不自行扩大 Context hard cap；但所有启动期已知的 Prompt Component 必须
   在 Bootstrap/Team/Spawn 装配门向 L2 current policy 证明可编码。不兼容时拒绝
   创建运行时，不能发布 Task 后再 blocked。

### 6.7 完成标准

- 任一 Invocation 能定位唯一 Prompt Build。
- Prompt 正文变化一定导致 component 或最终 digest 变化。
- Prompt 不包含未由 L3 能力快照提供的工具承诺。
- 内置与配置静态 Prompt 均能通过绑定版本的 L2 policy；正文增长越界会在装配期
  失败并给出 typed ContextAssemblyFailure。
- Prompt 单测不需要联网或调用真实模型。

## 7. L2 Context Engineering

L2 负责把某次 Invocation 应看到的信息编译成有界、可解释的上下文快照。

### 7.1 拥有

- Context 的确定性装配顺序、优先级和预算。
- Task 目标、上游输入、近期 Turn、ToolResult、Mailbox 和运行快照的选择。
- Task/Session Memory 的召回、筛选、相关性、新鲜度、压缩和渲染语义。
- tool schema 在模型可见 Context 中的序列化；工具资格仍由 L3 决定。
- Context Manifest：source、scope、authority、freshness、digest、预算和处置。
- 超大正文转摘要或引用的策略。

### 7.2 Memory 边界

Memory 同时触及 L2 和 L3，必须按“语义与存储”拆分：

| 责任 | 层级 |
|---|---|
| 记忆候选如何从运行事实产生 | L2 |
| inferred / confirmed / stale / superseded 的语义 | L2 |
| 哪些记忆与本轮相关、如何进入 Context | L2 |
| Task Memory 如何压缩、替换、呈现 | L2 |
| Memory 的文件格式、并发、fsync、索引和恢复 | L3 |
| Store.Put/Query/Supersede/MarkStale 的原子执行 | L3 |

L4 只触发“现在需要构建下一轮 Context”或“达到检查点”，不实现 Context
压缩算法；L3 只保存和读取状态，不自行决定某条记忆应进入下一轮请求。

### 7.3 输入与输出

输入：PromptBuildRef、Task/Activation 事实、Memory 查询结果、历史 Turn、
Harness 能力快照、上游 Result/Evidence、Mailbox 和预算。<br>
输出：`ContextSnapshot{Messages, ToolSchemas, Manifest}`。Messages 与 Manifest
必须由同一装配过程产生，不能分别猜测对方内容。

### 7.4 不拥有

- 执行工具、写文件、发送消息或修改外部状态。
- 授予工具、路径、模型或 Graph 控制权限。
- 决定 Task 完成、重试或取消。
- 修改 Graph Node、Transition 或 Activation。
- 把 Memory 内容提升为 system 指令。

### 7.5 当前映射

- `internal/agent/llm_executor.go::buildMessages`
- `internal/agent/context_manifest.go`
- `internal/contextcontract`、`internal/contextadapter`、`internal/contextcompiler`
- `internal/contextstore`、`internal/contentstore`、`internal/policycatalog`
- `internal/agent/context_runtime.go`
- `internal/agent/prompt_build.go` 的任务目标渲染
- `internal/agent/task_memory.go`
- `internal/agent/session_memory.go`
- `internal/agent/memory_context.go`
- `internal/taskmem/render.go`
- `internal/memory/promotion.go`

### 7.6 当前混乱

1. Messages 与 Context Manifest 由相邻但不同的逻辑构建；Manifest 被定义为
   影子账本，尚不是消息装配的唯一产物。
2. `buildMessages`、Prompt Build 和 Manifest 对依赖 Result 的渲染存在多条路径，
   map 遍历顺序还会妨碍完全稳定的 digest 对账。
3. Task Memory 的持久化生命周期、候选提取、Graph 输出契约和 Context 插入
   集中在 `internal/agent/task_memory.go`，横跨 L2、L3、L4、L5。
4. Session Memory 的查询、过滤、渲染和 trace 发射也放在 Agent 包中。
5. L1 Prompt、L2 动态 Context 与 L3 tool schema 的最终边界直到
   `LLMExecutor.Execute` 才临时汇合，难以独立测试完整请求预算。
6. 已由 Response commit gate + Replay v3 关闭坏响应/typed item 到下一轮的非法中间态；
   context overflow 使用 aggressive replay projection，不修改 Raw History。

### 7.7 提升要点

1. 建立单一 Context Compiler，同时生成 Messages、ToolSchemas 和 Manifest。
2. 所有动态段使用稳定 ID 和确定性顺序；map 进入 Context 前必须排序。
3. 抽出 Memory Policy：候选、验证、晋升、召回、预算和 supersede 规则与
   Memory Store 解耦。
4. 将 Task Memory runtime 分成 L2 projector 与 L3 repository；Graph 输出契约
   由 L5 通过明确端口提供，不由 L2 import Graph 解析自由文本。
5. 预算覆盖最终 messages、tool schema 和 provider-required extra fields；
   被排除内容在 Manifest 中记录原因。
6. 建立 Context fixture tests，断言给定权威输入时输出字节、Manifest 和预算
   均稳定，不调用真实模型。
7. hard cap 的任何放宽或收紧都创建新 PolicyVersion；历史 Task 使用已冻结的
   具体 ref，新 Run 才能使用 `ContextDefaultCurrent`。禁止就地改写旧 policy。

### 7.8 完成标准

- 每次 Invocation 绑定一个真实 ContextSnapshot，而非事后推导的影子清单。
- Context Manifest 与实际发送 messages/tool schema 使用同一来源数据。
- Task Memory 长度不随 Loop 次数线性增长。
- inferred 信息不会被无证据晋升为 confirmed。
- 静态 Prompt 与 current policy 不兼容时，Bootstrap/动态运行时创建 fail-closed，
  不产生“零模型调用却被当作普通任务失败”的执行事故。
- Context 编译无文件、网络、工具执行等副作用。

## 8. L3 Harness Engineering

L3 是模型与真实世界之间不可绕过的执行边界。它提供一次受控行动所需的能力，
但不自主形成多轮 Loop。

### 8.1 拥有

- ExecutionLease 的计算、冻结、复用和撤销。
- Tool Registry、Tool schema authority、allowlist 和实际 dispatch。
- Gate、Suggestion、路径边界、read-before-write 和终态 fence。
- Memory、Task、Session、Artifact、ReadSet 等运行状态的 Store/Repository。
- Effect Journal、副作用幂等策略和崩溃恢复裁决。
- workspace、Shell、Interaction、Roster 和执行环境视图。
- Model Invocation 客户端的运行时绑定；API 协议实现仍属于基础层。
- 一次 action 的结构化 Observation。

### 8.2 输入与输出

输入：冻结的 ExecutionLease、一次 tool/model action、当前 Task/Attempt 身份、
审批与模式状态。<br>
输出：结构化 Observation、ToolCallRecord、EffectRef、ArtifactRef、UsageRef 或
有类型拒绝/Suggestion。

### 8.3 不拥有

- 决定继续下一轮、重试整个 Attempt 或宣布任务完成。
- 改写 Prompt 或选择本轮应召回的 Memory。
- 根据自由文本修改权限。
- 直接选择 Graph 边或推进其它节点。
- 在副作用状态 unknown 时自动再执行一次。

### 8.4 当前映射

- `internal/agent/tool_registry.go`
- `internal/tools`
- `internal/runner`
- `internal/store`、`internal/memory`、`internal/taskmem`、`internal/session`
- `internal/gate`、`internal/hook/builtin`
- `internal/effect`
- `internal/workspace`、`internal/shell`、`internal/interaction`、`internal/roster`
- `internal/agent/execution_lease.go`
- `internal/agent/llm_executor.go` 的单轮 model/tool dispatch 部分

### 8.5 当前混乱

1. Harness 没有单一 façade；Runner、Agent、ToolGroup、Gate、Store、Effect 和
   workspace 分别持有执行边界的一部分。
2. `LLMExecutor` 同时负责 L2 Context 编译和 L3 model/tool dispatch，单轮
   请求边界不纯。
3. 工具实现直接依赖 `agent.ToolRegistry` 和 Agent context key，工具协议与
   Agent 运行时耦合。
4. ExecutionLease 类型在 model/agent/store/runner/Graph 之间传播，冻结权威
   虽已存在，但能力来源和执行适配仍分散。
5. Task、Session、Memory、Artifact、Effect、Graph 各自耐久化，跨 Store 收尾
   依赖手写顺序和补偿，缺少统一的事务边界说明。
6. Trace 仍同时承担部分 Reactor 事件分发，观测投影和运行时响应没有完全分离。

### 8.6 提升要点

1. 定义 Harness façade/ports，而不是新建一个包含所有依赖的 God object：
   `ModelInvoker`、`ContextProvider`、`ToolExecutor`、`StateRepository`、
   `EffectCoordinator`、`InteractionGateway`。
2. 将单轮执行拆为明确管线：构建 Context → invoke model → 校验响应 → dispatch
   action → settle state/effect → 返回 Observation。
3. ToolGroup 依赖稳定的工具注册/调用接口，不直接依赖整个 Agent 包。
4. 为跨 Store 收尾写出事务和补偿表，特别是 structured result、Artifact flush、
   workspace merge、Task terminal 和 Graph feed 的先后关系。
5. Domain Event/Outbox 在权威事实提交后产生；Trace 只消费脱敏投影，Reactor
   不再依赖 Trace JSON 作为唯一运行输入。
6. 对权限、路径、Effect、Interaction 和 Store recovery 建立离线 contract tests。

### 8.7 完成标准

- 没有 Prompt、Memory 文本或 Scheduler 声明可以扩大 Lease。
- 每次业务副作用都能定位唯一 Effect 状态和恢复策略。
- finalizing/terminal 后业务工具不可执行。
- Harness 的一次调用不会自行形成第二轮模型请求。
- Graph core 不需要 import Harness 具体实现。

## 9. L4 Loop Engineering

L4 负责单个 Agent Activation 内连续的 observe-think-act 过程及其生命周期。

### 9.1 拥有

- Attempt、Turn、Loop counter 和当前阶段。
- 每轮调用顺序、Observation 回灌和继续/停止决策。
- retry budget、deadline、取消、no-progress 和 emergency fuse。
- structured completed/blocked/failed/cancelled 收口。
- finalizing fence 的生命周期协调。
- checkpoint 时机和恢复入口；事实存储委托 L3。
- usage/tool/effect 等预算的 Loop 级预留与结算策略。

### 9.2 输入与输出

输入：Task/Activation 契约、PromptBuildRef、Context policy、ExecutionLease、
Harness Observation、取消和外部消息。<br>
输出：一个权威 `TaskOutcome`，或显式 retry/replan/blocked 状态。

### 9.3 不拥有

- 直接访问 OpenAI SDK 或自己拼 HTTP 请求。
- 自己实现 Memory Store、文件工具或 Shell。
- 修改 Graph topology、revision、边选择和其它 Activation。
- 把自由文本当成 Graph 节点终态。
- 通过重试偷偷改变已冻结的 Prompt、Lease 或调用契约。

### 9.4 当前映射

- `internal/agent/agent.go::processTask`
- `internal/agent/state.go`
- `internal/agent/finalization.go`、`submit_state.go`
- `internal/agent/progress_notify.go`、`activity.go`
- `internal/agent/llm_executor.go::Execute` 的单步结果
- `internal/runner` 的认领与运行外壳
- `internal/watchdog` 和 Scheduler 介入路径

### 9.5 当前混乱

1. `processTask` 同时负责 L2 Context/Memory、L3 Lease/workspace/Store 和 L4
   状态机，已成为主要 God function。
2. API transport error 经 `LLMExecutor` 桥接成 agent error 后，Loop 又通过错误
   文本判断 context overflow；例如 `context deadline exceeded` 会被误判为
   context-window overflow。
3. 没有完整的 per-turn durable checkpoint；turns.jsonl、TaskMemory、
   LastHistory、ToolCallRecord、Effect 和 usage 的结算边界不同。
4. no-progress 主要依赖 Prompt 自查、重复 Suggestion 和极高 fuse，尚未形成
   统一的机械进展判据与分级干预。
5. watchdog、Task timeout、LLM timeout、Harness timeout 和外部测试 timeout
   没有统一的层级关系，可能在外部收割前无法介入。
6. Retry 会保存历史和 Task Memory，但错误种类、冻结契约和恢复 Context policy
   尚无单一 Attempt 记录可解释。

### 9.6 提升要点

1. 抽出 Loop Controller，以 `ContextProvider`、`Harness`、`OutcomeCommitter`
   等小接口驱动；Agent 只保留身份与运行外壳。
2. 定义有类型 `LoopError`：transport timeout、context overflow、bad response、
   tool rejection、capability violation、cancelled、deadline 等禁止字符串猜测。
3. 建立 Attempt/Turn checkpoint，明确每个权威事实的 settled/unknown 状态。
4. 定义进展信号：新 Effect、Artifact、confirmed Fact、文件版本、Graph result
   字段、阻塞解除；重复读取或相同拒绝不算进展。
5. 统一时间层级：单次 Invocation < 单次 action < Attempt deadline < Graph/
   外部 harness deadline，并保证控制面有真实介入窗口。
6. Loop 收口只产生 TaskOutcome；L5 adapter 再把它转换为 Graph TerminalFact。

#### 9.6.1 Loop intervention 的正式跨层边界（2026-08-23）

- L4 达到 `InterventionAfterTurns` 时，只终结当前 Activation Task，提交
  `blocked[loop_intervention_required]` TaskOutcome 与 durable
  `LoopInterventionRequested`；它不决定 Graph outcome，也不重开旧 Activation。
- L5 必须消费该 typed reason。framework-owned simple Graph 将 `work blocked`
  路由到 `controller_role=loop_recovery` 的 Controller，而不是直接进入 end。
- recovery Controller 可以先以 GraphChangeProposal 修改未来 Definition，再用
  `result.decision=retry|blocked` 裁决；retry 永远创建 `<node>@<N+1>`，不修改
  `<node>@<N>` 的冻结 Prompt/Lease/Next。
- 对带上游数据的 Acceptance retry，L5 以 durable `replay_inputs` 转移复制旧
  Activation 的冻结 InputBinding/ResultRef/Evidence 到新 Activation；L2 只消费
  新 TaskSpec 投影，不自行回忆旧输入。每个 recovery 节点有 framework 冻结的
  有界 retry budget，超额必须 blocked，禁止 recovery 自递归。
- L3 只按冻结 role/source 授予 recovery Controller 的 Graph read/change/submit
  控制闭集，BusinessTools 恒为空；L2 只投影 `failure_context`，L1 只解释决策。
- recovery TaskOutcome delivery 完成后才 ACK 原 intervention；缺少 recovery
  Activation 是架构事故，不得因 Graph 最终 typed blocked 而判为正常。

### 9.7 完成标准

- completed、blocked、failed、cancelled 互斥且只有一个终态提交者。
- 每次 retry 能说明错误类型、已用预算、冻结契约和恢复点。
- finalization 后不存在新的业务副作用。
- 仍在产生机械可证进展的任务不会因任意固定轮数被截断。
- 无进展任务能在外部 deadline 前被 steer、blocked、cancel 或 replan。

## 10. L5 Graph Engineering

L5 是 AgentGo 的主协调层，负责多个局部 Loop、确定性节点和 Evaluator 的
拓扑、数据流与终态。

### 10.1 拥有

- GraphDocument schema、revision、digest 和语义校验。
- Node、Activation、Transition、Router、Join、Approval、Wait、Acceptance、
  Subgraph 和 End。
- Result Store、ResultRef、EvidenceRef、InputBinding 和 lineage。
- Node/Activation 状态机、边选择、回边新 Activation 和 Graph 终态。
- Graph patch CAS、恢复、snapshot、journal 和幂等补发。
- Evaluator/Acceptance 的证据要求与 verdict 路由。
- Node Requirement 和输出契约；实际能力由 L3 计算为 ExecutionLease。

### 10.2 输入与输出

输入：版本化 GraphDocument、外部事件、Interaction 决议、TaskOutcome、
结构化 Result/Evidence。<br>
输出：Activation TaskSpec、选中 Transition、下游 InputBinding、Graph change
请求和唯一 Graph outcome。

### 10.3 不拥有

- 直接进行多轮模型调用或工具 dispatch。
- 通过 Prompt 猜测节点是否成功。
- 修改旧 Activation 的冻结定义、Result 或 Evidence。
- 把 Trace 当作恢复 Graph 的权威日志。
- 让 Scheduler 通过整图覆盖伪造运行状态。

### 10.4 当前映射

- `internal/graph`
- `internal/tools/graph_control.go`
- `internal/tools/submit_result.go` 的 Graph 节点提交协议
- `internal/bootstrap/graph_*.go`
- `internal/scheduler` 的 Graph authoring、patch 和介入逻辑
- `internal/bootstrap/graph_runtime.go` 的 graph-terminal-feed Reactor
- `internal/team` 的 Graph scope 生命周期

### 10.5 当前混乱

1. `internal/graph` 核心通过 `TaskBoard` 等小接口保持较干净，但桥接层集中在
   超大的 `bootstrap/graph_runtime.go` 中。
2. Graph 终态协议分布在 submit tool、Agent finalization、Task Results、Store
   原子提交、terminal feed 和 Runtime，单一所有者不够显式。
3. TaskMemory 通过解析 task description 提取 Graph 输出契约，L2 反向理解 L5
   的文本编码。
4. evidence 从 ToolCallRecord 装配时需要跨 Agent、Store、Artifact 和 Graph
   多域拼接，桥接错误可能造成终态丢失或图僵尸。
5. Scheduler Prompt 同时承担 Graph authoring doctrine 和运行时故障介入，行为
   有效性依赖模型，确定性编译/修复能力仍有限。
6. Graph v1 legacy、v2 终态契约和 `publish_task` 兼容路径同时存在，增加测试
   矩阵和归类成本。
7. Graph change、watchdog wake、下游 worklog 等控制面事实仍部分经普通 Task
   文本传递，缺少统一有类型命令。

### 10.6 提升要点

1. 保持 `internal/graph` 不 import `agent`、`store` 或具体 Harness；桥接继续通过
   小接口和有类型 DTO。
2. 将 `bootstrap/graph_runtime.go` 拆为 Board Adapter、Terminal Adapter、
   Evidence Assembler、Recovery Coordinator 和 Wake Publisher。
3. 定义唯一 `TaskOutcome → TerminalFact` 转换器；任何状态和结构化字段均在
   一处校验、归一和落账。
4. OutputContract 作为 TaskSpec 的有类型字段传给 L2/L4，不再要求下层解析
   description 文本。
5. 将 graph-change、watchdog intervention 和 acceptance dispute 定义为有类型
   控制命令；自然语言只作为给 Scheduler 的说明。
6. 为 v1 compatibility 设明确退出条件；所有新 Graph 只走 v2。
7. 增加 Graph bridge 端到端测试，覆盖 TaskOutcome、Artifact flush、Evidence、
   ResultRef、边选择和 Graph terminal 的完整握手。
8. 新 simple Graph 的 recoverable blocked 必须经过 framework-owned recovery
   Controller；只有该 Controller 明确提交 `decision=blocked` 或自身 Runtime
   failed/blocked 后，Graph 才能进入对应 end。

### 10.7 完成标准

- 每个 Activation 只有一个可追踪 TaskOutcome 和 TerminalFact。
- 每条生效边都能定位稳定 ResultRef/EvidenceRef 和选择原因。
- 回边不会复用旧 Task、Lease、TaskMemory 或 Attempt。
- Graph 关键事实先 durable，再激活下游或发出 graph_ended。
- Graph core 可以用 fake ports 完全离线测试。
- `loop_intervention_required` 后 Graph 保持非终态，且能追踪 recovery Outcome、
  GraphChange revision 与新 Activation；旧 Activation 永不复用。

## 11. Validation / Trace 横切面

Validation / Trace 不作为第六层。它验证五层并解释运行事实，但不拥有运行事实。

### 11.1 权威与投影

| 事实 | 权威 | Trace 角色 |
|---|---|---|
| Prompt 组成 | PromptBuild | component/version/digest 投影 |
| Context 内容 | ContextSnapshot/Manifest | source/authority/budget/处置投影 |
| Memory | Task/Session Memory Store | version/晋升/召回/状态变化投影 |
| 权限 | ExecutionLease | frozen/reused/revoked 投影 |
| 副作用 | Effect Journal | prepared/settled/unknown 投影 |
| Task/Loop | Task Store + Attempt checkpoint | Turn、retry、finalizing、outcome 投影 |
| Graph | Graph Store + journal | revision/activation/transition/terminal 投影 |
| 人工决定 | Interaction Store | request/version/decision 投影 |

### 11.2 当前不足

1. `trace.Emit` 仍同时驱动 Reactor，领域事件与脱敏审计投影没有完成解耦。
2. Trace schema 仍是持续扩张的宽事件结构，跨层关联身份不完全统一。
3. 失败流式调用的 usage、Prompt/Context/Lease 关联和完成状态可能不完整。
4. 外部 SWE harness 能验证最终仓库结果，但与内部 deadline、watchdog 和
   trace completeness 尚未形成同一 Run contract。

### 11.3 提升要点

1. 权威提交后产生 Domain Event/Outbox，再分别驱动 Reactor 与 Trace Projection。
2. 统一 Session→Graph→Activation→Task→Attempt→Turn→Invocation→Tool/Effect
   的稳定关联身份。
3. 所有查询显示 complete/partial/degraded，不能把证据缺失解释为没有发生。
4. 每层至少具备 contract test、恢复测试和一个跨层集成验证。

## 12. 跨层契约

以下对象是层间交换的稳定契约。名称可在实现设计中调整，但责任不得重新混合。

| 契约 | 生产者 | 消费者 | 关键内容 |
|---|---|---|---|
| `PromptBuild` | L1 | L2/L4 | component IDs、versions、digest、正文引用 |
| `ContextSnapshot` | L2 | L3 ModelInvoker | messages、tool schemas、Manifest、budget result |
| `ExecutionLease` | L3 | L3/L4 | model、tools、control tools、workspace、policy、budget |
| `Observation` | L3 | L4 | model/tool result、Suggestion、Effect/Artifact/Usage refs |
| `TaskOutcome` | L4 | L3 Store / L5 Adapter | completed/blocked/failed/cancelled、result、evidence、risk |
| `TaskSpec` | L5 | L3/L4 | Activation 身份、目标、输入、能力需求、输出契约 |
| `TerminalFact` | L5 Adapter | L5 Runtime | activation status、typed Result、Evidence、stable refs |
| `DomainEvent` | 各权威 Store | Reactor/Trace | authority ref、causation、typed payload |

层间不得使用以下方式替代契约：

- 根据自由文本解析权限、终态、verdict 或 Graph path 字段；
- 根据 Agent kind 猜测 Graph role；
- 根据 Trace event absence 推断权威事实没有发生；
- 根据文件名、展示序号或日志顺序生成跨层稳定身份；
- 用 context value 无界传递新的业务协议而不定义类型和所有者。

## 13. 状态所有权

| 状态 | 唯一写入者/权威 | 允许的读者 |
|---|---|---|
| Prompt component 与 Build | L1 Prompt Compiler | L2、L4、Trace |
| ContextSnapshot 与 Manifest | L2 Context Compiler | L3、L4、Trace |
| Memory 条目与 checkpoint | L3 Repository，语义变换由 L2 policy 提议 | L2、L4、L5 adapter |
| ExecutionLease | L3 Lease Manager / Task Store | L3、L4、L5 adapter |
| Effect 状态 | L3 Effect Journal | L3 recovery、L4、L5 adapter |
| Attempt/Turn/TaskOutcome | L4，经 L3 Store 原子提交 | L5 adapter、UI、Trace |
| Graph revision/state/Activation/Transition | L5 Graph Store/Runtime | Scheduler、UI、Trace |
| Interaction decision | Interaction Store | L3 gateway、L5 approval adapter |
| Trace | Trace Writer | CLI、UI、测试；任何层不得反向以其为权威 |

## 14. 当前包映射与目标方向

| 当前区域 | 当前覆盖层 | 下一轮目标 |
|---|---|---|
| `internal/llm` | Model Invocation | 增加冻结调用契约、输出预算和有类型错误 |
| `internal/prompt` | L1 | 从身份对象演进为真正的有序编译核心 |
| `internal/agent/prompt_build.go` | L1/L2/L4 | Prompt 编译迁 L1，Attempt 绑定留 L4 |
| `internal/agent/context_manifest.go` | L2/L4 | 与单一 Context Compiler 合并 |
| `internal/agent/*memory*` | L2/L3/L4/L5 | 拆 projector、repository adapter、checkpoint trigger、output contract port |
| `internal/agent/llm_executor.go` | L2/L3/L4 | 拆 Context Compiler、single-step Harness、Loop adapter |
| `internal/agent/agent.go` | L2/L3/L4/L5 adapter | 收敛为 Agent identity + Loop Controller 外壳 |
| `internal/tools` | L3/L5 adapter | 依赖稳定 Tool API，Graph tool adapter 单列 |
| `internal/memory` / `taskmem` | L2/L3 | policy 与 repository 明确分离 |
| `internal/runner` | L3/L4 | 保留组合与认领外壳，不承载业务规则 |
| `internal/graph` | L5 | 保持纯核心和小端口 |
| `internal/bootstrap/graph_*` | L3/L5 adapter | 按桥接职责拆分，移出大组合函数 |
| `internal/scheduler` | L1/L4/L5 | Prompt、Scheduler Loop、Graph authoring/介入明确分层 |
| `internal/trace` / `reactor` | 横切 | Domain Event 与 Trace Projection 解耦 |
| `internal/bootstrap` | Composition Root | 只装配，不新增领域策略 |

## 15. 问题归类规则

遇到缺陷时按以下顺序定位：

1. **API 请求、SSE、finish reason、tool-call wire JSON、usage 是否有问题？**
   是 → Model Invocation。
2. **静态指令、角色、doctrine、输出约定是否错误？**
   是 → L1 Prompt。
3. **模型本轮看到或没看到什么、预算/顺序/新鲜度是否错误？**
   是 → L2 Context。
4. **权限、工具、状态存储、副作用、workspace、审批或恢复是否错误？**
   是 → L3 Harness。
5. **单 Agent 是否错误地继续、重试、结束、超时或空转？**
   是 → L4 Loop。
6. **节点、路由、回边、Join、Acceptance、Result/Evidence 流是否错误？**
   是 → L5 Graph。
7. **只是看不见事实或无法解释原因？**
   先查权威 Store，再修 Validation/Trace；禁止用补日志代替业务修复。

一个事故可以有多个责任点，但必须分别记录。例如“模型产生畸形 tool call，
重试后仍空转，最终图未推进”应拆为：

- Model Invocation：畸形 wire response 与输出预算；
- L4 Loop：错误分类、retry 和 no-progress；
- L5 Graph：只检查是否正确消费了最终 TaskOutcome，不为下层失控背锅。

## 16. 下一轮架构梳理的提升优先级

### P0：先恢复可控运行边界

1. Model Invocation 增加 completion、reasoning、content、tool arguments 的硬
   预算和流式早夭；错误类型端到端保真。
2. 修正 Loop 的错误分类，禁止 `strings.Contains("context")` 一类宽匹配。
3. 统一 LLM、action、Attempt、watchdog 和外部 harness deadline 的层级。
4. 建立 ContextSnapshot 单一装配路径，避免 Manifest 与真实请求漂移。

### P1：拆除主要 God boundaries

1. 从 `agent.go` 抽出 Loop Controller。
2. 从 `llm_executor.go` 拆分 Context Compiler 与 single-step Harness。
3. 从 `task_memory.go` 拆分 L2 policy、L3 repository adapter 和 L5 output
   contract port。
4. 拆分 Graph Board、Terminal、Evidence、Recovery 和 Wake adapters。

### P2：收紧权威与验证

1. Domain Event/Outbox 与 Trace Projection 解耦。
2. 为跨 Store 收尾建立事务/补偿矩阵。
3. 为五层建立 import/contract architecture tests。
4. 校正文档权威：本规范描述长期不变量，旧 V6 文档保留历史设计语境，
   `Archtechture.md` 不再混用过期行为与当前事实。

## 17. 分阶段迁移建议

### Phase 0：特征化与护栏

- 为当前主要路径补 characterization tests，不先移动代码。
- 固定 Prompt、Context、Lease、TaskOutcome、TerminalFact 的当前字节/状态行为。
- 建立包依赖快照和禁止新增反向依赖的检查。

### Phase 1：Model Invocation 与 Loop 止血

- 落地 InvocationContract、输出预算和有类型错误。
- 修正 timeout/context overflow 分类。
- 对齐所有 deadline，并用单题 SWE 对照 stream on/off。

### Phase 2：L1/L2 编译边界

- 建立 Prompt Compiler 和 Context Compiler。
- Messages 与 Manifest 同源生成。
- 保持旧 `LLMExecutor` 作为适配器，逐步切换调用方。

### Phase 3：L3 Harness 端口

- 提取 ToolExecutor、ModelInvoker、StateRepository、EffectCoordinator 等端口。
- 工具包不再依赖完整 Agent runtime。
- 为 structured result 和跨 Store 收尾固定事务顺序。

### Phase 4：L4 Loop Controller

- 抽出 Attempt/Turn 状态机和 TaskOutcome。
- 加入 checkpoint、no-progress 和统一 budget/deadline。
- Agent/Runner 退化为身份、认领和生命周期外壳。

### Phase 5：L5 Adapter 收敛

- 拆分 Graph bridge。
- OutputContract、TaskOutcome、TerminalFact 全部类型化。
- 退出不再需要的 v1/legacy 分支。

### Phase 6：事件、Trace 与文档

- 权威提交 → Domain Event/Outbox → Reactor/Trace。
- 更新运行手册、包速览和架构索引。
- 在实现与验证完成后，再决定是否将精简版不变量写入 `AGENTS.md`。

## 18. 验证策略

### 18.1 分层验证

| 层 | 必需测试 |
|---|---|
| Model Invocation | httptest/SSE、输出预算、错误分类、usage unknown/settled |
| L1 Prompt | component 顺序、版本、digest、能力承诺对账 |
| L2 Context | fixture、预算、稳定顺序、Manifest 同源、Memory 状态纪律 |
| L3 Harness | Lease、Gate、Effect recovery、workspace、权限 fail-closed |
| L4 Loop | retry、cancel、deadline、checkpoint、finalization、no-progress |
| L5 Graph | schema、CAS、activation、transition、acceptance、recovery、bridge E2E |

### 18.2 跨层完成标准

任何非平凡迁移切片至少需要：

1. 所属包单测通过；
2. `go test ./...`；
3. `go vet ./...`；
4. 独立二进制构建并真实启动；
5. 端到端走过新路径并断言预期文件、事件或状态；
6. 涉及并发或持久化时运行对应 race/recovery 测试；
7. 涉及 Prompt/模型行为时运行受版本控制的外部 SWE case，多 rollout 报告
   成功率、调用、usage、时延和禁止行为，不以单次结果替代架构验证。

## 19. 下一次架构梳理前的准备清单

- [x] 确认本规范的五层名称、Model Invocation 底座和 Trace 横切面。
- [ ] 为每个当前生产文件标注主要层、次要 adapter 和待迁移责任。
- [ ] 生成当前 import dependency 图并登记允许/禁止边。
- [ ] 列出 `agent.go`、`llm_executor.go`、`task_memory.go`、
      `bootstrap/graph_runtime.go` 的拆分候选。
- [x] 定义 PromptBuild、ContextSnapshot、Observation、TaskOutcome、
      TerminalFact 的最小字段与所有者。
- [x] 建立跨 Store 收尾和恢复窗口矩阵，并落地两阶段 TerminalIntent、checkpoint
      Seal、Outcome/Task CAS 与 pending intent 恢复。
- [x] 将当前 SWE 问题逐项归入 Model Invocation、L1–L5 或 Trace。
- [ ] 为 Phase 0 选定 characterization tests，确保重构前后行为可比较。
- [ ] 明确 legacy Graph v1、`publish_task` 和 Trace/Reactor 耦合的退出条件。
- [ ] 评审通过前不移动大包、不修改 `AGENTS.md`、不宣称当前实现已经符合本规范。

## 20. 文档关系

- 本文：长期五层工程架构、责任边界与迁移准备。
- [`docs/nextUpgrade-V6.md`](../nextUpgrade-V6.md)：V6 历史升级母本，保留当时
  的问题背景和完整设计推演。
- [`docs/agents-reference.md`](../agents-reference.md)：当前实现参考手册；与本文
  冲突时，本文描述目标边界，参考手册描述当前运行事实。
- [`Archtechture.md`](../../Archtechture.md)：历史与现状混合的总体说明；下一轮
  梳理时应按本文区分当前事实、历史设计和目标架构。
- [`docs/design/graph-terminal-contract-v2.md`](graph-terminal-contract-v2.md)：当前
  Graph v2 终态协议，属于 L5 的现行细化契约。
- [`Graph Draft / Commit / Start`](graph-draft-commit-start.md)、
  [`Loop Progress / Checkpoint / Deadline`](loop-progress-checkpoint-and-deadline.md)、
  [`Context Snapshot / Item Budget`](context-snapshot-item-budget.md)、
  [`Invocation Failure / Loop Recovery`](invocation-failure-and-loop-recovery.md)：
  第五轮 SWE 的四份正式实现设计。
- [`SWE 架构修复统一实施路线图`](swe-architecture-repair-roadmap.md)：当前实施账本、
  遗留分类、验证口径与后续顺序的唯一汇总。
- [`docs/activate/MemoryManageSystem.md`](../activate/MemoryManageSystem.md)：Memory
  的历史设计与迁移记录，后续应按 L2 语义/L3 存储重新归类。
- [`docs/activate/ReactiveSystem.md`](../activate/ReactiveSystem.md)：Gate/Reactor
  历史设计，后续应按 L3 Harness 与 Trace/Domain Event 横切面重新归类。

## 21. 决策记录

本 Draft 采用以下初始决策，后续评审若修改必须留下明确记录：

1. 主体架构固定为 L1 Prompt、L2 Context、L3 Harness、L4 Loop、L5 Graph。
2. Model Invocation 是五层下方的基础层，不强行归入 Harness。
3. Validation/Trace 是横切面，不是第六主体层。
4. Memory 语义属于 L2，Memory Store 与耐久执行属于 L3。
5. Graph 是主协调层，但不得侵占局部 Loop 与 Harness。
6. 当前包结构只是迁移起点，不作为本规范已经落地的证明。
7. 本轮只新增本文，不修改 `AGENTS.md`。
