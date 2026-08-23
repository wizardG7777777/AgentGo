# KNOWN_ISSUES — 当前限制与验证缺口

最后核对：2026-08-23。

## 2026-08-23 五层架构修复的当前开放项

本轮已落地 Invocation/Context/Effect/Loop/Graph 的主要 canonical contract、
durable Store 和生产接线，并通过 full/race/vet/build、harness 单测与真实二进制；但以下
仍是当前发布判断中的真实开放项：

- **L5 legacy 退出**：新图已走 Draft/Definition/commit/start、ChangeProposal、
  typed outcome/outbox；旧 `submit_graph`/direct `patch_graph` 仍保留受限兼容面，
  尚未达到无生产调用方的删除门。
- **L3 Effect unknown 人工边界**：两阶段 TerminalIntent 在 bounded settlement
  window 到期后只把仍未返回的 L4 action 标为 `ActionUnknown` 并 Seal checkpoint；
  它不会篡改、重放或自动补偿 Effect Journal 中的 prepared/unknown 外部副作用。
  这类 Effect 仍需按 Effect authority 进行人工核验。
- **基础层/L2 provider 证据**：Responses typed-item/SSE、Context v8/Replay v3、
  动态 OutputBudget 与 strict required-nonce `startup_probe=tool` 已接入。DeepSeek
  `deepseek-v4-flash` 的空 `message.content=[]` 现经“typed 校验 + raw exact
  override”跨轮保真，真实两轮 probe、Worker replay、thinking 单题与 8 题
  批次均未再出现 provider `invalid_request`。真实 API 矩阵证明 thinking
  拒绝 exact/required，但 auto + tools 成功；因此常规机械阶段使用 auto +
  singleton + L3 required-action gate。Graph 最终交付对历史工具有粘滞重放，
  经 live test 收窄为“交付历史投影 + phase contract + 单次 `reasoning=none`
  + exact submit”。当前只保留更多 provider-specific SSE/usage fixture 与三平台
  CI 作为发布级扩展证据；不把单个模型外推成所有后端能力。详见
  [`DeepSeek Responses 精确重放事故与单题回归`](../test-issues/2026-08-23-0500-deepseek-responses-exact-replay.md)。
- **SWE-015…019 实现已关闭**：Response replay、Raw History projection、Attempt/
  deadline、Scheduler phase Prompt/ToolRouter、真实 Lease、Tool batch/ToolResultRef
  已完成，并通过 full/race/vet/build/真实二进制验证。完成记录见
  [`第六轮 0/8 分层诊断`](../test-issues/2026-08-22-1510-swe-round6-zero-of-eight-layered-diagnosis.md) 第12节。
- **SWE-020…026 实现已落地**：versioned harness 已进一步收敛为单一 Python CLI
  （prepare/run/judge 仅为 task/batch 内部固定阶段，不再经过或暴露 Bash 式分段
  入口；批次门失败返回非零退出码；stdout 以四阶段标签明确区分预期红态、Agent
  执行与最终 Judge）、
  typed/bounded runtime snapshot、
  Context v8、Run/Attempt 与 Invocation-failure 进展分离、simple Graph/current
  transaction、typed Proposal Acceptance、Graph intervention scope 已进入生产主链，
  并获得一批8题真实运行证据。
- **SWE-027/028 已修复并获多题证据**：Responses typed-item 主链消除了正文工具
  猜测，verification 新证据超出 exploration allowance 后改为 auto-singleton + L3 required-action
  `submit_task_result` 交付阶段，不再直接 blocked。
- **SWE-032 实现与外部验证已关闭**：code-change/v1 在 rollover 用尽后于
  第 7 轮提前 intervention，违反声明的 `InterventionAfterTurns=9`。状态机现只按
  intervention 阈值介入；新 code-change Task 冻结 v3（4/10/18/24），v1/v2 保留供历史恢复。
- **SWE-033 已修复并获得外部验证**：第八轮三道失败题证明 typed
  intervention 曾在 simple Graph 中直接进入 blocked end，Scheduler wake 到达时
  Graph 已终态且无法 change/retry。新 simple Graph 加入 framework-owned
  `loop_recovery` Controller，冻结 source Task 与 GraphChange 控制租约，按
  `decision=retry|blocked` 创建新 Activation 或收官；SWE runner 同时新增
  `loop_intervention_without_recovery` 架构事故门。full/race/vet/build、真实
  `work@1→recovery@1→work@2`、三道历史失败题定向复跑均完成；其中
  `context-push-order` 与 `session-access-tracking` 业务 resolved，
  `pass-context-dispatch` 为模型业务失败但 recovery/terminal/ACK 架构链完整。
- **验证横切面**：仓库内 SWE harness 已分别实际运行 OpenRouter Luna 与 DeepSeek
  真实 thinking 路径。最终冻结二进制的 DeepSeek 8 题完整批次为
  `architecture_ok=8/8`、Judge resolved 5/8；三题未解均进入 typed blocked、
  known incident=0，是模型未产出补丁的业务失败，不是 API/系统拦截。
  同一最终二进制对受影响路径的独立复跑中，
  `automatic-options` 为 494/494，`ipv6-server-name` 为 493/493，
  `teardown-callbacks` 为 495/495，`session-access-tracking` 最终复跑为
  490/490，均 `architecture_ok=true / task_resolved=true`；因此 8 题中只有
  `pass-context-dispatch` 尚无业务 resolved 证据。
  三平台 CI 仍是发布层开放证据。详见 [`Responses 主链与单题成功`](../test-issues/2026-08-23-0109-responses-mainline-and-single-task-success.md)
  与 [`DeepSeek Responses 精确重放事故`](../test-issues/2026-08-23-0500-deepseek-responses-exact-replay.md)。

