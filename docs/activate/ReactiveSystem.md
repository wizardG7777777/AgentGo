# ReactiveSystem：Reactor 驱动的状态响应架构

> **状态**：✅ **设计决议完成 + 实施全部落地**（2026-04-30 至 2026-05-01 设计阶段；2026-05-01 实施阶段 Phase 1-7 全部完成）
>
> **实施进度（2026-05-01 终态）**：
> - **Phase 1**（命名空间清理 / Memory + Gate 统一）：✅ 完成（gate.Registry / memory.Store / 9 hook → Gate 适配）
> - **Phase 2**（TraceUpgrade）：✅ 完成（schema B 嵌套子结构体 + 4 个新 EventKind + CLI viewer 适配 + 4 条新启发式异常检测）
> - **Phase 3**（Agent 状态显式化）：✅ 完成（4 状态枚举 + SetState/mustSetState + 6 个调用点 + KindAgentStateChanged）
> - **Phase 4**（Reactor 单元）：✅ 完成（reactor.Registry + 4 个内置 reactor：record-artifact / task-end-callback / trace-history-event / read-set-write；3 个延后项均已收尾：SetState 运行期 guard / task-end-callback 迁移 holder 副作用 / MM7 AgentHook 子系统整体删除）
> - **Phase 5**（用户 YAML schema）：✅ 完成（S1-S7 全部落地：prompt 加载 + 模板 + when 条件 + publish_task + 独立 LLM client + invoke_llm + spawn_agent + via_translator）
> - **Phase 6**（ReadSet 显式化）：✅ 完成（Task.ReadSet 字段 + read-set-write Reactor + require-read-before-write Gate 改读 ReadSet）
> - **Phase 7**（端到端烟测 + 文档同步）：✅ 完成（v5_phase7_e2e_test 跨 Phase 协作烟测 + 本文档同步）
>
> **v5.x 增量已落地**：
> - §6.1 `call:` 动作（B 选项 — 内置工具调用 verb）— v1 仅支持 `call: send_message`，args 模板化
> - §6.2 per-kind reactors（reactor 配置粒度细化）— 顶层 `kind:` 字段过滤；spawn agents 通过 `spawn.Manager.KindOf` 继承 base_kind 路由（§6.2.4）
>
> **仍 fail-fast 的占位字段**（schema 接受、运行期报错）：
> - `lifecycle: persistent`（spawn_agent 长期形态）
> - `prompt.url` / `prompt.inline`（PromptSpec 占位字段）
> - `call:` 其他工具（read_file / web_search 等需要 agent 上下文，按需逐个加白名单）
>
> **历史决议存档**：
> **关键修订（2026-04-30）**：
> - 原"4 类角色"缩为"**2 类核心角色（Reactor + Gate）**"——Provider 废弃由 [MemoryManageSystem.md](MemoryManageSystem.md) 承接（取代 V2 时代的 team-awareness 临时方案）；Aggregator 下放为 Mailbox 子系统内部固定机制
> - GateRegistry 真正跨域统一：单一 `internal/gate/` 包 + 接口式 Context（详见 §4.4）
> - Phase 4 内置 Reactor 落地子集定为 3 个最小覆盖矩阵（详见 §5.1）
> - Shell 工具改造规格独立成 [ToolUpgradePlan.md](ToolUpgradePlan.md)；引入 TimeoutHandler 第三类决策点
>
> **关键补充（2026-05-01）**：
> - TraceUpgrade.md spec 定稿（schema B 嵌套子结构体 + 4 个新 EventKind + CLI viewer 适配）—— Phase 2 不再硬阻塞
> - §6.6 ReactorRegistry 完整接口形态补齐（与 §4.4 对称的接口契约 + 不含实现细节）
> - §5.2.1 ReadSet 数据结构与触发事件初版草案补齐（`Task.ReadSet` + 复用 `KindToolResult` + Gate 走 StoreView）
> **优先级**：P1(v5 核心重构之一，涉及拦截层瘦身 + Reactor 引入 + 命名空间清理)
> **前置依赖**：v4 §11 统一 Agent 声明式配置（✅ 已完成 2026-04-26）
> **关联文档**：
> - `nextUpgrade_v4.md` §7 Hashline / §10 Did-You-Mean（依赖现存 Gate 类组件）
> - `nextUpgrade_v5.md`（Scheduler 汇报分层 —— 平行模块，不冲突）
> - `TraceUpgrade.md`（事件 payload 结构化升级，本模块的前置基础设施）
> - [ToolUpgradePlan.md](ToolUpgradePlan.md)（配套文档：Shell 工具改造规格 —— 原 §7.4 的命令名单/UI/持久化/超时等实施细节迁移于此）
> - [MemoryManageSystem.md](MemoryManageSystem.md)（**配套文档：取代原 Provider 抽象**——team-awareness 删除后由 Memory System 承接长期记忆与上下文供给；本模块的"4 类角色"经讨论后已缩为"**2 类核心角色（Reactor + Gate）**"，Aggregator 下放为 Mailbox 内部固定机制不再立顶层）
> **关键转折**：把 trace 事件流从"只能记录"升级为"可触发用户配置的程序化逻辑"

---

## 命名决议

本模块在筹备阶段曾命名 `HookSystemUpgrade`，但调研后发现"Hook"一词在系统里覆盖了多类完全不同的职责，继续沿用会让 v5 的设计意图被误读。**v5 起整套架构改称 `ReactiveSystem`**——以"系统对状态变化的程序化反应能力"为核心命题。

> **2026-04-30 重大调整**：原计划的 4 类角色（Reactor / Gate / Provider / Aggregator）经多轮讨论后**缩减为 2 类核心角色（Reactor + Gate）**：
> - Provider 抽象彻底废弃，team-awareness 类需求由 [MemoryManageSystem.md](MemoryManageSystem.md) 承接
> - Aggregator 下放为 Mailbox 子系统内部的固定聚合机制（`wake-context-expand` 留在原位作为 Mailbox 内部组件），**不立顶层抽象、不暴露外部配置、无独立 Registry**——理由：当前只有 1 个实现且未来 1-2 年无第二个用例的迹象，强行立顶层是过度设计
>
> 下方表格已反映 2 类核心角色 + Mailbox 内部聚合机制的最终形态。

| 概念层级 | v5 命名 | 语义 | v4 时代对应物 |
|---|---|---|---|
| 模块总称 | **ReactiveSystem** | 系统对内部事件流的响应能力总和 | (无统一总称，散落三套 Hook Registry) |
| **核心单元** | **Reactor** | **处理状态发生变化之后的所有程序化逻辑** | 仅 1 个反模式实例（`record-artifact` 错位在 PostCall）|
| 配角 | Gate | 事前决策门，可否决动作 | 现有 9 个拦截 Hook |
| ~~配角~~（已下放，不立顶层）| ~~Aggregator~~ | ~~多事件 → 单事件的语义合并~~ | 现有 1 个 wake-context-expand Hook —— **v5 留在 Mailbox 子系统内部作为固定聚合机制，不再立顶层抽象、无独立 Registry、不暴露外部配置** |
| ~~配角~~（已废弃）| ~~Provider~~ | ~~ReactLoop 节奏点的 LLM 上下文供给~~ | 现有 2 个 team-awareness Hook（**v5 直接删除，迁 MemoryManageSystem**）|

**关键语义边界**：
- **Reactor 专司"状态变化之后"**——状态机已经走完，Reactor 不能否决，只能反应
- **Gate 专司"动作发生之前"**——可以 Abort 阻止动作发生，是决策门
- 两者互不替代：Gate 是事前守门人，Reactor 是事后响应者
- 邮件聚合（旧 Aggregator）属于 Mailbox 子系统的固定内部机制，与 ReactiveSystem 顶层抽象无关——本模块讨论范围不涵盖

---

## 0. 核心原则（不可妥协）

本模块在动笔前确立以下原则，作为后续所有设计决策的硬性约束。任一原则被破坏即视为架构事故，需要项目级评审。

### 原则 1：状态转换的驱动权归属 AgentGo 内核

**用户可以配置"A → B 之后该做什么"，但不能配置"A → B 这个过程本身"。**

- 状态转换的"主动作"（`store.ClaimTask` / `store.TransitionState` / `agent.SetState` 等核心调用）只能由 AgentGo 内置主流程驱动
- 用户配置的 Reactor 只能订阅"状态已经变成 B 之后"的事件做副作用
- **用户 Reactor 永远不允许直接调用 `agent.SetState(...)` 或 `store.TransitionState(...)`**——YAML 动作语言的设计层面就排除这种能力
- Reactor 想让 agent 进入下一状态，必须通过明确 API（publish_task / send_message / 调工具），让主流程在合适时机自然驱动转换

**理由**：状态机的原子性是系统稳定性的根基。一旦允许用户配置驱动状态转换，事件循环会从有向无环图退化为可能的环，调试地狱、责任归属混乱、Replay 困难。这条规则保证状态机入口固定且可审计；调试时永远知道"状态从哪儿变的"；用户配置错误最多打破 reactor 但不会打破状态机。

### 原则 2：Reactor 分两类，失败语义不同

| 类型 | 注册者 | 同步性 | 失败语义 | 典型示例 |
|---|---|---|---|---|
| **内置 Reactor** | 开发者 Go 注册 | 允许声明 `Sync: true` 同步执行 | 仍需 panic-isolated，但失败应被 trace 显眼记录（系统不变量被打破）| claim → 清理 FileCache、terminate → 调用 OnTaskEnd、compact → emit trace |
| **用户 Reactor** | YAML 声明 | **永远异步** | panic-isolated，失败仅记日志 | task_failed → 发邮件、file_written → 跑 lint |

两类共享同一个 ReactorRegistry 与事件订阅机制，区别仅在注册入口、同步性许可、失败的可见性级别。

### 原则 3：trace 是事实标准（事件流唯一真相源）

所有状态变更必须遵循三步序列：**主流程 SetState → emit `KindAgentStateChanged`（或对应 EventKind）→ Reactor 订阅者响应**。

- trace 事件不仅是观察手段，更是 Reactor 系统的**唯一事件源**
- 这意味着 trace 机制升级（事件 payload 结构化、新增状态变更字段等，详见 `TraceUpgrade.md`）从"前置依赖"升级为**硬性前置**——没有结构化事件，整套 Reactor 架构站不起来
- TraceUpgrade 与 ReactiveSystem 同步推进（详见 §10 实施顺序）

### 原则 4：Reactor 不允许直接驱动新状态转换

承接原则 1 的延伸——不仅用户 Reactor，**内置 Reactor 也不允许直接 SetState**。Reactor 想让 agent 进入下一状态必须通过明确 API（publish_task / send_message / 调工具），由主流程在适当时机驱动。

**理由**：避免 Reactor → Reactor 级联导致事件循环复杂度爆炸；保留单一驱动入口便于 Replay 与调试。如果某个内置副作用看起来"必须立即转换状态才能继续"，那它本身应当是主流程的一部分，不该是 Reactor。

### 原则 5：Reactor 自带的独立 LLM 调用必须是上下文隔离的纯文本生成器

Reactor 子系统支持两种"独立 LLM 调用"路径（详见 §6.1 动作语言）：

- `invoke_llm`：一次性 LLM 文本生成，输出写入指定去向
- `spawn_agent` + `via_translator`：用 reactor 自带 LLM 把用户 prompt 加工为新 agent 的 initial_task.description

**这两条路径必须严格遵守上下文隔离**：

| 维度 | 必须保证 | 禁止注入 |
|---|---|---|
| 工具权限 | **无工具** —— 不携带任何 tools schema 给 LLM，纯文本输入输出 | 任何 tool definitions |
| 历史 | **无 history 累积** —— 单轮请求，不参与任何 ReactLoop | 任何已有 agent 的历史消息 |
| 系统提示词 | **仅使用用户配置的 prompt** | 团队感知 / 角色定义 / 文件感知等由 MemoryStore 提供的上下文 |
| 运行时上下文 | **完全隔离** | mailbox / approval / file cache / token stats 等 |

**用途边界**：仅适用于"用 LLM 做一次轻量文本生成/转译"的场景（写日志摘要、为子 agent 生成 initial prompt、根据事件生成通知文案）。**不适用于**需要工具、需要多轮迭代、需要团队协调的场景——这些必须走 `publish_task` 或 `spawn_agent`（不带 via_translator 的 ad-hoc agent 仍走完整 ReactLoop）。

**理由**：独立 LLM 调用一旦携带工具就成了"无监督的影子 agent"——会触发文件写入、shell 调用、mailbox 联动等副作用，绕过 ReactiveSystem 的所有 Gate 与原则 4。把它收窄为"纯文本生成器"是阻断这条攻击链的关键一刀。同时也让这条路径的 cost / 监控 / 错误处理变得简单——单轮 + 单输入 + 单输出，没有循环、没有递归。

### 判别新代码归属的速查表

写新代码前问几步：

1. **它是状态转换的"主动作"吗？**（让状态从 A 变到 B 的那个调用）
   - 是 → 留在 agent.go 主流程，不进 ReactiveSystem
2. **它是状态变成 B 之后的副作用吗？**
   - 是 → Reactor（开发者注册 = 内置；用户 YAML 声明 = 用户）
3. **它在事件之前需要决策放行/否决吗？**
   - 是 → Gate
4. **它是邮件多对一聚合吗？**
   - 是 → **不进 ReactiveSystem**，是 Mailbox 子系统内部固定机制，扩到 `internal/mailbox/` 下增加新合并逻辑
5. **它需要"在 ReactLoop 节奏点为 LLM 提供上下文"吗？**
   - 是 → **不进 ReactiveSystem**，归 [MemoryManageSystem.md](MemoryManageSystem.md)（团队状态 / 文件占用 / 项目知识等长期记忆类需求）

---

## 1. 背景与动机

### 1.1 v4 时代的隐性问题

v4 落地了三套独立 Registry（`ToolHookRegistry` / `AgentHookRegistry` / `MailboxHookRegistry`），合计 13 个内置 Hook。表面各司其职，但实际审视下来，**"Hook" 这个词在系统里覆盖了 4 类完全不同的职责**——拦截 / 状态响应 / 上下文供给 / 消息聚合——它们被强行塞进同一个抽象，导致：

- **认知负担**：开发者写新组件时搞不清楚自己在写哪一类，常常写出"应该是观察但占用了拦截入口"或反之的错位代码
- **职责膨胀**：现有 `record-artifact` 实际是状态响应器（在 file_written 事件后写入 artifact 集合），却因为没有 Reactor 子系统被迫塞进 Tool PostCall hook
- **状态来源被迫反查**：`require-read-before-write` 的拦截逻辑没问题，但它依赖的"已读集合"这一状态没有显式表达，被迫去 `Store.GetToolCallHistory()` 反查工具调用历史推断
- **命名误导 + 形态错位**：`team-awareness-*` 不在状态转变时触发、没有决策能力，纯粹"给 LLM 注入文本"，叫 Hook 暗示了它不具备的语义；更进一步，把它叫什么名字（Hook 还是 Provider）都解决不了它**作为 V2 临时修复**的本质问题——团队感知信息被反复重新生成、无法持久化、无法跨任务复用，详见 [MemoryManageSystem.md](MemoryManageSystem.md)

### 1.2 v5 时代真正缺的能力

更关键的是，**当前所有 Hook 注册者都是开发者**（写 Go 代码 + 改 bootstrap.go）。系统**完全不允许用户在 YAML 配置层声明 "状态 X 变更后执行动作 Y"**——但这正是 v5 阶段最迫切的新增需求：

- "Task 失败时自动发邮件给指定 agent"
- "file_written 事件触发外部 lint 脚本"
- "Agent 进入 WaitingApproval 状态超过阈值时自动取消"
- "任务完成时自动 publish 一个验证子任务"

这些都是**状态转变后的副作用响应**，本质上跟 SQL `TRIGGER AFTER UPDATE` 或 systemd `OnFailure=` 是同一个 family。这正是 Reactor 单元的核心职责——既不能否决状态转变（状态机已经走完），也不应阻塞主流程（一个失败的 Reactor 不能让整个系统僵死）。

### 1.3 本模块的两条工作线

1. **拦截层瘦身与命名空间清理**：把现有 13 个 Hook 按 2 类核心角色（Reactor / Gate）重新归类，错位的迁移、混淆的重命名、反模式的治理。Hook 收敛为 Gate，专司"决策拦截"。team-awareness 类组件直接删除（迁 MemoryManageSystem.md）；wake-context-expand 留在 Mailbox 子系统内部作为固定聚合机制。
2. **Reactor 单元从零引入**：新增 `ReactorRegistry` 子系统，承接"用户可配置的状态变更响应"。**这是 v5 时代真正新增的核心能力**——把 trace 事件流从只能记录升级为可触发用户逻辑。

