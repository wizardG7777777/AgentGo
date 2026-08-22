# SWE/集成测试问题清单 — 2026-08-21 23:29（第五轮 Flask SWE 分层诊断）

> 管理约定见首份文档
> [`2026-08-20-1508-swe-round3-open-issues.md`](2026-08-20-1508-swe-round3-open-issues.md)。
> 本文记录第五轮批测结果，并按
> [`五层工程架构规范`](../design/five-layer-engineering-architecture.md)
> 归类问题。正文保留事故证据与验收方向；文末实施补记只记录状态和验证口径，
> 不替代各设计文档或路线图。

## 背景

第五轮 8 题 Flask SWE 批测使用 Nebius
`deepseek-ai/DeepSeek-V4-Flash`、全角色统一模型、禁网、每题外部时限 1200 秒。
批测完整结束，最终成绩 **2/8 resolved**：

| 考题 | verdict | terminal | wall | LLM calls | prompt | completion | patch |
|---|---|---|---:|---:|---:|---:|---:|
| ipv6-server-name | failed | timeout | 1203s | 6 | 160,109 | 130,154 | 0 |
| ipv6-session-txn | resolved | graph_done | 950s | 51 | 996,495 | 156,760 | 13 |
| secret-key-rotation | failed | graph_done | 340s | 6 | 157,526 | 52,687 | 0 |
| context-push-order | resolved | quiet | 1126s | 40 | 760,991 | 212,245 | 22 |
| session-access-tracking | failed | quiet | 1177s | 72 | 1,572,080 | 250,983 | 0 |
| automatic-options | failed | timeout | 1202s | 61 | 1,089,687 | 231,918 | 0 |
| teardown-callbacks | failed | graph_done | 1032s | 49 | 868,487 | 105,047 | 0 |
| pass-context-dispatch | failed | timeout | 1202s | 4 | 54,952 | 19,311 | 0 |

六个失败题合计 **198 次 LLM 调用、3,902,841 prompt tokens、790,100
completion tokens、0 行补丁**。因此本轮主故障发生在有效代码修改之前，不是
补丁写坏或测试篡改。两个 resolved 题证明 Graph→Task→Runner→工具→结果路径
能够成功，但成功成本仍异常高，不能以 2 个通过案例证明行为已健康。

原始证据（临时测试目录，可能被下一轮覆盖）：

- `/tmp/agentgo-swe/runs/summary.json`
- `/tmp/agentgo-swe/runs/<task>/snapshot.final.json`
- `/tmp/agentgo-swe/worktrees/<task>/.agentgo/sessions/<session>/turns.jsonl`
- `/tmp/agentgo-swe/worktrees/<task>/.agentgo/sessions/<session>/logs/system.log`
- `/tmp/agentgo-swe/worktrees/<task>/.agentgo/state/taskmem/*.json`

## 分层总览

| 层级 | 本轮判断 | 主要证据 |
|---|---|---|
| Model Invocation 基础层 | P0 敞口 | 超大 reasoning/content、SSE 直到 HTTP deadline、tool-call JSON 截断、DSML 泄漏 |
| L1 Prompt | 有放大问题 | Scheduler 多次生成损坏长 JSON；“先交 root+end 骨架再 patch”与真实生命周期冲突 |
| L2 Context | P0 敞口 | 单个超大模型输出可进入 History/ExtraFields；仅有累计压缩，缺少单项硬上限 |
| L3 Harness | 核心防线多数生效 | 越界路径、畸形工具名、未注册工具均被 fail-closed；但 Tool/Context/Invocation 仍混在 LLMExecutor |
| L4 Loop | P0 漂移 | 49–72 轮调查零写入仍不收敛；no-progress、deadline、retry 分散在 Prompt/watchdog/harness |
| L5 Graph | P0/P1 架构缺口 | end-only 图可无工作完成；失败 end 与成功 end 都表现为 Graph completed |
| Validation/Trace | 证据足够但非权威 | 能还原事故；外部 harness deadline 与内部 watchdog 没有形成同一 Run contract |

## 旧 ID 状态更新

### SWE-003 长 JSON 生成失控（partially-fixed；残余拆分并入 SWE-012/013，structured-output experiment-open）

第五轮再次出现长 JSON 和格式能力退化：

