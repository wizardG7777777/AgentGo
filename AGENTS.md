# AGENTS.md

本文件为在本仓库中工作的编码代理提供项目约束与入口指引。架构、机制与配置的细节描述在 `docs/agents-reference.md` 及下文索引的专文中，按需查阅，不要凭记忆假设。

## 项目

AgentGo 是一个 Go 多智能体任务编排系统：Scheduler（LLM 驱动的 `agent.Agent`）把请求分解为任务发到公告板，Runner 执行代理（`internal/runner`，按 kind 在 `setting.yaml` 声明）轮询认领并运行带工具调用的 ReAct 循环。v5 起为 Gate（动作前拦截）+ Reactor（状态变更后响应）反应式体系；V6 起以持久化 Graph 编排取代 Plan 运行时（`internal/plan` 已删），并引入 Execution Lease、Effect Journal、Task/Session Memory 与两轴模式。

语言：Go 1.25 ｜ 模块名：`agentgo` ｜ 配置：`setting.yaml`（YAML/JSON，仅 v4 schema） ｜ LLM：OpenAI-compatible Responses 主链（Chat Completions 仅显式协议配置），经 `internal/llm` 统一实现。

## 五层工程架构

1. Prompt Engineering：定义模型的角色、指令、规则与输出契约。
2. Context Engineering：编译本次模型调用应看到的任务、记忆、历史、工具与上游信息。
3. Harness Engineering：通过 Tool、Execution Lease、Store、Effect 与执行环境向模型提供受控行动能力。
4. Loop Engineering：管理单个 Agent Activation 的 Turn、Attempt、进展、重试、收敛与停止。
5. **Graph Engineering**：编排多个 Agent、Loop 与节点的职责、Activation、Result/Evidence 及数据流。

五层是工程责任域，不要与 Go 包一一对应；完整边界见 [`docs/design/five-layer-engineering-architecture.md`](docs/design/five-layer-engineering-architecture.md)。

## 构建、测试与调试命令

```bash
go build                          # 构建（产出 ./agentgo 或 .\agentgo.exe）
go test ./...                     # 全部测试
go test ./internal/store/         # 单包测试
go test -run TestName ./internal/agent/   # 单个测试
./agentgo -config setting.yaml    # 启动（另有 -skip-startup-probe、-resume <sessionID> 进入历史会话，不自动续跑）
./agentgo config doctor           # 校验配置 + prompt 承诺工具与实际 allowlist 对账
```

任务级 trace 调试入口：`./agentgo trace list / show / stats / graph / node`（不启动主系统），文件位置与事件字段详见 `TraceGuide.md`。无 Makefile / linter，只用标准 Go 工具链；测试假定 LF 行尾（`.gitattributes` 强制）。

SWE Test Runner 的 `probe` / `task` / `batch` / `verify-candidates` 启动前必须通过 provider 环境契约：`SWE_API_KEY`、`SWE_BASE_URL`、`SWE_MODEL` 共 3 项均非空。入口必须在任何网络、文件系统副作用或子进程启动前一次性报告全部缺项并 fail-closed；正式八题 manifest/prompt 位于受版本控制的 `scripts/swe_test_runner/suites/flask-8`，`SWE_SUITE_DIR` 与各路径变量只作可选覆盖。testbed 默认从 Windows `%LOCALAPPDATA%`（回退 `%USERPROFILE%`）、macOS 用户 Application Support 或 Linux `$XDG_DATA_HOME`（回退 `~/.local/share`）动态拼装，禁止硬编码用户名，完整说明见 `scripts/swe_test_runner/README.md`。

### SWE 测试程序命名红线

- 外部 SWE 测试程序的唯一正式名称是 **SWE Test Runner**。路径使用 `swe_test_runner`，Python 类型使用 `SWETestRunner...`，常量/诊断前缀使用 `SWE_TEST_RUNNER...`；入口固定为 `scripts/swe_test_runner/runner.py`。
- **禁止**把任何外部 SWE 测试脚本、测试执行器、判题器、批次编排器、目录、类或新 reason code 命名为 `Harness` / `harness`。后续 Agent 新建或重命名 SWE 测试程序时必须使用 `SWE_TestRunner` 系列语义，不得重新引入 `swe_harness`、`harness.py`、`HarnessConfig` 等名称。
- `Harness Engineering` / `L3 Harness` 是五层架构中 Tool、Execution Lease、Store、Effect 与执行环境的正式责任域，只能用于产品运行时架构，不能指代外部测试程序。
- `docs/archived/`、`docs/test-issues/`、旧 trace/result 中已经记录的名称可作为历史事实保留；活动代码、规范文档和新产物不得复制这些旧名称。若必须兼容旧 schema 字段，应显式标记 legacy，并为新写入定义 `SWE_TestRunner` 名称。

