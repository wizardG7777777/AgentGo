---
name: scheduler-task-dispatch
description: Guide AgentGo Scheduler agents to apply Graph-first request routing, choose live agent routes, build durable GraphDocument execution contracts, provision capabilities, and respect Interaction and Graph ownership boundaries.
---

# Scheduler Task Dispatch Skill

> 将 Scheduler 的调度知识作为显式、可评审的执行指令使用。

> **2026-08-20 机制搁置提示**：`agent_templates.enabled` 已缺省为 `false`，
> `list_agent_templates` / `provision_agent_team` 默认不注册，Scheduler 提示词
> 的组队教程已移除（v7.6 静态路由纪律）。本文中全部 provision/模板组队内容
> 仅在显式重新开启机制后适用；当前图节点只能路由 `agents:` 声明的静态 kind。

## 1. 核心职责

Scheduler 是 AgentGo 系统中唯一拥有完整工具能力的一等代理。其核心职责：

1. **观察全局状态**：解析 Board Snapshot（tasks、resources、agents、agent_capabilities）
2. **Graph-first 决策**：所有用户请求都先表达为 Graph；简单请求退化为单个 controller 节点到 `end`，复杂请求再按真实数据依赖扩展节点、分支、barrier 与回边
3. **编排节点与路由**：用 GraphDocument 表达节点、转移、验收与回边，再把 Agent 作为匹配 capability 的节点执行资源

### 1.1 用户 Interaction 边界

- `gate=plan` 与 plan review 通道已于 V6 移除：`submit_plan_for_review` 等 Plan 专属工具不复存在，执行前审阅改由 Graph approval 节点承担（用户经 `graph_approval` Interaction 批准/拒绝）。
- MUST NOT 从普通用户文本猜测用户的批准/拒绝决定，也 MUST NOT 代表用户选择。
- Graph Runtime 是图、activation 与节点终态的执行事实源；Interaction 只拥有用户选择。等待控制面完成 CAS 与领域 effect 时，`waiting_interaction` 是正常状态，不得通过重试、重新派发或另一路径绕过。
- Shell 灰名单决定属于精确绑定原 command、pattern、working directory、AgentID 和 TaskID 的 `shell_command` Interaction。Scheduler 不得把普通聊天回复解释为 Shell 授权。
- 当任务确实需要用户澄清或在普通方案间选择时，可调用 `request_user_input(prompt, options_json)`。`options_json` 必须含 2–8 个稳定选项，每项仅使用 `id`、`label`、可选 `description` / `requires_text`；工具只返回 `request_id`、`option_id` 与 `text`。问题应具体且让各选项互斥，不要把可以从仓库或 Board Snapshot 查证的事实抛回给用户。
- `request_user_input` 固定产生 `Purpose=agent_question`，不能提供 `ActionRef`、Resolution 或 Metadata，也不得用它伪造 Shell authorization、`graph_approval` 或其他特权 effect。这些领域路径仍必须调用各自控制面工具并等待受信任 handler。
- 不要假设前端显示序号或键位。TUI/Web 都按稳定 Option ID 回答；`/plan` 兼容入口已随 plan review 通道一并移除，任何回答都必须进入同一 Interaction 管线。
- pending Interaction 在当前进程内统一展示，`SessionID` 仅记录创建时的审计归属；切换 `/session` 不会隐藏、回答或取消仍在等待的请求。

---

## 2. 感知层：Board Snapshot 解读

每轮唤醒时注入的 JSON 快照是本 Skill 运行的全部输入。解读顺序：

### 2.1 渐进式披露映射

Anthropic Agent Skills 定义了三级加载模型，AgentGo 的 Board Snapshot 自然地实现了这一映射：

| Anthropic Skill 层级 | AgentGo 对应物 | 何时加载 |
|----------------------|---------------|---------|
| **L1 发现层**（Discovery） | `agent_capabilities` + `specialized_agents`（name + description） | 每次 wakeup 注入 |
| **L2 激活层**（Activation） | Agent 的 `system_prompt_file` 全文 | Agent 领取匹配的 Task 时 |
| **L3 执行层**（Execution） | `scripts/` + `references/` 目录（未来能力） | Agent 按需 read_file 加载 |

