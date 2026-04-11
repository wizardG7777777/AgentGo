# Hook System 架构与实现规范

> **📦 归档说明（2026-04-11）**：阶段 1（Tool Hook）与阶段 2（Mailbox Hook）全部落地完成——`internal/hook` + `internal/hook/builtin` 7 个内置 hook、`StoreHookView`、`ToolCallRecord` 二级索引、`mailbox.Registry.AttachHookRunner`、`MailNotifier` BeforeWake 触发点、`MailNotifierEnabled` 默认恢复为 `true`。本文档不再更新，也不应再作为未来开发的参考依据。运行时权威信息以 `Archtechture.md` §Hook System 与源代码为准；阶段 3+ 的延期项另行规划，不从本文档续写。

> 状态：✅ 设计已对齐（2026-04-09 完成）
> 目标读者：本文是阶段 1+2 的实现规范，§9 检查清单全部对齐，可按 §10 执行
> 范围控制：本文只覆盖**阶段 1（Tool Hook）+ 阶段 2（Mailbox Hook）**。其他类别只列出占位，详细延期项见 `nextUpgrade_v3.md` §5

---

## 0. 为什么要做这件事

### 0.1 起因

在修复 "Scheduler `report_done` 不基于 `task.Artifacts` 真实清单" P0 时，识别出一类系统性问题：**软约束 → 硬约束的断层**。系统里有大量"事实数据"和"LLM 自述"两条并行的信息流，每一对都有一个潜在的同型 bug——事实流是对的，但 LLM 自述流被允许在没有事实校对的情况下流出系统边界。

已知的断层至少 7 类：
- `task.Artifacts` vs LLM 在 `report_done` summary 里说的产物（**已修复**）
- `task.Status` vs LLM 在响应里说"任务已完成"
- `task.LastResponse` vs scheduler 看到的压缩历史
- `task.RetryCount` vs LLM 历史里读到的重试次数
- Roster `TryClaim` 返回值 vs LLM prompt 里的"目前你独占该文件"
- `pathutil.ValidatePath` 拒绝结果 vs LLM 历史中记忆的允许路径
- trace event log（事实）vs 历史压缩后留下的概述

每发现一个断层就外科手术修一次，能解决具体问题，但当断层数量增加到 15-20 个时会变成 review 负担和代码碎片化。Hook System 是对"一类一修"的元层抽象。

### 0.2 已经在做的"硬约束兜底"

不是所有断层都没修。当前已存在 8 处硬约束：

| 防线 | 位置 | 强度 |
|---|---|---|
| `checkExpectedArtifacts`（任务结束前） | `agent.go:284` | ✅ 硬，缺失即重试 |
| `model.IsValidTransition` 状态机校验 | `model/task.go` | ✅ 硬，非法转换 error |
| `Roster.TryClaim` 原子互斥 | `roster/memory.go` | ✅ 硬 |
| `expected_hash` TOCTOU 校验 | `tools/local_write.go` | ✅ 硬 |
| `pathutil.ValidatePath` 路径越界 | `pathutil/pathutil.go` | ✅ 硬 |
| Scheduler 拒绝 explore + expected_artifacts | `scheduler.go:554` | ✅ 硬 |
| `report_done` pending 任务拦截 | `scheduler.go:620` | ✅ 硬 |
| `report_done` 产物事实校对 | `scheduler.go:632` | ✅ 硬（2026-04-09 新增） |

**Hook System 不是要替换这些，而是为它们提供一个统一的注册、组合、测试、扩展面**。

### 0.3 非目标 — 我们不是在做什么

为了避免重蹈 worktree 覆辙（一个机制承诺 4 个能力，只有 1 个真兑现），先明确**不做**的事：

- ❌ **通用的"LLM 反幻觉框架"**——开销大、误报多、规则难维护
- ❌ **运行时把 LLM 每次响应都拦截校验**——同上
- ❌ **一次性把 6 个 hook 类别全建出来**——4 个类别没有清晰需求，会变成空转脚手架
- ❌ **第三方插件加载机制**（阶段 1+2 不做）——等真有第三方需求再加，避免提前考虑安全沙箱
- ❌ **runtime 配置加载 hook**（阶段 1+2 不做）——编译时注册，类型安全、可调试、零安全顾虑

---

## 1. 范围与分阶段路线图

### 1.1 阶段划分

| 阶段 | 内容 | 交付物 | 决策依据 |
|---|---|---|---|
| **阶段 1** | Tool Hook 框架 + 把现有 4 处 hard-coded 逻辑迁移成 hook | `internal/hook` 包 + 4 个 ToolHook + 既有测试全绿 | 验证 hook 抽象是否成立；迁移既有逻辑天然提供回归测试 |
| **阶段 2** | Mailbox Hook 框架 + 4 个邮件 hook + 重新启用 mail-notifier | `internal/hook` 扩展 + 4 个 MailboxHook + 邮件级联爆炸 P0 关闭 | 闭合一个具体 P0，直接产生用户可见价值 |
| **阶段 3+** | 按需扩展（Chathistory / Board / Session / Skill） | — | **必须有具体的、可写出 spec 的痛点** |

**阶段 1 的成功标准**（在动手之前先对齐）：
- 不引入任何新功能，仅迁移既有行为
- 所有既有的 worker / explorer / scheduler 单元测试不修改地通过
- Hook 框架本身有独立单元测试覆盖：注册、顺序、Continue/Abort、panic 恢复
- 可以通过"禁用所有 hook"的开关恢复到迁移前的行为，作为可逆性证明

**阶段 2 的成功标准**：
- `mail_notifier_enabled: true` 重新启用，且 KNOWN_ISSUES 中的"4 分钟 16+ 派生任务"重现路径在测试环境下不再爆炸
- 3 个 mail hook 各有独立单测 + 1 个端到端测试模拟级联场景

### 1.2 为什么是 Tool Hook 先行

- 它的需求最清晰，已经在 KNOWN_ISSUES 和现有代码里能找到 4 个具体迁移目标
- pre/post tool call 是清洁的、广为人知的模式（git hooks、pytest fixtures、各种 middleware）
- 失败爆炸半径最小：单个 tool 调用范围内出错，不会拖垮 agent 或其他子系统
- 迁移既有逻辑是最强的烟雾测试 — 既有测试就是回归网

