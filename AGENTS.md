# AGENTS.md

本文件是仓库实施约束入口。当前职责与接口以 [五层规范](docs/design/five-layer-engineering-architecture.md)、[工具契约](docs/design/tool-taxonomy-and-contracts.md) 和 [当前冻结基线](docs/design/contract-freeze-2026-08-30.md) 为准。历史设计不能用来恢复已退役的运行路径。

## 项目与调用链

AgentGo 是 Go 多智能体图编排系统。Scheduler 通过图工具创建或更改持久化图；Graph Runtime 发布 Activation 对应的 Task，Runner 按 route 认领并运行 Agent Loop，结果经唯一终态事务回填图并完成 Delivery。工具请求、工具实际执行、节点结果、图结果与 Python 测试判题是不同事实。

Go 1.25，模块 agentgo。YAML/JSON 配置使用 v4 嵌套 schema。Responses 与 Chat Completions 是两个显式协议，两者均只使用 SSE，不自动降级或切换。

## 五层职责、文件与接缝

| 层级 | 职责 | 实际实现 | 跨层接缝 |
|---|---|---|---|
| L1 Model Invocation Engineering | 完整请求校验、协议编码、HTTP/SSE、响应归一化、失败与时序事实 | internal/llm | 仅消费 L2 封存 Request；不读取角色文件、记忆、Graph 或 UI |
| L2 Context Engineering | 指令/目标/记忆/历史/工具/非文本装配，预算投影、封存、重放检查、模型输出订阅 | internal/contextruntime、contextcontract、contextcompiler；taskmem 的 render/update | 通过窄接口使用 L3 存储；不 import Agent/Graph/UI；不强制模型提交观察报告 |
| L3 Harness Engineering | 工具授权与分发、执行环境、Lease、Store、Effect | internal/agent/execution_lease.go、tool_registry.go、tool_router_snapshot.go、tool_call_identity.go；llm_executor.go::Execute 的 gate/dispatch；internal/tools、store、effect、workspace、shell、gate | 冻结能力与选项交 L2；完整工具调用经 gate 执行并结算事实 |
| L4 Loop Engineering | Task/Activation 的 Attempt、Turn、错误重试、取消与唯一终态 | internal/agent/agent.go::processTask、state.go、loop_progress.go、finalization.go；internal/runner/runner.go 的认领外壳；loopcontract/loopprogress/loopstore | 决定何时调用与终止，不装配 Prompt，不按经验轮数强制报告或交接 |
| L5 Graph Engineering | 定义、版本、Activation、路由、验收与交付 | internal/graph、delivery；internal/bootstrap/graph_runtime.go 等图桥；internal/scheduler/activator.go；internal/tools/graph_authoring.go | 图事实驱动调度；不能把 API 完成或普通消息当作节点成功 |

混合包按函数职责划分，不能将整个 agent/runner 包归为单一层。Bootstrap 是依赖装配入口；Trace 和 UI 是横切面，不是第六层。完整文件索引见五层规范。

## 当前工具契约

四类核心目录共 13 个入口；这不代表每个角色都获得全部工具。

| 分类 | 工具 | 实现 |
|---|---|---|
| 执行 | run_shell、read_file、apply_change、submit_task_result | tools/shell.go、local_read.go、local_write.go、submit_result.go |
| 编排 | read_graph_definition、apply_graph_change、control_graph、request_replan | tools/graph_authoring.go、graph_schema.go、plan_control.go |
| 检视 | inspect_board、inspect_node、read_evidence | tools/inspection.go、content_ref.go、graph_evidence.go |
| 通信 | send_message、request_user_input | tools/meta.go、agent_question.go |

工具名权威为 internal/tools/known_tools.go。web_search/web_fetch 及可选 Team 工具是目录之外的显式能力；注册和角色授权分别核对。新工具必须更新目录与配置/模板，不允许未知工具靠 fallback 混入 ToolRouter。

