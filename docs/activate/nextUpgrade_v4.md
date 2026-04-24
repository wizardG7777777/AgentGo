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

## 7. Hashline 行哈希增强

> 状态：📝 **设计定稿，待实施**（2026-04-19）
> 优先级：P1（解决多 agent 并发编辑的行级漂移痛点）
> 前置依赖：无（hook 系统已稳定、tools 子包已成型）
> 关联：v3 §2-§5 原"行哈希增强"占位（本节为其定稿方案，v3 占位关闭）
> 参考：`oh-my-openagent` 项目的 `HASHLINE_EDIT_INVESTIGATION.zh.md`（2026-04-19 取得）

### 7.1 背景

v3 §2-§5 原计划引入"行哈希增强"，因缺少参考实现而搁置。参考 `oh-my-openagent`（opencode 插件）的调查报告后，方案具备落地条件。本节是 AgentGo 适配版的定稿设计——**校验层叠加，不替换 `edit_file` 的 `old_str/new_str` 编辑模型**，并将护栏机制完整接入 Hook 系统。

**当前 `edit_file` 的三个真实痛点**：

1. **空白字符脆弱性**：`old_str` 要求字节级完全匹配。LLM 稍微吃掉一个缩进、tab/空格混用就匹配失败。为规避此问题 LLM 倾向把 `old_str` 写长以保证唯一性，反而放大空白漂移概率——正反馈失败模式。

2. **`expected_hash` 粒度过粗（多 agent 致命）**：整文件 SHA256 是 binary 信号，任何字节变化都失效。典型场景：worker-1 想改第 42 行（未变），worker-2 修改了第 5 行 → worker-1 的 `expected_hash` 失败 → 被迫重读全文。worker 数量越多这种"无辜失效"越频繁。

3. **缺少"位置 + 内容"双锚点**：`old_str/new_str` 纯内容，无位置；行号纯位置，无内容。LLM 推理时两者都需要，但工具只能表达一个——导致经常认错行（特别是文件中有多个相似块时）。

**Hashline 解决的是什么**：为每行绑定稳定的内容指纹（2 字符哈希），让 LLM 用 `LINE#HASH` 作为既有位置又有内容验证的锚点。多 agent 场景下，**只有 LLM 关心的那几行变了才拒绝编辑**，而非整文件任何变化都拒绝。

### 7.2 设计决策摘要

| 议题 | 决策 | 理由 |
|---|---|---|
| read_file 哈希输出位置 | 工具内格式化，不走 hook | AgentGo hook 语义禁止 Replace（v3 §5.8），postCall 只能观察 |
| edit_file 校验位置 | 新增 `ValidateLineAnchorsHook` | 维持 hook 作为护栏的架构一致性，与 ValidateExpectedHashHook 对称 |
| 两个 hash hook 关系 | 互斥：line_anchors 存在时跳过 expected_hash | 行级保护更细，整文件哈希变为噪声；通过 args 自检实现，hook 不互相通信 |
| RequireReadBeforeWrite 是否升级 | 不升级 | "anchors 是否新鲜"是 ValidateLineAnchorsHook 的职责，每 hook 守一块边界 |
| old_str 含 `N#HH\|` 前缀 | 工具内自动剥离 | 宽容解析，非校验逻辑 |
| 哈希算法 | `hash/crc32` IEEE | 标准库内置、硬件加速、无新依赖 |
| 字典与字符数 | `ZPMQVRWSNKTXJBYH`（16 字母低视觉歧义），2 字符 = 1 字节 | 256 种组合够低碰撞；4 字符是 overkill |
| 空白行 seed | 使用行号作 seed | 避免大量空行同哈希（参考实现） |
| 默认开启？ | 默认开启，`hashline_enabled` 可关 | 哈希前缀对 LLM 可读性几乎无负面，默认开启才能让 LLM 习惯 |
| 参数类型 | `line_anchors []string`（如 `["12#VK","13#QZ"]`） | 与 read_file 输出格式一致，LLM 复制粘贴即可 |
| Hook 命名 | `ValidateLineAnchorsHook`（用户决议） | 直观，与现有 hook 命名风格一致 |
| CRLF/BOM 保真 | **不在本节范围**，单独拆功能 | 用户决议（2026-04-19）——避免 PR 复杂度翻倍 |
| autocorrect 启发式 | **不做，且很长一段时间内都不做** | 用户决议（2026-04-19）——LLM 行为补丁需真实数据驱动 |
| Worker prompt 引导 | **仅靠工具描述**，不改 system prompt | 用户决议（2026-04-19）——新工具描述自说明，prompt 越短越好 |

### 7.3 Hashline 子包设计

新增 `internal/tools/hashline/` 子包，纯函数、无外部依赖：

```go
package hashline

// constants.go
const DictStr = "ZPMQVRWSNKTXJBYH" // 16 个低视觉歧义字母（无 0/O、1/l 等）
// HashLineRefRegex:    ^([0-9]+)#([ZPMQVRWSNKTXJBYH]{2})$
// HashLineOutputRegex: ^([0-9]+)#([ZPMQVRWSNKTXJBYH]{2})\|(.*)$

// hash.go
// ComputeLineHash 返回 2 字符哈希。
//   - 规范化：strip \r，trimEnd whitespace
//   - 空白行（无字母数字）seed=lineNumber，否则 seed=0
//   - 底层 crc32.IEEE → & 0xFF → 映射 DictStr 两次
func ComputeLineHash(lineNumber int, content string) string

// format.go
// FormatHashLine 单行：returns "1#VK|first"
func FormatHashLine(lineNumber int, content string) string
// FormatHashLines 整段：startLine 起每行 FormatHashLine，\n 连接
func FormatHashLines(startLine int, content string) string

// parse.go
type LineRef struct{ Line int; Hash string }
// ParseLineRef 宽容解析：
//   - 剥 ">>>", "+", "-" 前缀
//   - 允许 "42 # VK" 内部空格
//   - 剥 "|content" 尾巴
func ParseLineRef(ref string) (LineRef, error)
// StripHashPrefix 剥 old_str 中的 N#HH| 前缀（≥50% 行带前缀时批量剥离，
// 防误剥真实内容）
func StripHashPrefix(text string) string
```

### 7.4 read_file 输出格式变更

`hashline_enabled=true`（默认）时，输出：

```
[file] foo.go (lines 1-3 of 10)
[hash] <full-file-sha256>
---
1#VK|package main
2#QZ|
3#NP|func main() {
```

关闭时退回当前 `formatReadFileResult` 的纯内容（不带 `N#HH|` 前缀，行号通过 header `(lines X-Y of Z)` 已经表达）。

**实现位置**：`internal/tools/local_read.go` 的 `readFile` 函数，在 `formatReadFileResult` 之前对 `content` 调 `hashline.FormatHashLines`。受 `LocalReadGroup.HashlineEnabled bool` 字段控制（启动时从 cfg 注入）。

**与 FileStateCache 交互**：开关在启动时确定、运行期间不变 → cache 无需改动（缓存的格式化内容自动带前缀）。

### 7.5 edit_file 参数与行为变更

新增可选参数 `line_anchors`：

```go
r.Register("edit_file", "...", 
    schema.Object().
        String("path", "...", true).
        String("old_str", "...", true).
        String("new_str", "...", true).
        String("expected_hash", "...", false).
        StringArray("line_anchors",
            "行哈希锚点列表，如 [\"12#VK\",\"13#QZ\"]。"+
                "提供时 expected_hash 会被忽略；任一行哈希失配则拒绝并返回当前哈希。", false).
        Build(),
    g.editFile,
)
```

**工具内处理**（不变 + 增量）：
1. Hook 链跑完（含 ValidateLineAnchorsHook 的校验）
2. 工具内调 `hashline.StripHashPrefix(oldStr)` 剥离可能的 `N#HH|` 前缀
3. 余下匹配/替换逻辑不变

**注意**：`line_anchors` 仅供 hook 校验使用，工具本身不消费这个参数（除了被 schema 接收）。

### 7.6 ValidateLineAnchorsHook

新建 `internal/hook/builtin/validate_line_anchors.go`：

```go
type ValidateLineAnchorsHook struct{}

func (h *ValidateLineAnchorsHook) Name() string                     { return "validate-line-anchors" }
func (h *ValidateLineAnchorsHook) Phase() hook.ToolHookPhase        { return hook.PhasePreCall }
func (h *ValidateLineAnchorsHook) Priority() int                    { return 25 }
func (h *ValidateLineAnchorsHook) Matches(t string) bool            { return t == "write_file" || t == "edit_file" }
func (h *ValidateLineAnchorsHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision
```

