# Context Snapshot / Item Budget 架构

> 状态：Accepted Design，Context v7 implementation complete / single-task architecture verified<br>
> 日期：2026-08-22<br>
> 归属：L2 Context Engineering<br>
> 对应问题：SWE-013<br>
> 上位规范：[`五层工程架构规范`](five-layer-engineering-architecture.md)<br>
> 关联设计：[`Loop Progress Contract / Checkpoint / Deadline`](loop-progress-checkpoint-and-deadline.md)<br>
> 统一路线图：[`SWE 架构修复统一实施路线图`](swe-architecture-repair-roadmap.md)

## 0.1 2026-08-22 实施状态

### Context v3–v7 / Replay v2 修订

最新外部单题继续冻结历史 policy ref，并把新 Run 默认推进到
`context:default/v7`：v4 修正 mixed ASCII/Unicode estimator；v5 扩大
RequiredExact reasoning；v6 在128K窗口内冻结92K input + 32K completion + 4K
protocol overhead；v7 放宽 optional reasoning 字节容器，避免131078-byte观察字段
使同响应 typed verdict 丢失。v1–v6 均保留原 digest/恢复语义，不原地改写。

第六轮 SWE 的 SWE-015 证明 v2 仍允许“当前 Invocation 接受、下一轮无法
replay”的非法中间态。正式修订保留 v1/v2 digest 语义，新增
`context:default/v3` + `provider-replay:openai-compatible/v2`，并在该切片当时设为新 Run 默认：

- `reasoning` 为 Optional：超限时 Raw Turn 不变，Snapshot 记录
  `assistant_reasoning + dropped`，不产生 WireItem，也不终止 Agent；
- `reasoning_content/reasoning_details` 为 RequiredExact：Response commit 在任何
  Tool dispatch/History commit 前用真实下一轮 envelope 证明可表示；
- ContextBinding 冻结 Invocation OutputBudget；SDK 取 L2 completion reserve、
  L4 当前 action 剩余预算与绝对安全上限的最小值；
- v3 显式声明 128K-token model window、16K completion reserve 与 4K protocol
  overhead，Compiler/Policy 使用同一不等式校验；
- Raw History 不再按累计完整 Prompt spend 修改。每轮按当前 section pressure
  派生 replay；旧摘要不会再次被摘要；provider context overflow 只切换
  aggressive projection；
- v3 无真实 tokenizer 时使用 `max(bytes/3, Unicode runes)` 保守估算；v1/v2
  继续冻结原 `rune/3` 行为，未来真实 tokenizer 通过新 PolicyVersion 接入；
- 完整大 ToolResult 先进入 Task-scope ContentStore，再以可分页 Ref + 有界预览
  进入 L2 tombstone；旧 `enforce_compact_token_threshold` 已删除。

`ContextFragment`、versioned policy、ContentStore、ContextCompiler、不可变
ContextSnapshot/Manifest metadata、单项/section/总预算、atomic group 与
ContextRuntime adapter 已进入仓库，并装配到主要 Scheduler/Runner Agent 请求路径；
Invocation 层的 content/reasoning/extra/tool-args/total 绝对输出上限和 partial
no-dispatch 也已接入。相关 focused tests 与全仓 compile-only 已通过。

生产入口审计已经收口：Agent、用户 Reactor 和 Proposal Verifier 都必须先完成
Context 编译与 Snapshot Put，再以包含 invocation/snapshot/context-policy/
tool-router/request-digest 的 `ContextBinding` 调用统一 `llm.Invoke`；源码扫描测试
阻止生产调用方重新直接调用 `Client.Chat`。旧 Snapshot/隔离测试只能显式走
`InvokeLegacy`，部分装配 fail-closed。

2026-08-22 首次修复后 SWE 批跑又暴露一项 L1/L2 装配回归：冻结 Scheduler
`agent_role` 约 51 KiB，而 `context:default/v1` 的 `prompt_component` hard cap
为 48 KiB，导致 8/8 任务在首次 Invocation 前全部 blocked、模型调用数为零。
正式修复保留 v1 原语义并新增 `context:default/v2`：单 Prompt Component 为
64 KiB，system section 为 96 KiB；所有新 Run 使用 v2，历史 Run 继续使用其冻结
版本。Bootstrap、动态 Team 与 ad-hoc Spawn 在创建运行时副作用前，使用真实
ContextAdapter/Compiler 编码路径预检静态 Prompt；不兼容时拒绝装配，不能再把
问题推迟到用户 Task。

仓库验证已完成：full/race/vet/build、真实二进制、harness 单测与 provider 单题
架构门通过。最新 Run 使用 Context v7，未复现 `fragment_limit_exceeded`；任务因
Worker 零写入被 L4 blocked，属于任务正确率失败。仍开放真实 tokenizer、更多
provider fixture、三平台 CI 与 Flask 8题 rollout；这些属于外部效果/发布证据，
不再是 Context 实现缺口。

## 1. 决策摘要

本设计为所有 model-visible context 建立唯一、可解释且有硬边界的编译路径，
阻止单个超大 reasoning、assistant content、ToolResult、Memory、上游 Result、
ExtraFields 或 tool schema 原样污染后续模型调用。

本设计固定以下架构决策：

1. SWE-013 的主责层是 L2 Context；Model Invocation 基础层负责有界响应和流式
   早夭，L3 负责原始内容/引用存储，L4 只决定超限后的 Attempt 行为。
2. 所有可能进入模型视野的内容必须先成为 `ContextFragment`；任何调用方不得
   绕过 Fragment 直接 append 最终 messages、tool specs 或 provider extra fields。
3. Context 只经一个 `ContextCompiler` 生成不可变
   `ContextSnapshot{Messages, ToolSpecs, Manifest}`；Manifest 与真实发送内容同源，
   不再作为另一条独立推测路径。
