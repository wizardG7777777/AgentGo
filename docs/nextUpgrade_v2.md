# 下一阶段升级计划 v2

## 1. 工具层升级

### 1.1 Web 搜索工具重构

当前 `web_search` 实现直接爬取 DuckDuckGo 的 HTML 页面端点（`https://html.duckduckgo.com/html/`），通过正则匹配 CSS 类名（`result__a`、`result__snippet`）提取搜索结果。该方案存在以下问题：

| 问题 | 影响 |
|------|------|
| 非正式 API，爬取 HTML 页面违反 DuckDuckGo ToS | 法律与合规风险 |
| 正则解析依赖特定 CSS 类名，页面改版即静默失效 | 功能可靠性 |
| 无 rate limit 处理，高频调用会被封 IP | 可用性 |
| 无法获取结构化元数据（发布日期、来源评级等） | 结果质量 |

#### 升级方向

改为**可配置的搜索 API 后端**，在 `Config` 中新增：

```yaml
search_api_provider: "searxng"          # 可选: searxng / tavily / serper / google_custom
search_api_url: "http://localhost:8888" # SearXNG 自建实例地址，或商业 API 端点
search_api_key: ""                      # 商业 API 所需的密钥
```

**推荐的后端选项（按优先级）：**

1. **SearXNG 自建实例**（零成本，隐私友好）：Docker 一键部署，返回标准 JSON，支持多引擎聚合，无 API key 要求
2. **Tavily API**（搜索质量最优）：专为 AI Agent 设计的搜索 API，返回结构化摘要，免费层 1000 次/月
3. **Serper API**（Google 结果）：Google 搜索结果的 JSON API，免费层 2500 次/月
4. **Google Custom Search JSON API**（官方）：每天 100 次免费额度，需配置自定义搜索引擎

实现时保留当前 `web_fetch`（URL 抓取）不变，仅重构 `web_search` 的后端调用逻辑。通过 `SearchProvider` 接口抽象不同后端，运行时按 `search_api_provider` 配置动态选择实现。

当前 DuckDuckGo HTML 爬取方案可保留为 `duckduckgo_html` provider 作为零配置降级方案，但应在启动时打印警告提示用户其局限性。

> **状态：已实现** ✅ — SearchProvider 接口 + 4 个后端（duckduckgo_html / searxng / tavily / serper）

---

## 2. Git Worktree 隔离设计考量

### 2.1 Worktree 是否应纳入公告板机制

当前实现中，worktree 的创建和管理在 Agent 层（OnTaskStart/OnTaskEnd 回调），公告板（TaskStore）仅通过 `Task.WorktreePath` 记录路径。

**纳入公告板的潜在好处**：
- Scheduler 在 `boardSnapshot` 中看到每个任务的 worktree 路径，可以更智能地分配任务（如避免两个任务修改同一模块）
- Watchdog 可以监控 worktree 状态（是否存在残留、磁盘占用等）

**不纳入的理由**：
- worktree 是物理隔离的实现细节，对调度决策的影响有限
- 公告板接口（TaskStore）应保持简洁，不承担文件系统管理职责

**当前决策**：不纳入。如果未来证明 Scheduler 需要 worktree 信息来优化任务分配，可以在 `boardSnapshot` 中追加 `worktree_path` 字段，而无需修改 TaskStore 接口。

### 2.2 Per-Task vs Per-Agent 粒度评判标准

| 维度 | Per-Task | Per-Agent |
|------|----------|-----------|
| 隔离粒度 | 每个任务完全干净的环境 | 同一 Agent 的连续任务共享环境 |
| I/O 开销 | 每次任务创建/销毁 worktree | 仅 Bootstrap/Shutdown 时 |
| 任务间累积副作用 | 无（每次 clean slate） | 有（前一任务的临时文件影响后续） |
| 合并频率 | 每个任务完成后合并 | Agent 生命周期结束时合并 |
| 冲突概率 | 较高（频繁合并） | 较低（集中合并） |
| 适用场景 | 高并发、任务间无关联 | 低并发、任务间有顺序依赖 |

**当前决策**：Per-Task。原因：当前系统设计鼓励 Scheduler 发布独立无依赖的子任务，Per-Task 粒度与此理念一致。如果未来观察到 worktree 创建/销毁的 I/O 成为瓶颈，可回退到 Per-Agent。

---

## 3. 未来改进（待实现）

### 3.1 ConflictResolver 崩溃恢复

冲突处理代理有可能崩溃（LLM 超时、panic 等），这会导致：
- 冲突不能被正确解决
- 等待 DoneCh 的 Agent 永久阻塞（当前有 180s Resolver 侧超时保护，但超时后仅记录日志+放弃合并）

