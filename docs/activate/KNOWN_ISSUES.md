# KNOWN_ISSUES — 当前限制与验证缺口

最后核对：2026-07-29。

本文件只记录当前仍会影响使用、开发或发布判断的限制。2026-07-18 UI Hub 改造的 41 项问题均已修复，原始核查与测试证据已归档至 [ui-hub-remediation-2026-07-18.md](../archived/ui-hub-remediation-2026-07-18.md)。2026-07-20 验收事实核验失败循环与级联取消事故已修复，证据归档至 [acceptance-fact-verification-and-cascade-incident-2026-07-20.md](../archived/acceptance-fact-verification-and-cascade-incident-2026-07-20.md)。2026-07-21 artifact 路径归一化缺陷导致的验收马拉松事故已修复，证据归档至 [artifact-path-normalization-incident-2026-07-21.md](../archived/artifact-path-normalization-incident-2026-07-21.md)。2026-07-21 验收空转（6 次 AcceptanceRun）与 Scheduler 篡改工作区事故已修复，证据归档至 [acceptance-spin-and-env-mutation-incident-2026-07-21.md](../archived/acceptance-spin-and-env-mutation-incident-2026-07-21.md)。2026-07-22 浪费可观测化专项落地：TUI 顶栏新增 session 级 token 总计（Hub 进程级累加器喂入，ad-hoc 团队销毁后消耗不隐形）；trace CLI 新增 stats 子命令（task/agent/plan 三维度 token 聚合 + 浪费口径 + 异常提示，见 TraceGuide §3.4）；watchdog 为 pending 级联取消补发 task_cancelled 事件（此前排队中的级联取消在 trace 中不可见）；修正 detectAnomalies 第 9 条从不命中的死检查（cancel_source 实际值为 dependency_failure）。2026-07-27 Windows ConPTY 长多行粘贴被固定 100ms Enter 防抖切成多条请求的问题已修复，状态机与回归证据归档至 [windows-tui-multiline-paste-incident-2026-07-27.md](../archived/windows-tui-multiline-paste-incident-2026-07-27.md)。2026-07-27 shell 旁路写入（`run_shell` 写文件不产生 `file_written` 事件）导致 artifact 账本缺失、`expected_artifacts` 校验假阴性的问题已修复：record-artifact Reactor 现订阅 `KindShellExecuted`，成功命令后对任务声明的 ExpectedArtifacts 做盘后补登（幂等、workspace 感知、回归测试见 `internal/reactor/builtin/record_artifact_test.go`）。2026-07-27 plan 楔死事故（验收目标含失败节点：验收 runner 认领闸要求 completed 而永远 pending、supersede 不改写 acceptance 节点依赖边被 digest 校验回滚、run 创建零预警）已修复：验收 runner（AcceptanceRunID 非空）依赖只需终态、supersede 改写全部剩余当前节点依赖边、EnsureAcceptanceRun 追加 `acceptance_target_incomplete` PlanWarning，回归见 `internal/plan/supersede_redirect_test.go`、`internal/store/claim_acceptance_runner_test.go`、`internal/bootstrap/supersede_wedge_test.go`。2026-07-28 验收证据类型混挂（verifier 把 command 佐证证据挂到 file_hash criterion，连续三次真实运行复现）已修复：类型化 criterion 的证据纯度违例消息改为指明违例证据 ID 与修正指引，verifier 指引文本补 typed-criterion purity 规则，回归见 `internal/plan/acceptance_evidence_purity_test.go`。2026-07-28 Agent 工作台跨轮输出被最新一轮覆盖的问题已修复：Scheduler 与普通 Agent 现共用不可变完成轮次事件和 Session `turns.jsonl` 账本，TUI/Web 可恢复并浏览全部轮次；证据归档至 [agent-turn-history-loss-incident-2026-07-28.md](../archived/agent-turn-history-loss-incident-2026-07-28.md)。2026-07-29 验收 verifier 的 command 证据在 Windows 上误读 UTF-8 中文文件（PowerShell 对无 BOM 文件按系统 ANSI/GBK 解码，子串断言全部误 fail；行为评测全量首跑 long-form-write 连续两轮验收因此翻车，verifier 自述「文档实际含全部标题」仍被迫判 fail）已修复：内置 verifier 指引补充「内容断言优先 read_file/file_hash 证据；必须 command 证据时显式带 -Encoding UTF8」（`internal/agenttemplate/prompts/verifier.md`）。2026-07-29 expected-artifacts 校验「账本失忆」空转（重试/替代任务换新任务 ID 后 artifact 账本为空，前次尝试写好的文件明明在盘上却连撞提交拒绝，eval smoke 实测 worker 空转 4 轮 + 一次重试回滚）已修复：校验增加磁盘兜底——账本缺失的预期项经 `agent.NewArtifactPhysicalResolver` 解析后 stat，盘上存在（非目录）即转入 `ArtifactCheckResult.Recovered` 视为满足；resolver 为 nil 时退化为纯账本比对，装配点见 `internal/runner/runner.go` 与 `internal/runner/dependency_map.go`。截至本次核对，没有把已修复条目重新列为开放缺陷。

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

### Memory 的 Project 作用域与向量查询仍未实现

`internal/memory` 的 `ProcessStore`（进程内）与 `SessionStore`（`sess-<id>/memory.jsonl` 持久化，MM8）可用；`ScopeProject` 仍返回 `ErrScopeUnsupported`，`QueryByVector` 明确返回 `ErrNotImplemented`。`KindLearning` / `KindPattern` / `KindConstraint` 尚无生产写入方。

- 影响：跨 session 的长期知识（Project 级）与语义检索不可用；学习类记忆目前不存在。
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

残余风险（有意接受）：`run_shell` 的副作用不受同等隔离——隔离任务的 shell 默认工作目录切到 workspace 根，但命令写主根**绝对路径**不可完全阻止（重定向写文件检测仍以 projectRoot 为边界）；verifier 模板不授文件写工具，但 `run_shell` 不是只读沙箱（见 [AgentTemplate.md](AgentTemplate.md) §6），"不修改被验收对象"由 prompt 与 Shell 命令策略保障，不是 OS 级强隔离。非隔离任务之间仍只有 Roster 文件级互斥。

- 影响：并发 Agent 可通过 shell 互相踩踏工作区；验收 runner 理论上能污染被验收对象。
- 处置：高风险操作用 exec=strict 全量审批或灰名单 Interaction 把守；fan-out 并行写同一批文件的 DAG 节点应声明 `isolation: "workspace"`；容器级隔离评估后再立项。

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

### 缺少 Agent 行为级评测

现有测试证明控制面不变量（状态机、验收事实核验、级联取消），但没有跨模型/Prompt 的固定任务行为基线：成功率、token、重规划次数、验收轮数、无进展暂停次数均无自动采集。单元测试全绿不等于真实 LLM 行为达标。

- 影响：prompt 修改、模型更换或工具描述调整后，行为回归只能靠人工复测发现（2026-07-23 重读浪费复测即为例证）。
- 处置：重要 prompt 变更后用 `trace stats` 抽查；行为 eval 框架（固定任务集 + 指标采集）待立项。

## 维护规则

- 新的、可复现且尚未修复的问题在这里记录影响、复现/证据、临时处置和归属。
- 修复后从本文件移除；若修复过程值得保留，迁移到 `docs/archived/` 并在提交中链接测试。
- 不把设计愿望、已完成任务或推测性风险伪装成当前 bug。