- `automatic-options` 至少 7 次 `submit_graph` 调用，多次在偏移
  107/647/1208/515/880 等位置发生 JSON 语法损坏；
- `ipv6-server-name` 出现 `unexpected EOF`、换行进入 string literal；
- `secret-key-rotation` 两次 Graph JSON 损坏后退化为 end-only 骨架；
- `session-access-tracking`、`automatic-options`、`teardown-callbacks` 出现
  DSML 泄漏和畸形工具名；
- 多题单轮 reasoning/content 达 100K–275K 字符。

已落地的畸形工具名清洗正常工作，因此 SWE-003①/SWE-009 的框架缓解没有
回归。2026-08-22 完成重分类后，SWE-003 不再作为单一实现项目：

| 原残余 | 最终归属 |
|---|---|
| 完整长 Graph JSON / 分段构图 | 并入 [`Graph Draft / Commit / Start`](../design/graph-draft-commit-start.md) 的原生 Draft patch |
| 流式 output/tool arguments hard cap、partial no-dispatch | 并入 [`Context Snapshot / Item Budget`](../design/context-snapshot-item-budget.md) 的 Invocation Slice |
| malformed 后无界 Retry | 并入 [`Loop Progress Contract / Checkpoint / Deadline`](../design/loop-progress-checkpoint-and-deadline.md) |
| structured output / response_format | 保留为窄 provider capability 实验，不作为框架正确性前提 |

structured output 只可在真实 provider/model probe 证明 strict function schema、
JSON Schema 子集、streaming 和 tool calling 兼容后按 capability 启用；不支持时仍
由 framework hard cap、validator 和 no-dispatch 保证安全。

### SWE-004 Graph 终态语义（既有五切片 fixed；残余 subsumed-by-SWE-012，implementation-landed/validation-open）

`session-access-tracking` 与 `teardown-callbacks` 均走
`impl failed → end_impl_failed`，业务目标未达成，但顶层 Graph 状态为
`completed`。Scheduler 最终文本能够解释失败路径，不等于运行时具有可机械消费
的业务 outcome。

残余子问题 3“到达任意 end = graph completed，成功/失败收官不可区分”本轮
再次实证。该残余已由 [`Graph Draft / Commit / Start`](../design/graph-draft-commit-start.md)
的 typed `EndOutcome`、GraphStatus 推导和 Slice 6 Outcome/Recovery/UI 完整吸收，
不再建立第二套 Graph Outcome 设计。typed EndOutcome/GraphStatus、TaskOutcome
adapter/outbox 与 UI 投影已经落地，但在 SWE-012 完整回归前，本残余保持
`subsumed-by-SWE-012 / implementation-landed / validation-open`。历史事故不是
Graph terminal durability 回归；当前剩余的是 closure 证据而非第二套语义设计。

### SWE-008 / SWE-009 直答后门与 DSML 缓解（框架修复维持，模型格式敞口继续）

未观察到第四轮同款“二次纯文本直接放行垃圾”的主导路径。图节点纯文本退出
nudge、畸形工具名清洗、retry/fail-closed 均有真实触发记录。但这些防线只能
拒绝坏动作，不能让模型恢复结构化调用；结果变成多 Attempt 空转和失败收官。

### SWE-010 watchdog → Scheduler 接线（接线 fixed；行为有效性本轮不可验证）

Graph 节点缺省 Task timeout 为 3600 秒，本批外部 harness 在 1200 秒收割。
因此 watchdog 的 Graph 唤醒接线通常来不及触发，不能保护本轮，也不能作为
下一轮有效性证据。该问题不推翻 SWE-010 的接线修复，但暴露新的 deadline
层级缺口，纳入 SWE-011。

## SWE-011 L4 无进展收敛缺失 + deadline 层级倒置（P0，implementation-landed，validation-open）

**层级归属**：L4 Loop 主责；L2 Context、Model Invocation 和运行时控制面协同。

**已接受修复方案**：[`Loop Progress Contract / Checkpoint / Deadline 架构`](../design/loop-progress-checkpoint-and-deadline.md)。

### 现象

- `automatic-options`：61 次调用、零文件、零补丁，外部 timeout 时节点仍
  processing；
- `session-access-tracking`：72 次调用，`read_file×23`、`run_shell×22`、
  `grep_search×18`，零 write/edit；多次定位同一根因、跑测试、输出拟议代码，
  仍未落地修改；
