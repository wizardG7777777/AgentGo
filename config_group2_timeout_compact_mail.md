## 1. 超时与资源限制（Timeout & Limits）

| 字段名 | 类型 | 默认值 | yaml tag | json tag | 注释说明 |
|--------|------|--------|----------|----------|----------|
| `DefaultTimeoutSec` | `int` | 300 | `yaml:"default_timeout_sec"` | `json:"default_timeout_sec"` | （无内联注释）任务默认超时时间，单位秒 |
| `ShellTimeoutSec` | `int` | 30 | `yaml:"shell_timeout_sec"` | `json:"shell_timeout_sec"` | （无内联注释）Shell 命令执行超时时间，单位秒 |
| `MaxRetry` | `int` | 3 | `yaml:"max_retry"` | `json:"max_retry"` | （无内联注释）最大重试次数 |

## 2. 压缩与上下文管理（Context Compaction）

| 字段名 | 类型 | 默认值 | yaml tag | json tag | 注释说明 |
|--------|------|--------|----------|----------|----------|
| `CompactTokenThreshold` | `int` | 80000 | `yaml:"compact_token_threshold"` | `json:"compact_token_threshold"` | （无内联注释）触发上下文压缩的 token 阈值 |
| `CompactKeepRecent` | `int` | 3 | `yaml:"compact_keep_recent"` | `json:"compact_keep_recent"` | （无内联注释）压缩时保留的最近消息轮数 |

## 3. 通知与邮箱系统（Notification & Mailbox）

| 字段名 | 类型 | 默认值 | yaml tag | json tag | 注释说明 |
|--------|------|--------|----------|----------|----------|
| `MailboxBufferSize` | `int` | 32 | `yaml:"mailbox_buffer_size"` | `json:"mailbox_buffer_size"` | （无内联注释）邮箱缓冲区大小 |
| `MailNotifierIntervalSec` | `int` | 5 | `yaml:"mail_notifier_interval_sec"` | `json:"mail_notifier_interval_sec"` | （无内联注释）邮件通知器轮询间隔，单位秒 |
| `MailNotifierEnabled` | `bool` | `true` | `yaml:"mail_notifier_enabled"` | `json:"mail_notifier_enabled"` | 控制邮差通知器是否启动。Phase 2 完成后默认 true：4 项防御已经全部到位（ChainDepthLimitHook 截断深链 + PerAgentDedupHook 镜像去重 + WakeContextExpandHook 上下文注入 + worker/explorer 提示词弱化"必回复"），邮件级联爆炸的根因都被堵住了。如有需要可用 yaml 强制关闭。详见 KNOWN_ISSUES.md。 |
| `MailChainMaxDepth` | `int` | 3 | `yaml:"mail_chain_max_depth"` | `json:"mail_chain_max_depth"` | 邮件链跳数上限。worker 通过 send_message 触发的邮件继承"自己当前任务的 MailChainDepth + 1"；超过此阈值的消息仍然会投递到收件箱（保留可见性），但不会触发 mail-notifier 发布唤醒任务，从而打断邮件级联爆炸链。Phase 2 引入；用户 /steer 投递的初始邮件 ChainDepth=0。 |