## 改代码前必知

- **运行契约已冻结**：当前版本与允许的接缝修复见 `docs/design/contract-freeze-2026-08-30.md`。不得在现有 schema/policy 版本内原地改变字段含义、权限或状态迁移；改变语义必须发布新版本并定义旧快照恢复/拒绝规则。Prompt 不硬编码经验轮数、token、秒数或候选条数，机械阈值只放在 policy/schema。

- 接口驱动、无全局状态：依赖全部经 `runner.RunnerDeps` 或 `scheduler.New` 注入。
- 新增工具必须同步 `internal/tools/known_tools.go`；工具按 allowlist 剪枝，per-node 子集越界 fail-closed。
- Reactor 不得直接驱动状态迁移（无 `SetState`），只能发任务/消息让主循环自然迁移；用户 YAML Reactor 永远异步。
- Graph 当前采用**无 flow generation/correlation token 的单赋值安全基线**：所有非 barrier 节点最多一条静态入边；join / acceptance 的每个 `target_input` 也只能有一条生产边，并行 AND 必须使用不同端口。条件分支各自保留后续与 `end`，禁止共享端口 OR 或汇入同一普通节点；循环体可直接作为 root（隐式初始 activation + 唯一回边）。复杂 OR mux 等 generation/correlation token 落地后再开放；`join →` 下游只能匹配 `completed`。
- Acceptance 节点必须有非空 `task.title` 和写明逐项验收标准的非空 `task.description`。completed 业务结论只通过 `$.verdict` 精确 `eq` 路由，verdict 枚举为 `pass` / `fixable` / `failed`，completed 结果必须省略 `event`。`cited_evidence` 可逐字引用输入 Evidence 的 `ref` 或同条 typed Check Evidence 的 `check_ref`；Runtime 只对可解引用 EvidenceEntry 建立该别名，`output_ref`/CallID/ResultRef 不得充当引用。acceptance 出边禁止无条件、`always`、`completed`、`pass`/`fixable` 事件条件；只允许 `$.verdict eq ...` 业务分支及 Runtime `failed` / `blocked` 兜底事件。证据或能力不足时提交 `status=blocked`；`disputed` 是 Runtime 核验状态，不是 verifier verdict。
- `submit_task_result` 提交即 finalizing（后续工具调用被 fence），同一任务唯一终态提交者；`status=blocked` 需同填 `blocked_reason`。自定义 Graph path 路由字段必须放在 `result` object（如 `result={"coverage":"gap"}`），不能只写 summary/event；结构化终态字段原子落盘失败时任务 fail-closed。新 simple Graph 的 code-change Recovery 固定为 `agentgo.recovery-delta/v4`：L5 冻结最小 `EvidenceContract`，L3 逐段证明完整且新鲜的文件覆盖后只开放 `submit_change_decision`；Worker 可 typed 选择 `edit` / `need_context` / `hypothesis_rejected` / `blocked`，只有 `edit` 才按声明的 `{tool,path}` mutation 顺序进入 `edit_file`/`write_file` 与冻结 CheckContract。证据读取失败时只开放 `hypothesis_rejected` / `blocked`，不得在不完整上下文上修改或继续扩展。证据文件与修改目标相互独立，允许声明新文件；不得重新引入“只读开头后强制编辑”。`recovery_directive` 是单值端口，retry 回放必须替换旧代，历史脏输入由 L3 选择最后一代并记录歧义。acceptance recovery 保持 v2 只读语义，v1-v3 只按历史冻结语义恢复。
- Scheduler create/configure/validate/commit/start 与 Proposal Acceptance 的机械调用使用 **`tool_choice=auto` + 唯一 ToolRouter + L3 required-action gate**，因 DeepSeek thinking 会拒绝 exact/required choice。L3 冻结时必须校验 phase→tool 精确映射；零 tool call 不得自然退出；provider 忽略 `parallel_tool_calls=false` 返回 fan-out 时只 dispatch 首个，其余 call_id 写 skipped result 以保持 Responses 重放完整。Graph 最终交付是经 live matrix 验证的狭义例外：L3 投影掉旧工具历史并注入 phase contract，Model Invocation 仅对该次调用覆盖为 `reasoning_effort=none` + exact `submit_task_result`。两条路径都不得恢复为按 provider/model 名称特判。
- Graph-change coordination 必须结构化收口：先成功 `read_graph`；需要修改走 propose→validate→commit，成功 commit 结束非图 coordination；无需修改调用 `submit_graph_change_decision(decision=no_change)`，不得用自然文本退出。Acceptance 在节点/出路结算后才请求 change，Graph 已终态时只留审计，不发布无未来 Activation 可修改的 Scheduler task。
- 新 Run 默认使用 `context:default/v10` + `provider-replay:openai-compatible/v4`：ModelCapability 只调整 snapshot 总预算、completion reserve 与 RequiredExact replay，不得把普通 Fragment cap 放大到完整模型窗口。最新 Observation 覆盖的探索 history 转为稳定 ContentRef；同 task/attempt/path/content digest 的重复 read/grep/list 只保留最新 bounded preview，Raw History 与 Responses 原子组不得删除。
- 新建 code-change Task/Graph 使用 `progress:code-change/v6`（`ProgressCodeChangeCurrent`）：每 6 个 knowledge turn 进入 `agentgo.observation-delta/v3` checkpoint。v3 中模型提交的自然语言 facts 即使绑定 settled evidence 也只能投影为 `inferred`，独立显示为“待验证观察”，不得进入 confirmed/Session 权威；v2 历史对象按旧 confirmed 语义只读恢复。新 read/grep 与 `workspace:empty` 的 pre-mutation check 只推进 knowledge；只有 phase、真实 workspace change、晚于最后 mutation 的 typed check、artifact digest 前进或关闭 predecessor candidate 才重置 decision stagnation。连续两份无决策前进、累计 24 个探索 turn 仍无首次 deliverable，或首次 deliverable 仍为空且进入 Attempt deadline 前 5 分钟 handoff window 时，先冻结 Observation 再以 `decision_progress_stalled` 交 L5 recovery，避免慢调用跨过 recovery 可执行窗口。no-progress 阶段仍为 reminder=4 / rollover=10 / intervention=18 / hard=24。
- 新 Run 默认使用 `agentgo.run-contract/v2` 与 `interactive/v3`（SWE 为 `swe/v3`）：阶段严格为 execution → verification → recovery → finalization；SWE reserve 为 180/120/90 秒。v1 快照按旧语义恢复。默认 model/tool/token/cost 只记账；显式 `RunContract.Budget` 由 `.agentgo/state/run-budgets` 按 RunID 执法。pre-dispatch cancel、已 dispatch 明确失败 settle、取消/deadline 不确定 settle unknown；Store `Close` 不伪造结算。Recovery retry 仍须 `RecoveryStartPermit`；permit claim 按目标 Task durable，同一 Activation 的新 Attempt 不重复认领，bind/terminal 未放行 retry 时必须立即取消。
- RunContract v2 可冻结 `check_contracts`（check_id/kind/可选 exact_command）。`run_check` 的可见 ID 是 Graph required checks 与 Run check contracts 的并集；kind 或 exact command 不符必须在 Shell 前拒绝。SWE Test Runner 必须从当前 manifest 的 `test_files` 生成跨 shell exact `targeted` 命令，`verification` 固定为 exact `uv run --no-sync python -m pytest -q`；Recovery check gate 同时 const 冻结 check_id/kind/exact_command。只有 Graph required 的 verification 进入 fulfillment，禁止用定向 pass 冒充全量验收。
- Observation checkpoint 是独立 L3 Control Invocation：投影业务历史，只保留 TaskMemory 与当前 Task/Attempt settled evidence authority，前一 Attempt、累计 artifact、DependencyResult/Graph 上游 `ev:*` 不进入 control catalog；wire 使用 `reasoning=none` + exact `record_observation_delta`，冻结 2048 completion tokens、16 KiB tool arguments、32 KiB response。tool schema 与 system catalog 同源列出 claims/post-predecessor/candidate 字面值；L3 只证明 evidence ref 归属，不能证明自然语言 claim 的语义正确。首次 malformed/invalid 用全新投影和有界机械错误回执重试一次，第二次以 `control_contract_unstable` blocked，不做正文提取/JSON 修补。工具从 framework control registry 取得，普通业务轮不得看到；control ToolCall 不进入业务 Responses replay。不得按 provider/model 名称特判。
- Provider HTTP 402/结构化 billing code 使用 `provider_quota_exhausted`，与可重试的 429 `rate_limited` 分开；quota 属于 Run 外部资源，当前 Activation blocked 等待新 Run，不得靠 retry、Context 重建或 Graph recovery 消耗更多调用。`RecoveryRequestIntervene` 必须形成 durable `LoopInterventionRequested`，不得落入 `non_recoverable_error`。
- `submit_task_result` / `submit_recovery_decision` 一旦成功进入 finalizing，L4 只允许结算当轮账本并完成唯一终态事务；finalizing 必须优先于 Attempt deadline、runtime fuse、rollover 与 intervention，禁止先 `task_finalizing` 后 `task_retry`。
- `recovery_action_gated` 是 Recovery handoff 的脱敏 trace 权威；SWE Test Runner 必须按已 `task_result_committed` 的 Recovery Task 取最终一次成功裁决，再把 `first_action` 与下一 Task 的 first-action gate、实际 tool call 逐次对账。Attempt rollover 重放的 raw tool receipt 不得重复计数。缺 gate、工具/路径/check_id 不一致或 `directive_count != 1` 都属于架构事故，不得以 typed blocked 掩盖。
- SWE batch summary 是当前 `.batch_start` 绑定的事务产物：无论 task/启动异常都在 `finally` 原子重写，并用 `completed` / `completed_with_infrastructure_error` / `infrastructure_error` / `not_run` 区分覆盖率；旧 result/judge 不得填充当前批次，incomplete batch 不得显示成普通 X/8 正确率。Graph terminal 后还必须等待 final-report 与当前 Run 全部 Task terminal，禁止 grace 到点时 terminate 仍在途的 intervention/reservation。
- **Graph 终态/交付契约 v3（2026-08-29，`docs/design/graph-delivery-transaction-v3.md`）**：新图默认 `agentgo.graph/v3`，保留 v2 的封闭 status、event 废弃、path 路由与两击 outlet 协议。mutating producer 强制 workspace candidate、禁止 raw `run_shell`；completed 只冻结 `TaskOutcome v3.delivery_id/candidate_ref`，Acceptance `pass` 后由 L5 以 Effect Journal prepared→promotion→settled 提交主根。success GraphOutcome 必须有 `delivery_commit_ref`，prepared/unknown 不自动重放。v1/v2 快照永不静默迁移。
- Graph v3 Acceptance 必须以同一 `DeliveryID` 进入 producer workspace，不得在主根验收旧版本。稀疏 COW 文件视图不是可执行项目树；`run_check`/Shell 必须在排除 `.agentgo`/`.git` 后的可丢弃完整快照中运行，每次调用前覆盖 manifest dirty set。promotion 前必须重算 candidate digest，冻结后任何变化一律 quarantine。
- Graph v3 Check/Observation/fulfillment 的 workspace revision 必须按 `DeliveryID` 从实际 manifest + dirty content digest 计算，并汇总同一 Delivery 的 producer/repair write refs；不得在新 repair Task 中因当前 TaskID 没有亲自 edit 就回退 `workspace:empty`。空 manifest 在 pre-mutation 阶段仍合法投影为 `workspace:empty`，但 `FreezeCandidate` 交付门仍拒绝空 dirty set。
- workspace 业务读只能命中 manifest 已登记 dirty 文件，不得因某个物理文件存在就暴露 owner/manifest/shell cache。`.workspace-owner.json`、`.workspace-manifest.json`、`.workspace-baseline/**`、`.workspace-shell/**` 为内部保留名，业务写入必须 fail-closed。活动租约归零后立即丢弃完整 Shell snapshot，只保留 candidate 审计必需文件。
- workspace Shell 快照的 Python 执行环境必须优先加载 `<snapshot>/src` 与 `<snapshot>`，并在存在时把 `UV_PROJECT_ENVIRONMENT` 绑定到 `<snapshot>/.venv`。否则复制的 editable `.pth` 会穿透回主根，让 post-mutation check 测到旧代码。这是 workspace 执行环境绑定，不是 provider/model 特判。
- Graph v3 Delivery workspace 必须写入 `agentgo.workspace-owner/v1` 并由
  Activation 活动租约保护；`delivery-*` 物理目录名绝不是 TaskID。Watchdog
  只允许清理已有 settled `delivery_commit_ref` 的 success 残留；运行中与
  blocked/failed/cancelled candidate 保留。业务 write/edit 禁止写
  `.agentgo/**`。Candidate 必须经 dirty manifest + 文件 digest 冻结并进入
  durable DeliveryStore，禁止重新拼接字符串 ref。新增 workspace 生命周期时
  必须回归“materialize→Watchdog sweep→edit/read/run_check→freeze→acceptance→promotion”。
