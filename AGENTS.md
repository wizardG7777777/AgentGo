# AGENTS.md

本文件是仓库实施约束入口。当前 Graph 使用唯一 `agentTask` 节点，按输入就绪执行，迭代向同一图追加新实例，图级完成与交付。职责权威为 [五层规范](docs/design/five-layer-engineering-architecture.md)、[工具契约](docs/design/tool-taxonomy-and-contracts.md)、[冻结基线](docs/design/contract-freeze-2026-08-30.md)。实施来源和文件清单见 [agentTask 计划](docs/design/dataflow-graph-simplification-proposal.md)。旧控制图文档不得用于恢复已删除的执行路径。

## 项目与五层接缝

AgentGo 是 Go 1.25 多 Agent 数据流图系统，模块 agentgo。Scheduler 在业务图外创建或更新图；Runtime 冻结就绪输入，发布唯一 Task；Runner 按真实 route 认领，L4 Loop 使用 L3 工具完成工作，TaskOutcome 回填为不可变结果。模型调用完成、工具执行、节点完成、图完成和 Python 判题是不同事实。

| 层级 | 职责 | 实际实现与接缝 |
|---|---|---|
| L1 Model Invocation Engineering | 完整请求校验、协议编码、HTTP/SSE、归一化与时序 | internal/llm；不装配角色/记忆/Graph/UI |
| L2 Context Engineering | 指令、目标、记忆、历史、工具及附件装配；封存与重放；输出订阅 | internal/contextruntime、contextcontract、contextcompiler；taskmem render/update；存储经 L3 端口 |
| L3 Harness Engineering | 权限、Lease、ToolRouter、工具、工作区、Store 与 Effect | agent/execution_lease.go、tool_registry.go、tool_router_snapshot.go、tool_call_identity.go；llm_executor.go 的 gate/dispatch；tools/store/workspace/effect/shell/gate |
| L4 Loop Engineering | Task 的 Attempt/Turn、错误重试、取消和唯一终态 | agent/agent.go、state.go、loop_progress.go、finalization.go；runner/runner.go 认领外壳；loopstore；bootstrap/task_outcome.go 终态持久化接缝 |
| L5 Graph Engineering | agentTask 定义、输入就绪、增量调度与图级交付 | graph/dataflow_contract.go、dataflow_inputs.go、dataflow_store.go、dataflow_runtime.go、dataflow_terminal.go；bootstrap/dataflow_runtime.go；delivery/store.go |

混合包按函数职责划分，不把整个 agent/runner 包归为一层。Bootstrap 是依赖装配入口；Trace/UI 是横切面，不是第六层。

## 数据流图不变量

- `kind` 只接受 `agentTask`。旧 controller/agent/router/tool/approval/acceptance/subgraph/join/wait_event/end 已退役，无别名、wrapper 或自动转换。
- 图没有 root、next、when、end_outcome。边由 inputs 推导；同一输入槽单赋值，多上游使用不同槽。每个 revision 无环，迭代用新 node_id，不能重开旧节点或覆写结果。
- 初图可以只有调查任务，不要求完整成功路径或预先分配所有交付物。输入齐备且实际执行能力可用才派发；缺输入/执行者必须显示原因，不猜测或绕过授权。
- Activation 冻结定义与输入。未激活节点可以修改/移除；已激活/已结束节点的变化用新实例。应用变更必须 CAS，request_id 不得换内容；同一 Run 只创建一张顶层图。
- Scheduler 依据持久化图事件规划，业务节点仅提交结果/请求规划。没有进展轮数、新知识、强制 record 或独立 Proposal Acceptance 模型关卡。send_message 仍只传递信息。
- ResultRef、候选和证据带完整来源。下游检查/修改必须使用输入候选，不能读取旧主根冒充候选。工作基线由输入候选的实际谱系自动解析：同谱系取后继版本，独立分支明确拒绝任取或隐式合并；workspace_input 已退役。
- complete 是图级动作：校验选定结果、在途结算和处置说明，冻结完成意图，提交必要文件，持久化回执后才成功。普通复核任务没有提交特权。
- 候选版本不可变，新修改建立新版本。Shell 实际差异进入同一工作视图。主根基线冲突拒绝覆盖，Effect unknown 不自动重放；多文件提交不假称 OS 原子事务。
- 不自动续跑历史 Session。新目录不读取旧图日志执行；历史磁盘保留，不编写迁移器，不重新解释旧 Trace。