**Scheduler 的核心工作**：在 L1 阶段利用元数据做出路由决策——无需展开 L2/L3。

### 2.2 第一步：扫 resources

```yaml
resources:
  worker_count / busy_workers / available_workers  # 容量判断
  agents:  # 逐 agent 读: id, type, mailbox_pending, current_task, locked_files
  agent_capabilities:  # 每种 type 的 capabilities 标签数组
  unavailable_tools:  # 不可用工具（决定能否派发网络相关任务）
  specialized_agents:  # 特化代理列表（event_type, count, busy, role）
  agent_templates:  # 可 provision 的模板蓝图（ref/digest/capabilities）
```

### 2.3 第二步：扫 tasks

看公告板上所有任务的状态：processing / pending / completed / failed。
重点关注 `artifacts` 字段（实际写入的文件清单）和 `dependencies` 字段。

### 2.4 第三步：扫 session_history

```yaml
session_history:
  - text: "用户原话"
    scheduler_task_id: "uuid"
    outcome: completed | failed | processing | pending
```

用户说"继续"时从这里找上一个任务 ID。

---

## 3. 决策层：Graph-first

收到新用户输入后统一走 Graph，不再在 Direct 与 Graph 两条路径之间做前置选择：

- 闲聊、快照状态或一次原子只读查询也要提交 Graph；最小形态是单个 controller 节点完成工作后转到 `end`。
- 多步调查、Shell、写入、验证、外部研究、并行、条件分支、回边、审批或等待，按真实数据依赖扩展 Graph。
- **只读不等于简单**：跨文件归纳、仓库审计、多来源调查都应建图，只是节点 capability 保持只读。
- 建图前只允许为决定图形做一次轻量探测，MUST NOT 先做完主体工作再补装饰性 Graph。
- 新用户请求 MUST NOT 用多个 `publish_task` 手工拼 DAG；`publish_task + report_done` 只保留为 legacy/恢复兼容路径。

---

## 4. 路由层：Event Type 选择逻辑

当 Graph 中的 agent/acceptance 节点需要执行 route 时，按以下决策链选择 `metadata.route`；legacy Task 的 `event_type` 使用同一套边界：

### 4.1 三步问自己

1. **纯只读调查？**（读文件、搜代码、查网页、核验事实——全程不写任何东西）
   - 是 → 查看 `specialized_agents` 中有没有能胜任的类型，发布为该 event_type
   - 不是 → 走默认 `event_type=""`（Worker）

2. **必须落盘？**（expected_artifacts 非空？description 要求写文件？）
   - 有 → **MUST** 路由到具备 write_file/edit_file 能力的 Agent。如果只有 Worker 能写盘，**MUST** 使用 Worker。即使前半段是调查，正确做法是拆成 explore + worker 两步
   - 没有 → 参考第 1 条

3. **需要执行 shell 命令？**（跑测试、编译、curl、git 操作）
   - 需要 → **MUST** 路由到具备 run_shell 能力的 Agent
   - 不需要 → 参考第 1 条

### 4.2 基于 Capabilities 的精确路由

参考 `resources.agent_capabilities` 中每种类型的真实工具名做精准匹配：

| 需要的能力 | 筛选条件 |
|-----------|---------|
| `run_shell` | agent_capabilities 包含 "run_shell" |
| `write_file` / `edit_file` | agent_capabilities 包含 "write_file" 和 "edit_file" |
| `web_search` + `web_fetch`（只读） | agent_capabilities 包含两者；优先用 Explorer |
| `submit_task_result.verdict`（Graph 验收，枚举 `pass/fixable/failed`） | MUST 路由到 acceptance.verify（或包含 submit_task_result 的验收 Agent） |

### 4.3 硬性约束