- Graph 任务必须把冻结节点类型从 `TaskSpec.NodeKind` 持久化到 `Task.GraphNodeKind` 与 Session 快照；ExecutionLease 只给 controller/agent 注入 `request_replan`，acceptance、旧快照空 kind 与未知 kind 只给 `submit_task_result`，不得从可自定义 route 推断角色；恢复时非空 kind 与 activation 冻结定义不一致必须 fail-closed。新算与复用的 acceptance/未知角色 Lease 都必须通过 read/list/grep/glob/web/submit 正向闭集，Graph Lease 即使 `BusinessTools=nil` 也只能换入 `ToolUnion`，不能泄露完整 registry。
- Graph Result 按 activation 存入 durable Result Store，实际生效边只冻结稳定 `ResultRef`、有界内联值、目标端口与结构化 Evidence；EvidenceRef 按调用/内容身份稳定生成，不得依赖展示序数。合法跨任务回边无 activation 次数上限，emergency fuse 只限制单次 Runtime 调用内不让出控制权的同步机械级联。
- Effect Journal 红线：prepared 未 settled 的副作用恢复时标 unknown，**不得静默重跑**。
- **Session 不自动续跑（2026-08 二期）**：启动永远新建 Session（不读 `active-session` 自动恢复）；进入历史会话（`--resume` / `/session` 解冻）恢复历史上下文，但快照非终态任务一律阻断为 `blocked`、图保持僵尸停驻（`ResumeGraphsForSession` 只剩冻结失败回滚与无 Session 模式启动两个合法调用方）；续跑只能由用户提交新提示词驱动。空会话（`TaskCount==0 && FirstUserInput==""`）在退出/切走/启动清扫时丢弃。
- Gate Abort 携带的结构化 Suggestions 只作提示注入，不自动执行；Gate panic 恢复为 Continue。
- v5 已删除（不要再找）：`internal/cli`、`internal/worker`、`internal/explorer`；V6 已删除：`internal/plan` 整包与 `model.Plan`、验收四工具。
- TUI 是两层渲染模型（2026-08 inline 重构）：默认 `/chat` inline 主态——定稿内容（轮次/反馈/结果全文）经 `tea.Println` 排放进终端 scrollback，不进 alt screen，滚轮翻页归还终端；`/graph`、`/result`、节点详情为全屏层，经 `viewTransitionCmds` 动态 EnterAltScreen + `EnableMouseCellMotion` 捕获滚轮，回 Chat 退出。**`tea.Println` 在 alt screen 下会被丢弃**，所以排放统一走 `pendingEmit` 队列 + `flushEmitCmd`（仅 Chat 主态排放，全屏期攒着回 Chat 补排）。`/dashboard`、`/activity`、`/logs`、`/trace` 已退役，原始日志/Trace 诊断走 `agentgo trace` CLI。
- 机制细节（启动流程、包速览、各子系统语义）见 `docs/agents-reference.md` 与文末索引。

