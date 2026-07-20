# Agent Skill 调研报告：面向 AgentGo Scheduler 的设计启示

> **来源**：基于 explorer-3 对中文互联网"Agent 技能（Skill）定义与撰写"的广度调查（2026年6-7月）
> **上游任务**：c50cb70a-70ef-46f1-83f7-da52f76e6e9c
> **目标读者**：AgentGo 项目的 scheduler 开发者，用于指导"生成执行代理配置表"的设计决策

---

## 一、核心概念：什么是 Agent Skill

### 1.1 定义

**Agent Skill（智能体技能）** 是一种轻量级、开放格式的能力扩展模块，用于将**过程性知识**（领域专业知识、工作流、最佳实践）封装为 AI Agent 可发现、可加载、可复用的标准化单元。[来源: yeasy.gitbook.io 4.4节]

### 1.2 Skill 与 MCP 的本质区别（关键认知）

| 概念 | 比喻 | 解决什么问题 |
|------|------|-------------|
| **MCP** | USB 接口 / 驱动程序 | Agent **如何连接外部系统**（工具层面） |
| **Skill** | 软件应用程序 / 专家脑回路 | Agent **如何按正确步骤完成特定任务**（流程层面） |

[来源: datawhalechina/hello-agents Extra05]

> **对 AgentGo 的启示**：当前 AgentGo 的 `tool_profiles` 和 `internal/tools` 实现了 MCP 层的工具连接，但缺少 Skill 层的流程封装。Agent 的 `system_prompt_file`（如 `prompts/worker.md`）是目前最接近 Skill 概念的东西——一段注入到 Agent 上下文的专家指令。

---

## 二、Skill 的形式化定义（六元组模型）

腾讯云开发者社区的综述论文提出了 Skill 的六元组形式化定义：[来源: cloud.tencent.com/article/2680478]

```
Skill = <ID, I, O, P, Pre, Eff>
```

| 元素 | 含义 | AgentGo 现有对应 |
|------|------|-----------------|
| **ID** | 唯一标识符及元数据（名称、版本、适用范围） | `agent.kind`（如 "worker"、"explorer"） |
| **I** | 输入模式（参数及类型约束） | 任务描述中的参数字段（隐式，无类型约束） |
| **O** | 输出模式（状态变化与返回值） | TransferNote / SubmitResult（隐式约定） |
| **P** | 步骤计划（有向无环图 DAG） | system_prompt 中的 SOP 描述（非结构化） |
| **Pre** | 前置条件（激活所需的逻辑断言） | `event_type` 匹配（如 "explore"） |
| **Eff** | 效果描述（执行后对系统的影响） | `agent.description` 字段（仅用于 scheduler 派发判断） |

### 关键发现

当前 AgentGo 已具备六元组的**雏形**，但均以非结构化、隐式约定形式存在：

- `P`（步骤计划）散落在 system_prompt 的自然语言中，无法被程序解析
- `Pre`（前置条件）仅靠 `event_type` 字符串匹配，缺乏丰富的断言能力
- `Eff`（效果描述）依赖人工撰写的 description，scheduler 无法自动验证

---

## 三、Skill 的核心设计哲学：渐进式披露（Progressive Disclosure）

这是 Anthropic Agent Skills 规范中最值得 AgentGo 借鉴的设计机制。[来源: 人人都是产品经理 woshipm.com/ai/6332201.html]

### 三层加载模型

```
L1 元数据层    →  启动时加载 name + description（建立"全局认知地图"）
L2 指令层      →  匹配后加载完整 SKILL.md（注入领域思维框架和 SOP）
L3 资源层      →  按需加载脚本/参考文档（脚本代码不进上下文）
```

### 对 AgentGo 的映射

