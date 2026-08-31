# SWE-058～062：统一弱模型下的 L1–L4 适应性缺口

> 状态：Closed / mechanical and Qwen business gate verified
> 日期：2026-08-28
> 范围：L1 Prompt、L2 Context/Replay、L3 Invocation/RunBudget、L4 Progress/Deadline
> 非目标：切换模型、provider 回退、多 provider、修改 L5 Graph 拓扑或路由

## 1. 真实暴露证据

Windows testbed 的最新八个逐题结果位于
`%LOCALAPPDATA%\AgentGo\swe\runs\<task>\result.json`；对应 trace 位于
`%LOCALAPPDATA%\AgentGo\swe\worktrees\<task>\.agentgo\sessions\*\logs\*.jsonl`。
`teardown-callbacks` 的 `execution_lease_frozen` 明确记录全部角色使用
`qwen3.8-flash`。这八个逐题结果的机械汇总为：

- `architecture_ok=5/8`，`task_resolved=4/8`；
- prompt tokens `15,163,385`，model calls `314`；
- `ipv6-server-name`、`secret-key-rotation` 为
  `graph_terminal_incomplete_final_report`，并各遗留 1 条 active reservation；
- `teardown-callbacks` 记录 7 次 `observation_submission_invalid`，最终业务 blocked；
- 全部使用 `agentgo.run-contract/v1` 与 `context:default/v9`。

当前 `%LOCALAPPDATA%\AgentGo\swe\runs\summary.json` 只绑定同一
`.batch_start` 后前三题的 architecture gate，后五题是随后逐题运行；因此这组数据
只能作为缺陷暴露证据，不能冒充完整 batch summary。关闭本问题必须重新运行原
Flask-8 batch，不能用单题补跑拼接。

## 2. 分层问题

| ID | 层 | 机械缺口 | 观察后果 |
|---|---|---|---|
| SWE-058 | L2 | Context v9 把普通 Fragment/section cap 扩大到完整模型输入窗口；Observation 后旧探索交换仍逐轮重放 | `pass-context-dispatch` 单题 prompt `5,548,613`，重复探索持续放大 |
| SWE-059 | L4/L1 | knowledge progress 与 decision progress 共用时钟；周期 Observation 格式失败可以反复回到业务阶段 | 新 grep 可持续刷新进展；`teardown-callbacks` 出现 7 次 Observation invalid |
| SWE-060 | L4/L3 | RunContract 只有 execution/recovery/finalization，没有独立 verification reserve | 实现、验收与最终报告争抢尾部窗口，已有代码也可能因验收/收口时间不足 blocked |
| SWE-061 | L3/L4 | pre-dispatch、取消/期限不确定和 finalization 期限分支没有统一关闭 reservation/fallback | 两题 final-report 不完整并各遗留 1 条 active reservation |
| SWE-062 | L1/L4 | 正确补丁与 frozen check 已存在时，uniform weak-model verifier/terminal settlement 仍可能把业务结论判 failed/blocked | 完整批次外部 Judge `5/8` resolved，但正式 `task_resolved=0/8` |

## 3. 已落地修复

- `context:default/v10` + Replay v4：ModelCapability 只调整 snapshot 总预算、
  completion reserve 与 RequiredExact provider replay；普通 Prompt/ToolResult/
  TaskMemory/ToolDefinition cap 保持稳定。最新 Observation 之前的探索结果转为
  task-scoped ContentRef；同 task/attempt/path/content digest 的旧 read/grep/list
  副本只保留引用，最新 preview 保留。Manifest 记录 disposition、reason、digest
  与 ContentRef；Raw History 不修改，Responses 原子组不拆分。
- `progress:code-change/v6`：knowledge checkpoint 改为每 6 turn；
  `DecisionStagnationCount` 只由 phase/workspace/typed check/artifact/predecessor
  closure 等机械信号重置。连续两次无决策前进产生
  `decision_progress_stalled`；连续两次 control contract 失败以
  `control_contract_unstable` blocked。
- `agentgo.run-contract/v2`：新增 verification reserve，顺序为
  execution → verification → recovery → finalization。v1 解码与旧 deadline 语义
  保持不变；v2 Graph acceptance 只增加 verification phase 标记，不改节点/边。
  SWE 默认冻结 180s verification、120s recovery、90s finalization。