## 工具与具体实现

核心四类 13 个入口，不代表每个 Agent 都拥有全部工具：

| 类别 | 工具 | 文件 |
|---|---|---|
| 执行 | run_shell、read_file、apply_change、submit_task_result | tools/shell.go、local_read.go、local_write.go、submit_result.go |
| 编排 | read_graph_definition、apply_graph_change、control_graph、request_replan | tools/graph_authoring.go、graph_schema.go、plan_control.go |
| 检视 | inspect_board、inspect_node、read_evidence | tools/inspection.go、content_ref.go、graph_evidence.go |
| 通信 | send_message、request_user_input | tools/meta.go、agent_question.go |

- known_tools.go 是名称权威。可选 Web/Team 工具仍须注册与授权；未知名称不能经 fallback 混入。
- apply_change 创建、覆盖和精确替换共用写入链；路径、锁、版本与实际产物记录不能分叉。run_shell 覆盖搜索、构建、测试，保留启动、退出码、输出、取消/超时和关联身份。
- 旧 write_file/edit_file/run_check/record_observation_delta/submit_change_decision 等模型工具不恢复。
- read_graph_definition 不带 graph_id 时提供能力目录；route_ref=default 表示默认队列，不是 Agent 名称。其他 route_ref 必须来自目录。
- apply_graph_change(create/update) 只做机械校验和原子应用。运行图追加后自动调度，无需重新 start。control_graph 支持 start/cancel/complete。
- agentTask 最终纯文本可正常结束并原样登记；可选 submit_task_result 交付结构化 JSON，进入 finalizing 后后续工具被 fence；无 event/verdict/cited_evidence 专属参数。业务复核结论可放普通 result 字段，不触发控制跳转。
- request_replan 登记图规划事件；普通消息不唤醒、不创建 Task/Activation、不授予权限。
- graph_input 是已声明输入的版本，不是旧瞬时事件。UI 的 ProvideGraphInput / Web /api/graphs/input 共用作用域与版本校验，旧 /event 入口退役。

## 版本和配置

| 域 | 当前版本 |
|---|---|
| L1 / L2 | model-request/v1、model-result/v1；context/v3、context:default/v12、provider-replay:openai-compatible/v6 |
| 模型历史/输出 | model-history/v2、model-output/v1 |
| Graph / Result / Completion | graph/v7、agent-task-result/v2、graph-completion/v1 |
| TaskOutcome / TerminalIntent | task-outcome/v5、terminal-intent/v3 |
| Candidate / Delivery | candidate/v1、delivery/v2 |
| Session / Lease | Session 8、execution-lease/v4 |
| Run / Progress / Shell | run-contract/v3、progress-contract/v2、shell-execution/v2 |
| SWE | swe-result/v5、swe-judge/v2；pytest-phase-report/v2、swe-test-execution/v1；suite/v1（Flask）、suite/v2（Gin）、go-test-report/v1 |
| SWE 资源归档 | swe-artifact-manifest/v2；旧 v1 只读校验，不改写 |

schema 带 agentgo. 前缀。文件必须显式声明 llm.request_contract=agentgo.model-request/v1 和 graph.request_contract=agentgo.graph/v7。llm.stream、observation_model、max_subtask_depth 等退役字段拒绝；不由默认配置填补必需的文件标识。两个协议 Responses/Chat Completions 都只使用 SSE，不自动切换或降级。

Graph 定义、运行、请求回执、输入及完成意图共用 `.agentgo/state/graphs-v8` 的摘要链日志，不另建影子 authoring/completion 账本。TaskOutcome、Loop、TaskMemory、Delivery 分别使用 task-outcomes-v4、loop-facts-v3、taskmem-v3、deliveries-v3。不可变候选在 `.agentgo/candidates-v1`。L2 使用 context-snapshots-v3，启动探针使用 model-probes-v3；run-usage 目录不变。