---

## 2. 阶段 1：Tool Hook 详细设计

### 2.1 核心接口（已对齐）

```go
// internal/hook/tool.go
package hook

import "context"

// ToolHookPhase 标识 hook 在工具调用生命周期中的位置，但是使用显式的String进行标记
type ToolHookPhase string

const (
    PhasePreCall  ToolHookPhase = "preCall"  // 工具执行前
    PhasePostCall ToolHookPhase = "postCall" // 工具执行后（无论成功失败）
)

// ToolHookContext 是 hook 能拿到的全部信息（只读）
type ToolHookContext struct {
    Ctx       context.Context
    Phase     ToolHookPhase
    AgentID   string
    TaskID    string
    ToolName  string
    Args      map[string]any // pre 阶段可见
    Result    string         // post 阶段可见，pre 阶段为空
    Err       error          // post 阶段可见
    // 注：history / store / roster 不放在此处，hook 通过构造时注入的 HookView 接口访问。详见 §11
}

// ToolHookDecision 是 hook 的返回值
type ToolHookDecision struct {
    Action      HookAction // Continue / Abort
    AbortReason string     // Action=Abort 时填写，会被注入 LLM 历史
}

type HookAction int

const (
    Continue HookAction = iota
    Abort               // 中断本次工具调用，错误返回给 LLM
)

// ToolHook 是单个 hook 的接口
type ToolHook interface {
    Name() string                       // 唯一标识，用于日志和调试
    Phase() ToolHookPhase               // 在哪个阶段触发
    Matches(toolName string) bool       // 是否对该工具感兴趣（"*" 表示所有）
    Priority() int                      // 数字越小越先执行
    Run(hctx *ToolHookContext) ToolHookDecision
}
```

### 已对齐的决策

- ✅ **`Replace` 不做**：Hook System 的定位是让系统可控，而不是介入工具调用本身的执行逻辑。`Continue / Abort` 已经足够表达"放行"和"拒绝"，`Replace` 会让 hook 和工具调用产生耦合，违背这个定位。`HookAction` 只保留 `Continue / Abort`。
- ✅ **全值传递**：Hook System 内部所有参数传递一律走值传递，不用指针。`ToolHookContext` 和 `ToolHookDecision` 都按值传递。由于 hook 不涉及长文本内容传递，值传递的拷贝开销可以忽略，同时彻底消除 hook 通过指针悄悄修改上下文的可能。
- ✅ **`Args` 字段深拷贝**：值传 `ToolHookContext` 不能保护 `Args map[string]any`——map 是引用类型，hook 即使收到的是值传 context，仍然能通过 `hctx.Args["path"] = ...` 修改原始 args。`ToolHookRegistry.RunPre` / `RunPost` 在调用每个 hook 之前，必须对 `hctx.Args` 做一次浅拷贝（`make(map[string]any, len(orig)); for k,v := range orig { dst[k] = v }`）。当前阶段 1 的工具参数都是扁平 `string`/`int`/`bool`/`string数组`，浅拷贝足够；如果未来出现嵌套 map，需要升级为深拷贝。该约束写入 `ToolHookRegistry` 实现的单元测试中验证。
- ✅ **字符串直接匹配**：`Matches` 使用字符串直接匹配（精确匹配或 `"*"` 通配），不做 glob 模式。声明式、可静态分析、日志可读，满足当前所有迁移目标的需求。
- ✅ **Post Hook 无返回值**：工具执行完毕后只有成功（`Err == nil`）和失败（`Err != nil`）两种状态，结果已经确定，post hook 的职责是纯观察，不干预。`RunPost` 无返回值，post 阶段的 hook 也不需要返回 `HookAction`。


### 2.2 Hook Registry

```go
// internal/hook/registry.go
type ToolHookRegistry struct {
    mu    sync.RWMutex
    hooks []ToolHook
}

func (r *ToolHookRegistry) Register(h ToolHook) error {
    // 校验：name 不能重复；priority 范围 [0, 1000]
}

func (r *ToolHookRegistry) RunPre(hctx ToolHookContext) ToolHookDecision {
    // 按 priority 升序遍历所有 PhasePreCall 且 Matches(toolName)=true 的 hook
    // 任一 Abort → 立即返回 Abort（短路）
    // 任一 panic → recover()，记 log，视为 Continue
}

func (r *ToolHookRegistry) RunPost(hctx ToolHookContext) {
    // 按 priority 升序遍历所有 PhasePostCall 且 Matches(toolName)=true 的 hook
    // post 阶段只观察，不干预，无返回值
    // hctx.Err == nil 表示工具成功，hctx.Result 有内容
    // hctx.Err != nil 表示工具失败（报错或 internal error），hook 据此决定是否执行副作用
    // 任一 panic → recover()，记 log，继续执行后续 hook
}
```

### 2.3 在工具执行链路中的接入点

当前 `internal/agent/llm_executor.go` 的并行工具执行段是唯一的工具调用枢纽：

```go
// 伪代码 — 当前
for _, call := range toolCalls {
    go func() {
        result := registry.Call(call.Name, call.Args)
        // ...
    }()
}

// 伪代码 — 接入 hook 后
for _, call := range toolCalls {
    go func() {
        hctx := &ToolHookContext{Phase: PhasePreCall, ToolName: call.Name, Args: call.Args, ...}
        if d := hookReg.RunPre(hctx); d.Action == Abort {
            result = "[hook 拒绝] " + d.AbortReason
            // 注入到 LLM 历史，跳过实际调用
        } else {
            result = registry.Call(call.Name, call.Args)
        }
        hctxPost := &ToolHookContext{Phase: PhasePostCall, Result: result, ...}
        hookReg.RunPost(hctxPost)
        // ...
    }()
}
```

**接入点**：`internal/agent/llm_executor.go` 的 `NewLLMExecutor` 函数（lines 64-223）的并行工具调用 goroutine（lines 147-203）。**改动局部化**。

