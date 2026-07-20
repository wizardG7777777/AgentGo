# AgentGo 技能感知调度设计 —— 从调研到可执行配置的落地方案

> **状态：** Draft v1.0
> **上游依赖：** `docs/agent-skill-survey-and-design-implications.md` + Explorer 补充调查 (task b85091f2)
> **目标读者：** Scheduler 开发者、AgentGo 核心贡献者
> **核心结论：** AgentGo 已具备配置驱动架构的骨架，仅需 **3 项增量改造** 即可实现"技能感知调度"——从静态 AgentKind 声明进化为可渐进披露的 Skill 包。

---

## 1. 调研结论摘要（从 Explorer 调查提炼）

### 1.1 业界共识

| 结论 | AgentGo 现状匹配度 |
|---|---|
| **Skill ≠ System Prompt 片段**——Skill 是完整的可发现/可加载配置包 | ⚠️ 当前 `AgentKind.Description` ≈ 一句话摘要，`SystemPromptFile` ≈ 独立文件，但无"包"概念 |
| **渐进式披露（Progressive Disclosure）**——启动时仅加载 name+desc，触发后才加载完整 SKILL.md | ✅ 已有基础：`AgentRegistry` + `EventType` 路由实现按需唤醒 |
| **Block 三原则**——确定性规则放脚本、解读留给 Agent、硬约束而非建议 | ⚠️ Reactors 已覆盖部分硬约束场景，但非系统性 |
| **调度策略文档化→可执行配置**处于早期阶段——完整方案尚属空白 | 🎯 **AgentGo 有机会成为先行者** |

### 1.2 关键缺口（从调研中识别）

1. **技能发现机制缺失**：当前 AgentKind 是静态列表，Scheduler LLM 只能通过 `Description` 字段做语义匹配——没有结构化的技能能力声明（如 `input_formats`、`output_formats`、`preconditions`）
2. **路由表隐式散落**：`EventType` 路由散落在 YAML 配置、Reactor 规则和硬编码中，无法集中查看和调试
3. **技能版本/演进无追踪**：AgentKind 无版本字段，prompt 变更后无法追溯
4. **缺少"从技能描述自动生成代理配置表"的机制**：这正是上下游 scheduler-2e174601 正在攻克的问题

---

## 2. AgentGo 技能定义规范（Skill Definition Spec）

### 2.1 设计原则

基于 Block 三原则和 agentskills.io 标准，AgentGo 的 Skill 定义分三层：

```
┌─────────────────────────────────────────────┐
│  Level 1: 元数据（始终加载到 board snapshot）  │
│  - skill_id / version / description          │
│  - input_formats / output_formats            │
│  - capabilities (结构化的能力标签)              │
│  Token 预算: ~150 tokens/skill               │
├─────────────────────────────────────────────┤
│  Level 2: 指令正文（技能触发时按需加载）         │
│  - system_prompt_file (已有)                  │
│  - skill_constraints (硬约束声明)              │
│  - fallback_behavior                        │
│  Token 预算: < 5K tokens                     │
├─────────────────────────────────────────────┤
│  Level 3: 引用资源（按需读取/执行）             │
│  - scripts/ (确定性脚本)                      │
│  - references/ (知识库)                       │
│  - assets/ (模板)                             │
│  按需读取，几乎无 token 上限                    │
└─────────────────────────────────────────────┘
```

### 2.2 技能目录物理布局（面向未来）

当前 AgentGo 的 `prompts/` 目录已经是一个雏形。建议的演进方向：

```
skills/                          # 新顶层目录（或复用 prompts/ + 扩展）
├── explorer/
│   ├── SKILL.md                 # 等价于当前 prompts/explorer.md
│   ├── constraints.yaml         # 硬约束声明（新增）
│   └── references/
│       └── web-search-guide.md  # 按需加载的参考资料（新增）
├── worker/
│   ├── SKILL.md                 # 等价于当前 prompts/worker.md
│   └── constraints.yaml
├── verifier/
│   ├── SKILL.md                 # 等价于当前 prompts/verifier.md
│   └── constraints.yaml
└── scheduler/
    ├── SKILL.md                 # 等价于当前 prompts/scheduler 使用
    └── orchestration.yaml       # 编排策略配置（新增，见 §4）
```

