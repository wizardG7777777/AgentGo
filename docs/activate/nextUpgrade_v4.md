# nextUpgrade v4

> 状态：📝 规划中（2026-04-15 记录）

---

## 1. Per-Worker 工具集配置（独立 Profile 分配）

> 状态：✅ **已完成**（2026-04-19）
> 优先级：P2
> 前置依赖：v3 §9.1 工具集分层配置（已完成 2026-04-15，当前所有 worker 共享同一 profile）
> Spec：[`.kiro/specs/per-worker-tool-profiles/`](../../.kiro/specs/per-worker-tool-profiles/)
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

---

## 2. 分级权限模型（PermissionMode）

> 原 v2 §1.4 → v3 §9.2 迁入 | 优先级：P3
> 前置依赖：v3 §9.1 工具集分层配置（✅ 已完成）、v4 §1 Per-Worker 工具集配置
> 关联：v4 §1.3 Scheduler 分配感知联动、v4 §3 能力声明阶段二

### 2.1 背景

不同任务的风险等级不同，"搜索调研"任务不应持有 `write_file / run_shell`，"代码修改"任务不应持有 `web_fetch`。工具集与任务风险不匹配会放大 LLM 幻觉导致的破坏面。

### 2.2 改进方向

- **任务级工具裁剪**：`Task` 结构体新增 `AllowedTools []string` 和/或 `DisallowedTools []string` 字段，Scheduler 在 `publish_task` 时指定
- **预设权限模板**：定义命名权限等级（如 `readonly`、`standard`、`privileged`），Scheduler 通过模板名快速指定
- **运行时权限提升**：Agent 执行中发现需要额外工具时，通过 `permission_request` 协议向 Scheduler 申请临时提权

### 2.3 与 v4 §1 的关系

v4 §1 的 Per-Worker 配置让不同 worker 拥有不同工具集，这是"静态分级"。本节的分级权限模型是"动态分级"——同一个 worker 在不同任务中使用不同权限。两者互补：

- 静态分级（§1）：启动时确定，适合长期角色分工
- 动态分级（§2）：任务级确定，适合临时权限裁剪

### 2.4 触发条件

当 v4 §1 Per-Worker 配置落地后，如果实测发现"同一个 worker 需要在不同任务中使用不同权限"的具体场景，启动本项。

---

## 3. 能力声明阶段二（任务级能力匹配）

> 原 v2 §1.7 阶段二 → v3 §9.4 阶段二迁入 | 优先级：P4
> 前置依赖：v3 §9.4 阶段一（✅ 已完成）、v4 §2 分级权限模型
> 关联：v4 §1.3 Scheduler 分配感知联动

### 3.1 方案

`publish_task` 工具新增 `required_capabilities` 参数，由 `ClaimTask` 逻辑在代理认领时做能力匹配校验。

### 3.2 触发条件

当系统中存在不同能力的 worker（v4 §1 落地后），且通用型 worker 的 capabilities 不再是全集时，`ClaimTask` 匹配校验才有实际意义。

---

## 4. 管理员信赖标记（SourceAdminTrusted）

> 原 v2 §1.6 → v3 §9.3 迁入 | 优先级：P4
> 前置依赖：待引入外部代理/插件机制后

### 4.1 背景

当系统未来支持用户自定义代理或外部插件代理时，需要区分"可信来源"和"不可信来源"，限制不可信代理的工具访问范围。

### 4.2 改进方向

- **代理来源标记**：Agent 结构体新增 `Source string`（如 `"system"`、`"user"`、`"plugin"`）和 `Trusted bool` 字段
- **信任级别与工具映射**：不可信代理自动降级为只读工具集，且不注入 mailbox 的 `send_message` 工具
- **配合分级权限模型**：信任标记作为权限模板选择的输入之一

### 4.3 触发条件

出现外部代理接入机制后启动。当前所有代理由 Bootstrap 内建创建，无需区分信任级别。

---

## 5. 冲突避免长期方案

> 原 v2 §3.2 → v3 §9.7 迁入 | 优先级：P3
> 前置依赖：v3 §8.3 文件冲突排队（✅ 已完成，过渡方案）
> 关联：v4 §1 Per-Worker 配置（worker 数量增加后冲突频率上升）

### 5.1 背景

v3 §8.3 的 Roster 写入等待队列是过渡方案（低效但可用）。当 worker 数量增加到 5+ 且冲突频率显著上升时，需从根源减少冲突发生。

### 5.2 改进方向

- **Roster 意图声明**：扩展 Roster 从"文件写锁"升级为"文件意图声明"——Agent 在修改文件前声明意图，Scheduler 可以看到声明并避免分配涉及同一文件的任务
- **Mailbox 协调**：Agent 发现冲突风险时，通过 send_message 通知对方 Agent 协商分工
- **Scheduler 层面**：在 `boardSnapshot` 中暴露各 Agent 正在修改的文件列表（来自 Roster），让 LLM 在任务规划时主动避开冲突

### 5.3 触发条件

v3 §8.3 过渡方案的实测数据表明冲突频率过高时启动。

---

## 6. Agent 休眠/唤醒优化（Suspend/Resume）

> 原 v2 §3.5 → v3 §9.8 迁入 | 优先级：P4
> 触发条件：Worker 数量扩展到 20+ 时

### 6.1 背景

Agent 空闲时每 500ms 扫描一次 store。在 1-3 个 Worker 的 MVP 规模下 CPU 开销可忽略。

### 6.2 改进方向

- 用 `sync.Cond` 或专用 channel 替代定时轮询：TaskStore 在 `PublishTask` 时 broadcast 通知
- 动态调整 PollInterval：空闲时逐步增大间隔（1s → 2s → 5s），有任务时重置为 500ms

### 6.3 触发条件

Worker 数量扩展到 20+ 时启动。

---

## 状态总览

| 章节 | 内容 | 优先级 | 前置依赖 | 状态 |
|------|------|--------|----------|------|
| §1 | Per-Worker 工具集配置 | P2 | v3 §9.1 ✅ | ✅ 已完成（2026-04-19） |
| §2 | 分级权限模型 | P3 | §1 ✅ | 📝 可启动 |
| §3 | 能力声明阶段二 | P4 | v3 §9.4 阶段一 ✅ + §2 | 📝 待 §2 完成 |
| §4 | 管理员信赖标记 | P4 | 待引入外部代理 | 📝 触发条件未满足 |
| §5 | 冲突避免长期方案 | P3 | v3 §8.3 ✅ | 📝 待冲突频率上升 |
| §6 | Agent 休眠/唤醒 | P4 | 待 Worker 规模增长 | 📝 触发条件未满足 |