**精确接入步骤**：
1. `NewLLMExecutor(client, tools, systemPrompt...)` 签名扩展为 `NewLLMExecutor(client, tools, hookReg *hook.ToolHookRegistry, systemPrompt...)`，新增一个 `hookReg` 参数（可为 nil，nil 时整个 hook 路径退化为 no-op，保留与改动前完全一致的行为——这是回归验证的关键）
2. 在 line 164 的 `result, toolErr := tools.Dispatch(ctx, c)` 调用前后插入 hook 调用与 store 写入：
   ```go
   // 构造 hook context（Args 深拷贝在 RunPre 内部完成）
   preCtx := hook.ToolHookContext{Phase: hook.PhasePreCall, AgentID: agentID, TaskID: task.ID, ToolName: c.Name, Args: c.Arguments, Ctx: ctx}
   preDecision := hookReg.RunPre(preCtx)  // hookReg=nil 时返回 Continue
   var content string
   var toolErr error
   if preDecision.Action == hook.Abort {
       content = "[hook 拒绝] " + preDecision.AbortReason
       toolErr = fmt.Errorf("hook %s 拒绝: %s", preDecision.HookName, preDecision.AbortReason)
   } else {
       content, toolErr = tools.Dispatch(ctx, c)
   }
   // 无论 abort 还是真正调用，都记录到 store（详见 §11.1.3）
   if storeView != nil {
       storeView.AppendToolCall(task.ID, store.ToolCallRecord{Timestamp: time.Now(), AgentID: agentID, ToolName: c.Name, Args: c.Arguments, Success: toolErr == nil})
   }
   postCtx := hook.ToolHookContext{Phase: hook.PhasePostCall, ..., Result: content, Err: toolErr}
   hookReg.RunPost(postCtx)
   ```
3. 调用方更新（每处一行）：`internal/worker/worker.go` / `internal/explorer/explorer.go` 在构造 LLMExecutor 时传入 hookReg；`internal/agent/llm_executor_test.go` 中的 mock 构造同步更新

**关键豁免：Scheduler 工具不走该路径**

Scheduler 的工具（`publish_task` / `cancel_task` / `report_done` / `send_message`）由 `internal/scheduler/scheduler.go` 内的 `dispatchTool` switch 直接处理，**不经过 `agent.NewLLMExecutor`**。这意味着：

- Hook 系统对 Scheduler 工具调用**完全不生效**
- `PathBoundaryHook` 不会拦截 Scheduler 的工具（Scheduler 也不调用文件操作工具，所以没有实际影响）
- `RecordArtifactHook` 不会记录 Scheduler 的产物（Scheduler 不写文件，所以没有实际影响）
- 这是**正确的**架构隔离——Scheduler 的关注点是任务编排，与 Worker 的文件操作横切完全不同
- 如未来需要给 Scheduler 工具加 hook，需要单独设计 `SchedulerHookRegistry` 或把 Scheduler 也迁移到走 `agent.NewLLMExecutor`，**不在阶段 1+2 范围**

### 2.4 阶段 1 迁移目标：4 个现有 hard-coded 行为

| Hook | 替代的现有逻辑 | 阶段 | 典型用例 |
|---|---|---|---|
| `RequireReadBeforeWriteHook` | 当前散落在 worker.systemPrompt 的软约束 | Pre on `write_file`/`edit_file` | 检查同任务历史里是否有过对该路径的 `read_file`，没有则 Abort 提示 LLM 先读 |
| `RecordArtifactHook` | `tools/local_write.go:40` `recordArtifact` | Post on `write_file`/`edit_file` | 调用 `Store.AppendArtifact`（事实流写入） |
| `ValidateExpectedHashHook` | `tools/local_write.go` 的 hash 校验段 | Pre on `write_file`/`edit_file` | 校验 `expected_hash`，不一致 Abort |
| `PathBoundaryHook` | `pathutil.ValidatePath` 在工具内部的调用 | Pre on `read_file`/`write_file`/`edit_file`/`list_dir`/`grep_search`/`glob_search`/`run_shell` | 路径越界 / 敏感文件 → Abort |

迁移完成后，对应工具的 hard-coded 校验代码可以被删除，因为 hook 已经接管。**这是迁移成功的关键标志**：原代码删除而不是冗余共存。

### 2.5 preCall 和 postCall Hook 之间的区别

| 维度 | preCall | postCall |
|---|---|---|
| 触发时机 | 工具执行前 | 工具执行后（无论成功失败） |
| 返回值 | `ToolHookDecision`（可 Abort） | 无返回值 |
| 能否阻止工具执行 | ✅ 可以（返回 Abort） | ❌ 工具已执行，无法撤回 |
| panic 处理 | recover，视为 Continue（放行） | recover，记 log，继续执行后续 hook |
| 典型用途 | 权限检查、路径越界校验、hash 一致性校验、竞争阻拦、限流 | 产物记录、trace 事件写入、相关机制通知 |

**preCall 的职责边界**：只做合法性判定，返回放行或拒绝。不能改写工具参数（无 Replace），不能发起新的工具调用。

**postCall 的职责边界**：纯观察和记录。通过 `hctx.Err` 区分成功/失败后决定是否执行副作用（如只在成功时记录产物）。不能影响当前对话上下文，不能反向修改工具结果。


### 2.6 接口细节的已对齐决策

- ✅ **历史查询走 store**：阅读记录使用内存存储，程序退出时清空，不需要持久化。`TaskStore` 接口扩展 `AppendToolCall` / `QueryToolCalls` 两个方法，由 `llm_executor.go` 在每次工具调用 post hook 之前自动写入。`RequireReadBeforeWriteHook` 通过 `StoreHookView.GetToolCallHistory` 查询。详细规格见 §11.1。
- ✅ **Hook 不能发起新的工具调用**：hook 内部发起工具调用会产生复杂的循环依赖，破坏 hook 的职责边界。如果需要在 hook 触发后执行复杂逻辑，应当以 pipeline 的方式组织，将多个步骤串联成独立的处理链，而不是在 hook 内部嵌套调用。
- ✅ **Hook 通过专用 HookView 接口访问机制**：store、roster、trace、mailbox 等机制各自留一个 `XxxHookView` 接口，hook 在构造时拿到该只读接口（除 `AppendArtifact` / `AppendToolCall` 等明确的写入方法外），不持有完整对象引用。详细规格见 §11。

---

## 3. 阶段 2：Mailbox Hook 详细设计