## 原文请求与普通文本结束

按用户要求暂时停用模型正文引用替换：全部已有对话/工具历史原文装配，不做最少保留轮数、重复读摘要或片段/分区/原子组大小裁剪。read_file 重读仍返回正文，force_full 已退役；inspect_node 直接返回执行记录和最后回复。保留模型整体窗口、输出规格和协议原子完整性。SWE 模型上下文统一配置为 880000 tokens。见 [原文模式记录](docs/design/raw-context-and-plain-results.md) 与 [L3 接缝修复](docs/design/l3-workspace-seams.md)。

## 必须保留的执行与输出纪律

- Task 状态权威仍为 model/task.go；只有授权错误重试可以 processing→pending。图迭代不能借此重开已结算节点。
- 依赖经 Bootstrap/Runner 注入。派发前核对实际工具和 workspace 组件，不能只核对名字。Reactor 不直接 SetState，用户 YAML Reactor 异步。
- 真实 provider 配额、用户显式 deadline/预算、HTTP/进程超时分别处理；不用默认经验轮数强制结束调查。
- L2 完成全部上下文装配和 Request 封存；Snapshot 必需持久化成功后调用 L1。原始历史不变，工具交换原子；部分 SSE 参数不得执行。
- WatchModelOutput 是 UI 模型输出统一入口，eventCursor 为“流式事件游标 / SSE events cursor”；慢消费者/过期游标显式重同步，UI 不承担轮次持久化。
- TUI 默认 /chat inline；/graph、/result 与详情才进入 alt screen，scrollback 继续走 pendingEmit/flushEmitCmd，不恢复旧页面。
- LLM 时序只由 L1 采集，经 Trace 展示，不进入 Prompt 或控制逻辑；不可用值保持缺席，不记录凭据。
- watchdog 生产代码不改为规划/复核器。活动工作区和未确认副作用保留；成功候选的回收由已保存交付事实决定。

## 测试与交付

使用 go test ./...、go vet ./...、go build；跨子系统须实际运行二进制并核对产物。scripts/local_fake_provider_smoke.py 覆盖两协议、增量图、普通检查任务、候选一致性与图级交付；不能冒充真实 SWE。

SWE Test Runner 唯一入口为 scripts/swe_test_runner/runner.py。执行类入口 probe/task/batch/verify-candidates 在副作用前检查 SWE_API_KEY、SWE_BASE_URL、SWE_FAST_MODEL、SWE_FLAG_SHIP_MODEL，只列缺项；离线 storage report、cleanup plan/apply、archive verify 不读取模型配置、不要求这些变量、不调用 provider。原生入口由外部 Python 管理独立尝试、scratch、进程、预算、归档和清理回执；容器显式设置 SWE_RESOURCE_OWNER=container，由既有容器外层管理资源，禁止内外双重清理。角色模型来自 setting.swe-flask.yaml；模型探针去重。真实命令、测试/源码/依赖身份及 pytest/Go 正式判题由外部 Python Runner 负责，AgentGo 不恢复 CheckRecord。

Gin 通过同一入口的 `--suite gin-1` 选择，角色模型/工具配置仍来自 setting.swe-flask.yaml，角色正文来自 prompts/swe/gin。Go 判题固定工具链、核对实际模块和依赖，目标测试全部通过且无跳过，完整回归不能新增失败、漏跑或丢失原先通过的测试。Windows 既有平台失败单列，不能删除测试换取全绿。Gin 当前仅纳入原生 PowerShell 与 Linux zsh 验证；不要扩展容器流程或把 WSL zsh 成绩当作 macOS 原生成绩。

