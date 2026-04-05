# 已发现缺陷

本文档记录 MVP 阶段已知的设计缺陷和未实现的功能，供调试和后续迭代参考。

## ~~代理空闲回收未实现~~ （已修复，简化 MVP）

Agent 结构体新增 `IdleThreshold` 字段，Run 方法中加入空闲计数器，连续空轮询（无任务、claim 失败、查询出错）达到阈值后自行退出。`IdleThreshold=0` 时禁用（向后兼容）。Config 新增 `agent_idle_threshold` 配置项。注意：架构要求的"系统代理数超过最低保留数量"条件未实现，留待后续迭代。

---

## ~~代理间无实时事件感知~~ （已修复，方案 C）

采用 per-task cancel context 方案替代广播模式。新增 `TaskCancelRegistry` 组件管理 taskID→CancelFunc 映射。代理 ClaimTask 成功后通过 registry 获取 per-task context 传入 processTask。看门狗/调度器调用 TransitionState 到 terminal 状态时，Store 内部自动调用 `Registry.Cancel(taskID)`，正在执行该任务的代理通过 `ctx.Done()` 立即感知。不修改 TaskStore 接口签名，registry 通过 setter 注入，nil 时无影响。

---

## LLM 上下文无截断机制，复杂任务可能触发截断死循环

**位置**: `internal/agent/agent.go` `processTask` / `internal/llm/client.go` `FinishReasonLength` 处理

**现象**: `processTask` 的 ReAct 循环中，每轮工具调用都会向 `history []HistoryEntry` 追加一条记录，历史随循环轮次线性增长，当前无任何截断或窗口限制。对于迭代轮次较多的复杂任务，消息序列可能超过模型的 token 上限。

更严重的是，当前对 `FinishReasonLength` 的处理会形成**死循环**：

```
上下文过长
  → finish_reason=length (client.go:154)
  → ErrBadResponse → ErrRecoverable
  → handleFailure() → saveHistory + RetryRollback
  → 重试时携带相同的过长历史（LastHistory 未截断）
  → 再次触发 finish_reason=length
  → 无限重试，直到 MaxRetries 耗尽
```

**影响**: MVP 阶段测试任务规模较小，不会触发此问题。在以下场景中会出现：
- 任务需要大量文件读取，工具结果体积大
- 多次重试后历史跨重试累积（每次重试在上一次历史基础上继续追加）
- 使用 token 上限较小的模型（< 32K）

**建议修复（优先级从低到高）**:

1. **滑动窗口**（最轻量）：`buildMessages()` 中仅保留最后 N 条 HistoryEntry，超出部分丢弃。会丢失早期上下文，但消除死循环风险
2. **历史摘要**：每 K 轮自动调用 LLM 生成"执行摘要"消息替代详细历史，保留语义但压缩体积
3. **token 计数预检**：追加历史前估算 token 数，超过阈值时提前触发摘要或丢弃最旧条目

---

## 多 Agent 并发写文件存在 TOCTOU 竞争问题

**位置**: `internal/worker/worker.go` `makeWriteFileTool` 函数

**现象**: Roster 的文件锁（`TryClaim` / `Release`）仅覆盖 `os.WriteFile` 调用本身，无法保护读取到写入之间的时间窗口。当多个 Worker 并发执行时，以下序列会导致一个 Agent 的修改被另一个完全覆盖：

```
Agent-A: read_file("foo.go")              ← 无锁读取，获得内容 v1
          ... LLM 推理（数秒）...
Agent-B: read_file("foo.go")              ← 同样读取 v1
Agent-A: write_file("foo.go", contentA)   ← 获锁，基于 v1 写入 v2，释放锁
Agent-B: write_file("foo.go", contentB)   ← 获锁，基于过期的 v1 写入 v3，Agent-A 的修改丢失
```

**影响**: MVP 阶段只有 1 个 Worker，单进程串行执行规避了此问题。一旦激活多 Worker（`worker_count > 1`），并发修改同一文件的任务将产生不可预测的数据丢失，且 LLM 不会感知到冲突。

**根因**: 这是经典的 TOCTOU（Time-of-Check-Time-of-Use）问题。LLM 的"读取—推理—写入"三阶段天然存在时间间隔，Roster 的锁粒度不足以覆盖完整的读写事务。

**建议修复（乐观并发）**:

1. `read_file` 工具在返回文件内容的同时，附带当前内容的 SHA256 哈希（`content_hash`）
2. `write_file` 工具新增可选参数 `expected_hash`；写入前在锁内重新计算文件哈希，若与 `expected_hash` 不一致，拒绝写入并返回明确的冲突提示
3. LLM 收到冲突提示后，重新调用 `read_file` 获取最新内容和新哈希，再重新生成写入内容

此方案无需修改 Roster 接口，架构改动最小，且与 git 的冲突检测逻辑一致。`expected_hash` 为空时退化为当前行为，向后兼容。

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
