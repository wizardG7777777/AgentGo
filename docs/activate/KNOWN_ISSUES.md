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

## ~~多 Agent 并发写文件存在 TOCTOU 竞争问题~~ （已部分修复，物理隔离层于 2026-04-09 移除）

**乐观并发控制**：`read_file` 返回 `content_hash: SHA256`，`write_file`/`edit_file` 支持 `expected_hash` 参数，写入前在 Roster 锁内校验哈希，不一致时返回冲突错误（"冲突"）。

> **当前状态**：原本"双层防护"的第二层（git worktree 物理隔离）已于 2026-04-09 整体删除，详见下方"架构决策：删除 git 依赖"段。当前防线只剩乐观并发控制 + Roster 文件锁 + `pathutil.ValidatePath`。"两个 worker 并发写同一文件 → 后写覆盖前写"是被故意暴露的退化之一，等待"多代理协同重建"阶段按真实失败模式驱动设计。

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

`bootstrap.Bootstrap` 已实现完整启动流程：配置 → trace → 公告板 → 花名册 → 邮箱注册表 → LLM 客户端 → 调度器 → 看门狗 → 调查代理 → 命令审批通道 → 执行代理(×N) → 邮差通知器 → CLI。`Start` 方法启动所有 goroutine，`RunCLI` 阻塞主线程。详见 `Archtechture.md` § 系统启动流程。

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

## Scheduler 提前 report_done 导致任务结果丢失（已临时缓解）

**位置**: `internal/scheduler/scheduler.go`（`handleEvent`、`toolReportDone`、`schedulerSystemPrompt`）

**现象**: Scheduler 在发布任务后，LLM 在任务仍为 pending/processing 状态时就调用 `report_done`（例如回复"任务已发布，请稍候"后直接 report_done）。`report_done` 清空 `currentBatch`，导致后续 `task_completed` 事件到达时 `batchComplete()` 返回 false（batch 为空），不再触发 reactLoop。任务结果无法汇报给用户。

**根因**: Scheduler 的 reactLoop 是"发布任务 → LLM 决定下一步"的单次循环。LLM 如果在同一轮 reactLoop 中发布任务又调用 report_done，batch 就被提前清空。这是 Scheduler 架构的内在缺陷——它依赖 LLM 正确判断"何时该等待、何时该汇报"，但 LLM 不一定遵从 prompt 约束。

**当前缓解措施**:
1. **Prompt 层（软性）**: system prompt 新增"如果任务仍为 pending/processing，绝对不要调用 report_done"
2. **代码层（硬性）**: `toolReportDone` 执行前扫描 `currentBatch`，存在未到终态的任务时拒绝执行并返回错误消息

**局限性**: 硬性拦截可以防止 batch 被错误清空，但 LLM 可能在被拒绝后进入无效循环（反复尝试 report_done 被反复拒绝），直到 `SchedulerMaxLoops` 耗尽。此时 batch 仍在，后续 `task_completed` 事件可正常触发 reactLoop，但用户体验不佳（有延迟且可能看到无意义的中间输出）。

**根治方向**:
- 将 Scheduler 从"单次 reactLoop 发布+汇报"改为"发布后挂起，事件驱动唤醒后再汇报"的两阶段模式
- 或在 `reactLoop` 中检测到"已发布任务但均未到终态"时，主动退出循环并保留 batch，等事件驱动回来
- 详见 `nextUpgrade_v2.md` 后续架构改进

---

## ~~日志审计颗粒度不足——代理内部工具调用不可见~~ （已修复）

已在 `llm_executor.go` 并行执行 goroutine 内添加结构化工具调用日志，格式：

```
[agent worker-1] task=<id> loop=3 tool=read_file args={"path":"internal/foo.go"}
[agent worker-1] task=<id> loop=3 tool=read_file duration=12ms result_len=2048
```

实现细节：
- `agent.go` 的 `processTask` 循环中通过 `WithAgentContext(ctx, a.ID, i)` 将 agentID 和 loop 轮次注入 context
- `llm_executor.go` goroutine 内从 context 提取后随工具调用前后各记录一条日志
- 参数截断为 120 字符防止日志过长（`truncateForLog`）
- 记录内容：agentID、taskID、loop 轮次、工具名、截断参数、耗时（毫秒精度）、结果长度或错误信息

---

## ~~Worker 凭空捏造任务结果（无依赖、无 read_file）~~ （已修复，Level 3 全量方案）

**修复时间**：2026-04-08（在 Level 1+2+3 方案中一并落地）

**修复内容**：

1. **Task 数据模型扩展**：`model.Task` 新增 `Artifacts []string`（实际写入的文件，自动去重）和 `ExpectedArtifacts []string`（发布者声明的预期产出，硬性合约）

2. **Store 接口扩展**：新增 `AppendArtifact(taskID, path)` 和 `GetDependencyArtifacts(taskID)` 两个方法。`MemoryTaskStore` 实现了带去重的 append 和按依赖分组的查询。8 个新单元测试覆盖。