### 3.1 触发时机

Mailbox 系统有 3 个关键时机：

| 时机 | 触发位置 | Hook 阶段名 |
|---|---|---|
| 消息发送时 | `mailbox.SendMessage` 调用前 | `PhaseBeforeSend` |
| 消息投递到收件箱时 | `mailbox.Mailbox` channel 写入前 | `PhaseBeforeDeliver` |
| MailNotifier 决定是否发布唤醒任务时 | `notifier.scan` 内部 | `PhaseBeforeWake` |

### 3.2 阶段 2 实现的 3 个 Hook（直接对应邮件级联爆炸根因）

| Hook | 触发时机 | 行为 |
|---|---|---|
| `ChainDepthLimitHook` | PhaseBeforeSend / PhaseBeforeDeliver | 每次消息发送或转发时自动将 `chain_depth` 加一；超过 `cfg.MailChainMaxDepth`（建议 3）则把消息标记为"不触发唤醒"但仍投递 |
| `PerAgentDedupHook` | PhaseBeforeWake | 检查目标 agent 是否已有 pending 唤醒任务，有则把当前未读邮件合并到既有任务的 description，不发布新任务 |
| `WakeContextExpandHook` | PhaseBeforeWake | 把目标 agent 收件箱前 N 条邮件的 `summary` 字段拼接到唤醒任务的 description，让被唤醒的 LLM 看到"我为什么被叫醒"具体内容 |

### 3.3 配套的非 hook 改动

阶段 2 不是纯 hook 工作，还需要：
- `mailbox.Message` 加 `ChainDepth int` 字段
- `Task` 加 `MailChainDepth int` 字段，记录该任务被第几层邮件唤醒（用户 `/steer` 触发的初始任务为 0）
- `MailNotifier.scan` 暴露 hook 调用点
- `cfg.MailChainMaxDepth`（默认 3）
- **重新启用 `cfg.MailNotifierEnabled = true`**（这是阶段 2 的胜利标志）

### 3.4 已对齐的决策

- **`chain_depth` 自动维护**：`chain_depth` 字段由 `ChainDepthLimitHook` 在每次消息发送或转发时自动加一，worker 完全不感知该字段。hook 在 preCall 阶段从 store 读取当前任务的 `Task.MailChainDepth`，加一后写入消息，不需要修改 `send_message` 工具的参数定义。
- **`Task.MailChainDepth` 语义**：记录"该任务是被第几层邮件唤醒的"。用户 `/steer` 触发的初始任务为 0，被 chain_depth=1 的邮件唤醒的任务为 1，以此类推。hook 读此字段即可直接知道当前处于第几层，无需额外计算。
- **`ReplyCooldownHook` 放弃**：实现有效的相似消息检测需要引入词嵌入模型，成本过高，不适合当前阶段。精确哈希对 LLM 生成的消息几乎无效（微小措辞差异即绕过）。该 hook 从阶段 2 移除，级联爆炸的抑制由 `ChainDepthLimitHook` 和 `PerAgentDedupHook` 承担。

---

## 4. 阶段 3+：占位（不做接口设计，仅记录考虑过）

### 4.1 Chathistory Hook
**潜在用途**：把现有 3 层压缩策略重构成 hook，允许用户自定义压缩规则。
**为什么不做**：3 层压缩当前工作良好，重构没有新功能。等真有"用户想自定义压缩"的需求再做。

### 4.2 Board Hook
**潜在用途**：任务状态变更时触发副作用（统计、告警、外部通知）。
**为什么不做**：现有的 eventCh 已经覆盖事件订阅。除非有具体的"必须在状态转换前拦截"的需求，否则 hook 是冗余的。

### 4.3 Session Hook
**潜在用途**：CLI 命令执行前后触发逻辑（审计、权限、日志）。
**为什么不做**：当前 CLI 命令很少（/quit /mode /status /cancel /help /steer），直接在 cli 包里硬编码足够。

### 4.4 Skill Hook
**潜在用途**：第三方 skill 文件加载机制。
**为什么不做**：用户自己说"暂时没用处"。

**触发阶段 3 任一类别的标准**：必须能写出**至少 2 个具体的、独立的、当前无法解决的痛点**，否则不动。

---

## 5. 跨阶段共同的设计决策（已对齐）

### 5.1 注册时机：编译时
✅ **决策：编译时注册**。
- 在 `bootstrap.go` 里显式 `hookReg.Register(&hook.PathBoundaryHook{...})`
- 简单、类型安全、零安全顾虑、调试可读
- 运行时加载延期到 `nextUpgrade_v3.md` §5.3，需要时再设计沙箱机制

### 5.2 执行顺序：显式 Priority 整数
✅ **决策：Priority 整数 [0, 1000]，数字越小越先执行**。
- 简单、可读、无图算法
- 注册顺序脆弱（重构 bootstrap 顺序就坏），不采用
- 声明式依赖需要图算法和环检测，过度设计
- 默认 Priority=500
- 约定段：0-100 给系统级强制 hook（Path 校验）、900-1000 给观察类 hook（trace 记录、artifact 记录）

### 5.3 失败语义：仅 Continue / Abort
✅ **决策：阶段 1+2 只做 Continue / Abort，不做 Replace**。
- 4 个迁移目标都不需要 Replace
- Replace 会让 hook 与工具调用执行逻辑产生耦合，违背"hook 让系统可控"的定位
- Pre Abort = 不执行工具，错误注入历史；Post 阶段无返回值（详见 §2.5 对比表）
- 延期项见 `nextUpgrade_v3.md` §5.8

### 5.4 同步 vs 异步
✅ **决策：阶段 1+2 全部同步**。
- Tool hook 必须同步（write_file 之前要校验）
- Mailbox hook 也同步（chain_depth 校验、cooldown 校验都需要在发送决策中即时返回）
- 异步 hook 等阶段 3+ 真有"非阻塞观察"需求时再加

### 5.5 错误隔离：panic 处理
✅ **决策：每个 hook 都包在 `defer recover()` 里，panic 视为 Continue**。
- 一个 hook panic 不能拖垮 agent
- 记 log + emit trace event，便于排查
- 不抛回主流程

