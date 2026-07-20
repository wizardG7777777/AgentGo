# 外部生态调研：Reactor 模式验证、声明式 Agent 配置与 Prompt 版本管理

> **来源**：基于 explorer-2 对国际互联网的广度调查（2026年6月）
> **上游任务**：c379cd7f-da2d-4685-9dfd-07e1ad48a843
> **目标读者**：AgentGo scheduler 开发者。配合 `agent-skill-survey-and-design-implications.md`（中文源）阅读，形成完整的 Skill 生态认知。

---

## 一、Reactor 模式在多 Agent 系统中的验证

### 1.1 核心发现

Reactor 模式（事件循环 + 多路分发 + Handler）与 Scheduler/Orchestrator 架构高度同构。AgentGo 的 ReactiveSystem 架构已在多个独立的工业项目中得到模式层面验证。

### 1.2 映射表

| 经典 Reactor 概念 | 多 Agent 系统对应物 | AgentGo 实现 |
|---|---|---|
| Event Loop | Scheduler 主循环（监控邮件、任务队列） | `internal/scheduler/` 主循环 |
| Demultiplexer | 任务路由引擎（解析请求 → 选择 Agent 类型） | `event_type` 匹配 + `suggest` 子系统 |
| Event Handler | Agent 执行单元（worker / explorer） | `internal/agent/` + kind 体系 |
| Synchronous Event DeMultiplexer | 邮件通知系统 / reactLoop | `internal/mailbox/` + `internal/reactor/` |

### 1.3 外部项目验证

**Confluent**「Four Design Patterns for Event-Driven, Multi-Agent Systems」[来源: confluent.io] 系统性地将 4 种 MAS 模式（hub-and-spoke、orchestrator-worker、hierarchical、group-chat）转化为事件驱动架构。核心洞察："使用数据流式传输，可以移除 agent 编排中专用通信路径的需求"——这与 AgentGo 的 Reactor "事件层解耦 Handler"原则完全一致。

**微软 Azure Architecture Center** [来源: learn.microsoft.com] 的 AI Agent Orchestration Patterns 列出 concurrent、group-chat、handoff、magentic 四种模式。其中 **handoff 模式**本质上是 Reactor 的 Handler 链式调度——AgentGo 的 `publish_task` + TransferNote 已实现此模式。

**Tetrate.io** [来源: tetrate.io] 明确指出 Agent 的基本特征包含 **reactivity**（对环境变化做出响应的能力），这与 AgentGo 的 ReactiveSystem §0 核心原则中"系统对状态变化的程序化反应能力"直接对应。

**Spring AI Agentic Patterns** [来源: spring.io] 将 Agent Skills 作为模块化可复用能力实现，其本质是一个事件驱动的技能加载/调度系统。

### 1.4 对 AgentGo 的启示

1. AgentGo 的 Reactor 架构方向已被多个独立的工业项目验证——不是理论推测
2. Confluent 的"事件流解耦通信路径"理念可指导 AgentGo 未来减少 Agent 间直接 messaging
3. Azure handoff 模式与 AgentGo 的 TransferNote 机制对等，说明当前设计符合行业标准

---

## 二、声明式 Agent 配置的业界实践

### 2.1 核心发现

Kubernetes CRD 风格已成为声明式 Agent 配置的事实标准，多个项目一致性极高。AgentGo 的 YAML 配置方向与此趋势对齐。

### 2.2 项目对比

| 项目 | 配置格式 | 关键字段 | 运行时 |
|---|---|---|---|
| **kagent** | K8s CRD (v1alpha2) | `spec.declarative.modelConfig`, `promptTemplate`, `dataSources`, `systemMessage` | Kubernetes |
| **DMAP** | YAML + Markdown 目录 | `agentcard.yaml` (identity, tier, constraints), `AGENT.md` (goals, workflow), `tools.yaml` | 任意 CLI 运行时 |
| **Anthropic Skills** | 文件系统目录 | `SKILL.md` (YAML frontmatter + markdown body + 附属文件) | Claude Code / API |
| **Spring AI** | 文件系统目录 | `SKILL.md` (name, description, instructions) | Spring Boot / JVM |
| **Microsoft 365 Declarative Agents** | 平台配置 | instructions, actions, knowledge | M365 Copilot |

### 2.3 共同模式（对 AgentGo 配置表设计的直接参考）

所有项目的配置本质上都是 **"system prompt 的声明式外化"**：

1. **元数据与正文分离**：name + description 用于路由决策，正文按需加载
2. **可版本化、可评审**：配置是文件，可走 PR review
3. **可分发的制品**：支持从远程拉取或作为目录安装

### 2.4 kagent 详解（AgentGo 最直接的对标参考）

kagent 是 CNCF Sandbox 项目 [来源: kagent.dev]，Kubernetes-native 的 Agent CRD：