Priority=25 位于 `ValidateExpectedHash`(20) 与 `RequireReadBeforeWrite`(30) 之间。完整 PreCall 链：

```
PathBoundary(10) → ValidateExpectedHash(20) → ValidateLineAnchors(25) → RequireReadBeforeWrite(30)
```

**Run 逻辑**：

1. `args["line_anchors"]` 不存在或为空 → Continue
2. `args["path"]` 缺失/类型错 → Continue（让 PathBoundary 报错）
3. 文件不存在（`os.IsNotExist`）→ Continue（新建豁免，与 ExpectedHash hook 对齐）
4. 其他 ReadFile 错误 → Abort
5. 按行切分文件内容（`strings.Split(content, "\n")`），对每个 anchor：
   - `hashline.ParseLineRef` 解析失败 → 加入 `parseErrors`
   - 行号越界 → 加入 `mismatches`
   - 重算哈希，失配 → 加入 `mismatches`
6. `parseErrors` 或 `mismatches` 非空 → Abort，AbortReason 含格式化消息

**错误消息格式**（参考 oh-my-openagent §4.4，简化为 AgentGo 风格）：

```
行哈希校验失败：1 行自读取以来已改变。请用下方更新后的 LINE#HASH 引用重试（>>> 标记失配行）。

    40#XB|  const config = loadConfig()
>>> 41#QZ|  const user = await getUser()     ← 期望 VK，实际 QZ
    42#NP|  return { config, user }

提示：复用最新 read_file / edit_file 输出里的 LINE#HASH 引用；不要凭记忆构造哈希。
```

`±2` 行上下文 + `>>>` 高亮 + 期望/实际哈希对比，让 LLM 不需要重读全文就能修正。

### 7.7 ValidateExpectedHashHook 互斥改造

`internal/hook/builtin/validate_expected_hash.go` 的 `Run` 入口新增一行：

```go
func (h *ValidateExpectedHashHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
    // 新增：line_anchors 提供时让行级 hook 接管，整文件 hash 退场
    if anchors, ok := hctx.Args["line_anchors"].([]any); ok && len(anchors) > 0 {
        return hook.ToolHookDecision{Action: hook.Continue}
    }
    // ... 其余逻辑不变
}
```

规则一句话：**"提供 line_anchors 时 expected_hash 即使存在也忽略，让行级保护接管。"**

实现采用"两个 hook 各自看 args 自决"，**不通过 hctx 通信**——保持 hook 隔离原则。

### 7.8 配置项

`internal/config/config.go` 新增：

```go
// HashlineEnabled 控制 read_file 是否在输出中附加行哈希前缀（N#HH|content）。
// 开启后 edit_file 可接受 line_anchors 参数做行级校验。
// 用 *bool 区分"未设"与"显式 false"——未设默认 true。
HashlineEnabled *bool `yaml:"hashline_enabled,omitempty" json:"hashline_enabled,omitempty"`
```

`LoadConfig` 默认值处理：未显式设置时 `cfg.HashlineEnabled = ptrTo(true)`。已有 `AgentDeclaration` 用 `*bool` 区分 nil/false 的先例。

Bootstrap 解析后，把 `*cfg.HashlineEnabled` 注入到 `LocalReadGroup.HashlineEnabled`。

### 7.9 实施步骤

| 步骤 | 内容 | 产出 |
|---|---|---|
| S1 | `internal/tools/schema/jsonschema.go` 新增 `StringArray(name, desc, required)` 方法（当前只有 String/Int/Object） | 前置基础设施 |
| S2 | 创建 `internal/tools/hashline/` 子包：constants.go / hash.go / format.go / parse.go + 对应 `_test.go` | 纯函数库 |
| S3 | `internal/tools/local_read.go` 集成哈希输出，受 `LocalReadGroup.HashlineEnabled` 控制 | read_file 新格式 |
| S4 | `internal/tools/local_write.go` `edit_file` schema 加 `line_anchors`；工具内调 `hashline.StripHashPrefix(oldStr)` | edit_file 改造 |
| S5 | 新增 `internal/hook/builtin/validate_line_anchors.go` + `validate_line_anchors_test.go` | 校验 hook |
| S6 | 改 `internal/hook/builtin/validate_expected_hash.go`：`Run` 入口添加 line_anchors 跳过逻辑 + 单测覆盖互斥规则 | 互斥规则 |
| S7 | `internal/bootstrap/bootstrap.go` 注册 `ValidateLineAnchorsHook`；构造 `LocalReadGroup` 时注入 `HashlineEnabled` | 启动集成 |
| S8 | `internal/config/config.go` 新增 `HashlineEnabled *bool`；`LoadConfig` 默认 true；YAML/JSON round-trip 测试 | 配置开关 |
| S9 | 更新 read_file / edit_file 的 schema description 与 register 时的描述文本，明确新格式与 line_anchors 用法 | LLM 指导（仅工具描述） |
| S10 | 单测：哈希稳定性 / 字典覆盖 / 空行 seed / ParseLineRef 宽容（剥 >>> + - 空格 \|content）/ StripHashPrefix 阈值 / Hook 错误消息格式 / 互斥规则 / old_str 前缀剥离 | 回归保证 |
| S11 | 属性测试（pgregory.net/rapid 或 testing/quick）：`ComputeLineHash` 同 normalized-input → 同输出；`FormatHashLine → ParseLineRef` round-trip | 不变量保证 |
| S12 | 文档：`config.example.yaml` 加 `hashline_enabled` 示例；`Archtechture.md` 工具表 + Hook 链图同步；`nextUpgrade_v3.md` §2-§5 占位关闭 | 文档同步 |

### 7.10 不在本节范围

- **CRLF/BOM 保真**：参考实现的 `file-text-canonicalization.ts`（BOM 信封 + \r\n 保真）是独立关注点，单独拆出为下一次小功能。当前 `os.ReadFile/WriteFile` 行为不变。
- **Autocorrect 启发式**：参考实现的 `autocorrect-replacement-lines.ts`（单行多行展开、缩进恢复、整行还原）不做，且"至少从现在开始的很长一段时间都不做"（用户决议 2026-04-19）。
- **位置型操作模型（replace/append/prepend + pos）**：不替换 `edit_file` 现有 `old_str/new_str` 语义。行哈希只做**校验层叠加**，不改编辑模型。
- **批量编辑（edits 数组）**：参考实现允许一次 call 多条 edits + 去重 + 底向上排序。AgentGo 保持单次 edit 语义，多处修改靠多次调用——简单、可组合、与现有 prompt 对齐。
- **Formatter 触发**：参考实现写入后触发用户配置的 formatter（prettier / gofmt 等）。AgentGo 让用户/Worker 自行调 `run_shell`，工具层不承担格式化责任。
- **生成带哈希 diff 输出**：参考实现有 `generateHashlineDiff`。AgentGo 目前 edit_file 只返回"字节变化 +N/-M"，不加这一层。
- **Per-edit 可观测性**：参考实现输出 `additions / deletions / firstChangedLine / noopEdits / deduplicatedEdits` 给 TUI。AgentGo 无 TUI，不加。
- **System prompt 引导**：仅靠工具描述（`schema.String(..., desc, ...)` 与 `r.Register(..., desc, ...)` 文本）让 LLM 自然学会使用，不动 worker / explorer 的 system prompt。

### 7.11 验证计划

| 优先级 | 场景 | 测试方法 | 需要多 worker |
|---|---|---|---|
| V1 | 哈希输出正确性 | 调 read_file，确认输出含 `N#HH\|` 前缀且重复调用哈希稳定 | 否 |
| V2 | 匹配成功路径 | LLM 提供正确 line_anchors 调 edit_file，校验通过 | 否 |
| V3 | 匹配失败路径 | 修改文件后用旧 anchors 调 edit_file，确认 Abort + 错误消息含新哈希 + ±2 行上下文 | 否 |
| V4 | 互斥规则 | 同时提供 line_anchors + expected_hash（expected_hash 故意错）→ 应放行，line_anchors 生效 | 否 |
| V5 | old_str 前缀剥离 | 让 LLM 把读到的 `42#VK\|foo` 整行粘回 old_str，确认仍能匹配 | 否 |
| V6 | 新建文件豁免 | 对不存在文件 write_file + line_anchors → hook 放行 | 否 |
| V7 | 多 agent 粒度优化（核心价值） | WorkerCount=2，两个 worker 读同一文件，A 改第 5 行，B 用行锚点改第 50 行 → 均成功 | **是** |
| V8 | 配置关闭 | `hashline_enabled: false` 时 read_file 退回老格式；edit_file 的 line_anchors 参数仍可用但 LLM 看不到带 hash 输出 | 否 |
| V9 | 属性测试 | `hashline.ComputeLineHash` 在等同 normalized 输入下输出稳定；FormatHashLine ↔ ParseLineRef round-trip | 否 |