3. **LocalWriteGroup 自动记录**：`write_file`/`edit_file` 成功后自动调用 `Store.AppendArtifact`，路径经 `normalizeArtifactPath` 标准化为相对项目根的相对路径。3 个新单元测试覆盖标准化和去重。

4. **下游 prompt 自动注入**：`agent.processTask` 在启动任务前调用 `Store.GetDependencyArtifacts`，把每个上游任务的产出文件路径列表追加到 `depResults` 对应条目，由 `buildMessages` 注入到 user prompt 的"前置任务结果"段。下游 worker 会看到：
   ```
   【该任务实际写入的文件】
     - docs/output/foo.md
     - docs/output/bar.md
   （你必须 read_file 这些文件来获取一手数据，不要仅凭上面的总结文本就凭空生成下游产出）
   ```

5. **read_file 自描述头部**：`read_file` 的输出现在以 `[file] <path> (lines X-Y of N)\n[hash] <sha256>\n---\n<content>` 格式返回，让 LLM 在历史压缩后仍知道自己读了什么。

6. **Scheduler prompt 改造**：`schedulerSystemPrompt` 加入硬性指引——"任务 B 需要使用任务 A 的产出时必须传 dependencies"，含正反例。

7. **Worker prompt 红线**：`worker.systemPrompt` 加入"先读后写"红线——任务要求"整合/汇总/总结/分析"已存在材料时，**第一步必须**是 `list_dir` 或 `read_file`，禁止凭空 `write_file`。

**验证**：trace 系统的 `agentgo trace show <task_id>` 异常检测器仍会捕获"调用 write_file 但全程未调用 read_file"模式，作为运行时 sanity check。

**残留风险**：本次主要是 prompt + 数据流改造，运行时仍依赖 LLM 配合。但 ExpectedArtifacts 校验提供了硬性兜底——任务声称完成但缺少预期产出会被 `agent.processTask` 主动失败重试。

---

## ~~Worker 凭空捏造任务结果原始记录（保留供历史参考）~~

**位置**：`internal/scheduler/scheduler.go`（任务发布逻辑）+ `internal/worker/worker.go`（systemPrompt）

**严重程度**：🔴 P0（数据正确性风险）

**现象**：2026-04-08 系统测试中，调度器发布了第三个任务"整合前两个任务的总结报告"，由 worker-3 执行。worker-3 全程只有 2 个 tool call：

```
loop=0 tool=write_file (路径错被拒)
loop=1 tool=list_dir   (探查 docs/)
loop=2 tool=write_file (写入最终报告，成功)
```

worker-3 **从未调用 read_file**，没有读取前两个任务写入的报告文件，也没有依赖结果注入。但它生成的"文档库全景报告"包含了大量看似精确的内容（如 `ISSUE-005`、`1 人覆盖 8 项需求`、`🔴 高风险 3 项 / 🟡 中风险 4 项 / 🟢 低风险 2 项`），**这些内容在原始 docs 中并不存在**——纯 LLM 自由发挥。

**双重根因**：

1. **Scheduler 发布任务时未声明依赖**。MetaGroup 的 `publish_task` 工具支持 `dependencies` 参数，但 `schedulerSystemPrompt` 没有指导 LLM 在拆解任务时声明依赖关系。结果：`agent.processTask` 调用的 `Store.GetDependencyResults(taskID)` 拿到空 map，worker-3 没有任何上游上下文。

2. **Worker system prompt 缺少"先读后写"硬约束**。Worker 看到任务描述里的"整合前两个任务"时，理论上应该先 `list_dir` + `read_file` 找源材料。但当前 prompt 没有禁止"未读任何源材料就直接 write_file 总结报告"的行为。

**失败模式的隐蔽性**：worker-3 整个任务在系统层面看起来"成功"——write_file 返回正常、worktree 合并成功、scheduler 标记任务完成。**只有把生成的文件内容和原始 docs 一行行对比**才能发现是假的。

**修复方向**：

P0a — Scheduler prompt 改造：
- 在 `schedulerSystemPrompt` 中加入"任务拆分时，若任务 B 需要依赖任务 A 的产出，必须在 publish_task 调用中显式声明 dependencies=[A.id]"
- 给出一个反例和正例

P0b — Worker prompt 加硬约束：
- 在 `worker.systemPrompt` "调查与研究类任务的额外约束"段加入：
  > "若任务要求'整合/汇总/总结/分析'已存在的材料（文档、前序任务结果、上游产出），第一步**必须**是 read_file 或 list_dir 探查源材料。禁止在没有读取任何源材料的情况下直接 write_file 生成总结报告。这是数据正确性的红线。"

P0c — `GetDependencyResults` 注入更完整的上下文：
- 当前只返回依赖任务的 `SubmitResult` 文本（LLM 生成的最终输出）
- 应当同时附带依赖任务在 worktree 内**写入的文件路径列表**，供下游任务直接 read_file
- 否则下游只能拿到二手的总结，看不到一手数据

**状态**：⏳ 待实现

---

## ~~Worker 任务完成但无文件产出（report-only 失败模式）~~ （已修复，与"凭空捏造"同一轮）