统一状态与后续顺序见
[`SWE 架构修复统一实施路线图`](../design/swe-architecture-repair-roadmap.md)。

本文件只记录当前仍会影响使用、开发或发布判断的限制。2026-07-18 UI Hub 改造的 41 项问题均已修复，原始核查与测试证据已归档至 [ui-hub-remediation-2026-07-18.md](../archived/ui-hub-remediation-2026-07-18.md)。2026-07-20 验收事实核验失败循环与级联取消事故已修复，证据归档至 [acceptance-fact-verification-and-cascade-incident-2026-07-20.md](../archived/acceptance-fact-verification-and-cascade-incident-2026-07-20.md)。2026-07-21 artifact 路径归一化缺陷导致的验收马拉松事故已修复，证据归档至 [artifact-path-normalization-incident-2026-07-21.md](../archived/artifact-path-normalization-incident-2026-07-21.md)。2026-07-21 验收空转（6 次 AcceptanceRun）与 Scheduler 篡改工作区事故已修复，证据归档至 [acceptance-spin-and-env-mutation-incident-2026-07-21.md](../archived/acceptance-spin-and-env-mutation-incident-2026-07-21.md)。2026-07-22 浪费可观测化专项落地：TUI 顶栏新增 session 级 token 总计（Hub 进程级累加器喂入，ad-hoc 团队销毁后消耗不隐形）；trace CLI 新增 stats 子命令（task/agent/plan 三维度 token 聚合 + 浪费口径 + 异常提示，见 TraceGuide §3.4）；watchdog 为 pending 级联取消补发 task_cancelled 事件（此前排队中的级联取消在 trace 中不可见）；修正 detectAnomalies 第 9 条从不命中的死检查（cancel_source 实际值为 dependency_failure）。2026-07-27 Windows ConPTY 长多行粘贴被固定 100ms Enter 防抖切成多条请求的问题已修复，状态机与回归证据归档至 [windows-tui-multiline-paste-incident-2026-07-27.md](../archived/windows-tui-multiline-paste-incident-2026-07-27.md)。2026-07-27 shell 旁路写入（`run_shell` 写文件不产生 `file_written` 事件）导致 artifact 账本缺失、`expected_artifacts` 校验假阴性的问题已修复：record-artifact Reactor 现订阅 `KindShellExecuted`，成功命令后对任务声明的 ExpectedArtifacts 做盘后补登（幂等、workspace 感知、回归测试见 `internal/reactor/builtin/record_artifact_test.go`）。2026-07-27 plan 楔死事故（验收目标含失败节点：验收 runner 认领闸要求 completed 而永远 pending、supersede 不改写 acceptance 节点依赖边被 digest 校验回滚、run 创建零预警）已修复：验收 runner（AcceptanceRunID 非空）依赖只需终态、supersede 改写全部剩余当前节点依赖边、EnsureAcceptanceRun 追加 `acceptance_target_incomplete` PlanWarning，回归见 `internal/plan/supersede_redirect_test.go`、`internal/store/claim_acceptance_runner_test.go`、`internal/bootstrap/supersede_wedge_test.go`。2026-07-28 验收证据类型混挂（verifier 把 command 佐证证据挂到 file_hash criterion，连续三次真实运行复现）已修复：类型化 criterion 的证据纯度违例消息改为指明违例证据 ID 与修正指引，verifier 指引文本补 typed-criterion purity 规则，回归见 `internal/plan/acceptance_evidence_purity_test.go`。2026-07-28 Agent 工作台跨轮输出被最新一轮覆盖的问题已修复：Scheduler 与普通 Agent 现共用不可变完成轮次事件和 Session `turns.jsonl` 账本，TUI/Web 可恢复并浏览全部轮次；证据归档至 [agent-turn-history-loss-incident-2026-07-28.md](../archived/agent-turn-history-loss-incident-2026-07-28.md)。2026-07-29 验收 verifier 的 command 证据在 Windows 上误读 UTF-8 中文文件（PowerShell 对无 BOM 文件按系统 ANSI/GBK 解码，子串断言全部误 fail；行为评测全量首跑 long-form-write 连续两轮验收因此翻车，verifier 自述「文档实际含全部标题」仍被迫判 fail）已修复：内置 verifier 指引补充「内容断言优先 read_file/file_hash 证据；必须 command 证据时显式带 -Encoding UTF8」（`internal/agenttemplate/prompts/verifier.md`）。2026-07-29 expected-artifacts 校验「账本失忆」空转（重试/替代任务换新任务 ID 后 artifact 账本为空，前次尝试写好的文件明明在盘上却连撞提交拒绝，eval smoke 实测 worker 空转 4 轮 + 一次重试回滚）已修复：校验增加磁盘兜底——账本缺失的预期项经 `agent.NewArtifactPhysicalResolver` 解析后 stat，盘上存在（非目录）即转入 `ArtifactCheckResult.Recovered` 视为满足；resolver 为 nil 时退化为纯账本比对，装配点见 `internal/runner/runner.go` 与 `internal/runner/dependency_map.go`。截至本次核对，没有把已修复条目重新列为开放缺陷。

