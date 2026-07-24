# KNOWN_ISSUES — 当前限制与验证缺口

最后核对：2026-07-22。

本文件只记录当前仍会影响使用、开发或发布判断的限制。2026-07-18 UI Hub 改造的 41 项问题均已修复，原始核查与测试证据已归档至 [ui-hub-remediation-2026-07-18.md](../archived/ui-hub-remediation-2026-07-18.md)。2026-07-20 验收事实核验失败循环与级联取消事故已修复，证据归档至 [acceptance-fact-verification-and-cascade-incident-2026-07-20.md](../archived/acceptance-fact-verification-and-cascade-incident-2026-07-20.md)。2026-07-21 artifact 路径归一化缺陷导致的验收马拉松事故已修复，证据归档至 [artifact-path-normalization-incident-2026-07-21.md](../archived/artifact-path-normalization-incident-2026-07-21.md)。2026-07-21 验收空转（6 次 AcceptanceRun）与 Scheduler 篡改工作区事故已修复，证据归档至 [acceptance-spin-and-env-mutation-incident-2026-07-21.md](../archived/acceptance-spin-and-env-mutation-incident-2026-07-21.md)。2026-07-22 浪费可观测化专项落地：TUI 顶栏新增 session 级 token 总计（Hub 进程级累加器喂入，ad-hoc 团队销毁后消耗不隐形）；trace CLI 新增 stats 子命令（task/agent/plan 三维度 token 聚合 + 浪费口径 + 异常提示，见 TraceGuide §3.4）；watchdog 为 pending 级联取消补发 task_cancelled 事件（此前排队中的级联取消在 trace 中不可见）；修正 detectAnomalies 第 9 条从不命中的死检查（cancel_source 实际值为 dependency_failure）。截至本次核对，没有把已修复条目重新列为开放缺陷。

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

### Memory 目前只支持进程内存储

`internal/memory` 的 `ProcessStore` 可用，但 Session/Project 持久化后端尚未实现；`QueryByVector` 明确返回 `ErrNotImplemented`。

- 影响：重启后不能依赖 Memory 保留长期知识，也不能使用向量查询。
- 路线图：[MemoryManageSystem.md](MemoryManageSystem.md)。

### Shell 超时处理仍是固定超时

当前 Shell 拦截已经使用通用 `shell_command` authorization Interaction：黑名单硬拒绝，灰名单回答与原始 command/pattern/working directory/Agent/Task 精确绑定，并在 Interaction 不可用或绑定不一致时 fail closed。但仍没有 `shell_commands.yaml` 持久化规则、`ShellCommandGate` 重构或可插拔 `TimeoutHandler`。`shell_timeout_pending` 与 `shell_timeout_resolved` 是被拒绝订阅的 reserved event kind，不会发射。

- 影响：不要在用户 Reactor 中订阅这两个 kind，也不要假定 Shell 超时可以交互式续时或截断。
- 当前用户选择契约：[Interaction 设计](../design/interaction.md)。`ToolUpgradePlan.md` 的旧四键提示与名单写回方案属于已废弃历史，不是当前契约。

### 用户 Reactor 的 prompt/lifecycle 有意受限

用户 Reactor 目前只支持文件型 prompt；`prompt.url`、`prompt.inline` 会在加载时报错。`spawn_agent` 只支持 `one_shot` lifecycle。

- 影响：配置这些预留字段会导致启动失败，不能视为兼容的未来配置。
- 处置：使用 `prompt.file` 和 `one_shot`；其他形态需先实现并补测试。

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

## 维护规则

- 新的、可复现且尚未修复的问题在这里记录影响、复现/证据、临时处置和归属。
- 修复后从本文件移除；若修复过程值得保留，迁移到 `docs/archived/` 并在提交中链接测试。
- 不把设计愿望、已完成任务或推测性风险伪装成当前 bug。