**修复时间**：2026-04-08

**修复内容**：

1. **`Task.ExpectedArtifacts` 字段**：发布者声明的"本任务必须产出的文件路径"清单
2. **publish_task 工具新增 `expected_artifacts` 参数**（Scheduler 和 MetaGroup 双端实现）
3. **`agent.processTask` 任务结束前校验**：调用 `checkExpectedArtifacts(store, taskID)`，缺失任何一个预期文件就 `handleFailure` 触发重试，并在错误消息中明确告知缺失了哪些文件
4. **Scheduler prompt 引导**："如果任务的产出是报告/总结/文档，必须填写 expected_artifacts"
5. **Worker prompt 落盘契约**："任务要求产出持久化产物时必须使用 write_file 落盘，不要只在文本响应里返回总结"

5 个新单元测试覆盖 `checkExpectedArtifacts` 的各种场景：全部存在、部分缺失、全部缺失、无声明跳过、任务读取失败时跳过。

**残留风险**：低。Scheduler 仍可能漏填 expected_artifacts（软约束），但只要填写了，硬性校验保证一定落盘。

---

## ~~Worker 任务完成但无文件产出原始记录（保留供历史参考）~~

**位置**：`internal/scheduler/scheduler.go` 任务描述生成 + `internal/worker/worker.go` systemPrompt

**严重程度**：🟡 P1

**现象**：2026-04-08 系统测试中，task 4a0eb048 任务描述为"汇总成一份结构化的进行中文档总结报告"。worker-2 跑了 13 个 loop，read_file 读了 4 份 activate 文档，但**从头到尾没有调用 write_file**。最后一次操作是 `wc -l` 数行数，然后任务结束。

```
04:18:51 [worktree] 任务 4a0eb048 无文件变更，跳过合并
```

任务被标记为 completed，SubmitResult 里只有 LLM 在文本响应里生成的总结。同一批次中 worker-1 处理的同样措辞的另一个任务却**写了文件**（已归档文档总结报告）。

**根因**：LLM 行为不一致。"汇总成报告"这个措辞既可以理解为"在文本输出里写一段总结"，也可以理解为"在文件系统里写一个 .md 文件"。两个 worker 实例对同一句指令有不同的解读。

**连锁影响**：直接放大了 P0 问题 2——下游 worker-3 想"整合"这份总结时，依赖任务的 SubmitResult 里只有文本，没有文件落盘路径，于是 worker-3 没东西可读，索性自己编。

**修复方向**：
- Scheduler 拆分任务时，在 description 里**显式写明产出要求**："在 `docs/活动文档总结.md` 写入一份 markdown 报告"，而不是模糊的"汇总成报告"
- Worker prompt 加规范："如果任务要求产出'报告'/'总结'/'文档'，必须使用 write_file 落盘到 docs/ 下，不要只在文本响应里返回"
- 任务终态判定可考虑加一个可选 hook：`expected_artifacts: ["docs/foo.md"]`，任务结束时检查文件是否真的存在

**状态**：⏳ 待实现

---

## Scheduler 对任务完成事件反应延迟（约 3 分钟）

**位置**：`internal/scheduler/scheduler.go`（reactLoop 退出 + 事件唤醒路径）

**严重程度**：🟡 P1

**现象**：2026-04-08 系统测试时间线：

```
04:18:51  task 4a0eb048 完成（worker-2 无文件变更跳过合并）
04:19:05  task 321b561d 完成（worker-1 合并成功）
04:21:53  scheduler loop=1 触发，发布新任务 84da843f      ← 距离前两任务完成 2分48秒
04:22:44  scheduler loop=2 处理 task_completed 事件
04:22:47  worker-3 开始执行
```

**异常**：从两个任务完成到 scheduler 发布下一轮任务，间隔 **2 分 48 秒**。配置 `scheduler_ticker_sec: 10`，正常应该最多 10 秒后由 ticker 触发；事件驱动应该几乎即时。

**疑似关联**：与本文档下方"Scheduler 提前 report_done 导致任务结果丢失"是同一架构缺陷的不同表现。Scheduler 的 reactLoop 退出后，事件驱动唤醒机制不可靠。

**待排查**：
- 04:22:44 那两个 `task_completed` 事件的时间戳是事件被消费的时间还是事件发布的时间？
- eventCh 缓冲区是否积压？
- scheduler 在 04:21:53 由谁唤醒（ticker 还是其他事件）？

**状态**：⏳ 需进一步定位，与 Scheduler 提前 report_done 一并排查

---

## 邮件级联爆炸：自动唤醒 + 强制回复义务导致无限循环（2026-04-08 系统测试发现）

**位置**：`internal/mailbox/notifier.go`（mail-notifier 自动唤醒机制）+ `internal/worker/worker.go` 的 systemPrompt（强制回复约束）+ `internal/explorer/explorer.go` systemPrompt

**严重程度**：🔴 **P0 架构级缺陷**——可被任意一条 `to=*` 广播 + `msg_type=question` 触发，让整个系统进入无限循环直到代理空闲超时或 token 预算耗尽