2026-08-19 Graph artifact Evidence 异步登记竞态已修复：`write_file` / `edit_file` 现在返回前同步、幂等地写入路径与 sha256/bytes，artifact log 追加或 fsync 失败会 fail-closed 且保持可重试状态，不再接受 group-commit 的掉电窗口；`submit_task_result` 与自然完成的磁盘恢复项也会在终态前补登 ledger。异步 record-artifact Reactor 仅保留为兼容观察器。回归见 `internal/tools/local_write_test.go`、`internal/tools/submit_result_test.go`、`internal/agent/agent_test.go`、`internal/store/persistence_test.go`、`internal/store/persistence_groupcommit_test.go` 与 `internal/runner/submit_result_runner_test.go`。

2026-08-19 Flask SWE 评测（8 题批量）暴露的 watchdog 超时误杀已修复：`default_timeout_sec=300` 默认使每个节点任务 330s 即被 watchdog 杀死，4/8 题中招——两道验收节点在修复已全绿后被杀（图内误判 runtime failed）、两道实现节点在接近完成时被杀。watchdog 不再终止任何任务或进程：超时（预期时长 `default_timeout_sec`，默认改为 3600 秒）只发一次性结构化告警（`watchdog_alert` / `processing_overtime`）+ 日志 + 汇报邮件，任务继续运行；级联取消等依赖终态传播保持不变。回归见 `internal/watchdog/watchdog_test.go`（`TestWatchdog_OvertimeWarnsOnceWithoutKilling` / `TestWatchdog_OvertimeWarningRearmsOnRetry`）与 `internal/watchdog/crash_report_test.go`。

2026-08-19 同批复测暴露的四项编排缺陷已修复：(1) Graph controller 节点租约越权——scheduler 把执行节点声明为 controller 后被路由回自己，经合成授予拿到完整注册工具面并亲自修改业务代码，架空验收链；现 controller 的 `BusinessTools` 恒为空集（新算与快照复用双路校验），图校验期拒绝 controller 声明 `capability.tools`。(2) Scheduler 直答逃逸——scheduler 不建图、不发任务、空答复直接收口（5 轮调查后零交付）。现分两层：空响应（无文本且无工具调用）一律视为异常轮次，注入提醒继续，连续 3 次按可恢复错误收口重试；新增 `scheduler-closure-review` Gate 在 report_done preCall 审查收口——零图/零 delegated 任务/零 pending 交互时，空 summary 直接拒绝，非空直答须二次确认后放行并记 `scheduler_direct_answer` 日志。(3) 占位图防线——实测 title="t"/description="d" 的占位图通过校验并被真实派发；现 controller/agent/acceptance 节点的 title/description 为单个 ASCII 字母/数字时校验拒绝（单字中文不受影响，阈值刻意保守只拦占位符）。回归见 `internal/agent/empty_response_test.go`、`internal/hook/builtin/scheduler_closure_test.go`、`internal/graph/validate_test.go`（`TestRejectPlaceholderNodeText` / `TestRejectControllerCapabilityTools`）与 `internal/agent/execution_lease_test.go`。

2026-08-20 第三轮 Flask SWE 复测（Nebius DeepSeek-V4-Flash）暴露的模板装配漏接已按「机制搁置」处置：Scheduler 现场 provision 的图节点执行 Team 全部字段取自内嵌 `builtin/generalist@1` 模板，模板写死的累计压缩阈值导致「失忆循环」，模板自带 web 工具又击穿禁网纪律。当前 `agent_templates.enabled` 仍缺省 `false`；2026-08-22 Context v3 修复已从 AgentTemplate schema 删除 `enforce_compact_token_threshold`（旧模板显式设置报迁移诊断），因此重新开放时只需重新评审 phase Prompt、静态 route 与 web 工具授权，不得恢复模板私有 Context 阈值。

2026-08-20 同批复测暴露的「纯文本自然退出绕过一切收口」（SWE-001）已按兜底+预防分层修复：ReAct 循环「有文本、零工具调用」的自然退出路径不经任何工具调用，挂在 `report_done` 上的收口 Gate 对它结构性不可达（scheduler 两题 graph=0 零交付逃逸、worker/verifier 节点不调 `submit_task_result` 假完成、scheduler 处理图变更请求时 134KB 文本不调 `patch_graph`）。修复：①scheduler 根任务的零证据收口审查移到 processTask 终态判别处（新端口 `hook.NaturalExitReviewer`，实现复用 `SchedulerClosureHook` 并与 Gate 共享 confirmed 状态——report_done 与纯文本两路径同一「拒绝一次、确认放行」语义）；②图节点任务纯文本退出视为未提交，注入 submit_task_result 提醒（上限 2 次）后按可恢复错误收口；③收口契约由系统从 GraphID/GraphNodeKind 派生钉入 `TaskMemory.Constraints`（每轮渲染注入、压缩不可达）；④`taskmem.ApplyAttemptEnd` 把 attempt 终止原因有界写入 Failures，重试接手可见；⑤长 JSON 防线：`submit_graph` 语法期失败强制附「骨架图 + patch_graph 逐次扩展」建议、载荷超 8000 rune 附温和提醒、LLM 层 tool_call 参数解析失败错误携带载荷字符数。回归见 `internal/agent/natural_exit_review_test.go`、`internal/hook/builtin/scheduler_closure_test.go`（ReviewNaturalExit 四例）、`internal/agent/task_memory_contract_test.go`、`internal/taskmem/update_test.go`（TestApplyAttemptEnd）、`internal/tools/graph_control_test.go`（长 JSON 两例）与 `internal/llm/client_test.go`（载荷尺寸一例）；5 个旧图任务文本收口测试已更正为结构化收口路径。问题台账与阶段管理见 `docs/test-issues/`。