- **`kind: Agent`** CRD，包含完整的 Agent 生命周期管理
- **System prompt 使用 Go template**：`{{include "builtin/safety-guardrails"}}` 语法支持模板化 prompt 组装
- **5 个预置 ConfigMap 模板**：`skills-usage`、`tool-usage-best-practices`、`safety-guardrails`、`kubernetes-context`、`a2a-communication`
- **两种 Skill 类型**：
  - **A2A Skills**：纯元数据描述（能力目录，不执行）
  - **Container-based Skills**：可执行容器镜像（从 registry 加载）

**对 AgentGo 的启示**：
- Go template 的 `{{include}}` 语法可直接借鉴到 AgentGo 的 prompt 模板系统
- ConfigMap 式的可复用模板正好对应 AgentGo 的 `prompts/` 目录
- A2A Skills 的概念与 AgentGo 的 `kind` + `description` 体系一致

### 2.5 DMAP 详解（三层 Clean Architecture）

DMAP [来源: medium.com/@hiondal | 2026-02] 将 Clean Architecture 应用于 Agent 系统：

```
Skills（Controller 层） → Agents（Service 层） → Gateway（Infrastructure 层）
```

**三层职责**：
- **Skills 层**：请求入口，意图分类与路由
- **Agents 层**：每 Agent = `AGENT.md` + `agentcard.yaml` + `tools.yaml`
- **Gateway 层**：抽象声明到具体实现的映射

**Prompt 三阶段组装**（兼顾缓存效率）：
1. 全局共享静态（runtime-mapping）
2. 每 Agent 静态（AGENT.md + agentcard.yaml + tools.yaml）
3. 动态（任务指令）

**4-Tier 模型映射**：HEAVY → HIGH → MEDIUM → LOW，将 Agent 从抽象声明映射到具体 LLM 模型。

**对 AgentGo 的启示**：
- DMAP 的三层架构正好映射到 AgentGo 的：`Skill 路由` → `Agent kind` → `LLM client`
- Prompt 三阶段组装策略可以直接指导 AgentGo scheduler 的 prompt 生成逻辑
- 4-Tier 模型映射与 AgentGo 可能需要的"不同任务复杂度 → 不同 LLM 模型"路由一致

---

## 三、System Prompt 即技能包

### 3.1 核心共识

System Prompt 正在被明确视为一种可管理、可版本化、可组合的"技能包"，多个独立来源一致确认：

- **Anthropic** [来源: anthropic.com]：Agent Skills = "organized folders of instructions, scripts, and resources" → 模块化、版本化的 system prompt 集合
- **kagent** [来源: kagent.dev]：Agent Instructions = "A set of instructions that define the agent's behavior and capabilities. This is also called a system prompt."
- **SMU 论文** (arXiv 2606.06923) [来源: arxiv.org | 2026]：DeclarativeAgent = "AI agents equipped with natural-language skill files **appended to the system prompt**"
- **DMAP** [来源: medium.com/@hiondal]：AGENT.md 本质上就是每个 Agent 的 system prompt 声明式文件

### 3.2 学术实证（arXiv 2606.06923）

SMU 的正式研究直接对比**声明式（Skill 文件）vs 命令式（状态机）编排**[来源: arxiv.org | 2026]：

- **结论**：在高质量检索条件下，声明式 Skills **持续提升过程性任务准确率，减少编排错误**
- 命令式状态机的"脆弱性"并不能可靠提升任务成功率
- 声明式 Agent = 在 system prompt 中附带领域特定 markdown skill 文件，由 LLM 自主决定控制流

**对 AgentGo 的启示**：AgentGo 当前混合了声明式（YAML 配置、prompt 文件）和命令式（Go 代码中的状态机）两种风格。学术结论支持 AgentGo **增加声明式的比重**——将更多编排逻辑从 Go 代码迁入 YAML + Markdown 配置。

### 3.3 Prompt 版本管理工具生态

| 工具/平台 | 定位 | 链接 |
|---|---|---|
| **LaunchDarkly** | 企业级 feature flag + prompt versioning | launchdarkly.com |
| **LangSmith (LangChain)** | LLM 可观测性 + prompt hub + 版本管理 | langchain.com |
| **LangWatch** | 集中式 prompt registry，不可变版本，环境映射 | langwatch.ai |
| **Braintrust** | prompt 版本化 + 评估 + 部署 | braintrust.dev |
| **Agenta** | 开源 LLMOps，prompt 管理与评估 | agenta.ai |
| **Latitude** | 开源 prompt 版本控制（Git-like） | latitude.so |
| **Lilypad** | 开源 prompt 版本控制 | mirascope.com |
| **Kore.ai** | 企业级 prompt 版本控制 | kore.ai |

### 3.4 Prompt 版本管理最佳实践

基于 LaunchDarkly、Braintrust、Agenta 的共识 [来源: launchdarkly.com, braintrust.dev, agenta.ai]：

1. **语义化版本号**（Major.Minor.Patch）管理 prompt 变更
2. **不可变版本记录**——每条 prompt 变更产生唯一 ID
3. **Git 式工作流**——PR review → 测试 → 部署
4. **环境分离**（dev / staging / production）
5. **回滚能力**——秒级恢复到稳定版本
6. **可溯源性**——每次输出可追溯到产生它的 prompt 版本

