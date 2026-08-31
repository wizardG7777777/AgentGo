# SWE-063～095：Graph v3 Delivery workspace 与弱模型级联收敛事故

> 状态：Closed / Qwen Flask-8 architecture 8/8、business 5/8 verified
> 日期：2026-08-29～2026-08-30
> 范围：L1 Prompt、L2 WorkLog、L3 Workspace、L4 Retry、L5 Delivery、Validation/Trace

## 1. 批次事实

同一 `.batch_start`（2026-08-29 16:13:38+08:00）的统一
`qwen3.8-flash` Flask-8 完整批次为：

- completed `8/8`，但 `task_resolved=0/8`、Judge `0/8`、patch `0` 行；
- Graph 全部 blocked，`architecture_ok` 却错误显示 `8/8`；
- 385 calls、5,989,675 prompt、256,383 completion；
- 19 次 `workspace_materialized`，31 次 `workspace_cleaned`；八题全部在
  未发生 `workspace_merged` 前清理 `delivery-*`；
- 33 次 Observation 成功、10 次失败，22 次 action contract rejection，
  4 次 attempt deadline；final-report 与 reservation 均正常关闭。

## 2. 错误回溯

| ID | 层 | 根因 | 级联结果 |
|---|---|---|---|
| SWE-063 | L5→L3 | Graph v3 用 `delivery-*` 作为 workspace 身份，Watchdog 仍把目录名当 TaskID 查询 | 活跃 candidate 被当孤儿删除；read/run_check 根路径失效 |
| SWE-064 | L3 | Manager 没有活动租约与持久化 owner；旧 View 在目录被删后仍可继续写 | edit/write 短暂成功、目录反复重建/再删除，revision 失真 |
| SWE-065 | L2→L5 | work-log 只显示调用次数，丢弃普通工具 success/failure 与 typed CheckRecord | Recovery 把失败调用误读为成功，重复同一不可恢复环境 |
| SWE-066 | L1/L3/L4 | Worker 普通工具清单常驻列出 control-only Observation 工具；same-snapshot failure 实际 rollover Attempt | default phase 未授权、Attempt 浪费、control contract 不稳定 |
| SWE-067 | L5 | `FreezeCandidate` 与 `delivery.Transaction` 没有生产接线 | TaskOutcome 只拼接 candidate 字符串，promotion 缺少 durable transaction |
| SWE-068 | Validation/Trace | workspace 事件没有 RunID，Test Runner 在 Run 过滤时丢弃；known incidents 不含未 merge 清理 | 确定性架构事故被误标 `architecture_ok=true`，批次继续烧 token |
| SWE-069 | L4 | mutation 前的旧 evaluation pass 与后续 file change 只按“近期共存”判断 | stale check 使刚完成 edit 的下一轮立刻进入 submit-only |
| SWE-070 | L3 | Control schema 枚举当前 Attempt 多条 CheckRef，handler 只授权同 check_id 最新一条 | 模型严格复制 schema 仍连续两次 `control_contract_unstable` |
| SWE-071 | L1 | Worker prompt 写死 POSIX `.venv/bin/python`，suite prompt 要求跨平台 `uv run --no-sync` | Windows 上误判解释器/uv/worktree 环境损坏 |
| SWE-072 | L4 | `workspace:empty` 的 baseline run_check 被算作 verification/decision | 模型换测试命令即可持续重置停滞，零 mutation 运行到 deadline |
| SWE-073 | L4/L5 | candidate closure 可合法推进 decision，但没有独立首次 deliverable 上限 | 30+ turn 调查仍不进入 recovery |
| SWE-074 | 基础层/L3 | startup probe 只重试 transport/malformed；一次 auto text-only 直接判不兼容 | 外部 probe 通过后内部 probe 偶发失败，任务未启动 |
| SWE-075 | Validation | `invalid_recovery_deadline` 用全局错误文本做多词命中 | 不同事件的 recovery/invalid/deadline 被拼成假 incident |
| SWE-076 | L2/L3 | coordination/v2 承诺 Observation，Scheduler 却未注册 control tool/共享 Task Memory | graph-change wake 进入 checkpoint 时工具面为空，`control_contract_unstable` |
| SWE-077 | L3/L5 | graph-change wake 没有冻结 Graph scope，Intervention scope 也未进 Session snapshot | 同图 `get_task_result` 被拒，GraphChange 反而可尝试跨图 |
| SWE-078 | L2/L3/L5 | fulfillment `CheckRef` 未解引用为 Graph Evidence；work-log 把已覆盖的历史失败累计成当前状态 | verifier 自行拼 `ev:*:check:*` 后被判越谱系，合法 candidate 变 disputed |
| SWE-079 | L3 | 稀疏 COW 目录被直接当作 Shell/run_check cwd；Acceptance 未绑定 producer Delivery workspace；业务读路径又会命中任意内部物理文件 | 项目/.venv 不可见，弱模型反复猜绝对路径；验收可读到主根旧版本；owner/manifest 命名空间泄漏 |
| SWE-080 | L3/L5 | Acceptance 期 candidate 可变但 promotion 前不重算 digest；`DeliveryID` 不进 Task snapshot | TOCTOU 可提升非冻结内容，恢复后丢失候选工作区身份 |
| SWE-081 | L3/L4 | Observation control tool 后追加但租约未重排；24-turn 门未考虑慢模型的绝对 Attempt 剩余时间 | `read_content_ref` 拒绝合法租约；首次交付 guard 尚未到达就先撞 deadline |
| SWE-082 | Validation/Test Runner | batch summary 只依赖 Python `finally`，Windows PTY Ctrl+C 可让进程退出却留下启动时 `[]` | 已完成的 4/8 结果没有形成 incomplete 事务摘要，需手工重建 |
| SWE-083 | L3 | 完整 Shell snapshot 复制了 Python editable `.venv`，但 `.pth` 仍固化主根绝对路径 | post-edit pytest 从主根导入旧 Flask，candidate 修复正确仍保持红态 |
| SWE-084 | L1/L3/L4 | `run_check.check_id` 没有当前 GraphContract enum；任意 post-edit pass 都被 L4 投影成 declared evaluator；exact submit 遇 fulfillment 拒绝又不 reopen | 模型使用 `tests-options-green` 后被锁入 submit-only，同一拒绝重放 36 次并撑爆 Context section |
| SWE-085 | L5/RunBudget | `BindRecoveryDeltaAuthority` 在完整校验模型字段前预留 permit，malformed 后取消的确定 ref 无法再激活 | 首次尾空格参数拒绝后，第二次正确 retry 被误报 `RecoveryStartPermit 不存在或已失效` |
| SWE-086 | L3/L4 | Delivery candidate 按 DeliveryID 跨 repair 累积，`WorkspaceRevision` 却只计当前 Task/Attempt write/edit | `work@3` 在前代 candidate 上完整 pytest 通过，CheckRecord 仍被记为 `workspace:empty` 而无法 fulfillment |
| SWE-087 | Validation/Test Runner | 同一题没有 testbed 外部独占锁，第二个 Runner 直接 `rmtree` 活动 worktree | 已关闭的 origin trace 先被删，遇到仍打开 Worker JSONL 才 WinError 32，当前 Run 的 trace/budget 对账被外部污染 |
| SWE-088 | L2→L3 | Check Evidence 的 `output_ref` 按 producer Task scope 落盘，下游 Acceptance 虽经冻结 Graph 数据流取得该 Ref，`read_content_ref` 仍只允许 owner Task | verifier 读取合法上游检查输出时 `scope_mismatch`，弱模型会把权限拒绝误判为证据缺失或重复调查 |
| SWE-089 | Validation/Test Runner | Windows 重定向/PTY 下 Python stdout/stderr 可回落到 cp1252，Runner 又直接打印中文阶段与诊断 | 批次事务测试和真实 CLI 在结果 checkpoint 前后均可能 `UnicodeEncodeError`，把终端编码误报成基础设施失败 |
| SWE-090 | L1/L3/L5 | 同一条 typed Check Evidence 同时展示外层 `ref` 与权威 `check_ref`，但 `cited_evidence` 只接受前者 | 弱模型复制实际消费的 `check_ref` 仍被判越谱系 `disputed`；已通过的 candidate 无法 promotion |
| SWE-091 | L5 | Acceptance 在节点结算前发布 graph-change wake；随后 failed 兜底已让 Graph 终态，wake 仍被 Scheduler 认领 | 终态 Definition 无未来 Activation 可改，却额外消耗 recovery/finalization 时间与 token |
| SWE-092 | L3/L4 | graph-change phase 强制至少一个工具调用，但工具闭集没有“无需修改”的结构化裁决，成功 commit 也不 finalizing | Scheduler 正确判断 no-change 后只能正文退出并被 required-action gate 连续拒绝，最终 `progress_authority_failure`；final-report 被拖到 deadline |
| SWE-093 | L1/L3 | SWE Worker 把 required `verification` ID 用于定向 pytest 后即 completed；schema 只约束 ID，不说明同 ID 最新记录的验收范围 | `ipv6-server-name` candidate 修复与定向测试均正确，Acceptance 只能因未证明 full suite green 判 fixable，候选不 promotion |
| SWE-094 | L4 | 2 分钟首次交付 handoff 小于 qwen3.8-flash 的尾部单次 Invocation，guard 尚未获得一次 settlement 观察机会就跨过 Attempt deadline | `pass-context-dispatch` 等三题把 execution 窗口全部消耗在只读调查；Recovery 正确介入时已无法再启动一次有效 work Activation |
| SWE-095 | L4 | 时间/decision guard 返回 `Intervention=true` 同时携带 `ObservationAction=decision_stalled`；主循环把任何非空 ObservationAction 都优先 `continue` | LoopStore 已 durable 写入 intervention 仍继续发模型调用，同一 Task 累计 19 条 intervention、48 个只读 turn；5 分钟 handoff 表面启用但没有终结 Activation |

