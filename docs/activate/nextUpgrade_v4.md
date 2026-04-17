# nextUpgrade v4

> 状态：📝 规划中（2026-04-15 记录）

---

## 1. Per-Worker 工具集配置（独立 Profile 分配）

> 优先级：P2
> 前置依赖：v3 §9.1 工具集分层配置（已完成 2026-04-15，当前所有 worker 共享同一 profile）
> 关联：v3 §9.2 分级权限模型、v3 §9.4 能力声明阶段二

### 1.1 背景

v3 §9.1 引入了 Tool Set Profiles：在 YAML 中定义命名工具集，通过 `worker_profile` / `explorer_profile` 全局指定各类 agent 使用哪个 profile。这解决了"工具集可配置"的基础问题，但有一个限制：

> **所有同类 worker 共享同一个 profile。不能在一个系统中同时存在"全能 worker"和"只读 worker"。**

这意味着以下场景无法通过纯配置实现：

- **混合能力团队**：3 个 worker 中 2 个全能（读/写/Shell），1 个只读（纯调查），scheduler 根据任务性质路由
- **安全隔离**：敏感任务（如操作数据库）只给持有 `run_shell` 的 worker，其他 worker 无权限
- **渐进式扩展**：先加一个新 profile 的 worker 试水，确认稳定后再推广到全量

### 1.2 设计方案

Config 新增 `workers` 数组，每个元素独立指定 id、profile、以及未来可能的其他参数：

```yaml
# 方案 B：per-worker 独立配置
workers:
  - id: worker-1
    profile: worker_standard
  - id: worker-2
    profile: worker_standard
  - id: worker-3
    profile: worker_readonly

# 向后兼容：如果没有 workers 数组，则使用 worker_count + worker_profile 全局模式
# （即 v3 §9.1 的当前行为）
worker_count: 3
worker_profile: "worker_standard"
```

**解析优先级**：
1. 如果 `workers` 数组非空 → 按数组内容创建 worker，忽略 `worker_count` / `worker_profile`
2. 如果 `workers` 数组为空或未定义 → 回退到 `worker_count` + `worker_profile` 全局模式

```go
// config.go 新增
type WorkerConfig struct {
    ID      string `yaml:"id" json:"id"`
    Profile string `yaml:"profile" json:"profile"`
}

type Config struct {
    // ... 现有字段

    // Per-worker 配置（优先于 WorkerCount + WorkerProfile）
    Workers []WorkerConfig `yaml:"workers" json:"workers"`
}
```

**Bootstrap 改造**：

```go
// bootstrap.go Step 8
if len(cfg.Workers) > 0 {
    // 方案 B 模式：按 workers 数组逐个创建
    for _, wc := range cfg.Workers {
        allowed, err := cfg.ResolveProfile(wc.Profile)
        if err != nil {
            return nil, fmt.Errorf("worker %s profile 解析失败: %w", wc.ID, err)
        }
        workerLLM := llm.NewSDKClient(...)
        wk := worker.NewWithID(wc.ID, ..., allowed)
        workers = append(workers, wk)
    }
} else {
    // 回退到全局模式（v3 §9.1 现有行为）
    for i := 1; i <= workerCount; i++ {
        wk := worker.NewWithID(fmt.Sprintf("worker-%d", i), ..., workerAllowed)
        workers = append(workers, wk)
    }
}
```

**启动日志**：

```
[启动] 执行代理已启动 (3 个)
  worker-1 [profile=worker_standard, tools=11]
  worker-2 [profile=worker_standard, tools=11]
  worker-3 [profile=worker_readonly, tools=5]
```

### 1.3 与 Scheduler 分配感知的联动

当系统中存在不同能力的 worker 时，scheduler 需要知道"哪个 worker 有什么能力"才能正确路由任务。需要扩展 v3 §8.1 的 `AgentRegistry`：

- 当前 `AgentRegistry` 只注册特化 agent（Explorer）的静态声明
- 扩展后应包含每个 worker 的 profile 信息
- Board snapshot 的 `resources` 段展示每个 worker 的工具集标签
- Scheduler prompt 路由指引据此决定任务的 `event_type` 或目标 worker

这与 v3 §9.4（能力声明阶段二）的"任务级能力需求声明 + ClaimTask 匹配校验"自然衔接——当 worker 有不同 profile 时，`ClaimTask` 可以按 profile 做能力匹配。

### 1.4 预置 Profile 模板

per-worker 模式落地后，以下预置 profile 变得有意义（当前仅 `worker_standard` 和 `explorer_codebase` 在用）：

```yaml
tool_profiles:
  worker_standard:       # 全能执行：代码修改 + Shell + 网络 + 协作
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - write_file
    - edit_file
    - run_shell
    - web_search
    - web_fetch
    - publish_task
    - send_message

  worker_readonly:       # 只读 worker：纯调查，不能修改代码
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message

  worker_code_only:      # 纯代码 worker：无网络无 Shell
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - write_file
    - edit_file
    - publish_task
    - send_message

  explorer_codebase:     # 本地只读调查
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message

  explorer_full:         # 本地 + 网络只读调查
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message
```

### 1.5 实施步骤

| 步骤 | 内容 |
|------|------|
| S1 | Config 新增 `WorkerConfig` 结构体 + `Workers []WorkerConfig` 字段 |
| S2 | Bootstrap `workers` 数组优先解析 + 回退全局模式 |
| S3 | 启动日志逐 worker 打印 profile + tools 数量 |
| S4 | AgentRegistry 扩展：注册每个 worker 的 profile 标签 |
| S5 | Board snapshot `resources` 渲染 per-worker 能力信息 |
| S6 | Scheduler prompt 路由指引支持 per-worker 路由 |
| S7 | 预置 profile 模板 + 配置示例 |
| S8 | 单测 + 多 Worker 系统测试验证 |

### 1.6 不在本节范围

- **Per-explorer 配置**：当前只有 1 个 explorer 实例，per-explorer 没有意义。等 `explorer_count` 配置项引入后再考虑。
- **运行时动态切换 profile**：worker 启动后 ToolRegistry 是只读的。如果需要运行时切换，需要重新设计 ToolRegistry 的并发安全模型。当前不需要。
- **Profile 继承 / extends**：如 `worker_code_only` 继承 `worker_readonly` 再加 `write_file` / `edit_file`。MVP 阶段保持平铺，重复优于隐式。
