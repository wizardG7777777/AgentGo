# SWE/集成测试问题清单 — 2026-08-22 15:10（第六轮 Flask SWE 0/8 分层诊断）

> **修复状态：IMPLEMENTATION COMPLETE / REPOSITORY VERIFIED（2026-08-22）**<br>
> SWE-015…019 的正式架构修复均已进入生产主链；全仓普通测试、全仓 race、
> vet、build 与真实二进制烟测通过。修复后首次 provider 单题证明首轮 Prompt
> 降至 3847 tokens 并创建 durable GraphDraft；authoring-progress 最后一处映射
> 补齐后的完整外部重跑需要用户明确批准任务代码出境，当前记 validation pending。

> 管理约定见
> [`2026-08-20-1508-swe-round3-open-issues.md`](2026-08-20-1508-swe-round3-open-issues.md)。<br>
> 上一轮诊断见
> [`2026-08-21-2329-swe-round5-layered-diagnosis.md`](2026-08-21-2329-swe-round5-layered-diagnosis.md)。<br>
> 第1–11节保留修复前的已观察事实与讨论问题；第12节是批准后的完成记录。

## 1. 批次结论

第六轮继续使用 Nebius `deepseek-ai/DeepSeek-V4-Flash`、streaming、全角色统一
模型、每题外部时限 1200 秒。批次完整结束，成绩 **0/8 resolved**：

| 考题 | terminal | wall | LLM calls | prompt | completion | patch | 首要停止原因 |
|---|---|---:|---:|---:|---:|---:|---|
| ipv6-server-name | quiet | 160s | 2 | 24,373 | 16,384 | 0 | output/tool-call 异常后第二 Attempt 被提前耗尽 |
| ipv6-session-txn | quiet | 94s | 1 | 24,313 | 14,644 | 0 | 下一轮 `reasoning` replay 超限 |
| secret-key-rotation | quiet | 90s | 1 | 24,347 | 16,384 | 0 | DSML/畸形工具后 `reasoning` replay 超限 |
| context-push-order | quiet | 103s | 1 | 24,219 | 16,384 | 0 | 下一轮 `reasoning` replay 超限 |
| session-access-tracking | quiet | 217s | 4 | 92,983 | 28,452 | 0 | tool-call 异常后第二 Attempt 被提前耗尽 |
| automatic-options | quiet | 76s | 1 | 24,313 | 11,733 | 0 | 下一轮 `reasoning` replay 超限 |
| teardown-callbacks | quiet | 94s | 1 | 24,289 | 16,384 | 0 | 下一轮 `reasoning` replay 超限 |
| pass-context-dispatch | quiet | 140s | 2 | 24,429 | 16,384 | 0 | `finish_reason=length` 后第二 Attempt 被提前耗尽 |

与上一批“7–8 秒、0 calls”的静态 Prompt 事故不同，本轮所有题都完成
`context:default/v2` 启动预检并发生真实模型调用。此前 v1→v2 修复已经生效；
0/8 是继续进入动态 response replay 和 L4 recovery 后暴露的新问题，不是静态
Prompt hard cap 复发。

本轮仍有三个全局事实：

1. 八题 `patch_lines=0`，Judge 红态与 baseline 完全一致；
2. 八题 `graphs=[]`，每个 worktree 的
   `.agentgo/state/graph-authoring/authoring.jsonl` 均为 0 行；
3. 所有失败都发生在 origin Scheduler Task 或其 Loop Intervention wake，尚未发布
   Worker/Acceptance Graph 节点。

因此不能把本轮归因于 Flask 补丁、Graph Runtime 转移或验收逻辑。

## 2. “正常模型回复”与架构事实的区别

本轮多条 Turn 被记录为 `completed`，reasoning 文本也能讨论正确的 Graph 拓扑；
但一次模型响应是协议包，不是一段文字：

```text
Model Response
├── content
├── reasoning / provider ExtraFields
├── tool_calls{name, arguments}
├── finish_reason
└── usage
```

各层的“完成”不是同一事实：