V7 是验证本方案核心价值（粒度优化）的关键场景，必须实测通过。

---

## 8. 跨子系统装配的自动化护栏（流程/测试基础设施）

> 状态：📝 规划中（2026-04-19 从 KNOWN_ISSUES.md 迁入）
> 优先级：P2
> 触发来源：2026-04-19 单任务测试同时暴露 3 个"装配漏接"缺陷（Trace CLI 路径脱钩、history.jsonl 断链、Finalization 短路 emit 漏）

### 8.1 问题复盘

同一晚的测试中 3 个缺陷共享同一种失效模式：

| 缺陷 | A 子系统 | B 子系统 | 握手位置 | 单测能否拦截 |
|---|---|---|---|---|
| Trace CLI 路径 | bootstrap | main.go trace 子命令 | 共享 traceDir 常量 | ❌ 否（跨进程入口） |
| history.jsonl 断链 | session.HistoryLog | bootstrap + SessionManager | `SetHistoryEmitter` 调用点 | ❌ 否（bootstrap 无独立单测） |
| Finalization emit | trace | agent.go path B | path B 内的 `trace.Emit` | ❌ 否（emit 是副作用） |

**共同特征**：单元测试覆盖了"零件"，装配环节是手工的、无任何自动化护栏。v3 §9.9 Session 化落地时阶段三标记为 ✅ 完成，实际是"自底向上写完零件 + 各自单测过 → 最后没装配"。类似模式在 2026-04-20 复盘中再次出现（Mail chain_depth 全程为 0—hook 本身已实现但真实路径中从未被深链触发）。

### 8.2 建议落地项

1. **大功能落地必须有一条"端到端烟测"**——以 §9.9 Session 化为例：
   ```
   启动 SessionManager → 跑一个任务 → 关闭 → 断言 history.jsonl 存在且非空
   ```
   5 行能拦截 history.jsonl + task_count 两个问题。每个新增大特性规划 PR 必须同时规划端到端烟测的最小断言。
2. **"完成"定义升级**——目前"代码写完 + 单测过"算 ✅；应再加一道门槛："实际启动跑一次验证产物符合预期"。建议把启动-产物断言纳入 CI。
3. **约定事件对称性的 lint 级检查**——任何"终结类" return 出口前必须 emit 对应 trace 事件（如 `KindTaskCompleted`）。用代码扫描式测试守住（扫 `agent.go` 的 return，每个都要在 M 行内看到对应 emit），比靠 reviewer 肉眼检查更可靠。
4. **红态测试转绿作为修复门槛（2026-04-20 补充经验）**——缺陷发现时优先落地会失败的回归测试（参考 `path_boundary_test.go` 2026-04-20 红态测试模式），修复完成前该测试必须转绿，PR 内同步更新 KNOWN_ISSUES 对应条目状态，避免"代码修完但文档仍标为待修复"的漂移（2026-04-20 核查发现 7 项已修复缺陷文档未同步）。

### 8.3 落地优先级

- 先做 §8.2 第 4 条（零工作量，只是流程约定）
- 再做 §8.2 第 3 条（一次性代码扫描测试，可守护 agent.go 的所有终结 return）
- §8.2 第 1/2 条涉及 CI 基础设施，可与 §7 Hashline 实施同期起草

---

## 9. Bootstrap 阶段 LLM 连通性检查（快速失败）

> 状态：📝 规划中（2026-04-20 触发记录，具体方案待讨论）
> 优先级：P2
> 触发来源：2026-04-20 手工验证寄生唤醒修复时，LLM 服务器 `192.168.1.117:8080` 恰好关机，系统进入 scheduler 无限重试循环 166+ 次（wall-clock ~25 分钟）才手动 Ctrl+C 终止——完全避免本可以在启动阶段一次 TCP 探测就阻止的浪费。

### 9.1 问题描述

当前 [internal/bootstrap/bootstrap.go](../../internal/bootstrap/bootstrap.go) 初始化 LLM client（[llm.NewClient](../../internal/llm/client.go)）只检查配置字段是否齐全，**不验证 `llm_base_url` 是否真正可达**。若启动时 LLM 服务不可达：
- 启动阶段打印"系统就绪，等待用户输入"，看起来一切正常
- 直到用户提交第一个 prompt，scheduler 才发起 LLM 调用
- 调用以 `dial tcp: connect: connection refused` 失败
- `handleFailure` → `RetryRollback`；scheduler `MaxRetries=0` → 无限重试
- 日志屏幕刷"重试 #N，恢复 N 条历史记录"直到手动终止

用户视角：**启动成功 ≠ 真正可用**。这是"绿色说谎"——启动提示说就绪，但核心依赖其实断了。

### 9.2 设计方向（待讨论）

核心诉求：**启动阶段探测 `llm_base_url` 可达性；不可达时打印明确警告 + 终止启动流程**，而非让问题下推到运行时。

可选探测方式（具体采哪种在未来专项讨论）：
- **最轻**：TCP 层 `net.DialTimeout` 到 `host:port`，只要 3-way handshake 成功即视为可达
- **较强**：HTTP GET `{base_url}/models` 或 `{base_url}/chat/completions` 空请求，期望 2xx/4xx（不期望 5xx/connection error）
- **最强**：真实发一条 `messages=[{role:"user",content:"ping"}]` 的极短 chat completion，验证模型名可用

权衡：越强的探测越慢、越可能被鉴权/限流误判；越轻的探测越可能漏过"能 TCP 通但接口错"的故障。

可选失败处理：
- **硬失败**：直接 `os.Exit(1)` 带错误消息
- **软失败**：启动阶段只发警告，允许 `--skip-llm-probe` 绕过（开发环境 mock LLM 时用）
- **配置默认**：`startup_llm_probe: "tcp"` / `"models"` / `"off"`

警告内容必须包含的信息（明显 actionable）：
- 实际探测失败的 URL
- 错误类型（连接拒绝 / 超时 / 鉴权失败 / 5xx）
- 常见原因清单（"LLM 服务未启动 / base_url 配置错 / 网络隔离"）
- 如何绕过（如果提供 skip 选项）

### 9.3 落地价值

- **节省 wall-clock**：本次事故 25 分钟的无效循环一秒内可避免
- **节省 token 预算**：LLM 真正接通时第一次失败也会产生数百次重试费用（虽然本次恰好连不上所以没烧）
- **降低排查成本**：一次性在启动阶段给出明确错误，比翻 99 个 trace 文件更直接
- **与 §8 装配护栏同源**：本质是"启动 = 代码路径通了 ≠ 运行时依赖通了"，应当有一道"实际探测一次"的门槛

### 9.4 关联条目

- 与 §8 同属"启动阶段的装配/连通性烟测"大类，可合并为一套 `StartupProbe` 框架
- 连带解决 KNOWN_ISSUES.md 同批次新发现的"Scheduler 网络失败无限重试"老 bug（其属于运行时处理层面，与本章的启动时探测互补）

---

## 10. 检索工具主动反馈（Did-You-Mean 候选提示）

> 状态：📝 规划中（2026-04-24 触发记录）
> 优先级：P2
> 触发来源：2026-04-23 实战日志暴露 Explorer 连吃 5 次 grep_search 空结果 result_len=18；2026-04-24 已完成"诊断消息层"修复（工具行为说明），但用户提出应进一步叠加"意图拯救层"——空结果时主动给近似匹配候选，让 Agent 一次看到"Did you mean X?"直接换用正确参数。

### 10.1 背景与定位

2026-04-24 已完成 grep_search / glob_search 空结果的**行为说明层**：告诉 LLM "扫描了多少文件、跳过了什么、pattern 是字面子串非正则"。这解决了"工具为什么返回空"的问题。