**最小复现路径**：
1. 用户问"请确认日志系统以及多代理调用是否启动"（任何会让 worker 想"测试通信"的请求）
2. Scheduler 把这个理解为任务，发布 "测试代理间通信" 子任务
3. Worker 拿到任务后用 `send_message` **群发** `msg_type=question` 给所有代理（4 条独立消息）
4. mail-notifier 检测到每个代理"有未读邮件"，**对每个未读邮件单独发布一个"唤醒任务"**
5. 被唤醒的代理打开收件箱，看到 question 类消息，根据 system prompt 约束"收到 question 必须回复"，发出 reply
6. reply 又是新邮件 → mail-notifier 又唤醒原发送方 → 原发送方又回复 → ...

**实测数据**（2026-04-08 17:30-17:34，4 分钟内）：

```
17:31:50  worker-2 send_message → scheduler (1 message)
17:31:57  worker-2 send_message → worker-3, explorer-1, worker-1 (3 more)
17:32:01  mail-notifier 唤醒 worker-3 + explorer-1
17:32:05  各自 reply → 又产生 2 条新邮件
17:32:11  worker-2 累计未读=6，mail-notifier 连续 4 次单独唤醒 worker-2
          （任务 537f79d9 / b7b1a004 / d71e2dcd / e301365d）
17:32:34  worker-2 又广播 to=* "系统检查完成" → 又唤醒所有人
... 4 分钟内累计派生 16+ 派生任务
17:34:04  最终因任务超时 / 空闲触发停止
```

最荒谬的细节：被唤醒的 worker-1（任务 0abf9ff3）甚至开始 grep 项目代码找 "message" 相关文件，因为它收到了"请查看收件箱并采取行动"的指令但**不知道自己被叫醒的真正原因**——它在试图"理解自己的存在意义"。

**4 个叠加的根因**（必须系统性解决）：

### 根因 1：mail-notifier 无去重
看 17:32:11-17:32:26 这段：worker-2 累积 6 封未读邮件，mail-notifier **连续发布了 4 个独立的唤醒任务**给 worker-2。每个唤醒任务都说"请查看未读邮件"，但 worker-2 一次就能消化所有未读，于是产生 3 个浪费的任务。

**应当修复方向**：每个 agentID 同时最多只允许一个"未读邮件"唤醒任务在 pending/processing 状态。新邮件到达时，如果该代理已有 pending 唤醒任务，**合并到现有任务**而不是创建新任务。

### 根因 2：邮件链无环路检测 / 跳数限制
A 收到邮件 → A 回复 → B 收到邮件 → B 回复 → A 收到邮件 → ... 永远不停。

**应当修复方向**：每条 mailbox.Message 携带一个 `chain_depth` 字段。
- 用户通过 `/steer` 投递的初始邮件 `chain_depth=0`
- worker 通过 send_message 触发的邮件继承"自己当前任务的最深 chain_depth + 1"
- 超过阈值（建议 `mail_chain_max_depth: 3`）的邮件**仍然投递到收件箱**（保留可见性），但 mail-notifier **不再为它发布唤醒任务**（断开自动响应链）

### 根因 3：唤醒任务不携带原始上下文
被唤醒的 worker 看到的任务描述只有"你收到了来自其他代理的消息，请查看收件箱并根据消息内容采取行动"。它不知道自己为什么被唤醒，于是 LLM 自由决策——大概率选择"那我回复一下吧，礼貌一点"。

**应当修复方向**：唤醒任务的 description 里应当**直接展开未读邮件的摘要**（比如前 3 条邮件的 summary 字段拼接），让 LLM 直接看到"哦，这是 worker-2 在做通信测试"，从而能做出"这不是真的需要回复的请求"的判断。

### 根因 4：worker/explorer system prompt 强制回复
现在的 prompt 写：

> "收到 `<agent-mail type="question">` 时，应**尽快回复**（msg_type="reply"）"

这条规则在面对自动化"通信测试"场景时变成无限循环引擎。"应"被 LLM 解读为"必须"。

**应当修复方向**：
- 把"应尽快回复"弱化为"如果你能立即给出对发送方有价值的回答，可以 reply；如果对方只是在做通信测试或闲聊广播，可以 ignore"
- 加上反例："不要回复以下类型的消息：a) 来自 to=* 的广播且 content 含'测试/check/verify/确认收到'等关键词；b) 你已经在过去 5 分钟回复过同一发送方的类似消息"
- 引入"reply 抑制"：worker 自己跟踪自己最近 N 条已发出消息，避免重复回复

---

**为什么这是 P0 架构级缺陷**：

任何一个代理（甚至 user 自己通过 `/steer`）只要发出一条 `to=*, type=question` 的消息，理论上都能让整个系统进入无限循环。这不是"边界场景"，而是**系统的默认行为**——上面 4 个根因任意一个都能独立触发问题，必须**全部修复**才能彻底关闭。