| 事实 | 权威 | 本轮含义 |
|---|---|---|
| HTTP 200 / `llm_call_end` | Model Invocation | Provider 返回了响应 |
| Turn `completed` | L4 Turn ledger | 当前调用可记录，不代表工具成功 |
| Tool succeeded | L3 Tool dispatcher | name/args/Gate/handler 全部成功 |
| Context replayable | L2 ContextCompiler | 下一轮能合法重放全部 required 字段 |
| Progress accepted | L4 ProgressEvaluator | durable Delta 命中冻结 ProgressContract |
| GraphDraft exists | L5 AuthoringStore | `create_graph_draft` 事务成功落账 |
| Task completed | L4/L5 terminal authority | 交付与终态事务完成 |

所以“模型说准备创建 Graph”“reasoning 看起来合理”“Turn completed”都不能替代
工具、Context、Progress 或 Graph 的结构化事实。

## 3. 五层问题总览

| ID | 层级 | 状态 | 已证实问题 |
|---|---|---|---|
| SWE-018 | L1 Prompt | **implementation-complete** | 冻结 core Prompt + 每 Invocation phase contract；单体约52.9KiB Prompt 退出生产 |
| SWE-015 | L2 Context | **implementation-complete** | Context v3/Replay v2、Response commit、Optional dropped、Raw History projection |
| SWE-019 | L3 Harness | **implementation-complete** | 真实 Scheduler Lease、phase ToolRouter、batch cap、ToolResultRef、function-call probe |
| SWE-016 | L4 Loop | **implementation-complete** | Attempt 只在 start/rollover 边界执法；最后一个 Attempt 保留完整 Turn 权利 |
| SWE-017 | L4 Loop | **implementation-complete** | 唯一 Deadline Compiler + phase window + 1秒 durable handoff reserve |
| — | L5 Graph | **reached / no new issue** | 修复后 provider 单题已创建 durable GraphDraft；Result/Evidence 改走 typed ContextInputs |

这些编号从本文起保持稳定。SWE-015 是 SWE-013 实施后的动态 replay 回归；
SWE-016/017 是 SWE-011 实施后的执行边界回归。保留新编号是为了后续逐项讨论和
验收，不表示另起一套 Context/Loop 架构。

## 4. SWE-015 — Optional reasoning 无法完成 Response→Replay 闭环（L2，P0）

### 4.1 已证实事实

五题在首个成功 Turn 后的下一轮统一出现 `fragment_limit_exceeded`：

- automatic-options；
- context-push-order；
- ipv6-session-txn；
- secret-key-rotation；
- teardown-callbacks。

根据 `ContextAdapter.stableID` 的冻结算法，用各自 TurnID 反算失败 Fragment，五个
ID 全部精确对应：

```text
turn:<turn-id>/provider-extra:reasoning
```

这些成功 Turn 的 reasoning 长度约为 26,319–56,767 字符。当前合同为：

```text
Invocation MaxReasoningBytes / MaxExtraFieldBytes = 128 KiB
L2 assistant_reasoning / assistant_extra_field    = 32 KiB / 8K estimated tokens
ProviderReplayPolicy reasoning                     = Optional
ContextAdapter actual disposition/group            = Inline + RequiredExact
```

因此出现非法中间态：当前 Invocation 接受并写入 History，下一轮才发现无法表示。

### 4.2 L2 架构偏移

1. `reasoning` 已被 ProviderReplayPolicy 声明为 Optional，但 Adapter 没有消费该
   决策；
2. Adapter 把所有 provider ExtraFields 无条件建成
   `FragmentAssistantExtraField`，专用 `FragmentAssistantReasoning` 没有成为生产
   分类权威；
3. 所有 ExtraFields 又被无条件加入 `AtomicAssistantProviderReplay` 的
   `RequiredExact` 组；
4. Invocation 输出上限与下一轮 replay 上限独立配置，没有“接受即保证可表示”
   的跨层不变量。

### 4.3 明确不是修复方案的事项

- 不能只把 L2 reasoning cap 从 32 KiB 提高到 128 KiB；这会扩大 Context，却仍
  没有落实 Optional/RequiredExact 语义；