4. 预算按“单字段/单项 hard cap → 协议原子组 → section budget → snapshot total
   budget → completion reserve”顺序执行；总预算不能替代单项 hard cap。
5. 每个 FragmentKind 使用版本化 framework policy；Scheduler 不能修改 hard cap
   或允许静默截断，只能在 Graph/Task 契约中声明内容来源与交付要求。
6. 大型正文默认存入 L3 Content Store，L2 只注入有界摘要和稳定 `ContentRef`；
   模型不能仅凭文本 Ref 自动扩大权限，解引用仍受 ExecutionLease/工具边界约束。
7. system instruction、用户任务、输出契约、tool schema、tool-call arguments 和
   provider-required replay 字段禁止静默截断；无法合法外置时编译/Invocation
   fail-closed。
8. assistant tool-call message、对应 tool results 和 provider replay fields 按
   `ProtocolAtomicGroup` 处理；不能只裁一半而产生无效消息序列。
9. Model Invocation 在 SSE 累积阶段对 content、reasoning、ExtraFields、tool name、
   tool arguments 和 response total 分别计量；达到上限立即取消流，不等 HTTP
   deadline 或完整响应。
10. 超限/中断响应不得 dispatch 未完成 tool call，不得追加 partial response 到
    History；只返回有类型 `InvocationOutputLimitExceeded`。
11. provider 是否要求原样回传 reasoning/ExtraFields 由显式、版本化
    `ProviderReplayPolicy` 声明，禁止根据 provider 名称或错误字符串隐式猜测。
12. 未知 provider replay 语义按 fail-closed 处理；不得为节省 Context 私自删字段
    后继续请求。
13. Context policy 在 Attempt 开始时冻结；每次 Invocation 生成新的不可变
    ContextSnapshot。改变 policy、provider replay contract 或核心 Prompt 必须创建
    新 Attempt。
14. oversized raw content 的持久化受 retention/security policy 管理；reasoning
    不因调试需要默认永久落盘，Trace 只记录尺寸、digest、Disposition 和 Ref。
15. 不以降低累计压缩阈值、只截旧 ToolResult 或统一截到一个固定字符数作为
    SWE-013 的修复。

## 2. 问题定义

### 2.1 第五轮事故

第五轮 Flask SWE 批测观察到单轮：

- reasoning 最高约 241K 字符；
- assistant text 最高约 275K 字符；
- DSML、自言自语和重复文本随 History/ExtraFields 进入后续轮次；
- 多题 completion 消耗异常，并进一步放大下一轮 prompt；
- 现有累计压缩发生在完整响应落入 History 之后。

因此一个异常 Model response 可以先占用大量内存/时间，再成为最近 HistoryEntry，
随后被原样送回 provider。累计 Context 可能尚未达到压缩阈值，但单个 item 已经
远超健康边界。

### 2.2 当前数据路径

```text
SDK/SSE 累积 content/reasoning/ExtraFields/tool args
  → llm.Response
  → ExecuteResult
  → HistoryEntry
  → buildMessages
  → llm.Message + SetExtraFields
  → 下一次 provider request
```

当前 SSE 使用字符串/数组持续拼接，缺少逐字段累计 hard cap。`HistoryEntry`
保存完整 AssistantContent、ToolResults 和 ExtraFields；`buildMessages` 又按协议
原样重建。最近 HistoryEntry 会被历史压缩保留，因此旧历史压缩不能保护当前最
大的单项。

### 2.3 现有保护为何不足

当前主要有三类保护：

1. 部分高输出工具的旧结果被替换成墓碑；
2. 本压缩周期累计 prompt tokens 超过阈值后压缩旧历史；
3. Task/Session Memory 自身有渲染预算。

它们无法覆盖：

- 最新一条巨大 assistant content；
- reasoning/ExtraFields；
- 非 snip 目标工具的巨大 ToolResult；
- 单个巨大上游 Result/Graph input；
- tool schema 或 tool-call arguments；
- 用户任务、Mailbox、Prompt component；
- provider-required exact replay 原子组。

### 2.4 Manifest 不是执法边界

当前 `buildMessages` 先生成 messages，`buildContextManifest` 再从相邻输入生成影子
账本。Manifest 可以描述估算尺寸，却不能证明：

- 实际 wire message 与记录完全一致；
- 每个字段都经过相同 hard cap；
- tool specs 与实际 ToolRouter 快照一致；
- provider extra fields 没有绕过预算；
- 总预算为 completion 留出空间。

缺失的是一个唯一 Context 编译事务，而不是更多观测字段。

## 3. 目标与非目标

### 3.1 目标

1. 让每个 model-visible item 都有来源、权威、尺寸、预算、Disposition 和 digest。
2. 在响应进入 History 之前阻止无界 content/reasoning/tool arguments。
3. 在请求发送之前证明每个 wire item 和整个 snapshot 都满足 hard cap。
4. 保持 OpenAI-compatible tool calling 和 provider replay 协议完整。
5. 将大型内容外置为稳定 Ref，避免 Context 与持久化原文绑定。
6. 让 Context Manifest 与真实发送 Messages/ToolSpecs 从同一中间对象生成。
7. 让超限行为有类型、可恢复、可测试，L4 不解析自由文本。
8. 保证 policy/Prompt/Tool capability 在规定冻结边界内可追踪。

### 3.2 非目标

1. 不以本设计解决模型为什么产生 DSML 或重复 reasoning；那仍属 SWE-003/009。
2. 不把 L2 变成 Content Store、Artifact Store 或 Memory Store。
3. 不允许 ContextRef 绕过 ExecutionLease 自动读取任意原文。
4. 不假定所有 provider 都允许删 reasoning/ExtraFields。
5. 不把所有 item 强制为同一个数值上限。
6. 不默认永久保存模型 reasoning 或敏感 ToolResult。
7. 不让 Prompt 或 Scheduler 自由文本扩大预算。
8. 不以 summary 文本替代 ResultRef/EvidenceRef 的权威数据流。