**改进方向**：
- 类似 Watchdog 的 `runWithRecover` 模式，ConflictResolver 崩溃后自动重启
- 未完成的 ConflictRequest 在重启后重新处理（需要持久化请求队列或重放机制）
- 连续崩溃 N 次后降级：放弃自动合并，通知用户手动处理

### 3.2 冲突代理间互相避免

两个 Agent 发生 worktree 冲突说明它们的任务存在文件级竞争。事后解决不如事前避免：

**改进方向**：
- **基于 Roster 的预防**：扩展 Roster 从"文件写锁"升级为"文件意图声明"——Agent 在修改文件前声明意图，其他 Agent 的 Scheduler 可以看到声明并避免分配涉及同一文件的任务
- **基于 mailbox 的协调**：Agent 发现冲突风险时，通过 send_message 通知对方 Agent 协商分工
- **Scheduler 层面**：在 `boardSnapshot` 中暴露各 Agent 正在修改的文件列表（来自 Roster），让 LLM 在任务分配时主动避开冲突

### 3.3 Explorer 权限强化

当前 Explorer 在 worktree 中的"只读"限制仅靠 system prompt 提示，LLM 可能无视。

**改进方向**：
- **工具层面**：Explorer 不注册 write_file/edit_file/run_shell 工具（当前已是如此），但 worktree 本身不阻止 LLM 通过其他方式修改文件
- **文件系统层面**：将 Explorer 的 worktree 挂载为只读（`git worktree add` 后执行 `chmod -R a-w`），从 OS 层面阻止写入
- **沙箱机制**：未来引入容器级或 chroot 级沙箱，为不同权限等级的 Agent 提供硬隔离

### 3.4 ConflictResolver 独立模型配置

当前 ConflictResolver 使用 `cfg.ExplorerModel`。未来应新增独立配置项：
```yaml
resolver_model: "qwen3.6-plus"  # 冲突处理代理专用模型
```
冲突解决需要理解两方代码意图并做出正确取舍，可能需要比 Explorer 更强的推理能力。

### 3.5 Agent 休眠/唤醒优化（Suspend/Resume）

**当前状态**：已由现有机制部分覆盖，不构成当前规模下的实际问题。

**现有机制**：
- Agent 的 `Run` 循环每 500ms 执行一次 `QueryAvailable` → 遍历任务 → 尝试 claim → 失败则 sleep
- `IdleThreshold` 可配置连续空轮询后退出（当前 Worker/Explorer 设为 0，永不退出）
- MailNotifier 为有未读消息的空闲代理发布唤醒任务
- 任务依赖通过 `ClaimTask` 校验，依赖未完成时自动跳过

**现有机制的局限**：
- Agent 空闲时仍在忙等待（每 500ms 扫描一次 store），在 1-3 个 Worker 的 MVP 规模下 CPU 开销可忽略
- 如果扩展到 20+ Worker，每个每 500ms 都扫描 store 并竞争 claim，会成为不必要的开销
- Agent 无法感知"有新任务发布"事件——只能靠下一轮轮询发现

**未来改进方向**（待规模增长后实施）：
- 用 `sync.Cond` 或专用 channel 替代定时轮询：TaskStore 在 `PublishTask` 时 broadcast 通知，空闲 Agent 立即唤醒
- 或采用 `select` 多路监听：同时等待 `time.After(PollInterval)` 和 `taskAvailableCh`，有新任务时提前唤醒
- 动态调整 PollInterval：空闲时逐步增大间隔（1s → 2s → 5s），有任务时重置为 500ms

**暂不实施的原因**：当前 WorkerCount 默认为 1，最多配置到个位数。500ms 轮询的 CPU 和内存开销在微秒级，远不构成瓶颈。优先处理功能完整性和安全性。

### 3.6 Shell 命令拦截配置化

当前 MVP 使用硬编码的 `DefaultBlacklist` 和 `DefaultGreylist`（`internal/shell/intercept.go`）。

**未来改进方向**：
- **配置化名单**：在 `Config` 中新增 `ShellBlacklist []string` 和 `ShellGreylist []string`，用户可在 `setting.yaml` 中自定义规则
- **项目级规则覆盖**：支持 `.agentgo/shell_rules.yaml` 文件，项目维护者可定义项目专属的危险命令规则，与全局配置合并
- **会话级放行记忆**：新增"允许本次会话"选项，用户批准某个命令模式后，同一会话内相同模式不再重复询问（存储在内存中，重启清空）
- **命令白名单模式**：高安全场景下，反转为白名单模式——只允许预定义的命令前缀（如 `go build`、`go test`、`ls`），其余一律走审批