权威因果链为：

`Graph v3 TaskSpec.DeliveryID` → `delivery-*` 目录 → Watchdog `GetTask(directory)`
miss → `Cleanup` → Worker 的 COW View 指向缺失目录 → `run_check exit=-1` →
work-log 丢失失败语义 → Recovery retry → deadline/loop intervention → Graph blocked。

## 3. 修复

- workspace 根新增原子持久化 `agentgo.workspace-owner/v1`，明确
  `task|delivery`、Run、Graph 与 Delivery；目录名不再承载逻辑身份。
- Agent 换入 View 前取得活动租约；Cleanup 对活动租约 fail-closed，并发出
  `workspace_cleanup_rejected`。stale View 不得重建已消失目录。
- Watchdog 经 Graph retention resolver 裁决 Delivery：运行中保留；
  blocked/failed/cancelled candidate 隔离保留；只有带
  `delivery_commit_ref` 的 success 残留允许级联清理。
- 业务 `write_file`/`edit_file` 禁止写 `.agentgo/**` 控制面。
- CheckStore 新增 Task 级列举；work-log 输出每种工具 ok/fail 和 typed
  CheckRecord pass/failed/exit/workspace，失败写入不再列入文件变更。
- `FreezeCandidate` 在 TaskOutcome v3 提交前校验 Delivery owner、非空 dirty
  set 与文件 digest；DeliveryStore 持久化
  open→prepared→verifying→commit_prepared→committed，失败终态转 quarantined，
  prepared/unknown promotion 仍禁止重放。