| Skill 层 | AgentGo 当前状态 | 改进方向 |
|----------|-----------------|---------|
| **L1** | `board_snapshot` 中的 `agent_capabilities` 和 `specialized_agents` | ✅ 已基本实现，scheduler 和 agent 都能看到"谁有什么能力" |
| **L2** | `system_prompt_file` 全文注入 Agent 上下文 | ⚠️ 当前是启动时全量注入，无"按需激活"机制；长 prompt 占用上下文窗口 |
| **L3** | 无 | ❌ 缺失——脚本和参考文档无法按需加载 |

> **核心建议**：AgentGo 的 scheduler 在生成配置表时，应考虑支持"按需激活"的二级指令加载（L2），而非将所有 system_prompt 全量注入。这可以显著节省上下文窗口。

---

## 四、Skill 的目录结构标准

Anthropic 的开放标准定义了如下目录结构：[来源: platform.claude.com/docs skills]

```
skill-package/
├── SKILL.md          # 必需 — YAML Frontmatter + Markdown 指令
├── scripts/          # 可执行脚本（Python/Shell，不进入上下文）
├── references/       # 参考文档（按需加载）
├── assets/           # 模板和静态资源
└── ...其他文件
```

### 对 AgentGo 的启示

当前 AgentGo 的 agent 定义分散在：
- `config.yaml` 的 `agents[]` 段（运行时配置）
- `prompts/*.md`（system prompt 模板）
- `internal/tools/`（工具实现）
- `internal/reactor/`（reactor 流程）

如果能将每个 agent kind 收敛到一个**自包含的 skill 目录**，将极大简化配置管理和版本控制：

```
skills/
├── worker/
│   ├── SKILL.md              # system_prompt_file 指向这里
│   ├── scripts/              # 可执行脚本
│   └── references/           # 参考文档
├── explorer/
│   ├── SKILL.md
│   └── references/
└── verifier/
    ├── SKILL.md
    └── scripts/
```

---

## 五、Skill 撰写方法论（三步法）

国内文章总结的最佳实践：[来源: 53ai.com/news/tishicijiqiao/2026051232794.html]

### 三步撰写法

1. **定角色**：给 Skill 明确的专家人设
   - AgentGo 现有：`prompts/worker.md` 开头`"你是一个执行代理（Worker）"`——已做到
2. **画流程**：将任务拆解为 SOP 步骤（DAG 或拓扑排序）
   - AgentGo 现有：worker prompt 中有"你的工作方式"段——是线性列表，非 DAG
3. **画红线**：明确"不能做什么"，防止幻觉
   - AgentGo 现有：worker prompt 中有"先读后写"红线、"禁止通信测试"反例——已做到
4. **勤复盘**：将 bad case 转化为新规则补充进 Skill
   - AgentGo 现有：无系统化机制

### 关键经验数据

来自 SkillsBench 论文（2026）：**模型现写的 Skills 平均没有稳定收益；聚焦型 Skill（2-3个模块，围绕清晰工作流）优于大而全的综合手册。**[来源: yeasy.gitbook.io 4.4节引用 SkillsBench 2026]

> **对 AgentGo 的启示**：不要试图写一个"万能 worker prompt"——应该按任务域拆分为多个聚焦型 Skill（如 `worker-code-review`、`worker-test-gen`、`worker-refactor`），每个 Skill 2-3 个模块。这与 AgentGo 当前 `kind` 体系兼容：可以扩展为 `kind + skill` 的组合选择。

---

## 六、各平台 Skill 设计对比及对 AgentGo 的参考价值