不修复带来的后果：
- 任何"通信测试"类自检任务都会爆炸
- 任何包含广播的协作场景（如"通知所有人你的修改"）都有相同风险
- 长期运行系统的 token 成本不可控
- 系统自我陷入"代理之间的礼貌邮件交换"，对用户原始请求毫无进展

**修复优先级**：必须在下一次系统测试**之前**解决，否则任何涉及多代理协作的测试都不可信。

**讨论焦点**（需要在动手前对齐）：
1. mail-notifier 是修改还是重写？现在是 fire-and-forget 的简单设计，加去重需要额外状态
2. `chain_depth` 字段加在哪一层——`mailbox.Message` 还是任务的 metadata？
3. Worker prompt 的"是否应该回复"规则用什么粒度表达？是写死的几条 if-else 还是给 LLM 一个判断框架？
4. 是否需要一个系统级的 "broadcast cooldown"——同一来源 5 分钟内的第二次广播自动降级为非唤醒？

**状态**：🚧 临时一刀切禁用中（2026-04-09）

**临时缓解（2026-04-09）**：
- 新增 `config.MailNotifierEnabled bool`，默认 `false`
- `bootstrap.Start` 在 `MailNotifierEnabled=false` 时**不启动** `MailNotifier.Run` goroutine，仅打印 `[启动] 邮差通知器已禁用` 日志
- mailbox 投递、`send_message` 工具、scheduler 自驱 drain（scheduler 有自己的 ticker，不依赖 notifier）、ack 回执均**保留**，仅"为空闲代理发布唤醒任务"这一环节被切断
- 已知退化：空闲 worker/explorer 不会被自动唤醒读邮件，邮件仅在 agent 被 scheduler 派发其他任务时被动 drain

**恢复条件**（必须 4 项全部完成）：
1. mail-notifier 按 agentID 去重（同代理同时最多一个 pending 唤醒任务，新邮件合并入既有任务）
2. `mailbox.Message` 加 `chain_depth` 字段，超过 `mail_chain_max_depth`（建议 3）的邮件正常投递但不再触发自动唤醒
3. 唤醒任务 description 直接展开未读邮件摘要，让 LLM 看到具体内容做判断
4. worker/explorer system prompt 把"收到 question 应回复"改为弱化版 + 反例 + reply 抑制（5 分钟内不重复回复同一发送方的同类消息）

---

## Trace 文件多 goroutine 并发写入可能存在竞争

**位置**：未来 `internal/trace/writer.go`（尚未实现）

**严重程度**：🟡 P1（实现时必须考虑，否则 trace 文件可能损坏）

**背景**：规划中的任务级 trace 系统采用"每任务一个 JSONL 文件"的策略。从**文件层面**看每个任务独立，无跨任务竞争。但从**同一任务文件内部**看，存在并发写入：

- `llm_executor.go` 的并行工具执行段：同一个 task 的多个工具调用在多个 goroutine 中并发执行，每个 goroutine 都会 emit `tool_call` 和 `tool_result` 事件到同一个 task 的 trace 文件
- scheduler 发布事件、worker 认领事件、watchdog 健康检查事件可能同时到达同一个 task 的文件
- history_compaction 事件可能和其他工具调用事件同时发生

**风险**：
- JSONL 按行切分，如果两个 goroutine 同时 `f.Write([]byte(line + "\n"))`，行与行之间可能交错，产生**破损的 JSON 行**
- trace 文件损坏后 `jq` / `agentgo trace show` 都会解析失败，排查反而更难

**修复方向**（实现时必须做）：

两种可选设计，任选其一：

**选项 A：每任务一把互斥锁 + 同步写入**
```go
type Writer struct {
    mu    sync.Mutex
    files map[string]*os.File  // taskID → 文件句柄
}
func (w *Writer) Emit(event Event) {
    w.mu.Lock()
    defer w.mu.Unlock()
    f := w.fileFor(event.TaskID)
    data, _ := json.Marshal(event)
    f.Write(append(data, '\n'))
}
```
- 优点：简单、立刻可用
- 缺点：锁竞争可能影响工具并发性能（虽然 write 本身很快）

**选项 B：每任务一个 buffered channel + 独立写 goroutine**
```go
type Writer struct {
    channels map[string]chan Event  // taskID → 事件队列
}
// 每个 task 首次 Emit 时启动一个专属 goroutine，循环消费 channel 写文件
```
- 优点：主流程 fire-and-forget，不阻塞
- 缺点：实现复杂，需要处理 goroutine 生命周期（任务结束时关闭 channel）

**建议**：先做选项 A，简单可靠。如果出现性能问题再切到选项 B。

**另外的保险**：`encoding/json` 的 `Marshal` 保证输出的单行 JSON 不含换行，所以只要 `Write` 是原子的（POSIX `write(2)` 对 < PIPE_BUF 字节的写入是原子的，一般为 4KB），小事件可以不加锁。但 trace 事件可能超过 4KB（如含大段 args 的工具调用），所以**还是必须加锁**。

**状态**：⏳ 实现 trace 系统时必须同步处理

---