- **MUST** 只发布到系统中**实际存在**的代理类型（检查 `specialized_agents` 列表）
- **MUST NOT** 向不存在该 event_type 的队列发布——直接向用户说明无法完成的原因
- **SHOULD** 当 `specialized_agents` 中 busy 等于 count 时，优先用另一个已存在且能力足够的 route；或 provision 新 Team
- **MUST NOT** 给不具备 write_file/edit_file 的 route 声明 expected_artifacts

### 4.4 Graph-scoped 动态 Team（铁律）

Graph-first 请求需要动态能力时：

1. Scheduler **MUST 先决定合法且全局唯一的 `graph_id`**；
2. 调 `provision_agent_team` 时 **MUST 显式传同一个 `graph_id`**；
3. 等下一轮读取返回的真实 `event_type`，再把它写入该 Graph 节点的 `metadata.route` 并 `submit_graph`；同一响应中 MUST NOT 猜 route；
4. Team 从 provision 成功起归属 `graph:<graph_id>`。发起 provision 的 Scheduler task 终态不会回收它；只有 `graph_ended` 才停止实例并撤销 route；
5. `submit_graph` / `patch_graph` 对产任务节点的 route owner scope 与 capability 做 fail-closed 校验。跨 Graph、legacy task-owned 或工具不足的 route 必须先修正，不能依赖 Watchdog 事后发现；
6. 省略 `graph_id` 只用于 legacy `publish_task`；图内 controller 可省略以继承当前 Graph，但显式值不得指向另一个 Graph。

相同 Graph 下相同 template ref、purpose 和副本数是幂等 provision；这允许多个 controller activation 复用同一 ready Team，而不会重复扩容。

内联 subgraph 的运行时 `graph_id` 由父节点 activation 派生，不继承父 Graph 的私有 Team scope。内联子图只能使用全局静态 route；需要动态 Team 时，把节点留在父图，或拆成先有明确 `graph_id`、再按该 ID provision 的独立 Graph。

---

## 5. 配置表生成层：AgentKind 定义

当需要为系统生成或修改 Agent 配置时，使用以下 YAML Schema：

### 5.1 AgentKind 完整字段

```yaml
agents:
  - kind: "worker"                    # 必填，唯一标识
    replicas: 3                       # 必填，≥1
    description: "通用工作代理..."     # 推荐：拼入 board snapshot 供 Scheduler 决策
    event_type: ""                    # "" = 领取默认队列；"explore" = 仅领探索任务
    profile: "worker_standard"        # 与 tools 互斥，引用 tool_profiles 名称
    # tools: [...]                   # 与 profile 互斥，内联工具列表
    model: "deepseek-chat"           # 可选，覆盖全局 LLM
    system_prompt_file: "prompts/worker.md"  # 必填

    # === v2.0.0 新增字段（全部 optional，向后兼容）===
    skill_id: "worker-v1"            # 唯一技能标识，用于路由表索引
    skill_version: "1.0.0"           # 语义化版本号
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    context_limit: 16000
```

### 5.2 生成规则

1. `kind` MUST 唯一，不能重复
2. `profile` 和 `tools` MUST 互斥，二选一
3. `system_prompt_file` 路径 MUST 可读
4. 所有数值型参数 MUST > 0
5. 每个 kind MUST 至少 1 个 replica（InstanceID = `<kind>-<replicaIdx>`）
6. `skill_version` SHOULD 遵循语义化版本号（Major.Minor.Patch）

### 5.3 典型配置骨架

```yaml
# 双代理最小配置
agents:
  - kind: worker
    replicas: 3
    profile: full-access
    system_prompt_file: prompts/worker.md
  - kind: explorer
    replicas: 1
    event_type: explore
    profile: read-only
    system_prompt_file: prompts/explorer.md
```

---

## 6. Reactor 层：事件绑定策略

### 6.1 Reactor 触发模式

AgentGo 的 Reactor 系统订阅事件并在条件满足时自动触发下游任务。Scheduler 需要理解以下事件-反应映射：

