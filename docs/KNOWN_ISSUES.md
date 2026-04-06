# 已发现缺陷

本文档记录 MVP 阶段已知的设计缺陷和未实现的功能，供调试和后续迭代参考。

## ~~代理空闲回收未实现~~ （已修复，简化 MVP）

Agent 结构体新增 `IdleThreshold` 字段，Run 方法中加入空闲计数器，连续空轮询（无任务、claim 失败、查询出错）达到阈值后自行退出。`IdleThreshold=0` 时禁用（向后兼容）。Config 新增 `agent_idle_threshold` 配置项。注意：架构要求的"系统代理数超过最低保留数量"条件未实现，留待后续迭代。

---

## ~~代理间无实时事件感知~~ （已修复，方案 C）

采用 per-task cancel context 方案替代广播模式。新增 `TaskCancelRegistry` 组件管理 taskID→CancelFunc 映射。代理 ClaimTask 成功后通过 registry 获取 per-task context 传入 processTask。看门狗/调度器调用 TransitionState 到 terminal 状态时，Store 内部自动调用 `Registry.Cancel(taskID)`，正在执行该任务的代理通过 `ctx.Done()` 立即感知。不修改 TaskStore 接口签名，registry 通过 setter 注入，nil 时无影响。

---

## ~~LLM 上下文无截断机制，复杂任务可能触发截断死循环~~ （已修复）

已通过 3 层压缩策略修复：
- Layer 1（`snipOldToolResults`）：每轮自动清空旧工具输出，无 LLM 调用开销
- Layer 2（`compressHistory`）：`totalPromptTokens > CompactTokenThreshold`（默认 80000）时生成摘要 + 保留最近 N 条
- Layer 3：`handleFailure` 检测 context overflow 时以 `keepRecent=1` 激进压缩后 RetryRollback，消除死循环

---

## ~~多 Agent 并发写文件存在 TOCTOU 竞争问题~~ （已修复，双层防护）

已通过两层机制修复：
1. **乐观并发控制**：`read_file` 返回 `content_hash: SHA256`，`write_file`/`edit_file` 支持 `expected_hash` 参数，写入前在 Roster 锁内校验哈希，不一致时返回冲突错误（"冲突"）
2. **Git Worktree 物理隔离**（`cfg.WorktreeEnabled`）：每个 Worker 在独立 worktree 分支上工作，任务完成后自动 commit+merge，冲突由 ConflictResolver（LLM 驱动）自动解决

---

## ~~命令行参数覆盖配置未实现~~ （已部分修复）

`main.go` 已支持 `-config` flag 指定配置文件路径。单字段级别的命令行覆盖（如 `-worker_count=3`）暂未实现，但可通过不同配置文件切换。

---

## 端到端测试覆盖缺口（命令权限分层与拦截链路）— 本轮不实施

**位置**:
- `internal/shell/intercept.go` (`CommandFilter` / `WrapShellTool`)
- `internal/worker/worker.go` (`run_shell` 工具注册与包装接线)
- `internal/cli/cli.go` (`handleApproval`)
- `internal/bootstrap/bootstrap.go`（系统组件接线）

**现象**:
- 当前单元测试已覆盖大量局部逻辑（黑灰名单匹配、CLI 审批交互、若干变体基线）。
- 但缺少真实链路的端到端验证：`worker run_shell -> WrapShellTool -> approvalCh -> CLI 回复 -> 命令执行/拒绝`。

**本轮不实施的原因**:
- MVP 阶段优先验证多代理协作的功能正确性，E2E 测试属于质量保障而非功能交付
- 单元测试已覆盖各模块的核心逻辑，E2E 集成风险在 MVP 单 Worker 规模下可控
- E2E 测试需要模拟完整的 Bootstrap → Worker → CLI 交互链路，编写和维护成本较高
- 已记录到 `nextUpgrade_v2.md` 作为后续质量工程任务

**后续迭代建议的 E2E 用例**:
1. 安全命令直通：不触发审批，命令直接执行成功
2. 灰名单批准/拒绝/指导：CLI 回复 y/n/文本 的三种路径
3. 黑名单拦截：命令直接拒绝，不进入审批通道
4. 审批阶段取消：context cancel 时请求收敛、无 goroutine 泄漏
5. 双 Worker 并发审批：回复不串单
6. 已知误报基线：`reboot.conf` 等作为行为快照

---

## ~~代理 ReAct 循环未实现~~ （已修复）

已通过引入 `ExecuteResult` 结构体和 `processTask` 循环修复。循环上限触发 RetryRollback 并写入"因循环上限终止"标注。后续增强：executor 已支持接收 `[]HistoryEntry` 历史步骤。

---

## ~~启动流程不完整——调度器、调查代理、用户输入未启动~~ （已修复）

`bootstrap.Bootstrap` 已实现完整启动流程：配置 → 公告板 → 花名册 → 邮箱注册表 → LLM 客户端 → 调度器 → 看门狗 → Worktree 管理器 → 冲突解决代理 → 调查代理 → 命令审批通道 → 执行代理(×N) → 邮差通知器 → CLI。`Start` 方法启动所有 goroutine，`RunCLI` 阻塞主线程。

---

## ~~看门狗缺少花名册兜底清理职责~~ （已修复）

Watchdog 结构体已添加 `Roster` 字段，`inspect` 方法末尾调用 `cleanupStaleClaims`，通过 `Roster.ListAllAgents()` 获取所有持有声明的代理，与 processing 任务中的活跃代理对比，清理残留声明。Roster 接口新增 `ListAllAgents()` 方法。

---

## ~~配置加载不支持 JSON 格式~~ （已修复）

`LoadConfig` 已根据文件扩展名判断格式：`.json` 使用 `encoding/json`，其他使用 `gopkg.in/yaml.v3`。Config 结构体已添加 `json` tag。

---

## ~~看门狗重启循环缺少延迟控制~~ （已修复）

`runWatchdogWithRecover` 循环体末尾添加了 1 秒延迟和 `ctx.Done()` 检查，防止 panic 恢复后热循环和 ctx 取消后空转。

---

## ~~启动完成提示信息不完整~~ （已修复）

提示已修改为 `[启动] 系统就绪，等待用户输入`。

---

## 总览

| 缺陷 | 状态 |
|------|------|
| 代理空闲回收 | ✅ 已修复 |
| 代理间无实时事件感知 | ✅ 已修复 |
| LLM 上下文截断死循环 | ✅ 已修复 |
| 多 Agent 并发写文件 TOCTOU | ✅ 已修复 |
| 命令行参数覆盖配置 | ✅ 已部分修复 |
| 代理 ReAct 循环未实现 | ✅ 已修复 |
| 启动流程不完整 | ✅ 已修复 |
| 看门狗花名册兜底清理 | ✅ 已修复 |
| 配置加载不支持 JSON | ✅ 已修复 |
| 看门狗重启循环延迟控制 | ✅ 已修复 |
| 启动完成提示信息 | ✅ 已修复 |
| Shell 拦截 E2E 测试缺口 | ⏳ 本轮不实施（见 nextUpgrade_v2.md） |

**11/12 项已修复，1 项为质量保障任务留待后续迭代。**
