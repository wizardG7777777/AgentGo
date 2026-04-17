# AgentGo 配置项功能分组调查报告

## 数据来源：internal/config/config.go

### 调度核心配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `MaxRetry` | `int` | 3 | 最大重试次数 |
| `DefaultConcurrency` | `int` | 2 | 默认并发数 |
| `FIFOLimit` | `int` | 100 | FIFO 队列限制 |
| `WatchdogIntervalSec` | `int` | 30 | 看门狗检查间隔（秒） |
| `SchedulerTickerSec` | `int` | 10 | 调度器滴答周期（秒） |
| `SchedulerMaxLoops` | `int` | 10 | 调度器最大循环次数 |
| `AgentMaxLoops` | `int` | 50 | Agent 最大循环次数 |
| `EventChannelBuffer` | `int` | 64 | 事件通道缓冲区大小 |
| `DefaultTimeoutSec` | `int` | 300 | 默认超时时间（秒） |
| `AgentIdleThreshold` | `int` | 0 | Agent 空闲阈值（秒），0 表示不限制 |

### LLM 配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `LLMBaseURL` | `string` | `""`（空） | LLM API 基础 URL，需用户配置 |
| `LLMAPIKey` | `string` | `""`（空） | LLM API 认证密钥，需用户配置 |
| `LLMModel` | `string` | `"gpt-4o"` | 默认 LLM 模型名称 |
| `LLMTimeoutSec` | `int` | 60 | LLM 请求超时时间（秒） |

### Explorer 配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `ExplorerModel` | `string` | `"gpt-4o-mini"` | Explorer 使用的 LLM 模型 |
| `ExplorerEventType` | `string` | `"explore"` | Explorer 任务的事件类型标识 |

### 上下文管理配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `ShellTimeoutSec` | `int` | 30 | Shell 命令超时秒数 |
| `CompactTokenThreshold` | `int` | 80000 | 上下文压缩 token 阈值 |
| `CompactKeepRecent` | `int` | 3 | 压缩时保留最近消息数 |

### Agent 管理配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `ProjectRoot` | `string` | `""`（空字符串） | 项目根目录 |
| `MaxSubtaskDepth` | `int` | 1 | 子任务最大深度 |
| `WorkerCount` | `int` | 1 | Worker 数量 |

### 邮箱通知配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `MailboxBufferSize` | `int` | 32 | 邮箱缓冲区大小 |
| `MailNotifierIntervalSec` | `int` | 5 | 邮件通知器间隔秒数 |
| `MailNotifierEnabled` | `bool` | `true` | 邮件通知器是否启用（Phase 2 完成，4 项防御已就绪后恢复默认启用） |
| `MailChainMaxDepth` | `int` | 3 | 邮件链最大跳数（Phase 2 新增，防止邮件级联爆炸；超过此阈值不触发 mail-notifier 唤醒，但仍保留可见性） |
| `TransferNoteMaxTokens` | `int` | 3000 | TransferNote 单条最大 token 预算（约 6000 字符中文 / 12000 字符英文，按 1 token ≈ 2 runes 估算） |
| `RosterWaitTimeoutSec` | `int` | 30 | 文件冲突排队最大等待时间（秒）；设为 0 表示不排队、立即返回错误 |

### 搜索 API 配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `SearchAPIProvider` | `string` | `"duckduckgo_html"` | 搜索 API 提供商，可选值包括 duckduckgo_html 等 |
| `SearchAPIURL` | `string` | `""`（空字符串） | 自定义搜索 API 的 URL 地址，为空时使用各提供商内置地址 |
| `SearchAPIKey` | `string` | `""`（空字符串） | 搜索 API 访问密钥，部分提供商需要认证 |

### Shell 命令拦截配置

| 字段名 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `ShellBlacklist` | `[]string` | `[]string{}`（空切片） | Shell 命令黑名单（追加到默认规则），匹配的命令将被拒绝执行 |
| `ShellGreylist` | `[]string` | `[]string{}`（空切片） | Shell 命令灰名单（追加到默认规则），匹配的命令需要额外确认或审计 |

---

## 统计

| 分组 | 配置项数量 |
|---|---|
| 调度核心配置 | 10 |
| LLM 配置 | 4 |
| Explorer 配置 | 2 |
| 上下文管理配置 | 3 |
| Agent 管理配置 | 3 |
| 邮箱通知配置 | 6 |
| 搜索 API 配置 | 3 |
| Shell 命令拦截配置 | 2 |
| **总计** | **33** |