| 事件 | 典型 Reactor 反应 | Scheduler 注意事项 |
|------|------------------|-------------------|
| `KindTaskCompleted` + explorer 结果 | 自动创建 Worker 任务将 Explorer 文本转化为文件 | 无需手动串联——但需要**在 board snapshot 中识别 pending_downstream_tasks** |
| `KindTaskCompleted` + worker 结果 | Verifier 审核 / 自动重试 | 等待 pending_downstream_tasks 清空后才汇报完成 |
| `KindFileWritten` | 触发后续流程（如 config change → reload） | 文件产出后不要立即假设系统已感知 |
| `KindTaskFailed` | 自动重试（最多 `task_max_retries` 次） | 超过重试上限后 Scheduler 需介入 |

### 6.2 进度汇报纪律

当 board snapshot 中存在 `pending_downstream_tasks` 时：

- ✅ 调用 `report_progress(summary="...")` 向用户说明进度
- ❌ **MUST NOT** 调用 `report_done`（会误导用户以为全部完成）

当 `pending_downstream_tasks` 为空时：

- ✅ 调用 `report_done` 或直接自然语言回答
- ❌ **MUST NOT** 调用 `report_progress`（显得啰嗦）

### 6.3 Reactor 与 Scheduler 的分工

| 职责 | Scheduler | Reactor |
|------|-----------|---------|
| 任务拆解与初始派发 | ✅ | ❌ |
| Explorer→Worker 结果转化 | ❌ | ✅（自动） |
| Verifier 审核触发 | ❌ | ✅（自动） |
| 失败重试 | ❌ | ✅（自动） |
| 进度汇报 | ✅ | ❌ |
| 超限干预（重试超过上限） | ✅ | ❌ |

---

## 7. Graph 依赖与 legacy 兼容

新请求的依赖用 Graph `next` 转移表达；条件分流用 `when`，返工用 `activation:"new"` 回边。不需要为 Graph 节点手工传 Task UUID。

- 当前 Runtime 尚无 flow generation / correlation token，因此 authoring 采用单赋值安全基线：除 `join` / `acceptance` 外，普通节点的静态入边数 MUST `<= 1`；条件分支各自拥有后续节点和 `end`，MUST NOT 先合入 OR mux。
- `join` / `acceptance` 是 barrier：`task.required_inputs` 声明必须齐备的端口名，入边用 `target_input` 写入；每个 `target_input` MUST 恰有一条生产边。并行 AND 的每个必需来源 MUST 使用不同端口，MUST NOT 让互斥分支共享端口。
- 成功汇合后，`join` 只发出 `completed`；规范形态是多个生产节点分别写入独立端口，再由 `join --completed--> summarize`。
- 循环体可直接声明为 root，由 Runtime 隐式产生初始 activation；该节点只允许一条 `activation:"new"` 回边作为后续 activation 来源，MUST NOT 另造 start 与回边汇入同一普通节点。
- controller 节点虽然路由到 `__scheduler__`，仍 MUST 用 `submit_task_result` 结算本节点。普通 agent/controller 的业务事件仍可供其自身路由；自定义 `$.path` 路由字段 MUST 放入 `result` object（如 `result={"coverage":"gap"}`），MUST NOT 只写在 summary/event。`report_done` 只属于非图 Scheduler 任务，Graph controller 调用会被硬拒绝。
- acceptance 节点的 `task.title` 与 `task.description` MUST 非空，description 必须明确逐项验收标准。其 completed 结果 MUST 省略 `event`，只提交 `verdict=pass|fixable|failed`；业务出边只能用 `$.verdict eq` 精确匹配这三个值。
- acceptance MUST NOT 使用无条件、`always`、`completed`、`pass` 或 `fixable` 事件作为业务出边；Runtime 自身 `failed` / `blocked` 事件只用于兜底。无法验收时提交 `status=blocked` 与 `blocked_reason`；`disputed` 是 Runtime 状态，不是 verifier 可提交的 verdict。
- `when` 缺省是**无条件**，blocked/failed 到达时也可能选中。普通 agent/controller 的成功边须按其输出契约显式匹配；join 成功出边 MUST 匹配 `completed`；为 Runtime `blocked` / `failed` 单独设计失败、重试或 replan 路径。
- 当前 MUST NOT 构造共享端口 OR、条件分支后的复杂汇流、嵌套图或复杂回环；这些形态等 generation/correlation token 能区分数据代际后再开放。
- MUST NOT 把多条边直接指向普通 summarize 并假设它会等齐；MUST NOT 用无条件边把 blocked/failed 送入成功汇总。

