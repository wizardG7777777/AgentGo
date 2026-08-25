# SWE L1-L5 Observation、Recovery 与 Finalization 主链修复

> 日期：2026-08-24
> 问题：SWE-037 / SWE-038 / SWE-039 / SWE-040
> 归层：L1 Prompt / L2 Context / L3 Harness / L4 Loop / L5 Graph
> 状态：closed / local-validation-complete / external-validation-complete

## 1. 事故分类

本轮 SWE 暴露的是四个相互级联、但权威层不同的问题：

| 编号 | 权威层 | 事故形状 |
|---|---|---|
| SWE-037 | L5 | graph-ended final-report 的 intervention 被通用非 Graph pump 当成新请求，可能创建第二个顶层业务 Graph |
| SWE-038 | L2 + L4 | 新读取证据没有成为可持久化进展，压缩、rollover 或 intervention 前又缺少冻结 Observation，导致重复调查或丢失已确认事实 |
| SWE-039 | L5 | recovery 只提交 `decision=retry`，没有证明输入、Definition 或策略发生变化，可能把同一失败原样重放 |
| SWE-040 | L1 + L3 + L4 | Worker 对 grep/pipeline 的使用契约不清，final-report 还能自然文本退出或继续探索，机械工具协议没有强制收口 |

SWE-037 的直接根因是 L5 `ControlScope` 缺失；L4 intervention 只是触发条件，
不应拥有创建 Graph 的权力。因此它不是“L4 先坏、L5 被动级联”，而是 L5 对
控制任务身份的分类和吸收顺序不完整。

## 2. L1：稳定指令

- Worker 明示 `grep_search` 默认 literal，regex 必须显式选择；测试判定命令禁止
  pipeline/重定向。
- 连续调查后必须记录 ObservationDelta，并转入编辑、验证或 blocked。
- final-report 只消费冻结 `GraphTerminalSummary`，有限补读结果后必须
  `report_done`；禁止 Graph authoring 和重新执行业务工作。
- recovery 的 `decision=retry` 必须同时给出合法 RecoveryDelta；没有可验证变化
  只能 blocked。

这些文字解释机制，但不承担权限、重试和终态的权威判断。

## 3. L2/L3：ObservationDelta 与受控动作

- 新增 `agentgo.observation-delta/v1`：只保存带 EvidenceRef 的 confirmed facts、
  下一步候选和 phase，不保存 reasoning、工具参数或原始大正文。
- Observation Store 为 append-only immutable record，稳定引用为
  `observation:sha256:*`；TaskMemory 只投影最新 ref、confirmed facts 和下一步。
- `record_observation_delta` 是 framework control tool。code-change/v4 的 Graph
  agent Lease 自动获得该工具，不依赖用户 profile；旧冻结 v1-v3 不被扩大。
- `agent:observation-checkpoint` 使用 `reasoning=none`、exact tool choice，只能
  调用一次 `record_observation_delta`。证据必须属于当前 Task/Attempt 的 settled
  tool call 或 artifact。
- `grep_search.pattern_mode=literal|regex`，默认 literal；非法 regex fail-closed。
  Shell pipeline 仍默认拒绝，并返回稳定 reason code 与去除 pipeline 的建议。
- `agentgo.graph-terminal-summary/v1` 只含 Graph/Run 身份、typed outcome、终态
  节点和安全 refs；graph-ended Task 不再嵌入完整 GraphDocument。

## 4. L4：进展、checkpoint 与 bounded finalization

- 新任务使用 `progress:code-change/v4`；v1-v3 仅供冻结任务恢复。新的 Evidence
  digest 是 knowledge progress，重复 digest 不刷新时钟；探索超过 4 次进入既有
  forced-delivery 阶段。
- Attempt rollover、首次历史投影压缩和 intervention 前必须有当前
  ObservationDelta；缺少时先进入 exact checkpoint。checkpoint 失败以
  `observation_checkpoint_failed` fail-closed，并保留原始历史。
- final-report 使用 `progress:final-report/v1` 与 `WorkFinalization`：最多两个
  探索 turn，随后 exact `report_done`，禁止普通 Attempt rollover。
- final-report 的 provider、格式或 intervention 失败由 L5 注入的确定性 fallback
  收口；只有 fallback 本身失败才 blocked。

## 5. L5：ControlScope、唯一 Graph 与 RecoveryDelta

- 统一 `ControlScope` 按结构化字段分类 Graph Activation、Recovery Controller、
  FinalReport、Root Authoring 和 legacy control task。FinalReport 必须同时满足
  `FinalReportGraphID`、`RunPhase=finalization`、`EventSource=graph-ended`。
- FinalReport intervention 在通用非 Graph pump 前被吸收，绝不创建 Draft、Graph
  或 Worker。
- 同一 Run 只允许一个顶层业务 Graph；第二次 commit/start fail-closed。子图与
  同图 GraphChangeProposal 不受影响。
- 新 simple Graph recovery opt-in `agentgo.recovery-delta/v1`。retry 必须引用
  failure checkpoint/observation、匹配 failure fingerprint，并声明非空变化维度、
  策略、首个必做动作和预期里程碑。
- Definition 不变时，delta 作为 `recovery_directive` InputBinding 注入下一
  Activation；Definition 变化时必须证明 revision 前进。相同 fingerprint 不得
  重复提交无新增 delta 的 retry。旧 Graph 未声明 schema 时继续按旧契约恢复。
- L5 的 finalization fallback 只读取冻结 TerminalSummary，生成 typed outcome、
  失败点和恢复条件，不读取 reasoning、不调用业务工具。

## 6. 本地验证与外部状态

本次只执行仓库 Python 单测、Go focused/full/race/vet/build、diff-check 与本地
fake-provider 真实二进制启动；没有调用 DeepSeek/OpenRouter，也没有运行
`SWEtest` 或 Flask 8 题批测。

真实二进制 smoke 使用 `scripts/local_fake_provider_smoke.py`，以本地 Responses
服务驱动 `create→configure→validate→commit→start`、12 次重复读取、exact
Observation checkpoint、Attempt 2 写入、独立 acceptance、两个 final-report
补读 turn 和 exact `report_done`。最终安全摘要为：

```json
{"schema":"agentgo.local-fake-provider-smoke/v1","graph_status":"completed","graph_outcome":"success","top_level_graphs":1,"worker_reads_before_checkpoint":12,"observation_records":1,"final_report_reads":2,"final_report_status":"completed"}
```

该 smoke 还在首次运行中发现并修复了一处装配遗漏：framework 自动工具曾进入
synthetic acceptance Route ceiling，导致 verifier Lease 因包含
`record_observation_delta` 而 fail-closed。现 synthetic acceptance/旧未知角色先
应用只读正向闭集；显式越权 capability 与旧不安全 Lease 仍被拒绝。

最终本地验收（2026-08-24）全部通过：

- `python3 -m unittest scripts/swe_harness/harness_test.py`：30 项；
- `go test ./...`；
- `go test -race ./...`；
- `go vet ./...`；
- `go build -o agentgo .`；
- `git diff --check`；
- `python3 scripts/local_fake_provider_smoke.py --binary ./agentgo`。

外部关闭证据（2026-08-25）：指定监控任务完成当前 8 题批次，8/8
task/architecture/infrastructure success；每题单顶层 Graph typed success，
final-report completed、Outcome ACK、pending delivery=0，known incidents 与 hard
kill 全为 0。Observation/Recovery/finalization 主链在当前二进制完整运行，
SWE-037～040 关闭。
