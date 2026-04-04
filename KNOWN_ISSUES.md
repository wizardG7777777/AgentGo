# Known Issues

## P2 — 质量与行为优化

### WD-1：随机采样可能遗漏紧急任务

Watchdog 用 50% 随机采样巡检任务，已超时的任务有 50% 概率每轮被跳过。MVP 任务量（≤100）下全量扫描开销可忽略。

- **位置**: `internal/watchdog/watchdog.go`（`sampleTasks` 函数）
- **影响**: 超时检测延迟

### BOOT-2：Scheduler system prompt 对 plan mode 描述不够明确

`schedulerSystemPrompt` 对计划模式只有一句泛泛描述，没有告诉 LLM 在计划模式下的具体行为约束。

- **位置**: `internal/scheduler/scheduler.go`（`schedulerSystemPrompt` 常量）
- **影响**: 计划模式下调度决策质量不稳定

## P3 — 代码规范与便利性

### BOOT-1：System.Store 暴露了具体类型

`System` 结构体中 `Store` 字段类型是 `*store.MemoryTaskStore` 而非 `store.TaskStore` 接口，破坏接口抽象。

- **位置**: `internal/bootstrap/bootstrap.go`（`System` 结构体）
- **影响**: 将来替换 Store 实现时需修改结构体

### ~~CONFIG-1：无 CLI 参数覆盖配置值~~（已修复）

`main.go` 现已支持 `-config` 旗标指定配置文件路径，缺省时使用 `setting.yaml`，文件不存在时在终端打印警告并回退内置默认配置。