### 5.6 Context 注入：hook 拿到什么
✅ **决策：值传 `ToolHookContext` 结构体（只读）+ 构造时注入 HookView 接口**。
- 运行时数据通过值传 `ToolHookContext`：taskID、agentID、tool name、args、result、err
- 持久依赖（store / roster / trace / mailbox）通过 hook 构造函数注入对应的 `XxxHookView` 接口（详见 §11）
- Hook 想改东西必须用返回值（`ToolHookDecision`），强制显式

### 5.7 测试策略
- 每个 hook 独立单测，不依赖 registry
- Registry 的注册、顺序、Abort 短路、panic 恢复独立单测
- 既有 worker/explorer/scheduler 测试**不加 hook，全部跳过 hook 注册**（默认禁用），作为"hook 不影响既有功能"的回归保证
- 端到端测试（少量）：注册某个 hook 后验证特定行为

---

## 6. "行哈希增强"——已澄清

行哈希增强（Hashline Read Enhancer）的目标是解决行号漂移问题：LLM 读文件时记住的行号，在实际写入时可能因为其他 agent 的修改而漂移，导致编辑错误的行。

**决策：行哈希增强直接集成到 `read_file` 工具内部，不走 Hook System。**

理由：postCall hook 的职责是纯观察，不能改写工具输出；行哈希是工具输出格式的一部分，属于工具层能力，不是横切关注点。

详细设计见 `docs/activate/nextUpgrade_v3.md`。

---

## 7. 与现有架构的交互

### 7.1 与 trace 系统的关系
- Hook 触发时 emit trace event（新增 `KindHookFired` event 类型）
- Trace 仍是事后审计工具，hook 是运行时拦截层
- 二者互补：hook 防止问题发生，trace 记录已发生的事

### 7.2 与 KNOWN_ISSUES.md 的关系
- 阶段 1 完成后，更新 KNOWN_ISSUES "多代理协同重建" 条目，注明部分能力可由 Tool Hook 表达
- 阶段 2 完成后，把"邮件级联爆炸"条目从 🚧 改为 ✅，并在总览表更新计数

### 7.3 与 Archtechture.md 的关系
- 阶段 1 完成后，在 Archtechture.md 加一段 § Hook System 章节
- 阶段 2 完成后，更新 § 邮箱与异步通讯 段落，移除"MailNotifier 当前禁用"的注记

### 7.4 与 nextUpgrade_v2.md 的关系
- v2 是工具系统重构计划（ToolGroup + 11 核心工具）
- ✅ **决策**：**先做 hook 阶段 1，同时改 v2 的 ToolGroup 设计来适配 hook**。理由：hook 接入点 `llm_executor.go` 是工具调用的唯一枢纽，ToolGroup 重构必然影响这一段；与其分两次改，不如让 v2 的 ToolGroup 接口在设计时就为 hook 留好钩子点，一次到位
- 行动项：在动手写代码前，需要把 `nextUpgrade_v2.md` §1.1 的 ToolGroup 接口草案与本文档 §2.3 的接入点对照一遍，识别需要在 v2 中预留的字段或方法。这一步在 §10 下一步中作为前置任务列出

---

## 8. 风险与缓解

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| Hook 框架本身成为过度设计（worktree 教训） | 中 | 高 | 阶段 1 严格只迁移既有 4 处行为，禁止加任何新功能 |
| Hook 顺序依赖导致难调试 | 中 | 中 | 显式 Priority + 强制 emit trace event，每次 hook 触发都可追溯 |
| Hook panic 拖垮 agent | 低 | 高 | 每个 hook 包 recover()，panic 视为 Continue 并记 log |
| 测试面爆炸（O(N²) 组合） | 中 | 中 | 明确测试分层：单 hook 单测 + registry 单测 + 少量端到端测试，禁止穷举组合 |
| 阶段 3 类别诱惑（"既然有框架了，加几个 hook 又何妨"） | 高 | 中 | 写明阶段 3 触发标准（"至少 2 个具体痛点"），review 时严格执行 |
| 与 v2 ToolGroup 重构接口冲突 | 中 | 中 | 先稳定 ToolGroup，再做 hook 接入；或与 v2 同步设计 |
| `HookView` 接口边界被侵蚀（传完整对象替代接口） | 高 | 中 | hook 构造函数参数类型必须是 `XxxHookView` 接口，禁止传具体实现类型；code review 强制检查 |
| `chain_depth` 加一逻辑分散在多处（hook 之外也有加一） | 中 | 高 | 所有 `chain_depth` 加一逻辑必须收束到 `ChainDepthLimitHook`，其他任何位置禁止出现加一操作；迁移时全局搜索确认无残留 |
| 迁移后原 hard-coded 代码未删除导致逻辑双重执行 | 高 | 高 | 每个 hook 迁移完成后，对应原代码必须在同一个 commit 内删除，不允许分开提交；code review 以"原代码已删除"作为迁移完成的验收条件 |

---

## 9. 讨论检查清单（动手前必须对齐的事）

- [x] **范围**：分阶段，阶段 1 只做 Tool Hook + 迁移既有 4 处逻辑，阶段 2 做 Mailbox Hook 关闭级联爆炸 P0
- [x] **接口草案**（§2.1）：`ToolHook` 接口 5 个方法、`HookAction` 仅 Continue/Abort、全值传递、字符串直接匹配 — 全部对齐
- [x] **历史查询**（§2.6）：通过扩展 `TaskStore` 接口（`AppendToolCall` / `QueryToolCalls`）实现，hook 通过 `StoreHookView.GetToolCallHistory` 查询。详细规格见 §11.1
- [x] **依赖注入**（§5.6 / §11）：hook 通过构造时注入 `XxxHookView` 接口，不持有完整对象引用
- [x] **Priority 范围与约定**（§5.2）：[0, 1000]，0-100 系统级强制 hook，900-1000 观察类 hook，默认 500
- [x] **行哈希增强**（§6）：已确认，集成到 `read_file` 工具层，详见 `nextUpgrade_v3.md`
- [x] **与 v2 的顺序**（§7.4）：先做 hook 阶段 1，同时改 v2 的 ToolGroup 设计来适配。前置任务：对照 v2 §1.1 与本文档 §2.3 识别 hook 钩子点
- [x] **阶段 1 退出条件**：4 处迁移完成 + 既有测试全绿 + hook 框架自身单测 + "禁用所有 hook 等价于迁移前行为" 的开关验证
- [x] **阶段 2 退出条件**：3 个 mail hook（ChainDepthLimit / PerAgentDedup / WakeContextExpand）完成 + `mail_notifier_enabled=true` 重新启用 + KNOWN_ISSUES 中级联爆炸条目转为 ✅