## 4. 分层责任

| 边界 | 拥有 | 不拥有 |
|---|---|---|
| Model Invocation 基础层 | response/output cap、SSE 计量、provider replay adapter、typed failure | 选择哪些业务事实进入 Context |
| L1 Prompt | 指令 component 和输出契约 | 动态 Context 预算、原文存储 |
| **L2 Context** | Fragment 选择、转换、排序、预算、Snapshot、Manifest | Tool 执行、Attempt 重试 |
| L3 Harness | Content Store、Memory/Result/Artifact ref、ToolRouter/Lease 快照 | 哪段内容应被摘要或进入 Context |
| L4 Loop | 请求下一 Snapshot、消费 typed failure、换 Attempt/blocked | 摘要算法、provider wire 编码 |
| L5 Graph | 提供 Task/Result/Evidence/Input 契约和稳定引用 | 直接拼 messages |
| Validation/Trace | policy 校验与脱敏投影 | 真实 Context 第二账本 |

Memory 仍按五层规范拆分：L2 决定召回、摘要和呈现，L3 负责存储、索引、并发和
恢复。ContentRef 也遵循相同边界。

## 5. 三种对象边界

### 5.1 ContextFragment：语义对象

Fragment 表示一个候选上下文单元：

```go
type ContextFragment struct {
    FragmentID       string
    Kind             FragmentKind
    SourceRef        string
    Scope            ContextScope
    Authority        Authority
    Freshness        Freshness
    Digest           string
    SerializedBytes  int64
    EstimatedTokens  int64
    RetentionClass   RetentionClass
    ReplayGroupID    string
    Content          []byte
    ContentRef       string
    Disposition      Disposition
    TransformRef     string
}
```

Fragment 是编译输入和 Manifest 的共同来源，但不是 provider wire message。

### 5.2 ProtocolAtomicGroup：协议对象

某些字段必须一起保留、一起转换或一起拒绝：

```go
type ProtocolAtomicGroup struct {
    GroupID       string
    GroupKind     AtomicGroupKind
    FragmentIDs   []string
    ReplayPolicy  ReplayPolicy
    TransformID   string
}
```

首版 GroupKind：

```text
assistant_tool_exchange
assistant_provider_replay
system_instruction_set
user_task_contract
tool_definition
```

原子组避免以下非法状态：

- assistant 声明 tool call，但对应 tool result 被直接删除；
- tool_call arguments 被截成非法 JSON；
- reasoning-required provider 收到丢失 replay 字段的 assistant message；
- tool schema 被截断成语义不同的 JSON schema；
- system/output contract 被部分截断后仍继续执行。

### 5.3 WireItem：编码对象

WireItem 是 provider adapter 编码前的最终、预算已通过对象：

```go
type WireItem struct {
    WireID          string
    Kind            WireItemKind
    FragmentIDs     []string
    SerializedBytes int64
    EstimatedTokens int64
    PayloadDigest   string
    Payload         any
}
```

只有 WireItem 可以进入最终 `Messages`、`ToolSpecs` 或 provider extra fields。

## 6. FragmentKind 与处置政策

### 6.1 首版 FragmentKind

```text
prompt_component
system_output_contract
user_task
task_control_context
upstream_result
upstream_evidence
assistant_content
assistant_reasoning
assistant_extra_field
assistant_tool_call
tool_result
task_memory
session_memory
mailbox_message
interaction_decision
runtime_snapshot
tool_definition
```

未知 Kind 不得进入 Context；必须先扩展 schema 和 policy catalog。

### 6.2 Disposition

```text
inline              # 原文进入 wire
summarized          # 有界摘要进入 wire，原文以 Ref 保留或按 policy 丢弃
referenced          # 只注入稳定 Ref + 有界元数据
tombstoned          # 保留协议位置和 identity，正文替换为结构化墓碑
dropped             # 允许省略的低优先级信息未进入 snapshot
rejected            # 不能合法转换，编译失败
quarantined         # 异常输出隔离，不进入 History/Context
```

Disposition 必须写入 Manifest，并引用具体 policy/transform。`dropped` 不能用于
system instruction、用户任务或 provider-required replay 字段。

### 6.3 类型政策表

| FragmentKind | 允许 inline | 允许 summary/ref | 允许 drop | 超限默认 |
|---|---:|---:|---:|---|
| prompt_component | 是 | 仅显式 component transform | 否 | rejected |
| system_output_contract | 是 | 否 | 否 | rejected |
| user_task | 是 | 附件/ContentRef，保留任务身份 | 否 | referenced 或 rejected |
| task_control_context | 是 | 有界规范化 | 否 | rejected |
| upstream_result | 是 | ResultRef + 摘要 | 条件允许 | referenced |
| upstream_evidence | 是 | EvidenceRef + 摘要 | 条件允许 | referenced |
| assistant_content | 是 | 摘要/ContentRef | provider policy 允许时 | summarized/quarantined |
| assistant_reasoning | policy 决定 | policy 决定 | 仅 Optional | rejected/quarantined |
| assistant_extra_field | policy 决定 | policy 决定 | 仅 Optional | rejected/quarantined |
| assistant_tool_call | 是 | 不得改变参数语义 | 否 | rejected |
| tool_result | 是 | ContentRef + 墓碑/摘要 | 否，须保留 call identity | referenced/tombstoned |
| task/session memory | 是 | 有界渲染 | 是 | summarized/dropped |
| mailbox_message | 是 | 有界摘要 | 低优先级可 | summarized/dropped |
| interaction_decision | 是 | 有界结构化表示 | 否 | rejected |
| runtime_snapshot | 是 | 有界差量 | 是 | summarized/dropped |
| tool_definition | 是 | 不允许截断 schema | 可整体不 advertise | rejected 或整工具 dropped |