2026-08-21 Graph 终态契约 v2 落地（`docs/design/graph-terminal-contract-v2.md`，SWE-004 子问题 2 机械消灭）：agent/controller 节点经 `submit_task_result` 的 `event` 曾是任意字符串，worker 交 `ready` 而契约要求 `completed` 即全图 fail-closed。v2 废弃图任务 event 参数、节点终态 status 封闭三值、业务路由全走 result 数据字段 + path 条件：①建图校验（`internal/graph/validate.go`）收窄 agent/controller 出边为 `completed/failed/blocked/always` 系统事件并要求 path 字段在 description 输出契约声明；②`Runtime.CheckActivationOutlet` 提交期出路预求值 + 两击协议（首击拒绝不 finalizing 可重交；次击节点 failed[contract_no_outlet] 并唤醒 `[graph-change-request: .../no-outlet]` 交 Scheduler 裁决，计数按 activation 持久化）；③输出契约机械派生（eq>in>exists>ne）以 `<output-contract>` 块注入任务描述并钉入 `TaskMemory.Constraints`；④scheduler 提示词 v8.0 v2 化、工具描述同步、legacy `publish_task` strict 渐进。v1 存量图按 v1 语义跑完不迁移。回归见 `internal/graph/outlet_check_test.go`、`output_contract_test.go`、`patch_version_test.go`、`internal/tools/submit_outlet_test.go`、`internal/bootstrap/graph_output_contract_test.go` 与 `internal/scheduler/scheduler_test.go`（v8.0 断言）；全量 43 包测试绿 + 二进制冒烟。残余：failed 边返工回边 doctrine 已于 v8.1 落地（见下条）；end outcome 落账随后续设计。

2026-08-21 图终态回填零容错（SWE-002，三轮 pass-context-dispatch 图僵尸事故的根因）已按三层防线修复：模型往 tool_call 名字段泄 DSML 标记产生 200+ 字符畸形「工具名」→ evidence kind/tool_name 超 128 上限 → 整条 activation result 被拒写 → reactor 仅记日志 → 终态永久丢失。①evidence 装配归一（`internal/bootstrap/graph_runtime.go`：`evidenceKindOf` fallthrough 不再透传，非法归一 `unknown`/超长截断；非法工具名归一为确定性占位 `malformed:<sha256前12>`，装配产物恒过 `validateEvidenceEntryBounds`）；②`ToolCallRecord` 落库清洗（`internal/store` 的 `AppendToolCall` 唯一写入点，同关 SWE-003 残余①，TaskMemory/evidence/anomaly 全链吃到干净名）；③回填失败不静默（`graphTerminalSink` 新增 `FailTerminalWriteback` 回落：节点标 failed[graph_writeback_failed] + 幂等唤醒 `[graph-change-request: .../writeback-failed]`，刻意不做盲重试——确定性数据错误重试必复发，瞬时 IO 失败走节点 failed + Scheduler 重激活；终态事实三态穷尽）。回归见 `internal/graph/writeback_fallback_test.go`、`internal/bootstrap/graph_evidence_sanitize_test.go`（含事故形状端到端回归）、`internal/store/tool_name_sanitize_test.go`；全量 42 包测试绿 + 二进制冒烟。

2026-08-21 Scheduler 提示词 v8.1 doctrine 补强（第四轮 SWE 复测暴露的三类提示词域问题，`docs/test-issues/2026-08-21-0209-swe-round4-direct-answer-backdoor.md`）：①失败路径 doctrine（SWE-004 子问题 1）——failed/blocked 出边默认指向能修复的节点（activation="new" 返工回边或 repair 节点），failed → end 明确定性为「放弃该部分目标」，仅失败确认不可局部修复时允许；②建图前调查纪律——「理解目标」不含亲自调查仓库，team 下定图需要先摸清仓库时提交以调查节点开头的最小图、结果到达后 patch_graph 扩展，至多容忍一次定位性查看（四轮 automatic-options/pass-context-dispatch 中 scheduler 亲自多轮 grep/read 后才建图、大图 JSON 损坏即逃逸的根因）；③大图骨架先行——载荷超约 8000 字符时先交 root + end + 首批节点骨架再 patch 逐次扩展（SWE-003 提示词侧强化，把「骨架图先行」从错误回执里的被动建议升级为建图纪律）。同步：worker 提示词（`prompts/worker.md` + `prompts/swe/worker.md`）新增 Gate 拒绝后补救重试（SWE-006）与打转自查（反复读同批文件无新结论即提交当前最优或 blocked）纪律，更正 watchdog「超时杀任务」与 plan_id 两处 V6 前过期描述。回归见 `internal/scheduler/scheduler_test.go`（v8.1 断言 + doctrine 短语断言）；全量 43 包测试绿 + config doctor 0 错误 + 二进制冒烟（healthz 200、3 静态 kind ×8 runner 装配、系统就绪）。**提示词变更的有效性以下一轮 8 题批测为回归证据。**