- action contract 的 `retry_same_snapshot` 真正在同一 Attempt 内有界重试；
  Recovery 仅在不足一次最小调用窗口时拒绝启动，不以固定一分钟过拟合。
- Worker 普通工具清单移除 `record_observation_delta`；只由独立 control phase
  与唯一 schema 暴露。
- workspace trace 写入 owner Run/Graph；Test Runner 同时兼容旧 disposable
  worktree 中无 RunID 的 delivery 事件，并把“cleaned before merged”计为事故。
- verification pass 必须晚于最后一次 deliverable；`workspace:empty` check 只算
  baseline knowledge。code-change/v6 在 24 个探索 turn 仍无首次 deliverable 时
  先冻结 Observation，再以 decision stalled 交 recovery。
- Observation handler 授权当前 Attempt 所有已有 settled run_check 对应的
  CheckRecord，与 schema enum 同源；latest CheckRef 仍只用于进展投影。
- Worker 统一使用 `uv run --no-sync python -m pytest -q`，禁止平台专属 venv
  路径。startup auto probe 对 text-only/wrong-call 使用全新 name/nonce 有界采样
  三次，连续失败才 fail-closed。
- Scheduler 注册独立 Observation control tool 并共享 Task Memory/
  CheckStore；graph-change wake 冻结 `InterventionGraphID`，Graph 读取、结果
  读取和 ChangeProposal 均以该字段 fail-closed，且随快照往返。