- 不能静默删除 `reasoning_content`/`reasoning_details` 等 RequiredExact 字段；
- 不能在下一轮编译失败后才修改 raw History。

### 4.4 待讨论问题

1. 是否建立 Response Commit 前的 next-turn representability gate；
2. Optional reasoning 超限应 `dropped` 还是 `quarantined`；
3. RequiredExact 输出上限是否必须直接由 ReplayPolicy 派生；
4. 是否新增 ProviderReplayPolicy v2 / ContextPolicy v3，保留既有版本恢复语义；
5. partial、History、Turn ledger、UI reasoning 的保留边界如何区分。

## 5. SWE-016 — 最后一个获准 Attempt 被提前耗尽（L4，P0）

### 5.1 已证实事实

ipv6-server-name、pass-context-dispatch、session-access-tracking 都先遇到 typed
Invocation failure 并按策略创建新 Attempt：

- `tool_calls[167].name` 4015 bytes > 512；
- `finish_reason=length`；
- `tool_calls[0].name` 1890 bytes > 512。

第二 Attempt 可以初始化，但首个 Turn settlement 后统一进入：

```text
no_progress_budget_exhausted
attempts=2
```

Coordination policy 的 `MaxNoProgressUsage.Attempts=2`；当前 per-turn 判定使用：

```text
CumulativeUsage.Attempts >= MaxNoProgressUsage.Attempts
```

而 Attempt 初始化只在 `used > limit` 时拒绝。结果是系统允许第二 Attempt 启动，
随后不论该 Attempt 当前 Turn 是否形成进展，都因 `2 >= 2` 进入 blocked。

### 5.2 L4 架构偏移

Attempt 上限本应控制“是否允许创建下一个 Attempt”，却被放进每个 Turn 的
no-progress exhaustion 判定。当前 Attempt 的剩余执行权与未来 Attempt slot 使用同一
计数表达，最后一个合法 Attempt 因此没有完整执行窗口。

### 5.3 待讨论问题

1. Attempt budget 是否只在 rollover/start transition 执法；
2. 如何分别表示 `current_attempt_remaining` 与 `future_attempt_slots`；
3. 最后一个 Attempt 何时可被 no-progress turns/duration/usage 正常终止；
4. InvocationFailure 新 Attempt 是否与业务 no-progress rollover 使用同一预算维度；
5. 当前 ProgressCheckpoint/LoopStore 的历史记录如何兼容修正后的语义。

## 6. SWE-017 — Recovery wake deadline 天生非法（L4，P0）

### 6.1 已证实事实

上述三个 origin Task blocked 后都成功发布了 durable `loop-intervention-wake`。wake
携带同一 Run lineage，`RunPhase=recovery`，但认领后立即 blocked：

```text
L4 ProgressCheckpoint 初始化/恢复失败:
attempt deadline 必须早于 run deadline-finalization_reserve
```

当前 `attemptHardDeadline` 对 Recovery phase 派生：

```text
AttemptDeadline = RunDeadline - FinalizationReserve
```

`ValidateChildDeadline` 又要求严格小于同一边界，因此所有非 Graph Recovery wake
都必然失败。Finalization phase 把 AttemptDeadline 设为 RunDeadline，也存在同型
问题。

### 6.2 L4 架构偏移

deadline 派生分散在 `attemptHardDeadline`、`loopDeadlineSet` 和 DTO Validator；
生成器与验证器没有共享一个权威编译过程。Loop Intervention bridge 已经可靠交付
command，但消费 command 的 Recovery Task 不能建立合法 Checkpoint，闭环仍然断裂。

### 6.3 待讨论问题

1. 是否建立唯一 Deadline Compiler，一次生成 Run/Graph/Activation/Attempt/Action；
2. Recovery/Finalization phase 是否应拥有不同的子作用域而非复用普通 Attempt；
3. 严格顺序使用明确 reserve 还是最小时间 epsilon；
4. Intervention wake 是否创建独立 Task/Attempt/Checkpoint，而不恢复原 Attempt；
5. 发布 wake 前由何处执行完整 contract validation。