2026-08-21 直答/崩盘三态状态机 + 上游工作记录摘要 + watchdog 接线（阶段 4，SWE-008/009/010）：第四轮三起「直答」经 turns.jsonl 逐轮还原证实全是**长 JSON 损坏 → 模型工具调用格式崩盘（DSML/自造标记）→ 被误诊为直答 → 确认放行落盘垃圾**的级联，非模型故意逃逸；触发器是单次 completion 过长（7893 tokens）而非上下文总长。修复全程无正文检测（正文 DSML 检测因无法区分「讨论标记」与「泄漏标记」明确废弃）：①三态状态机（`internal/hook/builtin/scheduler_closure.go` 等三处）——放行谓词 `exitCount≥1 且 toolFailed==false`，toolFailed 是本任务 ToolCallRecord.Success==false 的账本机械事实（任务级单调、跨 attempt 累计）；纯问答出口保留（首拒次放），有失败记录时第二次拒绝改格式提醒、第三次 ErrRecoverable 换上下文（exitCount 清零、MaxRetries 兜底）；report_done 的 PreCall 路径不加 toolFailed 前提（能调成 report_done 的模型格式能力未崩）；②未知工具回执清洗（`internal/agent/tool_registry.go`）——非法工具名回执以 `malformed:<sha前12>` 占位 + 格式提醒，不回灌原名；③**上游工作记录摘要**——转移结算时按来源 Task 聚合 ToolCallRecord（工具统计/shell 退出码/文件清单，Args 不进），随 EdgeInput 冻结注入「## 上游输入」段（`internal/graph/types.go`、`runtime.go`、`internal/bootstrap/graph_worklog.go`），下游与 verifier 据此交叉核对上游自述（声称已验证但 shell×0 即假成功探测器）；④介入闭环提示词 v8.2——下游发现实现类上游近零产出时 blocked 注明摘要事实，Scheduler 按返工→修正返工→controller 补位（仅限返工无效且缺口小）→降级失败升序裁决；⑤watchdog 接线（`internal/scheduler/activator.go`）——EventWatchdogAlert 除 legacy BatchUpdateCh 信号外发布 `__scheduler__` 唤醒任务（幂等标记 `[watchdog-alert: <taskID>]` + 超时事实），v8.2 含超时告警裁决指引（查进展→steer 收敛→cancel_task 走失败路径；cancelled 经 `graphTerminalStatusOf` 映射节点 failed 既有测试覆盖）。回归见 `scheduler_closure_test.go` 三态矩阵、`natural_exit_review_test.go`、`tool_registry_test.go`、`internal/graph/runtime_worklog_test.go`、`internal/bootstrap/graph_worklog_test.go`、`activator_test.go`；全量 43 包测试绿 + go vet 干净 + config doctor 0 错误（SWE 配置）+ 二进制冒烟（healthz 200、系统就绪）。**有效性以下一轮 8 题批测为回归证据。**

## 运行与安全

### LLM 凭证不会在启动期完全验证

`llm.default_model`（或 `scheduler.model`）必须存在，但空/失效 API key 通常不会阻止进程、TUI 或 Web Dashboard 启动。首次实际 LLM 调用才会暴露认证、模型或 provider 错误。

- 影响：可用 `-skip-startup-probe` 启动 UI 做配置和界面验证，但不能把“进程已启动”视为 LLM 可用。
- 处置：使用 `${OPENAI_API_KEY}` 等环境变量；部署前发送一次低风险请求并检查结果/Trace。

### Web Dashboard 是受控管理面，不是只读页面

它拥有提交输入、取消 Task、发送引导、回答 pending Interaction、模式和 Session 操作。默认监听 `127.0.0.1:8399`；若监听非 loopback 地址，配置校验会强制要求 `ui.web.token`。

- 影响：不要把端口直接暴露到 LAN/公网，也不要把 LLM API key 当作 Dashboard token。
- 处置：本机使用 loopback；远程使用独立强 token、受信任的反向代理/TLS 和网络访问控制。

### 同一运行时只能有一个实例

`.agentgo/agentgo.lock` 防止两个进程同时写 Session、trace 和原子文件。异常终止可能留下陈旧锁；Windows 下下一次启动会给出清理指引。

- 影响：不能通过同一个 `project_root` 并发启动多个 AgentGo 实例。
- 处置：正常退出；若确认没有存活进程，再按启动提示移除该项目运行目录中的陈旧锁。

## 已知功能边界

### Session 隔离的可恢复窗口与冻结边界

2026-08 起 session 是完整运行时隔离边界（冻结 → 切换 → 解冻，见 [session-isolation.md](../design/session-isolation.md)）。2026-08 二期修订「不自动续跑」后的有意接受边界：

- **启动永远是全新 Session**：不读 `active-session` 自动恢复；进入历史会话只有 `--resume <id>` 与 `/session` 切换两个显式入口。
- **进入历史会话不自动续跑**：快照中的非终态任务一律阻断为 `blocked`（历史事实保留在公告板供 Scheduler 参考），该 session 的非终态图永久停驻（僵尸图，没有恢复入口，随保留期归档退出）；续走 = 用户提交新提示词、Scheduler 参考历史重新规划。切走再切回同样阻断切换时仍在跑的作业（有意推论）。
- **空会话丢弃**：从未提交实际任务的会话在退出/切走/启动清扫时删除；崩溃遗留的空目录由下次启动 `SweepEmptySessions` 兜底。
- **冻结 session 的可恢复窗口 = 保留期**：`session_retention_days`（默认 30 天）后 `RunArchive` 把 closed session 移入 `archive/`，不再可经 `/session` 切换（归档仅作历史审计），其 workspace 豁免同时失效、随后被孤儿清扫。
- **冻结即中断 pending Interaction**：含 Graph approval、Shell 授权、`request_user_input`；切回时为 interrupted 终态，不复活。
- **冻结窗口内迟到邮件丢失**（归档后、team 邮箱注销前送达的），与崩溃窗口同级，不自动重放。
- **`/new force` 后旧 session 的 team spec 可能保持 ready**（异步 `graph_ended` 落到已重绑的新文件时）：解冻时经 team Start 恢复核对自愈为 stopped，无需人工干预。
- 影响：不要把「切走」当作暂停-续跑——切回后旧作业不会自动继续，需要一句新提示词让 Scheduler 接上。
- 处置：调整 `session_retention_days` 适配使用节奏；归档目录仅人工审计。

