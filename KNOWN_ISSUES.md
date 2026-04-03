# 已发现缺陷

本文档记录 MVP 阶段已知的设计缺陷和未实现的功能，供调试和后续迭代参考。

## ~~代理空闲回收未实现~~ （已修复，简化 MVP）

Agent 结构体新增 `IdleThreshold` 字段，Run 方法中加入空闲计数器，连续空轮询（无任务、claim 失败、查询出错）达到阈值后自行退出。`IdleThreshold=0` 时禁用（向后兼容）。Config 新增 `agent_idle_threshold` 配置项。注意：架构要求的"系统代理数超过最低保留数量"条件未实现，留待后续迭代。

---

## ~~代理间无实时事件感知~~ （已修复，方案 C）

采用 per-task cancel context 方案替代广播模式。新增 `TaskCancelRegistry` 组件管理 taskID→CancelFunc 映射。代理 ClaimTask 成功后通过 registry 获取 per-task context 传入 processTask。看门狗/调度器调用 TransitionState 到 terminal 状态时，Store 内部自动调用 `Registry.Cancel(taskID)`，正在执行该任务的代理通过 `ctx.Done()` 立即感知。不修改 TaskStore 接口签名，registry 通过 setter 注入，nil 时无影响。

---

## 命令行参数覆盖配置未实现

**位置**: `internal/config/config.go` LoadConfig 方法

**现象**: 架构文档定义了"命令行参数优先级高于配置文件"（Archtechture.md 全局配置章节），但 MVP 只实现了 YAML 文件加载，未解析命令行参数。

**影响**: 无法在不修改配置文件的情况下临时覆盖配置值。

**建议修复**: 使用 `flag` 包或 `cobra` 解析命令行参数，在 LoadConfig 返回后覆盖对应字段。

---

## ~~代理 ReAct 循环未实现~~ （已修复）

已通过引入 `ExecuteResult` 结构体和 `processTask` 循环修复。循环上限触发 RetryRollback 并写入"因循环上限终止"标注。后续增强：executor 已支持接收 `[]HistoryEntry` 历史步骤。

---

## 启动流程不完整——调度器、调查代理、用户输入未启动

**位置**: `internal/bootstrap/bootstrap.go` Bootstrap / Start 方法

**现象**: 架构文档定义的 7 步启动流程（配置→公告板→花名册→看门狗→调查代理→调度器→用户输入），当前仅实现前 4 步。调查代理（Explorer）、调度器（Scheduler）和用户输入接口均未创建和启动。

**影响**: 系统启动后无法接收用户输入、无法发布和编排任务、无法执行只读调查任务，处于"基础设施就绪但无法工作"的状态。

**建议修复**: 按架构文档顺序依次实现调查代理启动（步骤 5）、调度器启动（步骤 6）、用户输入循环（步骤 7），每步完成后打印对应的中文提示。

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