## 7. SWE-018 — Scheduler Prompt/行动阶段没有真正分离（L1，P1，假设待证）

### 7.1 已观察事实

本轮首次 Scheduler Context 约含：

```text
Provider 实测 prompt ≈ 24K tokens
system section       ≈ 52.9 KiB
tool definitions      = 32
```

模型 reasoning 能复述 GraphDraft、implement/check/acceptance、失败回边等正确概念，
却反复推翻拓扑并延迟合法动作。已观察到：

- 26K–56K 字符 reasoning；
- completion 多次精确达到 16,384 tokens；
- DSML 泄漏；
- 至少 168 个 tool calls 的单响应；
- 多次 `create_graph_draft` 实际缺少 `graph_id`。

### 7.2 尚不能下的结论

当前证据不能证明“大 Prompt”单独导致 provider 失稳。同一模型/provider、不同
Prompt/ToolRouter cohort 的对照尚未进行；provider 本身的 function-calling 能力也
未验证。因此 SWE-018 保持 `hypothesis-open`，不能直接以删 Prompt 文本作为修复。

### 7.3 待讨论问题

1. Scheduler 是否按 request-understanding、draft-create、draft-patch、validate、
   commit/start、intervention 分 Prompt phase；
2. 哪些机械规则应只存在于 schema/validator，不再重复进入 agent_role；
3. 首轮合法动作率、reasoning 长度、Draft 创建调用数如何成为 Prompt cohort 指标；
4. Prompt phase 变化是否必须创建新 Attempt/ToolRouter snapshot；
5. 如何保持 Graph 始终是请求控制面而不恢复 direct-answer 路径。

## 8. SWE-019 — Tool capability/ToolRouter 缺少生产门禁（L3，P0）

### 8.1 已证实事实

1. 批次活探针只发送 `ping` + 5-token 普通文本请求，不携带工具；
2. 它只能证明网络、密钥、模型存在和基础 completion 可用；
3. 本轮 provider 实际出现 DSML 工具名、超长工具名、自造工具、缺 required 参数、
   13/168 tool-call 批次；
4. L3 对这些调用均正确 fail-closed，没有把 prose 猜成动作；
5. Scheduler synthetic ExecutionLease 记录的 business/control 工具数量，不能完整表达
   实际向模型 advertise 的 32-tool ToolRouter。

### 8.2 L3 架构偏移

ToolRouter 已能冻结 definition/dispatch identity，但缺少“provider 能否可靠消费该
Router”的 capability contract，也没有按 Scheduler 当前 phase 收窄能力面。单项
tool name/arguments 有 hard cap，单响应 tool-call 数量与组合仍没有专门上限。

### 8.3 待讨论问题

1. 启动 probe 是否必须完成一次真实 streaming function call；
2. ProviderCapability 应归 Invocation 基础层还是由 L3 ToolRouter 解析消费；
3. Scheduler 是否取消 record-only synthetic Lease，改用真实 phase-specific Lease；
4. `max_tool_calls_per_response`、effectful call 顺序和总 arguments cap 如何冻结；
5. capability 不满足时是拒绝启动、换模型还是选择无工具适配路径。

## 9. L5 Graph 当前结论：未到达，不新增 Runtime 问题

模型 reasoning 中出现 Graph 拓扑，不构成 L5 事实。只有以下事务成功后才存在图：

```text
GraphDraft persisted
→ ValidationReport accepted
→ GraphDefinition committed
→ start succeeded
```

本轮没有任何 Draft journal，因此暂不新增 L5 Runtime issue。后续仍应保持
Draft/Commit/Start，不因模型调用失败回退到一次性完整 Graph JSON 或 direct-answer。
待 L1–L4 能稳定交付合法 `create_graph_draft` 后，再验证 L5 authoring API 的最小
合法形态和可达性。

## 10. 外部 SWE Harness 观察项（非 L3 Harness Engineering）