## 任务状态机

`pending → processing → completed / failed / cancelled / blocked`（processing 可回滚 pending 重试；pending 可直接转 cancelled/failed/blocked）。合法迁移权威：`internal/model/task.go`。

## 工具与配置

- 工具分组权威清单：`internal/tools/known_tools.go`；profile 与 agent 声明 schema 见 `docs/tool-profiles.md`；分组表见 `docs/agents-reference.md`。
- 配置仅支持 v4 schema；v3 顶层字段静默忽略；已删字段（`agent_max_loops`、`context_limit`、`modes.gate` 等）显式设置报迁移诊断。权威参考：`config.example.yaml`（全注释示例）+ `docs/yaml-config-guide.md`。

## 约定

- 日志与代码注释**全用中文**；测试断言也用中文错误串（如「未找到」「占用」「冲突」「截断」）。
- YAML 配置键用 `snake_case`。
- 不要假设常用库可用——先查 go.mod 与邻近代码（现有依赖含 `google/uuid`、`gopkg.in/yaml.v3`、`openai/openai-go/v3`、`charmbracelet/*`、`sahilm/fuzzy`）。
- agent 与 store 测试使用 property-based 测试（`testing/quick`）。
- 修改了本文件提及的结构、约定或工作流时，同步更新本文件。

