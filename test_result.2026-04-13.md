# 配置文件分析报告：internal/config/config.go

## 概述

| 项目 | 说明 |
|------|------|
| **配置文件来源** | `internal/config/config.go` |
| **Config 结构体字段总数** | 33 个 |
| **LoadConfig 支持格式** | YAML（`.yaml` / `.yml`）和 JSON |
| **未设置默认值的字段数** | 7 个（保持 Go 零值：`""` 或 `nil`） |
| **零值字段列表** | `ProjectRoot`、`LLMBaseURL`、`LLMAPIKey`、`SearchAPIURL`、`SearchAPIKey`、`ShellBlacklist`、`ShellGreylist` |

> **加载机制说明**：`LoadConfig` 函数通过 `explicit` 参数控制文件不存在时的行为——显式模式（`explicit=true`）下文件不存在会报错；默认模式（`explicit=false`）下文件不存在则回退到 `DefaultConfig()` 内置默认值。

---

## 1. 调度与事件系统

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `SchedulerTickerSec` | `scheduler_ticker_sec` | `int` | `10` | 调度器时钟周期（秒），控制调度器 tick 频率 |
| `SchedulerMaxLoops` | `scheduler_max_loops` | `int` | `10` | 调度器单次调度的最大循环次数 |
| `EventChannelBuffer` | `event_channel_buffer` | `int` | `64` | 事件通道的缓冲区大小 |
| `FIFOLimit` | `fifo_limit` | `int` | `100` | FIFO 队列容量上限 |

## 2. Agent/Worker 管理

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `WorkerCount` | `worker_count` | `int` | `1` | Worker 代理实例数量 |
| `DefaultConcurrency` | `default_concurrency` | `int` | `2` | 默认并发任务数 |
| `AgentMaxLoops` | `agent_max_loops` | `int` | `50` | Agent 单次执行的最大循环次数 |
| `AgentIdleThreshold` | `agent_idle_threshold` | `int` | `0` | Agent 空闲判定阈值（秒），0 表示禁用空闲检测 |
| `MaxSubtaskDepth` | `max_subtask_depth` | `int` | `1` | 子任务嵌套的最大深度 |
| `MaxRetry` | `max_retry` | `int` | `3` | 任务失败后的最大重试次数 |

## 3. LLM 模型配置

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `LLMBaseURL` | `llm_base_url` | `string` | `""`（空字符串） | LLM API 基础 URL，可对接兼容 OpenAI 接口的替代服务；未在 `DefaultConfig()` 中显式赋值 |
| `LLMAPIKey` | `llm_api_key` | `string` | `""`（空字符串） | LLM API 认证密钥；未在 `DefaultConfig()` 中显式赋值 |
| `LLMModel` | `llm_model` | `string` | `"gpt-4o"` | 主 LLM 模型名称 |
| `LLMTimeoutSec` | `llm_timeout_sec` | `int` | `60` | LLM API 请求超时时间（秒） |
| `ExplorerModel` | `explorer_model` | `string` | `"gpt-4o-mini"` | Explorer 代理使用的 LLM 模型 |
| `ExplorerEventType` | `explorer_event_type` | `string` | `"explore"` | Explorer 事件类型标识符 |

## 4. Shell 执行与安全

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `ShellTimeoutSec` | `shell_timeout_sec` | `int` | `30` | Shell 命令执行的超时时间（秒），超时后终止命令 |
| `ShellBlacklist` | `shell_blacklist` | `[]string` | `nil`（空切片） | Shell 命令黑名单，匹配的命令被直接拒绝执行（用户可追加自定义规则）；未在 `DefaultConfig()` 中显式赋值 |
| `ShellGreylist` | `shell_greylist` | `[]string` | `nil`（空切片） | Shell 命令灰名单，匹配的命令可能需要额外确认或限制（用户可追加自定义规则）；未在 `DefaultConfig()` 中显式赋值 |

## 5. 消息/邮件通知

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `MailboxBufferSize` | `mailbox_buffer_size` | `int` | `32` | 邮箱缓冲区大小，控制代理间消息队列的容量 |
| `MailNotifierIntervalSec` | `mail_notifier_interval_sec` | `int` | `5` | 邮件通知器的轮询间隔（秒），定期检查新邮件并决定是否触发唤醒任务 |
| `MailNotifierEnabled` | `mail_notifier_enabled` | `bool` | `true` | 是否启用邮件通知器（Phase 2 完成后恢复默认启用，4 项防御已就绪） |
| `MailChainMaxDepth` | `mail_chain_max_depth` | `int` | `3` | 邮件链跳数上限；超过此值的邮件仍可投递但不会触发 mail-notifier 唤醒，用于打断邮件级联爆炸 |

## 6. 上下文压缩

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `CompactTokenThreshold` | `compact_token_threshold` | `int` | `80000` | 触发上下文压缩的 token 阈值，当上下文 token 数超过此值时进行压缩 |
| `CompactKeepRecent` | `compact_keep_recent` | `int` | `3` | 压缩时保留的最近消息轮数（不被压缩掉） |

## 7. Token 预算

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `TransferNoteMaxTokens` | `transfer_note_max_tokens` | `int` | `3000` | TransferNote（L1/L3 交接备忘）单条最大 token 预算，按 1 token ≈ 2 runes 估算，约 6000 字符中文 / 12000 字符英文 |

## 8. 文件锁与冲突处理

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `RosterWaitTimeoutSec` | `roster_wait_timeout_sec` | `int` | `30` | 文件冲突排队的最大等待时间（秒）；TryClaim 失败时阻塞等待前任释放，超时后放弃并返回"重用"错误；设为 0 表示不排队（立即返回错误） |

## 9. 项目与超时配置

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `ProjectRoot` | `project_root` | `string` | `""`（空字符串） | 项目根目录路径，未设默认值，需外部指定；未在 `DefaultConfig()` 中显式赋值 |
| `DefaultTimeoutSec` | `default_timeout_sec` | `int` | `300` | 全局默认超时时间（秒），任务通用兜底超时 |

## 10. 搜索 API 配置

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `SearchAPIProvider` | `search_api_provider` | `string` | `"duckduckgo_html"` | 搜索引擎提供商标识，默认使用 DuckDuckGo HTML 模式 |
| `SearchAPIURL` | `search_api_url` | `string` | `""`（空字符串） | 自定义搜索 API 端点 URL；为空时使用 provider 内置默认值；未在 `DefaultConfig()` 中显式赋值 |
| `SearchAPIKey` | `search_api_key` | `string` | `""`（空字符串） | 搜索 API 认证密钥；未在 `DefaultConfig()` 中显式赋值 |

## 11. 看门狗/监控

| Go 字段名 | YAML 键名 | 类型 | 默认值 | 功能说明 |
|-----------|-----------|------|--------|----------|
| `WatchdogIntervalSec` | `watchdog_interval_sec` | `int` | `30` | 看门狗轮询间隔（秒），用于检测 agent 是否卡死或失联 |