当时已删除的 `run_task.sh` 把“root Task 已 blocked、所有 Agent idle”显示为
`terminal=quiet`。Judge 的 failed verdict 是正确的，但 terminal 标签隐藏了
`context_assembly_rejected`、`no_progress_budget_exhausted` 和
`progress_authority_failure` 的区别。

这是评测可观测性问题，不是 0/8 的原因，也不能与五层中的 L3 Harness 混为一谈。
后续可单独让 summary 投影 root terminal status/reason，但不能用脚本分类修饰框架
失败。

## 11. 建议讨论顺序

1. **SWE-015**：先关闭 Response→Replay 契约，否则长 reasoning 会稳定杀死下一轮；
2. **SWE-016**：修正最后一个 Attempt 的执行权；
3. **SWE-017**：恢复 Intervention recovery 闭环；
4. **SWE-019**：确定 ProviderCapability、真实 Lease 与阶段化 ToolRouter；
5. **SWE-018**：在机械边界稳定后做 Prompt/Tool cohort 对照；
6. 最后重跑单题，再决定是否已有资格讨论 L5 Graph authoring 行为。

以上条目已获批准并完成实现。外部 judge closure 仍必须来自新 Run；这里的
“完成”指架构实现与仓库验证完成，不把旧第六轮 0/8 改写成 resolved。

## 12. 修复落地与验证证据

### 12.1 SWE-015

- 新增 `context:default/v3` 与 `provider-replay:openai-compatible/v2`，v1/v2 保持历史 ref；
- Optional `reasoning` 超限形成 `assistant_reasoning + dropped`，无 WireItem；
- RequiredExact 使用真实下一轮 envelope，在任何工具执行前做 Response commit gate；
- ContextBinding 冻结 OutputBudget，completion/model/protocol reserve 真正闭合；
- Raw History 不可变，旧累计 Prompt 压缩键、递归摘要和三层有损压缩代码已删除。

### 12.2 SWE-016 / SWE-017

- per-Turn exhaustion 不再使用累计 Attempt 达上限作为阻断条件；
- 新 Attempt 在 durable rollover 前检查 future slot，`used == limit` 的当前 Attempt 正常执行；
- `runcontract.CompileDeadlines` 统一 execution/recovery/finalization 与 Graph 层级；
- 相邻 scope/action 使用1秒 handoff reserve；L4剩余 completion 下传到 Invocation。

### 12.3 SWE-018 / SWE-019

- Scheduler 生产 Prompt 改为小型 core + phase `task_control_context`；
- ToolRouter phase 为 draft-create/edit/validate-commit/start/recovery/final-report，
  新 Run 每轮只允许一个阶段动作，advertise/dispatch 同源；
- 默认 SDK 最多16个 Tool Calls、arguments total 128KiB；Scheduler phase 最大1个；
- `startup_probe=tool` 使用真实 function-call schema/arguments；text-only 不算通过；
- 完整大 ToolResult 先写 Task-scope ContentStore，再以 Ref/预览进入 L2 tombstone。

### 12.4 L5 typed Context 边界

- Graph Result/Evidence 不再拼接到 `Task.Description`；
- `Task.ContextInputs` 随 Store clone 与 Session snapshot 持久化，并在 L2 分别映射
  `upstream_result/upstream_evidence`；消除了 L5 96K runes 对 L2 user_task 64KiB
  的错层预算。

### 12.5 验证

- `go test ./... -count=1`：通过；
- `go test -race ./... -count=1`：通过；
- `go vet ./...`：通过；
- `go build -o ./agentgo .`：通过；
- 真实二进制：Web `/healthz` 200，Context/Graph Authoring/Outcome durable 文件存在，
  Ctrl+C 优雅关闭并移除 lock；
- 修复后首次 provider 单题：首轮 prompt 3847 tokens（此前约24K），成功
  `create_graph_draft → read_graph_draft → validate_graph_draft → patch_graph_draft`，
  authoring journal 非空；由此发现并补齐 Graph authoring tool 的 coordination
  progress 映射。最终映射后的重跑待用户明确批准向外部 provider 发送任务代码。