具体数值不散落在调用点，由版本化 policy catalog 决定。

## 7. Context Budget Policy

### 7.1 Policy 对象

```go
type ContextBudgetPolicy struct {
    PolicyID              string
    Version               int
    ModelClass            string
    FragmentRules         map[FragmentKind]FragmentBudgetRule
    AtomicGroupRules      map[AtomicGroupKind]AtomicGroupBudgetRule
    SectionBudgets        map[ContextSection]Budget
    SnapshotInputBudget   Budget
    CompletionReserve     Budget
    AbsoluteWireByteLimit int64
}
```

每条 FragmentRule 至少包含：

```text
max_serialized_bytes
max_estimated_tokens
allowed_dispositions
retention_class
transform_id
priority
```

### 7.2 谁能选择 Policy

- framework 提供封闭、版本化 catalog；
- 用户配置可以选择受支持 profile，或在 schema 允许范围内进一步收紧；
- Scheduler/Prompt 不能扩大 policy；
- per-node capability/model 可以选择兼容的 model class，但最终 policy 由框架
  根据冻结 model + RunContract + Context schema 解析；
- config doctor 输出 resolved PolicyID、digest、各 section budget 和 replay
  compatibility，不打印敏感正文。

### 7.3 数值冻结流程

本设计不把未经测量的 10K tokens 直接复制成所有类型的统一值。实现迁移时：

1. shadow mode 采集各 FragmentKind 的 bytes/tokens 分布；
2. 用成功/失败 SWE 与普通会话建立分位数和异常样本；
3. 为 `agentgo.context/v1` 固定具体默认数值；
4. 将数值写入版本化 policy catalog 和 contract tests；
5. 以后任何放宽/收紧都产生新 PolicyVersion。

在 enforcement 启用前必须已有非零 framework absolute ceiling；shadow mode 不能
继续允许真正无界内存累积。

### 7.4 默认 Policy 版本与静态 Prompt 装配门

| Policy | `prompt_component` | `system` section | 使用范围 |
|---|---:|---:|---|
| `context:default/v1` | 48 KiB / 12K estimated tokens | 64 KiB / 16K | 历史 Run/Graph 冻结兼容，只读保留 |
| `context:default/v2` | 64 KiB / 16K estimated tokens | 96 KiB / 24K | 历史 Run 冻结兼容，Replay v1 |
| `context:default/v3` | 64 KiB / 16K estimated tokens | 96 KiB / 24K | 历史 Run；Replay v2 + model window/reserve + Raw History projection |
| `context:default/v4–v5` | 64 KiB / 16K estimated tokens | 96 KiB / 24K | 历史 Run；mixed estimator 与 RequiredExact reasoning 修订 |
| `context:default/v6` | 64 KiB / 16K estimated tokens | 96 KiB / 24K | 历史 Run；92K input + 32K completion + 4K overhead |
| `context:default/v7` | 64 KiB / 16K estimated tokens | 96 KiB / 24K | 新 Run；v6窗口分配 + optional reasoning 字节容器修订 |

`ContextDefaultCurrent` 只允许在创建新契约时解析为 v7；恢复已有 Task 时必须读取
持久化的具体 ref，禁止借别名静默升级。v2 的 system section 与
`AtomicSystemInstructionSet` 同为 96 KiB，使一个最大 64 KiB 的 `agent_role` 与
独立 output contract 能同时合法存在；这不是取消 hard cap。

所有静态 Agent Prompt 必须满足以下装配不变量：

```text
真实 Prompt wire 编码可通过冻结 Policy
  或 Bootstrap / Team Provision / ad-hoc Spawn 立即失败
```

预检必须复用生产 ContextAdapter/Compiler，不能以 `len(prompt)` 的近似检查代替，
因为 JSON envelope、估算 token、原子组和 section 预算同样属于契约。动态 Task、
History、工具定义仍在每次 Invocation 的完整编译事务中检查。

## 8. 预算执行顺序

ContextCompiler 严格按以下顺序执行：

```text
冻结 PromptBuild / ContextPolicy / Lease / ToolRouter view / provider replay policy
  → 收集候选 ContextFragment
  → 规范化 source、authority、digest、稳定顺序
  → 执行每个 provider 字段和 Fragment hard cap
  → 执行 ProtocolAtomicGroup 完整性与 group cap
  → 执行 section budget
  → 执行 snapshot total input budget
  → 预留 completion/output budget
  → 编码 WireItem
  → 生成 Messages / ToolSpecs / provider extras
  → 从实际 WireItem 生成 Manifest 与 EncodedRequestDigest
  → Seal ContextSnapshot
```

前一阶段失败不得进入下一阶段。不得先构造 messages，再用 Manifest 检查后继续
发送一个已经超限的请求。

### 8.1 单字段 hard cap

provider payload 中每个 content、reasoning field、extra field、tool name、tool
arguments 和 schema 字段都要计量。单字段超限不能靠整个 snapshot 仍未超限放行。

### 8.2 原子组 cap

原子组超过预算时只能：

1. 使用该 GroupKind 已注册、已验证的完整 transform；
2. 创建新 Attempt/新 provider contract；
3. rejected。

禁止只删组内一个字段。

### 8.3 section budget

首版 section：

```text
system
task_contract
upstream_inputs
memory
conversation_history
tool_results
mailbox
runtime_control
tool_definitions
```

section budget 控制公平性，避免一个类别吞掉整个 Context。

### 8.4 completion reserve

Snapshot 输入预算必须为本次 InvocationContract 的 completion/tool-call 输出保留
空间。若 provider 只支持总 context window，则：

```text
input_budget
  <= model_context_window
     - completion_reserve
     - protocol_overhead_reserve
```

无法可靠取得 model context window 时使用保守 model class policy，不从 provider
名称自由文本猜测。

## 9. ContextSnapshot

### 9.1 类型