Windows `firewall-check` 不调用模型、不读取模型配置，验证临时测试 EXE 的执行前规则、普通权限和回收；其系统规则操作需要一次 UAC。执行入口启用 `--windows-firewall` 时必须先通过该检查。禁止关闭全局防火墙或通知来掩盖授权问题；新增可见防火墙提示、助手失联或清理未知时停止批次。管理员助手只处理本用户、本测试范围的规则，测试与模型进程不得整体提权。

受管命令的防火墙 lease 绑定命令进程树：启动前记录全部所需程序（包括 Python venv 的基础解释器和 CTest 选中的子程序），确认进程树退出后归还；启动失败回收已申请部分，状态未知时保留并报错。相同 EXE 的并发使用者持有独立 lease，最后一个归还后才撤销规则；命令内重复程序去重。整题 release_run 仅作兜底，不能把每条已结束命令的 lease 都积到整题结束。保留助手 256 个活动 lease 的限制，须验证超过 256 次顺序执行不会耗尽它。

`rejudge` 不读取模型配置、不调用 provider：只对已停止、已结算且归档核验通过的原生模型尝试，在同平台新建受管尝试应用原补丁；旧判题未保存补丁时可从归档重建原交付源码，但必须有失败回执，且受保护输入与原、新基线完全一致。题目定义、资格清单和源码产物必须一致，拒绝篡改。新旧报告分别保留，记录来源和新增调用数 0，不续跑历史 Session、不重复计算覆盖。Vitest 3/4 在执行前按声明真实路径选择上游测试规格，不依赖 CLI 子串过滤和 exclude 继承；报告若仍含主根 `.agentgo` 候选副本或范围外测试则拒绝，禁止事后过滤失败结果。

Vitest 新增测试文件只能在测试补丁应用前缺席，须以固定 base/fix 的 Git 新增记录及题目 test_files 共同证明。空基线明确记为未执行测试、零通过；红态、绿态与正式判题仍检查全部声明文件，不把删除或拼写错误当作可忽略项。

Cargo 测试清单查询与执行必须使用 artifact 所属 package 根目录，与 Cargo 原生语义一致；package_id、manifest 和工作树边界必须一致，保存执行目录证据。原源码复判保持目标和测试清单不变，允许新资格通过集合严格增强，禁止丢失旧通过项或增加未知测试；差异必须留证，不能重写历史成绩。

原生题目显式声明的 required_regression_tests 在应用测试补丁后必须被收集和实际执行，红态须为这些回归的真实失败。平台夹具适配只能修改声明的测试文件，必须固定输入/输出摘要、平台和精确替换，进入题目身份及执行证据；源码补丁保持独立，模型和 Judge 使用同一冻结测试输入。不得把平台条件排除、跳过或无关测试失败当成资格通过。

更换 SWE 题目须同步题目清单与对应题面文件，启动前核对当前清单的全部题面存在、非空且已纳入冻结摘要。红绿资格通过不等于模型输入已完整，不能以资格验证替代题面入口检查。旧题成绩保留为历史，不得移植给新题。

CTest 内存安全题须显式声明 `environment.sanitizer=address`，基线、红绿验证和最终判题使用同一检测环境并记录编译器及参数身份。Windows 使用具备 AddressSanitizer 的原生 MSVC 开发环境，Linux 使用原生 C++ 编译器；缺少工具或编译失败不得降级普通构建或放宽红态条件。

SWE 补丁导出的大路径集合使用 NUL 分隔的 literal pathspec 经 stdin 交给 Git；不能拼接超长 argv，也不能扩大为整棵工作树暂存。重置独立 index 后核对 staged 集合仍在选定生产文件范围内，再导出补丁。CMake 产物须以源/构建目录元数据、Git 已跟踪/原始输入归属和已知产物类型共同识别；保留输入摘要及归档，真实测试、声明测试和原有配置仍受保护，禁止按 `build*` 名称一律放行。生成产物可以未跟踪但未被 ignore，不得把这两种状态混同。原归档误判的纠正必须证明原受保护输入未变，并保留旧 judge。

