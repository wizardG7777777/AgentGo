# MemoryManageSystem：v5 记忆管理系统

> **状态**：📋 起草中（2026-04-30 从 nextUpgrade_v5.md §13 提升至 v5 主线）
> **优先级**：P0（v5 关键架构升级，废止 team-awareness 临时方案 + 取代 ReactiveSystem 原 Provider 抽象）
> **关联文档**：
> - [ReactiveSystem.md](ReactiveSystem.md)（原 Provider 抽象已被本模块替代；后续配套讨论中 Aggregator 也下放为 Mailbox 子系统内部固定机制——4 类角色最终缩为 **2 类核心：Reactor / Gate**）
> - [nextUpgrade_v5.md §13](nextUpgrade_v5.md)（本文档的设计源头；§13 已被本文档取代，仅保留指针）
> - [InterfaceDesign.md](InterfaceDesign.md)（Memory 接口规约由本文档定稿）
> - [TraceUpgrade.md](TraceUpgrade.md)（`memory_put` / `memory_query` 新 EventKind 由 Phase 2 落地）
> **关键判决**：
> - team-awareness 三个 hook **直接删除**，不重命名为 Provider
> - ReactiveSystem.md 原 Provider 抽象**彻底废弃**
> - AgentHook 子系统在 v5 阶段命运另议（详见 §5）

---

## 0. 模块命题

V4 时代的 `TeamAwarenessHook` 是 V2 早期的临时修复——把团队感知信息硬编码注入 LLM history。它的实质问题不是"叫 Hook 不合适"，而是**整个机制的形态都错了**：

> 所有"注入内容"都是临时文本，系统重启后项目知识全部丢失；agent 被多个 hook 反复"塞"东西，而不是主动从知识源拉取；信息复用、跨会话保留、向量检索等持久化记忆能力完全缺失。

ReactiveSystem.md 原计划的 Provider 抽象（v5 4 类角色之一）本质是给"V2 临时修复"换个名字——没有解决任何根本问题。**本模块取代 Provider 方案，是 v5 阶段对"agent 长期记忆"这个空白的真正填补**。

设计哲学（4 条）：

1. **记忆是一等公民**：跨任务、跨会话、跨进程的项目知识应当持久化
2. **Agent 主动拉取，而非被动注入**：Agent 在 `processTask` 入口从 `MemoryStore` 读取，不再被 N 个 hook 反复塞东西
3. **作用域分层**：Process / Session / Project 三档，分别对应内存 / 会话文件 / 项目持久化
4. **写入与读取解耦**：写入侧由系统组件（Roster 监听 / 团队状态变更通知 / 用户输入 / Reactor）触发；读取侧由 Agent 框架层调度

---

## 1. 背景：Agent Hook 的职责漂移

