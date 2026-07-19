---
name: scheduler-task-dispatch
description: >-
  Guide Scheduler agents in generating execution agent configuration tables and Reactor dispatch rules
  at runtime. Use when: (a) deciding A/B/C routing for user requests, (b) generating or modifying
  AgentKind definitions, (c) setting up Reactor event binding rules, or (d) managing multi-agent
  task dependencies. Complies with Anthropic agentskills.io open standard.
version: 2.0.0
tags: [scheduler, routing, agent-config, reactor, orchestration, skill-package]
capabilities: [task_routing, config_generation, reactor_binding]
input_formats: [board_snapshot, user_request]
output_formats: [publish_task, agent_config_table, reactor_rule]
compatibility: "AgentGo >= v0.1.0; requires board_snapshot injection and agent_capabilities lookup"
metadata:
  target_agent: scheduler
  skill_category: orchestration
  standards_compliance: "Anthropic Agent Skills open standard (agentskills.io)"
  authoring_compliance: "RFC 2119 constraint levels; SkillsMD version tracking"
---

# Scheduler Task Dispatch Skill

> 这份文档将 Scheduler 的调度知识从隐性的 system prompt 提升为显式、可版本化、可评审的 **技能包**。
> 遵循 Anthropic agentskills.io 开放标准（YAML frontmatter + Markdown 指令 + 渐进式披露）。
> **v2.0.0** 新增：「使用/不使用的场景」决策矩阵、RFC 2119 约束等级、领域启发式规则、失败模式表。

---

## 0. 何时使用本 Skill / 何时不使用

### ✅ 使用本 Skill 当：

| 场景 | 典型例子 |
|------|---------|
| 需要为收到的用户请求做 A/B/C 路由决策 | "帮我修改 main.go" → C 类，委派 Worker |
| 需要生成或修改 AgentKind 配置表 | 新增一个 code-reviewer Agent 类型 |
| 需要设置 Reactor 事件绑定规则 | 当 explorer 任务完成时自动触发 worker 落盘 |
| 管理跨多个 Agent 的任务依赖链 | explorer 调查 → worker 汇总报告 |
| 基于 agent_capabilities 选择正确的 event_type | "这个任务需要 run_shell，应发到 Worker" |
| 排查路由错误或任务分配失败 | 任务发到了没有写权限的 Explorer |

### ❌ 不使用本 Skill 当：

| 场景 | 正确做法 |
|------|---------|
| 简单的单 Agent 操作，无需路由决策 | 直接调用 read_file / web_search |
| 硬编码到 Go 代码的显式路由规则 | 代码层面已处理 |
| 只需要读取文件或回答事实性问题（B 类） | 自己调 tool，不要走 publish_task |
| 系统中没有配置任何 Agent（scheduler-only 模式） | 直接告诉用户当前无法委派 |
| 只是问候/闲聊（A 类） | 自然语言回答 |

---

## 1. 核心职责

Scheduler 是 AgentGo 系统中唯一拥有完整工具能力的一等代理。其核心职责：

1. **观察全局状态**：解析 Board Snapshot（tasks、resources、agents、agent_capabilities）
2. **决策三选一**：A 类（闲聊/状态查询）→ 直接回答；B 类（只读操作）→ 自己调 tool；C 类（写文件/跑命令/复杂改造）→ publish_task 委派
3. **生成执行代理配置表和 Reactor 规则**：将用户意图转化为结构化的任务分派和事件响应

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

## 3. 决策层：A/B/C 三选一

收到用户输入后，MUST 先判断请求类别：

| 类别 | 定义 | 行动 | 示例 |
|------|------|------|------|
| **A** | 闲聊 / 系统状态查询 / 资源查询 | 直接自然语言回答。**MUST NOT publish_task** | "你好"、"worker-1 在做什么" |
| **B** | 简单只读操作 | 自己调 read_file / list_dir / grep / glob / web_search / web_fetch | "读 main.go"、"grep TODO" |
| **C** | 写文件 / 跑命令 / 多方向并行 / 复杂改造 | **publish_task 委派**给 Worker / Explorer | "修改 main.go 加日志"、"调研 docs/ 目录" |

**默认假设：能自己干就自己干。** publish_task 至少多花一轮 LLM + 一次 worker poll 延迟。

---

## 4. 路由层：Event Type 选择逻辑

当走到 C 类需委派时，按以下决策链选择 event_type：

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
| `submit_acceptance_result`（正式验收） | MUST 路由到包含此工具的 Agent |

### 4.3 硬性约束

- **MUST** 只发布到系统中**实际存在**的代理类型（检查 `specialized_agents` 列表）
- **MUST NOT** 向不存在该 event_type 的队列发布——直接向用户说明无法完成的原因
- **SHOULD** 当 `specialized_agents` 中 busy 等于 count 时，优先用另一个已存在且能力足够的 route；或 provision 新 Team
- **MUST NOT** 给不具备 write_file/edit_file 的 route 声明 expected_artifacts

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
    agent_max_loops: 10
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

## 7. 依赖管理层

### 7.1 发布顺序规则（铁律）

