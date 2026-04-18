# 工具集分层配置（Tool Set Profiles）

> 状态：✅ 已实现（2026-04）
> 关联：nextUpgrade_v3.md §9.1

## 概述

工具集分层配置允许用户通过配置文件定义命名工具集（profile），并为不同类型的代理指定使用的工具集。这实现了工具集与代理类型的解耦，支持按任务场景裁剪代理能力。

## 配置方式

在配置文件（`config.yaml` 或 `config.json`）中添加以下字段：

```yaml
# 命名工具集定义
tool_profiles:
  worker_standard:        # 标准执行代理：代码修改 + 网络
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
  
  worker_readonly:        # 只读执行代理：无写权限
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message
  
  explorer_codebase:      # 代码库调查：本地只读
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message
  
  explorer_web:           # 网络调查：网络只读
    - web_search
    - web_fetch
    - send_message
  
  explorer_full:          # 全能调查：本地 + 网络只读
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message

# 为各代理类型指定工具集
worker_profile: worker_standard    # Worker 使用标准工具集
explorer_profile: explorer_full    # Explorer 使用全能调查工具集
```

## 可用工具列表

| 工具名 | 分组 | 说明 |
|--------|------|------|
| `read_file` | LocalReadGroup | 读取文件内容 |
| `list_dir` | LocalReadGroup | 列出目录内容 |
| `grep_search` | LocalReadGroup | 正则搜索文件内容 |
| `glob_search` | LocalReadGroup | 按模式搜索文件路径 |
| `write_file` | LocalWriteGroup | 写入文件（覆盖） |
| `edit_file` | LocalWriteGroup | 编辑文件（行级） |
| `run_shell` | ShellGroup | 执行 shell 命令 |
| `web_search` | WebGroup | 网络搜索 |
| `web_fetch` | WebGroup | 获取网页内容 |
| `publish_task` | MetaGroup | 发布子任务 |
| `send_message` | MetaGroup | 发送消息给其他代理 |

**注意**：以下工具属于 Scheduler 专属，不走 profile 配置：
- `cancel_task` - 取消任务
- `report_done` - 汇报完成
- `probe_directory` - 目录探测

## 配置规则

### 1. 留空 = 全部工具

如果 `worker_profile` 或 `explorer_profile` 留空（或不配置），该代理类型将注册所有可用工具：

```yaml
tool_profiles:
  worker_readonly:
    - read_file
    - list_dir

# 不配置 worker_profile，Worker 将拥有全部工具
# worker_profile: ""
```

### 2. Profile 必须存在

如果指定了 profile 名称，但该名称未在 `tool_profiles` 中定义，系统启动时会报错：

```yaml
tool_profiles:
  worker_standard:
    - read_file

worker_profile: worker_readonly  # 错误：profile 不存在
```

### 3. 工具名拼写校验

Profile 中的工具名必须在系统支持的工具列表中。拼写错误会导致启动失败：

```yaml
tool_profiles:
  my_profile:
    - read_file
    - write_filez  # 错误：应为 write_file
```

### 4. Scheduler 不走 Profile

Scheduler 作为一等代理，其工具集由系统固定（Worker 全集 + SchedulerGroup），不通过 profile 配置。这是有意为之的架构隔离。

## 使用场景

### 场景一：只读调查任务

对于只需要分析代码、不修改文件的任务，可以配置 Explorer 使用只读工具集：

```yaml
tool_profiles:
  explorer_readonly:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message

explorer_profile: explorer_readonly
```

### 场景二：限制网络访问

对于内网环境或不需要网络的任务，可以排除网络工具：

```yaml
tool_profiles:
  worker_local_only:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - write_file
    - edit_file
    - run_shell
    - publish_task
    - send_message

worker_profile: worker_local_only
```

### 场景三：最小权限原则

对于高风险任务，只授予必要的工具：

```yaml
tool_profiles:
  worker_minimal:
    - read_file
    - write_file
    - send_message

worker_profile: worker_minimal
```

## Per-Worker Tool Profiles（逐 Worker 工具集配置）

> 状态：✅ 已实现（2026-04）

### 概述