## ~~read_file 不返回文件总行数信息~~（已修复，self-describing header）

**修复时间**：2026-04-08（与 Level 3 Artifacts 改造同期）

**修复内容**：`read_file` 现在以 `[file] <path> (lines X-Y of N)\n[hash] <sha256>\n---\n<content>` 格式返回。LLM 可以一眼看到"返回的就是 lines 200-432 of 432，已经到底"，不再盲目翻页。

实现位于 `internal/tools/local_read.go:formatReadFileResult`，配套测试 `TestReadFile_SelfDescribingHeader`。

---

## 2026-04-08 第二轮系统测试 — 已修复（11 项）

第一次大修后又跑了一轮系统测试，暴露了一系列新的关联问题，集中修复如下。详细的设计讨论见对话日志。

### ~~Explorer 越权 expected_artifacts 引发死循环~~ ✅
- **现象**：Scheduler 给 explore 任务声明了 `expected_artifacts`，Explorer 没有 write 工具永远满足不了契约 → 重试 6+ 次烧 16 分钟
- **修复**：`scheduler.toolPublishTask` 和 `tools/meta.publishTask` 双端硬拒绝 `event_type=explore && expected_artifacts != nil`；scheduler prompt 加"代理能力清单"段，告知 explorer 是只读代理

### ~~EventType 弱匹配导致 Worker 抢 explore 任务~~ ✅
- **现象**：`store.QueryAvailable` 用 `if eventType != "" && task.EventType != eventType`，意味着 worker（eventType=""）会顺手认领 explore 任务，引发跨代理类型迁移
- **修复**：改为严格 `if task.EventType != eventType`，附测试 `TestQueryAvailable_FilterAndSort` 验证空过滤器只返回 EventType="" 的任务

### ~~可恢复错误重试无上限（最严重的一类死循环）~~ ✅
- **现象**：`agent.handleFailure` 的 recoverable 路径只调 `RetryRollback`，**没有 MaxRetries 检查**。ExpectedArtifacts 校验失败一直走可恢复路径，实测重试 24+ 次跨度 2 小时
- **修复**：handleFailure 的 recoverable 分支也检查 `RetryCount >= cfg.MaxRetry`，超限调用 `terminateTask`（FailTask + 崩溃汇报）

### ~~任务终态崩溃无汇报，scheduler 静默等待~~ ✅
- **现象**：任务最终失败时 scheduler 只能从公告板看到 `status=failed`，无任何上下文，可能继续等待依赖任务永不返回
- **修复**：新增 `agent.terminateTask + sendCrashReport`，向 `task.EventSource` 发送 `priority=high` 邮件，正文格式："代理 X 在执行任务 Y 时崩溃，原因 Z；任务描述、重试次数、expected vs actual artifacts、worker 最后一次响应原文"

### ~~校验失败反馈不进入历史，重试 LLM 看不见自己被打回~~ ✅
- **现象**：ExpectedArtifacts 校验失败后只 log 错误，重试时 LLM 看到的还是上一次的成功输出 → 无理由改变行为 → 死循环
- **修复**：`appendValidationFeedback` 把校验失败的诊断（缺失文件、实际写入文件、纠正策略）作为 `<validation-feedback>` 段以 IncomingMail 形式注入历史，重试时 LLM 能直接看见为什么被打回

### ~~ExpectedArtifacts 路径精确匹配过于刚性~~ ✅
- **现象**：worker 把 `expected="report.md"` 写到 `docs/report.md`，basename 一致但精确字符串不等，触发死循环
- **修复**：`checkExpectedArtifacts` 改为返回 `ArtifactCheckResult{Missing, Drifted, Actual}`。精确匹配失败时按 `filepath.Base` 兜底命中并记 `Drifted` warning，不再硬卡。配套测试 `TestCheckExpectedArtifacts_BasenameDriftToleratedAsSuccess`

### ~~RetryRollback 状态冲突误报为 error~~ ✅
- **现象**：watchdog 已接管任务时 agent 还在尝试 RetryRollback，store 返回 `ErrTaskNotProcessing` 被当成 error 日志报出
- **修复**：handleFailure / handleMaxLoops 检测 `errors.Is(err, store.ErrTaskNotProcessing)`，降级为 warning

### ~~失败路径上 worker 的最终响应被静默丢弃~~ ✅
- **现象**：worker 提交 "no tool call" 响应时如果 ExpectedArtifacts 校验失败，`lastOutput` 是局部变量直接随栈消失。`task.Results` 永远空，scheduler 只能看到一个干瘪的 "重试次数耗尽" 错误
- **修复**：
  - 新增 `model.Task.LastResponse` 字段
  - 新增 `Store.RecordLastResponse(taskID, content)` 接口和 memory 实现
  - `agent.processTask` 在每次 non-tool 响应时无条件持久化 `lastOutput`，无论后续校验成败
  - `taskSnapshot` 暴露 `Artifacts` 和 `LastResponse`，让 scheduler 在公告板里能看到 worker 自述了什么