- TaskOutcome 把 fulfillment 实际引用的 CheckRecord 解引用为 typed
  Evidence，穿过 TerminalAdapter/Graph dataflow 交给 verifier；work-log
  只呈现每个 check_id 的最新权威状态与 superseded 数。
- Shell/run_check 在可丢弃完整项目快照中运行，每次以 COW
  dirty set 覆盖；Acceptance 绑定同一 Delivery workspace。promotion
  前重算 candidate digest，验收期变化直接 quarantine；DeliveryID
  随 Task snapshot 持久化。业务读只命中 manifest dirty 文件，
  workspace 内部名称禁止业务写；租约归零即删除完整 Shell
  snapshot，不让 blocked candidate 长期保留整份依赖树。
- ExecutionLease 在计算边界统一排序 ControlTools；code-change/v6
  既保留 24-turn 门，也在 Attempt 剩余 2 分钟且仍无首次
  deliverable 时提前交 L5 recovery。
- Test Runner 在批次启动与每题结束后均原子 checkpoint
  `summary.json`，并显式收口 `KeyboardInterrupt`；即使终端绕过
  `finally`，也保留上一个完整题目的当前批次权威。
- workspace Shell 环境把 snapshot `src`/根置于 `PYTHONPATH`
  前部，并把 `UV_PROJECT_ENVIRONMENT` 绑定 snapshot `.venv`，使
  editable install 不得穿透回主根。
- `run_check` schema 按当前 Task 冻结 required check ID enum，handler
  在 Shell 前拒绝越界 ID。L4 只把 required ID 投影为 verification，
  其它 check 只是 auxiliary knowledge；exact submit 收到可修复 fulfillment/
  output-contract 拒绝后清除旧 marker 重开业务工具。
- RecoveryDelta 先填充 source 并完整 decode/validate，然后才预留
  RecoveryStartPermit；参数 typo 不再消费唯一重试授权。
- Check/Observation/fulfillment 对 Delivery Task 共用 WorkspaceManager
  revision resolver：由实际 manifest/content digest 决定版本，并汇总同一
  Delivery 全部 producer/repair write refs；普通 Task 保留 Attempt 局部语义。
- Test Runner 在 testbed `locks/` 中持有跨平台非阻塞任务锁，
  同题并发在任何 worktree 清理前以 `task_already_running` fail-fast。
- `read_content_ref` 为冻结 Graph 数据流增加最小委托：仅当 task-scoped Ref
  与当前 Task 同 Session/Graph，且 `ref_id` 是合法 upstream Result/Evidence
  JSON 中逐字相等的 string value，才以 owner scope 解引用；授权回调每页重读
  Task/Lease/ContextInputs。仅同 Graph、文本子串、Prompt 复制或伪造 source_ref
  均继续 fail-closed。
- SWE Test Runner 每个公开命令入口自行把可重配置 stdout/stderr 固定为
  UTF-8；不再依赖 Windows 活动代码页或调用者额外传 `-X utf8`。不支持重配置
  的嵌入式流保持调用方语义，结果文件仍始终以 UTF-8 原子写入。
- Acceptance 谱系把可解引用 EvidenceEntry 的 `check_ref` 注册为同条
  `ref` 的 typed 别名；Prompt/schema 明确只可复制这两类字段，`output_ref`、
  CallID 与 ResultRef 继续拒绝。别名不从字符串格式推断，猜中 CheckRef 不能授权。
- Acceptance 先完成 durable 节点/出路结算再请求 graph change；waker 发现图
  已终态时只保留审计、不再发 Scheduler task。活图 graph-change task 在成功
  `read_graph` 后获得 `submit_graph_change_decision(no_change)`；成功 commit 则
  直接收口非图 coordination。Test Runner 把这类任务的进展契约耗尽列为
  `graph_change_coordination_stalled` 架构事故。