### patch_graph 修改在途节点的 next 不改变本次路由（提示词已加固，校验层未实施）

节点激活时其定义（含 next）随 activation 冻结（`internal/graph/runtime.go` `nodeForExecution`；transition 记录按下标幂等，冻结是幂等前提）。对已激活节点 upsert 新出边，补丁合法落盘但对本次结算的出边求值无效——这是文档化语义，不是引擎缺陷。2026-08-10 真实事故（图 `g-fd4edcbf-985e252-hist-dive`，journal seq 26-34）：coverage controller 自创「先 patch 自己的 next、再提交事件」模式插入 file_digest→coverage2 补救链，路由仍沿 rev-1 冻结定义走向 end，两个新增节点从未激活即被收官取消。已加固：Scheduler 系统提示词三处（`internal/scheduler/scheduler.go` 覆盖度裁决 / patch_graph CAS 纪律 / 调查补节点）与 patch_graph 工具描述（`internal/tools/graph_control.go`）均写明冻结语义、禁止给在途节点改路由，并给出正确范式——运行时分支在建图时预铺条件边（如 `coverage[gap]→补救节点`），patch 只接未激活节点。注意转移事件名是封闭枚举，自定义分支取值（如 coverage=gap）必须写 path 条件边。

- 影响（残留）：提示词约束非强制，模型仍可能违反——违反时 patch CAS 成功、无报错，补救节点静默不执行，只能在 Graph 视图/journal 事后发现。
- 处置：遇到「patch 后新增节点 cancelled 未执行」先查 journal 中源节点 activation 的 definition_revision 确认沿旧定义路由；彻底消除幻觉需在 patch_graph 校验层对「upsert 修改有在途 activation 节点的 next」返回警告/拒绝（方案 c，未立项）。

### Memory 的 Project 作用域仍未实现

`internal/memory` 的 `ProcessStore`（进程内）与 `SessionStore`（`sess-<id>/memory.jsonl` 持久化，MM8）可用；`ScopeProject` 仍返回 `ErrScopeUnsupported`。CM3 起 `KindLearning` / `KindConstraint` / `KindResult` / `KindDecision` / `KindBlocker` 已有生产写入方（任务终态晋升器）；`KindPattern` 尚无生产写入方。Session Memory 不自动晋升为 Project Memory（V6 语义主链固定为「原始记录 → Task Memory → Session Memory」）。

- 影响：跨 session 的长期知识（Project 级）不可用；Session Memory 随 session 结束生命周期终结。
- 路线图：[MemoryManageSystem.md](MemoryManageSystem.md)。

### Shell 超时处理仍是固定超时

当前 Shell 拦截已经使用通用 `shell_command` authorization Interaction：黑名单硬拒绝，灰名单回答与原始 command/pattern/working directory/Agent/Task 精确绑定，并在 Interaction 不可用或绑定不一致时 fail closed。但仍没有 `shell_commands.yaml` 持久化规则、`ShellCommandGate` 重构或可插拔 `TimeoutHandler`。`shell_timeout_pending` 与 `shell_timeout_resolved` 是被拒绝订阅的 reserved event kind，不会发射。

- 影响：不要在用户 Reactor 中订阅这两个 kind，也不要假定 Shell 超时可以交互式续时或截断。
- 当前用户选择契约：[Interaction 设计](../design/interaction.md)。`ToolUpgradePlan.md` 的旧四键提示与名单写回方案属于已废弃历史，不是当前契约。

### 用户 Reactor 的 prompt/lifecycle 有意受限

用户 Reactor 目前只支持文件型 prompt；`prompt.url`、`prompt.inline` 会在加载时报错。`spawn_agent` 只支持 `one_shot` lifecycle。

- 影响：配置这些预留字段会导致启动失败，不能视为兼容的未来配置。
- 处置：使用 `prompt.file` 和 `one_shot`；其他形态需先实现并补测试。

### Agent 执行隔离：工具写已可按任务隔离，shell 残余风险仍在

2026-07-26 起，Scheduler 可在 `publish_task` 时对 DAG 节点声明 `isolation: "workspace"`（写时复制 overlay）：该节点的 `write_file`/`edit_file` 全部落入任务专属 workspace（`.agentgo/workspaces/<taskID>/`），读穿透主根，任务成功终态由控制面自动合并回主根（fast-forward / 行级三路自动合并），冲突则任务 failed + 自动高优 replan 交 Scheduler 裁决。详见 [workspace-isolation.md](../design/workspace-isolation.md)。

残余风险（有意接受）：`run_shell` 的副作用不受同等隔离——隔离任务的默认 cwd 与显式 `working_dir` 都限制在任务 workspace 内，但命令正文写主根**绝对路径**仍不可完全阻止；非隔离任务之间仍只有 Roster 文件级互斥。（2026-08-18 起 verifier 模板已不再授予 `run_shell`，"verifier 可经 shell 污染被验收对象"的口子随之关闭。）

- 影响：并发 Agent 可通过 shell 互相踩踏工作区；验收 runner 理论上能污染被验收对象。
- 处置：高风险操作用 exec=strict 全量审批或灰名单 Interaction 把守；fan-out 并行写同一批文件的 DAG 节点应声明 `isolation: "workspace"`；容器级隔离评估后再立项。