- `teardown-callbacks`：49 次调用、零文件，多次空响应/纯文本收口/畸形工具；
- 内部 Task timeout 3600 秒大于外部 1200 秒，watchdog 无介入窗口；
- 当前 emergency fuse 只防 10000 轮程序性死循环，不是正常 no-progress 策略。

### 架构偏移

五层规范要求 L4 拥有 retry、deadline、no-progress、finalizing 和 stop。当前
no-progress 主要依赖 L1 Prompt 的“打转自查”、Suggestion 重复熔断、watchdog
和外部 harness；L4 没有基于 Effect/Artifact/文件版本/confirmed Fact 的机械
进展判据。L4 责任被分散给其它层，属于架构偏移，不只是模型能力不足。

### 提升方向

1. 架构定义封闭进展语言与 policy，Scheduler 声明工作意图，框架编译、验证并
   将 `CompiledProgressContract` 随 Activation 冻结。
2. L3 将 settled Turn 投影为中性的 `TurnSettlementDelta`；L4 结合冻结契约生成
   `ProgressAssessment`。任意 Tool/Effect/TaskMemory version 或文件变化均不能
   自动等价为进展，重复与振荡必须机械识别。
3. 独立持久化 L4 `ProgressCheckpoint`：no-progress 时间/usage、最近指纹、预算
   和干预阶段在 retry、重新认领、重启、Context 压缩后保持连续。
4. 分级干预：Reminder → AttemptRollover → typed InterventionRequest → blocked；
   cancelled 保留给用户/系统主动撤销。
5. 统一绝对时限：operation < Attempt < Activation < Graph < Run；外部 harness
   deadline 进入 `RunContract`，并为 recovery/finalization 预留窗口。
6. action 前 durable 预留预算，settled Turn 后先写 Delta/Assessment/Checkpoint
   再允许下一 action；权威写失败 fail-closed。
7. Watchdog 退回 heartbeat/checkpoint、执行者失联和 hard deadline 的 liveness
   backstop，不再承担正常语义进展判断。
8. Checkpoint + append-only Delta 支持恢复、重放和未来诊断分支；回溯计算状态
   不得撤销或静默重放已发生 Effect。

### 验收标准

- 同类真实题中，连续无契约认可进展的节点在 1200 秒前被机械介入并 durable
  收口；重复 Shell/Effect settled 不得刷新交付进展；
- 不再出现 40+ 调用、零写入、仍 processing；
- 有持续机械进展的复杂任务不因固定轮数被截断；
- retry/重启不能重置停滞预算，外部 harness 不再成为首个正常终止者；
- Trace 能区分 progress、no-progress、regression、oscillation、intervention 和
  terminal decision。

## SWE-012 骨架建图建议与 finalizing 冲突，end-only Graph 无工作完成（P0，implementation-landed，validation-open）

**层级归属**：L5 Graph 主责；L1 Prompt/工具回执和 L4 finalization 形成跨层冲突。

**已接受修复方案**：[`Graph Draft / Commit / Start 事务化构图架构`](../design/graph-draft-commit-start.md)。

### 现象

`secret-key-rotation` 前两次 Graph JSON 损坏。错误回执建议“先提交仅含
root+end 的骨架图，再经 patch_graph 逐次扩展”。第三次提交成功的图实际为：

```text
root = done
done.kind = end
```

Graph 在没有 controller/agent/tool/acceptance 节点、没有 Task、没有 Effect、
没有 Artifact 的情况下立即 `completed`，最终 patch 为 0。

### 架构冲突

1. `submit_graph` 成功后 origin Scheduler Task 立即 finalizing；
2. origin Scheduler 无法继续 `patch_graph`；
3. root=end 让 Graph 立即终态；
4. Graph 已终态后不存在可靠的骨架扩展窗口；
5. Graph validator 只证明 schema/拓扑合法，没有证明该图覆盖用户请求所要求的
   工作和验收。

这是确定性的跨层协议矛盾。模型 JSON 损坏是触发器，但“合法接受无工作图并
宣告 completed”属于框架责任。

### 已接受的架构更改

1. 将 L5 拆为 `GraphDraft → GraphDefinition → GraphExecution` 三对象；
2. `commit` 与 `start` 分离：commit 只验证和 durable Definition，零 Activation/
   Task/Effect；