SWE 权限测试使用命令独立 scratch；POSIX Cargo 子执行也纳入进程和目录归属。只在相应进程树确认退出后恢复 owned scratch 的必要收尾权限，并记录原模式与身份；不得为盘点而修改运行中的测试权限、跟随外部链接或提权测试。权限拒绝造成的不完整盘点仍受新鲜度和最终完整盘点约束，未知状态继续保留并停止。

SWE 运行中盘点可以使用逻辑字节上界，未计算的 unique_file_bytes / allocated_bytes 必须为 null，不能伪装成精确值。缓存逻辑量可能超限时先精确去重再判定，收尾仍完整精确盘点；所有依赖副本继续计量。目录枚举句柄须在逐文件元数据处理前关闭，避免 Windows 子目录句柄阻止上级目录发布。每次盘点结果写入 inventory-samples.jsonl，后来的成功样本不得覆盖历史超时原因。临时扫描/新鲜度预算为 450/600 秒，监控线程收尾独立限额 30 秒，不随扫描预算倍增。

SWE 盘点须保留具体读取操作、errno/winerror、相对路径及复核状态。仅对受管盘点中的 Windows 5/32/33 进行只读有界重试：每次故障最多追加 4 次读取，全样本共用 1 秒等待额度并服从扫描截止时间；等待前后均不跟随链接地复核父路径。消失的条目按动态目录处理，链接不遍历，枚举中途失败必须先关闭句柄并丢弃局部结果；持续/未知拒绝访问仍停止，不能提权、改 ACL 或重放模型/业务命令。scratch 权限测试的新鲜度宽限仅用于既有 POSIX 场景。修改工作区源码不热更新已冻结的评测运行副本。

Windows 命令收尾读取本 Attempt 元数据时须与其 persist 共用互斥边界；运行期盘点 worker 从父进程接收脱离共享状态的最小所有权/布局/策略快照，仍核对实际根标记、目录身份和路径，不重读正在替换的 attempt.json。进程树退出事实与 scratch/文件恢复结果分开记录；已证明进程退出后发生文件恢复错误，不得将 quiescent 倒改为 false。真实进程未知仍停止并保留授权；记录具体操作、原异常类型、errno/winerror 及原因链，缺失字段保持缺失。

原生归档必须保留合法的文件系统名称，包括 POSIX 非 UTF-8 字节名。v2 清单保留原路径并校验 ZIP 安全成员名的对应关系；不得替换字符、跳过文件或删除测试换取归档成功。链接仅作数据保存，不跟随；旧 v1 归档按原布局只读校验，未知格式拒绝。

2026-09-12 已用当前 Graph v7 / Context v12 二进制完成真实 Responses SSE Flask-8：8/8 修复成功，无新增测试失败、无未结算调用或超时强杀；业务模型调用 335 次。上一轮 Context v11 为 6/8。版本、完整日志和残余工具可用性问题见 [本轮报告](docs/test-issues/2026-09-12-flask8-after-context-and-l3-fixes.md) 与 KNOWN_ISSUES；通过不等于模型不会误调用工具。真实测试遇推理服务429/500等HTTP错误或必需变量问题仍须停止并报告，不暴露凭据。

新测试与状态机边界优先用确定性测试及 testing/quick。并发域跑 race，CI 同时覆盖 Windows/Linux/macOS。修复同步更新 KNOWN_ISSUES；完成前提交删除/迁移/重写/新增对账。

## SWE 容器与 Git 提交边界