### 2.3 YAML 配置扩展（最小侵入方案）

当前 `AgentKind` 结构：

```yaml
agents:
  - kind: explorer
    replicas: 1
    event_type: explore
    profile: explorer
    description: "广度优先的网络调研代理，不写文件，仅返回 Markdown 文字回复"
    system_prompt_file: prompts/explorer.md
```

**建议扩展为 Skill-aware AgentKind**（向后兼容——所有新字段均为 optional）：

```yaml
agents:
  - kind: explorer
    replicas: 1
    event_type: explore
    profile: explorer
    description: "广度优先的网络调研代理，不写文件，仅返回 Markdown 文字回复"
    system_prompt_file: prompts/explorer.md

    # === 新增字段：技能感知扩展（全部 optional） ===

    # skill_id: 唯一技能标识符，用于路由表索引和技能发现
    skill_id: "explorer-v1"

    # skill_version: 语义化版本，prompt 变更时递增
    skill_version: "1.0.0"

    # skill_dir: 技能目录路径（当技能使用目录结构时）
    # 为空时回退到 system_prompt_file 单文件模式
    skill_dir: "skills/explorer"

    # capabilities: 结构化的能力标签（替代纯文本 description 中隐式描述的能力）
    # scheduler 用此字段做硬性筛选；description 用于语义优选
    capabilities:
      input:
        - "task_description"       # 接收任务描述
        - "dependency_context"     # 接收上游任务结果
        - "team_snapshot"          # 接收团队状态快照
      output:
        - "markdown_report"        # 产出 Markdown 报告
        - "transfer_note"          # 产出交接备注
      tools:
        - "web_search"
        - "web_fetch"
        - "read_file"
        - "grep_search"
        - "glob_search"
        - "list_dir"
        - "send_message"
      constraints:
        - "no_file_write"          # 硬约束：不写文件
        - "no_shell_exec"          # 硬约束：不执行 shell

    # fallback: 当此技能无法完成任务时的降级策略
    fallback:
      action: "escalate_to_worker"  # escalate_to_worker | retry | fail
      target_skill: "worker-v1"     # 降级目标技能 ID
```

### 2.4 Go 类型扩展

在 `internal/config/config.go` 中增量添加（不破坏现有 AgentKind）：

```go
// SkillCapabilitySet 技能的能力声明（vNext）。
// 替代纯文本 Description 中的隐式能力描述，供 scheduler 做结构化匹配。
type SkillCapabilitySet struct {
    Input       []string `yaml:"input,omitempty"       json:"input,omitempty"`
    Output      []string `yaml:"output,omitempty"      json:"output,omitempty"`
    Tools       []string `yaml:"tools,omitempty"       json:"tools,omitempty"`
    Constraints []string `yaml:"constraints,omitempty" json:"constraints,omitempty"`
}

// SkillFallback 技能降级策略。
type SkillFallback struct {
    Action      string `yaml:"action"       json:"action"`      // escalate_to_worker | retry | fail
    TargetSkill string `yaml:"target_skill,omitempty" json:"target_skill,omitempty"`
}

// 在 AgentKind 中新增字段（全部 omitempty，向后兼容）：
// SkillID          string              `yaml:"skill_id,omitempty"          json:"skill_id,omitempty"`
// SkillVersion     string              `yaml:"skill_version,omitempty"     json:"skill_version,omitempty"`
// SkillDir         string              `yaml:"skill_dir,omitempty"         json:"skill_dir,omitempty"`
// Capabilities     *SkillCapabilitySet `yaml:"capabilities,omitempty"      json:"capabilities,omitempty"`
// Fallback         *SkillFallback      `yaml:"fallback,omitempty"          json:"fallback,omitempty"`
```

---

## 3. 路由表设计（Routing Table）

### 3.1 设计目标

将当前**隐式分散**的路由逻辑集中为一张可读、可调试、可由 LLM 辅助生成的**路由表**。

### 3.2 路由表格式