**全部对齐，可以进入实现规划阶段。**

---

## 10. 下一步（实现路线）

### 10.1 前置任务（不写代码，仅文档更新）

- [x] **F1（2026-04-09 完成）**：完整对照分析 `nextUpgrade_v2.md` §1.1 ToolGroup 与本文档 §2.3 接入点。**关键发现**：
  - v2 §1.1 ToolGroup **已经全部实现**（`internal/tools/group.go` + 5 个 group 文件 + `worker.go:124-137` 已使用 `RegisterGroups`），v2 文档把它描述为"未来计划"是过时的
  - ToolGroup 是注册期抽象，hook 是运行时层，**两者完全正交**——hook 系统不需要为 v2 §1.1 预留任何字段或接口
  - 工具实现层面发现 2 个细节冲突，已分别解决：
    - **冲突 A（PathBoundary 路径标准化）**：`pathutil.ValidatePath` 同时做校验和路径标准化（相对→绝对）。Hook 拒绝 Replace 后无法把标准化路径写回 args。**决策 A1**：hook 只校验，工具内部保留 `pathutil.ValidatePath` 调用做标准化，性能代价 = 每次写文件多一次纯函数调用，可忽略
    - **冲突 B（ValidateExpectedHash 与 Roster 锁顺序耦合）**：当前 hash 校验在 Roster 锁内，hook 改为 pre 阶段后会移到锁外，引入微小 TOCTOU 窗口。**决策 B1**：接受这个微小窗口，因为单进程内所有写都走 Roster 锁，AgentGo 默认假设 ProjectRoot 没有外部并发修改，8-16 个代理环境下也很少出现小时间窗口内多代理改同一文件
  - **Scheduler 排除**：Scheduler 工具不走 `agent.NewLLMExecutor`，hook 不影响 Scheduler。已在 §2.3 末尾明确说明
  - 详见 hookSystem.md §2.3、§10.2 各 commit 的注脚

- [ ] **F2**：更新 `nextUpgrade_v2.md` §1.1
  - 在 §1.1 顶部加状态标记 ✅ 已完成
  - 修正 `LocalWriteGroup` 结构体定义，加入实际存在的 `Store` 和 `ProjectRoot` 字段
  - 修正 `MetaGroup` 结构体定义，加入实际存在的 `MaxDepth` 字段
  - 在 §1.1 末尾加一段 "Phase 1 hook 迁移后的工具内部变化"，说明 `pathutil` 保留双重校验、`recordArtifact` 删除、`hash` 校验删除并接受 TOCTOU 窗口、Roster 锁保留

### 10.2 阶段 1 实施步骤（按 commit 切分）

- [ ] **C1 — store 扩展**：`internal/store/iface.go` 加 `ToolCallRecord` 结构体（5 字段：Timestamp / AgentID / ToolName / Args / Success）+ `AppendToolCall(taskID, rec)` / `QueryToolCalls(taskID, toolName)` 两个新方法。`internal/store/memory.go` 实现这两个方法，使用 `map[taskID]map[toolName][]ToolCallRecord` 二级索引避免 O(N) 全扫，写入路径必须在 `MemoryTaskStore` 既有 `sync.RWMutex` 写锁下进行（防并行 goroutine 同时写同一 task）。新增 `internal/store/memory_test.go` 单测覆盖：单写、并发写同 task、按 toolName 过滤、不存在的 task 行为。**不接入任何 hook，store 层独立先行可单独 review。**
- [ ] **C2 — `internal/hook/` 包骨架**：新建 `internal/hook/tool.go`（`ToolHook` 接口、`ToolHookContext`、`ToolHookDecision`、`HookAction` 枚举仅 `Continue`/`Abort`、`ToolHookPhase` 字符串常量 `"preCall"`/`"postCall"`）+ `internal/hook/registry.go`（`ToolHookRegistry` 含 `Register` / `RunPre` / `RunPost`，按 Priority 升序遍历，pre 阶段 Abort 短路返回，post 阶段无返回值且不短路；每个 hook 调用包 `defer recover()`，panic 视为 Continue 并 emit log + trace event；`RunPre` / `RunPost` 内部对 `hctx.Args` 做浅拷贝防 hook 误改）。`internal/hook/registry_test.go` 单测覆盖：注册 + 重名拒绝 + Priority 排序 + Abort 短路 + panic 恢复 + Args 浅拷贝隔离。**不接入任何工具调用，本 commit 全部代码独立可单测。**
- [ ] **C3 — `StoreHookView` 接口**：新建 `internal/store/hookview.go` 定义 `StoreHookView` 接口（3 方法：`GetTask` / `AppendArtifact` / `GetToolCallHistory`，其中 `GetToolCallHistory` 委托给 C1 的 `QueryToolCalls`）。`MemoryTaskStore` 自动实现该接口（接口子集）。`internal/store/hookview_test.go` 单测验证 mock 替换可行 + 接口子集编译期保证（`var _ StoreHookView = (*MemoryTaskStore)(nil)`）。
- [ ] **C4 — `llm_executor.go` 接入点（关键 commit，需详细 review）**：
  - 修改 `agent.NewLLMExecutor` 签名：`NewLLMExecutor(client llm.Client, tools *ToolRegistry, hookReg *hook.ToolHookRegistry, storeView store.StoreHookView, systemPrompt ...string)`，新增 2 个参数（`hookReg` 和 `storeView`，两者均允许 nil）
  - 在 `llm_executor.go:147-203` 的并行 goroutine 内按 §2.3 末尾的"精确接入步骤"插入 hook 调用与 store 写入序列
  - hookReg=nil 时整个 hook 路径退化为 no-op，**这是回归验证的核心**：注释掉 bootstrap 中所有 `hookReg.Register(...)` 调用后，全测试套必须通过且行为字节级一致
  - 同步更新 `internal/worker/worker.go`、`internal/explorer/explorer.go`、`internal/agent/llm_executor_test.go` 中对 `NewLLMExecutor` 的所有调用（每处加 2 个参数）
  - **本 commit 不注册任何具体 hook，只把接入管道铺好**