```go
type ContextSnapshot struct {
    SnapshotID           string
    Schema               string
    AttemptID            string
    InvocationID         string
    PromptBuildRef       string
    ContextPolicyID      string
    ContextPolicyDigest  string
    ProviderReplayRef    string
    ExecutionLeaseRef    string
    ToolRouterSnapshotID string

    Fragments            []ContextFragmentRecord
    AtomicGroups         []ProtocolAtomicGroupRecord
    WireItems            []WireItemRecord
    Messages             []llm.Message
    ToolSpecs            []llm.ToolDef
    Manifest             ContextManifest

    InputBudgetUsed      BudgetUsage
    CompletionReserve    Budget
    EncodedRequestDigest string
    SealedAt             time.Time
}
```

Snapshot seal 后不可变。Messages、ToolSpecs、Manifest 和 request digest 必须来自
同一 WireItem 列表。

### 9.2 稳定顺序

所有 map 输入在编译前排序；顺序规则成为 Context schema 的一部分：

1. system components 按 Prompt Build 顺序；
2. task/control contract；
3. upstream input 按端口和 activation identity；
4. Memory 按 policy priority 和稳定 ID；
5. History 按 settled Turn sequence；
6. Tool definitions 按 ToolRouter snapshot 的规范顺序。

同一组冻结输入必须产生相同 Snapshot digest。

### 9.3 持久化与审计

权威持久化至少包含：

- SnapshotID/schema/policy/tool/prompt/lease refs；
- Fragment metadata、digest、Disposition、TransformRef；
- AtomicGroup 结果；
- WireItem metadata 与 EncodedRequestDigest；
- budget report 和 Manifest。

是否保存完整 encoded request 由安全配置决定。默认不能为了可观测性复制含秘密、
用户正文、ToolResult 或 reasoning 的完整第二份 payload。

## 10. Content Store 与 ContentRef

### 10.1 L3 Content Store

大型原文由 L3 存储：

```go
type ContentRef struct {
    RefID          string
    ContentDigest  string
    MediaType      string
    SizeBytes      int64
    RetentionClass RetentionClass
    Authority      Authority
    Scope          string
}
```

Content Store 负责：

- content-addressed 去重；
- 原子写入和 digest 校验；
- retention/过期；
- 敏感等级和访问审计；
- Session/Task/Graph scope；
- 崩溃恢复和 Windows-safe 文件处理。

L2 只决定是否使用 Ref/摘要，不直接实现文件格式或 fsync。

### 10.2 Ref 不授予权限

Context 中出现 `content_ref:...` 只是信息和谱系，不代表模型可读取原文。需要读取
时必须调用明确工具，经冻结 ExecutionLease、scope、path/content boundary 和
预算再次校验。

Graph ResultRef、EvidenceRef、ArtifactRef 应优先复用各自权威 Store，不把所有
类型强行复制进通用 Content Store。

### 10.3 RetentionClass

```text
ephemeral_request
task_lifetime
session_lifetime
artifact_retained
diagnostic_opt_in
never_persist
```

reasoning/ExtraFields 默认依 provider/security policy 选择 ephemeral 或
never_persist；不能因模型返回了字段就默认永久落盘。

## 11. Transform 与摘要纪律

### 11.1 Transform registry

每种允许的 summary/reference/tombstone 变换必须有稳定 TransformID：

```text
tool_result_ref/v1
upstream_result_ref/v1
memory_render/v1
mailbox_summary/v1
history_summary/v1
assistant_content_summary/v1
```

Transform 记录输入 digest、输出 digest、预算和保留的 identity/ref。

### 11.2 权威不升级

- inferred 原文的摘要仍是 inferred；
- ToolResult 摘要不能替代 ToolResultRef；
- Result 摘要不能替代 ResultRef；
- 模型生成摘要不能自动变成 confirmed Fact；
- tombstone 只证明内容被外置/清理，不证明原 action 成功。

### 11.3 是否允许 LLM 摘要

默认优先确定性摘要和结构化字段抽取。如果必须调用 LLM 摘要：

- 它是独立、有界 Invocation；
- 绑定独立 PromptBuild/ContextSnapshot/usage；
- 输出本身经过 hard cap；
- 记录 provenance 和 inferred authority；
- 失败不能悄悄回退为原文 inline。

## 12. ProviderReplayPolicy

### 12.1 显式契约

```go
type ProviderReplayPolicy struct {
    PolicyID       string
    Version        int
    Fields         map[string]ReplayRequirement
    GroupTransforms []ReplayTransform
}
```

`ReplayRequirement`：

```text
required_exact
required_transformable
optional
forbidden
unknown
```

### 12.2 处理规则

- `required_exact` 超限：当前 Attempt 不能继续沿用该历史；新 Attempt 或 rejected。
- `required_transformable`：只允许注册且经过 provider fixture 验证的 transform。
- `optional`：可按 policy summarized/dropped。
- `forbidden`：不得回传。
- `unknown`：fail-closed，不做猜测性删减。

provider policy 由 adapter 配置/fixture/version 建立，不能在 Loop 中写
`if strings.Contains(provider, ...)`。

### 12.3 tool calling 原子性

assistant tool call 与 tool result 必须保持：

- call ID 对应；
- 数量与顺序合法；
- tool arguments 是完整 JSON；
- tool result 即使外置，也保留 call identity 和结构化 Ref/tombstone；
- 未完整生成的 tool call 永远不 dispatch。

## 13. Model Invocation 有界响应

### 13.1 InvocationContract

```go
type InvocationOutputBudget struct {
    MaxContentBytes       int64
    MaxReasoningBytes     int64
    MaxExtraFieldBytes    int64
    MaxToolNameBytes      int64
    MaxToolArgumentsBytes int64
    MaxResponseBytes      int64
    MaxCompletionTokens   int64
}
```

预算在调用前冻结，进入 request/stream handler。