### ~~Graph acceptance 节点的验收判定暂无服务端核验~~（G1b 已落地；2026-08-18 起由数据流谱系核验取代）

验收机制已于数据流重构中整体换代（设计：`docs/design/scheduler-prompt-and-acceptance-redesign.md` §4）：

- **数据流绑定**：源 activation 终态先把完整 Result/Evidence 写入 activation 级 durable Result Store；每条实际生效转移再把稳定 `ResultRef`、≤32KiB 内联值、目标输入端口和结构化 EvidenceEntry 绑定给目标 activation（`TransitionRecord.Input` → `Execution.Input`）。下游任务通过 typed `Task.ContextInputs` 进入 L2 `upstream_result/upstream_evidence`，不再拼入 Description；EvidenceRef 由 CallID+调用内容或 artifact 身份稳定生成，不依赖展示序数。router/join/恢复路径按 ResultRef 精确解引用。
- **谱系核验判定矩阵**（`internal/graph/acceptance.go`）：验收 agent 经 `submit_task_result` 的 `cited_evidence`（逗号分隔 EvidenceRef）引用其实际消费的证据；服务端只核**谱系**——引用 ∈ 该 acceptance activation 的上游 Input 谱系 ∪ verifier 自身任务证据。越谱系引用 = disputed：不采信 verdict（节点 failed + graph change 唤醒）；**不引用不判死**（旧 `unverifiable` 判死与 command/file_hash/task_status 逐字格式契约整体删除，G1b 注入式 `AcceptanceVerifier` 退役）。
- **data-ready 端口门控**：join / acceptance 的 `required_inputs` 是目标输入端口；入边通过 `target_input` 绑定，并行必需来源使用不同端口，每个端口只有一条生产边。acceptance 可用 `required_evidence` 声明端口所需证据 kind；缺口随任务注入且 Runtime 不采信 pass。
- **当前 authoring 安全基线**：Runtime 尚无 flow generation/correlation token，因此非 barrier 节点最多一条静态入边，join / acceptance 端口也是单赋值；条件分支各自保留后续与 `end`，不支持共享端口 OR 或重新汇入同一普通节点。循环体可直接作为 root（隐式初始 activation + 唯一回边）。复杂 OR mux / 跨代汇流是 future token 能力，不属于当前 Scheduler 可生成拓扑。
- **acceptance verdict 路由**：acceptance 必须有非空 title 与写明逐项验收标准的非空 description。verifier 只提交 `pass` / `fixable` / `failed` 三种业务 verdict，completed 结果必须省略 `event`。completed 结论只能通过 `$.verdict eq ...` 精确分支；acceptance 的无条件、always、completed、pass/fixable 事件出边由 authoring 校验拒绝，Runtime `failed` / `blocked` 事件仅作兜底。证据或能力不足时提交 `status=blocked`；`disputed` 是 Runtime 核验状态，不是 verifier verdict。
- **verifier 工具闭集**：`builtin/verifier@1` 只授予 read/list/grep/glob/web + `submit_task_result`；`submit_graph` / `patch_graph` 对 acceptance route 做同一正向闭集校验，写入、Shell、消息、发任务、用户交互与 `request_replan` 均结构性拒绝——KNOWN_ISSUES 旧条目「verifier 可经 shell 污染被验收对象」随之关闭。
- **Graph 角色租约闭环**：节点 kind 已随 `TaskSpec → Task → Session snapshot` 持久化；acceptance 的新算/复用 Lease 只允许只读正向闭集与 `submit_task_result`，旧空 kind/未知 kind 也按同一最小权限处理，Graph controller 不再因路由 `__scheduler__` 退化为记录型全工具租约。恢复时非空 kind 与 activation 冻结定义不一致会 fail-closed。
- **emergency fuse**：不再按跨任务 activation 次数熔断；合法 `/goal` 与验收返工可任意长。保险丝只限制一次 Runtime 调用内不让出控制权的同步机械级联，触发后图以明确原因 durable failed 并发 graph change 唤醒，不留 running 僵尸。
- 回归：`internal/graph/acceptance_test.go`（判定矩阵 + 端口 data-ready）、`internal/graph/runtime_input_test.go` / `runtime_nodes_test.go`（Result Store、端口 barrier、恢复与同步保险丝）、`internal/bootstrap/graph_dataflow_test.go` / `graph_acceptance_integration_test.go`（稳定证据、任务注入、谱系 valid/disputed）。

剩余边界（有意接受）：

- **机械层只能验证"行为发生过"，验证不了"判断是对的"**：verifier 真看了失败输出仍报 pass，谱系核验抓不到；判断质量仍依赖 verifier 本身，治理与追责只能审计 Graph 已绑定的 Input、Result、Evidence 与 trace，不能把额外旁路读取当作质量证明。
- **Evidence 不证明节点内 happens-before**：调用证据只说明命令及结果，不证明它晚于同一实现节点的最后写入；需要可证明新鲜度的测试/构建必须建成 `implement → checker → acceptance`，以 Graph 因果边而非展示顺序/时间戳证明先后。
- 认知类验收（读代码判质量）的读行为不在 Effect Journal（账本只记副作用），其可观察证据限 ToolCallRecord 调用事实与 trace tool_call 事件。
- `Capability.Budget` 占位字段已删除（从无 Runtime 消费者）；理论上历史图若设置过非空 budget，升级后恢复时 digest 失配会被隔离——实践中该字段无人设置，风险忽略。

### ~~V6 C6a 过渡态：旧动态 DAG 路径无法形成正式终态~~（已于 C6b 解决）