- Observation control OutputBudget 冻结为 2048 completion tokens、16 KiB tool
  arguments、32 KiB response；首次失败使用新的 control projection，第二次失败
  不做正文提取或 JSON 修补。
- RunBudget reservation 在 pre-dispatch 时 cancelled；已 dispatch 明确失败为 failed；
  取消/期限不确定为 unknown。process/Attempt 退出统一关闭遗留 reservation；Store
  `Close` 不伪造结算。finalization phase 无可执行窗口时直接使用现有确定性
  TerminalSummary fallback，保证 final-report Task 终态。
- RecoveryStartPermit 的 claim 身份现按 Task durable 恢复：同一 Activation 新
  Attempt 不重复认领；无效 recovery delta 在 bind 后校验失败时立即取消 permit，
  未认领 permit 不会等过期后伪结算 unknown model call。SWE Test Runner 只有在
  Graph、final-report 与当前 Run 全部 Task 都 terminal 后才结束监控。
- Observation control 的动态 evidence enum 与 handler 使用同一当前
  Task/Attempt authority；前一 Attempt、累计 artifact 与 Graph 上游 `ev:*` 不进入
  control projection。facts/post-predecessor/candidate enum 同源形成有界 system
  catalog，首次失败的具体机械错误只回显给唯一一次 fresh retry；仍不做正文提取、
  JSON 修补或 provider 特判。
- `submit_task_result.result` 的 JSON Schema 直接排除系统保留键；recovery
  strategy/action/milestone 的 600 字符上限也钉入 schema，避免模型先产生 Runtime
  必拒的结构。
- SWE Worker/Verifier prompt 只解释上述机械行为：假设→mutation→typed check、
  decision stagnation、一次 control retry、verification reserve；不含模型/provider
  名称分支。

## 4. 关闭门

1. full/race/vet/build、SWE Test Runner Python tests 与 fake Responses 二进制链通过；
2. 定向运行 `context-push-order`、`pass-context-dispatch`、`teardown-callbacks`；
3. 同一 `qwen3.8-flash` 完整 Flask-8 batch：`architecture_ok=8/8`、
   `task_resolved>=4/8`、incomplete final-report=0、active reservation=0；
4. 无 infrastructure error、hard kill、test tampering 或非 Responses 标记；
5. 总 prompt tokens ≤ `12,130,708`，单题 ≤ `3,000,000`。

SWE-058～061 的 architecture、终态、reservation 与 token 机械门已有完整 batch
证据；SWE-062 因 `task_resolved=0/8` 保持开放。不得用本轮 `judge_resolved=5/8`
替代正式 Graph success 指标。

## 5. 2026-08-28 本地验证

- `go test ./...`：通过；
- `go vet ./...`：通过；
- `go build`：通过；
- `uv run --python 3.13 python -X utf8 -m unittest scripts.swe_test_runner.runner_test`：
  54 项通过；
- 最新二进制运行 `scripts/local_fake_provider_smoke.py`：Graph completed/success，
  Worker/Acceptance 共 2 份 Observation v2、1 份 CheckRecord；第一次 Observation
  故意缺 required tool call 后只失败 1 次并重试成功；slow final-report 走 fallback
  后 completed；普通与取消场景 active reservation 均为 0；Ledger/trace model calls
  均为 33。

Windows 本机 `go test -race ./...` 无法启动：Go race 需要 CGO，但机器没有
`gcc/clang/zig`；现有 WSL 也没有 Go。未安装系统编译器，因此 race 仍是环境验证
缺口，不冒充通过。

## 6. 2026-08-29 Qwen Flask-8 完整批次结论

权威事务产物为 `%LOCALAPPDATA%\AgentGo\swe\runs\summary.json`，mtime
`2026-08-29T05:17:47+08:00`，与同一 `.batch_start` 绑定；不是单题拼接：

- batch `complete`，`completed=8/8`、`not_run=0`、infrastructure error `0`；
- `architecture_ok=8/8`，全部 final-report completed，external hard kill `0`，
  每题 active reservation `0`，known incidents 为空；