**对 AgentGo 的启示**：当前 `prompts/*.md` 文件的版本管理依赖 git，但缺少运行时版本追踪。建议：
- 为每个 `prompts/*.md` 增加 YAML Frontmatter（含 `version` 字段）
- 在 trace 事件中记录使用的 prompt 版本
- 支持多环境 prompt 配置（如 `prompts/dev/worker.md` vs `prompts/prod/worker.md`）

---

## 四、对 AgentGo Scheduler 配置表生成的具体建议

### 4.1 配置表应增加的维度

基于 kagent CRD 和 DMAP 的字段设计，AgentGo 的 Agent 配置表建议增加以下字段：

| 字段 | 来源 | 用途 | 优先级 |
|---|---|---|---|
| `skill_id` | Anthropic / kagent | 唯一标识 Skill，用于路由匹配 | P0 |
| `skill_version` | LaunchDarkly / LangSmith | 追踪 prompt 版本，支持回滚 | P1 |
| `tier` | DMAP 4-Tier | 模型层级（HEAVY/HIGH/MEDIUM/LOW），指导 LLM 选择 | P1 |
| `prompt_template` | kagent Go template | 模板化的 system prompt 组装 | P2 |
| `preconditions` | DMAP / 六元组 Pre | 丰富的激活条件断言（当前仅 event_type） | P1 |
| `dag` | arXiv 六元组 P | 步骤计划 DAG，支持依赖解析 | P2 |

### 4.2 声明式 vs 命令式的权衡

| 场景 | 推荐方式 | 理由 |
|---|---|---|
| 简单路由（kind + event_type） | 命令式（Go 代码） | 确定性高，开销低 |
| 复杂匹配（多条件激活） | 声明式（YAML preconditions） | 可配置、可版本化 |
| 固定工作流 | 命令式 | 避免 LLM 偏离 |
| 开放任务分解 | 声明式（DAG + Skill） | 利用 LLM 推理能力 |
| 安全关键操作 | 命令式 Gate | AgentGo 原则 2：Gate 是事前守门人 |
| 副作用处理 | 声明式 Reactor | AgentGo 原则 2：Reactor 事后异步响应 |

### 4.3 渐进式披露的 AgentGo 落地路径

结合 Anthropic 三层模型和 AgentGo 现状：

```
L1 元数据层 → board_snapshot 增加 skill_id + tier（✅ 低成本，高收益）
L2 指令层   → 按需加载 SKILL.md 而非全量注入 prompt（⚠️ 需 scheduler 改造）
L3 资源层   → 支持 scripts/ + references/ 按需加载（❌ 新能力，需文件系统工具增强）
```

### 4.4 与已有调研的互补关系

本文档应配合 `docs/agent-skill-survey-and-design-implications.md` 阅读：

| 维度 | 中文调研（explorer-3） | 国际调研（本文档） |
|---|---|---|
| **理论框架** | 六元组模型 + DAG | Reactor 模式验证 + 声明式配置 |
| **平台参考** | Coze, Dify, 华为 AgentArts, 嘉为蓝鲸 | kagent, DMAP, Spring AI, Azure |
| **Skill 撰写** | 三步法 + 聚焦原则 | System Prompt 版本管理 |
| **学术验证** | SkillsBench 论文 | arXiv 2606.06923（声明式 vs 命令式） |
| **运维场景** | 红帽 + 嘉为蓝鲸 + Elastic | Confluent 事件驱动 MAS |

---

## 五、优先级建议

| 优先级 | 建议 | 预期收益 | 来源 |
|---|---|---|---|
| **P0** | 配置表中增加 `tier` 字段（模型层级路由） | Scheduler 能根据任务复杂度选择合适模型 | DMAP 4-Tier |
| **P0** | prompt 文件增加 YAML Frontmatter（name + version） | 可追踪、可回滚的 prompt 版本管理 | Anthropic + LangSmith |
| **P1** | 增强 preconditions 为结构化断言（不止 event_type） | 更精准的任务-Agent 匹配 | DMAP + 六元组 Pre |
| **P1** | Go template 语法支持（如 `{{include "builtin/safety"}}`） | 减少重复 prompt，提升一致性 | kagent |
| **P2** | 声明式 DAG 任务图替代线性队列 | 支持依赖解析和并行执行 | arXiv 六元组 P |
| **P2** | Agent 收敛为自包含 Skill 目录 | 提升可维护性和社区兼容性 | Anthropic / DMAP |

---

> ⚠️ **时效性说明**：本报告基于 2026 年 6 月国际互联网公开资料。Agent Skills 标准仍在快速演进中，kagent（CNCF Sandbox）和 Anthropic Skills 规范可能有更新。建议在关键设计决策前验证引用源的最新状态。
>
> **上游调查约束**：explorer-2 的搜索覆盖了 confluent.io、learn.microsoft.com、tetrate.io、spring.io、kagent.dev、medium.com/@hiondal、arxiv.org、anthropic.com、launchdarkly.com、langchain.com、langwatch.ai、braintrust.dev、agenta.ai 等来源，各结论均标注了出处。