以下 UUID 依赖规则只适用于已有 legacy batch 的 `publish_task` 兼容路径。

### 7.1 legacy 发布顺序规则（铁律）

当任务 B 依赖任务 A 的产出时：

```
第一步：先 publish_task(A) → 从返回值读取真实 UUID
第二步：再 publish_task(B, dependencies="<A的UUID>")
```

⚠️ **MUST NOT** 在同一轮 reactLoop 中先发 B 后发 A。
⚠️ **MUST NOT** 在 dependencies 中使用占位符（如 "task-part1"、"A"、"<id>"）。

### 7.2 legacy 并行无依赖任务

无依赖关系的独立任务 SHOULD 在**同一轮 reactLoop 中并行发布**（多次 publish_task tool call）。

### 7.3 legacy 依赖链示例

```
用户请求："调查 docs/ 目录并产出报告"

步骤1：publish_task(
  description="探索 docs/ 目录结构并总结内容",
  event_type="explore"
)
→ 返回值: "已创建任务: id=a1b2c3d4-..."

步骤2：publish_task(
  description="基于上游调查结果，将分析写入 docs_investigation.md",
  event_type="",
  dependencies="a1b2c3d4-...",
  expected_artifacts="docs_investigation.md"
)
```

---

## 8. Expected Artifacts 规则

### 8.1 何时填写

| 情景 | 是否填写 expected_artifacts |
|------|---------------------------|
| 任务产出是落盘文件（报告/文档/代码） | ✅ MUST 填写 |
| 纯调查任务（event_type="explore"） | ❌ MUST NOT 填写（Explorer 无写权限） |
| 任务执行 shell 命令但不产生新文件 | ❌ 不填 |

### 8.2 路径规范

- 路径 MUST 可被 Worker 字面执行——不能带占位符（如 `<name>.md`）
- 路径 SHOULD 同时在 description 中显式声明："产出文件：report.md（位于项目根目录）"
- MUST 使用相对项目根的相对路径

---

## 9. Capability 边界硬规则

| 规则 | 违反后果 |
|------|---------|
| MUST NOT 给不具备 write_file/edit_file 的 Agent 声明 expected_artifacts | 任务陷入重试地狱 |
| MUST NOT 把调查+落盘塞进同一个 explore 任务 | 同上，MUST 拆为两步 |
| MUST 路由 run_shell 任务到具备该工具的 Agent | 任务失败 |
| MUST 路由 write_file/edit_file 任务到具备该工具的 Agent | 任务失败 |

---

## 10. 事实校对准则

- MUST 在引用文件时先扫 board snapshot 中所有 `task.artifacts` 字段
- MUST 只引用真实存在的文件路径——禁止凭空声称未在 artifacts 中出现的文件
- SHOULD 在调查/研究 Graph 的 `end` 前设 controller 覆盖度裁决节点；有缺口时按 `read_graph` 得到的 revision 调 `patch_graph` 扩展尚未激活的后续图
- MUST 在 report_done 的 summary 中只列 artifacts 中确认存在的文件

---

## 11. 领域启发式规则（Domain Heuristics）

本节捕获 Scheduler 在反复实践中积累的隐性知识——这些规则在 SOP 中容易被忽略，
但对 Agent 的决策质量有决定性影响。

### 11.1 任务拆分启发式

| 信号 | 推荐行动 |
|------|---------|
| 用户请求涉及 3+ 个独立子方向 | 在 Graph 中 fan-out 多个 agent 节点，再用 join 收敛 |
| 单个文件 >500 行 | 在 description 中按模块拆分，而非让 Agent 逐行读 |
| 目录下 20+ 个同类型文件 | 按子目录或功能模块拆分任务 |
| 用户说"简短/不用详细/不需要文档" | **不要 expected_artifacts**，让任务产出纯文本回复 |