- 容器工具目录为 `SWE_LinuxContainers/`；CI、脚本和当前使用文档必须引用实际目录。容器验证仅覆盖 Linux，Windows/macOS 仍需原生验证。
- 允许提交容器构建/运行脚本、必要的 Runner 与 Prompt 适配、测试、文档和仅含占位符的配置示例。AgentGo Linux 二进制由运行者通过 `--agentgo-binary` 提供，不随源码提交。
- 真实 `config.json`、`.env`、密钥/令牌/认证文件、`.build/`、`artifacts/`、完整探针/行为日志、测试工作区和镜像归档只保留在本地。`.gitignore` 与 `.dockerignore` 分别保护 Git 暂存和 Docker 构建上下文；文档中的真实配置、凭据或个人路径也必须移除或替换为占位符。
- 隐私检查只输出文件名、规则名及是否命中，不打印、截断或哈希凭据真值。配置示例不得从真实配置直接复制；使用明确的无效占位符。
- 提交前按明确文件清单暂存，核对 `git diff --cached`、新增文件及忽略规则；不使用 `git add .`、`git add -A` 或强制加入被忽略文件。保留与本次任务无关的改动和历史 stash。
- 推送前运行相关检查，提交后核对目标远程地址与分支；推送后直接查询远程提交，确认与本地提交一致。真实 SWE 成绩只引用已有对应批次证据，离线测试和冒烟不作为新的真实 SWE 成绩。

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
- **Windows Node 依赖联接必须能随工作副本迁移**。SWE Vitest 准备阶段仅把自有 node_modules 内、目标仍在工作树内的 NTFS junction 转换为相对符号链接，保留依赖图而不递归展开复制。先核对全部边界与普通用户符号链接权限，失败则停止准备；测试进程不整体提权。转换回执纳入环境身份，不能保留指回旧基线的绝对目录链接。
- **Windows 工作副本复制必须保留源符号链接的目录/文件类型**。目标目录可能尚未复制，不能通过目的地是否存在推断链接类型；统一使用 `workspace/copySnapshotSymlink`，保留相对目标和长路径行为，不跟随链接复制依赖数据。POSIX 继续使用原生 `os.Symlink`。
- **SWE Test Runner 清理 disposable worktree 时必须处理 Windows Git object 的 ReadOnly 属性**：`shutil.rmtree` 使用 `onexc`，只对 Windows `PermissionError` 清除 ReadOnly 后重试原操作一次；非 Windows、非权限错误和重试失败全部原样 fail-closed，禁止 `ignore_errors` 或吞掉文件占用/真实 I/O 故障。回归必须包含同一路径连续两次清理。
- **新增 CI 时应同时跑 `ubuntu-latest` 与 `windows-latest`**——上述故障在 POSIX 上几乎全是静默的。

## 文件访问边界

read_file/apply_change 的路径受 ProjectRoot/当前 workspace 和 pathutil 双重边界约束；框架内部状态不能通过业务路径读写。run_shell 的工作目录受同一边界约束，但命令正文仍有宿主 Shell 的能力，不能把“没有 Web 工具”当作没有网络或把“没有 apply_change”当作 Shell 只读。

文件工具参数、read_file 文件头和 run_shell.working_dir 共用逻辑项目路径；执行时映射到任务副本。Git 项目的 Shell 副本必须拥有独立 index/refs/objects 和冻结输入基线，禁止共享宿主 .git 或向父目录发现仓库；副本 Git 提交不等于 Delivery。Shell 恢复/删除文件必须同步撤销旧 overlay 条目。实现为 workspace/snapshot_git.go、shell_root.go、dataflow.go 和 tools/shell.go、local_read.go。inspect_node/inspect_board 也必须从 Graph 权威展示未派发节点，不以“没有 Task”隐藏等待原因。

启动期 YAML 的 system_prompt_file 允许绝对路径，以用户权限加载；这与运行时工具的授权边界不同。不要为单个运行时工具增加临时越界入口。

## 文档入口

- docs/design/dataflow-graph-simplification-proposal.md：实施计划及当前实现对账。
- docs/design/five-layer-engineering-architecture.md：职责及实际文件索引。
- docs/design/contract-freeze-2026-08-30.md：当前版本与拒绝边界。
- docs/design/tool-taxonomy-and-contracts.md：四类工具；config.example.yaml：配置。
- docs/agents-reference.md、Archtechture.md、TraceGuide.md：运行与诊断。
- docs/activate/KNOWN_ISSUES.md：真实未完成项；docs/test-issues、docs/archived 保留历史证据。

可选 Team 初建使用 provision_agent_team(graph_request_id=R)，再 apply_graph_change(create,request_id=R)。图外规划任务继承其目标图作用域；图内 agentTask 不因名称获得 Team/编排权限。