## 跨平台硬约束

AgentGo 同等支持 Windows / macOS / Linux。以下每一条都曾在生产坏过一次，视为硬性要求：

- **测试中文件句柄必须先关闭再让 `TempDir` 清理**。Windows 的 `os.OpenFile` 不给 `FILE_SHARE_DELETE`；凡打开长生命周期 writer（history、snapshot、artifact log、trace writer）的测试必须 `t.Cleanup(func() { _ = x.Close() })`。
- **按代理的缓存命中时必须再验证新鲜度，不能信自己的 Invalidate**。跨代理写无法失效别人的缓存；参考 `FileStateCache`：Put 记 mtime+size，Get 时 `os.Stat` 比对。
- **路径只用 `filepath.Join` / `filepath.Clean`**，禁止 `/` 或 `"\\"` 拼接；`pathutil.ValidatePath` 是唯一权威边界检查。
- **Shell 一律走 `internal/shell`**（POSIX `sh -c`，Windows `powershell -NoProfile -NonInteractive -Command`，刻意不用 cmd）；不要从工具或 hook 直接 `exec.Command("sh", ...)`。
- **行尾 LF，`.gitattributes` 强制**。不要对字面 `"\r\n"` 做比较；解析可能带 CRLF 的输入时在边界处 `strings.ReplaceAll(s, "\r\n", "\n")` 归一。
- **终端输入无跨 shell 的统一「提交」语义**。TUI 用 Bubble Tea `textarea`（Enter 提交，Ctrl+J 换行）；粘贴按平台分两条正式投递路径——macOS/Linux 终端以 bracketed paste 事件投递，Windows ConPTY 不透传 bracketed paste，以高速 `KeyRunes + Enter` 流投递，必须经 `internal/tui/paste_burst.go` 状态机重组（这是 Windows 的正式粘贴路径，禁止回退为固定 Enter 防抖）。任何新输入通路（Interaction、session 选择等）必须建在 Bubble Tea MVU 模型内，不用裸模式。Interaction 动作不得绑裸字母/数字键。
- **Windows NTFS 上 fsync 频率更敏感**。append 密集的 JSONL 日志保持「每次 append  flush+sync」，但绝不在已经过一次 fsync 的路径里加第二次。
- **SWE Test Runner 进程监控必须持有 `subprocess.Popen` 并用 `poll()` 查询生命周期**。禁止用 `os.kill(pid, 0)` 模拟 POSIX 存活探测；当前 Windows Python 会对不存在 PID 抛 `WinError 87`，并可能破坏仍在运行的被监控进程。批次 result/judge 新鲜度必须与 `.batch_start` marker 使用同一文件系统 mtime 权威，禁止拿独立 `time.time()` 与 NTFS mtime 做零容差边界比较。
- **SWE Test Runner 渲染 v4 YAML 时，`Path` 占位符必须先归一为 forward slash**。Windows `project_root` 与 `agents[*].system_prompt_file` 同受配置红线约束；只转换 `Path` 类型，禁止顺手改写 URL、model、token 等普通字符串。
- **SWE Test Runner 自行固定 stdout/stderr UTF-8**。公开命令入口必须调用统一 console 配置，不能依赖 Windows 活动代码页、`PYTHONUTF8` 或调用者额外传 `-X utf8`；不支持 `reconfigure` 的嵌入式流保持调用方语义，持久化结果仍显式使用 UTF-8。
- **SWE Test Runner 清理 disposable worktree 时必须处理 Windows Git object 的 ReadOnly 属性**：`shutil.rmtree` 使用 `onexc`，只对 Windows `PermissionError` 清除 ReadOnly 后重试原操作一次；非 Windows、非权限错误和重试失败全部原样 fail-closed，禁止 `ignore_errors` 或吞掉文件占用/真实 I/O 故障。回归必须包含同一路径连续两次清理。
- **新增 CI 时应同时跑 `ubuntu-latest` 与 `windows-latest`**——上述故障在 POSIX 上几乎全是静默的。

