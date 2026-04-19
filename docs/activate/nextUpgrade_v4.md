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