### ~~Scheduler prompt 缺少"代理能力清单"~~ ✅
- **现象**：LLM 不知道哪类 event_type 对应哪种代理能力，盲目给 explore 任务塞 expected_artifacts
- **修复**：scheduler system prompt 新增"预制代理能力清单"段，说明 Worker（全能落盘）vs Explorer（只读）vs Scheduler 的能力边界，含正反例

### ~~Worker prompt 缺少"路径字面执行"指引~~ ✅
- **现象**：worker 看到任务描述里有"docs/" 关键字，倾向于把输出文件也写到 docs/ 下，与 expected 路径漂移
- **修复**：worker system prompt 新增"产出落盘契约 - 路径字面执行"段，强调 expected_artifacts 中的字符串就是 write_file path 的字面值；新增 "如何读取 `<validation-feedback>` 自我纠正" 段

---

## 架构决策：删除 git 依赖 / Worktree 子系统（2026-04-09）

**背景**：第二轮系统测试暴露出 4 项 P0 级 worktree 相关问题（merge 失败假成功、main 脏状态阻塞 merge、git 分支 ref 泄漏、worktree 重试丢失上下文）。复盘后判断 git worktree 子系统是过度设计——名义上提供 4 个能力（隔离/原子性/3-way merge/git history），实际只有"进程级隔离"真兑现，其余 3 项要么冗余（trace 已覆盖 history）要么基本不工作（LLM 永远整体重写文件，3-way merge 算法没有用武之地）。

**决策**：

> **AgentGo 的代码本体永远不调用 git。** Git 是项目用户的外部工具，不是 agent 的运行时依赖。

**执行**（2026-04-09 完成）：

- 删除 `internal/isolation/` 整个包（worktree.go, resolver.go + tests）
- 移除 `worker.go` / `explorer.go` / `agent.go` / `bootstrap.go` 中所有 wtManager / resolver 接线
- 删除 `config.WorktreeEnabled`、`model.Task.WorktreePath`
- 简化 `tools.DefaultWorkdir`：从二态 Set/Fallback 退化为单态 ProjectRoot
- 简化 `tools.normalizeArtifactPath`：删除 `.worktrees/<id>/` 前缀剥离逻辑
- 删除 `trace.KindWorktreeCreated` / `KindWorktreeMerged` 事件类型及关联字段
- 删除 `internal/worker/worktree_isolation_test.go` 和 `internal/bootstrap/worktree_wiring_test.go`
- 19 个 Go 包全部编译通过、单元测试全绿

**保留的、与 git 无关的能力**：

- `roster` 子系统（agent 注册表 + 文件锁）——与 worktree 零耦合，必须保留作为感知全局活跃代理的方法
- `expected_hash` TOCTOU 检查
- `pathutil.ValidatePath` 路径越界防护
- `Artifacts` / `ExpectedArtifacts` / `LastResponse` / 校验反馈 / 崩溃汇报 全部数据流
- trace 系统主体
- shell 拦截子系统（用户依然可以通过 `run_shell` 用 git，agent 自身不调用即可）

**故意暴露的临时退化**（等待"重建多代理协同"阶段按真实失败模式针对性修复）：

1. 两个 worker 并发写同一个文件 → 后写覆盖前写（无任何防护）
2. 任务执行中失败 → 半成品文件留在 ProjectRoot（无回滚）
3. 任务 A 写入对任务 B 立即可见（无可见性隔离）
4. 看门狗杀任务时无文件状态清理

这些问题被允许暴露，作为下一阶段设计的输入。

**因此一并作废的 P0 / P1 条目**：

- ~~Worktree commit/merge 失败 → 任务假成功~~：worktree 已删除，问题不复存在
- ~~Scheduler `report_done` 不基于 Artifacts~~：仍未完全修复，但与 worktree 无关，独立追踪
- ~~Main 工作区脏状态阻塞 worktree merge~~：worktree 已删除，问题不复存在
- ~~Git 分支 ref 泄漏~~：worktree 已删除，问题不复存在
- ~~Worktree 重试丢失文件上下文~~：worktree 已删除，问题不复存在

注意 "Scheduler report_done 不基于 Artifacts" **没有**被这次架构决策解决——它是 scheduler 的独立缺陷，需要单独修复（让 toolReportDone 强制扫描 task.Artifacts 注入真实清单），保留在待办列表。

---

## Scheduler `report_done` 不基于 `task.Artifacts` 真实清单

**位置**：`internal/scheduler/scheduler.go` `toolReportDone`

**严重程度**：🔴 P0（数据正确性）

**现象**：2026-04-08 第二轮系统测试中，`taskSnapshot` 已经包含 `Artifacts` 字段，但 LLM 在生成 summary 时仍然抄 worker 的 `last_response` 文本，凭空声称 3 个不存在的产物。

**根因**：`toolReportDone` 把 LLM 给的 summary 字符串直接打印到终端，没有任何事实校对。LLM 看到的快照里有 Artifacts 但没有任何机制强制它使用 Artifacts。

