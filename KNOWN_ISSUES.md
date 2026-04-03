# 已发现缺陷

本文档记录 MVP 阶段已知的设计缺陷和未实现的功能，供调试和后续迭代参考。

## 代理空闲回收未实现

**位置**: `internal/agent/agent.go` Run 方法

**现象**: 当所有任务并发数已满时，代理会以 500ms 间隔持续空转轮询 QueryAvailable，不会自行退出。

**影响**: 大量空闲代理浪费 goroutine 资源，违反架构文档中"长时间空闲且系统代理数超过最低保留数量时销毁"的设计（Archtechture.md 第 44 行）。

**建议修复**: 加入空闲计数器，连续空轮询超过阈值后代理自行退出 goroutine。

---

## 代理间无实时事件感知

**位置**: `internal/store/memory.go` sendEvent 方法

**现象**: 事件 channel 只有调度器持有读端，执行代理无法实时感知公告板变化（如任务被取消）。代理在执行途中只能依赖 context 取消来停止。

**影响**: 如果任务被看门狗取消但 context 未被取消，代理可能继续执行已取消的任务，浪费资源。

**建议修复**: 将 sendEvent 改为广播模式，代理可订阅事件 channel。改动范围小，只需修改 sendEvent 方法和 Agent 结构体。

---

## 命令行参数覆盖配置未实现

**位置**: `internal/config/config.go` LoadConfig 方法

**现象**: 架构文档定义了"命令行参数优先级高于配置文件"（Archtechture.md 全局配置章节），但 MVP 只实现了 YAML 文件加载，未解析命令行参数。

**影响**: 无法在不修改配置文件的情况下临时覆盖配置值。

**建议修复**: 使用 `flag` 包或 `cobra` 解析命令行参数，在 LoadConfig 返回后覆盖对应字段。

---

## 代理 ReAct 循环未实现

**位置**: `internal/agent/agent.go` Run / processTask 方法

**现象**: `MaxLoops` 字段在 Agent 结构体中已定义并在 NewAgent 中初始化，但从未被使用。代理只调用一次 `TaskExecutor` 就结束，缺少架构文档描述的多轮 LLM ReAct 循环结构（观察→思考→行动→循环判定）。

**影响**: 代理无法进行多步推理和多次工具调用，无法完成需要迭代的复杂任务。同时，达到循环上限时的重试回退路径（processing→pending，写入"因循环上限终止"标注）也未实现。

**建议修复**: 在 processTask 中实现 for 循环，每轮调用 LLM/TaskExecutor，检查是否需要继续（LLM 是否调用了工具），并在达到 MaxLoops 时触发 RetryRollback。

---

## 启动流程不完整——调度器、调查代理、用户输入未启动

**位置**: `internal/bootstrap/bootstrap.go` Bootstrap / Start 方法

**现象**: 架构文档定义的 7 步启动流程（配置→公告板→花名册→看门狗→调查代理→调度器→用户输入），当前仅实现前 4 步。调查代理（Explorer）、调度器（Scheduler）和用户输入接口均未创建和启动。

**影响**: 系统启动后无法接收用户输入、无法发布和编排任务、无法执行只读调查任务，处于"基础设施就绪但无法工作"的状态。

**建议修复**: 按架构文档顺序依次实现调查代理启动（步骤 5）、调度器启动（步骤 6）、用户输入循环（步骤 7），每步完成后打印对应的中文提示。

---

## 看门狗缺少花名册兜底清理职责

**位置**: `internal/watchdog/watchdog.go` Watchdog 结构体 / inspect 方法

**现象**: 架构文档要求看门狗作为 defer 机制的最后一道防线，定期清理因极端情况（如进程级崩溃）残留的花名册声明。但 Watchdog 结构体中没有 Roster 字段，inspect 方法中也没有任何花名册清理逻辑。

**影响**: 如果代理因极端情况退出且 defer 未执行，其花名册声明将永久残留，导致对应文件被永久锁定。

**建议修复**: 在 Watchdog 结构体中添加 `Roster roster.Roster` 字段，在 inspect 方法中对比公告板中活跃代理列表与花名册声明，清理不属于任何活跃代理的残留声明。

---

## 配置加载不支持 JSON 格式

**位置**: `internal/config/config.go` LoadConfig 方法

**现象**: 架构文档声明支持 `setting.yaml` 或 `setting.json`，但 LoadConfig 硬编码使用 `yaml.Unmarshal`，不判断文件扩展名，JSON 文件会解析失败。

**影响**: 用户无法使用 JSON 格式的配置文件。

**建议修复**: 根据文件扩展名判断格式，`.json` 使用 `encoding/json`，`.yaml`/`.yml` 使用 `gopkg.in/yaml.v3`。

---

## 看门狗重启循环缺少延迟控制

**位置**: `internal/bootstrap/bootstrap.go` runWatchdogWithRecover 方法

**现象**: 当 `Watchdog.Run()` 因 `ctx.Done()` 正常返回后，外层 for 循环会立即再次调用 `Run()`，在 ctx 已取消的情况下形成空转热循环。

**影响**: 系统关闭时可能短暂占用 CPU 资源。

**建议修复**: 在循环体开头优先检查 `ctx.Done()`，或在 panic 恢复后添加短暂延迟（如 1 秒），避免频繁重启。

---

## 启动完成提示信息不完整

**位置**: `internal/bootstrap/bootstrap.go` Start 方法

**现象**: 架构文档要求最终打印 `[启动] 系统就绪，等待用户输入`，实际只打印 `[启动] 系统就绪`。

**影响**: 与架构文档描述不一致，但功能无影响。

**建议修复**: 将提示修改为 `[启动] 系统就绪，等待用户输入`。
