## 1. 任务传递与冲突管理（Task Transfer & File Conflict）

| 字段名 | 类型 | 默认值 | yaml tag | json tag | 注释说明 |
|--------|------|--------|----------|----------|----------|
| `TransferNoteMaxTokens` | `int` | `3000` | `transfer_note_max_tokens` | `transfer_note_max_tokens` | TransferNoteMaxTokens 是 TransferNote 单条最大 token 预算。agent 在生成 L1/L3 交接备忘时按此预算截断文本长度——按 1 token ≈ 2 runes 估算。默认 3000 对应 ~6000 字符中文或 ~12000 字符英文。参考 next_upgrade_v3.md §8.4.6 的 token 预算规划。 |
| `RosterWaitTimeoutSec` | `int` | `30` | `roster_wait_timeout_sec` | `roster_wait_timeout_sec` | RosterWaitTimeoutSec 是文件冲突排队的最大等待时间（秒）。当 TryClaim 失败时，工具层调用 Roster.WaitForRelease 阻塞等待前任释放，超时后放弃并返回"忙"错误。设为 0 表示不排队（退回旧行为：立即返回错误）。参考 next_upgrade_v3.md §8.3 文件冲突排队设计。 |

---

## 2. 搜索 API 配置（Search API）

| 字段名 | 类型 | 默认值 | yaml tag | json tag | 注释说明 |
|--------|------|--------|----------|----------|----------|
| `SearchAPIProvider` | `string` | `"duckduckgo_html"` | `search_api_provider` | `search_api_provider` | （无行内注释） |
| `SearchAPIURL` | `string` | `""` | `search_api_url` | `search_api_url` | （无行内注释） |
| `SearchAPIKey` | `string` | `""` | `search_api_key` | `search_api_key` | （无行内注释） |

---

## 3. Shell 命令安全配置（Shell Security）

| 字段名 | 类型 | 默认值 | yaml tag | json tag | 注释说明 |
|--------|------|--------|----------|----------|----------|
| `ShellBlacklist` | `[]string` | `nil` | `shell_blacklist` | `shell_blacklist` | Shell 命令拦截配置（追加到默认规则） |
| `ShellGreylist` | `[]string` | `nil` | `shell_greylist` | `shell_greylist` | Shell 命令拦截配置（追加到默认规则） |
