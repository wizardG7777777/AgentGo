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
| SWE | swe-result/v5、swe-judge/v2；pytest-phase-report/v2、swe-test-execution/v1 |

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

SWE Test Runner 唯一入口为 scripts/swe_test_runner/runner.py。四变量 SWE_API_KEY、SWE_BASE_URL、SWE_FAST_MODEL、SWE_FLAG_SHIP_MODEL 在副作用前检查，只列缺项。角色模型来自 setting.swe-flask.yaml；模型探针去重。真实命令、测试/源码/依赖身份、Flask 导入位置和 pytest 判题由 Python 负责，AgentGo 不恢复 CheckRecord。

2026-09-12 已用当前 Graph v7 / Context v12 二进制完成真实 Responses SSE Flask-8：8/8 修复成功，无新增测试失败、无未结算调用或超时强杀；业务模型调用 335 次。上一轮 Context v11 为 6/8。版本、完整日志和残余工具可用性问题见 [本轮报告](docs/test-issues/2026-09-12-flask8-after-context-and-l3-fixes.md) 与 KNOWN_ISSUES；通过不等于模型不会误调用工具。真实测试遇推理服务429/500等HTTP错误或必需变量问题仍须停止并报告，不暴露凭据。

新测试与状态机边界优先用确定性测试及 testing/quick。并发域跑 race，CI 同时覆盖 Windows/Linux/macOS。修复同步更新 KNOWN_ISSUES；完成前提交删除/迁移/重写/新增对账。

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