- apply_change 统一文件创建、覆盖与精确替换；路径、版本/行锚点、逻辑路径锁、写入、Effect 与产物登记共用一条实现。不得重建 write_file/edit_file 别名。先说明决策只是提示词要求，没有 submit_change_decision 前置关卡。
- run_shell 是通用命令执行工具，所有角色走同一事实记录链。ShellExec v2 区分 process_started、实际退出码/作用域、超时与取消，失败保留部分输出。tool_call 在 gate 前发生，tool_dispatched 也不等于进程已经启动。命令非零退出与工具框架异常分开，不能由 exit=0 推导任务或测试通过。
- 已删除 run_check、CheckStore/CheckContract、强制 Observation 工具/模型/探针、机械单工具阶段、周期轮数/新知识/无进展等默认停止或恢复触发。不得把这些机制迁入 Prompt 解析、L2、其它工具或 watchdog。
- apply_graph_change 的 create/update 共用入口，内部校验并提交；草案仅用于事务暂存，不由模型调用多套创建/校验/提交工具。update 显式提供 expected_revision 和 in_flight=preserve；保留在途执行定义，改变未来执行，不能覆写已结算结果。非法/冲突请求不改变正式图，相同 request_id 不能换内容。
- control_graph 仅 start/cancel；接受取消不意味着副作用已结算。没有暂停/恢复/单节点跳过等隐含能力。request_replan 是请求，不能直接改图；无变更协调通过 submit_task_result 记录 no_change 结论。
- send_message 只传递信息；info/question/reply、回复关联和投递回执不授予权限，不唤醒、不打断、不创建 Task/Activation、不复活终态图。内部用户控制消息与 watchdog 信号另有契约。
- 检视保持 Session/Run/Graph/Task/Attempt/Invocation/CallID 来源身份，分页检测版本变化。大内容经 read_evidence 解引用，不能以任意磁盘路径绕过作用域。
- 验收角色只读闭集统一由 agent.IsAcceptanceToolAllowed 提供给路由和租约校验；允许检视，不允许 Shell、文件写入、普通消息、用户交互或重规划。工具配置仍受实际 registry 与 route 能力约束。

## 数据版本与恢复

| 数据 | 当前新运行版本 |
|---|---|
| L1 请求/结果 | agentgo.model-request/v1、agentgo.model-result/v1 |
| L2 | agentgo.context/v2、context:default/v11、provider-replay:openai-compatible/v5 |
| 模型历史/输出 | agentgo.model-history/v1、agentgo.model-output/v1 |
| Session / ExecutionLease | Session 7、agentgo.execution-lease/v3 |
| Run / ProgressContract | agentgo.run-contract/v3、agentgo.progress-contract/v2 |
| Progress 标签 | code-change/v13、investigation/v8、verification/v4、coordination/v3、final-report/v2 |
| Graph / fulfillment / Delivery | agentgo.graph/v5、agentgo.fulfillment/v2、agentgo.delivery/v1 |
| Shell 事实 | agentgo.shell-execution/v2 |

- 新目录：.agentgo/state 下 graphs-v5、graph-authoring-v2、loop-facts-v2、run-usage-v2、task-outcomes-v2、taskmem-v2、deliveries-v2、context-snapshots-v2。旧目录保留，不编写自动迁移器、不删除历史、不回退旧数据继续执行。目录代次与内部 JSON schema 分开：当前非 Graph TaskOutcome 仍原生使用 v1，图节点使用 v2/v3。
- 文件配置必须显式声明 llm.request_contract: agentgo.model-request/v1。llm.stream、agents[*].observation_model、max_subtask_depth 已退役，明确拒绝，不由默认配置补齐。
- L1/L2 旧入口、Prompt Build、Binding、context override、非流式分支和控制历史投影已退役；不增加 wrapper/alias 或测试专用生产旁路。旧观察锚点不得转换为新的可执行历史。
- 没有 Run deadline 表示没有默认阶段窗口；用户明确设置的 deadline/预算、HTTP/进程超时与真实 provider 配额仍有各自语义。使用量可以记账，不成为默认经验轮数关卡。
- 启动总是新 Session；--resume 或 /session 只恢复可接受版本的历史，不自动续跑。历史非终态任务阻断，图停驻；新提示词才能驱动新运行。空会话按既有策略丢弃。