**修复方向**：`toolReportDone` 内部强制扫描 `currentBatch` 每个任务的 `task.Artifacts`，构造一段"实际写入磁盘的文件清单"附加到 LLM summary 末尾，覆盖 LLM 的自由发挥。例如：

```
=== 任务完成 ===
<LLM 给的 summary>

=== 实际产出（系统校验） ===
任务 8357c8f9: 无文件产出
任务 49c665df: docs/archived/归档文档分析报告.md
任务 7957bfc7: 无文件产出
```

LLM 想编也编不出来。

**状态**：⏳ 待修复（独立 P0，未被 2026-04-09 删 git 架构决策解决）

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
| 日志审计颗粒度不足 | ✅ 已修复 |
| **Worker 凭空捏造任务结果** | ✅ 已修复（2026-04-08 Level 3：Artifacts + ExpectedArtifacts 硬合约） |
| **Worker 任务无文件产出** | ✅ 已修复（同上） |
| **read_file 不返回总行数** | ✅ 已修复（2026-04-08 self-describing header） |
| **Explorer 越权 expected_artifacts** | ✅ 已修复（第二轮，scheduler/meta 双端硬拒绝） |
| **EventType 弱匹配 → Worker 抢 explore** | ✅ 已修复（第二轮，QueryAvailable 严格匹配） |
| **可恢复错误重试无上限** | ✅ 已修复（第二轮，handleFailure 接入 MaxRetries） |
| **任务终态崩溃无汇报** | ✅ 已修复（第二轮，sendCrashReport priority=high 邮件） |
| **校验反馈不进入历史** | ✅ 已修复（第二轮，appendValidationFeedback IncomingMail） |
| **ExpectedArtifacts 路径过于刚性** | ✅ 已修复（第二轮，basename 兜底 + Drift 标记） |
| **RetryRollback 状态冲突误报** | ✅ 已修复（第二轮，降级为 warning） |
| **失败路径 worker 响应被丢弃** | ✅ 已修复（第二轮，Task.LastResponse + Store.RecordLastResponse） |
| Scheduler prompt 缺代理能力清单 | ✅ 已修复（第二轮） |
| Worker prompt 缺路径字面执行指引 | ✅ 已修复（第二轮） |
| Shell 拦截 E2E 测试缺口 | ⏳ 本轮不实施（见 nextUpgrade_v2.md） |
| Scheduler 提前 report_done | ⚠️ 已临时缓解（prompt + 硬性拦截），根因未解决 |
| **Scheduler 事件响应延迟 3 分钟** | 🟡 **P1 待排查** |
| **Trace 多 goroutine 写入竞争** | 🟡 **P1 复核**（trace 系统已实现，需确认上锁覆盖） |
| **邮件级联爆炸**（4 根因叠加） | 🚧 **临时一刀切禁用中**（2026-04-09，`mail_notifier_enabled=false` 默认）；4 项根因修复完成后再恢复 |
| **Scheduler report_done 不基于 Artifacts** | 🔴 **P0**（独立 scheduler 缺陷，未被架构决策解决） |
| **架构决策：删除 git 依赖** | ✅ **已执行**（2026-04-09，删除 internal/isolation/ 等全部 worktree 接线，19 包测试全绿） |
| **多代理协同重建**（并发写 / 原子性 / 跨任务可见性 / 杀任务清理） | 🟡 **设计待启动**（4 项故意暴露的临时退化，按真实失败模式驱动设计） |

**24/29 项已修复。剩余：1 项 P0（scheduler report_done 事实校对）+ 1 项 P0（邮件级联，已临时禁用）+ 2 项 P1（Scheduler 延迟、Trace 多 goroutine）+ 1 项 E2E 测试 + 1 项设计任务（多代理协同重建）。**

> 注：6 项 worktree 相关条目（Worktree 相对路径解析、Worktree Remove git 失忆兜底、Worktree merge 假成功、Main 工作区脏状态、Git 分支 ref 泄漏、Worktree 重试丢上下文）已于 2026-04-09 整体清出本文档 — 详细复盘随 `internal/isolation` 包一同消失。仅在"架构决策：删除 git 依赖"段保留作为历史索引。

近期修复轨迹：
- **2026-04-08 第一轮**：trace.CloseTask defer 顺序、Level 3 Artifacts/ExpectedArtifacts 全量方案、read_file 自描述头部、scheduler/worker prompt 重塑
- **2026-04-08 第二轮**：Explorer 越权拒绝、EventType 严格匹配、可恢复错误受 MaxRetries 约束、终态崩溃汇报邮件、校验反馈注入历史、basename 兜底、Task.LastResponse 持久化
- **2026-04-09 架构决策**：删除 git/worktree 子系统，回归"所有 worker 共享 ProjectRoot"的简单模型；6 项 worktree 相关条目（4 P0 + 2 已修复）一并清出本文档
- **2026-04-09 邮件级联临时禁用**：`mail_notifier_enabled=false` 默认；恢复条件见对应条目
- **下一阶段目标**：(a) 修复 scheduler report_done 事实校对（独立 P0，非架构层）；(b) 设计多代理协同重建方案（按真实失败模式驱动）