---

## 2. 现状盘点：13 个内置 Hook 的完整清单

> 数据来源：`internal/hook/builtin/` 目录扫描（2026-04-29）。详细签名与触发位点见 `internal/hook/tool.go` / `agent.go` / `mailbox.go`。

### 2.1 Tool Hook（7 个）

| # | 名字 | Phase | Priority | Matches | 一句话职责 |
|---|---|---|---|---|---|
| T1 | `path-boundary` | PreCall | 10 | 文件相关工具 | 校验路径在项目根内、非敏感文件，越界即 Abort |
| T2 | `validate-expected-hash` | PreCall | 20 | 文件读写 | 校验整文件 SHA256，不符即 Abort |
| T3 | `dependency-validator` | PreCall | 25 | (待补) | 校验工具调用的依赖关系 |
| T4 | `validate-line-anchors` | PreCall | 25 | write_file / edit_file | 校验行哈希锚点（v4 §7 落地），失配即 Abort |
| T5 | `require-read-before-write` | PreCall | 30 | write_file / edit_file | 反查 ToolCallHistory，未读过即 Abort |
| T6 | `enforce-expected-artifacts` | PreCall | 35 | (待补) | 强制实际产出符合声明的预期产物清单 |
| T7 | `record-artifact` | PostCall | 950 | write_file / edit_file | 写入 task.Artifacts 列表（**纯副作用**）|

### 2.2 Mailbox Hook（4 个）

| # | 名字 | Phase | Priority | 一句话职责 |
|---|---|---|---|---|
| M1 | `chain-depth-limit` | BeforeSend | 10 | 邮件链深度超限即拒发 |
| M2 | `per-agent-dedup` | BeforeWake | 500 | 同一 agent 多封邮件去重，仅唤醒一次 |
| M3 | `wake-worthy-filter` | BeforeWake | 600 | 过滤掉不值得唤醒的邮件 |
| M4 | `wake-context-expand` | BeforeWake | 800 | 把多封邮件累加构造成一段 wake task description |

### 2.3 Agent Hook（2 个）

| # | 名字 | Phase | Priority | 一句话职责 |
|---|---|---|---|---|
| A1 | `team-awareness-task-start` | TaskStart | 500 | 任务入口注入团队快照（TeamSnapshot / FileAwareness / GoalAnchor 三段）|
| A2 | `team-awareness-loop-pre` | LoopPre | 500 | 每轮 ReactLoop 顶部按各 section 独立频率刷新感知 |

---

## 3. v5 视角的重新归类：ReactiveSystem 的 2 类核心角色

按"是否决策 / 是否在状态变更点触发 / 是否用户可配 / 失败影响"四个维度把 13 个 Hook 重新归类：

| 角色 | 当前数量 | 何时触发 | 能否否决 | 用户可配？ | 失败影响 | v5 应归属 |
|---|---|---|---|---|---|---|
| **Gate（拦截门）** | **9** | 工具调用 / 消息发送之前 | ✅ 可 Abort | ❌ 系统纪律 | 阻塞主流程 | 保留瘦身后的 Hook 系统，专司决策 |
| **Reactor（状态响应器）** | 1（T7 record-artifact）| **状态转变之后** | ❌ 不可 | ✅ **核心新需求** | **必须隔离**，不影响状态机 | **本模块核心，全新引入** |
| ~~Aggregator（消息聚合）~~（**v5 下放**）| ~~1（M4 wake-context-expand）~~ | 多事件合并时 | ❌ 不可 | ❌ 不暴露外部配置 | 聚合失败 = 退化为单条唤醒 | **留在 Mailbox 子系统内部作为固定聚合机制**，无独立 Registry，不立顶层抽象 |
| ~~Provider（上下文供给）~~（**v5 废弃**）| ~~2（A1 / A2）~~ | ~~ReactLoop 节奏点~~ | — | — | — | **彻底删除，由 [MemoryManageSystem.md](MemoryManageSystem.md) 承接** |

### 3.1 逐项归类与去向

**图例**：✅ 角色清晰原地保留 ｜ 🔁 错位应迁移 ｜ ⚠️ 混合体或反模式 ｜ 🗑 v5 删除

| # | Hook | v5 真实角色 | 去向 |
|---|---|---|---|
| T1 | `path-boundary` | Gate | ✅ 保留为 Gate |
| T2 | `validate-expected-hash` | Gate | ✅ 保留为 Gate |
| T3 | `dependency-validator` | Gate | ✅ 保留为 Gate |
| T4 | `validate-line-anchors` | Gate | ✅ 保留为 Gate |
| T5 | `require-read-before-write` | Gate + **反模式**（状态来源是反查日志）| ⚠️ Gate 留下，但状态来源应改由 Reactor 维护"已读集合"|
| T6 | `enforce-expected-artifacts` | Gate | ✅ 保留为 Gate |
| T7 | `record-artifact` | **Reactor**（典型！）| 🔁 迁到 ReactorRegistry，作为内置示范 |
| M1 | `chain-depth-limit` | Gate | ✅ 保留为 Gate |
| M2 | `per-agent-dedup` | Gate（过滤是拦截子集）| ✅ 保留为 Gate |
| M3 | `wake-worthy-filter` | Gate | ✅ 保留为 Gate |
| M4 | `wake-context-expand` | Mailbox 内部固定聚合机制（非顶层抽象）| 📦 留在 `internal/mailbox/` 下，作为 mailbox 私有组件存在；不立 Registry，不暴露外部配置 |
| A1 | `team-awareness-task-start` | ~~Provider~~ → 删除 | 🗑 **v5 删除**——TeamSnapshot / FileAwareness 迁 MemoryManageSystem.md，GoalAnchor 直接删（task.Description 已承载）|
| A2 | `team-awareness-loop-pre` | ~~Provider~~ → 删除 | 🗑 同上 |

#### 3.1.1 不在 13 个 Hook 列表内但应当治理的反模式

下列组件**当前不属于 Hook 系统**（在工具内部实现），但按 v5 视角实质是 Gate，Phase 1 命名空间清理时一并归位：

| # | 组件 | 当前位置 | v5 真实角色 | 去向 |
|---|---|---|---|---|
| X1 | `shell.CommandFilter`（黑/灰/白名单匹配 + 阻拦/审批分流）| `internal/tools/shell/` 内部，bootstrap 时 `shell.BuildFilter` 构造 | **Gate**（事前决策门，按命令名单决定 Abort / 直接放行 / 标记 needs_approval）| 🔁 重构为正式 Gate，与 path-boundary 同档（详见 §7.4 Shell 工具的 ReactiveSystem 集成）|

---

## 4. ReactiveSystem 的目标形态

### 4.1 拆分前后对比

```
v4（现状）：
  ToolHookRegistry        ──┐
  AgentHookRegistry       ──┼── 13 个 Hook，4 种职责混杂
  MailboxHookRegistry     ──┘

v5 ReactiveSystem（2 类核心角色 + 单一统一 Registry 各类）：

  ┌─ ReactorRegistry ────── 1 个内置（record-artifact 迁移而来）
  │                         + 用户 YAML 配置 N 个
  │                         【核心单元】状态变更后副作用，不可否决，用户可配
  │                         ━━━━━━ 这是 v5 真正新增的能力 ━━━━━━
  │
  └─ GateRegistry（统一）── 9 个纯拦截门（跨域统一，按 Phase 路由）
                            【核心单元】决策门，可否决，开发者注册
                            统一 Phase 枚举 + 接口式 Context（详见 §4.4）
                            ┌─ Tool 域：PreCall / PostCall（6 个 Gate）
                            ├─ Mailbox 域：BeforeSend / BeforeDeliver / BeforeWake（3 个 Gate）
                            └─ 未来加 Cron / Webhook / MCP 等域时仅扩 Phase + Context 类型

  （原 AggregatorRegistry 已下放——wake-context-expand 留在
    internal/mailbox/ 下作为 Mailbox 子系统的固定内部组件，
    不立顶层抽象、无独立 Registry、不暴露外部配置）

  （原 ProviderRegistry 已废弃，team-awareness 类需求由
    MemoryManageSystem.md 承接，不在 ReactiveSystem 范围内）
```

### 4.2 各角色的职责边界（v5 定义）

| 角色 | 输入 | 输出 | 失败语义 | 配置入口 |
|---|---|---|---|---|
| **Reactor** | 已发生的状态变更事件 + 完整 payload | 内部副作用（调工具 / 发邮件 / publish 任务），无返回 | **必须 panic-isolated**，失败仅记日志 | 用户 YAML 声明，运行时加载 |
| **Gate** | 即将发生的动作（工具调用 / 消息发送 / 未来其他域）+ 该域具体 Context（接口式）| `Continue` 或 `Abort{reason}` | Abort 阻止动作，主流程感知 | 开发者 Go 代码注册，bootstrap.go；Context 接口 + 各域具体类型实现详见 §4.4 |

> ~~**Aggregator** | 多个待唤醒事件 | 单个合并后的唤醒任务描述 | 聚合失败 = 退化为单条唤醒 | 开发者 Go 代码注册~~ —— **2026-04-30 下放**：Aggregator 不再是顶层角色，wake-context-expand 留在 `internal/mailbox/` 下作为 Mailbox 子系统的固定内部组件；语义层面 Mailbox 仍可在收件 → 唤醒路径中做合并，但实施上是 mailbox 私有逻辑，无独立 Registry，不暴露外部配置
>
> ~~**Provider** | ReactLoop 节奏点 + 当前 Agent / Task 状态 | 一段文本（追加到 history）| 注入失败 = 该轮少一段提示 | 开发者 Go 代码注册~~ —— **2026-04-30 废弃**：Provider 抽象由 [MemoryManageSystem.md](MemoryManageSystem.md) 取代。Agent 在 `processTask` 入口主动从 `MemoryStore.Query()` 拉取上下文，不再有"被 hook 反复塞东西"的注入路径

### 4.3 为什么要拆两个 Registry 而不是统一加 phase 区分

候选方案 A："统一一个 Registry，加 `Role` 枚举区分各类"。
**否决理由**：
- 用户认知负担——为什么我注册的组件不能 Abort？为什么我配的组件失败能让系统僵死？
- 失败语义无法在同一抽象内统一（Abort 阻塞 vs 必须隔离）
- 配置入口异构（Go 注册 vs YAML 声明）强行合并意味着 YAML schema 要支持所有类型，复杂度爆炸

候选方案 B（采纳）：两个独立 Registry，各自最小化 API。Reactor / Gate 互不通信，bootstrap.go 各自装配。

> **2026-04-30 修订路径**：
> - 原方案为"四个 Registry（含 Provider / Aggregator）"
> - 第一轮缩减：Provider 抽象被认定为 V2 临时修复的延续，应当由 MemoryManageSystem 承接，4 → 3
> - 第二轮缩减：Aggregator 当前只 1 个实现且无第二个用例迹象，强行立顶层是过度设计，下放为 Mailbox 子系统内部固定机制，3 → 2
> - 最终形态：仅 Reactor + Gate 两个核心 Registry。详见 [MemoryManageSystem.md §0](MemoryManageSystem.md)。

### 4.4 GateRegistry 的统一形态：接口式 Context

**Q1' 决议（2026-04-30）**：v5 把 v4 时代分立的 `ToolHookRegistry` / `MailboxHookRegistry` **真正统一为单一 `GateRegistry`**，采用"统一协议层 + 接口式 Context"的 Go 惯用模式。理由：Reactor 已经跨域统一（task / tool / shell 等事件用同一套订阅机制），Gate 没有理由不对称——同时 v4 时代分开的合理性（"领域不同避免 fat context"）可以用 Go interface 完美规避。

#### 4.4.1 设计核心思路

把 Gate 系统拆解为两层：

| 层 | 内容 | 跨域 |
|---|---|---|
| **协议层**（dispatch protocol）| Phase 路由 / Abort 语义 / Priority 排序 / 注册机制 / Decision 类型 | ✅ 跨域统一 |
| **数据层**（runtime payload）| Tool 域要 ToolName/Args/Result；Mailbox 域要 Message/DeliverTo；未来 Cron 域有 ScheduleID 等 | ❌ 各域专属 |

**关键认识**：数据形态本身就因域而异，这是事实而不是缺陷。强行把所有域字段塞进一个 fat struct 是建模失败。承认这一事实、用接口统一协议、用具体类型保留各域形态——这是 Go 类型系统给的礼物。

参考标准库同模式：`error` / `context.Context` / `io.Reader` / `http.Handler` 都是这种"接口定义协议层契约 + 具体类型携带数据层各自实情"的形态。

#### 4.4.2 接口与类型草案

```go
// internal/gate/gate.go（新建）

// 统一 Phase 枚举，跨域用前缀区分
type Phase string

const (
    // Tool 域
    PhaseToolPreCall   Phase = "tool:preCall"
    PhaseToolPostCall  Phase = "tool:postCall"

    // Mailbox 域
    PhaseMailboxBeforeSend    Phase = "mailbox:beforeSend"
    PhaseMailboxBeforeDeliver Phase = "mailbox:beforeDeliver"
    PhaseMailboxBeforeWake    Phase = "mailbox:beforeWake"

    // 未来扩展位（仅声明，不实现）
    // PhaseCronBeforeFire Phase = "cron:beforeFire"
    // PhaseWebhookBeforeDispatch Phase = "webhook:beforeDispatch"
)

// 统一 Action 与 Decision
type Action int
const (
    Continue Action = iota
    Abort
)

type Decision struct {
    Action      Action
    AbortReason string
    HookName    string  // 产生本次决策的 Gate 名（追溯日志用）
}

// Context 是 marker interface——所有具体 context 实现它
// 协议层只看接口定义的方法，具体字段由各域类型自带
type Context interface {
    Phase() Phase
    AgentID() string
    TaskID() string
    Ctx() context.Context
}

// 统一 Gate 接口
type Gate interface {
    Name() string
    Phase() Phase                // 它对哪个 phase 感兴趣
    Priority() int               // 0-100 系统强制 / 500 默认 / 900-1000 观察类
    Matches(ctx Context) bool    // 比"工具名匹配"更通用——mailbox gate 用通配即可
    Run(ctx Context) Decision    // ← Context 是接口，实现里 type assert 到具体类型
}

// 统一 Registry
type Registry struct {
    gatesByPhase map[Phase][]Gate  // 按 phase 索引；同 phase 内按 priority 排序
}

func (r *Registry) Register(g Gate) { /* 按 phase 分桶，按 priority 插入 */ }

func (r *Registry) Dispatch(c Context) Decision {
    for _, g := range r.gatesByPhase[c.Phase()] {
        if !g.Matches(c) { continue }
        if d := g.Run(c); d.Action == Abort { return d }
    }
    return Decision{Action: Continue}
}
```

**Tool 域 Context（具体实现）**：

```go
// internal/gate/tool_context.go
type ToolContext struct {
    phase    Phase  // 必为 PhaseToolPreCall 或 PhaseToolPostCall
    agentID  string
    taskID   string
    ctx      context.Context

    // Tool 域专属字段
    ToolName string
    Args     map[string]any
    Result   string  // 仅 PostCall
    Err      error   // 仅 PostCall
}

func (c *ToolContext) Phase() Phase            { return c.phase }
func (c *ToolContext) AgentID() string         { return c.agentID }
func (c *ToolContext) TaskID() string          { return c.taskID }
func (c *ToolContext) Ctx() context.Context    { return c.ctx }
```

**Mailbox 域 Context（具体实现）**：

```go
// internal/gate/mailbox_context.go
type MailboxContext struct {
    phase    Phase  // 必为 PhaseMailboxBeforeSend / BeforeDeliver / BeforeWake 之一
    agentID  string
    taskID   string
    ctx      context.Context

    // Mailbox 域专属字段
    Message     mailbox.Message
    DeliverTo   string  // 仅 BeforeDeliver
    EventType   string  // 仅 BeforeWake
    UnreadCount int     // 仅 BeforeWake
}

// 实现 Context 接口的方法（同上）
```

**Gate 实现示例**：