本节要叠加的是**意图拯救层**：在 LLM 拼写打错、命名记混、路径层级猜错时，工具自己基于"当前目录下实际存在什么"给出 Top-K 近似候选，让 LLM 一眼看到"`Archtechture.md` 不存在，但有 `Architecture.md`——你是不是想找这个？"。

两层的关系：**行为说明**（现在的诊断）+ **意图拯救**（本节）= 完整的"空结果恢复信息"。前者回答"为什么 0"，后者回答"你或许真正想要的是什么"。

### 10.2 覆盖范围的分层

按实现复杂度 / 收益比排序，建议 MVP 先做前三个，grep_search 延后：

| 工具 | 候选空间 | 实现成本 | MVP 包含 |
|---|---|---|---|
| **glob_search** | 本次 Walk 已扫的所有相对路径（≤10k 条） | 低（复用 Walk 结果） | ✅ |
| **read_file** | 失败路径的父目录 ReadDir 列表 | 极低（1 次 syscall） | ✅ |
| **list_dir** | 失败路径的父目录 + 上两级 ReadDir | 低（≤3 次 syscall） | ✅ |
| **grep_search** | 扫描文件的 token/标识符（百万量级） | 高（需建 trigram 索引 + 去噪） | ❌（暂缓） |

grep_search 候选空间是"扫过的所有文件中出现的 token"，典型 Go 项目百万级。做好需要 trigram/BK-tree 索引和按 token 频率去噪（不能把 `the` `func` 当候选）。等前三项上线收集实战数据后再决定是否启动。

### 10.3 算法选择（MVP）