```yaml
# routing_table.yaml（由 scheduler 在启动时从 AgentKind[] 生成）
routing_table:
  version: "1.0.0"
  generated_at: "2026-05-01T10:00:00Z"

  # === 按 EventType 索引的路由条目 ===
  entries:
    - event_type: "explore"
      skill_id: "explorer-v1"
      kind: "explorer"
      replicas: 1
      capabilities:
        input: ["task_description", "dependency_context", "team_snapshot"]
        output: ["markdown_report", "transfer_note"]
        tools: ["web_search", "web_fetch", "read_file", "grep_search",
                "glob_search", "list_dir", "send_message"]
      constraints:
        - "no_file_write"
        - "no_shell_exec"
      match_rules:
        - type: "intent_keyword"
          values: ["调查", "调研", "搜索", "research", "explore", "survey"]
        - type: "output_format"
          values: ["markdown_report", "transfer_note"]
      fallback:
        action: "escalate_to_worker"

    - event_type: ""              # 默认队列 → worker
      skill_id: "worker-v1"
      kind: "worker"
      replicas: 2
      capabilities:
        input: ["task_description", "dependency_context", "team_snapshot"]
        output: ["file_write", "shell_result", "transfer_note"]
        tools: ["read_file", "write_file", "edit_file", "run_shell",
                "web_search", "web_fetch", "glob_search", "grep_search",
                "list_dir", "publish_task", "send_message"]
      constraints: []
      match_rules:
        - type: "default"          # 兜底路由
      fallback:
        action: "fail"

  # === 路由决策优先级 ===
  # 1. 显式 event_type 匹配（最高优先级）
  # 2. capabilities 硬性筛选（tools 超集匹配）
  # 3. match_rules 语义匹配（intent_keyword / output_format）
  # 4. default 兜底路由
```

### 3.3 路由表生成逻辑（Scheduler 启动时）

```
输入: cfg.Agents[] (AgentKind 列表含 skill_* 扩展字段)
处理:
  1. 遍历 cfg.Agents，为每个 AgentKind 生成一个 RoutingEntry
  2. 提取 EventType → RoutingEntry 映射（显式路由）
  3. 构建 match_rules:
     a. 从 capabilities.output 反推 output_format match
     b. 从 description / skill_id 提取 intent_keyword（或用 LLM 辅助提取）
  4. 标记 default 路由（EventType == "" 的条目）
  5. 构建降级链（fallback.target_skill → 查找对应 RoutingEntry）
输出: RoutingTable (可序列化为 YAML/JSON，注入 board snapshot)
```

### 3.4 路由表在 Board Snapshot 中的呈现

当前 `BoardSnapshot` 的 `specialized_agents` 段已经做了雏形。扩展方案：

```json
{
  "specialized_agents": [
    {
      "event_type": "explore",
      "skill_id": "explorer-v1",
      "count": 1,
      "role": "广度优先的网络调研代理",
      "capabilities": {
        "input": ["task_description", "dependency_context"],
        "output": ["markdown_report", "transfer_note"],
        "tools": ["web_search", "web_fetch", "read_file", "..."],
        "constraints": ["no_file_write", "no_shell_exec"]
      }
    }
  ],
  "routing_table_summary": [
    {"event_type": "explore", "skill": "explorer-v1", "match": "intent:调查/调研/搜索"},
    {"event_type": "",        "skill": "worker-v1",   "match": "default"}
  ]
}
```

---

## 4. 调度策略文档化→可执行配置（核心贡献）

### 4.1 问题定义

当前的调度策略**散落在 3 个地方**：
1. YAML 配置的 `AgentKind.EventType`（显式路由）
2. Scheduler 的 system prompt（隐式指导 LLM 如何派发）
3. Reactors 规则（条件触发）

**目标：** 将这些分散的策略统一为单一可执行配置源——`orchestration.yaml`。

### 4.2 编排配置格式（Orchestration Config）