3. Scheduler 在读取 commit 结果后显式决定 `start_graph`；只有 start 成功才
   finalizing origin；
4. commit 前验证由框架机械执行，Scheduler 不能自评或绕过；
5. 新图必须满足最小合法图规则：root 非 end、至少一个产出 Result 的执行节点、
   typed end outcome、完整 terminal outlets、无零步 success；
6. GraphDraft 绑定 GraphContract，机械检查 deliverable、Effect、Artifact、check
   和 acceptance 覆盖；语义覆盖由独立 Proposal Acceptance 核验；
7. 运行中修改统一走 GraphChangeProposal，commit 新 revision 且只影响未来
   Activation；
8. 新 authoring 工具使用原生结构 patch，退役完整 Graph JSON 字符串的
   JSON-in-JSON 提交方式；
9. 不单独落地 root=end 拦截等止血改动，按独立设计文档的迁移切片整体交付。

### 验收标准

- Draft 可分批 patch，commit 前无运行时副作用；
- 代码修改请求的 end-only/zero-work/zero-result Graph 被机械拒绝；
- commit 不激活，Scheduler 显式 start，start 成功后才 finalizing；
- EndOutcome 正确区分 completed/failed/blocked/cancelled；
- 运行中 ChangeProposal 不改变在途 Activation；
- 崩溃恢复不会激活半构造图或重复 root/Task/Effect；
- 设计文档“完成定义”全部满足后才可将 SWE-012 标为 fixed。

## SWE-013 单个 Context item 无硬上限，异常响应污染后续轮次（P0，implementation-landed，validation-open）

**层级归属**：L2 Context 主责；Model Invocation 提供有界响应契约，L4 决定
超限后的 Attempt 行为。

**已接受修复方案**：[`Context Snapshot / Item Budget 架构`](../design/context-snapshot-item-budget.md)。

### 现象

第五轮观察到单轮：

- reasoning 最高约 241K 字符；
- assistant text 最高约 275K 字符；
- DSML/自言自语/重复文本随 History/ExtraFields 进入后续轮次；
- 当前主要依靠累计 prompt token 阈值触发历史压缩，缺少“任一单项不得超过
  固定上限”的硬约束。

### 2026-08-22 实施后复测回归：静态 Prompt 与 v1 policy 不兼容

完成首轮 ContextCompiler cutover 后，Flask 8 题批跑全部在 7–8 秒内以
`quiet/failed` 收口。八份最终 Snapshot 的机械事实完全一致：Scheduler Task
`status=blocked`、`session_call_count=0`、`fragment_limit_exceeded`、零补丁；并非
八道题分别做错。实际冲突是 Scheduler `agent_role` 约 51 KiB，而冻结
`context:default/v1` 只允许 48 KiB `prompt_component`。Harness 的 `quiet` 只是
“已无 pending/processing”的观察分类，不能解释成模型完成。

修复不覆写 v1：新增 `context:default/v2`（Prompt Component 64 KiB、system
section 96 KiB），新 Run 统一绑定 v2，历史 v1 继续按原 digest/预算恢复；并在
Bootstrap、Team Provision、ad-hoc Spawn 产生执行面副作用前复用真实 L2 编码路径
预检静态 Prompt。L4 同步把该终态 reason code 从误导性的
`invocation_deadline` 改为 `context_assembly_rejected`。相关 contract/focused
测试已加入；按用户要求未在本次修复后代跑 SWE，因此仍为
`implementation-landed / validation-open`。

### 架构偏移

L2 应保证所有进入模型 Context 的 fragment 有来源、预算和处置。首轮诊断时，
`buildMessages`、HistoryEntry、ExtraFields、Task/Session Memory 和 Manifest
尚未共同产出一个带单项硬上限的 ContextSnapshot；异常 Model response 可以
直接成为巨大历史项，L2 责任没有在唯一装配边界落实。当前生产主链已经 cutover，
本次复测回归属于版本化预算与静态 L1 产物没有在装配期对账，而非重新出现旁路。

### Codex 参考范式

本地 Codex checkout 的仓库约束明确要求：所有注入模型 Context 的 item 必须
有 hard cap，单项不得超过 10K tokens；每次 sampling 捕获一个 StepContext，
同一 ToolRouter 同时提供真实 Registry 与 model-visible specs，确保 Context、
展示工具和实际 dispatch 使用同一请求视图。