### 3.7 分级权限模型（PermissionMode）

当前 MVP 所有 Worker 拥有相同的完整工具集（10 个），所有 Explorer 拥有相同的只读工具集（4+1 个）。无运行时权限提升/降级机制。

**Why：** 对标 Claude Code 的 `permissionMode` 机制——不同任务的风险等级不同，"搜索调研"任务不应持有 write_file/run_shell，"代码修改"任务不应持有 web_fetch。工具集与任务风险不匹配会放大 LLM 幻觉导致的破坏面。

**未来改进方向**：
- **任务级工具裁剪**：`Task` 结构体新增 `AllowedTools []string` 和/或 `DisallowedTools []string` 字段，Scheduler 在 `publish_task` 时指定。Agent 在 `processTask` 开始时根据任务声明动态过滤 ToolRegistry
- **预设权限模板**：定义命名权限等级（如 `readonly`、`standard`、`privileged`），Scheduler 通过模板名快速指定，无需逐个列举工具
- **运行时权限提升**：Agent 执行中发现需要额外工具时，通过 `permission_request` 协议向 Scheduler 申请临时提权，Scheduler 审批后动态注入工具

**暂不实施的原因**：MVP 阶段 Worker 数量少（默认 1），任务由同一个 Scheduler 中心化分配，风险可控。Shell 命令拦截（黑名单+灰名单）已提供基础安全屏障。优先验证多代理协作的功能正确性。

### 3.8 管理员信赖标记（SourceAdminTrusted）

当前系统所有代理均为内建代理，无"外部代理"概念。

**Why：** 对标 Claude Code 的 `isSourceAdminTrusted` 机制——当系统未来支持用户自定义代理或外部插件代理时，需要区分"可信来源"和"不可信来源"，限制不可信代理的工具访问和资源获取范围。

**未来改进方向**：
- **代理来源标记**：Agent 结构体新增 `Source string`（如 `"system"`、`"user"`、`"plugin"`）和 `Trusted bool` 字段
- **信任级别与工具映射**：不可信代理自动降级为只读工具集，且不注入 mailbox 的 send_message 工具（防止向其他代理注入恶意指令）
- **配合分级权限模型**：信任标记作为权限模板选择的输入之一

**暂不实施的原因**：当前无外部代理接入机制，所有代理由 Bootstrap 内建创建。待引入插件体系或用户自定义 Agent 后再实施。

### 3.9 代理间通信防循环机制（Anti-Loop Guard）

当前邮箱系统允许代理之间无限来回通信。当代理 A 发送含幻觉/错误的请求给代理 B，B 发回质疑，A 再次回复仍有问题，B 再次质疑……形成无意义循环，消耗大量 token 且无法收敛。

**问题场景**：
```
代理A → 代理B: [请求] 含错误/幻觉信息
代理B → 代理A: [question] 要求澄清
代理A → 代理B: [reply] 仍有问题的澄清
代理B → 代理A: [question] 再次要求澄清
... 无限循环 ...
```

**Why：** `question → reply → question → reply` 循环是引入反问机制后的必然副作用。反问机制提升了可靠性，但缺少收敛保障会将 token 浪费在无法解决的分歧上。

**解决方案（分两个阶段）**：

#### 阶段一：硬性来回上限（简单版本）

在邮箱系统中引入 **会话追踪**（conversation tracking），按 `(agentA, agentB)` 配对记录来回次数。

- **最多允许 2 个来回**：即 A→B, B→A, A→B, B→A 共 4 条消息
- **超限后强制收敛**：代理 B 在第二次收到代理 A 的回复后，如仍认为存在问题，则：
  1. 直接拒绝执行，不再继续对话
  2. 向代理 A 发送 `type="reply"` 消息，内容为拒绝原因
  3. 代理 A 收到拒绝消息后不再发起新一轮对话（由邮箱系统在代码层阻止，非 prompt 依赖）
- **实现要点**：
  - `Registry` 维护 `conversationCount map[[2]string]int`，按发送方+接收方排序的配对键计数
  - `Send()` 中检查计数，超限时返回错误（如 `"对话轮次超限，请直接执行或拒绝"`）
  - 计数在任务边界重置（或定时清零）

#### 阶段二：仲裁代理（进阶版本）

对于超过 2 个来回仍无法达成一致的通信，引入 **仲裁代理（Arbitrator）** 进行裁决。