## 必须保持的业务不变量

- 状态权威是 internal/model/task.go：pending → processing → completed/failed/cancelled/blocked；仅授权的重试可 processing → pending。
- submit_task_result 一旦进入 finalizing，后续工具被 fence，只结算当轮事实并完成唯一终态；优先于 deadline、取消后的重试或介入。blocked 必须带 blocked_reason。自定义路由字段写入 result object，不能只写自然语言 summary。
- Graph 的单赋值端口基线不变：非 barrier 节点最多一条静态入边；join/acceptance 每个 target_input 最多一个生产者。并行 AND 使用不同端口；不支持共享端口 OR。合法回边没有 Activation 总次数上限，同步机械级联 fuse 不得变成 Agent 轮次上限。
- acceptance 必须有明确任务标题和验收条件。completed 只按 $.verdict 精确 eq 路由 pass/fixable/failed；Runtime failed/blocked 单独兜底。证据不足提交 blocked。cited_evidence 只接受可解引用的真实 EvidenceRef，不能编造引用或使用已退役 CheckRef 别名。
- Graph v5 继续单 mutable producer 的 Delivery 基线；未实现多候选联合原子 promotion。mutating producer 使用 workspace，成功必须有已提交 Delivery。验收进入同一 Delivery 候选，不能在主根检查旧版本；冻结后变更必须隔离。
- Candidate 由实际 manifest、dirty content digest 和产物构成，不从路径字符串拼接身份。Shell 使用完整可丢弃快照；稀疏 COW 目录不是可执行项目树。Python 环境必须优先 snapshot/src 与 snapshot，UV_PROJECT_ENVIRONMENT 指向 snapshot/.venv，避免 editable 安装穿透主根。
- workspace 内部 owner/manifest/baseline/shell 目录不对业务开放，不能写 .agentgo/**。活动租约保护 Delivery workspace。watchdog 生产代码本次不修改：只清理已结算交付的 success 残留，运行中及失败/阻塞候选保留。
- Effect prepared 未 settled 的恢复结果为 unknown，不静默重跑。取消发生在派发前与派发后分别记录；关闭 Store 不能伪造结算。
- provider_quota_exhausted 与 429 rate_limited 分开，余额不足不能靠重试、重建上下文或重规划消耗更多调用。错误归因保留具体调用身份，不按 provider/model 名称特判。
- 依赖经 RunnerDeps/Scheduler/Bootstrap 注入。Reactor 不直接 SetState；用户 YAML Reactor 异步。Gate Abort 的建议仅作为材料，不自动执行；Gate panic 沿用既有恢复行为。

## L2 输出与 UI

WatchModelOutput 是模型输出统一入口。游标代码字段 eventCursor，中文“流式事件游标”，英文“SSE events cursor”，由 L2 生成和解释。快照与增量原子衔接，缓存有界，慢消费者和过期游标明确重同步；重启恢复完整输出，不承诺逐 chunk 持久化。UI Hub 仅转接，不承担轮次持久化。

TUI 默认 /chat inline，定稿内容经 pendingEmit/flushEmitCmd 和 tea.Println 排入 scrollback。/graph、/result、节点详情才进入 alt screen；全屏期不能直接 tea.Println，回 Chat 后补排。旧 dashboard/activity/logs/trace 视图不恢复，诊断使用 trace CLI。

## 构建、测试与交付

```text
go test ./...
go vet ./...
go build -o agentgo.exe .
./agentgo -config setting.yaml
./agentgo config doctor
./agentgo trace list
```

跨子系统改动必须实际启动二进制并断言产物，不能只靠包单测。本地双协议 fixture 是 scripts/local_fake_provider_smoke.py，使用本地 HTTP SSE，不访问真实模型，也不执行 Flask。修 bug 同步更新 docs/activate/KNOWN_ISSUES.md；已解决事项移出当前问题列表，保留历史证据。

本次用户授权完成全部工具改造后一次提交并推送；验收 Go 测试与构建。SWE 真实 probe/task/batch/verify-candidates 暂不运行；离线测试与本地 fixture 不能冒充真实 SWE 成绩。阶段证据与剩余项见工具契约第 13 章。

## SWE Test Runner

- 外部测试程序唯一名称 SWE Test Runner，路径 scripts/swe_test_runner/runner.py。不得将外部评测代码命名 Harness；Harness Engineering 只指 L3。
- 公开入口在网络、文件副作用和子进程前一次性校验 SWE_API_KEY、SWE_BASE_URL、SWE_FAST_MODEL、SWE_FLAG_SHIP_MODEL；只列缺项，不输出值。两个模型可相同，探针去重；旧 SWE_MODEL/SWE_BASE_MODEL/SWE_WORKER_MODEL 不回退。
- 角色模型由 setting.swe-flask.yaml 决定。manifest/题目在 scripts/swe_test_runner/suites/flask-8；testbed 使用当前用户的跨平台数据目录，不硬编码用户名。
- 正式测试的命令、范围、被测源码/测试/依赖身份、实际 Flask 导入位置和 pytest verdict 由 Python 负责。AgentGo 不生成测试 CheckRecord，也不把 run_shell 成功当判题通过。
- Python 结果为 swe-result/v4、swe-judge/v2；pytest-phase-report/v2 保留 nodeid/阶段失败集合，swe-test-execution/v1 保存实际执行与输入身份。相同失败数不等于没有新增破坏。
- 批次绑定 .batch_start，每题后及 finally 原子重写；区分完成、基础设施失败、证据不完整和 not_run。Graph terminal 后仍等待在途 Task/final-report/结算，不能到 grace 就杀掉工作。

## 编码约定

中文日志、注释及新测试诊断；YAML 键使用 snake_case，文件 LF。先查 go.mod 与邻近实现再添加依赖。agent/store 的状态与集合不变量优先使用 testing/quick。

LLM 时序只在 L1 事实点采集，通过 trace 展示；不进入 L2 Prompt、Context 摘要或控制流程。不可用字段保持缺席，禁止补零；时序数据不记录 endpoint、IP、凭据或模型正文。不能按模型名分支。

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

启动期 YAML 的 system_prompt_file 允许绝对路径，以用户权限加载；这与运行时工具的授权边界不同。不要为单个运行时工具增加临时越界入口。

## 文档入口

- docs/design/tool-taxonomy-and-contracts.md：工具权责、删除清单、SWE 适配、阶段证据。
- docs/design/five-layer-engineering-architecture.md：五层职责及文件索引。
- docs/design/contract-freeze-2026-08-30.md：当前数据版本与拒绝边界。
- docs/tool-profiles.md、config.example.yaml：工具授权与配置示例。
- docs/agents-reference.md、Archtechture.md：启动与组件参考。
- TraceGuide.md：运行事实及诊断。
- docs/activate/KNOWN_ISSUES.md：当前开放项；docs/test-issues 与 docs/archived 仅保留历史事实，不用旧机制指导新实现。

可选 Team 初建使用 `provision_agent_team(graph_request_id=R)`，随后 `apply_graph_change(create, request_id=R)` 使用同一个稳定值；图 ID 由运行时按调用者和请求身份派生，返回的 ready route 才能写入节点。图内 controller 扩展时继承当前图，不提供旧 task-scoped 模型入口。