| 平台 | 核心特点 | 对 AgentGo 的参考价值 |
|------|---------|---------------------|
| **Anthropic Claude Skills** | 开放标准、渐进式披露、本地运行 | ⭐⭐⭐⭐⭐ 目录结构和加载机制可直接参考 |
| **Coze（字节跳动）** | 零代码、自然语言描述方法论、技能商店 | ⭐⭐⭐ 技能商店模式可启发未来的 Skill 共享生态 |
| **Dify** | 开源、可视化工作流编排（Canvas）、开发者可控 | ⭐⭐⭐⭐ Canvas 可视化编排思路可参考用于 reactor 设计 |
| **AutoGen（微软）** | 多 Agent 群聊编排、非 Skill 管理平台 | ⭐⭐ Agent 协作模式可参考，但 Skill 管理需自己实现 |
| **华为 AgentArts** | Skill 组件库、ZIP 制品包、资产广场 | ⭐⭐⭐⭐ 完整的 Skill 制品管理流程值得借鉴 |
| **嘉为蓝鲸 Agentic Ops** | 运维领域最完整的 Skill 实践、MCP Gateway + Skill 分层 | ⭐⭐⭐⭐⭐ 运维场景的 Skill 架构与 AgentGo 目标场景高度吻合 |

[来源: 53ai.com, adg.csdn.net, support.huaweicloud.com, canway.net]

---

## 七、运维/调度知识 → Agent Skill 的转化框架

### 7.1 五步转化框架

国内社区共识的"五步框架"：[来源: X/DtDt666]

| 步骤 | 含义 | AgentGo 中的落地方式 |
|------|------|---------------------|
| **1. 拆分** | 将工作流拆成单一职责的 skill / sub-agent | 按任务域定义新的 `kind`（如 `kind: worker-triage`） |
| **2. 编排** | 在主 skill 中用自然语言描述整个流程 | reactor YAML 或 scheduler 的编排逻辑 |
| **3. 存储** | 中间结果保存为本地文件，不在上下文传递大段内容 | AgentGo 已有文件系统操作能力，需在 prompt 中强化此规范 |
| **4. 分摊** | Sub-agent 之间只传文件路径，不传内容（控制上下文窗口） | 在 TransferNote 规范中强化"传路径不传内容" |
| **5. 迭代** | 每次踩坑后把经验和修复喂回 Skill | 建立 `prompts/*.md` 的版本管理和 badcase 复盘流程 |

### 7.2 已有生产级实践参考

**红帽（Red Hat）Agent Skills** — 运维技能包：[来源: redhat.com/zh-cn/agentic-skills]
- CVE 解析、诊断数据收集、问题单等级判定、产品生命周期核验
- 安装方式：引导 Skill → 从源 URL 拉取 → 安装到项目目录
- **启示**：AgentGo 可考虑支持远程 Skill 拉取（类似 `go get`）

**嘉为蓝鲸 Agentic Ops** — 国内运维最完整 Skill 实践：[来源: canway.net]
- 架构：CMDB/可观测/ITSM → MCP Gateway → AIDev（含 Skill 管理）→ 智能体生态层
- 目标：90%+ 日常运维由 AI 自主完成
- **启示**：MCP Gateway + Skill 的分层架构可直接参考

**Elastic Agent Skills** — 模块化设计：[来源: elastic.co]
- ES|QL 查询技能、仪表板构建技能、安全事件分流技能
- 效果：ES|QL 查询准确率从"猜语法"提升至"像工程团队一样使用标准语法"
- **启示**：聚焦型 Skill 能显著提升专业领域任务质量

---

## 八、DAG 驱动的调度知识 Skill 化

### 8.1 腾讯云综述论文的核心贡献

将 Skill 步骤计划（P 元素）建模为**有向无环图（DAG）**，支持依赖解析和并行执行。[来源: cloud.tencent.com/article/2680478]

这为 AgentGo 的 scheduler 提供了直接可用的理论框架：

```
当前状态：scheduler 生成线性任务队列（FIFO），依赖靠 TransferNote 隐式传递
目标状态：scheduler 生成 DAG 任务图，显式管理依赖和并行度
```

### 8.2 计划-技能耦合算法

- **正向验证（计划→技能）**：检查计划子目标与 Skill DAG 的结构兼容性
- **反向验证（技能→计划）**：确保后续计划不冲突

这对应 AgentGo 中 scheduler 在发布任务前应做的**前置条件校验**（当前仅靠 `event_type` 匹配）。

### 8.3 OceanBase 架构演进路径