```yaml
# orchestration.yaml —— 调度策略的可执行文档
# 位置: skills/scheduler/orchestration.yaml（或 config.orchestration.yaml）

orchestration:
  version: "1.0.0"

  # === 工作流模板（预定义的调度模式） ===
  workflows:
    - name: "survey_then_report"
      description: "先调研后写报告的经典两阶段流水线"
      steps:
        - phase: 1
          target_skill: "explorer-v1"
          description: "广度调查阶段"
          timeout_sec: 600
          on_failure: "retry_once"
        - phase: 2
          target_skill: "worker-v1"
          description: "落盘报告阶段"
          depends_on: [1]
          timeout_sec: 300
          on_failure: "escalate_to_scheduler"

    - name: "code_change_with_verification"
      description: "代码修改→审查→验证的三阶段流水线"
      steps:
        - phase: 1
          target_skill: "worker-v1"
          description: "编写代码/修改文件"
        - phase: 2
          target_skill: "verifier-v1"
          description: "审查产出"
          depends_on: [1]
          trigger: "auto"          # 自动触发（通过 reactor）
        - phase: 3
          target_skill: "worker-v1"
          description: "修复审查发现的问题"
          depends_on: [2]
          trigger: "conditional"   # 仅在审查发现问题时触发

  # === 路由规则（任务→技能的映射策略） ===
  routing_rules:
    - name: "exploration_intent"
      description: "当任务涉及调研/搜索/调查时路由到 explorer"
      match:
        intent_keywords: ["调查", "调研", "搜索", "研究", "research",
                          "explore", "survey", "find", "查找"]
        expected_output: ["markdown_report", "transfer_note"]
      route_to: "explorer-v1"
      priority: 10              # 数字越大优先级越高

    - name: "file_write_intent"
      description: "当任务需要写文件时路由到 worker"
      match:
        intent_keywords: ["创建", "写入", "修改", "生成文件", "编辑",
                          "create", "write", "edit", "generate"]
        required_tools: ["write_file", "edit_file"]
      route_to: "worker-v1"
      priority: 5

    - name: "default_catch_all"
      description: "兜底路由——所有未匹配的任务发给 worker"
      match:
        default: true
      route_to: "worker-v1"
      priority: 0

  # === 调度约束（跨工作流的全局约束） ===
  constraints:
    max_concurrent_tasks: 5
    max_retries_per_task: 3
    task_timeout_default_sec: 600
    escalation_receiver: "scheduler"  # 无法处理时的上报目标
```

### 4.3 从编排配置到执行的分层

```
orchestration.yaml (人写 + LLM 辅助维护)
        │
        ▼
  [Scheduler 启动时解析]
        │
        ├──→ RoutingTable (已定义于 §3)
        │
        ├──→ WorkflowTemplates (注入 scheduler system prompt)
        │       scheduler LLM 看到可用工作流模板，按需实例化
        │
        └──→ SchedulingConstraints (注入 scheduler system prompt)
                全局约束：并发上限、超时、重试
```

### 4.4 LLM 辅助的路由规则生成（填补空白）

调研结论指出"完整的从描述性调度策略文档自动生成 agent 路由/分发表的生产级方案尚不存在"——**这正是 AgentGo 可以率先实现的差异化能力**。

建议在 Scheduler 启动时增加一个**引导阶段**：

```
1. 读取所有 AgentKind（含 skill_id / capabilities / description）
2. 用 LLM（低 token 消耗，单轮）生成：
   a. 每个技能的能力摘要（注入 board snapshot）
   b. intent_keyword → skill_id 的推荐映射
   c. 冲突检测（两个技能声称相同 intent 时的告警）
3. 人工审阅（可选：通过 YAML 文件的 human_review_required 标志控制）
4. 生成最终 routing_table + board snapshot
```

这个 LLM 调用是**一次性启动开销**（非每任务），且产出结果可缓存——与现有 scheduler LLM 每任务推理相比，成本可忽略。

---

## 5. 与现有 AgentGo 架构的集成路径

### 5.1 Phase 1：最小可行扩展（建议本 Sprint 完成）