```go
// internal/gate/builtin/path_boundary.go
type PathBoundaryGate struct{ /* ... */ }

func (g *PathBoundaryGate) Name() string  { return "path-boundary" }
func (g *PathBoundaryGate) Phase() gate.Phase { return gate.PhaseToolPreCall }
func (g *PathBoundaryGate) Priority() int { return 10 }

func (g *PathBoundaryGate) Matches(c gate.Context) bool {
    tc, ok := c.(*gate.ToolContext)
    if !ok { return false }
    return matchesFileTools(tc.ToolName)
}

func (g *PathBoundaryGate) Run(c gate.Context) gate.Decision {
    tc := c.(*gate.ToolContext)  // 此处必成功——dispatcher 已按 phase 路由
    if !pathOK(tc.ToolName, tc.Args) {
        return gate.Decision{
            Action: gate.Abort,
            AbortReason: "path outside project root",
            HookName: g.Name(),
        }
    }
    return gate.Decision{Action: gate.Continue}
}
```

#### 4.4.3 type assertion 的安全边界

`Run()` 入口的 `c.(*gate.ToolContext)` 必须断言成功——失败即 panic。这不是宽容性问题而是**装配纪律**：

- Dispatcher 按 phase 分发：`PhaseToolPreCall` 的 Gates 只会被 ToolContext 喂入
- 一个 Gate 声明 `Phase() = PhaseToolPreCall`，就承诺它的 Run 接受 ToolContext
- 如果实现里 expect 了 MailboxContext，那是注册时就装配错了