### 13.2 SSE 计量

每个 chunk 到达时先：

1. 更新每字段 bytes/tokens 估算；
2. 检查单字段上限；
3. 检查 response total；
4. 校验 tool-call 增量仍可形成合法 JSON 前缀/结构；
5. 通过后才进入 accumulator 和 UI coalescing。

达到上限时使用明确 cancel cause 终止 stream，并返回：

```go
type InvocationOutputLimitExceeded struct {
    Field          string
    ActualBytes    int64
    LimitBytes     int64
    Partial        bool
    UsageState     UsageState
    ReplayPolicyID string
    Cause          error
}
```

### 13.3 失败纪律

超限后：

- 不把 partial content/reasoning/ExtraFields 加入 History；
- 不 dispatch tool call；
- 不把 partial assistant 消息写成 provider replay history；
- UI 可以显示“输出因预算终止”的有界状态，不能把 partial 当正式答案；
- usage 标为 settled/partial/unknown；
- 原始 partial 是否 quarantine 由 retention policy 决定；
- L4 收到 typed failure 后选择新 Attempt 或 blocked。

`finish_reason=length` 与本地 output hard cap 使用不同 FailureKind，不能混为输入
Context overflow。

## 14. L4 超限处置

L4 不实现 Fragment transform，只消费结果：

```text
ContextCompiled(snapshot)
ContextAssemblyRejected(failure)
InvocationOutputLimitExceeded(failure)
ProviderReplayRejected(failure)
```

默认策略：

| Failure | L4 动作 |
|---|---|
| 可安全外置的 ToolResult/Result 超限 | 请求 L2 以同 policy 重编译 Snapshot |
| history assistant content 可摘要 | 新 ContextSnapshot；必要时新 Attempt |
| required_exact replay 超限 | 结束 Attempt，换 provider/replay policy 或 blocked |
| user task/system contract 超限 | 请求附件/引用或 blocked，不静默裁剪 |
| tool schema 超限 | 缩减 ToolRouter capability 后新 Attempt；不能裁 schema |
| tool arguments/output stream 超限 | 结束 Invocation，不 dispatch；按 typed policy 重试/blocked |
| policy/transform unknown | fail-closed |

policy、PromptBuild、Lease 或 provider replay contract 变化必须生成新 Attempt；同一
Attempt 内只允许按冻结规则生成不同 ContextSnapshot。

## 15. Context Compiler

### 15.1 输入端口

```go
type ContextCompileInput struct {
    AttemptID          string
    InvocationID       string
    PromptBuild        PromptBuildRef
    TaskContract       TaskContractRef
    UpstreamInputs     []InputBinding
    HistoryCursor      HistoryCursor
    MemoryCandidates   []MemoryRef
    MailboxCursor      MailboxCursor
    RuntimeSnapshotRef string
    ExecutionLease     ExecutionLeaseRef
    ToolRouterSnapshot ToolRouterSnapshotRef
    BudgetPolicy       ContextBudgetPolicy
    ReplayPolicy       ProviderReplayPolicy
}
```

Compiler 是纯 Context 决策组件：不执行工具、不写外部状态、不修改 Graph、不决定
Task 终态。

### 15.2 输出端口

```go
type ContextCompileResult struct {
    Snapshot *ContextSnapshot
    Failure  *ContextAssemblyFailure
}
```

`ContextAssemblyFailure` 使用封闭 reason：

```text
fragment_limit_exceeded
atomic_group_limit_exceeded
section_budget_exceeded
snapshot_budget_exceeded
completion_reserve_unavailable
untransformable_required_fragment
provider_replay_unknown
tool_schema_too_large
content_ref_unavailable
non_deterministic_encoding
```

### 15.3 ToolRouter 同步

L3 冻结一个 ToolRouter snapshot，同时提供：

- runtime dispatch registry view；
- model-visible tool specs；
- capability/Lease identity；
- stable tool ordering；
- schema digest。

ContextCompiler 只能使用该 snapshot 的 visible specs；实际 dispatch 也必须使用同
一 snapshot，防止模型看见与执行能力漂移。

## 16. Manifest 与 Trace

### 16.1 Manifest Item

每个 Fragment/WireItem 记录：

```text
fragment_id
kind
source_ref
scope
authority
freshness
input_digest
output_digest
serialized_bytes
estimated_tokens
budget_limit
disposition
transform_ref
content_ref
atomic_group_id
wire_id
```

### 16.2 Snapshot 级字段

```text
snapshot_id
attempt_id
invocation_id
policy_id/digest
provider_replay_policy_id
prompt_build_ref
lease_ref
tool_router_snapshot_id
input_budget_used
completion_reserve
encoded_request_digest
```

### 16.3 Trace 事件

建议投影：

```text
context_snapshot_compiled
context_fragment_transformed
context_fragment_dropped
context_compile_rejected
invocation_output_limit_exceeded
provider_replay_rejected
content_ref_created
```

Trace 只写尺寸、digest、Ref、policy 和 reason code，不写完整 reasoning、秘密、用户
正文或大 ToolResult。

## 17. 恢复与一致性

### 17.1 Snapshot 与 Attempt

- ContextPolicy/ReplayPolicy/PromptBuild/Lease 在 Attempt 开始冻结；
- 每次 Invocation 生成新 SnapshotID；
- Snapshot seal 后不可修改；
- RetryRollback 若创建新 Attempt，必须生成新 AttemptID 和 policy refs；
- 同一 Snapshot 重试 transport 请求时可复用其 digest，但使用新 InvocationID；
- Context rebuild 必须记录 parent SnapshotRef 和 recovery reason。

### 17.2 ContentRef 恢复

Snapshot 引用的 ContentRef 若已按 retention 合法过期：