- [ ] **C5 — 第一个迁移：`RecordArtifactHook`（最安全的开胃菜）**：
  - 新建 `internal/hook/builtin/record_artifact.go`，实现 `RecordArtifactHook`：`Phase=PostCall`、`Matches=write_file/edit_file`、`Priority=950`（观察类高位）、`Run` 在 `hctx.Err==nil` 时调用 `storeView.AppendArtifact(taskID, normalizedPath)`
  - 删除 `internal/tools/local_write.go:40-50` 的 `recordArtifact` 方法和 `internal/tools/local_write.go` 中所有调用点
  - 删除 `LocalWriteGroup` 结构体的 `Store` 和 `ProjectRoot` 字段（如果它们只是为 `recordArtifact` 服务的话——需要在 commit 前 grep 确认）
  - bootstrap 注册该 hook
  - **退出验收**：worker 包既有测试**未修改**通过；新增 1 个端到端测试验证"调用 write_file 后 task.Artifacts 包含该路径"
- [ ] **C6 — 第二个迁移：`PathBoundaryHook`（决策 A1：双重校验）**：
  - 新建 `internal/hook/builtin/path_boundary.go`，实现 `PathBoundaryHook`：`Phase=PreCall`、`Matches={read_file, list_dir, grep_search, glob_search, write_file, edit_file, run_shell}`、`Priority=10`（系统级最早）、`Run` 调用 `pathutil.ValidatePath(args["path"], projectRoot)`，错误时返回 Abort
  - **不删除工具内部的 `pathutil.ValidatePath` 调用**——保留作为路径标准化（相对→绝对）的实现，hook 只做校验。这是冲突 A 的 A1 解法：双重校验，hook 禁用时仍正确
  - 在 `local_read.go` / `local_write.go` / `shell.go` / `web.go` 内的所有 `pathutil.ValidatePath` 调用旁加注释 `// 注：此处保留是为了路径标准化，hook 端的 PathBoundaryHook 已做过校验`
  - **退出验收**：worker / explorer 既有测试**未修改**通过；新增 1 个测试验证"PathBoundaryHook 拒绝越界路径时工具不被调用"
- [ ] **C7 — 第三个迁移：`ValidateExpectedHashHook`（决策 B1：接受微小 TOCTOU）**：
  - 新建 `internal/hook/builtin/validate_expected_hash.go`，实现 `ValidateExpectedHashHook`：`Phase=PreCall`、`Matches=write_file/edit_file`、`Priority=20`、`Run` 检查 `args["expected_hash"]`，非空时计算当前文件 SHA256 并比对，不一致返回 Abort
  - 删除 `internal/tools/local_write.go` 内的 hash 校验段（约 20 行）
  - **冲突 B 的 B1 决策记录**：迁移后 hash 校验从 Roster 锁内移到 hook 阶段（锁外）。引入微小 TOCTOU 窗口：从 hook 校验 hash 到 tool 拿 Roster 锁后写入之间，文件可能被外部进程修改。在单进程 AgentGo 内此风险微秒级，8-16 个代理环境下也极少撞上同一时间窗口的同一文件
  - **不接受这个权衡的退路**：如未来发现 TOCTOU 真实复现，可以把 ValidateExpectedHashHook 退回 inline 实现，hook 系统设计不需要变
  - **退出验收**：worker 既有测试**未修改**通过；新增 1 个测试验证"hash 不匹配时 hook Abort 且 write_file 不被执行"
- [ ] **C8 — 第四个 hook：`RequireReadBeforeWriteHook`（注意：这是新增不是迁移）**：
  - **特殊性**：当前代码里**没有**"先读后写"的硬性检查——它只是 worker prompt 里的一句软约束。所以 C8 是**新增 hook 而非迁移既有逻辑**，没有"原代码删除"这一项
  - 新建 `internal/hook/builtin/require_read_before_write.go`，实现 `RequireReadBeforeWriteHook`：`Phase=PreCall`、`Matches=write_file/edit_file`、`Priority=30`、`Run` 调用 `storeView.GetToolCallHistory(taskID)` 过滤出 `read_file` 调用，检查目标 path 是否曾被读过（args 里 `path` 字段精确匹配），未读过则返回 Abort
  - 这是**第一个真正用到 `StoreHookView.GetToolCallHistory` 的 hook**，验证整个 store→hook 查询链路工作
  - **退出验收特殊**：不能用"既有测试不修改通过"作为验收（既有测试没有"先读后写"行为，禁用 hook 时与启用 hook 时行为本就不同）。验收方式：
    - 新增 1 个测试构造"先 write 后 read"场景，验证 write 被 hook 拒绝
    - 新增 1 个测试构造"先 read 后 write"场景，验证 write 通过
    - 新增 1 个测试验证"被拒绝的 write 调用也被记录到 ToolCallRecord 且 Success=false"

### 10.3 阶段 1 退出验收

- [ ] 3 个迁移 hook（C5/C6/C7）的对应 hard-coded 代码已全部删除（同 commit 内），不允许双重执行
  - **C6 例外**：`pathutil.ValidatePath` 调用因决策 A1 故意保留作为路径标准化实现，但工具内部不再因校验失败而 return error（校验失败由 hook Abort 处理）
- [ ] C8 `RequireReadBeforeWriteHook` 是**新增 hook 不是迁移**，无原代码删除项
- [ ] worker / explorer / scheduler 既有测试**未修改**通过（C5/C6/C7 适用，C8 不适用——见 §10.2 C8 退出验收特殊条款）
- [ ] hook 包自身单测覆盖率 ≥ 80%
- [ ] **"禁用所有 hook"开关回归验证**（决定阶段 1 成败的关键）：注释掉 bootstrap 中的所有 `hookReg.Register` 调用后：
  - C5/C6/C7 路径：全测试套必须通过且行为与改动前**字节级一致**
  - C8 路径：禁用 hook 后，"先写后读"行为不再被拦截，回到改动前的"由 prompt 软约束"状态——这是预期，C8 测试应跳过此场景