- **触发条件**：`conversationCount` 达到阈值时，不直接阻止，而是将对话升级到仲裁流程
- **仲裁流程**：
  1. 邮箱系统自动收集代理 A 和代理 B 的对话全文
  2. 从公告板获取双方各自正在执行的任务描述作为上下文
  3. 仲裁代理（独立 LLM 调用，类似 ConflictResolver）分析分歧，给出最终裁决
  4. 裁决结果以 `type="steer"`, `from="arbitrator"` 送达代理 B 的信箱
  5. 代理 B 只能选择接受（执行裁决）或拒绝（放弃执行）
  6. 无论代理 B 做出何种选择，结果都通过 `type="reply"` 送达代理 A
  7. 整个仲裁流程不超过 1 轮 LLM 调用，不可递归
- **实现要点**：
  - 新增 `Arbitrator` 组件（复用 `isolation.ConflictResolver` 的 ReAct 模式，但上下文不同）
  - `Registry` 在超限时自动触发仲裁，而非简单阻止
  - 仲裁代理的 system prompt 强调中立性和一次性裁决
  - 仲裁结果带有特殊标记，收信代理的 prompt 引导其尊重仲裁

**暂不实施的原因**：当前 MVP 阶段代理间通信频率低，反问机制刚引入，尚未观测到实际循环问题。待实际运行中确认循环频发后再实施。阶段一可作为快速止血方案，阶段二在多代理自组织（P2-1）落地后更有价值。

### 3.10 团队感知系统（Team Awareness）

与邮箱系统（通信通道）平级的独立子系统，解决的是"代理知道队友是谁、在做什么"的感知问题。邮箱解决"怎么说"，团队感知解决"对谁说、为什么说"。

**当前已实现（MVP 基线）**：
- `BuildTeamSnapshot` 函数：任务开始时调用 `ScanAll()` + `Registry.AllIDs()` 构建 `<team-snapshot>` XML，注入为 LLM 上下文首条消息
- 内容包括：队友 ID、忙碌/空闲状态、正在执行的任务描述（截断 80 字）
- Worker/Explorer system prompt 引导代理根据快照主动通知队友变更、遇到阻塞时直接联系

**未来升级方向**：

#### 3.10.1 动态刷新

当前快照仅在任务开始时注入一次。对于长时间运行的任务，团队状态可能已经发生变化（新代理加入、其他代理完成任务）。

- **定期刷新**：每 N 轮 ReAct 循环重新生成快照并作为新的 `HistoryEntry` 注入（需控制频率避免上下文膨胀，如每 10 轮或 5 分钟）
- **事件驱动刷新**：当收到 `type="ack"` 或 `type="info"` 消息时，在下一轮自动刷新快照（队友状态可能已变）

#### 3.10.2 角色与技能描述

当前快照只包含代理 ID 和任务描述，代理无法判断"谁擅长什么"。

- **Agent 角色标签**：Agent 结构体新增 `Role string`（如 `"code-writer"`、`"test-runner"`、`"investigator"`），在 Bootstrap 时配置
- **快照渲染角色**：`<team-snapshot>` 中展示角色标签，帮助代理判断该联系谁
- **Scheduler 发布任务时标注**：任务可携带 `PreferredRole` 字段，优先匹配对应角色的空闲代理

#### 3.10.3 任务关联感知

当前代理只能看到队友"在做什么"，不知道队友的任务和自己的任务是否存在关联（如修改同一模块、有依赖关系）。

- **文件级关联**：在快照中标注队友正在修改的文件列表（来自 Roster），代理可判断是否存在潜在冲突
- **依赖关联**：如果队友的任务是当前任务的前置或后置依赖，在快照中特别标记
- **共享上下文**：队友任务的 `PartialOutput` 摘要可纳入快照，让代理了解队友的进展而非仅知道任务标题

#### 3.10.4 从感知到自治

团队感知是跨代理自组织的基础设施。当感知足够丰富时，代理可以做出更自主的协作决策：

- **自发任务分流**：Worker 发现自己的任务过大，主动联系空闲队友请求协助（通过 `send_message type="question"`），而非经由 Scheduler 的 `publish_subtask`
- **冲突预防**：Worker 看到队友正在修改同一文件，主动协商分工顺序
- **进度同步**：长任务中周期性向相关队友广播进展，减少 Scheduler 的信息中转负担

**这些改进依赖**：结构化消息类型（已实现）、防循环机制（§3.9，待实现）、分级权限模型（§3.7，待实现）。建议按 3.10.1 → 3.10.2 → 3.10.3 → 3.10.4 的顺序逐步推进。