Per-Worker Tool Profiles 允许每个 Worker 实例拥有独立的工具集 profile，取代所有 Worker 共享单一 `worker_profile` 的设计。核心价值是安全隔离与角色特化——一个只做调查工作的 Worker 移除写文件和执行命令的工具后，即使 LLM 产生幻觉也无法造成破坏。Scheduler 可据此做能力感知路由。

### 配置格式

在配置文件中使用 `workers` 列表替代 `worker_count` + `worker_profile`：

```yaml
workers:
  - id: writer-1
    profile: worker_standard
  - id: writer-2
    profile: worker_standard
  - id: reader-1
    profile: worker_readonly
  - id: general-1
    profile: ""              # 空字符串 = 全量工具
```

每条记录包含两个字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | Worker 的唯一标识符，不可为空，不可重复 |
| `profile` | string | 引用 `tool_profiles` 中定义的 profile 名称；空字符串表示使用全部可用工具 |

JSON 格式同样支持：

```json
{
  "workers": [
    {"id": "writer-1", "profile": "worker_standard"},
    {"id": "reader-1", "profile": "worker_readonly"},
    {"id": "general-1", "profile": ""}
  ]
}
```

### 与旧配置的关系

`workers` 列表与旧的 `worker_count` + `worker_profile` 是互斥的两种配置路径：

| 条件 | 行为 |
|------|------|
| `workers` 列表存在且非空 | 以 `workers` 列表为准，忽略 `worker_count` 和 `worker_profile` |
| `workers` 列表不存在或为空 | 回退到 `worker_count` + `worker_profile` 旧行为（向后兼容） |

旧格式：

```yaml
worker_count: 2
worker_profile: worker_standard
```

等价于：

```yaml
workers:
  - id: worker-1
    profile: worker_standard
  - id: worker-2
    profile: worker_standard
```

### 校验规则

系统启动时对 `workers` 列表执行以下校验，任一失败则阻止启动：

1. `id` 不能为空字符串（报错包含记录索引）
2. `id` 不能重复（报错包含重复的 id 值）
3. `profile` 引用的名称必须在 `tool_profiles` 中已定义（空字符串除外）
4. `profile` 中的工具名必须拼写正确（通过 `tools.ValidateToolNames` 校验）

### agent_declarations Per-Profile 扩展

`agent_declarations` 支持 `worker/<profile_name>` 格式的 key，为不同 profile 的 Worker 配置独立的能力标签和描述：

```yaml
agent_declarations:
  worker:                          # 默认 Worker 声明（未匹配 profile 时回退到此）
    capabilities: [code_edit, shell_exec, web_search, subtask_publish, message]
    description: "通用执行代理，拥有完整工具集"
  worker/worker_readonly:          # worker_readonly profile 专属声明
    capabilities: [codebase_read, web_search, message]
    description: "只读执行代理，无写权限，适合审查和调查任务"
  explorer:
    capabilities: [codebase_read, web_search, message]
    description: "只读调查代理"
```

查找顺序（三级回退）：

1. `agent_declarations["worker/<profile_name>"]` — 精确匹配
2. `agent_declarations["worker"]` — 默认 Worker 声明
3. 内置默认值

这些信息会暴露到 Board Snapshot 的 `agent_capabilities` 段中，Scheduler LLM 据此做路由决策。当 `workers` 列表包含不同 profile 时，`agent_capabilities` 会为每种 profile 输出独立的能力声明记录。

### Board Snapshot 输出示例

使用 `workers` 列表时，Board Snapshot 的 `agents` 和 `agent_capabilities` 段会包含 per-profile 信息：

```json
{
  "agents": [
    {"id": "writer-1", "type": "worker", "profile": "worker_standard", "mailbox_pending": 0},
    {"id": "reader-1", "type": "worker", "profile": "worker_readonly", "mailbox_pending": 1}
  ],
  "agent_capabilities": [
    {
      "agent_type": "worker",
      "profile": "worker_standard",
      "capabilities": ["code_edit", "shell_exec", "web_search", "subtask_publish", "message"],
      "description": "标准执行代理，拥有完整工具集"
    },
    {
      "agent_type": "worker",
      "profile": "worker_readonly",
      "capabilities": ["codebase_read", "web_search", "message"],
      "description": "只读执行代理，无写权限"
    }
  ]
}
```

