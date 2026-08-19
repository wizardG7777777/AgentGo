# AGENTS.md

本文件为在本仓库中工作的编码代理提供项目约束与入口指引。架构、机制与配置的细节描述在 `docs/agents-reference.md` 及下文索引的专文中，按需查阅，不要凭记忆假设。

## 项目

AgentGo 是一个 Go 多智能体任务编排系统：Scheduler（LLM 驱动的 `agent.Agent`）把请求分解为任务发到公告板，Runner 执行代理（`internal/runner`，按 kind 在 `setting.yaml` 声明）轮询认领并运行带工具调用的 ReAct 循环。v5 起为 Gate（动作前拦截）+ Reactor（状态变更后响应）反应式体系；V6 起以持久化 Graph 编排取代 Plan 运行时（`internal/plan` 已删），并引入 Execution Lease、Effect Journal、Task/Session Memory 与两轴模式。

语言：Go 1.25 ｜ 模块名：`agentgo` ｜ 配置：`setting.yaml`（YAML/JSON，仅 v4 schema） ｜ LLM：OpenAI-compatible Chat Completions，经 `internal/llm` 统一实现。

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

## 改代码前必知

- 接口驱动、无全局状态：依赖全部经 `runner.RunnerDeps` 或 `scheduler.New` 注入。
- 新增工具必须同步 `internal/tools/known_tools.go`；工具按 allowlist 剪枝，per-node 子集越界 fail-closed。
- Reactor 不得直接驱动状态迁移（无 `SetState`），只能发任务/消息让主循环自然迁移；用户 YAML Reactor 永远异步。
- Graph 当前采用**无 flow generation/correlation token 的单赋值安全基线**：所有非 barrier 节点最多一条静态入边；join / acceptance 的每个 `target_input` 也只能有一条生产边，并行 AND 必须使用不同端口。条件分支各自保留后续与 `end`，禁止共享端口 OR 或汇入同一普通节点；循环体可直接作为 root（隐式初始 activation + 唯一回边）。复杂 OR mux 等 generation/correlation token 落地后再开放；`join →` 下游只能匹配 `completed`。
- Acceptance 节点必须有非空 `task.title` 和写明逐项验收标准的非空 `task.description`。completed 业务结论只通过 `$.verdict` 精确 `eq` 路由，verdict 枚举为 `pass` / `fixable` / `failed`，completed 结果必须省略 `event`。acceptance 出边禁止无条件、`always`、`completed`、`pass`/`fixable` 事件条件；只允许 `$.verdict eq ...` 业务分支及 Runtime `failed` / `blocked` 兜底事件。证据或能力不足时提交 `status=blocked`；`disputed` 是 Runtime 核验状态，不是 verifier verdict。
- `submit_task_result` 提交即 finalizing（后续工具调用被 fence），同一任务唯一终态提交者；`status=blocked` 需同填 `blocked_reason`。自定义 Graph path 路由字段必须放在 `result` object（如 `result={"coverage":"gap"}`），不能只写 summary/event；结构化终态字段原子落盘失败时任务 fail-closed。
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
- `docs/activate/KNOWN_ISSUES.md` — 当前限制、验证缺口与可复现的开放问题
- `docs/tool-profiles.md` — 工具 profile / agent 声明 schema
- `docs/archived/` — 历史 RFC 与已完成的升级计划