### 11.2 能力路由启发式

| 信号 | 推荐行动 |
|------|---------|
| `specialized_agents` 中 busy == count | 任务会排队；如果长时间如此，考虑 provision 新 Team |
| `unavailable_tools` 包含 "web_search" | 不要发布网络调查任务；直接告诉用户 |
| runtime_mode == "scheduler_only" | MUST NOT 向空 event_type 发布任务；先 provision |
| 模板 (agent_templates) 可覆盖能力缺口 | 调用 provision_agent_team 而非放弃 |

### 11.3 保留用户原始约束

- 拆分任务 description 时，MUST **逐字保留**用户的否定性约束（"不要/禁止/避免/不用/不需要"）
- MUST NOT 以"更清晰的表述"或"润色"为由弱化否定约束
- 例：用户说"不用生成 .md 文件" → 子任务 MUST 去掉 expected_artifacts

---

## 12. 失败模式（Failure Modes）

| 场景 | 典型症状 | 恢复策略 |
|------|----------|----------|
| Explorer 声明的 expected_artifacts 永远完成不了 | 任务 status = failed，RetryLoop | 检查 Capability 边界（§9）；cancel + 重新发布为两步 |
| 依赖任务 ID 用了占位符 | publish_task 返回 Abort 错误 | 先发被依赖任务，从返回值读真实 UUID 后重新发布 |
| Graph route 不存在、归属另一 Graph 或 capability 不足 | submit_graph / patch_graph 被 fail-closed 拒绝 | 使用当前 graph_id provision，读取真实 route，或收窄节点 tools 后重试 |
| Explorer 任务完成后直接 report_done | pending_downstream_tasks 非空就被截断 | 先调用 report_progress，等下游清空后再 report_done |
| 丢失用户的否定约束 | 子任务生成了用户明确拒绝的文件 | Section 11.3：改写 description 时逐字保留否定词 |
| 节点多轮尝试后仍无进展 | 节点 blocked / graph change 唤醒 Scheduler | 先 `read_graph`，再用 `patch_graph` 调整未来图；需用户决定时走受信 Interaction |
| Scheduler-only 模式无路由 | runtime_mode == "scheduler_only" | 先决定 graph_id，再从 agent_templates 选择模板并带 graph_id provision |

---

## 附录 A：Graph-first 速查卡

```
用户输入
  │
  └─ 所有请求 ──→ submit_graph
        │
        ├─ 简单请求？ → 单个 controller 节点 → end
        │
        ├─ 只读调查？ → 只读 agent 节点（跨文件/多来源仍建图）
        │
        ├─ 需要写/Shell？ → 路由到具备对应 capability 的 agent 节点
        │
        ├─ 多方向 AND？ → fan-out → 独立 target_input → join → controller
        │
        └─ 改变状态/正确性声明？ → acceptance 以 $.verdict 精确分支 / 必要时回边
```

## 附录 B：与 AgentGo 架构的对应关系

| 本 Skill 章节 | AgentGo 代码位置 | 现有配置文件 |
|--------------|-----------------|-------------|
| §2 Board Snapshot | `internal/scheduler/scheduler.go` | — |
| §3 Graph-first 决策 | Scheduler system prompt | — |
| §4 Event Type 路由 | `internal/suggest/` | `setting.yaml → agents[].event_type` |
| §5 AgentKind 配置 | `internal/config/config.go` → `AgentKind` | `setting.yaml → agents[]` |
| §6 Reactor 绑定 | `internal/reactor/` | `general_reactor.yaml`, `reactors_file` |
| §7 Graph 依赖 / legacy 兼容 | `internal/graph/`, `internal/tools/graph_control.go`, `internal/tools/meta.go` | `max_subtask_depth` 仅 legacy |
| §9 能力边界 | `internal/gate/` | `tool_profiles` |
| §1.1 Interaction 边界 | `internal/interaction/`, `internal/bootstrap/interaction_runtime.go`, `internal/tools/` 的 MetaGroup | `modes.gate`, `tool_profiles` |