承接 [nextUpgrade_v5.md §13.1](nextUpgrade_v5.md#L1086)：

`internal/hook/agent.go` 在 Sprint 1（2026-04-12）落地时，Agent Hook 被实现为**上下文注入框架**（`TeamAwarenessHook` 的 `PhaseTaskStart` / `PhaseLoopPre` 注入团队快照、文件占用、目标锚点）。**这与早期 RFC 中 "Trigger" 层（事件 → 动态拉起 Agent → 工作 → 销毁）的设计意图发生了漂移**。

当前 Agent Hook 造成的三个架构负担：

| 负担 | 表现 | 根因 |
|------|------|------|
| **语义错位** | `AgentHook` 名字暗示 "Agent 生命周期 hook"，实际做的是 "LLM 上下文注入" | 命名与职责不匹配 |
| **职责过载** | `TeamAwarenessHook` 同时处理 TeamSnapshot + FileAwareness + GoalAnchor + token 预算截断 | 一个 hook 承载三种不同维度的感知信息 |
| **记忆缺失** | 所有 "注入内容" 都是临时文本，系统重启后项目知识全部丢失 | 没有持久化的 Memory 层 |

InterfaceDesign.md 明确标注过 `#memory——本项目无对等的记忆持久化模块`。`TransferNote` 和 `LastHistory` 是任务级交接备忘，不构成真正的长期记忆。

---

## 2. 核心设计决策

| # | 决策 | 说明 |
|---|---|---|
| **D1** | **新建独立 `internal/memory/` 包** | 负责长短期记忆的存储、检索、更新。Agent 从 Memory System 读取注入内容，而非从 Agent Hook |
| **D2** | **TeamAwarenessHook 三 section 各自迁移** | TeamSnapshot / FileAwareness / GoalAnchor 不再作为 hook 注入，按各自语义走不同迁移路径（详见 §4） |
| **D3** | **三档作用域 + 三种存储后端** | Process（内存 map）/ Session（`.agentgo/sessions/sess-<id>/memory.jsonl`）/ Project（`.agentgo/memory/`） |
| **D4** | **Memory 抽象不依赖 LLM** | `MemoryStore` 是纯存储层，文本检索为基本能力；向量检索作为可选扩展（v5.x 引入） |
| **D5** | **AgentHook 子系统的命运另议** | team-awareness 删除后 AgentHookRegistry 实际为空。子系统是否保留 / 是否合并到 Reactor 见 §5 |
| **D6** | **ReactiveSystem 原 Provider 抽象废弃 + Aggregator 下放** | ReactiveSystem.md §3-§4 的"4 类角色"经两轮修订缩为"**2 类核心角色**（Reactor / Gate）"。Provider 相关章节同步删除（本文档承接）；Aggregator 下放为 Mailbox 子系统内部固定机制（不立顶层抽象、无独立 Registry、不暴露外部配置）|

---

## 3. Memory System 架构

### 3.1 作用域分层

```go
type MemoryScope int

const (
    ScopeProcess   MemoryScope = iota // 进程级：随系统重启清空
    ScopeSession                      // 会话级：session 结束清空
    ScopeProject                      // 项目级：持久化到磁盘，跨会话保留
)
```

| 作用域 | 存储位置 | 内容示例 | 生命周期 |
|---|---|---|---|
| **Process** | 内存（`map[string]MemoryEntry`） | 当前活跃 Agent 状态、board snapshot 缓存、实时文件占用 | 进程级 |
| **Session** | `.agentgo/sessions/sess-<id>/memory.jsonl` | 会话内积累的项目洞察、用户偏好、已确认事实、本次会话的约束调整 | Session 级 |
| **Project** | `.agentgo/memory/`（JSONL 或 SQLite） | 项目约束文档（"禁止直接操作 DB"）、代码规范、API 使用约定、常见错误模式 | 持久化（跨会话）|

### 3.2 记忆种类

```go
type MemoryKind string

const (
    KindConstraint   MemoryKind = "constraint"   // 项目级约束文档
    KindLearning     MemoryKind = "learning"     // 学习到的经验（失败/成功总结）
    KindPattern      MemoryKind = "pattern"      // 代码模式/项目结构洞察
    KindContext      MemoryKind = "context"      // 进程级上下文（TeamSnapshot / FileAwareness 等）
    KindAgentState   MemoryKind = "agent_state"  // Agent 级状态快照
)
```

### 3.3 存储接口

```go
type MemoryStore interface {
    // 写入
    Put(ctx context.Context, entry MemoryEntry) error

    // 文本检索 + 标签过滤
    Query(ctx context.Context, scope MemoryScope, kind MemoryKind, query string, limit int) ([]MemoryEntry, error)

    // 向量检索（可选，v5.x 引入）
    QueryByVector(ctx context.Context, scope MemoryScope, embedding []float32, limit int) ([]MemoryEntry, error)

    // 删除
    Delete(ctx context.Context, id string) error

    // 按作用域清空
    Clear(ctx context.Context, scope MemoryScope) error
}

type MemoryEntry struct {
    ID          string
    Scope       MemoryScope
    Kind        MemoryKind
    Key         string      // 检索键
    Content     string      // 文本内容（或序列化后的结构化数据）
    Embedding   []float32   // 可选：向量嵌入
    Tags        []string    // 标签
    Source      string      // 来源（agentID / taskID / user）
    CreatedAt   time.Time
    UpdatedAt   time.Time
    AccessCount int         // 访问频次（LRU 依据）
}
```

### 3.4 Agent 侧的读取接入点

Memory System 的读取发生在 **Agent 框架层**，不再走 Hook：

```go
// internal/agent/agent.go: processTask
func (a *Agent) processTask(ctx context.Context, taskID string) {
    // 替代原有的 runAgentInject(PhaseTaskStart)
    if a.Memory != nil {
        if entries, _ := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext,
            "team_snapshot", 1); len(entries) > 0 {
            history = append(history, HistoryEntry{IncomingMail: entries[0].Content})
        }
        if entries, _ := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext,
            "file_awareness", 1); len(entries) > 0 {
            history = append(history, HistoryEntry{IncomingMail: entries[0].Content})
        }
    }
    // ... 进入 ReAct 循环
}
```

`Agent` 结构体新增字段：

```go
type Agent struct {
    // ... 现有字段 ...
    Memory memory.MemoryStore // nil 时退化为 noop（即不读取任何记忆，行为等价于 v4 无 team-awareness）
}
```

### 3.5 当前代码对记忆扩展的约束

承接 [§13.6](nextUpgrade_v5.md#L1321)，Memory System 落地时需要正视的现状约束：

| 约束 | 影响 | 缓解方案 |
|---|---|---|
| `AgentHookContext.Store` 是只读视图 | 旧 Agent Hook 无法写入记忆 | Memory System 独立存储，不依赖 Hook |
| `AgentHookResult.InjectContent` 是纯文本 | 结构化记忆需序列化 | Memory System 提供 `formatForLLM(entry) string` 统一序列化 |
| Agent 结构体无 Memory 字段 | Agent 无法直接访问记忆 | 新增 `Agent.Memory memory.MemoryStore`，nil 安全 |
| `HistoryEntry` 只有 `IncomingMail` | 记忆载体单一 | 保持 `IncomingMail` 作为统一注入载体，Memory System 负责格式化 |
| `TransferNote` 是任务级字符串 | 无法承载多维度跨任务记忆 | `TransferNote` 保留作为任务交接载体；跨任务记忆走 Memory System |
| `AgentHook` 被要求无状态 | Hook 不能维护记忆缓存 | Memory System 是外部有状态服务，调用方只持有客户端引用 |

---

## 4. team-awareness 三 section 的迁移路径

承接 [§13.5](nextUpgrade_v5.md#L1290)：

| Section | 当前位置 | 迁移目标 | 理由 |
|---|---|---|---|
| **TeamSnapshot**（队友状态）| `PhaseTaskStart` / `PhaseLoopPre` 注入 | **Process Memory** 的定时写入 + Agent `processTask` 入口读取 | 团队状态是全局信息，不应在每个 Agent 的每轮循环里重复生成 |
| **FileAwareness**（文件占用）| `PhaseTaskStart` / `PhaseLoopPre` 注入 | **Process Memory**：Roster 监听写入 + Agent 按需读取 | Roster 变更时实时更新 Memory，Agent 读取缓存而非直接调 `ListClaims` |
| **GoalAnchor**（目标锚定）| `PhaseTaskStart` / `PhaseLoopPre` 注入 | **直接删除，不迁移** | `task.Description` 本身就是目标，注入 GoalAnchor 是冗余——Agent 看 task 描述就够了 |

迁移后 Agent 的 `processTask` 入口替换：

```go
// 旧逻辑（Agent Hook 注入）—— v5 删除
if injected := a.runAgentInject(ctx, hook.PhaseTaskStart, taskID, -1, false); injected != "" {
    history = append(history, HistoryEntry{IncomingMail: injected})
}

// 新逻辑（Memory System 读取）—— v5 引入
if a.Memory != nil {
    if entries, _ := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext, "team_snapshot", 1); len(entries) > 0 {
        history = append(history, HistoryEntry{IncomingMail: entries[0].Content})
    }
    if entries, _ := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext, "file_awareness", 1); len(entries) > 0 {
        history = append(history, HistoryEntry{IncomingMail: entries[0].Content})
    }
}
```

---

## 5. AgentHook 子系统的命运（D5 展开）

team-awareness 三个 hook 删除后，**`AgentHookRegistry` 在事实上为空**——v4 时代 13 个 hook 的清单里，`team-awareness-task-start` 和 `team-awareness-loop-pre` 是仅有的 2 个 AgentHook。所以 v5 阶段 AgentHook 子系统该如何处置，需要专门拍板。

### 5.1 三种处置方案

| 方案 | 内容 | 优点 | 缺点 |
|---|---|---|---|
| **A. 完全删除 AgentHook 子系统** | 移除 `internal/hook/agent.go` + `AgentHookRegistry` + 4 个 Phase 枚举 | 代码最干净；命名空间彻底清理 | 失去未来"在 ReactLoop 节奏点做副作用"的扩展位（虽然 Reactor 可承接大部分场景）|
| **B. 保留 AgentHook 子系统，但仅保留 LoopPost / TaskEnd 两个观察类 Phase** | TaskStart / LoopPre 删除（注入语义被 Memory System 取代）；LoopPost / TaskEnd 保留作为"纯观察 hook" | 给未来"ReactLoop 节奏点观察"留扩展位 | 子系统空壳运行（v5 无任何 hook 注册）|
| **C. AgentHook 子系统合并入 Reactor 子系统** | LoopPost / TaskEnd 改造为 Reactor 的事件订阅（`KindReactLoopIterationEnd` / `KindTaskEnd`）；AgentHookRegistry 删除 | 抽象统一（v4 隐含的"hook 二分"被显式归并）；与 Reactor 的"状态变化后副作用"语义对齐 | Reactor 事件清单要新增 2 个事件；ReactLoop 节奏点暴露成 trace 事件需要 Phase 2 配套 |

### 5.2 倾向方案 C

理由：

1. AgentHook 当初的本意是"在 ReactLoop 关键时点跑代码"——这跟 Reactor 的"事件 → 程序化反应"语义本就同源
2. v5 已经在 Reactor 子系统投入大量设计（动作语言 / 配置粒度 / 执行模型 / 触发事件清单）—— LoopPost / TaskEnd 改造为 Reactor 事件的边际成本极低
3. 减少子系统数量 = 减少认知负担，与 ReactiveSystem.md "4 类角色"两轮瘦身为"2 类核心角色（Reactor + Gate）"的精神一致

但方案 C 需要 Reactor 事件清单加 2 个事件（`KindReactLoopIterationEnd` / `KindTaskEnd`）。这是 ReactiveSystem.md §6.4 已决议的"5 + 1 + 3"清单的进一步扩展——拍板时机看 Phase 1 启动具体安排。

**v5 范围内**：方案 C 落地的工作量约 100 行（Reactor 事件 emit + AgentHookRegistry 删除）。建议在 Phase 1 命名空间清理时一并处置。

---

## 6. 与 ReactiveSystem.md 的协同

本模块对 ReactiveSystem.md 产生连锁修改（已在 D6 概括）：

| ReactiveSystem.md 章节 | 修改内容 | 修改原因 |
|---|---|---|
| 命名决议表 | 删除 Provider 行 + Aggregator 标注下放 | Provider 废弃 + Aggregator 下放 |
| §0 关键语义边界 | 删除 Provider 提法 + 改"Aggregator 是辅助角色"为"邮件聚合属 Mailbox 内部" | 同上 |
| §0 判别新代码归属速查表 | 删除 Provider 分支；Aggregator 分支改为"扩 Mailbox 子系统" | 范围调整 |
| §1.1 v4 隐性问题 | "team-awareness 命名误导" 段落改为引用本文档 §1 | 团队感知问题归本文档治理 |
| §2.3 Agent Hook 现状盘点 | 标注"v5 删除（迁 MemoryManageSystem.md）" | 不再重命名为 Provider |
| §3 角色表 | 删 Provider 行 + Aggregator 行加"v5 下放" 标记 | 顶层只剩 Reactor / Gate |
| §3.1 逐项归类 | A1/A2 改"删除（迁 MemoryManageSystem.md）"；M4 改"留在 Mailbox 子系统内部" | 顶层抽象瘦身 |
| §4.1 拆分前后对比框图 | 删除 ProviderRegistry + AggregatorRegistry 两个框 | Registry 不立 |
| §4.2 职责边界表 | 删除 Provider + Aggregator 行 | 同上 |
| §4.3 拆几个 Registry | "拆四个" → "拆两个"，记录两轮修订路径 | Q1 措辞更新 |
| §5.3 team-awareness 治理 | 改为指针：详见 MemoryManageSystem.md | 治理路径变化 |
| §10.1 Phase 1 框图 | "team-awareness → Provider" 改为 "→ 删除"；"wake-context-expand → Aggregator" 改为 "→ 留在 Mailbox 子系统内部" | 两条迁移路径同步更新 |
| §11 关系表 | 加 MemoryManageSystem.md 一行 | 配套关系 |
| §9 Q1 决议 | "拆 4 个 Registry" → "拆 2 个 Registry" | Q1 两轮修订后的最终态 |
| 附录 A | A1/A2 标"v5 删除"；M4 标"迁 internal/mailbox/" | 实施位置更新 |

第 2 步起会执行所有这些修改。

---

## 7. 实施 Phases

| Phase | 工作 | 依赖 | 工作量估算 |
|---|---|---|---|
| **MM1** | `internal/memory/` 包 + `MemoryStore` interface + `MemoryEntry` struct | 无 | ~150 行 |
| **MM2** | `ScopeProcess` 内存实现（`processStore` 类型）| MM1 | ~200 行 + 测试 |
| **MM3** | TeamSnapshot 写入侧：scheduler / worker 在团队状态变化时调 `Memory.Put` | MM2 | ~80 行 |
| **MM4** | FileAwareness 写入侧：Roster 监听 → `Memory.Put`（事件驱动）| MM2 | ~100 行 + Roster 加 listener 钩子 |
| **MM5** | Agent.processTask 读取侧：`Memory.Query` 替换 `runAgentInject(PhaseTaskStart)` | MM2 | ~50 行 |
| **MM6** | 删除 `team-awareness-task-start` / `team-awareness-loop-pre` + GoalAnchor + bootstrap 注册去除 | MM3+MM4+MM5 | ~30 行删除 |
| **MM7** | AgentHook 子系统命运处置（方案 C：合并入 Reactor）| MM6 + ReactiveSystem Phase 1 | ~100 行 |
| **MM8** | `ScopeSession` 文件存储后端（`.agentgo/sessions/sess-<id>/memory.jsonl`）| MM2 | ~150 行 + 测试 |
| **MM9** | `ScopeProject` 持久化后端（`.agentgo/memory/`）| MM2 | ~200 行 + 测试 |

**v5 首版最小可发布集合**：MM1-MM7（仅 Process 作用域 + 替换 team-awareness + AgentHook 处置）。MM8-MM9 可作为 v5 增量按需推进。

**强制依赖链**：MM1 → MM2 → MM3+MM4+MM5（并行）→ MM6 → MM7

---

## 8. 不在本模块范围

- **Trigger System 重构**（[nextUpgrade_v5.md §13.4](nextUpgrade_v5.md#L1207) "AgentHook 重新定位为 Trigger System"）—— **仍归 V6 方向**，本模块不涉及。Trigger System 与 Memory System 是 §13 的两个并列议题，提前到 V5 的只有 Memory 部分
- **常驻 Agent 与临时 Agent 共存**（[§13.4.3 AgentPool](nextUpgrade_v5.md#L1264)）—— 归 Trigger System，留 V6
- **向量检索**（`QueryByVector`）—— 接口预留，v5.x 引入实现
- **`MemoryEntry.Embedding` 字段填充** —— 同上
- **从 LLM 自动生成 Memory（如对话总结自动写入 Project Memory）** —— v5 不做；v5.x 探索时再讨论触发条件
- **跨进程 Memory 共享（如多个 AgentGo 实例共享同一 Project Memory）** —— v5 不考虑，仅单进程
- **Memory 的事后检索 UI** —— `internal/cli` 当前不提供 `/memory` 命令查看历史记忆，CLI 升级时再考虑（详见 InterfaceDesign.md §CLI 交互层）

---

## 附录：与 §13 的差异

本文档基于 nextUpgrade_v5.md §13 改写，主要差异：

1. **范围收窄**：§13 含 Memory + Trigger 两个子提案，本文档**仅承接 Memory**；Trigger 仍归 V6
2. **状态升级**：§13 标 "V6 方向 / 设计阶段"，本文档标 "v5 主线 / 起草中"
3. **新增 §6 协同章**：明确对 ReactiveSystem.md 的连锁修改清单（Provider 删除）
4. **新增 §5 AgentHook 处置**：§13 仅说 D1 重新定位，本文档展开三个候选方案 + 倾向方案 C
5. **新增 §7 实施 Phases**：§13 没给具体 phase 序列，本文档补 MM1-MM9 + 最小可发布集合定义
