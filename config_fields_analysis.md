## 1. 调度与事件管理（Scheduler & Event）

| 字段名 | 类型 | 默认值 | yaml/json tag | 注释说明 |
|---|---|---|---|---|
| `SchedulerTickerSec` | `int` | `10` | `yaml:"scheduler_ticker_sec"`<br>`json:"scheduler_ticker_sec"` | 无内联注释；根据字段名推测为调度器心跳/轮询间隔（秒） |
| `SchedulerMaxLoops` | `int` | `10` | `yaml:"scheduler_max_loops"`<br>`json:"scheduler_max_loops"` | 无内联注释；根据字段名推测为调度器单次唤醒最大循环处理次数 |
| `FIFOLimit` | `int` | `100` | `yaml:"fifo_limit"`<br>`json:"fifo_limit"` | 无内联注释；根据字段名推测为 FIFO 队列容量上限 |
| `EventChannelBuffer` | `int` | `64` | `yaml:"event_channel_buffer"`<br>`json:"event_channel_buffer"` | 无内联注释；根据字段名推测为事件通道的 buffer 大小 |
| `WatchdogIntervalSec` | `int` | `30` | `yaml:"watchdog_interval_sec"`<br>`json:"watchdog_interval_sec"` | 无内联注释；根据字段名推测为看门狗检查间隔（秒） |

## 2. Agent 与 Worker 管理

| 字段名 | 类型 | 默认值 | yaml/json tag | 注释说明 |
|---|---|---|---|---|
| `AgentMaxLoops` | `int` | `50` | `yaml:"agent_max_loops"`<br>`json:"agent_max_loops"` | 无内联注释；根据字段名推测为 Agent 单次任务最大循环次数 |
| `AgentIdleThreshold` | `int` | `0` | `yaml:"agent_idle_threshold"`<br>`json:"agent_idle_threshold"` | 无内联注释；根据字段名推测为 Agent 空闲判定阈值（秒），0 可能表示不启用空闲检测 |
| `WorkerCount` | `int` | `1` | `yaml:"worker_count"`<br>`json:"worker_count"` | 无内联注释；根据字段名推测为 Worker 并发数量 |
| `MaxSubtaskDepth` | `int` | `1` | `yaml:"max_subtask_depth"`<br>`json:"max_subtask_depth"` | 无内联注释；根据字段名推测为子任务嵌套最大深度 |
| `DefaultConcurrency` | `int` | `2` | `yaml:"default_concurrency"`<br>`json:"default_concurrency"` | 无内联注释；根据字段名推测为默认并发任务数 |
| `ProjectRoot` | `string` | `""` | `yaml:"project_root"`<br>`json:"project_root"` | 无内联注释；根据字段名推测为项目根目录路径，空字符串表示未设置 |

## 3. LLM 与模型配置

| 字段名 | 类型 | 默认值 | yaml/json tag | 注释说明 |
|---|---|---|---|---|
| `LLMBaseURL` | `string` | `""` | `yaml:"llm_base_url"`<br>`json:"llm_base_url"` | 无内联注释；根据字段名推测为 LLM API 基础地址，空字符串表示使用默认端点 |
| `LLMAPIKey` | `string` | `""` | `yaml:"llm_api_key"`<br>`json:"llm_api_key"` | 无内联注释；根据字段名推测为 LLM API 密钥，空字符串表示未配置 |
| `LLMModel` | `string` | `"gpt-4o"` | `yaml:"llm_model"`<br>`json:"llm_model"` | 无内联注释；根据字段名推测为主 LLM 模型名称 |
| `LLMTimeoutSec` | `int` | `60` | `yaml:"llm_timeout_sec"`<br>`json:"llm_timeout_sec"` | 无内联注释；根据字段名推测为 LLM 请求超时时间（秒） |
| `ExplorerModel` | `string` | `"gpt-4o-mini"` | `yaml:"explorer_model"`<br>`json:"explorer_model"` | 无内联注释；根据字段名推测为 Explorer 专用模型名称 |
| `ExplorerEventType` | `string` | `"explore"` | `yaml:"explorer_event_type"`<br>`json:"explorer_event_type"` | 无内联注释；根据字段名推测为 Explorer 任务的事件类型标识 |

> **注**：以上所有字段在 `internal/config/config.go` 源码中均无行内注释（inline comment）。"注释说明"列基于字段名和 `DefaultConfig()` 中的默认值进行语义推断，非源码原文，标注为 `[推断]`。实际业务含义需结合调度器、Agent、LLM 调用等具体使用处代码确认。