当任务 B 依赖任务 A 的产出时：

```
第一步：先 publish_task(A) → 从返回值读取真实 UUID
第二步：再 publish_task(B, dependencies="<A的UUID>")
```

⚠️ **MUST NOT** 在同一轮 reactLoop 中先发 B 后发 A。
⚠️ **MUST NOT** 在 dependencies 中使用占位符（如 "task-part1"、"A"、"<id>"）。

### 7.2 并行无依赖任务

无依赖关系的独立任务 SHOULD 在**同一轮 reactLoop 中并行发布**（多次 publish_task tool call）。

### 7.3 依赖链示例

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
- SHOULD 在调查/研究类任务完成后评估信息缺口，有缺口则追加任务而非急于收尾
- MUST 在 report_done 的 summary 中只列 artifacts 中确认存在的文件

---

## 11. 领域启发式规则（Domain Heuristics）

本节捕获 Scheduler 在反复实践中积累的隐性知识——这些规则在 SOP 中容易被忽略，
但对 Agent 的决策质量有决定性影响。

### 11.1 任务拆分启发式

| 信号 | 推荐行动 |
|------|---------|
| 用户请求涉及 3+ 个独立子方向 | 并行发布多个 explore 任务 |
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
| 路由到不存在的 event_type | 任务 publish 被拒绝 | 检查 `specialized_agents`，使用已存在的类型或 provision |
| Explorer 任务完成后直接 report_done | pending_downstream_tasks 非空就被截断 | 先调用 report_progress，等下游清空后再 report_done |
| 丢失用户的否定约束 | 子任务生成了用户明确拒绝的文件 | Section 11.3：改写 description 时逐字保留否定词 |
| 多轮尝试后仍无法完成 | 任务反复重试失败 | 向用户汇报三种选择：continue/converge/terminate（resolve_plan_pause） |
| Scheduler-only 模式无路由 | runtime_mode == "scheduler_only" | 从 agent_templates 选择合适的模板 provision |

---

## 附录 A：决策树速查卡

```
用户输入
  │
  ├─ A类（闲聊/状态/资源查询）──→ 直接自然语言回答
  │
  ├─ B类（只读操作：读文件、搜索代码、查网页）
  │     └──→ 自己调 tool（read_file / grep / web_search / web_fetch）
  │
  └─ C类（写文件 / 跑命令 / 复杂改造）
        │
        ├─ 纯只读调查？
        │   ├─ 是 → event_type="explore"（❌ expected_artifacts）
        │   └─ 否 → event_type=""（Worker）
        │
        ├─ 需要写文件？
        │   └─ 是 → MUST event_type="" + 声明 expected_artifacts
        │
        ├─ 需要跑 shell？
        │   └─ 是 → MUST event_type=""
        │
        └─ 有依赖？
            └─ 先发被依赖任务 → 读 UUID → 再发依赖方任务
```

## 附录 B：与 AgentGo 架构的对应关系

| 本 Skill 章节 | AgentGo 代码位置 | 现有配置文件 |
|--------------|-----------------|-------------|
| §2 Board Snapshot | `internal/scheduler/scheduler.go` | — |
| §3 A/B/C 决策树 | Scheduler system prompt | — |
| §4 Event Type 路由 | `internal/suggest/` | `setting.yaml → agents[].event_type` |
| §5 AgentKind 配置 | `internal/config/config.go` → `AgentKind` | `setting.yaml → agents[]` |
| §6 Reactor 绑定 | `internal/reactor/` | `general_reactor.yaml`, `reactors_file` |
| §7 依赖管理 | `internal/tools/meta.go` → `publish_task` | `max_subtask_depth` |
| §9 能力边界 | `internal/gate/` | `tool_profiles` |

## 附录 C：Skill 撰写检查清单（Skill Authoring Checklist）

基于 Anthropic 官方最佳实践和本次互联网调查结果，撰写或增强一个 Skill 包时检查以下项：

### Frontmatter
- [ ] `name` 全小写、仅限字母/数字/连字符、不超过 64 字符
- [ ] `name` 与父目录名完全一致
- [ ] `description` 同时包含**能力声明**和**触发条件**，不超过 1024 字符
- [ ] `version` 遵循语义化版本号（Major.Minor.Patch）
- [ ] `compatibility` 声明了必需的环境依赖（如适用）

### 文档结构
- [ ] 包含 "When to Use / When Not to Use" 决策矩阵
- [ ] 步骤描述遵循"三句规则"：做什么 → 为什么 → 验证什么
- [ ] 约束使用 RFC 2119 等级（MUST / MUST NOT / SHOULD / SHOULD NOT / MAY）
- [ ] 包含至少 1 个**好例子**和 1 个**坏例子**（对比法）
- [ ] 包含领域启发式规则和失败模式表

### 反模式检查
- [ ] 未将 SKILL.md 写成百科式文档（面向 Agent 执行，非面向人类阅读）
- [ ] 未提供过多选项（避免 Agent 决策瘫痪）
- [ ] 未假设不存在的工具或环境
- [ ] 长文档已拆分进 references/ 目录