## 运行时文件访问边界（早期阶段，有意为之）

所有运行时文件/Shell 工具（`read_file`/`write_file`/`edit_file`/`list_dir`/`grep_search`/`glob_search`/`run_shell`）被限制在 `ProjectRoot` 内，工具内与 path-boundary Gate 双重执行，对所有 agent kind 一视同仁。这是**有意且暂时**的：在按代理能力声明体系成熟前，**不要**给单个工具加逃生口或「就这一次」例外。注意不对称性：YAML 配置层路径（如 `agents[*].system_prompt_file`）**允许**绝对路径——它以用户完整权限在启动期解析，与运行时工具权限是两个信任域。

## 交付约定（shipping conventions）

以下规则因其缺失反复浪费过工程时间，对任何非平凡改动视为硬性要求：

- **「完成」= 单测通过 + 二进制真实启动过一次 + 断言到预期产物**。本项目的重复故障模式是「装配漏接」：各包单测都绿，但跨包握手（bootstrap 装配、Gate/Reactor 注册、跨子系统状态注入）从未被端到端执行过。凡触及子系统边界的特性，汇报完成前：跑起二进制、端到端走一遍新路径、断言预期产物（文件落盘 / 事件发出 / 日志行出现）。5 行冒烟测试能抓到 100 行单测抓不到的东西。
- **修 bug 的提交必须同提交更新 `docs/activate/KNOWN_ISSUES.md`**。只记录当前可复现、未解决的问题；已解决的条目在同一变更中移除或归档其修复证据。