panic 让这种 bug 在测试期或灰度期立即暴露，与 [§7.3.2 状态机非法切换 panic](#732-失败处理panic--watchdog-兜底) 同一种纪律——**编程错误必须 fail-fast，不能软失败**。

#### 4.4.4 加新域的接入流程（开闭原则验证）

未来若引入第 3 个域（例如定时任务），接入流程：

| 步骤 | 改动位置 | 改动内容 |
|---|---|---|
| 1. 加 Phase 常量 | `internal/gate/gate.go` | `PhaseCronBeforeFire Phase = "cron:beforeFire"` |
| 2. 定义具体 Context | `internal/gate/cron_context.go`（新建）| `type CronContext struct { ScheduleID string; FireTime time.Time; ... }` + 实现 `Context` 接口 |
| 3. Cron dispatcher 调 Registry | `internal/cron/dispatcher.go`（新建或扩）| `r.Dispatch(&gate.CronContext{...})` |
| 4. 写 Cron 域 Gate | `internal/gate/builtin/cron_xxx.go` | 同 `PathBoundaryGate` 形态 |

**`internal/gate/registry.go` / `gate.go` 的 Gate / Decision / Phase 定义零改动**——这就是接口设计的开闭原则：对扩展开放，对修改关闭。

#### 4.4.5 Mailbox 内部 aggregator 与 BeforeWake Gate 的并存

承接 §3 / §4.1 的设计：Aggregator 已下放为 mailbox 子系统私有组件。但 `PhaseMailboxBeforeWake` 这个 phase 名字依然在 GateRegistry 里出现（M2 per-agent-dedup / M3 wake-worthy-filter 的归属 phase）。

**两者通过调用路径分离，零冲突**：

```go
// internal/mailbox/notifier.go（伪代码）
func (n *Notifier) wakeAgent(agentID string) {
    // 1. 调统一 GateRegistry 的 BeforeWake gates（M2, M3）
    decision := n.gates.Dispatch(&gate.MailboxContext{
        phase: gate.PhaseMailboxBeforeWake,
        agentID: agentID,
        // ...
    })
    if decision.Action == gate.Abort { return }  // M2/M3 拒绝唤醒

    // 2. 调 mailbox 私有 aggregator（不走 GateRegistry，是 mailbox 内部方法）
    description := n.aggregator.BuildWakeDescription(agentID, ...)

    // 3. publish wake task
    n.store.PublishTask(&Task{Description: description, ...})
}
```

GateRegistry 的 BeforeWake 只管"要不要唤醒"决策；Mailbox 私有 aggregator 是 dispatcher 在决策通过后单独调的方法。两者**不通过同一个调度路径**，phase 名共享但代码完全分离。

#### 4.4.6 v4 → v5 迁移工作量估算

| 项 | 估算行数 |
|---|---|
| 新建 `internal/gate/` 包（Phase 枚举、Action、Context 接口、Decision、Gate 接口、Registry）| ~200 |
| 9 个现有 Gate 实现改造（Phase 命名 + Run 签名 + type assert）| 50 × 9 = 450 |
| Dispatcher 调用点改造（agent.go 工具调用前 / mailbox 三个 phase 触发点）| ~80 |
| 删除旧 `internal/hook/tool.go` / `mailbox.go` / 两个旧 Registry | -500（净删）|
| 测试调整 | ~200 |
| **总计** | **新增 ~930 - 删除 500 = 净增 ~430 行** |

属于 Phase 1 命名空间清理范围内的可控工作量。

---

## 5. 三个值得在 v5 里专门治理的反模式

### 5.1 `record-artifact` 错位在 PostCall —— Reactor 的第一个内置示范

**现状**：`internal/hook/builtin/record_artifact.go` 注册为 Tool PostCall hook，Priority 950，Matches `write_file / edit_file`。它在每次 write_file/edit_file 工具成功执行后，把路径追加到 `task.Artifacts`。

**为什么是反模式**：它不是"工具调用的观察者"，它是"file_written 状态变更后写入 artifact 集合"的响应器。被迫挂在 PostCall 是因为系统没有 Reactor 子系统。这导致：
- 它跟其他真正的拦截类 PostCall hook（暂时没有，但未来可能有）混在同一个 Phase 枚举下
- 它的失败处理与拦截 hook 相同（写日志），但失败后果完全不同（artifact 丢失 vs 调用阻塞）
- 它无法跨工具复用——如果未来有别的工具也产生文件（如 `apply_patch`），需要给每个工具单独写一个 record-artifact hook

**v5 治理**：迁到 ReactorRegistry，订阅"file_written"语义事件。**作为 Reactor 子系统第一个内置示范用例**——证明这个抽象能优雅承接现有需求，不是空中楼阁。同时验证：迁移后主流程行为零变化、测试全绿。

#### 内置 Reactor 首批改造清单（候选）

除 record-artifact 外，agent.go 内还有多处"状态转换后的固定副作用"散落在主流程中——它们是 Phase 3 内置 Reactor 改造的候选清单：

| 副作用 | 当前位置 | 候选 Reactor 名 | 同步性 |
|---|---|---|---|
| ClaimTask 后清理 FileCache | [agent.go:362](internal/agent/agent.go#L362) | `clear-file-cache-on-claim` | Sync |
| panic recover 后 emit `KindTaskFailed` | [agent.go:317](internal/agent/agent.go#L317)（v4 §11 §S11 已修复）| `panic-emit-failed` | Sync |
| processTask 退出时的 OnTaskEnd 回调 | processTask defer 链 | `task-end-callback` | Sync |
| TruncateHistory / Compaction 路径的 trace emit | agent.go 多处 | `trace-history-event` | Async |
| ReadSet 写入（承接 §5.2 反模式治理）| 不存在（待引入）| `read-set-write` | Async |
| TokenStats 累加 | LLM 调用后 | `token-stats-accumulate` | Async |

这条清单不是 must-have——Phase 4 落地时按价值优先级取子集即可。但它证明了：**内置 Reactor 不是为了凑数，agent.go 主流程当前确实存在一批"应该是 Reactor 但被迫散落"的副作用**。

##### Phase 4 落地子集决议（2026-04-30）

7 个候选（含 T7）经讨论后定子集——**最小 3 个 + Phase 6 时再补 1 个**：

| # | 候选 | 同步性 | Phase 4 决议 | 理由 |
|---|---|---|---|---|
| 1 | T7 `record-artifact` | Async | ✅ **Phase 4 必做** | **示范 / 试金石**——它已是 hook 形态，迁移最 mechanical，验证 Reactor 抽象正确性的第一关 |
| 2 | `clear-file-cache-on-claim` | Sync | ❌ Phase 4 不做（v5.x 增量）| 与③同档（Sync），价值递减；inline 代码工作良好 |
| 3 | `panic-emit-failed` | Sync | ❌ Phase 4 不做（v5.x 增量）| v4 §S11 已修过且稳定，迁移引入新风险得不偿失 |
| 4 | `task-end-callback` | Sync | ✅ **Phase 4 必做** | 验证 **Sync Reactor 完整链路**（注册 / 调度 / 失败处理） |
| 5 | `trace-history-event` | Async | ✅ **Phase 4 必做** | 验证 **Async Reactor 完整链路**（失败隔离 / 不阻塞主流程） |
| 6 | `read-set-write` | Async | ⏸ **Phase 6 同 PR 引入** | 它是 §5.2 ReadSet 治理的前置；与 Phase 6 同 PR 引入避免"Reactor 写但 Gate 仍反查日志"半成品过渡态 |
| 7 | `token-stats-accumulate` | Async | ❌ Phase 4 不做（v5.x 增量）| 与⑤同档（Async），价值递减 |

**为什么是这 3 个**：①②③④⑤⑥⑦ 中，T7（①）+ task-end-callback（④）+ trace-history-event（⑤）三个共同构成 Reactor 系统的"最小覆盖矩阵"——验证了从 hook 迁移 / Sync 路径 / Async 路径 / 多 Reactor 共订阅同一事件互不干扰这四条关键路径。再多就只是同类验证的重复。

**Phase 4 内部子序列**：

```
Step 4.1: 建 ReactorRegistry 基础设施
          （Reactor 接口 / Register / Dispatch / 失败隔离骨架 / Sync vs Async 标记）
            ↓
Step 4.2: T7 record-artifact 迁移
          ─ 已是 hook 形态，迁移路径最 mechanical
          ─ 跑现有 record-artifact 测试，Reactor 抽象第一关验证
            ↓
Step 4.3: task-end-callback 迁移（Sync 链路）
            ↓
Step 4.4: trace-history-event 迁移（Async 链路）
            ↓
Step 4.5: 集成测试——一个完整 task 跑完，三个 Reactor 都触发，行为零变化
```

**Phase 4 完成的判定准则（验证 Reactor 抽象正确性）**：

| 准则 | 检查方式 |
|---|---|
| 抽象正确性 | T7 迁移后行为零变化——覆盖全部 v4 record-artifact 测试用例 |
| Sync 链路 | 单测：故意 panic 的 Sync Reactor，panic 被正确捕获 + trace 显眼标红 |
| Async 链路 | 单测：故意 sleep 的 Async Reactor，主流程不被阻塞 |
| 多 Reactor 协同 | 单测（Registry 层 stub Reactor）：同一 `EventKind` 上同时挂载 Sync + Async Reactor，dispatch 时两者都触发且互相隔离。`trace-history-event` 与 `task-end-callback` 订阅事件不同（前者订阅 `KindHistoryCompaction/Truncated`，后者订阅 task lifecycle 退出事件），不直接共享 EventKind——多 Reactor 协同验证以 Registry 单测为准 |
| 内置 Reactor 不能违反原则 4 | 当前阶段以**接口注释 + code review** 约束（v5 内置 Reactor 静态可控）；Phase 5 引入用户 YAML Reactor 时再加运行期 guard 或包分层 |
| 不依赖 Phase 5 用户 YAML | Phase 4 单测覆盖 Go 注册路径即可，用户 YAML 路径留 Phase 5 验证 |

### 5.2 `require-read-before-write` 反查日志推断"已读"

**现状**：`internal/hook/builtin/require_read_before_write.go` 在 PreCall 触发，通过 `Store.GetToolCallHistory()` 查询本任务的 ToolCallRecord 历史，按 Success=true 的 read_file 调用记录判定"该文件已读过"。

**为什么是反模式**：
- "已读集合"是任务级**显式状态**，但因为没地方放被迫每次反查日志
- 反查的开销随历史增长线性上升（O(N) per check）
- 状态语义模糊——"工具调用记录里有 read_file" ≠ "文件已被 LLM 真正消化过"，但没办法精确表达
- Gate 和状态来源耦合——未来想加"已读集合的过期清理"或"压缩历史时保留已读集合"这些需求时无处下手

**v5 治理**：把"已读集合"显式化为 `Task.ReadSet map[string]ReadInfo` 或 `Agent.ReadCache`，由 Reactor 在 read_file 完成事件触发时写入。Gate 只读这个集合，不再反查日志。

> **承接 v5 命题**："Agent 字段需要适当升级" 的第一个具体落地点就是这里——给 Agent / Task 加显式状态包，让 Reactor 有地方写。

#### 5.2.1 ReadSet 数据结构与触发事件（Phase 6 落地形态 — 初版草案）

> **状态**：📋 初版草案（2026-05-01 起草）。Phase 6 启动前可能基于 Phase 4 / 5 的实施经验调整。当前版本作为 spec 起点。

##### 5.2.1.1 数据结构

ReadSet 挂在 `Task` 上而非 `Agent` 上——理由：

- "已读"是**任务级**语义（同一 agent 跨任务时，前一任务读过的文件不该影响后一任务的 require-read-before-write 判定）
- Task 的生命周期与 ReadSet 自然对齐（任务完成 = ReadSet 失效）
- v4 已有的 `Task.Artifacts` 也是任务级显式状态，ReadSet 与之同档处理

```go
// internal/store/task.go（增量）

type Task struct {
    // ... 现有字段 ...

    // ReadSet 显式化"哪些文件该任务里被读过"。
    // 由 read-set-write Reactor 写入；require-read-before-write Gate 查询。
    // key 为文件**绝对路径**（避免相对路径在不同 cwd 下的二义性）。
    ReadSet map[string]ReadInfo `json:"read_set,omitempty"`
}

type ReadInfo struct {
    FilePath  string    `json:"file_path"`            // 冗余存储绝对路径（与 map key 一致），便于 list 输出
    ReadAt    time.Time `json:"read_at"`              // 首次读取时间戳
    Loop      int       `json:"loop,omitempty"`       // 触发读取的 ReactLoop 轮次
    Hash      string    `json:"hash,omitempty"`       // 读取时刻的文件 SHA256（与 v4 §7 hashline 配合）
    LastReadAt time.Time `json:"last_read_at,omitempty"` // 最近一次读取时间戳（多次读取覆盖）
}
```

**索引选择**：
- 用 `map[string]ReadInfo` 而非 `[]ReadInfo`——查找是 require-read-before-write 的核心操作（O(1) vs O(N)）
- key 用绝对路径——所有"读取"在 v4 §7 path-boundary Gate 通过后已经规范化为绝对路径，复用此规范

##### 5.2.1.2 触发事件

**决议**：复用现有 `KindToolResult` 事件，**不新增** `KindFileRead`。

理由：
- 已有 `KindToolResult` 携带 `Tool / Args / Err`，Reactor 可在 Run 中 filter `tool=read_file && Err=nil`
- 新增 `KindFileRead` 与 `KindToolResult` 语义高度重叠（read_file 工具成功 = 文件被读取），徒增 EventKind 数量
- 与 [§5.1 record-artifact 迁移到 Reactor](#51-record-artifact-错位在-postcall--reactor-的第一个内置示范) 设计哲学一致——它也是订阅 `KindFileWritten`（语义事件），而非订阅"write_file 工具调用结果"

**read-set-write Reactor 的形态**（草案）：

```go
// internal/reactor/builtin/read_set_write.go

type ReadSetWriteReactor struct{}

func (r *ReadSetWriteReactor) Name() string { return "read-set-write" }
func (r *ReadSetWriteReactor) IsSync() bool { return false }  // Async（写 ReadSet 不在主流程关键路径）

func (r *ReadSetWriteReactor) Subscribe() []trace.EventKind {
    return []trace.EventKind{trace.KindToolResult}
}

func (r *ReadSetWriteReactor) Run(ev trace.Event) error {
    // filter：仅对 read_file 工具且无错误的事件感兴趣
    if ev.Tool != "read_file" || ev.Error != "" {
        return nil
    }

    // 从 Args 取 path（read_file 的核心参数）
    path, _ := ev.Args["path"].(string)
    if path == "" {
        return nil
    }
    absPath := normalizeAbsPath(path)  // 同 path-boundary 的规范化

    // 写入 task.ReadSet（通过 store API，不是直接改 Task struct）
    return store.UpsertReadSet(ev.TaskID, absPath, ReadInfo{
        FilePath:   absPath,
        ReadAt:     ev.Timestamp,  // 首次写时设
        Loop:       ev.Loop,
        Hash:       ""  /* 暂时不填，v5.x 增量 */,
        LastReadAt: ev.Timestamp,  // 每次读取都更新
    })
}
```

##### 5.2.1.3 Gate 侧的读取路径

`require-read-before-write` Gate 改造点：

```go
// internal/gate/builtin/require_read_before_write.go（v5 形态）

type RequireReadBeforeWriteGate struct{}

func (g *RequireReadBeforeWriteGate) Phase() gate.Phase { return gate.PhaseToolPreCall }

func (g *RequireReadBeforeWriteGate) Run(c gate.Context) gate.Decision {
    tc := c.(*gate.ToolContext)
    if tc.ToolName != "write_file" && tc.ToolName != "edit_file" {
        return gate.Decision{Action: gate.Continue}
    }

    path, _ := tc.Args["path"].(string)
    absPath := normalizeAbsPath(path)

    // 直接查 task.ReadSet（O(1)），不再调 Store.GetToolCallHistory() 反查日志
    readSet, err := tc.StoreView.GetReadSet(tc.TaskID)
    if err != nil { /* ... */ }

    if _, ok := readSet[absPath]; !ok {
        return gate.Decision{
            Action: gate.Abort,
            AbortReason: fmt.Sprintf("file %s must be read before write/edit", path),
            HookName: g.Name(),
        }
    }
    return gate.Decision{Action: gate.Continue}
}
```

**关键：`gate.Context` 接口需要扩展**——目前只有 AgentID/TaskID 等基础字段。需要给 `gate.Context` 加一个 `StoreView() store.ReadView` 方法（或在 ToolContext 具体类型里加），让 Gate 能查询任务级状态：

```go
// internal/gate/gate.go（§4.4 接口的微调）

type Context interface {
    Phase() Phase
    AgentID() string
    TaskID() string
    Ctx() context.Context
    StoreView() store.ReadView  // ← 新增：让 Gate 能查询任务状态（ReadSet / Artifacts 等）
}
```

`store.ReadView` 是只读视图接口（与 v4 现有 `AgentStoreView` 同档设计），只暴露 `GetTask` / `GetReadSet` / `GetArtifacts` 等查询方法，不能写入。

##### 5.2.1.4 生命周期

| 时机 | ReadSet 行为 |
|---|---|
| 任务创建（`store.PublishTask`）| `ReadSet` 初始化为空 map（或 nil，UpsertReadSet 时延迟初始化）|
| 任务执行中（read_file 成功）| `read-set-write` Reactor 异步写入 |
| 任务结束（completed / failed / cancelled）| **不主动清理**——任务对象本身在 Store 中保留供查询（与 Artifacts 同等待遇）|
| Store 历史压缩（v5.x 增量）| 与 task.Artifacts 同档处理：保留 / 截断策略由 Store 历史压缩模块决定 |

**为什么不跨任务持久化**：
- ReadSet 是"已读判定"的依据，是任务内时序属性；跨任务复用会导致"我在 task A 读过 file X，task B 写 X 不需 read"这种错误判定
- 跨任务的"项目文件知识"由 [MemoryManageSystem.md](MemoryManageSystem.md) 的 Project Memory 承接，那是不同维度的知识

##### 5.2.1.5 与 §5.2 反模式治理的闭环

Phase 6 落地完成后的状态变化：

| 维度 | 旧（v4）| 新（v5 Phase 6 后）|
|---|---|---|
| "已读"判定来源 | 反查 `Store.GetToolCallHistory()` ToolCallRecord | 查询 `task.ReadSet`（O(1)）|
| 写入路径 | 不存在显式写入——隐含在工具调用历史中 | `read-set-write` Reactor 订阅 `KindToolResult` 异步写入 |
| Gate 与状态来源耦合 | 紧耦合（Gate 内部硬编码 GetToolCallHistory 调用）| 解耦——Gate 只查 ReadView，写入由 Reactor 独立完成 |
| 性能 | O(N) per check（N = 工具调用历史长度）| O(1) per check |
| 测试覆盖 | 现有 require-read-before-write 测试 | 现有测试保留（Gate 行为不变）+ 新增 read-set-write Reactor 单测 + ReadSet 写入读取联合测 |

##### 5.2.1.6 Phase 6 实施步骤（草案）

```
Step 6.1: 在 internal/store/task.go 加 Task.ReadSet 字段 + ReadInfo struct
Step 6.2: 在 internal/store/ 加 UpsertReadSet / GetReadSet API
Step 6.3: gate.Context 接口加 StoreView() 方法 + 实现注入
Step 6.4: 实现 read-set-write Reactor（订阅 KindToolResult，filter read_file）
Step 6.5: 重写 require-read-before-write Gate，改读 ReadSet
Step 6.6: 删除 Store.GetToolCallHistory 在 Gate 路径上的引用（可能仅此一处）
Step 6.7: 单元测试 + 集成测试
```

预估工作量：~500 行代码 + ~150 行测试。

---

##### 5.2.1.7 当前版本的不确定性

本节标记为"初版草案"原因——以下决议在 Phase 4 / 5 实施过程中可能调整：

1. **`gate.Context` 加 `StoreView()` 方法**会侵入 §4.4 已定稿的 Context 接口。Phase 4 实施时如果发现还有其他 Gate 需要访问任务状态，可能要把 StoreView 设计得更通用
2. **Reactor.Subscribe 返回 []EventKind 是否够用**——如果未来需要按 `tool=read_file` 这种字段过滤，可能要引入更细的订阅 API 而不是在 Run 内 filter
3. **ReadInfo.Hash 字段是否填充**——v5 Phase 6 暂不填（与 v4 §7 hashline 整合留作 v5.x 增量）

这三点都属于"不阻塞 Phase 6 启动但实施时可能微调"——现在拍板的方案是默认起点，Phase 6 启动时基于实操决定是否调整。

### 5.3 `team-awareness-*` 的真实问题：不只是命名

**现状**：`internal/hook/builtin/team_awareness_task_start.go` 与 `team_awareness_loop_pre.go` 在 TaskStart / LoopPre 阶段被调用，向 history 注入团队快照文本。

**为什么是反模式**（命名层）：
- 不在状态转变时触发（LoopPre 是 ReactLoop 节奏点，不是状态变更）
- 没有决策能力（不能 Abort，只能注入或不注入）
- "Hook" 的语义在工程界普遍暗示"切入点 + 决策"，但这两个组件纯粹是"内容供给"

**为什么是更深层反模式**（架构层，**2026-04-30 重新认识**）：
- 团队感知信息**每轮重新生成**——同一份团队快照在 N 个 Agent 的每轮 ReactLoop 顶部都被重算一遍
- **缺持久化**——系统重启后所有项目知识丢失；跨任务、跨会话的知识无法复用
- **被动注入而非主动拉取**——Agent 不知道这些信息从哪来，无法按需查询，无法用语义索引找到相关历史
- 这是 V2 早期 Sprint 1 的临时修复，与早期 RFC 中"Trigger 层"设计意图本就漂移

**v5 治理**：原计划"改名 Provider，机制零变化"——但仅换名字根本没解决上面任何架构问题。**最终决议**：team-awareness 三个 hook **直接删除**：

- `TeamSnapshot` / `FileAwareness` 迁入 [MemoryManageSystem.md](MemoryManageSystem.md) 的 Process Memory，由 Agent 在 `processTask` 入口从 `MemoryStore.Query()` 主动拉取
- `GoalAnchor` 直接删除（`task.Description` 本身已承载目标，重复注入是冗余）
- ReactiveSystem 不再立 Provider 抽象 / ProviderRegistry

详见 [MemoryManageSystem.md §0-§4](MemoryManageSystem.md)。

---

## 6. Reactor 子系统的关键设计决策点（待讨论）

> 本节列出 Reactor 子系统在动笔写 spec 之前必须决策的岔路口。每个岔路口都给出候选方案 + 倾向 + 理由，但**最终方案待对齐后再定稿**。
>
> **范围说明**：本节主要讨论**用户 Reactor**（YAML 声明）的设计——动作语言、配置粒度、执行模型等。**内置 Reactor**（开发者 Go 注册）由原则 2 决定其形态，本节不再单列讨论；但内置 Reactor 与用户 Reactor 共享同一 `ReactorRegistry` 注册机制（区别在注册入口与 Sync 许可）。
>
> **与 §7 的关系**：§7（Agent 状态显式化）与本节不是顺序依赖，而是**配套设计**——状态机给 Reactor 提供事件源，Reactor 承接状态机的副作用，二者必须一起设计、一起验证。

### 6.1 动作语言（已决议：B + D）

**Q4 决议（2026-04-29）**：采纳 **B（内置工具调用） + D（投递新任务）** 混合方案，不引入 Shell（A 安全风险）与 DSL（C 复杂度过高）。D 选项进一步**拆分为三个动词 + 一个修饰符**，覆盖完整的"触发后动作"语义空间。

#### 6.1.1 候选回顾

| 候选 | 形式 | v5 决议 |
|---|---|---|
| A. Shell 命令 | `run: ./scripts/notify.sh` | ❌ 拒绝（安全风险面太大）|
| **B. 内置工具调用** | `call: <tool>; args: {...}` | ✅ **采纳** |
| C. 嵌入式 DSL | Starlark / Lua / 表达式 | ❌ 拒绝（首版表达力溢价不值得）|
| **D. 投递新任务** | 拆为 `publish_task` / `invoke_llm` / `spawn_agent` 三动词 | ✅ **采纳并扩展** |

附加：最小化 `when:` 条件字段（仅支持 `${event.x} 比较 y` 简单判断），避免引入 DSL 也能覆盖大部分条件分支需求。

#### 6.1.2 D 选项的三动词拆分

D 选项不再是单一的 "publish a new task"，而是按"事件触发后该走哪条执行路径"拆分为三个语义不重叠的动词，再加一个修饰符：

| 动词 | 走公告板？ | 走 ReactLoop？ | 工具权限 | 提示词来源 | 适用场景 |
|---|---|---|---|---|---|
| `publish_task` | ✅ | ✅ | 取决于认领 kind | 外部文件 | 已有 kind 资源池处理新任务（最便宜，复用现有链路）|
| `invoke_llm` | ❌ | ❌（单轮）| **无**（原则 5）| 外部文件 | 一次性 LLM 文本生成（写摘要、生成通知文案）|
| `spawn_agent` | ✅（独立队列）| ✅ | 取决于 base_kind | 外部文件 | 创建临时 agent 实例（base_kind 继承 + override 字段）|
| `spawn_agent` + `via_translator` | ✅ | ✅ | base_kind 工具集（但 translator **无工具**）| reactor 独立 LLM 加工后的产出 | spawn_agent 的初始 prompt 由 reactor 独立 LLM 二次加工 |

**关键边界（必读）**：三动词覆盖的场景**互不重叠**，不是同一个东西的三种写法。`publish_task` 是"已有 agent 干新任务"，`invoke_llm` 是"绕过 agent 系统直接消费一次 token"，`spawn_agent` 是"创建独立的临时 agent 实例"。spec 阶段会在工具描述里明文标注此边界。

#### 6.1.3 通用 prompt 加载层（三动词共享）

为了"一开始就做好足够的扩展空间"，所有动词的 prompt 来源走同一套抽象，未来只在这层加新字段：

```yaml
# 通用 prompt 来源结构（所有需要 prompt 的字段都用这套）
prompt:
  file: ./prompts/xxx.md           # ← v5 首版唯一实现
  # url: https://...               # ← 预留位（未来）
  # inline: |                       # ← 预留位（未来）
  #   ...
  args:                             # 模板变量替换
    task_id: ${event.task.id}
    reason: ${event.task.reason}
```

**安全约束**：
- `file:` 路径必须**在启动期校验存在 + 可读 + 在 ProjectRoot 内**，与 v4 §11.5.2 `system_prompt_file` 同一套约束
- prompt 内容在**配置加载阶段读入内存**，运行时不再读磁盘——避免 TOCTOU 风险
- `args:` 仅支持 `${event.x.y}` 字段引用，**禁止** shell-style 替换或 eval——纯字符串拼接语义

#### 6.1.4 四个场景的具体 YAML 形态

```yaml
reactors:
  # ── 场景 1：投递任务到公告板 ────────────────────────
  - on: task_failed
    publish_task:
      kind: explorer                  # 必填，路由到已声明的 kind
      description:
        file: ./prompts/investigate_failure.md
        args:
          task_id: ${event.task.id}
          reason: ${event.task.reason}

  # ── 场景 2：一次性 LLM 调用（上下文隔离，无工具）────
  - on: task_failed
    invoke_llm:
      model: qwen3.6-flash            # 可选；缺省回落 llm.default_model
      prompt:
        file: ./prompts/summarize_failure.md
        args:
          task_id: ${event.task.id}
      output:                          # 必填，否则结果丢失
        write_file: ./logs/failure-${event.task.id}.md
        # 或 send_message: { to: admin, content_var: output }
        # 或 emit_trace: { kind: failure_summary }

  # ── 场景 3：启动 ad-hoc agent ──────────────────────
  - on: task_failed
    spawn_agent:
      base_kind: explorer             # 必填，继承该 kind 的工具/模型/行为参数
      override:                        # 可选，仅覆盖列出的字段
        system_prompt:
          file: ./prompts/post_mortem_system.md
        agent_max_loops: 5
        context_limit: 8000
      initial_task:
        description:
          file: ./prompts/post_mortem_task.md
          args:
            task_id: ${event.task.id}
      lifecycle: one_shot              # 一次性，任务完成即销毁；预留 persistent

  # ── 场景 4：spawn_agent + via_translator ───────────
  - on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:              # ← 关键修饰符
            # translator 默认走 reactor 自带 LLM client（无工具、无 history、上下文隔离）
            # 不允许 translator 复用当前 agent —— 见原则 5
            translator_prompt:
              file: ./prompts/translate_for_subagent.md
              args:
                task_id: ${event.task.id}
            # translator 的输出直接成为新 agent 的 initial_task.description
      lifecycle: one_shot
```

#### 6.1.5 v5 首版的实施先后关系

按 §10.4 Phase 5 内部步骤序列：

```
S1: 公共基础设施 ───────────────────────┐
    - prompt 加载层（file + 启动期校验 + 内存缓存）
    - args 模板变量替换（${event.x.y}）
    - reactor schema 解析 + when 条件字段
    │
    ▼
S2: publish_task ──────────────────────┐  ← 零新增基础设施，复用 store.PublishTask
    - 最简，复用 v4 §11 kind 体系             首先验证 reactor 整体框架可行
    │
    ▼
S3: reactor 自带独立 LLM client ────────┐  ← S4 + S5 的共享基础设施
    - 无工具 / 无 history / 无 system prompt 注入
    - 单轮纯文本生成（原则 5）
    - cost / 超时 / 错误处理
    │
    ├──────────┬──────────┐
    ▼          ▼          │
S4: invoke_llm + output dispatcher       │
    - write_file / send_message / emit_trace 三种去向
    │                     │
    ▼                     │
S5: ad-hoc agent 生命周期机制 ──────────┘
    - base_kind 继承 + override 合并
    - lifecycle: one_shot 销毁链路
    - 队列路由（独立 event_type 或 ad-hoc 队列）
    │
    ▼
S6: spawn_agent（不含 via_translator）
    - 组合 S5 + S1 prompt 加载
    │
    ▼
S7: spawn_agent + via_translator
    - 组合 S3 reactor 自带 LLM + S6 spawn_agent
    - translator 输出 → 新 agent initial_task.description
```

**强制依赖链**：S1 → S2 + S3 → S4（依赖 S3）+ S5 → S6（依赖 S5）→ S7（依赖 S3 + S6）

**可并行点**：S2 与 S3 / S4 与 S5 互不依赖，资源允许时可并行。

**v5 首版工作量边界**：
- S1-S2 是最便宜的部分，预计是首版主体
- S3-S7 都涉及新执行路径或新生命周期机制，单独都是中等量级工作
- 若时间约束紧，**S1-S4 是最小可发布集合**（覆盖 publish_task + invoke_llm，已经是当前 80% 用例的解决方案）；S5-S7 留作 v5.1 增量

但用户已指示"V5 版本尽量全部实现"，所以默认目标是 S1-S7 全做，仅在实施阶段遇到具体阻碍时再决定缩减。

#### 6.1.6 占位 schema vs 真实实现的边界

v5 首版的 schema 定义中，以下字段是**预留占位位**，YAML 解析期接受、运行期遇到时直接报"v5.x 未实现"错误：

| 占位字段 | 当前状态 | 拟实现版本 |
|---|---|---|
| `prompt.url` | schema 接受，运行期报错 | v5.x（待实战需求驱动）|
| `prompt.inline` | schema 接受，运行期报错 | v5.x |
| `lifecycle: persistent`（spawn_agent）| schema 接受，运行期报错 | v5.x |
| 其他未来扩展字段 | 启动期 unknown field 警告 | 按需 |

**关键约束**：占位字段在 spec 中明文标注为"v5 不实现，预留扩展位"，避免用户误用后才发现。代码侧 schema 保留字段定义但实现路径直接 fail-fast。

#### 6.1.7 配套：when 条件字段

承接前面讨论，加一个最小化的 `when:` 字段以避免常见的"按 retry_count 分支"需求被迫升级到 DSL：

```yaml
reactors:
  - on: task_failed
    when: ${event.task.retry_count} >= 3
    call: send_message
    args: { to: admin, content: "终态失败：${event.task.id}" }

  - on: task_failed
    when: ${event.task.retry_count} < 3
    publish_task:
      kind: explorer
      description: { file: ./prompts/investigate.md }
```

**支持的运算符**：`==` / `!=` / `<` / `<=` / `>` / `>=` / `in` —— 仅这 7 个，**不允许**逻辑组合（and / or / not）。复杂条件需求出现时，第一选项是写多个 reactor，第二选项才是讨论是否引入 DSL。

---

### 6.2 Reactor 配置粒度（已决议：a + b 起步）

**Q5 决议（2026-04-29）**：v5 首版采纳 **a 全局 + b per-kind**，**c per-task 延后到 v5.x**。

#### 6.2.1 候选回顾

| 候选 | 配置位置 | 范围语义 | v5 决议 |
|---|---|---|---|
| **a. 全局 Reactor** | YAML 顶层 `reactors:` | "任何 agent 的任何 task 触发的事件" | ✅ **采纳** |
| **b. Per-kind Reactor** | 顶层 `reactors:` 条目加 `kind: <agent-kind>` | "**这一类 agent** 触发的事件" | ✅ **采纳** |
| c. Per-task Reactor | scheduler 调 `publish_task` 时携带 | "**这一个具体任务**的生命周期事件" | ❌ 延后到 v5.x |

#### 6.2.2 三个独立维度的关系（spec 必读）

写一条 reactor 配置实际是在 3 个维度上做选择，**三者独立、自由组合**：

| 维度 | 字段 / 位置 | 控制什么 |
|---|---|---|
| 事件类型 | `on:` | 订阅哪种事件类型（task_failed / file_written / ...）|
| **粒度（本节）** | `kind:` 为空还是指定某个 agent kind | 订阅范围内的哪些 agent 触发的事件 |
| 动作内容 | `call:` / `publish_task:` / `invoke_llm:` / `spawn_agent:` | 触发后做什么 |

`on:` 与粒度**交叉过滤**——只有事件类型匹配 *且* 来源 agent 在粒度范围内时，reactor 才触发。

#### 6.2.3 多粒度叠加规则

当一个事件同时被多个粒度的 reactor 订阅时（典型：全局 reactor + 该 agent kind 的 per-kind reactor 都订阅了 `task_failed`），**所有匹配的 reactor 全部触发，无覆盖、无优先级择一**。

**理由**：
- 覆盖语义复杂（按粒度优先级？按名字？）容易错
- 用户直觉是"我加了一条订阅它就会跑"
- 失败隔离原则保证一个 reactor 失败不影响另一个

**触发顺序**（仅指投递入队顺序，实际执行可能并发）：

```
Global reactor → Per-kind reactor
```

只有这两层（c 不做），顺序简单。trace 事件按此顺序记录，便于调试。

#### 6.2.4 spawn_agent 的 reactor 继承

承接 §6.1.2 的 `spawn_agent` 动词：spawn 出来的 ad-hoc agent 实例，其触发的事件**完整继承 base_kind 的所有 per-kind reactor + 全局 reactor**。

**理由**：ad-hoc agent 在行为上应当与 base_kind 实例无差别，否则用户预期被打破——你 spawn 一个 base_kind=explorer 的临时 agent，它跑出的事件如果不被 explorer kind 的监控 reactor 看到，会让"换个 kind 就静默"成为 footgun。

但这意味着 reactor 链可能形成环：A 的 reactor spawn 出 B → B 失败触发 base_kind reactor → 又 spawn C → ... 

**防爆炸机制**：

- **硬上限**：引入 `ReactorSpawnMaxDepth` 系统级常量（不进 YAML，类比 v4 `MailChainMaxDepth=10`），首次拟定值在 spec 阶段确定（建议范围 3-5）。超过即拒绝 spawn 并 emit `KindReactorSpawnDepthExceeded` trace 事件
- **depth 计数携带**：每个 spawn 出的 ad-hoc agent 在自身上下文中记录当前 spawn 深度，trace 事件携带此数值便于排查
- **位置**：`internal/reactor/dependency_map.go` 或同等位置定义常量

阈值定位待 Phase 5 spec 阶段实战需求驱动，先把字段框架立住。

#### 6.2.5 c 延后的理由（决策记录）

c 在 Q4 拍板三动词后**独立价值下降**：

1. "任务级链式编排"已被 `publish_task` / `spawn_agent` 部分承接——scheduler 想让 A 完成后做 B，更简单的方式是**在 A 的 description 里写"完成后请 publish_task B"**让 LLM 自然完成
2. a + b 已覆盖"系统级 + kind 级"所有静态需求；c 是"运行时动态 reactor"——这是个真正的新维度，应作为独立模块（v5.x 或 v6 单独设计），不是 v5 首版赶工
3. c 的实现复杂度跳一级——**任务级注册表的生命周期管理**比 a/b 复杂得多（任务终结时清理、取消时回滚部分副作用、scheduler LLM 学会输出 reactor 配置），与 v5 首版"S1-S7 全做"目标不兼容

#### 6.2.6 ReactorRegistry 的简化收益

c 不做，意味着 ReactorRegistry **没有运行期动态注册/注销机制**——所有 reactor 在启动期固定，与 GateRegistry 一致。这大幅降低 Phase 4 ReactorRegistry 的实现复杂度（无需任务级注册表、无需生命周期同步、无需取消时的清理协议）。

### 6.3 Reactor 的执行模型：同步还是异步

按原则 2 二分：

**用户 Reactor（永远异步）**：

| 候选 | 形式 | 优点 | 缺点 |
|---|---|---|---|
| ii. 异步 fire-and-forget | 状态转变 → 投递事件到 channel → 独立 goroutine 消费 | 主流程零阻塞 | 失序、丢失风险（buffer 满）、调试困难 |
| **iii. 异步带回执** | ii 但每个 Reactor 完成后写 trace 事件 | ii 优点 + 可观测 | 实现复杂度比 ii 高 |

**用户 Reactor 倾向 iii**——异步执行 + trace 事件回执：
- 主流程零阻塞符合"Reactor 必须隔离"原则
- trace 事件回执让 Reactor 自身的执行成为可调试、可监控的对象
- 失序问题：每个事件携带 sequence ID 或 timestamp，让用户在编写 Reactor 时可判断时序

**内置 Reactor（允许声明 Sync: true）**：

承接原则 2——某些内置副作用必须同步完成才能继续主流程（例如"进入 InReactLoop 前初始化 history"，没初始化好就跑 LLM 会崩）。这类内置 Reactor 在注册时显式声明 `Sync: true`，主流程在该状态变更点会顺序等待这些 Sync Reactor 完成后才继续。

| 同步语义 | 适用场景 | 失败处理 |
|---|---|---|
| `Sync: true`（仅内置可声明）| 系统不变量必须维持的副作用（history 初始化、FileCache 清理、OnTaskEnd 回调）| 失败 = 系统级 panic（trace 显眼标红 + 默认行为视具体场景：重试 / 终止任务 / fail-fast）|
| `Sync: false`（内置 + 用户共用，用户唯一选项）| 通知、记录、派生任务、外部触发 | panic-isolated，失败仅记日志 |

**待你拍板**：用户 Reactor 走 iii 异步带回执 + 内置 Reactor 允许声明 Sync 二分模型，是否同意？

### 6.4 v5 首批支持的触发事件清单（已决议：5 + 1 + 3）

**Q7 决议（2026-04-30）**：v5 首批支持 **9 个事件**——5 个来自现有事件源（payload 升级即可），1 个来自 agent 状态机全新构建（`agent_state_changed`，依赖 Q9 + Q10 + Phase 3 配套落地，采用方案 A "配套同开"），3 个来自 shell 工具升级派生（`KindShellExecuted` + `KindShellTimeoutPending` + `KindShellTimeoutResolved`，详见 ToolUpgradePlan.md §2）。

**Q12 决议（2026-04-30）**：再补 **1 个事件 `KindShellExecuted`**，但仅向**内置 Reactor** 开放订阅，**用户 YAML schema 暂不开放**（占位预留），等实战需求驱动再开。

**Q13 配套决议（2026-04-30）**：超时机制改为「事件 + TimeoutHandler」二段式抽象后，再补 **2 个事件 `KindShellTimeoutPending` / `KindShellTimeoutResolved`**——同样仅内置 Reactor 可订阅，用户 YAML 暂不开放（详见 [ToolUpgradePlan.md §2.8](ToolUpgradePlan.md#28-shell-超时机制)）。

#### 6.4.1 首批清单

| # | 事件 | 语义 | 事件源 | 标记 |
|---|---|---|---|---|
| 1 | `KindTaskClaimed` | Pending → Processing | Task 状态机（已存在）| ✅ |
| 2 | `KindTaskCompleted` | Processing → Completed | Task 状态机（已存在）| ✅ |
| 3 | `KindTaskFailed` | Processing → Failed | Task 状态机（已存在）| ✅ |
| 4 | `KindTaskCancelled` | * → Cancelled | Task 状态机（已存在）| ✅ |
| 5 | `KindFileWritten` | 文件写入完成 | 工具执行后派生（已存在）| ✅（承接 record-artifact 迁移）|
| 6 | `KindAgentStateChanged` | Agent 实例状态变更 | **全新构建**（Phase 3 落地）| 🔗 **配套依赖**（依赖 Q9 状态枚举 + Q10 SetState 归属 + Phase 3 实施）|
| 7 | `KindShellExecuted` | shell 命令执行完成（含成功/失败/超时）| 工具执行后派生（**新增**）| 🔒 **仅内置 Reactor 可订阅**（用户 YAML schema 暂不开放，详见 §7.4）|
| 8 | `KindShellTimeoutPending` | shell 命令运行时长达到 timeout 阈值，TimeoutHandler 即将决策 | 工具超时点派生（**新增**）| 🔒 **仅内置 Reactor 可订阅**（事实记录，handler 决策由 [ToolUpgradePlan §2.8](ToolUpgradePlan.md#28-shell-超时机制) 承接）|
| 9 | `KindShellTimeoutResolved` | TimeoutHandler 决策完成（携带 decision 与 reason）| 工具超时点派生（**新增**）| 🔒 **仅内置 Reactor 可订阅** |

#### 6.4.2 延后事件清单（v5 不开，按实战需求逐步开放）

| 事件 | 延后理由 |
|---|---|
| `KindTaskRetry` | v4 §9.6 MaxRetries 已管控重试本身，reactor 在此能做的仅限观察性记录，价值有限 |
| `KindHistoryCompaction` / `KindHistoryTruncated` | 纯内部机制事件，用户能挂的 reactor 仅是 metric 记录，需求等级低 |
| `KindToolCall` / `KindToolResult` | 频次高、payload 大、缺乏明确 reactor 用例 |

#### 6.4.3 事件源的三类不对称（spec 必读）

5 + 1 + 3 不是 9——它们在实施路径上完全不对称：

| 事件 | 实施路径 |
|---|---|
| 1-5（5 个已存在事件源）| **Phase 2 升级 payload 结构**——加 prev/new_status / cause / cancel_source / 等结构化字段。事件本身已经在系统中产生 |
| 6（agent_state_changed 全新事件源）| **Phase 3 全套构建**——Q9 引入 AgentRuntimeState 枚举 + Q10 落地 SetState API + 6 个状态切换点穿插主流程 + trace.EventKind 新增 + payload schema 设计 |
| 7-9（shell 三事件全新事件源）| **Phase 1 + ToolUpgradePlan §2 配套**——Phase 1 shell.CommandFilter 重构为 Gate 时即 emit `KindShellExecuted`；TimeoutHandler 抽象落地（§2.8）时即 emit `KindShellTimeoutPending` / `KindShellTimeoutResolved`。trace.EventKind 新增 + payload schema 设计 |

#### 6.4.4 方案 A "配套同开" 的连锁影响

Q7 选择方案 A 等于**预承诺**以下决议方向：

- **Q9 必落地**：AgentRuntimeState 枚举必须引入（具体枚举内容仍需 Q9 单独拍板，§7.2 列出的 8 个候选 Idle/Polling/Claiming/InReactLoop/WaitingApproval/Compacting/Truncating/Terminating 是起点）
- **Q10 必落地**：SetState API 形态必须落地（显式 vs helper 的具体决议仍待 Q10）
- **Phase 3 必执行**：与 Phase 4 ReactorRegistry 配套上线（§10.1 依赖图已确认这是配套设计）
- **Phase 2 工作量边界扩大**：除 5 个现有事件 payload 升级外，还要预留 `agent_state_changed` 与 3 个 shell 事件（`KindShellExecuted` / `KindShellTimeoutPending` / `KindShellTimeoutResolved`）的事件 schema 定义

#### 6.4.5 各事件 payload 最小字段集（草案）

Phase 2 spec 阶段细化，但首批清单决定数量与起点字段：

```
task_claimed:    {task_id, agent_id, kind, claimed_at, prev_status, new_status}
task_completed:  {task_id, agent_id, kind, completed_at, result_summary, prev_status, new_status}
task_failed:     {task_id, agent_id, kind, failed_at, reason, retry_count, prev_status, new_status}
task_cancelled:  {task_id, agent_id?, kind?, cancelled_at, cancel_source, reason, prev_status, new_status}
                 # cancel_source 必填：user / watchdog / scheduler
                 # 因为 reactor 用例多样（清理 vs 回滚 vs 通知）取决于谁取消
file_written:    {task_id, agent_id, kind, file_path, byte_count, tool_used, written_at}
                 # tool_used: write_file / edit_file（首批），未来扩展 apply_patch 等
agent_state_changed: {agent_id, kind, prev_state, new_state, changed_at, cause}
                     # 字段定义依赖 Q9/Q10 拍板后细化
shell_executed:  {task_id, agent_id, kind, command, exit_code,
                  stdout_excerpt, stderr_excerpt, duration_ms,
                  outcome, executed_at}
                 # outcome 枚举：success / failure / timeout
                 # stdout_excerpt / stderr_excerpt 截断（前后各 N 字节），完整内容仍在 trace 文件
                 # 仅内置 Reactor 可订阅（详见 §7.4）
shell_timeout_pending:  {task_id, agent_id, command, elapsed_sec,
                         stdout_excerpt, stderr_excerpt, previous_waits, triggered_at}
                        # previous_waits: TimeoutHandler 已经 Wait 续命过几次（防 handler 无限续命）
                        # 详见 ToolUpgradePlan §2.8
shell_timeout_resolved: {task_id, agent_id, command, decision, extra_seconds,
                         reason, resolved_at}
                        # decision 枚举：truncate / wait / continue
                        # 即使 v5 只产出 truncate，schema 一开始就支持三档
```

#### 6.4.6 cancel_source 的特殊提示

`task_cancelled` payload 必须含 `cancel_source` 字段，否则 reactor 写不了精准条件。Phase 2 spec 阶段需要把现有取消路径全部摸清并枚举：

- 用户主动取消（CancelRegistry）
- watchdog 系统级取消
- scheduler LLM 决定取消
- 未来扩展（如超时取消、依赖任务失败级联取消）

这本身是 Phase 2 一项独立工作，不能因为"先开个事件再说"而草率定型。

### 6.5 事件 payload 是否需要先升级（前置依赖）

**现状**：现有 `trace.Event` 的 payload 多数只带 task ID 和 reason 字符串，**没有结构化的 from_state / to_state / actor / cause / context_diff**。

如果 Reactor 要让用户在 YAML 里写 `${event.from_state}` 这种引用，事件 payload 必须**显式携带状态转变的结构化语义**，而不是让用户自己 grep reason 字符串。

**v5 隐含前置任务**：
- 升级 `trace.Event` 结构体，加 `From / To / Cause / Diff` 等结构化字段
- 在所有状态变更点显式 emit 升级后的事件
- 这部分工作量独立——可以挂在现有 `TraceUpgrade.md`（当前空文件）下作为前置基础设施

**待你拍板**：TraceUpgrade 是否提前到本模块的 §S0 一并做掉？还是分开走两条线？

> **2026-05-01 补**：TraceUpgrade 已独立成 [TraceUpgrade.md](TraceUpgrade.md) 作为 ReactiveSystem 的 Phase 2，spec 已定稿（schema B + 4 个新 EventKind + Transition / ShellExec / ShellTimeout 三个 sub-payload + CLI viewer 适配 + 4 条新启发式异常检测）。

### 6.6 ReactorRegistry 完整接口形态（与 §4.4 GateRegistry 对称）

**Q4' 决议（2026-05-01）**：参考 [§4.4 GateRegistry 的统一形态](#44-gateregistry-的统一形态接口式-context)，Reactor 子系统也定义同等详尽的接口形态。本节仅给**接口与类型签名**，具体实现（panic recover 实现细节 / channel buffer 大小 / goroutine 池形态等）留待 Phase 4 实施时定。

#### 6.6.1 设计核心思路

把 Reactor 系统拆解为两层（与 Gate 同模式）：

| 层 | 内容 | 跨场景 |
|---|---|---|
| **协议层** | EventKind 订阅 / Sync vs Async 标记 / 失败隔离 / Decision-less 返回值（区别于 Gate）| ✅ 跨域统一 |
| **事件层** | trace.Event（已结构化，含 Transition / ShellExec / ShellTimeout 子结构）| ✅ 跨域统一 |

**与 GateRegistry 的关键差异**：

| 维度 | Gate | Reactor |
|---|---|---|
| 触发时机 | 动作之前 | 状态变化之后 |
| 决策权 | ✅ 可 Abort | ❌ 不可（[原则 4](#原则-4reactor-不允许直接驱动新状态转换)）|
| 同步性 | 总是同步（主流程等结果）| Sync / Async 二分（[原则 2](#原则-2reactor-分两类失败语义不同)）|
| 失败语义 | 内置失败 = panic 主流程；用户失败 = 由 Decision 表达 | 内置 Sync 失败 = trace 显眼记录 + 视场景；用户 Async 失败 = panic-isolated + 仅记日志 |
| 输入 Context | 接口式（Tool / Mailbox / 未来 Cron 等多形态）| 单一 trace.Event（已经是结构化通用形态）|

**Reactor 不需要"接口式 Context"**——因为 trace.Event 已经是经过 Phase 2 升级的结构化通用类型，三个 sub-payload（Transition / ShellExec / ShellTimeout）覆盖跨域语义。Reactor 直接消费 Event。

#### 6.6.2 接口与类型草案

```go
// internal/reactor/reactor.go（新建，Phase 4）

import "agentgo/internal/trace"

// Reactor 是单个 Reactor 的接口
type Reactor interface {
    // Name 唯一标识，用于日志、Registry 去重、trace 回执
    Name() string

    // Subscribe 声明本 Reactor 订阅哪些 EventKind
    // 单个 Reactor 可订阅多个事件类型（典型：监控类 Reactor 订阅所有 KindTask*）
    // 不支持运行期改变订阅（启动期固定，对应 Q5 决议"不做 c per-task 粒度"）
    Subscribe() []trace.EventKind

    // Run 处理事件
    // 返回 error 仅对 Sync Reactor 有意义——Sync 失败需 trace 显眼标红
    // Async Reactor 的 error 仅记日志，不传播
    Run(ev trace.Event) error

    // IsSync 标记同步性
    // 仅内置 Reactor 可声明 true；用户 YAML 声明的 Reactor 强制 false
    // 见 §6.3 Reactor 执行模型决议
    IsSync() bool

    // Priority 决定同 EventKind 多个 Reactor 的执行顺序（数字越小越先执行）
    // 范围 [0, 1000]：
    //   0-100   系统级强制（如 panic-emit-failed）
    //   500     默认中段
    //   900-1000 观察类（如 trace-history-event）
    // Sync Reactor 严格按 priority 顺序串行执行
    // Async Reactor 的 priority 仅决定投递入队顺序，实际执行可能并发
    Priority() int
}

// ReactorRegistry 是统一注册表
type ReactorRegistry struct {
    // 按 EventKind 分桶；同 EventKind 内按 priority 排序
    reactorsByKind map[trace.EventKind][]Reactor

    // Async 投递通道（具体 buffer 大小、goroutine 池形态留待实施时定）
    // ... 实现细节非接口形态范围 ...
}

// Register 启动期注册 Reactor
// 同名 Reactor 重复注册返回 error
// Subscribe 返回的 EventKind 必须非空（否则 Reactor 永远不被触发，是装配 bug）
func (r *ReactorRegistry) Register(reactor Reactor) error

// Dispatch 入口——主流程 emit trace 事件后调用
// 同步部分（Sync Reactor 串行执行 + 失败 trace 显眼记录）必须在本调用内完成
// 异步部分（Async Reactor 投递）立即返回，由后台 worker 消费
//
// 调用方约定：Dispatch 不返回 error——任何 Reactor 失败都被隔离
//             调用方不需要也不应该处理 Dispatch 结果
func (r *ReactorRegistry) Dispatch(ev trace.Event)
```

#### 6.6.3 内置 Reactor 注册示例

```go
// internal/reactor/builtin/record_artifact.go（v5 形态，从 v4 hook 迁移而来）

type RecordArtifactReactor struct{}

func (r *RecordArtifactReactor) Name() string { return "record-artifact" }
func (r *RecordArtifactReactor) IsSync() bool { return false }  // Async：写 artifact 列表不阻塞主流程
func (r *RecordArtifactReactor) Priority() int { return 950 }   // 观察类

func (r *RecordArtifactReactor) Subscribe() []trace.EventKind {
    return []trace.EventKind{trace.KindFileWritten}
}

func (r *RecordArtifactReactor) Run(ev trace.Event) error {
    // 注：Phase 4 时 trace.Event 已经经过 TraceUpgrade Phase 2 升级
    // 直接访问 ev.Path / ev.Bytes / ev.Hash 等字段即可
    return store.AppendArtifact(ev.TaskID, ev.Path)
}
```

```go
// internal/reactor/builtin/task_end_callback.go（v5 形态，从 inline 迁移而来）

type TaskEndCallbackReactor struct {
    callbacks []TaskEndCallback  // bootstrap 注入
}

func (r *TaskEndCallbackReactor) Name() string { return "task-end-callback" }
func (r *TaskEndCallbackReactor) IsSync() bool { return true }   // Sync：必须在主流程继续前跑完
func (r *TaskEndCallbackReactor) Priority() int { return 100 }   // 系统级强制

func (r *TaskEndCallbackReactor) Subscribe() []trace.EventKind {
    return []trace.EventKind{
        trace.KindTaskCompleted,
        trace.KindTaskFailed,
        trace.KindTaskCancelled,
        trace.KindTaskRetry,
    }
}

func (r *TaskEndCallbackReactor) Run(ev trace.Event) error {
    for _, cb := range r.callbacks {
        if err := cb(ev); err != nil {
            return fmt.Errorf("task-end-callback failed: %w", err)
        }
    }
    return nil
}
```

#### 6.6.4 用户 Reactor 通过 YAML 注册（Phase 5）

§6.1 已经详细定义了 YAML schema（三动词 + via_translator + when 字段）。Phase 5 实施时引入 YAML loader 把声明式配置转换为 `Reactor` 接口的具体实现：

```go
// internal/reactor/userdef/yaml_loader.go（伪代码）

func LoadFromYAML(yamlPath string, registry *ReactorRegistry) error {
    decls, err := parseYAML(yamlPath)
    if err != nil { return err }

    for _, decl := range decls {
        // 把每个 reactor 声明转换为 Reactor 接口实现
        userReactor := &UserReactor{
            name:      decl.Name,
            kinds:     []trace.EventKind{decl.OnEvent},
            whenExpr:  decl.When,        // ${event.x.y} 比较表达式
            action:    decl.Action,      // publish_task / invoke_llm / spawn_agent
            // 用户 Reactor 强制 IsSync=false（原则 2）
        }
        if err := registry.Register(userReactor); err != nil {
            return err
        }
    }
    return nil
}
```

`UserReactor.Run(ev)` 内部：
1. 评估 `when` 表达式（false → 直接返回 nil 不做事）
2. 按 action 分发（publish_task → 调 store.PublishTask；invoke_llm → 走 §6.1.2 isolated LLM client；spawn_agent → 创建临时 agent 实例）

#### 6.6.5 EventKind 订阅过滤的边界

**v5 首版仅支持精确 EventKind 匹配**——`Subscribe()` 返回 `[]trace.EventKind`，registry 按 kind 索引分桶。

**不支持**的形态（v5 首版，未来 v5.x 可考虑）：
- 通配匹配（如 `KindTask*`）：当前需要订阅所有 task lifecycle 时只能列举所有 5 个 kind
- 字段级 filter（如 `KindToolResult && tool=read_file`）：当前在 `Run` 内部 filter，详见 §5.2.1.2 read-set-write Reactor 示例
- 跨 EventKind AND/OR（如"`KindTaskFailed` 后 5 分钟内若发生 `KindTaskRetry`"）：超出 Reactor 单事件触发模型，需要更复杂的状态机

这三种扩展属于"出现具体需求时再加 API"——目前的简化形态够用，避免过度设计。

#### 6.6.6 失败隔离的实现边界（接口层不强制）

接口层面只规定**契约**：
- Sync Reactor 失败 → trace 显眼标红 + 上层视场景处理（panic-emit-failed 这种系统不变量类失败可以 fail-fast；其他场景 trace 记录后继续）
- Async Reactor 失败 → panic-isolated（recover 在 worker goroutine）+ 仅记日志，不影响主流程

**具体实现技术**（Phase 4 实施决定）：
- Sync Reactor 是否每个独立 goroutine + WaitGroup？还是同一个调度 goroutine 串行 try/recover？
- Async Reactor 是固定 worker 池？per-Reactor 一个 goroutine？还是 ad-hoc spawn？
- panic recover 是逐 Reactor 包装，还是 dispatcher 统一拦截？

这些都是实施细节——只要不破坏上述契约，就有自由度。

#### 6.6.7 与 §4.4 GateRegistry 的对照表

| 维度 | GateRegistry | ReactorRegistry |
|---|---|---|
| 输入 Context 形态 | 接口（`gate.Context` + 具体类型 ToolContext / MailboxContext） | 单一 `trace.Event`（已经结构化）|
| Phase 枚举 | 跨域统一（tool: / mailbox: 前缀）| 复用 trace.EventKind，无独立 Phase 枚举 |
| Decision 类型 | `Decision{Action, AbortReason}` | **无返回 Decision**——Reactor 不可决策 |
| 失败语义 | Abort 阻塞主流程；其他失败 = 装配 bug 导致 panic | Sync 显眼记录；Async panic-isolated |
| 注册时机 | bootstrap.go 启动期固定 | 内置在 bootstrap 启动期；用户 YAML 启动期加载 |
| Dispatch 返回 | `Decision`（让调用方知道是否 Continue / Abort）| `void`（Reactor 失败被吞，不影响主流程）|
| 配置入口 | 仅开发者 Go 注册 | 内置 = Go 注册；用户 = YAML 声明 |
| **接口对称性** | ★★★★★ | ★★★★（Reactor 不需要 Context 接口因为 Event 已通用）|

#### 6.6.8 Phase 4 实施清单（接口落地范围）

| 内容 | 大致行数 |
|---|---|
| `internal/reactor/reactor.go`：Reactor 接口 + ReactorRegistry struct + Register / Dispatch | ~150 |
| `internal/reactor/dispatcher.go`：Sync 串行 + Async worker 池骨架 | ~200 |
| `internal/reactor/builtin/record_artifact.go`：T7 迁移 | ~80 |
| `internal/reactor/builtin/task_end_callback.go`：从 terminateTask inline 迁移 | ~100 |
| `internal/reactor/builtin/trace_history_event.go`：从 agent.go 多处 inline 迁移 | ~80 |
| 单元测试（接口契约 + 失败隔离 + 多 Reactor 协同）| ~300 |
| **小计** | **~910 行** |

跟 §5.1.1 估算的 Phase 4 工作量一致（包含了 §5.1.1 决议的"3 个最小覆盖矩阵"）。

---

## 7. Agent 实例状态显式化（Agent 字段升级的具体形态）

> **与 §6 的关系**：本节与 §6 是**配套设计而非顺序依赖**。状态机给 Reactor 提供事件源（emit `KindAgentStateChanged`），Reactor 承接状态机的副作用（接管 agent.go 内散落的 OnTaskEnd / FileCache 清理 / 压缩 emit 等）。二者必须一起设计、一起验证——本节落地后，§6 的 Reactor 抽象才有完整的事件源；§6 落地后，本节散落的副作用才有干净的承接位。

承接 v5 命题"Agent 字段可能需要一些适当的升级"。当前 Agent 实例的运行时状态是**完全隐式的**——散落在多个字段里：

```
Polling             → for 循环位置（无字段）
Claiming            → ClaimTask 调用瞬间（无字段）
InReactLoop         → processTask 函数内（loop 变量）
WaitingApproval     → ApprovalCh recv 阻塞（无字段）
Compacting          → 压缩函数调用栈内（无字段）
Truncating          → TruncateHistory 调用瞬间（无字段）
Idle                → idleCount 计数器（部分显式）
```

### 7.1 为什么要显式化

- **Reactor 订阅需要**：如果想让用户配 "Agent 进入 WaitingApproval 状态超过 5 分钟自动取消"，"WaitingApproval 状态" 必须有显式枚举
- **健康检查与监控**：未来的 watchdog / progress notifier 需要精确知道 "agent 现在在干嘛"
- **附录 D suspend/resume 前置**：v4 附录 D 的休眠/唤醒优化前提就是状态机显式化
- **调试**：trace 事件能携带 "状态从 X 转到 Y"，远比 "agent 1 调用了 ClaimTask" 信息密度高

### 7.2 状态枚举（已决议：4 个核心状态）

**Q9 决议（2026-04-30）**：v5 引入 4 个核心状态，原 §7.2 草案的 8 个候选中 Polling / Claiming / Compacting / Truncating 被移除——它们要么与其他状态语义重复，要么是更适合用 trace 事件而非状态枚举监控的瞬时子动作。

```go
type AgentRuntimeState string

const (
    AgentStateIdle             AgentRuntimeState = "idle"              // 无任务，轮询 Store 中
    AgentStateProcessing       AgentRuntimeState = "processing"        // 处理任务中（含 ReactLoop / LLM 调用 / 工具执行 / 历史压缩 / 截断）
    AgentStateWaitingApproval  AgentRuntimeState = "waiting_approval"  // 阻塞等用户批准（仅 needs-approval 工具调用时触发）
    AgentStateTerminating      AgentRuntimeState = "terminating"       // 任务结束清理中
)
```

#### 7.2.1 各状态的精确触发条件

每个状态的边界必须明文化，避免未来实施时误判：

| 状态 | 进入条件 | 退出条件 | 期间 agent 在干什么 |
|---|---|---|---|
| `idle` | agent 启动后 / 上一任务 Terminating 完成 | 成功 ClaimTask 一个新任务 | 周期性调 `Store.QueryAvailable`，可能在 `time.Sleep(pollInterval)` 中 |
| `processing` | ClaimTask 成功 / WaitingApproval 收到 approved 或 rejected 信号 | 进入 WaitingApproval 或 Terminating | ReactLoop 内部所有动作——包括但不限于：LLM 调用、工具执行（非 needs-approval 类）、历史压缩、历史截断、mailbox drain、history 构造 |
| `waiting_approval` | **仅当**工具调用进入 `ApprovalCh` 阻塞等待时触发 | 收到 approved / rejected 信号（→ processing，rejected 作为工具错误注入 history）或超时 / 外部 cancel（→ terminating）| 阻塞在 `ApprovalCh` 上，不消耗 LLM token，不执行任何动作 |
| `terminating` | ReactLoop 退出（自然完成 / 超过 MaxLoops / panic recover）| TaskEnd hook 全部执行完，回到 idle | 调用 SubmitResult 或 FailTask、emit TaskEnd hook、清理 FileCache、写最终 trace 事件 |

#### 7.2.2 被显式排除的子动作（用 trace 事件而非状态枚举）

下列动作**不**作为独立状态，由专门的 trace 事件覆盖监控需求：

| 子动作 | 当前所属状态 | 监控手段 |
|---|---|---|
| `ClaimTask` 原子操作（毫秒级）| idle → processing 的转换边 | `KindTaskClaimed` trace 事件（已有）|
| 历史压缩（含期间的 LLM 调用）| **processing** | `KindHistoryCompaction` trace 事件（已有）|
| 历史截断 | **processing** | `KindHistoryTruncated` trace 事件（已有）|
| 主动 polling Store | **idle** | 无独立事件（idle 本身已表达）|

**关键决议（用户确认 2026-04-30）**：上下文压缩机制触发期间虽然调用 LLM，但**目前架构下仍计入 processing 状态**——压缩是 ReactLoop 主流程的内部子动作，不需要单独切换状态。reactor 想监控压缩用 `KindHistoryCompaction` 事件，而不是切到独立的 `compacting` 状态。

#### 7.2.3 状态转换图

```
                  ┌──────────────────────────────────┐
                  │                                  │
                  ▼                                  │
              ┌──────┐  ClaimTask    ┌────────────┐  │
       ┌─────▶│ idle │──────────────▶│ processing │  │
       │      └──────┘               └────────────┘  │
       │                              │     ▲        │
       │                              │     │        │
       │                              │     │ approved / rejected
       │                              ▼     │        │
       │                       ┌──────────────────┐  │
       │                       │ waiting_approval │  │
       │                       └──────────────────┘  │
       │                              │              │
       │                              │ timeout / cancel
       │                              │              │
       │  TaskEnd hook done           ▼              │
       │                       ┌────────────┐       │
       └───────────────────────│terminating │◀──────┘
                               └────────────┘
                              ReactLoop 退出
                              （自然完成 / MaxLoops
                                 / panic recover）
```

**6 条转换边**：
1. `idle → processing`（ClaimTask 成功）
2. `processing → waiting_approval`（needs-approval 工具调用阻塞）
3. `waiting_approval → processing`（收到 approved 或 rejected 信号——两者都让 agent 回到 ReactLoop，差别仅在工具调用结果是正常 string 还是 error）
4. `waiting_approval → terminating`（超时 / 外部 cancel，**不含** rejected——见 Q11.r 决议）
5. `processing → terminating`（ReactLoop 退出，含自然完成 / MaxLoops 耗尽 / panic recover 等所有路径）
6. `terminating → idle`（TaskEnd hook 执行完，回到等任务）

**Q11.r 配套改动（2026-04-30 决议）**：原设计 `waiting_approval → terminating` 包含 rejected，被 Q11.r 修正为"rejected 仅作为工具调用错误注入 history，agent 继续 ReactLoop（→ processing）"。这让 reject 与 path-boundary Abort、validate-line-anchors 失败等其他 Gate Abort 走完全一致的错误处理路径——agent 看到工具失败后由 system prompt 处理纪律决定是否重试、转向替代方案、或最终在任务报告中明确说明因果链后自然 ReactLoop 退出（`processing → terminating`）。

**显式排除**：
- 不存在 `processing → idle` 的直接边——所有任务结束必须经过 terminating（保证 cleanup hook 全跑）
- 不存在 `idle → terminating` 边——idle 时没有任务可终结
- 不存在状态自循环（如 processing → processing）——状态只在边界变化时切换

#### 7.2.4 Q10 的预承诺（状态切换归属）

承接 Q7 方案 A 的连锁影响：状态切换由 **Agent 主流程显式调用 SetState** 落地（v5 阶段简单优先，避免 state machine helper 的抽象层）。具体显式切换点位于：

- `Idle → Processing`：[agent.go ClaimTask 调用成功后的位置]
- `Processing → WaitingApproval`：进入 ApprovalCh 阻塞前
- `WaitingApproval → Processing`：从 ApprovalCh 接收到 approved 或 rejected 时（rejected 注入 history 后 agent 继续 ReactLoop）
- `WaitingApproval → Terminating`：从 ApprovalCh 接收到超时信号 / 外部 cancel 时（**不含** rejected）
- `Processing → Terminating`：ReactLoop 退出位（含 panic recover defer）
- `Terminating → Idle`：terminateTask / SubmitResult 完成、TaskEnd hook 跑完后

总计 **6 个显式 SetState 调用点**——这是 Q10 决议（详见 §7.3 待补）的具体落地形态。

#### 7.2.5 未来可能的扩展位（不进 v5）

| 状态 | 触发场景 | 扩展时机 |
|---|---|---|
| `Suspended` | agent 被外部命令暂停（v4 附录 D 唤醒优化）| 附录 D 触发条件满足时（agent 总数 ≥ 20）|
| `InTranslation` | spawn_agent + via_translator 的 ad-hoc agent 进入翻译态 | 实测发现需要单独标记翻译活动时 |

**被显式排除的候选（决议记录）**：

| 候选 | 排除理由（2026-04-30 决议）|
|---|---|
| `Ready`（OS 线程模型的"已就绪等调度"中间态）| AgentGo 走 goroutine 模型，goroutine 足够轻量，**当前架构下不存在 CPU 资源竞争场景**——没有中央调度器、没有跨 agent 的 LLM 配额管理、抢到任务即立即进入 ReactLoop。直接照搬 OS 5 状态模型是形式美而非功能价值。如果未来引入全局 LLM 配额调度器或中央 agent 调度器，再重新评估 Ready 的引入 |

#### 7.2.6 trace 事件配套（与 §6.4.1 对齐）

每次状态切换调用 `a.SetState(newState, cause)` 时，**同步 emit `KindAgentStateChanged` trace 事件**，payload 草案：

```
agent_state_changed: {
    agent_id, kind, prev_state, new_state, changed_at, cause
}
```

`cause` 字段示例：
- `"task_claimed:<task_id>"` （Idle → Processing）
- `"approval_required:<tool_name>"` （Processing → WaitingApproval）
- `"approved"` / `"rejected"` （WaitingApproval → Processing；两者都让 agent 回 ReactLoop，差别仅在工具结果是 string 还是 error）
- `"timeout"` / `"cancel:<source>"` （WaitingApproval → Terminating，仅这两种出口走 terminating）
- `"react_loop_exit:natural"` / `"react_loop_exit:max_loops"` / `"react_loop_exit:panic"` （Processing → Terminating）
- `"task_end_hook_done"` （Terminating → Idle）

具体字段定义在 Phase 2 TraceUpgrade spec 阶段细化。

### 7.3 SetState API 形态（已决议：方案 ii + panic + 自循环 no-op）

**Q10 决议（2026-04-30）**：状态切换由 Agent 类自身的方法 `SetState` 承担，内部封装"合法性校验 + 自动 emit trace + 状态字段更新"三件事。非法切换 panic，自循环（同状态切换）合法但视为 no-op。

#### 7.3.1 实施形态

`SetState` 是 `internal/agent.Agent` 的方法（不引入独立 `StateMachine` struct），约 30 行实现 + 一张 6 条边的转换表。

```go
// 调用形态（agent.go 主流程内 6 个显式位置）
a.mustSetState(AgentStateProcessing, "task_claimed:"+task.ID)
a.processTask(task)
a.mustSetState(AgentStateTerminating, "react_loop_exit:natural")

// SetState 实现（伪代码）
func (a *Agent) SetState(newState AgentRuntimeState, cause string) error {
    prev := a.runtimeState

    // 自循环：合法但 no-op（详见 §7.3.3）
    if prev == newState {
        return nil
    }

    // 非法切换：panic（详见 §7.3.2）
    if !isValidTransition(prev, newState) {
        return fmt.Errorf("illegal transition: %s → %s (cause: %s)", prev, newState, cause)
    }

    a.runtimeState = newState

    // 自动 emit（不允许调用方手动 emit，避免遗漏与重复）
    a.Store.EmitTrace(trace.Event{
        Kind:    trace.KindAgentStateChanged,
        AgentID: a.ID,
        Payload: map[string]any{
            "prev_state": prev,
            "new_state":  newState,
            "cause":      cause,
        },
    })
    return nil
}

// must-style 包装：所有调用点统一用这个
func (a *Agent) mustSetState(newState AgentRuntimeState, cause string) {
    if err := a.SetState(newState, cause); err != nil {
        panic(fmt.Sprintf("BUG: %v", err))
    }
}
```

#### 7.3.2 失败处理：panic + watchdog 兜底

非法状态切换调用 `panic`，由 watchdog 进行后处理。

**理由**：状态机错误是 100% 编程错误（不是用户输入错误、不是网络错误、不是 LLM 输出错误），属于 v4 §11 §S11 同档的"装配漏接"类问题——应当立即暴露而非软失败。立即 panic 让 bug 在测试期或灰度期就被发现，避免长期累积成隐性状态错乱。

**watchdog 后处理路径**（已在 v4 §S11 落地，复用现有机制）：
- panic recover 进入 `processTask` 的 defer
- 调用 `Store.FailTask` 标记任务失败（含 panic 堆栈作为 reason）
- emit `KindTaskFailed` trace 事件
- agent 实例终结，由 runner 决定是否重启

#### 7.3.3 自循环：合法但 no-op

**用户决议（2026-04-30）**：`SetState(Processing)` 在当前已是 `Processing` 时**视为合法切换**，但实质行为是 no-op——状态字段不变、不 emit trace 事件、直接返回 nil。

**理由**（用户原话）：
- Agent 在持续处理任务的时候就是一个自循环的工作
- 目前不允许在两个 ReAct 循环中插入动作

这两条原则共同指出：**ReactLoop 的多轮迭代在 Agent 视角下是连续的 Processing 状态**，不应被状态切换语义打断。如果未来需要表达"ReactLoop 第 N 轮开始"这类粒度更细的事件，应当通过专门的 trace event（如 `KindReactLoopIteration`）而非反复 emit `agent_state_changed: processing → processing`。

**实践预期**：6 个显式调用点中**不会出现自循环 SetState**（每个切换点的 prev/new 状态都不同）。自循环的合法性更多是"防御性宽容"——即使将来意外写出 `SetState(currentState)` 也不会 panic。

#### 7.3.4 6 个显式调用点（用户确认完整无遗漏）

| # | 调用点 | 切换 | cause 字段示例 |
|---|---|---|---|
| 1 | ClaimTask 成功后 | Idle → Processing | `"task_claimed:<task_id>"` |
| 2 | needs-approval 工具进入 ApprovalCh 阻塞前 | Processing → WaitingApproval | `"approval_required:<tool_name>"` |
| 3 | 从 ApprovalCh 接收 approved 或 rejected | WaitingApproval → Processing | `"approved"` / `"rejected"`（rejected 时 ApprovalCh 包装层把工具结果设为 error，agent 在下一轮 ReactLoop 看到该错误后由 system prompt 处理纪律决定后续动作）|
| 4 | 从 ApprovalCh 超时 / 外部 cancel | WaitingApproval → Terminating | `"timeout"` / `"cancel:<source>"`（**不含** rejected——见 §7.2.3 Q11.r 配套改动说明）|
| 5 | ReactLoop 退出位（含 panic recover defer）| Processing → Terminating | `"react_loop_exit:natural"` / `":max_loops"` / `":panic"` |
| 6 | TaskEnd hook 跑完 / SubmitResult 完成 | Terminating → Idle | `"task_end_hook_done"` |

#### 7.3.5 isValidTransition 转换表（6 条边）

```go
var validTransitions = map[AgentRuntimeState]map[AgentRuntimeState]bool{
    AgentStateIdle:             {AgentStateProcessing: true},
    AgentStateProcessing:       {AgentStateWaitingApproval: true, AgentStateTerminating: true},
    AgentStateWaitingApproval:  {AgentStateProcessing: true, AgentStateTerminating: true},
    AgentStateTerminating:      {AgentStateIdle: true},
}
```

注：自循环不进表（由 `prev == newState` 短路 no-op 判定承接，不走 isValidTransition 校验）。

#### 7.3.6 与 §6.4 / Phase 3 的连锁

- §6.4.1 `KindAgentStateChanged` 事件由 SetState 内部统一 emit，调用方零负担
- Phase 3 实施清单：6 个调用点穿插 + isValidTransition 表 + SetState/mustSetState 方法 + 单元测试覆盖 6 条合法边 + 至少 6 条非法边 panic 用例（含一条专门测试 `WaitingApproval → Processing` 在 rejected cause 下也合法的用例，避免后续重构误把 rejected 当成 terminating 触发器）
- Phase 2 TraceUpgrade 把 `KindAgentStateChanged` 加入事件 schema 时直接对齐 §7.2.6 的 payload 草案

### 7.4 Shell 工具的 ReactiveSystem 集成（详见 ToolUpgradePlan.md §2）

Shell 工具（`run_shell`）是 AgentGo 与现实世界交互的核心入口，是当前唯一会触发 `WaitingApproval` 状态的工具。其完整改造规格（命令名单格式、approval UI 4 选项、匹配语法、yaml 持久化、TimeoutHandler 抽象等）已迁移至 [ToolUpgradePlan.md §2](ToolUpgradePlan.md#2-shell-工具升级首批重点)。

**ReactiveSystem 视角的关键集成接口**（Q11/Q12/Q13/Q11.r 已决议 2026-04-30）：

| 集成点 | ReactiveSystem 侧语义 | 详细规格位置 |
|---|---|---|
| `shell.CommandFilter` 重构为 Gate | 与 path-boundary 同档的正式 Gate，Phase 1 命名空间清理一并完成 | [ToolUpgradePlan §2.3](ToolUpgradePlan.md#23-gate-决策流程) |
| `WaitingApproval` 触发条件 | needs-approval **不再是工具属性硬编码**，而是 Gate 计算结果（承接 §7.2.1 Q9 措辞精修）| [ToolUpgradePlan §2.3](ToolUpgradePlan.md#23-gate-决策流程) |
| Reject 作为工具调用错误返还 | 与 path-boundary Abort、validate-line-anchors 失败同档，走 v4 §7/§10 现有错误处理路径；agent 通过 system prompt 处理纪律自适应 | [ToolUpgradePlan §2.4 / §2.5](ToolUpgradePlan.md#24-4-选项-approval-ui-与-agent-通知) |
| `KindShellExecuted` 事件 | 进 §6.4.1 首批清单（#7），但**仅内置 Reactor 可订阅**，用户 YAML schema 暂不开放 | [ToolUpgradePlan §2.9](ToolUpgradePlan.md#29-kindshellexecuted-事件的开放策略) |
| `TimeoutHandler` 抽象（第三类决策点）| 与 Gate / Reactor 并列的"动作进行中决策点"——Gate 决定开跑前、Reactor 响应已发生、TimeoutHandler 决定动作进行中。v5 仅内置 `truncate` handler，接口层 + YAML 占位 schema 一开始就立住 | [ToolUpgradePlan §2.8](ToolUpgradePlan.md#28-shell-超时机制) |
| `KindShellTimeoutPending` / `KindShellTimeoutResolved` 事件 | 进 §6.4.1 首批清单（#8 / #9），仅内置 Reactor 可订阅；事件订阅与 handler 决策解耦 —— 内置 Reactor 可监听做日志/metric/告警，不必走 TimeoutHandler 链 | [ToolUpgradePlan §2.8.5](ToolUpgradePlan.md#285-trace-事件配套) |
| Worker / Explorer prompt 模板更新 | 追加"工具权限处理纪律"段落，与 Gate 重构同 PR 上线避免中间态 | [ToolUpgradePlan §2.5](ToolUpgradePlan.md#25-system-prompt-处理纪律q11r-配套) |

**为什么拆分**：§7.4 的大部分内容（YAML 文件格式、4 选项 UI 文案、shell-aware tokenization 规则、yaml.v3 Node API 持久化、SIGKILL truncate 机制）属于 Shell 工具自身的实施细节，与 ReactiveSystem 抽象本身解耦——它们只是 Gate 的一个具体消费者。把这部分搬到 ToolUpgradePlan.md，让本文档专注于"四类角色抽象 + 状态机 + 事件流"的核心命题。

---

## 8. 不在本模块范围

- **重写现有 Gate（拦截器）逻辑**：本模块只做归类、迁移、重命名，不改任何 Gate 的判定逻辑（path-boundary 的边界规则、validate-line-anchors 的算法等都保持原状）
- **Reactor 的 LLM 化决策**：用户配置 Reactor 的"动作"必须是声明式的（调工具 / publish 任务），**不允许 Reactor 内部直接跑 LLM**——LLM 调用必须由 Agent 处理任务时承担，否则成本不可控
- **任务级权限模板（v4 附录 A）**：本模块不涉及，附录 A 触发条件未满足
- **Reactor / Gate 的运行时热重载**：v5 阶段配置依然启动期固定，重启生效。热重载留作未来扩展点
- **跨进程 Reactor（如调用外部 webhook）**：v5 首版仅支持进程内动作（调内置工具 / publish 任务）。webhook / gRPC / 外部消息队列等进程外交互留作未来
- **Reactor 之间的依赖与编排**：v5 首版每个 Reactor 独立执行，不支持 "Reactor A 完成后触发 Reactor B" 这种链式依赖。需要时改用 publish_task → 新任务上挂新 Reactor 的方式表达
- **Shell 命令名单 / approval UI / 持久化 / 超时机制等具体实施细节**：迁移至 [ToolUpgradePlan.md §2](ToolUpgradePlan.md#2-shell-工具升级首批重点)，本模块仅保留 §7.4 集成接口表
- **WaitingApproval 的"持续时长触发"**：reactor 不支持"状态持续超过 N 分钟自动触发"这种时间维度——这是个独立模块（需要定时器调度 / 状态时长追踪），留作 v5.x。当前用 watchdog 兜底超时取消（用户长时间不批准是用户自己的选择）

---

## 9. 待讨论清单（按优先级排序）

进入 spec 定稿之前，需要逐项对齐：

| # | 议题 | 候选 | 倾向 |
|---|---|---|---|
| Q1 | 是否同意按 ReactiveSystem 2 类核心角色（Reactor / Gate）拆 2 个独立 Registry？（**2026-04-30 两轮修订**：原 4 类→3 类→2 类。Provider 抽象废弃由 MemoryManageSystem.md 承接；Aggregator 下放为 Mailbox 子系统内部固定机制，不立顶层）| 拆两个 / 统一加 phase | ✅ **已拍板**（拆两个）|
| Q1' | GateRegistry 是否真正跨域统一（合并 v4 时代 ToolHookRegistry / MailboxHookRegistry）？| 名义统一 / 真正统一（接口式 Context）/ 强行 fat struct | ✅ **已拍板**：真正统一为单一 `internal/gate/` 包，统一 Phase 枚举 + 接口式 Context（`gate.Context`），具体类型 `ToolContext` / `MailboxContext` 实现接口；详见 [§4.4](#44-gateregistry-的统一形态接口式-context) |
| Q4' | ReactorRegistry 完整接口形态（与 §4.4 GateRegistry 对称）| 仅契约（接口 + 类型）/ 含实现细节 | ✅ **已拍板**：仅给接口与类型签名，具体实现（panic recover 形态 / channel buffer / goroutine 池）留待 Phase 4 实施时定。`Reactor` 接口含 `Subscribe / Run / IsSync / Priority`；`Dispatch` 不返回值（失败被吞）；详见 [§6.6](#66-reactorregistry-完整接口形态与-44-gateregistry-对称) |
| Q3' | ReadSet 数据结构与触发事件（Phase 6 落地形态初版草案）| 不预先定 / 现在补初版 / 等 Phase 6 启动再定 | ✅ **已拍板**：现在补初版 spec。`Task.ReadSet map[string]ReadInfo` + 复用 `KindToolResult` 事件不新增 EventKind + Gate 通过 `gate.Context.StoreView()` 查询。Phase 6 启动时基于 Phase 4/5 实施经验可微调；详见 [§5.2.1](#521-readset-数据结构与触发事件phase-6-落地形态--初版草案) |
| Q2 | ReactiveSystem 是否独立成 v5 主章节，与 Scheduler 汇报分层（v5.md）平行？ | 独立 / 作为子节 | 独立 |
| Q3 | `record-artifact` 与 `require-read-before-write` 的"已读集合"是否进入 v5 must-have？ | 进 / 不进 | 进（试金石）|
| Q4 | Reactor 动作语言走 §6.1 哪个组合？ | B + D（D 拆为 3 动词 + via_translator 修饰符 + when 条件）| ✅ **已拍板**（2026-04-29，详见 §6.1.1-6.1.7）|
| Q5 | Reactor 配置粒度走 §6.2 哪种？ | a + b 起步（c 延后到 v5.x）；多粒度叠加触发；ad-hoc agent 继承 base_kind reactor + spawn 防爆炸阈值待 spec 阶段定 | ✅ **已拍板**（2026-04-29，详见 §6.2.1-6.2.6）|
| Q6 | Reactor 执行模型走 §6.3 哪种？ | i 同步 / ii 异步无回执 / iii 异步带回执 | iii |
| Q7 | v5 首批支持的触发事件是 §6.4 哪几个？ | 5 + 1（task_claimed/completed/failed/cancelled/file_written + agent_state_changed 配套）；方案 A 同开 Q9+Q10+Phase 3 | ✅ **已拍板**（2026-04-30，详见 §6.4.1-6.4.6）|
| Q8 | TraceUpgrade（事件 payload 结构化）是否提前到本模块作为 §S0？ | 提前 / 分开走 | 提前 |
| Q9 | Agent 实例状态枚举是否就是 §7.2 那 8 个？是否漏了什么？ | 4 个核心状态（idle / processing / waiting_approval / terminating）；waiting_approval 仅在 needs-approval 工具调用时触发；压缩期间仍属 processing | ✅ **已拍板**（2026-04-30，详见 §7.2.1-7.2.6）|
| Q10 | 状态切换由 Agent 主流程显式调用 vs state machine helper 自动管理？ | ii 显式封装（Agent.SetState 方法）+ 非法切换 panic 由 watchdog 兜底 + 自循环合法但 no-op + 6 调用点确认完整 | ✅ **已拍板**（2026-04-30，详见 §7.3.1-7.3.6）|
| Q11 | Shell 命令名单结构 + Gate 重构 + approval UI + 匹配语法 + 持久化 | 单一文件 shell_commands.yaml（扁平 blacklist/whitelist）+ shell.CommandFilter 重构为 Gate + 4 选项 approval（Once/All）+ Gate 动态计算 needs_approval + 整体匹配前缀通配 + 启动期模板自动生成 + 直接写回 yaml.v3 Node | ✅ **已拍板**（2026-04-30，详见 [ToolUpgradePlan §2.1-§2.7](ToolUpgradePlan.md#2-shell-工具升级首批重点) + §7.4 集成接口表）|
| Q11.r | reject 后 agent 的处理机制（多 agent 场景下用户不便对单个 agent 追加指令）| reject 作为工具调用错误返还（与 path-boundary Abort 同档）+ system prompt 补充处理纪律（侵入性更小的替代 / 任务失败时显式说明因果链）→ 用户通过任务报告事后追溯，无需 approval 时输入 reason | ✅ **已拍板**（2026-04-30，详见 [ToolUpgradePlan §2.4 / §2.5](ToolUpgradePlan.md#24-4-选项-approval-ui-与-agent-通知)）|
| Q12 | KindShellExecuted 是否进 v5 首批事件？ | 进首批，但仅内置 Reactor 可订阅，用户 YAML schema 暂不开放（占位预留）| ✅ **已拍板**（2026-04-30，详见 [ToolUpgradePlan §2.9](ToolUpgradePlan.md#29-kindshellexecuted-事件的开放策略)）|
| Q13 | Shell 超时机制 | 用户配置 timeout 阈值（单位秒）即容忍上限；超时拆为「事件 + TimeoutHandler」二段式抽象（emit `KindShellTimeoutPending` → handler 决策 → emit `KindShellTimeoutResolved`）；v5 仅内置 `truncate` handler（行为等同原 `truncate` 单档：SIGKILL + 部分输出 + 错误返还）；接口层 + YAML 占位 schema + 三档 decision 枚举（truncate / wait / continue）一开始就立住，未来 `wait_then_truncate` / `consult_llm` / `message_agent` / `escalate_to_user` 等 handler 可增量加而无需重构 | ✅ **已拍板**（2026-04-30，详见 [ToolUpgradePlan §2.8](ToolUpgradePlan.md#28-shell-超时机制)）|

每个 Q 答完，对应章节就可以从"待讨论"转为"已定稿"。

---

## 10. 后续计划与实施顺序

### 10.1 依赖关系总览

按原则 3，**TraceUpgrade（事件 payload 结构化）是 ReactiveSystem 的硬性前置**——没有结构化事件，Reactor 系统站不起来。但二者并非全程串行，部分工作可以并行推进。

```
┌─ Phase 1: 命名空间清理 ────────────────────────┐
│  新建 internal/gate/ 统一包                     │
│   - 统一 Phase 枚举（tool: / mailbox: 前缀）   │  ← §4.4 接口式 Context
│   - 接口 Context + ToolContext / MailboxContext │     落地点
│   - 统一 GateRegistry + Dispatch              │
│  9 个拦截 Hook → Gate（接入新接口）            │
│  team-awareness → 删除（迁 MemoryManageSystem）│  ← 独立、纯重构、零依赖
│  wake-context-expand → 留在 Mailbox 子系统内部 │     可与 Phase 2 并行
│  删除旧 internal/hook/tool.go / mailbox.go     │
└────────────────────────────────────────────────┘
        │
        │ (无依赖)
        │
┌─ Phase 2: TraceUpgrade（硬前置）───────────────┐
│  事件 payload 结构化（From/To/Cause/Actor）    │
│  CLI 工具适配新 schema                         │  ← Reactor 的事件源
│  端到端：trace 文件可重放完整状态序列          │     必须先于 Phase 3-6
└────────────────────────────────────────────────┘
        │
        ▼
┌─ Phase 3: Agent 状态显式化（与 Phase 4 配套）─┐
│  引入 AgentRuntimeState 枚举                   │
│  显式状态切换点 + emit KindAgentStateChanged   │  ← 与 Phase 4 一起设计
│  状态切换由主流程显式驱动（原则 1）             │     一起验证
└────────────────────────────────────────────────┘
        │
        ▼
┌─ Phase 4: Reactor 单元引入（与 Phase 3 配套）─┐
│  新增 ReactorRegistry                          │
│  内置 Reactor 接管 §5.1 清单的散落副作用       │  ← 配套上线
│  移植 record-artifact 作为示范                 │     验证抽象正确性
└────────────────────────────────────────────────┘
        │
        ▼
┌─ Phase 5: 用户 YAML schema 落地（S1-S7 子序列）┐
│  S1 公共基础设施（prompt 加载 + 模板替换 + when）│
│  S2 publish_task（复用现有 store.PublishTask） │
│  S3 reactor 自带独立 LLM client（原则 5）      │  ← Q4 决议三动词全做
│  S4 invoke_llm + output dispatcher（依赖 S3）  │     S1-S4 是最小可发布集
│  S5 ad-hoc agent 生命周期机制                   │
│  S6 spawn_agent（依赖 S5）                     │
│  S7 spawn_agent + via_translator（依赖 S3+S6） │
└────────────────────────────────────────────────┘
        │
        ▼
┌─ Phase 6: ReadSet 显式化（验收用例）──────────┐
│  Task.ReadSet 字段引入                         │
│  read-set-write Reactor 在 read_file 后写入    │  ← 反模式治理 +
│  require-read-before-write Gate 改读 ReadSet   │     端到端验收
└────────────────────────────────────────────────┘
        │
        ▼
┌─ Phase 7: 端到端烟测与文档同步 ────────────────┐
│  Shipping conventions 全套                      │
│  KNOWN_ISSUES.md / Architecture.md 同步         │
└────────────────────────────────────────────────┘
```

### 10.2 推荐执行顺序

**串行强制依赖**：Phase 2 → Phase 3+4 → Phase 5 → Phase 6 → Phase 7
**可并行**：Phase 1 与 Phase 2 完全无依赖，可同时开工
**配套设计**：Phase 3 与 Phase 4 是配套设计（§7 与 §6 的关系），同 PR 上线最佳

理想节奏（不假设具体时间表，只给依赖逻辑）：

| 阶段 | 工作 | 前置 | 是否阻塞下游 |
|---|---|---|---|
| 1 | 命名空间清理 | 无 | 不阻塞（独立重构）|
| 2 | TraceUpgrade | 无 | **阻塞 Phase 3+4+5+6** |
| 3+4 | 状态显式化 + Reactor 引入（配套）| Phase 2 | 阻塞 Phase 5+6 |
| 5 | 用户 YAML schema | Phase 3+4 | 阻塞 Phase 6 验收路径 |
| 6 | ReadSet 验收 | Phase 5 | 不阻塞 |
| 7 | 烟测文档同步 | 全部 | — |

### 10.3 各 Phase 内部细化

每个 Phase 内部按 v4.md 的 S1-SN 风格细化为可独立提交的步骤清单。Q1-Q10 已于 2026-04-30 全部拍板，下一步进入各 Phase 的 spec 细化阶段。Phase 5 内部 S1-S7 子序列已在 §6.1.5 落地。

### 10.4 与 TraceUpgrade.md 的协同

`TraceUpgrade.md` 当前是空文件占位。本模块筹备期间已确认：**TraceUpgrade 不是独立模块，而是 ReactiveSystem 的 Phase 2**。

**归属决议（2026-04-29 已定）**：未来 Phase 2 启动时，由 `TraceUpgrade.md` 承接 Phase 2 的详细 spec（事件 payload 字段定义、CLI 适配清单、迁移策略）。保留独立文件便于读者按主题查找，符合 `docs/activate/` 现有组织风格（ToolUpgradePlan / TraceUpgrade 都是按主题切的）。

**当前阶段不做的事**：
- **不立刻动 `TraceUpgrade.md`**——Q1-Q10 都未拍板，Phase 2 的边界都未锁定，此刻往里填内容只能是猜测，必将返工
- TraceUpgrade.md 保持空占位状态，是当前正确状态
- Phase 2 真要启动时（即 Q8 拍板 + 进入实施阶段时）再统一填充

本节仅确认未来归属，不触发当前文档清理动作。

---

## 11. 与 v4 / v5 现有规划的关系

| 模块 | 关系 |
|---|---|
| v4 §7 Hashline | ValidateLineAnchorsHook 是本模块归类为 Gate 的对象，无功能影响 |
| v4 §10 Did-You-Mean | 与 ReactiveSystem 独立，本模块不涉及 |
| v4 §11 v4 配置 | 本模块依赖 v4 已落地的 kind 体系；新增 `reactors:` YAML 块作为顶层平行字段 |
| v5.md（Scheduler 汇报）| 平行模块，互不干涉。两者落地顺序由项目方决定 |
| TraceUpgrade.md（空）| **硬性前置**（按原则 3）。本模块 Phase 2 = TraceUpgrade 的具体落地工作。两种处置见 §10.4：A. TraceUpgrade.md 改写为 Phase 2 详细 spec（推荐）｜ B. 删除并全部并入本文。决议待 §10.4 拍板 |
| [ToolUpgradePlan.md](ToolUpgradePlan.md) | **配套文档**。原 §7.4 Shell 工具的完整改造规格已迁移至 ToolUpgradePlan.md §2，本模块 §7.4 仅保留集成接口表。Shell 工具是 ReactiveSystem 第一个具体的 Gate 消费者，与 Phase 1 命名空间清理同档落地 |
| [MemoryManageSystem.md](MemoryManageSystem.md) | **配套文档**。2026-04-30 两轮修订让本模块从"4 类角色（Reactor / Gate / Provider / Aggregator）"缩为"2 类核心角色（Reactor / Gate）"——Provider 抽象废弃由 MemoryManageSystem 承接 team-awareness 类需求；Aggregator 下放为 Mailbox 子系统内部固定机制不立顶层。Phase 1 命名空间清理时与 MemoryManageSystem MM6 / MM7 配套上线 |

---

## 附录 A. 现有 Hook 完整代码引用索引

为便于后续 Phase 1 重构定位代码：

| Hook | 文件路径 | v5 角色 |
|---|---|---|
| path-boundary | `internal/hook/builtin/path_boundary.go` | Gate |
| validate-expected-hash | `internal/hook/builtin/validate_expected_hash.go` | Gate |
| dependency-validator | `internal/hook/builtin/dependency_validator.go` | Gate |
| validate-line-anchors | `internal/hook/builtin/validate_line_anchors.go` | Gate |
| require-read-before-write | `internal/hook/builtin/require_read_before_write.go` | Gate（状态来源待治理）|
| enforce-expected-artifacts | `internal/hook/builtin/enforce_expected_artifacts.go` | Gate |
| record-artifact | `internal/hook/builtin/record_artifact.go` | **Reactor**（首个内置）|
| chain-depth-limit | `internal/hook/builtin/chain_depth_limit.go` | Gate |
| per-agent-dedup | `internal/hook/builtin/per_agent_dedup.go` | Gate |
| wake-worthy-filter | `internal/hook/builtin/wake_worthy_filter.go` | Gate |
| wake-context-expand | `internal/hook/builtin/wake_context_expand.go` → 迁至 `internal/mailbox/` | Mailbox 内部固定聚合机制（非顶层抽象）|
| team-awareness-task-start | `internal/hook/builtin/team_awareness_task_start.go` | 🗑 v5 删除（迁 MemoryManageSystem.md）|
| team-awareness-loop-pre | `internal/hook/builtin/team_awareness_loop_pre.go` | 🗑 v5 删除（迁 MemoryManageSystem.md）|

注册位置：`internal/bootstrap/bootstrap.go` lines 182-291（Tool / Mailbox / Agent 三段）。