- 不得把缺失 Ref 当成空内容；
- 编译失败为 `content_ref_unavailable`；
- L4 决定重新获取、换 Attempt 或 blocked；
- 已 sealed 的历史 Snapshot 仍保留 metadata/digest，查询显示 degraded。

### 17.3 原始 History 纪律

原始 settled Turn/Tool/Effect 事实保持不可变；ContextSnapshot 是一个选择和渲染
视图。压缩、summary、drop 不应破坏原始审计记录。provider partial output 未 settled
则不成为正式 History Turn。

## 18. 安全与隐私

1. Context compiler 不因 ContentRef 读取扩大当前 Lease。
2. Secret/redacted fragment 的 digest 不能用于可逆展示；Trace 不打印正文。
3. reasoning retention 默认最小化，provider replay 只在当前 Attempt 所需作用域内
   保留。
4. Content Store 按 Session/Task/Graph scope 校验；跨 scope 引用 fail-closed。
5. 大内容 quarantine 与普通 Artifact 分离，不能被误当成用户交付物。
6. summary transform 保留 authority，不把模型自述升级为 confirmed。
7. 原文删除与 retention 到期需要明确 ledger；不能只删文件留下悬空“可用”Ref。
8. Prompt dump 仍受显式调试开关和脱敏策略控制，不能成为 hard cap 旁路。

## 19. 与其他 SWE 问题的关系

### 19.1 SWE-003 / SWE-009

本设计的流式 hard cap、tool arguments cap 和 partial no-dispatch 会限制长 JSON、
DSML 和 malformed response 的破坏半径，但不会让模型自动恢复格式能力。structured
output/response format 的效果仍需 SWE-003 独立评估。

### 19.2 SWE-014

本地 output limit、provider `finish_reason=length`、input context-window overflow 和
request timeout 必须使用不同 typed failure。SWE-014 的错误契约是 L4 正确选择
Context rebuild/transport retry 的前置条件。

### 19.3 SWE-011

一次超大响应被拒绝后，L4 ProgressCheckpoint/预算不能被 Retry 重置；重复 output
limit failure 应进入有界 Attempt rollover/intervention/blocked。Context 编译结果、
SnapshotRef、typed failure 和 usage 作为本 Turn 的事实进入
`TurnSettlementDelta`，再由 L4 评估进展和预算；L2 不直接修改 ProgressCheckpoint。

### 19.4 SWE-012

Graph Draft/Definition 的 Task/Output/Result/Evidence 只通过稳定引用进入 L2；Graph
commit validator 应检查节点声明所需 Context policy/tool capability 可被编译。运行中
policy 变化只影响未来 Activation。

## 20. 建议包边界与接口

包名不是最终强制项，依赖边界应满足：

```text
internal/contextcontract
  FragmentKind / Disposition / BudgetPolicy / ReplayPolicy / Snapshot DTO

internal/contextcompiler
  candidate collection ports / transforms / budget engine / wire compiler

internal/contentstore
  ContentRef / retention / scope / durable storage

internal/invocation
  output budget / stream counters / typed failure / provider replay adapter

internal/agent
  只经 ContextProvider 获取 Snapshot，逐步退役 buildMessages 直拼路径

internal/bootstrap
  Prompt/Task/Memory/Graph/ToolRouter adapters 与真实装配
```

建议端口：

```go
type ContextProvider interface {
    Compile(ctx context.Context, input ContextCompileInput) (ContextCompileResult, error)
}

type ContentRepository interface {
    Put(ctx context.Context, content ContentObject) (ContentRef, error)
    Resolve(ctx context.Context, ref ContentRef, lease ExecutionLeaseRef) ([]byte, error)
}

type ToolRouterSnapshotProvider interface {
    Freeze(ctx context.Context, lease ExecutionLeaseRef) (ToolRouterSnapshot, error)
}

type ModelInvoker interface {
    Invoke(ctx context.Context, snapshot ContextSnapshot,
        contract InvocationContract) (InvocationResult, error)
}
```

## 21. 迁移切片

### Slice 0：特征化与 absolute ceiling

- 固定第五轮超大 reasoning/content/ExtraFields fixture；
- 统计每类 Context item 当前尺寸；
- 在 SSE accumulator 增加不可配置为无限的 absolute byte ceiling；
- 保持行为 shadow/告警，异常绝对上限直接 fail-closed；
- 固定 tool-call partial 不 dispatch。

### Slice 1：Context contract types

- 定义 FragmentKind、Disposition、AtomicGroup、Policy、Snapshot DTO；
- 建立 policy catalog/version/digest；
- config doctor 输出 resolved policy；
- contract tests 覆盖未知 Kind/policy fail-closed。

### Slice 2：Content Store / Ref

- ContentRef、scope、digest、retention；
- ToolResult/上游 Result 大正文外置；
- crash recovery、过期和 degraded 查询；
- Windows 文件句柄与原子写测试。

### Slice 3：ContextCompiler shadow mode

- 从现有 buildMessages 输入生成 Fragment/Wire/Manifest shadow Snapshot；
- 与实际 request 做 EncodedRequestDigest 对账；
- 稳定排序和 map 确定性；
- 不改变发送字节，收集差异。

### Slice 4：ToolRouter snapshot

- runtime registry 与 model-visible specs 同一冻结对象；
- LLM invoke 与 dispatch 共享 snapshot identity；
- tool schema hard cap 和整工具 drop/reject；
- capability/Lease 对账。

### Slice 5：ContextCompiler 接管发送路径

- Messages/ToolSpecs/Manifest 全部从 WireItem 生成；
- 退役独立 `buildMessages + buildContextManifest` 装配；
- 每项/原子组/section/total/completion reserve enforcement；
- History 只提供 source refs/cursor。

### Slice 6：Provider replay 与流式预算

- ProviderReplayPolicy adapter 和 fixtures；
- SSE content/reasoning/extra/tool args/total counters；
- typed early abort；
- partial quarantine/no-history/no-dispatch；
- usage partial/unknown。