参考路径（只作范式比较，不作为 AgentGo 运行依赖）：

- `/Users/yanchenyu/Documents/ClawSeries/codex/AGENTS.md`（Model visible context）
- `codex-rs/core/src/session/step_context.rs`
- `codex-rs/core/src/session/turn.rs`
- `codex-rs/core/src/tools/router.rs`

### 提升方向

1. 所有 model-visible 内容先转为 `ContextFragment`，只经唯一
   `ContextCompiler` 生成不可变 `ContextSnapshot{Messages, ToolSpecs, Manifest}`；
   Manifest 与真实 wire 同源。
2. 预算按 provider 字段/单项 hard cap → `ProtocolAtomicGroup` → section →
   snapshot total → completion reserve 执行；总预算不能替代单项预算。
3. 大型 ToolResult/Memory/上游 Result 转稳定 ContentRef/ResultRef/EvidenceRef
   和有界摘要；Ref 不扩大 ExecutionLease。
4. system、用户任务、输出契约、tool schema、tool arguments 和 provider-required
   replay 字段禁止静默截断；unknown replay policy fail-closed。
5. Model Invocation 对 content/reasoning/ExtraFields/tool arguments/response total
   流式计量并早夭；partial response 不入 History、不 dispatch tool call。
6. Context、advertised tools 和 actual dispatch 共享冻结 ToolRouter snapshot；
   Context policy/provider replay contract 改变时创建新 Attempt。
7. oversized raw content 按 retention/security policy 隔离或外置；Trace 只记录
   尺寸、digest、Disposition、policy 和 Ref。

### 验收标准

- 任一发送给模型的单项均有可测试 hard cap；
- Manifest 与实际发送 Messages/ToolSpecs 同源；
- 超大 response 不会原样进入下一轮；
- 超限策略不会破坏 tool calling/provider replay 原子组，未知时 fail-closed；
- partial response 不进入正式 History 或工具派发；
- 合法大 Result 可通过稳定 Ref/Evidence 消费，不能因 hard cap 丢失数据谱系。

## SWE-014 `context deadline exceeded` 被误判为 context-window overflow（P1，implementation-landed，validation-open）

**层级归属**：L4 Loop 错误分类；根因是 Model Invocation 类型在跨层桥接后
退化为字符串。

**已接受修复方案**：[`Invocation Failure / Loop Recovery 契约`](../design/invocation-failure-and-loop-recovery.md)。

### 现象与根因

`internal/agent/agent.go::isContextOverflow` 当前使用：

```go
strings.Contains(msg, "length") ||
strings.Contains(msg, "截断") ||
strings.Contains(msg, "context")
```

因此网络/HTTP 调用的 `context deadline exceeded` 也命中 `context`，日志错误
记录“检测到上下文溢出”，并执行三层历史压缩机制中的 Layer 3 激进压缩
（不是五层架构的 L3 Harness）。第五轮在
`ipv6-server-name`、`automatic-options`、`pass-context-dispatch` 等题真实触发。

### 影响

- 误导排障和 Trace 归因；
- 对非 context-window 问题执行错误恢复动作；
- retry 后可能丢弃仍有价值的历史，而没有修复真实的请求超时；
- 掩盖 Invocation timeout 与 Context overflow 应采用不同 policy 的事实。

### 提升方向

1. Model Invocation 返回封闭的 typed error/code；
2. L4 只按类型分支，禁止解析自由文本；
3. 明确区分 transport timeout、context window exceeded、finish_reason=length、
   malformed response 和 user cancellation；
4. 增加 `context deadline exceeded` 的阴性回归测试。

### 验收标准

- `context deadline exceeded` 不触发 Context L3 压缩；
- 真正 context-window overflow 仍触发正确策略；
- Trace 保留原始分类和跨层稳定错误码。

## L5 残余：Graph Outcome（SWE-004 子问题 3，已并入 SWE-012）

本轮不新建重复 ID。正式设计已进入
[`Graph Draft / Commit / Start`](../design/graph-draft-commit-start.md) 的
`EndOutcome`、GraphStatus 推导、durable transition settlement 和 Slice 6；以下
要求作为 SWE-012 的验收子集，不再单独实现：