```
Single Agent → Multi-Agent → Agent Skills → Agent Teams
```

AgentGo 当前处于 **Multi-Agent** 阶段（多个 worker/explorer 协作）。下一步 **Agent Skills** 意味着：将 agent 的 prompt 从"通用能力描述"升级为"领域专家技能包"。[来源: open.oceanbase.com/blog/26106899472]

---

## 九、对 AgentGo Scheduler 生成配置表的具体建议

### 9.1 配置表结构增强

当前 `board_snapshot` 包含 `agent_capabilities`（工具集列表）+ `specialized_agents`（kind 与工具映射）。建议增加 Skill 维度的字段：

```yaml
# 建议：agent 配置中增加 skill 字段
agents:
  - kind: worker
    skill: code-review          # 新增：关联的 Skill ID
    skill_package: skills/code-review/  # 新增：Skill 目录路径
    # ... 现有字段保持不变
```

对应的 `SpecializedAgent` 结构增强：

```go
type SpecializedAgent struct {
    Kind         string   `json:"kind"`
    Description  string   `json:"description"`
    Tools        []string `json:"tools"`
    SkillID      string   `json:"skill_id,omitempty"`      // 新增
    SkillVersion string   `json:"skill_version,omitempty"` // 新增
    DAG          []Step   `json:"dag,omitempty"`           // 新增：步骤计划
}
```

### 9.2 渐进式披露在 AgentGo 中的实现路径

1. **L1**：`board_snapshot` 中增加 `skill_id` + `skill_description`（不改现有逻辑）
2. **L2**：支持按 `skill_id` 动态加载对应的 SKILL.md（而非启动时全量注入 system_prompt）
3. **L3**：支持 `scripts/` 和 `references/` 按需加载（新增能力）

### 9.3 聚焦型 Skill 拆分建议

基于调研结论（2-3个模块优于大而全），建议将现有 Agent 拆分为：

| 当前 | 建议拆分 |
|------|---------|
| `worker`（通用工作代理） | `worker-code`（代码读写）+ `worker-shell`（命令执行）+ `worker-integrate`（子任务编排） |
| `explorer`（广度调研） | `explorer-web`（网络搜索）+ `explorer-code`（代码分析）+ `explorer-verify`（事实核查） |

每个聚焦型 Skill 对应一个专门的 `prompts/<kind>.md`，遵循三步撰写法。

---

## 十、总结与优先级建议

| 优先级 | 建议 | 预期收益 | 实现难度 |
|--------|------|---------|---------|
| **P0** | 为现有 agent 补充形式化的 I/O/Pre/Eff 描述（YAML 字段化） | scheduler 能更精准地匹配任务与 agent | 低 |
| **P1** | 实现渐进式披露 L2（skill 按需加载而非全量注入） | 节省 30-50% 上下文窗口 | 中 |
| **P1** | 按任务域拆分聚焦型 Skill（2-3 模块原则） | 提升专业领域任务质量 | 中 |
| **P2** | 支持 Skill 目录结构标准（SKILL.md + scripts/ + references/） | 提升可维护性和社区兼容性 | 中 |
| **P2** | Scheduler 生成 DAG 任务图代替线性队列 | 支持依赖解析和并行执行 | 高 |
| **P3** | Skill 制品包管理（远程拉取、版本锁定） | 支持 Skill 共享生态 | 高 |

---

> ⚠️ **时效性说明**：本报告基于 2026 年 6-7 月中文互联网公开资料。Agent Skills 标准仍在快速演进中，建议在关键设计决策前再次验证引用源的最新状态。
>
> **上游调查约束**：explorer-3 的搜索覆盖了 yeasy.gitbook.io、cloud.tencent.com、53ai.com、woshipm.com、redhat.com、canway.net、elastic.co、open.oceanbase.com 等来源，各结论均标注了出处。未找到"调度策略/Agent 配置表的 Skill 化"的直接讨论，DAG 驱动执行 + 计划-技能耦合算法是最接近的理论框架。