## 文档索引

- `docs/agents-reference.md` — 自本文件迁出的参考手册：启动流程、包速览、核心机制详述、工具分组、配置细节、trace 子命令
- `Archtechture.md` — 详细系统设计与组件职责
- `TraceGuide.md` — trace 工具与事件参考
- `docs/activate/ReactiveSystem.md` — v5 Gate + Reactor 架构
- `docs/activate/MemoryManageSystem.md` — v5 Memory 设计与迁移
- `docs/design/interaction.md` — Interaction 状态机、Graph approval/Shell effect 边界、TUI/Web 响应契约
- `docs/design/per-node-capability.md` — per-node 能力覆盖（publish_task tools/model/isolation）设计
- `docs/design/workspace-isolation.md` — 按任务写时复制执行隔离（overlay / 合并协议 / 触点表）设计
- `docs/design/session-isolation.md` — Session 生命周期隔离（冻结/切换/解冻协议、Graph 归属、保留策略交互）设计
- `docs/design/tui-inline-scrollback.md` — TUI 两层渲染模型（inline scrollback + 全屏层）设计与实现记录
- `docs/design/graph-terminal-contract-v2.md` — Graph 终态契约 v2（封闭 status、event 废弃、数据流路由、两击升级协议）设计
- `docs/design/graph-delivery-transaction-v3.md` — Graph v3 Delivery Transaction（候选、验收、promotion 与恢复）设计
- `docs/activate/KNOWN_ISSUES.md` — 当前限制、验证缺口与可复现的开放问题
- `docs/test-issues/` — 集成测试与 SWE 测试问题的阶段化管理（文件名以 `YYYY-MM-DD-HHMM-` 开头；问题用 `SWE-NNN` 跨文档稳定引用；每阶段修复完毕重跑 8 题 Flask 批测作回归证据）
- `docs/tool-profiles.md` — 工具 profile / agent 声明 schema
- `docs/archived/` — 历史 RFC 与已完成的升级计划