- Graph 生命周期终态与业务 outcome 分离；
- `end_success`、`end_failed`、`end_blocked` 可机械区分；
- Scheduler/harness 无需解析 end 标题或最终文本；
- `graph_done` 不能让调用方误判业务成功；
- outcome 与到达 end 的 transition 同一 durable 事务产生。

## Codex 范式对照结论

Codex 不是 AgentGo L5 Graph 的直接模板，但其局部 Agent Harness/Loop 有三项可
复用原则：

1. **每次 sampling 捕获唯一 StepContext**，模型、权限、环境、MCP 与工具面
   属于同一个请求快照；
2. **ToolRouter 同时持有 runtime Registry 与 model-visible specs**，防止模型
   看见的工具与实际可执行工具漂移；
3. **所有 model-visible context item 必须有单项 hard cap**，累计压缩不能替代
   单项边界。

AgentGo 下一轮不应复制 Codex 的产品拓扑，而应把上述范式落实为自己的
`ContextSnapshot + ToolRouter/ExecutionLease + Loop Controller`。

## 评估结论与实施入口

1. **SWE-014**：主要实现已落地、验证开放，见 Invocation Failure / Loop Recovery 契约；
2. **SWE-013**：主要实现已落地、验证开放，见 Context Snapshot / Item Budget 架构；
3. **SWE-011**：主要实现与两阶段终态事务已落地、外部验证开放，见 Loop Progress
   Contract / Checkpoint / Deadline 架构；
4. **SWE-012**：主要实现已落地、legacy 退出与验证开放，见 Graph Draft /
   Commit / Start 架构；
5. **SWE-003**：残余已拆分并入 SWE-012/013/011；只保留 structured-output
   provider capability 实验；
6. **SWE-004**：残余已并入 SWE-012 typed EndOutcome，不单独设计；
7. 统一依赖顺序、共享类型、迁移切片和回归门槛见
   [`SWE 架构修复统一实施路线图`](../design/swe-architecture-repair-roadmap.md)。

本轮问题调查、五层归类和修复方案评估至此结束。设计接受不等于修复完成；
运行时代码、配置、测试和 legacy 退出必须按统一路线图逐门交付。本批记录本身
不授权修改 `AGENTS.md`。

### 2026-08-22 实施状态补记（文档收口）

四个主问题的主要生产实现已经进入仓库：typed Invocation failure/output cap、
ContextCompiler/ContentStore、ProgressContract/LoopStore/checkpoint/deadline、
Graph Draft/Definition/commit/start/ChangeProposal、typed EndOutcome、durable
TaskOutcome/terminal adapter/delivery outbox、UI 与 typed OutputContract enforcement。
LoopIntervention 也已通过 outcome-delivery-gated reliable bridge 接到脱图的
Scheduler coordination wake：source outcome/Graph settlement 先完成，wake 自身的
durable outcome delivery 完成后才 Ack 原 command。
相关 focused/恢复/包测试、终态 focused race 和全仓 compile-only 已通过。

两阶段 TerminalIntent/checkpoint Seal/Outcome/Task CAS、pending intent recovery、
直接 LLM 的 `ContextBinding → Invoke` 生产入口，以及 per-(Agent, Run, Session)
Mailbox envelope/wake/drain/ACK/steer 隔离也已完成代码收口。`current_unsealed`
只保留为历史/legacy schema 事实；新 ProgressContract Task 先 Seal 再提交 Outcome。

这仍不构成 issue closure。本轮没有重新运行 Flask SWE，也没有完成全仓运行测试、
vet、三平台、真实二进制或真实 provider 多 rollout。L5 legacy submit/patch、provider
capability/replay、真实 tokenizer 与兼容 wrapper 删除仍未达到退出门。因此
SWE-011/012/013/014 统一记为 `implementation-landed / validation-open`；实时细节
以路线图 §1.1/§1.2 为准。

### 后续批次

2026-08-22 15:10 的第六轮已执行并以 0/8 结束；它证明 v2 静态 Prompt cutover
生效，同时暴露 SWE-015/016/017/018/019。第五轮文末“没有重新运行 Flask SWE”
是当时的历史证据口径，不再代表当前状态。后续事实与讨论入口统一见
[`第六轮 0/8 分层诊断`](2026-08-22-1510-swe-round6-zero-of-eight-layered-diagnosis.md)。