| 改动 | 文件 | 工作量 |
|---|---|---|
| AgentKind 新增 `skill_id`、`skill_version`、`capabilities` 字段 | `internal/config/config.go` | ~30 行 |
| 路由表生成函数 `BuildRoutingTable(cfg *Config) *RoutingTable` | `internal/scheduler/routing.go`（新建） | ~80 行 |
| Board Snapshot 中新增 `routing_table_summary` 段 | `internal/scheduler/snapshot.go` | ~40 行 |
| 配置校验：skill_id 唯一性检查 | `internal/config/config.go` → `Validate()` | ~15 行 |

### 5.2 Phase 2：渐进式披露（建议下个 Sprint）

| 改动 | 说明 |
|---|---|
| 技能目录加载器 `LoadSkill(skillDir string) (*Skill, error)` | 按 SKILL.md + constraints.yaml + references/ 结构加载 |
| `skill_constraints` 注入 agent system prompt | 将硬约束从"提醒"升级为结构性注入 |
| `orchestration.yaml` 解析器 | 统一调度策略配置入口 |

### 5.3 Phase 3：LLM 辅助路由生成（后续版本）

| 改动 | 说明 |
|---|---|
| Scheduler 启动引导阶段 | LLM 辅助从 AgentKind[] 生成 routing_rules |
| 路由冲突检测与告警 | 两个技能声明相同 intent 时的自动化检测 |
| 路由表版本管理与回滚 | 支持 routing_table 变更追溯 |

---

## 6. 总结：从调研到落地的关键决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| Skill 定义格式 | 扩展 AgentKind（不新增独立 Skill 类型） | 最小侵入，向后兼容，渐进演化 |
| 路由表生成 | 启动时从 AgentKind[] 静态生成 + 可选 LLM 辅助 | 确定性优先，LLM 辅助作为增强 |
| 编排策略 | 独立的 `orchestration.yaml`（非嵌入 AgentKind） | 关注点分离：agent 定义 vs 调度策略 |
| 技能目录 | 当前 `prompts/` 向后兼容，未来可选迁移到 `skills/` | 零破坏性变更 |
| Block 三原则落地 | `constraints.yaml` 作为硬约束声明 | 脚本化确定性规则，LLM 不自由发挥 |

---

## 附录 A：与调研来源的对应关系

| 设计元素 | 来源 |
|---|---|
| 三层渐进式披露 | [agentskills.io 规范](https://agentskills.io/specification) |
| System Prompt vs Skill 的宪法/双手比喻 | [System Prompt vs Agent Skills - Medium](https://medium.com/@the_manoj_desai/system-prompt-vs-agent-skills-the-architecture-decision-that-makes-or-breaks-your-ai-agent-b58357df1f10) |
| 硬约束设计（constraints.yaml） | [Block Engineering Blog](https://engineering.block.xyz/blog/3-principles-for-designing-agent-skills) |
| orchestration.yaml 的结构 | [MindStudio - Orchestrator Skill](https://www.mindstudio.ai/blog/what-is-orchestrator-skill-claude-code) |
| 配置驱动架构思路 | [Alibaba Cloud Community](https://www.alibabacloud.com/blog/configuration-driven-dynamic-agent-architecture-network-achieving-efficient-orchestration-dynamic-updates-and-intelligent-governance_602564) |
| YAML 路由配置实践 | [Reddit r/ClaudeAI](https://www.reddit.com/r/ClaudeAI/comments/1t2i664/set_up_multiagent_orchestration_with_claude_code) |
| 多代理架构分类 | [LangChain Blog](https://www.langchain.com/blog/choosing-the-right-multi-agent-architecture) |
| SSL 结构化表示 | [arXiv 2604.24026](https://arxiv.org/html/2604.24026v1) |

## 附录 B：关键缺口与后续探索建议

| 缺口 | 优先级 | 建议 |
|---|---|---|
| Agent 间能力协商协议 | 中 | 当两个 agent 对同一任务声称能力时，需要协商机制 |
| 运行时技能热加载 | 低 | 当前 YAML 重启生效已足够；热加载增加复杂度 |
| 技能市场（Skill Marketplace） | 低 | 参考 Block 100+ skills 的内部市场，AgentGo 可建立社区贡献的技能库 |
| 技能的跨实例共享 | 低 | 当前每个 AgentKind 独立加载；共享可减少 token 消耗 |