**决策：采用 [`github.com/sahilm/fuzzy`](https://github.com/sahilm/fuzzy) 作为模糊匹配底座，不自造轮子。**

理由：
- fzf 风格子序列匹配 + 连续性/位置加权打分，**直接就是 Did-You-Mean 语义**——API 一步到位：`fuzzy.Find(pattern, candidates) Matches`，返回已按 score 排序的匹配列表
- 真实采用方包括 fzf、lazygit 等，参数调优经过大量真实用户打磨（连续命中加权、边界加权、大小写策略等），自写必定复现不到这个成熟度
- 单一依赖、零传递依赖、约 400 行代码可审计
- 副产物 `Match.MatchedIndexes []int` 可以用来在输出里**高亮命中字符**（如 `m*u*lti*A*gent_upgrade_plan.md`），比"编辑距离=2"的数字化 reason 对 LLM 更直观

trade-off 认知：
- fzf 风格对**纯拼写错乱序**（`Archtechture` ↔ `Architecture`——字母顺序变了）的打分会弱于 Levenshtein。但 Did-You-Mean 的候选池通常 << 1000 条，真候选即使 score 偏低仍会排进 Top-3；实战数据不佳时可叠加一层 Levenshtein 兜底，但 **MVP 阶段只用 sahilm/fuzzy**
- 不引入 [`agnivade/levenshtein`](https://github.com/agnivade/levenshtein) 作为补充——原则上一个问题只挑一个库，避免同类能力重复依赖；fzf 风格覆盖 80% 用例已经足够

不纳入的算法选项（决策记录）：
- **自写 Levenshtein + 子串匹配**：错误的自举冲动，距离阈值 / 子串长度下限 / CJK rune 边界等细节坑已被上游库填过
- **[`lithammer/fuzzysearch`](https://github.com/lithammer/fuzzysearch)**：同类功能但 API 不如 sahilm/fuzzy 成熟，采用方少
- **Token 分词 + 共享 token 计数**：MVP 不做，等 sahilm/fuzzy 实战有证据不够用再叠加
- **Trigram 索引**：扫描目标 > 10k 时才必要，MVP 阶段线性扫描 O(N) 够用

### 10.4 候选构造范围（避免误导）

关键原则：**候选必须来自"LLM 看得到的、实际存在的"实体**，不能跨越它当前查询的范围去瞎提示。

- **glob_search**：候选 = 本次 Walk 已扫到的所有路径（工具已经有这份列表），对 pattern 的"文件名主干"（去掉 `**/`、`*`、`.ext`）做相似度。pattern 是 `**/Archtechture.md` → 主干是 `Archtechture.md` → 找相似文件名
- **read_file**（path 错）：候选 = `filepath.Dir(path)` 下的文件列表。**不跨目录提示**，否则 `internal/foo/x.go` 不存在时建议 `cmd/bar/x.go` 会误导
- **list_dir**（path 错）：候选 = `Dir(path)` 下的目录 + `Dir(Dir(path))` 下的目录（覆盖"层级猜错一层"）

### 10.5 输出格式

空结果消息在现有诊断基础上追加一段（仅当 sahilm/fuzzy 返回非空 Matches 时——由库内部的子序列匹配语义自然筛出"字符全命中"的候选，MVP 阶段不额外加 score 阈值）：

```
未找到匹配 "Archtechture.md" 的文件（扫描 128 个文件，根目录=.，隐藏目录已跳过）。
若意外为空：1) 确认 pattern 符合 glob 语法（** 递归、* 单层、? 单字符）；...

Did you mean:
  - Arch[i]tecture.md
  - [arch]itecture_v3.md
  - docs/[arch]itecture/overview.md
```

**高亮策略**：用 sahilm/fuzzy 返回的 `Match.MatchedIndexes` 在每段连续命中字符两侧加 `[...]`（方括号配对），直观展示"为什么推荐这条"。**不用 `*` 或 `**`** —— 前者在 markdown 渲染上下文会被当作斜体指令，后者是粗体，都容易与候选本身的字面字符混淆；方括号成对出现语义明确、与文件名字符冲突概率低。

约束：
- 最多 3 条候选（对应 `suggest.Suggest(pattern, candidates, 3)`）
- **高亮完整性优先于长度省略**：候选本身展示必须包含全部 MatchedIndexes 对应的字符——超过 80 字符时允许在**首个命中字符之前**或**末个命中字符之后**做 `...` 截断（如 `.../nested/[arch]itecture/overview.md`），但决不能在命中段内省略
- **不设额外 score 阈值**：sahilm/fuzzy 返回的 Matches 本身就是"至少能完成子序列匹配"的集合，空列表即"实在没有合理候选"；MVP 不再叠加 `minScore`，等实战出现"低质候选刷屏"证据再调
- 调用方逻辑：`matches := fuzzy.Find(pattern, candidates); if len(matches) == 0 { 不追加 Did-you-mean 段 }`

### 10.6 性能

- **glob_search**：Walk 已扫过的路径列表已有现成的，调 `fuzzy.Find(pattern, paths)` 复杂度约 O(N·L²)（fzf 风格子序列匹配最差情况，N=候选数、L=平均候选长度），**预期 < 20ms（N=10k, L=30），以 S7 benchmark 实测为准**——这是预期值不是已知 benchmark 数
- **read_file / list_dir**：仅失败路径触发一次 `os.ReadDir`，成功路径零开销
- **MVP 不建索引**：纯线性扫描足够。若 S7 benchmark 显示 N > 10k 时延迟感知明显，再考虑换库或叠加前置筛选（如按首字符桶分组缩小 N）——sahilm/fuzzy 本身不提供索引选项

### 10.7 与 Hook 系统的关系

检索工具的反馈应当分为**两层叠加**，各自覆盖不同的职责：

| 层 | 归属 | 职责 | 本节范围 |
|---|---|---|---|
| **第一层：工具自身反馈** | 工具函数内部 | 主动给 Agent 第一手帮助——诊断消息说明工具行为（2026-04-24 已完成）；空结果给出 Did-you-mean 候选（本节） | ✅ 本节实施 |
| **第二层：Tool Hook 兜底** | Hook 系统 | 在错误参数 / 越界路径 / 违反契约时拦截——path-boundary 校验 path 字段（2026-04-24 已修复字段分派） | ⛔ 本节不改动 hook |

两层是**叠加**而非互斥：工具在 happy path 给出最有用的上下文，hook 在调用层面守住边界。同一次失败可能只触发其中一层（如 pattern 拼错导致空结果——工具层 Did-you-mean 接管；path 越界——hook 层拦截）。

Did-you-mean 归到工具自身反馈层的具体理由：
- 候选构造需要**工具内部状态**（glob_search 的 Walk 结果、read_file 的 ReadDir）——hook 从 args 层面拿不到这些
- 与现行 v3 §5.8 决议一致：hook 不做 Replace，不改写工具返回值；"给返回值加 Did-you-mean 段"本质是增强返回值，不是拦截
- Hook 保持"护栏"单一职责，工具保持"结果质量"单一职责，两层分工清晰不越界

### 10.8 共享基础设施

抽 `internal/tools/suggest/` 子包做**薄适配层**——算法全部由 `sahilm/fuzzy` 承担，本子包只负责"候选构造约束 + 命中字符高亮 + 输出格式化"：

```go
package suggest

import "github.com/sahilm/fuzzy"

// Suggest 对 candidates 按 fzf 风格子序列匹配打分，返回 Top-K 高亮标记后的字符串。
// - pattern: LLM 传入的查询（如 "Archtechture.md"）
// - candidates: 当前查询范围内实际存在的实体（glob_search 的 Walk 结果 / ReadDir 列表）
// - k: 最多返回几条（建议 3）
// 返回空切片表示无合理候选（调用方应不显示 "Did you mean" 段）。
func Suggest(pattern string, candidates []string, k int) []string

// highlightMatch 用 fuzzy.Match.MatchedIndexes 在每段连续命中字符两侧加 [...] 方括号，
// 超长路径（>80 字符）时在首个命中字符之前 / 末个命中字符之后截断，
// 绝不在命中段内省略（高亮完整性优先，见 §10.5）。
func highlightMatch(m fuzzy.Match) string

// FormatForToolMessage 把 Suggest 返回的高亮候选列表格式化为
// "Did you mean:\n  - Arch[i]tecture.md\n  - ..." 文本段；空切片返回 ""。
func FormatForToolMessage(highlighted []string) string
```

整个子包预计 ≤ 80 行代码 + 对应单测。**所有距离/打分/排序逻辑委托给 sahilm/fuzzy**，本子包不出现任何算法实现。

### 10.9 实施步骤

| 步骤 | 内容 |
|------|------|
| S1 | `go get github.com/sahilm/fuzzy@latest`，确认无传递依赖污染 go.mod |
| S2 | 新增 `internal/tools/suggest/` 子包：`suggest.go`（Suggest + highlightMatch + FormatForToolMessage）+ `suggest_test.go`；总代码 ≤ 80 行 |
| S3 | `local_read.go` globSearch 空结果路径：用 Walk 过的 paths 列表调 suggest.Suggest |
| S4 | `local_read.go` readFile 路径不存在路径：调 os.ReadDir(Dir(path)) 后 suggest.Suggest |
| S5 | `local_read.go` listDir 路径不存在路径：父目录 + 上两级目录合并后 suggest.Suggest |
| S6 | 单测：各工具空结果/路径不存在场景，断言输出含"Did you mean"+ 正确候选 + 高亮标记 |
| S7 | 性能基准：10k 文件 glob_search 失败路径耗时 benchmark，确认 < 20ms |
| S8 | KNOWN_ISSUES.md 登记本节作为"检索工具主动反馈"后续项，观察实战效果 |

### 10.10 不在本节范围

- **自写模糊匹配算法**：sahilm/fuzzy 已覆盖 fzf 风格子序列匹配；不叠加 agnivade/levenshtein 做编辑距离补充（一个问题只挑一个库，避免同类能力重复依赖）
- **grep_search 候选**：候选空间（token 集合）数量级差两三个数量级，需要独立的"token 提取 + 频率去噪 + 索引"方案。MVP 期待"实战证实 grep_search 空结果误用严重"后再单独启动
- **语义相似度（向量/嵌入）**：语义"你是不是想找 authentication？" vs `login`——需要 embedding 模型。当前 MVP 保持纯字符串算法，不引入 embedding 依赖
- **跨工具自动切换建议**（"你用了 grep_search 但或许 glob_search 更合适"）：需要对任务意图做推断，过早且容易误导
- **多语言分词**：sahilm/fuzzy 按 rune 匹配，对中英混合路径够用；不引入专门的中日韩分词器

### 10.11 验证计划

| 优先级 | 场景 | 测试方法 |
|---|---|---|
| V1 | glob_search 拼写错一字母（`Archtechture` ↔ `Architecture`）→ 候选正确 + 输出含 `[...]` 方括号高亮所有命中段 | 单测：临时目录放正确文件，pattern 用错拼写；断言候选字符串 + MatchedIndexes 对应区间均被方括号包住 |
| V2 | glob_search 只记得一半（`multi_agent` ↔ `multiAgent_upgrade_plan.md`）→ 候选正确（验证 fzf 子序列匹配场景） | 单测 |
| V3 | read_file 路径层级错（`docs/Architecture.md` 实际 `docs/architecture/Architecture.md`）→ 候选为父目录下同名 | 单测 |
| V4 | list_dir 层级错 → 候选含上级目录 | 单测 |
| V5 | 完全无相似候选 → 输出不含 "Did you mean"，避免噪声 | 单测 |
| V6 | 性能基准 → 10k 文件 < 20ms | benchmark |
| V7 | 实战：重跑 2026-04-23 类似调查任务，观察 Explorer 是否在第一次空结果后直接用候选纠错 | 手工烟测 |
| V8 | **已知弱点验证**：纯字母乱序拼写错（`Archtechture` 其实是 transpose `te` ↔ `ech`）fuzzy 打分可能偏低，确认仍排进 Top-3；若实战失败则记录为"v2 可能叠加 Levenshtein 兜底"的触发信号 | 单测 + 实战观察 |

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
| §7 | Hashline 行哈希增强 | **P1** | 无 | 📝 设计定稿（2026-04-19），待实施 |
| §8 | 跨子系统装配护栏 | P2 | 无 | 📝 规划中（2026-04-19） |
| §9 | Bootstrap LLM 连通性检查 | P2 | 无 | 📝 规划中（2026-04-20，方案待讨论） |
| §10 | 检索工具主动反馈（Did-You-Mean） | P2 | 无 | 📝 设计定稿（2026-04-24），底座选定 `sahilm/fuzzy`，待实施 |

---

## 11. 统一 Agent 声明式配置（v4 配置格式重写）

> 状态：📝 设计定稿（2026-04-24）
> 优先级：P1
> 触发来源：v3/v4 早期配置字段扁平分散、全局共享，无法支持 per-agent 差异化行为参数
> 约束：**不向后兼容**——v4 配置格式为全新 schema，不保留旧字段 fallback

### 11.1 背景

当前 `setting.yaml` 的配置结构是**扁平全局式**的：

```yaml
# v3 现状（问题示意）
agent_max_loops: 10                    # ← 所有 Agent 共享
compact_token_threshold: 4000          # ← 所有 Agent 共享
worker_count: 3                        # ← 仅 Worker 有数量概念
worker_profile: "standard"             # ← 仅 Worker 有 Profile
explorer_profile: "explorer_default"   # ← Explorer 单独字段
```

这导致三个结构性问题：

1. **行为参数全局共享**：Scheduler 需要更多轮次做任务拆解（`max_loops: 15`），Explorer 只需要调查（`max_loops: 5`），但当前只能共用同一个 `agent_max_loops`。
2. **提示词硬编码**：`systemPrompt` 是各 package 的 `const`，无法针对不同场景微调（如"严格代码审查者" vs "激进开发者"）。
3. **上下文管理缺失**：没有 `context_limit` 概念，Agent 历史长度无上限，长任务会溢出模型上下文窗口导致 400/413 错误。

### 11.2 设计原则

1. **Agent 类型即一级公民**：Scheduler、Explorer、Worker 在配置中用统一的 schema 描述行为与部署。
2. **模板复用 + 实例覆盖**：公共行为参数抽成 `agent_templates`，实例通过 `template` 引用 + `params_override` 局部覆盖。
3. **上下文上限内建**：`context_limit` 不是可选装饰，而是核心运行时参数，配套 token 估算与截断策略。
4. **不向后兼容**：v4 配置格式为全新文件，旧字段全部移除。升级时人工迁移一次即可，避免代码中充斥 fallback 逻辑。

### 11.3 v4 YAML 配置格式（完整示例）

```yaml
# ============================================================
# v4 setting.yaml — 统一 Agent 声明式配置
# ============================================================

# --- 基础设施层（全局共享，可被 per-agent 覆盖）---
llm:
  base_url: https://api.deepseek.com
  api_key: ${DEEPSEEK_API_KEY}
  default_model: deepseek-v4-flash
  default_provider: deepseek-v4
  default_temperature: 0.2
  timeout_sec: 120

# --- 工具集定义（命名白名单）---
tool_profiles:
  standard:
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

  readonly:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - send_message

  explorer_default:
    - read_file
    - list_dir
    - grep_search
    - glob_search
    - web_search
    - web_fetch
    - send_message

# --- Agent 行为模板（复用层）---
# 模板只定义行为参数，不绑定工具集。多个 Agent 可引用同一模板。
agent_templates:
  scheduler_default:
    max_loops: 15
    compact_token_threshold: 6000
    compact_keep_recent: 3
    context_limit: 24000
    temperature: 0.1
    system_prompt: |
      你是任务调度器，负责分析用户需求、拆解为可执行的子任务，
      并分配给合适的执行代理。你拥有全局视角，可以查看所有代理的状态和工具集。

  explorer_default:
    max_loops: 5
    compact_token_threshold: 3000
    compact_keep_recent: 2
    context_limit: 8000
    temperature: 0.0
    system_prompt: |
      你是代码调查员，只能读取文件和搜索代码，绝对不能修改任何文件。
      你的任务是通过阅读代码理解项目结构、定位相关逻辑、总结发现。

  worker_standard:
    max_loops: 10
    compact_token_threshold: 4000
    compact_keep_recent: 3
    context_limit: 16000
    temperature: 0.2
    system_prompt: |
      你是全能开发者，可以读写文件、执行 Shell、搜索网络、与其他代理协作。
      遵循用户的编码规范，修改前充分理解上下文。

  worker_readonly:
    max_loops: 5
    compact_token_threshold: 2000
    compact_keep_recent: 2
    context_limit: 8000
    temperature: 0.0
    system_prompt: |
      你是只读调查员，只能查看代码和搜索结果，无权修改文件或执行命令。
      你的输出应当是结构化的调查报告。

# --- Agent 实例声明（核心）---
# 每个 Agent 类型在此处声明其实例数量、引用模板、工具集，以及可选覆盖
agents:
  scheduler:
    count: 1                    # Scheduler 必须为 1，Bootstrap 校验
    template: scheduler_default
    profile: standard           # Scheduler 也需要工具集（publish_task 等）
    # Scheduler 通常不需要覆盖，但保留 params_override 能力

  explorer:
    count: 1                    # Explorer 当前限制为 1
    template: explorer_default
    profile: explorer_default

  workers:
    # Worker 支持两种声明模式（互斥）：
    # 模式 A：同质批量（简单场景）
    #   count: 3
    #   template: worker_standard
    #   profile: standard
    # 模式 B：异质列表（精细化场景，优先）
    instances:
      - id: worker-1
        template: worker_standard
        profile: standard
      - id: worker-2
        template: worker_standard
        profile: standard
      - id: worker-3
        template: worker_readonly
        profile: readonly
        params_override:
          max_loops: 3
          system_prompt: |
            你是只读调查员，绝对不能修改文件。
            当前任务涉及敏感数据库代码，请格外谨慎。

# --- 运行时基础设施（非 Agent）---
infra:
  watchdog:
    interval_sec: 30

  mail_notifier:
    enabled: true
    interval_sec: 60

  store:
    event_channel_buffer: 256
    fifo_limit: 100
    default_concurrency: 3

  roster:
    wait_timeout_sec: 300

# --- 杂项 ---
project_root: "."
max_subtask_depth: 3
shell_timeout_sec: 60
shell_blacklist: []
shell_greylist: []
search_api_provider: serper
search_api_url: https://google.serper.dev/search
search_api_key: ${SERPER_API_KEY}
```

### 11.4 Go 配置结构体

```go
package config

// ============================================================
// v4 Config — 全新结构，不兼容 v3
// ============================================================

type Config struct {
    // --- LLM 基础设施（全局默认值）---
    LLM LLMConfig `yaml:"llm" json:"llm"`

    // --- 工具集定义 ---
    ToolProfiles map[string][]string `yaml:"tool_profiles" json:"tool_profiles"`

    // --- 行为模板（复用层）---
    AgentTemplates map[string]AgentTemplate `yaml:"agent_templates" json:"agent_templates"`

    // --- Agent 实例声明 ---
    Agents AgentDeclarations `yaml:"agents" json:"agents"`

    // --- 运行时基础设施 ---
    Infra InfraConfig `yaml:"infra" json:"infra"`

    // --- 杂项 ---
    ProjectRoot       string `yaml:"project_root" json:"project_root"`
    MaxSubtaskDepth   int    `yaml:"max_subtask_depth" json:"max_subtask_depth"`
    ShellTimeoutSec   int    `yaml:"shell_timeout_sec" json:"shell_timeout_sec"`
    ShellBlacklist    []string `yaml:"shell_blacklist" json:"shell_blacklist"`
    ShellGreylist     []string `yaml:"shell_greylist" json:"shell_greylist"`
    SearchAPIProvider string `yaml:"search_api_provider" json:"search_api_provider"`
    SearchAPIURL      string `yaml:"search_api_url" json:"search_api_url"`
    SearchAPIKey      string `yaml:"search_api_key" json:"search_api_key"`
}

// LLMConfig 全局 LLM 默认值
 type LLMConfig struct {
    BaseURL            string  `yaml:"base_url" json:"base_url"`
    APIKey             string  `yaml:"api_key" json:"api_key"`
    DefaultModel       string  `yaml:"default_model" json:"default_model"`
    DefaultProvider    string  `yaml:"default_provider" json:"default_provider"`
    DefaultTemperature float64 `yaml:"default_temperature" json:"default_temperature"`
    TimeoutSec         int     `yaml:"timeout_sec" json:"timeout_sec"`
}

// AgentTemplate 行为参数模板（无 ID、无 Profile，纯行为）
 type AgentTemplate struct {
    MaxLoops              int     `yaml:"max_loops" json:"max_loops"`
    CompactTokenThreshold int     `yaml:"compact_token_threshold" json:"compact_token_threshold"`
    CompactKeepRecent     int     `yaml:"compact_keep_recent" json:"compact_keep_recent"`
    ContextLimit          int     `yaml:"context_limit" json:"context_limit"`
    Temperature           float64 `yaml:"temperature" json:"temperature"`
    SystemPrompt          string  `yaml:"system_prompt" json:"system_prompt"`
}

// AgentDeclarations 各类型 Agent 的部署声明
 type AgentDeclarations struct {
    Scheduler SchedulerDecl `yaml:"scheduler" json:"scheduler"`
    Explorer  ExplorerDecl  `yaml:"explorer" json:"explorer"`
    Workers   WorkerDecl    `yaml:"workers" json:"workers"`
}

// SchedulerDecl Scheduler 只有一个实例
 type SchedulerDecl struct {
    Count          int            `yaml:"count" json:"count"`
    Template       string         `yaml:"template" json:"template"`
    Profile        string         `yaml:"profile" json:"profile"`
    ParamsOverride AgentTemplate  `yaml:"params_override,omitempty" json:"params_override,omitempty"`
    LLMOverride    *LLMOverride   `yaml:"llm_override,omitempty" json:"llm_override,omitempty"`
}

// ExplorerDecl Explorer 只有一个实例
 type ExplorerDecl struct {
    Count          int            `yaml:"count" json:"count"`
    Template       string         `yaml:"template" json:"template"`
    Profile        string         `yaml:"profile" json:"profile"`
    ParamsOverride AgentTemplate  `yaml:"params_override,omitempty" json:"params_override,omitempty"`
    LLMOverride    *LLMOverride   `yaml:"llm_override,omitempty" json:"llm_override,omitempty"`
}

// WorkerDecl Worker 支持同质批量或异质列表
 type WorkerDecl struct {
    // 模式 A：同质批量（instances 为空时使用）
    Count    int    `yaml:"count,omitempty" json:"count,omitempty"`
    Template string `yaml:"template,omitempty" json:"template,omitempty"`
    Profile  string `yaml:"profile,omitempty" json:"profile,omitempty"`

    // 模式 B：异质列表（优先）
    Instances []WorkerInstance `yaml:"instances,omitempty" json:"instances,omitempty"`
}

// WorkerInstance 单个 Worker 实例声明
 type WorkerInstance struct {
    ID             string         `yaml:"id" json:"id"`
    Template       string         `yaml:"template" json:"template"`
    Profile        string         `yaml:"profile" json:"profile"`
    ParamsOverride AgentTemplate  `yaml:"params_override,omitempty" json:"params_override,omitempty"`
    LLMOverride    *LLMOverride   `yaml:"llm_override,omitempty" json:"llm_override,omitempty"`
}

// LLMOverride per-agent LLM 参数覆盖
 type LLMOverride struct {
    Model       *string  `yaml:"model,omitempty" json:"model,omitempty"`
    Provider    *string  `yaml:"provider,omitempty" json:"provider,omitempty"`
    Temperature *float64 `yaml:"temperature,omitempty" json:"temperature,omitempty"`
}

// InfraConfig 非 Agent 运行时基础设施
 type InfraConfig struct {
    Watchdog struct {
        IntervalSec int `yaml:"interval_sec" json:"interval_sec"`
    } `yaml:"watchdog" json:"watchdog"`

    MailNotifier struct {
        Enabled     bool `yaml:"enabled" json:"enabled"`
        IntervalSec int  `yaml:"interval_sec" json:"interval_sec"`
    } `yaml:"mail_notifier" json:"mail_notifier"`

    Store struct {
        EventChannelBuffer int `yaml:"event_channel_buffer" json:"event_channel_buffer"`
        FIFOLimit          int `yaml:"fifo_limit" json:"fifo_limit"`
        DefaultConcurrency int `yaml:"default_concurrency" json:"default_concurrency"`
    } `yaml:"store" json:"store"`

    Roster struct {
        WaitTimeoutSec int `yaml:"wait_timeout_sec" json:"wait_timeout_sec"`
    } `yaml:"roster" json:"roster"`
}
```

### 11.5 字段语义与默认值

#### 11.5.1 配置解析流程（Bootstrap）

```
1. 加载 YAML → Config 结构体
2. 校验阶段：
   - agent_templates 中引用的模板名必须存在
   - tool_profiles 中引用的 profile 名必须存在
   - tool_profiles 中的工具名必须在系统注册的全集内
   - scheduler.count == 1，explorer.count == 1（超限报错）
3. 实例化阶段：
   - 对每个 Agent 实例，按以下优先级合并参数：
     a) AgentTemplate（基础层）
     b) ParamsOverride（覆盖层）
     c) LLMConfig 全局默认值（LLM 参数未覆盖时）
   - 生成最终的 AgentRuntimeConfig 注入到构造函数
```

#### 11.5.2 默认约定

| 字段 | 无 Template 时 | 无 ParamsOverride 时 |
|------|---------------|---------------------|
| `max_loops` | 10 | 继承 Template |
| `compact_token_threshold` | 4000 | 继承 Template |
| `compact_keep_recent` | 3 | 继承 Template |
| `context_limit` | **16000** | 继承 Template |
| `temperature` | 0.2 | 继承 Template |
| `system_prompt` | 使用内置 package 默认值 | 继承 Template |

**特殊规则**：`system_prompt` 如果全部为空（无 Template、无 Override、无内置），Bootstrap 阶段报错——Agent 不能没有系统提示。

#### 11.5.3 Worker 两种声明模式的互斥规则

```go
func (d WorkerDecl) ResolveInstances() ([]WorkerInstance, error) {
    if len(d.Instances) > 0 {
        // 模式 B 优先：按 instances 列表启动
        return d.Instances, nil
    }
    if d.Count > 0 {
        // 模式 A：按 count 生成同质实例
        instances := make([]WorkerInstance, d.Count)
        for i := range d.Count {
            instances[i] = WorkerInstance{
                ID:       fmt.Sprintf("worker-%d", i+1),
                Template: d.Template,
                Profile:  d.Profile,
            }
        }
        return instances, nil
    }
    return nil, fmt.Errorf("workers 必须配置 count 或 instances")
}
```

### 11.6 Agent 启动与配置解析流程

```go
// Bootstrap 中的新流程（伪代码）

// 1. 构建 Agent 运行时配置工厂
factory := NewAgentConfigFactory(cfg)

// 2. Scheduler（单例）
schedCfg := factory.BuildSchedulerConfig()
sched := scheduler.New(..., schedCfg)

// 3. Explorer（单例）
expCfg := factory.BuildExplorerConfig()
exp := explorer.New(..., expCfg)

// 4. Workers（多例）
workerInstances := cfg.Agents.Workers.ResolveInstances()
for _, inst := range workerInstances {
    wkCfg := factory.BuildWorkerConfig(inst)
    wk := worker.NewWithID(inst.ID, ..., wkCfg)
}
```

**关键改动**：`worker.NewWithID`、`explorer.New`、`scheduler.New` 的签名不再接收零散的 `int` / `string` 参数，而是接收一个**聚合的配置对象**（`AgentRuntimeConfig`），减少构造函数参数爆炸。

```go
// AgentRuntimeConfig 运行时聚合配置（内部使用，不出现在 YAML 中）
type AgentRuntimeConfig struct {
    ID                    string
    EventType             string
    MaxLoops              int
    CompactTokenThreshold int
    CompactKeepRecent     int
    ContextLimit          int
    Temperature           float64
    SystemPrompt          string
    AllowedTools          []string        // 从 Profile 解析
    LLM                   LLMClientConfig // 合并后的 LLM 参数
}
```

### 11.7 Context Limit 与 Token 消耗追踪策略

`context_limit` 是 v4 配置格式的核心新增能力。它不依赖外部 tokenizer 库，而是基于 **SDK 返回的实测 `Usage` 数据 + 轻量新增内容估算** 实现精确管控。

#### 11.7.1 核心洞察：openai-go SDK 已返回实测 Token 数

`openai-go/v3` 的 `ChatCompletion` 响应结构体中包含 `Usage` 字段：

```go
completion.Usage.PromptTokens     // 本次请求实际消耗的 prompt token 数
completion.Usage.CompletionTokens // 本次请求实际消耗的 completion token 数
```

AgentGo 的 `internal/llm/client.go` 已在每次调用后提取这两个值（第 216-217 行）。这意味着**每次 LLM 调用后，我们都知道这条历史记录对应的实际 token 消耗**。

**关键认知**：`Usage.PromptTokens` 是**事后值**——请求发出去、模型处理完才返回。它不能替代"请求前的截断决策"，但可以作为**下一次请求的精确基准点**。

#### 11.7.2 双层阈值机制

```
历史消息 Token 长度
       │
       ▼
  ┌────────────┐  context_limit（硬上限，如 16000）
  │  强制截断   │  ← 超过时从最老消息开始丢弃（保留 system + 最近 N 条）
  └────────────┘
       │
  ┌────────────┐  compact_token_threshold（压缩阈值，如 4000）
  │  触发 Summary │  ← 超过时对老历史做 LLM 压缩，生成 condensed summary
  └────────────┘
       │
       └─ 正常区间，不做任何处理
```

#### 11.7.3 Token 追踪方案：实测锚定 + 轻量估算

**不引入 tiktoken-go，不自建复杂估算器**。策略分为两部分：

##### （1）HistoryEntry 逐条记录实测 Token 消耗

在 `HistoryEntry` 中增加两个字段，记录**产生该条 assistant 回复时**的实际消耗：

```go
type HistoryEntry struct {
    // ... 现有字段（Role, Content, ToolCalls, ExtraFields 等）
    PromptTokens     int `yaml:"prompt_tokens,omitempty" json:"prompt_tokens,omitempty"`
    CompletionTokens int `yaml:"completion_tokens,omitempty" json:"completion_tokens,omitempty"`
}
```

**记录时机**：每次 `processTask` 中 LLM 调用返回后，将 `resp.Usage` 写入刚产生的 assistant 条目：

```go
history = append(history, HistoryEntry{
    Role:             "assistant",
    Content:          resp.Content,
    ToolCalls:        resp.ToolCalls,
    ExtraFields:      resp.ExtraFields,
    PromptTokens:     resp.Usage.PromptTokens,      // ← 实测值
    CompletionTokens: resp.Usage.CompletionTokens,  // ← 实测值
})
```

**为什么记录在 assistant 条目上**：
- `PromptTokens` 描述的是"请求时整条历史的长度"，属于该轮对话的元数据
- 放在 assistant 条目上，自然形成"每轮一问一答都携带该轮的实际 token 开销"的结构
- 序列化到 `history.jsonl` 时不增加新文件类型

##### （2）下次请求前的长度预测：上次实测 + 新增估算

```go
func PredictNextPromptTokens(history []HistoryEntry, newUserContent string) int {
    if len(history) == 0 {
        // 首次请求：无实测值，粗略估算
        return len(systemPrompt)/3 + len(newUserContent)/3 + 100
    }
    
    // 找到最近一条带有 PromptTokens 的 assistant 条目
    lastActual := 0
    lastIdx := -1
    for i := len(history) - 1; i >= 0; i-- {
        if history[i].Role == "assistant" && history[i].PromptTokens > 0 {
            lastActual = history[i].PromptTokens
            lastIdx = i
            break
        }
    }
    
    if lastActual == 0 {
        // 历史中没有实测值（兼容性保护），退化到粗略估算
        return estimateFromScratch(history)
    }
    
    // 计算上次 assistant 之后新增的内容
    // 包括：tool results、新的 user message、system prompt 不变
    added := 0
    for i := lastIdx + 1; i < len(history); i++ {
        added += len(history[i].Content) / 3  // 轻量估算新增部分
        for _, raw := range history[i].ExtraFields {
            added += len(raw) / 3
        }
    }
    // 加上本次即将发出去的新 user message
    added += len(newUserContent) / 3
    
    return lastActual + added
}
```

**精度分析**：

| 场景 | 纯自估算误差 | 实测锚定+新增估算误差 | 原因 |
|------|------------|---------------------|------|
| 英文为主 | ±25% | ±8% | 主体部分有实测值锚定 |
| 中文为主 | ±40% | ±10% | 新增部分占比小，估算误差被稀释 |
| 代码混合 | ±30% | ±7% | 工具定义等固定开销已被实测值覆盖 |

误差从"整条历史"缩小到"新增几条消息"，即使新增部分的 `/3` 估算有 ±30% 偏差，对最终二值决策（是否超过 16000）的影响也很小。

##### （3）Agent 级别的累计统计

每个 Agent 维护累计 Token 消耗，用于成本追踪和运行时监控：

```go
type Agent struct {
    // ... 现有字段
    TokenStats TokenStats
}

type TokenStats struct {
    TotalPromptTokens     int64
    TotalCompletionTokens int64
    CallCount             int
}
```

每次 LLM 调用后累加：

```go
a.TokenStats.TotalPromptTokens += int64(resp.Usage.PromptTokens)
a.TokenStats.TotalCompletionTokens += int64(resp.Usage.CompletionTokens)
a.TokenStats.CallCount++
```

可产出运行时日志：

```
[worker-1] 本轮: prompt=4213, completion=892 | 累计: prompt=38721, completion=8402, 调用=12
[explorer] 本轮: prompt=1892, completion=340 | 累计: prompt=12450, completion=2103, 调用=8
```

未来可在 YAML 中配置模型单价，实现**任务结束时的成本汇报**。

#### 11.7.4 截断策略（基于预测值）

```go
func TruncateHistory(history []HistoryEntry, contextLimit int) ([]HistoryEntry, error) {
    predicted := PredictNextPromptTokens(history, "")
    
    if predicted <= contextLimit {
        return history, nil
    }
    
    // 保护不可删除的部分
    protectedHead := 1  // system prompt（第 0 条）
    protectedTail := cfg.CompactKeepRecent * 2  // 最近 N 对 request/response
    
    for PredictNextPromptTokens(history, "") > contextLimit &&
          len(history) > protectedHead + protectedTail {
        // 删除 protectedHead 之后最老的一条
        history = append(history[:protectedHead], history[protectedHead+1:]...)
    }
    
    if PredictNextPromptTokens(history, "") > contextLimit {
        return history, ErrContextLimitTooSmall
    }
    return history, nil
}
```

**关键约束**：
- 截断发生在**每次 LLM 调用前**（`agent.go` 的 `processTask` → `buildMessages` 阶段）
- 截断后必须保证消息序列仍满足 OpenAI 格式约束（`assistant(tool_calls)` 后必须紧跟对应的 `tool` 消息）
- 如果删除某条 `assistant(tool_calls)`，必须同时删除其后直到下一条非 `tool` 消息之前的所有 `tool` 消息

#### 11.7.5 与 Compact 机制的协作

`compact_token_threshold` 和 `context_limit` 不是互斥的，而是**协同工作**：

```
Token 长度
    │
16000 ├──────────── context_limit（硬上限，截断）
    │     ╱
 6000 ├────╱─────── compact_token_threshold（软阈值，触发 summary）
    │   ╱
    └─╱──────────── 正常区间
```

- **`< compact_token_threshold`**：不做任何处理，完整历史直送 LLM
- **`≥ compact_token_threshold`**：触发历史压缩（summary），由 Agent 自己发起 compaction，用 LLM 生成老历史的 condensed summary
- **`≥ context_limit`**：即使还未压缩到阈值以下，**强制截断**最老的消息，确保不超限

两者的关系：**压缩是主动的语义保留，截断是被动的物理丢弃**。压缩优先于截断——如果历史超过了 `compact_token_threshold` 但未超过 `context_limit`，Agent 会尝试用 LLM 压缩；如果压缩后仍然超过 `context_limit`，则进入强制截断。

#### 11.7.6 局限与应对

| 局限 | 说明 | 应对 |
|------|------|------|
| **首次请求无实测值** | 历史为空时，没有 `lastActual` 可参考 | 首次请求用粗略估算（`/3`），从第二次开始有实测值锚定 |
| **工具定义未计入 HistoryEntry** | `PromptTokens` 包含 tools schema，但 history 中不存储 tools | 工具定义长度相对固定，可被实测值自然吸收；新增工具时首条请求的实测值会更新基准 |
| **模型切换后实测值失效** | 不同模型的 tokenizer 不同，之前的 `PromptTokens` 不再可比 | 在 `HistoryEntry` 中同时记录 `Model` 字段；切换模型时重置基准，首条请求退化到粗略估算 |
| **Reasoning content 的 token 数** | DeepSeek thinking 模式下 `reasoning_content` 是否计入 `PromptTokens` | 由模型/API 决定，SDK 返回的 `Usage` 已经包含，应用层无需关心 |

### 11.8 实施步骤

| 步骤 | 内容 | 产出 |
|------|------|------|
| S1 | 定义 v4 Config 结构体（`config/v4/config.go`），与 v3 完全隔离 | 新包 |
| S2 | 实现 `AgentConfigFactory`：Template 合并 + ParamsOverride 覆盖 + LLM 默认值回退 | 合并逻辑 + 单测 |
| S3 | 重构 `worker.NewWithID` / `explorer.New` / `scheduler.New` 签名，接收 `AgentRuntimeConfig` | 构造函数简化 |
| S4 | 改写 `bootstrap.go`：按 v4 Config 流程启动所有组件 | 新启动流程 |
| S5 | `HistoryEntry` 增加 `PromptTokens` / `CompletionTokens`；`processTask` 中记录实测值；实现 `PredictNextPromptTokens`（实测锚定 + 新增估算）+ `TruncateHistory` | token 追踪 + 截断 |
| S6 | 在 `agent.go` `buildMessages` 阶段接入截断逻辑 | 运行时保护 |
| S7 | `config.example.yaml` 重写为 v4 格式 | 新示例配置 |
| S8 | 启动校验：scheduler.count==1、explorer.count==1、模板名存在性 | 防御性校验 |
| S9 | 端到端烟测：启动 → 发一个长任务 → 确认历史压缩和截断工作正常 | 回归保证 |

### 11.9 不在本节范围

- **v3 配置格式的自动迁移工具**：v4 不向后兼容，升级时人工迁移一次即可。不维护迁移脚本。
- **动态模板切换**：Agent 启动后 Template 不可变。运行时行为调整靠重启或未来热重载机制。
- **Per-tool 权限模板**：v4 §2 分级权限模型是独立功能，本节只解决"Agent 行为参数配置"，不涉及任务级权限裁剪。
- **Tokenizer 依赖决策**：本节不引入 tiktoken-go 等外部 tokenizer。Token 长度管理完全基于 SDK 返回的 `Usage.PromptTokens` 实测值 + 轻量新增估算。如果未来实测值策略被证明不够精确，再考虑引入 tiktoken-go 作为可选后端。
- **Explorer 多实例**：`explorer.count` 当前限制为 1，schema 预留字段但不实现多 Explorer 的路由逻辑。等 scheduler 的 `AgentRegistry` 支持多 Explorer 后再放开。