- RunContract v2 新增通用 `check_contracts`。SWE Test Runner 冻结
  `targeted/test`（自由定向命令）和 `verification/test`（exact
  `uv run --no-sync python -m pytest -q`）；ToolRouter 把两者与 Graph required
  ID 合并成 enum，handler 在 Shell 前校验 kind/exact command，只有 required
  `verification` 进入 fulfillment。
- code-change/v6 的首次 deliverable handoff 从 2 分钟前移到 5 分钟；仍按
  冻结 policy、是否已有 deliverable 与 Attempt 总窗口机械判断，不按模型名、
  provider 或题目分支。
- Agent 主循环只在 `ObservationAction != "" && !Intervention` 时继续机械
  checkpoint；已有 Observation 的 time/decision/observation-stalled intervention
  立即按 typed reason blocked，不再重复调用。直接收口路径同步保真
  `decision_progress_stalled` / `observation_state_stalled` cause。行为测试断言一次
  settlement、一次 durable intervention、一次模型调用即结束当前 Activation。

## 4. 当前证据与关闭门

已通过：

- `go test ./... -count=1`；
- SWE Test Runner 58 项 Python 测试；
- 旧八题 trace 重放：八题均被新规则识别为
  `delivery_workspace_cleaned_without_merge`；
- 最新真实二进制 + fake Responses：Graph completed/success、Worker/Acceptance、
  CheckRecord、Candidate、Delivery committed、Effect promotion、主根产物、
  final-report fallback、取消结算与 active reservation=0 全链通过。
- 真实 `automatic-options`：Graph success、Judge resolved、494 passed、26 行
  patch、Delivery committed、cleanup incident=0、workspace residue=0；该运行同时
  暴露并关闭 SWE-069。
- `context-push-order` 定向问题发掘依次复现 SWE-070/071/072/073；各机械问题
  均有单测，最后一次未修复二进制运行仍为模型零 mutation，不能作为修复后
  业务结论，需由最终 batch 覆盖。
- `secret-key-rotation` 在 SWE-076～078 修复后定向闭环：Graph
  success、architecture true、Judge resolved、487 passed、22 行 patch，
  Observation/control/cleanup/reservation incident 全为 0。
- 后续引入 Graph v3 完整 candidate/check 流后，修复前二进制再次定向
  `secret-key-rotation`：Worker 已正确修改 `sessions.py` 且 required check pass，
  Acceptance 也读取了 typed Check Evidence；但它复制同条证据的 `check_ref`
  后被旧 Runtime 判 `disputed`，candidate 未 promotion，Judge 因而仍为
  486 passed / 1 failed / patch 0。终态图又留下 graph-change coordination，
  该任务因 no-change 无结构化动作而耗尽 progress authority；整题 59 calls、
  967,715 prompt、56,490 completion、1,086s。旧 Test Runner 仍报
  architecture true；SWE-090～092 与 `graph_change_coordination_stalled`
  正是从这条反证补齐。
- 包含 SWE-088～092 的新二进制定向复跑同题已闭环：Graph
  completed/success、四个内部 Task 全 completed、final-report completed、
  architecture/task/Judge 三门全通过；487 passed、0 failed、22 行 patch、
  tampered=false。25 calls、249,517 prompt、10,218 completion、233s，较上一条
  失败运行分别减少 34 calls 与 718,198 prompt tokens；所有 known incident、
  Observation failure、Delivery cleanup-before-merge 与 active reservation 均为 0。
- 随后同一 `qwen3.8-flash`、同一二进制完成 Flask-8 全批：8/8 completed、
  architecture 8/8、infrastructure/not-run/external hard kill/active reservation
  均为 0；业务/Judge resolved 3/8。总计 367 calls、5,757,570 prompt、
  201,283 completion、5,142s。`automatic-options`、`context-push-order`、
  `secret-key-rotation` resolved；`ipv6-server-name` 是“正确 candidate + 定向
  pass + 缺全量 check”导致 fixable，形成 SWE-093；另三道零 mutation 长调查
  与一题两代 candidate 未通过形成模型能力下限证据，并暴露 SWE-094 的过晚
  handoff。该批证明主架构闭环稳定，不证明当前小模型业务能力达到 8/8。