### 10.4 阶段 2 启动条件

阶段 1 退出验收全部通过后，回到本文档 §3，确认 mailbox hook 设计仍然成立，然后开始阶段 2 的 commit 切分。

---

## 11. 原有系统机制的对接接口改造

Hook System 需要访问系统中已有的三个核心机制。根据"对接方法用于上下文注入"的原则，每个机制应各自留一个专用接口，hook 通过构造时注入拿到只读视图，不直接持有完整对象引用。

### 11.1 公告板（Store）

Store 是 hook 访问频率最高的机制，阶段 1 的 `RecordArtifactHook` 和 `RequireReadBeforeWriteHook` 都依赖它。

#### 11.1.1 hook 端的对接接口

Hook 在构造时拿到的只读视图（`AppendArtifact` 是 postCall hook 的唯一写操作例外）：

```go
// internal/store/hookview.go
type StoreHookView interface {
    GetTask(taskID string) (Task, bool)                  // 读取任务当前状态、Artifacts
    AppendArtifact(taskID string, path string) error     // RecordArtifactHook 写入产物事实
    GetToolCallHistory(taskID string) []ToolCallRecord   // RequireReadBeforeWriteHook 查历史
}
```

`MemoryTaskStore` 直接实现 `StoreHookView`（接口子集），hook 构造函数接收 `StoreHookView` 类型而非具体实现，测试时可传 mock。

#### 11.1.2 store 底层数据结构扩展

为支持 `GetToolCallHistory`，`TaskStore` 接口需要新增 2 个公开方法和 1 个新数据结构：

```go
// internal/store/iface.go 新增
type ToolCallRecord struct {
    Timestamp time.Time
    AgentID   string
    ToolName  string
    Args      map[string]any
    Success   bool   // 区分工具调用是否成功（hook 决定是否计入"先读"）
}

type TaskStore interface {
    // ... 既有方法
    AppendToolCall(taskID string, rec ToolCallRecord) error
    QueryToolCalls(taskID string, toolName string) ([]ToolCallRecord, error)
}
```

`StoreHookView.GetToolCallHistory` 在内部委托给 `QueryToolCalls`（不带 toolName 过滤即返回全部）。

#### 11.1.3 自动写入点

`AppendToolCall` 由 `internal/agent/llm_executor.go` 在每次工具调用之后、`RunPost` 之前自动写入。worker / explorer 不感知该字段，hook 也不能主动写入（只能查询）。

**重要细节**：

- **被 hook 拒绝的调用也记录**：当 pre hook 返回 `Abort`、`tools.Dispatch` 未被实际调用时，仍然要写入一条 `ToolCallRecord` 且 `Success=false`。理由：(a) 让历史与 LLM 看到的对话记录一致——LLM 看到 `[hook 拒绝]` 结果，那 `GetToolCallHistory` 也应反映这次失败的尝试；(b) 让 hook 可以做"该 agent 已被同一 hook 拒绝过 N 次"的二阶决策（如未来加 cooldown）
- **Scheduler 工具不被记录**：Scheduler 的 `publish_task` / `cancel_task` / `report_done` / `send_message` 由 `internal/scheduler/scheduler.go` 自有的 `dispatchTool` 处理，**不经过 `agent.NewLLMExecutor`**，因此不会触发 `AppendToolCall`。这是设计上的边界，详见 §2.3 末尾"关键豁免"
- **写入时机**：`AppendToolCall` 在 `tools.Dispatch` 返回之后、`hookReg.RunPost` 之前调用——这样 post hook 可以通过 `GetToolCallHistory` 看到刚刚结束的调用（包括自身）；而 pre hook 在 `Dispatch` 之前查询时，看到的不包括当前调用（避免"自己引用自己"）

#### 11.1.4 性能与生命周期

- 内存存储，程序退出清空。崩溃重启历史丢失属预期行为
- `MemoryTaskStore` 内部用 `map[taskID]map[toolName][]ToolCallRecord` 二级索引，避免 hook 在每次工具调用前做 O(N) 全量扫描
- 任务完成 / 失败 / 取消进入 FIFO 淘汰队列时，`ToolCallRecord` 一并淘汰，不需要单独清理路径
- **并发安全**：`llm_executor.go` 在并行 goroutine 中调用工具（一个 LLM 响应可能同时跑 5 个 tool call），每个 goroutine 都会触发 `AppendToolCall`。`MemoryTaskStore.AppendToolCall` 必须在既有 `sync.RWMutex` 的**写锁**下进行（不能用读锁，因为要修改 map）。读路径 `QueryToolCalls` 用读锁。C1 commit 的单测必须包含 "N 个 goroutine 并行 AppendToolCall 同一 task" 的并发场景

### 11.2 Trace 系统

所有 hook 触发时都应该 emit 一条 trace event，用于事后审计。Trace 系统需要新增一个 event 类型并暴露写入接口：

```go
// 新增 event 类型
const KindHookFired = "hook_fired"

type TraceHookView interface {
    EmitHookFired(taskID string, hookName string, phase ToolHookPhase, action HookAction, reason string)
}
```

这与 §7.1 的设计一致：hook 防止问题发生，trace 记录已发生的事，二者互补。

### 11.3 邮箱注册表（Mailbox Registry）

阶段 2 的 Mailbox Hook 直接依赖邮箱机制。需要暴露的对接接口：

```go
type MailboxHookView interface {
    HasPendingMail(agentID string) bool                 // PerAgentDedupHook：检查是否有未读邮件
    GetRecentMessages(agentID string, n int) []Message  // WakeContextExpandHook：读取前 N 条邮件摘要
}
```

> 注：`ReplyCooldownHook` 已在 §3.4 放弃（精确哈希对 LLM 生成消息无效，词嵌入过重），因此 `GetSentHashes` 不在阶段 2 范围。如未来引入新的 reply 抑制策略再追加。

### 11.4 改造原则

- 每个机制只新增一个 `XxxHookView` 接口，不修改原有接口
- 原有对象实现该接口，hook 构造时传入接口而非具体类型，测试时可传 mock
- 接口方法全部只读（`AppendArtifact` 除外，它是 postCall hook 的唯一写操作），不暴露任何状态变更能力