C6b 已删除 `internal/plan` 整包、验收四工具（`define_acceptance_spec` / `ensure_acceptance_run` / `submit_acceptance_result` / `get_acceptance_evidence`）与收尾强制逻辑（`planNeedsFormalFinalization`）：旧 `publish_task` DAG 的收尾不再需要正式验收，`report_done` 只校验 SchedulerBatch 终态；多节点编排走 `submit_graph`，验收语义由 Graph acceptance 节点 + `submit_task_result.verdict` 契约承担，并已于 G1b 获得服务端核验（见上条）。

### 引用幻觉没有独立的强制防线

目前没有 `CitationVerifierHook`、`RetrievalGate` 或专门的端到端引用真实性基线。Trace 可用于事后审计，但不等价于验证外部引用或模型知识。

- 影响：对外部事实、链接和引用的结果仍需由调用方复核。
- 历史审计：[hallucination-acceptance-audit-2026-05.md](../archived/hallucination-acceptance-audit-2026-05.md)。

### 调查型任务存在 read_file 重读浪费（缓解已落地，首次复测后指标已修正）

Layer-1 历史压缩（`snipOldToolResults`）会清掉旧的 read_file 结果，agent 不做中间笔记时只能重读恢复。修正后的重读口径（重复全文读 + 相同 offset 重复分页，顺序分页不算）下，历史任务基线为 34–61%。

2026-07-22 已落地四层缓解：explorer prompt 边读边记硬要求（方案 A）、snip 结构化墓碑（`snipStub`）、read_file 缓存命中摘要 + `force_full` 逃生门（闸 1）、截断元数据公告 + 分页建议（闸 2）；`trace stats` 重读率检测已按 path+offset 去重修正。

2026-07-23 首次复测（~950k tokens，总量未降）暴露三个残留问题：

- **模型先发制人绕过闸 1**：从墓碑/工具描述学会 `force_full` 后预传该参数，stub 未省到 token；
- **方案 A 遵从度弱**：未出现结构化笔记，但报告更详细（指令转化为"更仔细阅读"）；
- **分页/上限错配**：模型按 300–400 行/页请求，超出 10000 字符上限导致截断与重叠重读（已在工具描述与截断公告中加入"每次 200 行左右"的放宽建议，效果待观察）。

- 影响（残留）：缓解效果依赖模型遵从，首次复测未证实节费；worker 编辑流中若模型忘记传 `force_full`，edit_file 可能因内容不在上下文而反复失配（失败安全，但增加 round trip）。
- 处置：复测后用 `trace stats` 对比修正口径下的重读率（基线 34–61%）；若仍不足，评估方案 B（压缩分层）或 view-time 裁剪。完整证据与方案：[explorer-reread-waste-analysis-2026-07-22.md](explorer-reread-waste-analysis-2026-07-22.md)。

## 验证缺口

完整单元/集成测试应运行 `go test ./...` 和 `go vet ./...`。涉及并发变更时，还应在安装 C 编译器的环境执行：

```text
go test -race ./...
```

2026-07-19 已在 Windows 完成全仓测试、`go vet` 与构建，并在 WSL/GCC 下完成全仓 `-race`。Bubble Tea 的非 TTY 输入现显式读取调用方 stdin 并把 EOF 视为正常退出，相关 Windows 跳过已移除；后续仍应把 Windows、macOS、Linux 验证保持为发布门。

### TUI inline 重构（2026-08）的 TTY 专属路径只有单测覆盖

两层渲染模型落地后，`viewTransitionCmds`（动态进出 alt screen + `EnableMouseCellMotion`/`DisableMouse`）、`flushEmitCmd` 的逐行 `tea.Println` 排放、全屏层滚轮滚动都依赖真实 TTY 行为（bubbletea 渲染器与终端模式序列），自动化只覆盖到模型层状态迁移（`fullscreen_test.go`、`TestFlushEmitCmd`）与非 TTY 启动冒烟（`main_startup_test.go`）。alt screen 下 `tea.Println` 被丢弃、Windows ConPTY 的鼠标事件解码（`initCancelReader` 重初始化路径）都未经三平台真实终端验证。

- 影响：TTY 专属回归（如全屏层退出后终端滞留 alt screen、排放行物理行数与渲染几何错位）只能人工复测发现。
- 处置：改动 `runWithIO` / `viewTransitionCmds` / `flushEmitCmd` / `styledMsgLines` 后，在三平台真实终端各跑一次「发任务→/graph→滚轮→回 /chat→翻 scrollback」流程。

### 缺少 Agent 行为级评测

现有测试证明控制面不变量（状态机、验收事实核验、级联取消），但没有跨模型/Prompt 的固定任务行为基线：成功率、token、重规划次数、验收轮数、无进展暂停次数均无自动采集。单元测试全绿不等于真实 LLM 行为达标。

- 影响：prompt 修改、模型更换或工具描述调整后，行为回归只能靠人工复测发现（2026-07-23 重读浪费复测即为例证）。
- 处置：仓库不再内置行为评测框架；重要 prompt 变更后用 `trace stats` 抽查。若重新建设行为基准，应采用受版本控制的真实软件工程任务与外部基准，而不是本地玩具任务集。

## 维护规则

- 新的、可复现且尚未修复的问题在这里记录影响、复现/证据、临时处置和归属。
- 修复后从本文件移除；若修复过程值得保留，迁移到 `docs/archived/` 并在提交中链接测试。
- 不把设计愿望、已完成任务或推测性风险伪装成当前 bug。