### 使用场景

#### 场景一：安全隔离——读写分离

将调查类任务路由到只读 Worker，修改类任务路由到标准 Worker：

```yaml
tool_profiles:
  worker_standard:
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
  worker_readonly:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message

workers:
  - id: writer-1
    profile: worker_standard
  - id: writer-2
    profile: worker_standard
  - id: reader-1
    profile: worker_readonly

agent_declarations:
  worker:
    capabilities: [code_edit, shell_exec, web_search, subtask_publish, message]
    description: "标准执行代理，拥有完整工具集"
  worker/worker_readonly:
    capabilities: [codebase_read, web_search, message]
    description: "只读执行代理，无写权限，适合审查和调查任务"
```

#### 场景二：混合环境——内网 + 外网

一个 Worker 可以访问网络，另一个只能操作本地文件：

```yaml
tool_profiles:
  worker_local_only:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - write_file
    - edit_file
    - run_shell
    - publish_task
    - send_message
  worker_standard:
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

workers:
  - id: local-worker
    profile: worker_local_only
  - id: web-worker
    profile: worker_standard
```

#### 场景三：最小权限 + 全量工具混合

一个 Worker 只有最小权限，另一个拥有全量工具：

```yaml
tool_profiles:
  worker_minimal:
    - read_file
    - write_file
    - send_message

workers:
  - id: restricted-1
    profile: worker_minimal
  - id: unrestricted-1
    profile: ""              # 全量工具
```

## 与分级权限模型的关系

工具集分层配置是分级权限模型（§9.2）的前置基础设施。未来可以基于此实现：

- **任务级工具裁剪**：Scheduler 在 `publish_task` 时指定该任务允许的工具子集
- **预设权限模板**：定义 `readonly`、`standard`、`privileged` 等权限等级
- **运行时权限提升**：Agent 发现需要额外工具时，向 Scheduler 申请临时提权

当前阶段仅支持代理级别的工具集配置，上述能力留待后续版本。

## 实现细节

### ToolRegistry 白名单过滤

`ToolRegistry` 支持 `NewToolRegistryWithAllowlist(allowed []string)` 创建带白名单的注册表。不在白名单中的工具会被静默跳过：

```go
// internal/agent/tool_registry.go
func NewToolRegistryWithAllowlist(allowed []string) *ToolRegistry {
    r := &ToolRegistry{
        tools: make(map[string]ToolFunc),
        defs:  make([]llm.ToolDef, 0),
    }
    if len(allowed) > 0 {
        r.allowedTools = make(map[string]bool, len(allowed))
        for _, name := range allowed {
            r.allowedTools[name] = true
        }
    }
    return r
}
```

### Bootstrap 集成

系统启动时，Bootstrap 会：

1. 解析 `worker_profile` 和 `explorer_profile` 配置
2. 从 `tool_profiles` 中查找对应的工具列表
3. 校验工具名拼写
4. 将工具列表传递给 Worker/Explorer 构造器

```go
// internal/bootstrap/bootstrap.go Step 6.5
workerAllowed, err := cfg.ResolveProfile(cfg.WorkerProfile)
if err != nil {
    return nil, fmt.Errorf("worker profile 解析失败: %w", err)
}
explorerAllowed, err := cfg.ResolveProfile(cfg.ExplorerProfile)
if err != nil {
    return nil, fmt.Errorf("explorer profile 解析失败: %w", err)
}
// 校验 profile 中的工具名拼写
for profileName, toolNames := range cfg.ToolProfiles {
    if err := tools.ValidateToolNames(toolNames); err != nil {
        return nil, fmt.Errorf("tool_profiles.%s 校验失败: %w", profileName, err)
    }
}
```

## 测试验证

相关测试位于：
- `internal/config/config_test.go` - 配置解析测试
- `internal/agent/tool_registry_test.go` - 工具注册表测试
- `internal/tools/known_tools.go` - 工具名校验测试