- `task_resolved=0/8`，未达到门槛 `>=4/8`；外部 Judge 为 `5/8`
  resolved，说明补丁生成与 Graph/Acceptance 终态必须分层统计；
- prompt tokens 合计 `7,996,589`，是旧基线 `15,163,385` 的 `52.74%`，
  通过 `<=12,130,708` 门；单题最大 `1,689,549`，通过 `<=3,000,000` 门；
- 全角色仍为统一 `qwen3.8-flash` / Responses，没有模型切换、provider 回退或
  multi-provider。

结论：Context/Replay、四阶段 reserve、control 两击、finalization fallback 与
reservation/accounting 的机械目标已由完整批次关闭；发布门整体仍失败，唯一未闭合
的是 SWE-062 的业务 Graph success/验收校准。后续修复必须继续保留本轮已经通过的
architecture 与 token 门，不得通过放宽 evidence、隐藏 blocked/failed 或改用单题
补跑提高表面正确率。

## 7. 2026-08-30 Graph v3 修复后的同模型结论

同一 `qwen3.8-flash`、同一 Responses provider、无 fallback/切换的最新完整
Flask-8 事务批次为：8/8 completed、architecture 8/8、business/Judge 3/8，
367 calls、5,757,570 prompt、201,283 completion、5,142s；全部 final-report
completed、active reservation=0、infrastructure/not-run/external hard kill=0。
相比本文件第 6 节，架构稳定性保持 8/8，prompt 再降 28.0%，Graph success 从
0/8 回升到 3/8，但仍未达到 `>=4/8` 业务门。

失败分层显示两类不同边界：

- `ipv6-server-name` 的代码与定向测试正确，却因缺 candidate revision 全量检查
  被 Acceptance 判 fixable；Prompt 加固后的首个定向虽 Judge success，trace 仍
  证明内部只跑定向 `verification`。最终采用通用 RunContract v2 Check Contract：
  `targeted` 允许缩小，`verification` exact command 由外部 suite 注入、L3 在
  Shell 前执法。第二次定向形成 `targeted→targeted→full verification`，内部
  full CheckRecord、Graph success 和外部 493 passed 同时成立。
- `ipv6-session-txn`、`pass-context-dispatch`、`session-access-tracking` 的主要
  失败是几十次只读后零 mutation；`teardown-callbacks` 有候选编辑但无法通过
  检查。这些是当前模型的决策/实现能力下限。2 分钟 handoff 又小于尾部慢调用，
  Recovery 常在 execution window 关闭后才到达，因此通用窗口前移到 5 分钟。
  `pass-context-dispatch` 定向证明新 `work@2` 确实获得 execution 并产生修改，
  但 Judge 仍失败且 token 从 1,057,670 增至 1,560,280：机械机会已修复，业务
  能力未修复，额外 retry 也不是天然节费。

因此在上述 3/8 阶段 SWE-062 仍开放，但归因已经收敛：不是 AgentGo 仅对 deepseek-v4 家族有效，
也不是 provider 路由问题；相同架构下小模型可稳定完成 3/8，并在 exact-check
修复后定向完成第 4 类题。剩余难题更需要“低成本模型负责有界检索/候选，较强模型
负责 mutation/recovery/acceptance”的后续静态角色分级实验。该结论不授权当前
Runtime 自动切换模型或 provider；每个实验 cohort 仍必须冻结明确的模型分配。

### 7.1 最终关闭批次

SWE-093 exact Check Contract 与 SWE-095 intervention 优先终结补齐后，最终同模型
完整批次达到：8/8 completed、architecture 8/8、business/Judge 5/8、345 calls、
5,110,655 prompt、195,448 completion、4,393s；known incident、infrastructure、
hard kill、not_run、active reservation 全为 0。五道成功题的最终 internal
`verification` 都逐字执行无路径全量 pytest；失败三题均由有界 Graph blocked
收口，单 Activation 最大 intervention count=1。

这超过第 4 节冻结的 `task_resolved>=4/8`，并把第 6 节的 0/8、7,996,589 prompt
改善为 5/8、5,110,655 prompt（减少 36.1%）。SWE-062 至此关闭。结论仍不是
“小模型等同强模型”：3/8 复杂题业务失败保留为后续静态角色分级实验的能力
基线；本次没有实现或启用运行时模型/provider 切换。