### Slice 7：L4 恢复策略

- 接入 SWE-014 typed failure；
- safe rebuild、新 Attempt、provider change、blocked；
- 与 SWE-011 ProgressCheckpoint/Attempt budget 对账；
- 重复超限不能无限 Retry。

### Slice 8：真实验证与退出兼容

- 单题超大 response fixture；
- 真实二进制启动和请求；
- 8 题 Flask SWE 回归；
- 断言 Snapshot/Manifest/Ref/typed failure durable 产物；
- 设置旧直拼路径退出条件并删除。

## 22. 验证矩阵

### 22.1 Fragment/预算

| 场景 | 预期 |
|---|---|
| 单 assistant item 超限、总预算未超 | 单项规则仍拒绝/转换 |
| 多小 item 总和超限 | section/total policy 确定性裁剪 |
| system/output contract 超限 | rejected，不静默截断 |
| 内置/配置静态 Prompt 与 current policy 不兼容 | 启动或运行时创建事务立即失败，不发布用户 Task |
| 历史 v1 Run 恢复 | 保持 v1 hard cap/digest，不静默升级 v2 |
| 用户任务超限且无附件/Ref | rejected/blocked |
| 大 ToolResult | 保留 call ID，注入 Ref+有界摘要/墓碑 |
| 大上游 Result | ResultRef + 摘要，不复制全文 |
| tool schema 超限 | 整工具不 advertise 或编译失败，不裁 JSON |
| ContextRef 越权 | L3 fail-closed |
| 相同冻结输入重复编译 | Snapshot/Wire digest 一致 |

### 22.2 协议原子组

- assistant tool call 与 tool result 数量/ID/顺序保持合法；
- tool args 超限不 dispatch；
- required_exact reasoning 超限不删字段继续；
- optional ExtraFields 可按 policy drop 并记 Manifest；
- unknown replay policy fail-closed；
- 注册 transform 必须通过 provider request/response fixture。

### 22.3 SSE/Invocation

- content、reasoning、单 extra field、tool name、tool args、total 分别触发 hard cap；
- 达限后及时取消 stream，不等 HTTP deadline；
- partial 不进入 History；
- partial tool call 不执行；
- UI 不把 partial 标成完成；
- `finish_reason=length`、local output limit、context overflow、request timeout 分类不同；
- usage 正确标为 settled/partial/unknown。

### 22.4 ContextSnapshot/Manifest

- Messages、ToolSpecs、Manifest 同一 WireItem 来源；
- Manifest item 与 wire item 一一可追踪；
- EncodedRequestDigest 与真实请求字节一致；
- ToolRouter snapshot ID 在 advertise/dispatch 相同；
- Context policy/Prompt/Lease/replay identity 完整；
- 不在 Trace 中泄露 raw reasoning/secret/大 ToolResult。

### 22.5 恢复

- ContentRef 过期显示 degraded，不当空内容；
- 新 Attempt 不复用旧的 incompatible replay group；
- transport retry 可复用同 Snapshot digest；
- Context rebuild 有 parent SnapshotRef/reason；
- Store 写失败不发送未审计请求；
- raw History 保持不可变，Context transform 只改变视图。

### 22.6 真实事故

- 241K reasoning 在进入 History 前被有类型阻止；
- 275K assistant content 不原样进入下一轮；
- malformed/DSML tool args 达限或无效时不 dispatch；
- 单项超限不会靠累计压缩延后处理；
- 成功任务所需的合法大 Result 能通过 Ref/Evidence 正常消费；
- provider-required replay 未被静默破坏。

## 23. Trace 与诊断口径

每个 Context 问题至少能回答：

```text
哪个 Fragment 超限？
来源和权威是什么？
输入/输出 digest 与尺寸是多少？
应用了哪个 Policy/Transform？
最终 Disposition 是什么？
属于哪个 AtomicGroup？
真实 wire 是否包含它？
completion reserve 是否满足？
provider replay 为什么允许/拒绝？
L4 随后创建了哪个 Attempt/终态？
```

CLI/UI 显示 `complete/partial/degraded/rejected`，不能把 Ref 解析失败解释为内容为空，
也不能把被 quarantine 的 partial response 当正式模型输出。

## 24. 完成定义

只有以下全部满足，SWE-013 才能标记 fixed：

1. 所有 model-visible 内容都经 `ContextFragment` 进入唯一 ContextCompiler；
2. `ContextSnapshot` 同时生成 Messages、ToolSpecs、Manifest 和 request digest；
3. 每个 FragmentKind、provider 字段、AtomicGroup、section 和 snapshot 都有版本化
   hard cap，且 completion reserve 生效；
4. Scheduler/Prompt 不能扩大 policy 或声明静默截断；静态 Prompt 在产生 Runner
   副作用前通过真实编码预检，policy hard cap 改变必须新增版本；
5. 大型 ToolResult/Result/Memory 使用稳定 Ref 和有界表示；
6. system、用户任务、输出契约、tool schema、tool args 和 required replay 不会被
   静默截断；
7. ToolRouter advertise/dispatch 使用同一冻结 snapshot；
8. SSE 在 content/reasoning/extra/tool args/total 达限时早夭，partial 不入 History、
   不 dispatch；
9. ProviderReplayPolicy 显式、版本化、fixture 验证，unknown fail-closed；
10. L4 只消费 typed compile/invocation failure，并受 Attempt/no-progress budget 限制；
11. Content Store retention、scope、敏感数据和过期恢复通过测试；
12. Trace 不泄露原始大正文，能对账每项 Disposition 与真实 wire；
13. 目标单测、provider fixtures、恢复测试、race、全量测试、真实二进制启动和
    预期 durable 产物通过；
14. 新一轮 Flask SWE 证明异常大 response 不再污染后续 Context，同时合法成功题
    不因错误裁剪丢失必要协议或证据。