- 仅用 Prompt 强化全量纪律后的第一次 `ipv6-server-name` 定向虽然最终
  Graph/Judge success（493 passed、18 行 patch、25 calls、255,549 prompt），
  但 trace 证明 Worker 两次都以 `verification` 执行定向单测（output 仅 8 passed），
  全量 493 只来自外部 Judge。这条反证否定了“Prompt 即权威”，促成上述
  RunContract exact Check Contract；该次不能作为 SWE-093 机械关闭证据。
- exact Check Contract 新二进制定向已机械关闭 SWE-093：trace 依次记录两次
  `targeted/test` 定向测试与最后一次 `verification/test`，后者命令逐字为
  `uv run --no-sync python -m pytest -q`；candidate revision 的内部 CheckRecord
  pass 后 Acceptance/Graph success，外部 Judge 493 passed、20 行 patch、
  architecture/task resolved，25 calls、288,294 prompt、10,429 completion，
  known incident/active reservation 均为 0。
- 5 分钟 handoff 的 `pass-context-dispatch` 定向机械关闭 SWE-094：`work@1`
  在 18:27:46 以 `decision_progress_stalled` 提前结束，Recovery 创建 `work@2`；
  后者获得约 8 分钟真实 execution，产生 edit_file×3/write_file×1，最终才因
  自身未完成验证于 18:35:52 `invocation_deadline` blocked。Graph/FinalReport/
  reservation 均正确收口且 architecture=true；业务 Judge 仍失败（487 passed /
  3 failed / patch 0），73 calls、1,560,280 prompt，说明架构已提供第二次机会，
  当前模型仍未收敛，且额外机会会增加 token——不能把它误报成节费成功。
- 含 5 分钟 policy 的后续完整批次前四题全部 architecture/task/Judge resolved；
  第五题纯读取到 handoff window 时，LoopStore 已在 19:06:46 写入
  `decision_progress_stalled`，但主循环随后继续到 turn 48、累计 19 条
  intervention，直接复现 SWE-095。为避免已知缺陷继续烧 token，本次批次主动
  中断；增量 summary 保留前 4/8 resolved，不能冒充完整 batch。修复后的行为
  单测已证明一次 settlement 后立即 blocked；最终完整外部批次仍需重跑。
- 最终权威完整批次绑定 `.batch_start=2026-08-30 03:16:00+08:00`，
  `summary.json` mtime `04:31:01+08:00`：8/8 completed、architecture 8/8、
  business/Judge 5/8、infrastructure/not-run/external hard kill/known incident/
  active reservation 均为 0。345 calls、5,110,655 prompt、195,448 completion、
  4,393s；相较前一完整 3/8 批次减少 22 calls、646,915 prompt（11.2%）和
  749s。五道 resolved mutating 题各恰有一条最终 `verification`，trace 审计
  全部逐字为无路径的 `uv run --no-sync python -m pytest -q`。
- `automatic-options`、`context-push-order`、`ipv6-server-name`、
  `ipv6-session-txn`、`secret-key-rotation` resolved；
  `pass-context-dispatch`、`session-access-tracking`、`teardown-callbacks` 走有界
  typed blocked，分别由至多一次/Activation 的 intervention 与最多两代 Worker
  收口。这三题是当前模型未完成业务修复，不是架构、provider 或资源泄漏。
  全批固定同一 `qwen3.8-flash`/Responses 分配，无模型切换、provider fallback
  或模型名特判。SWE-063～095 与本轮既定 `business>=4/8` 关闭门全部通过。
- 随后从头 batch 的 `automatic-options` 仍在 925s 以
  `invocation_deadline` blocked，无 artifact。回溯证明其 25 次模型调用中
  只有 19 个 exploration turn，并反复遇到稀疏 workspace 无项目树、
  绝对主根被 Gate 拒绝与非 canonical Lease 导致 ContentRef 不可读。
  该运行成为 SWE-079/081 的外部复现，不作为新实现的关闭证据。

发布级剩余证据不再属于本事故关闭门：当前 Windows 缺少 CGO 编译器，无法在
本机重跑 `go test -race ./...`；Windows/macOS/Linux CI 仍应按 AGENTS.md 保持。
