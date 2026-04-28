# AgentGo V5 升级提案：Scheduler 汇报分层决策模型

> **版本**: V5-draft  
> **状态**: 📝 设计阶段，待讨论定稿  
> **提案日期**: 2026-04-26  
> **相关文档**: nextUpgrade_v4.md (§7 Hashline, §9 错误码, §11 v4 配置), tool-profiles.md  
> **影响范围**: Scheduler 系统提示词、report_done 工具语义、Explorer 输出规范、Board Snapshot 结构（可选）、trace 事件类型与 CLI 工具（§11）

---

## 1. 背景与动机

### 1.1 当前架构的汇报链路

在 V4 架构中，Scheduler 汇报 Explorer（或 Worker）调查结果给用户的链路如下：

```
Explorer 完成调查
  → SubmitResult(agentID, taskID, lastOutput)
  → task.Results[explorerAgentID] = "调查文本..."

Scheduler 被唤醒
  → SchedulerExecutor.Execute()
    → waitForBatchTerminal()          // 等所有子任务终态
    → BuildBoardJSON()                // 构造含 Results 的全局快照
    → history = append(snapshot)      // 注入 LLM context
    → LLM 生成 report_done(summary)   // Scheduler LLM 自由发挥
  → report_done 打印 summary + Artifacts 校对块
```

### 1.2 核心矛盾

这条链路存在三个隐性成本：

| 矛盾 | 表现 | 根因 |
|------|------|------|
| **Token 冗余** | Board Snapshot 是全量 JSON（所有任务、agents、session_history），即使只需要 1 个 Explorer 的结果 | 被动注入，无法按需裁剪 |
| **转述失真** | Scheduler LLM "理解"Explorer 输出 → "用自己的话重写" → summary | 二次转述必然丢失细节、改变原意、产生幻觉 |
| **延迟叠加** | 必须等 batch 终态 → 注入 snapshot → LLM 生成 summary → report_done | 至少多一轮 LLM 调用 |

### 1.3 关键观察：并非所有场景都需要"分析"

用户请求可分为两类：

- **"给我看调查结果"**：用户只想知道 Explorer 发现了什么，不需要 Scheduler 做额外解读
- **"分析这些调查结果"**：用户需要 Scheduler 综合、对比、提炼多个 Explorer 的发现

当前架构对两类请求一视同仁，都走"Scheduler LLM 重写"路径。这导致第一类请求付出了不必要的 token 和延迟成本。

---

## 2. 核心设计：两层汇报模型

> **V5 设计演进（2026-04-26）**：经过深入讨论，将原三层模型（Layer 1 透传 / Layer 2 引用 / Layer 3 综合）简化为**两层模型**。
>
> 核心洞察：**"直接透传"本质上是"结构化引用"的特例**——即 Scheduler 在引用 Explorer 原文时，不添加任何原创内容、不添加任何框架、不添加任何连接词，输出 100% 等于 Explorer 原文。
>
> 这样设计的优势：
> - 概念统一，无需代码层短路机制
> - Scheduler 始终参与，保留"把关者"角色
> - 统一走 LLM 路径，Trace/Hoo/可观测性完整保留
> - Token 成本可控（LLM 只需"复制"而非"生成"）

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 2: 结构化引用（Structured Reference）                  │
│  Scheduler LLM 基于 Explorer 原文构建报告                     │
│  责任归属：Explorer 对内容准确性负责，Scheduler 负责呈现形式   │
│                                                              │
│  子模式 2A — 纯复制（Passthrough）：                         │
│    单 explore 任务，结果清晰完整，无需任何加工                │
│    Scheduler 输出 = Explorer 原文 + 极简 framing（来源标注）  │
│    成本：极低 completion tokens（LLM 只需复制，无需思考）    │
│                                                              │
│  子模式 2B — 分节引用（Sectioned Reference）：               │
│    多 explore 任务，各调查独立主题，无矛盾                    │
│    Scheduler 输出 = 分节标题 + 各节原文逐字复制              │
│    成本：低 completion tokens（标题 + 分隔符）               │
├─────────────────────────────────────────────────────────────┤
│  Layer 3: 综合分析（Synthesis）                              │
│  Scheduler LLM 深度理解 → 原创总结 → 生成连贯 narrative     │
│  责任归属：Scheduler 对内容准确性负责                        │
│  适用：多 Explorer 矛盾需调和、因果推断、用户要求"给我分析"   │
│  成本：高 completion tokens（需要理解、提炼、重写）          │
└─────────────────────────────────────────────────────────────┘
```

> **设计原则**：默认走 Layer 2（最低成本），只有触发 Layer 3 条件时才升级。Layer 2 内部，单 explore 场景走 2A，多 explore 场景走 2B。

---

## 3. 决策权归属：交给 Scheduler LLM

### 3.1 为什么不走代码层硬路由

代码层硬路由（如 `shouldPassthrough()` 启发式）有两个致命缺陷：

| 缺陷 | 示例 |
|------|------|
| **丢失语义** | 代码层看到 `"description":"查一下A模块架构"` 和 `"description":"分析一下A模块架构设计是否合理"`，无法区分"查询"和"分析" |
| **冻结策略** | 一旦启发式写死，后续调整必须改代码发版 |

Scheduler LLM 拥有**完整的决策上下文**：用户原始请求语义、`task.Results` 内容、结果间矛盾检测、任务元数据。让信息最多的一方做决策是正确选择。

### 3.2 为什么不做代码层短路

经过深入分析，**代码层短路（跳过 LLM 调用）被否决**。原因：

| 维度 | 代码层短路 | 统一走 LLM 路径（当前方案） |
|------|-----------|---------------------------|
| **实现复杂度** | 高（需改 Executor、状态管理、FinalizationNotifier 协调） | 低（只改 Prompt） |
| **风险** | 高（绕过 Scheduler 把关、状态不一致、Trace 缺失） | 低（Scheduler 始终参与） |
| **覆盖场景** | 窄（仅"单 completed explore + 无 Worker + 无 failed"） | 宽（所有场景） |
| **Token 节省** | 极大（跳过整轮 LLM） | 中等（completion tokens 减少） |
| **延迟节省** | 极大（跳过 LLM 调用） | 无（仍需 LLM 调用） |

**关键判断**：Token 和延迟的节省**不值得**引入代码层复杂性和风险。LLM "复制"原文的成本极低（见 §3.3 量化分析），统一走 LLM 路径是更干净的架构。

### 3.3 Token 成本量化分析

| 场景 | 当前（全 Layer 3） | 新方案（Layer 2 统一路径） | 变化 |
|------|-------------------|---------------------------|------|
| **Prompt tokens** | Board Snapshot ~4000 | Board Snapshot ~4000 | 不变 |
| **Completion tokens** | 生成原创 summary ~800 | 2A: 复制原文 ~50；2B: 标题+复制 ~150 | **下降 80-95%** |
| **总 tokens** | ~4800 | 2A: ~4050；2B: ~4150 | **下降 15-20%** |

**为什么 completion tokens 下降这么多？**

LLM 在生成原创文本时需要"思考"：理解 → 提炼 → 组织 → 措辞。这个过程消耗大量 completion tokens。

LLM 在"复制"原文时几乎是机械操作：读取 Board Snapshot 中的 Results → 按模板格式输出。不需要理解、不需要提炼、不需要创造。completion tokens 接近原文长度 + 少量 framing。

**为什么 prompt tokens 不变？**

因为 Board Snapshot 仍然需要注入（Scheduler 需要看到 Results 才能决定走哪一层）。但注意：
- Phase 2 可以优化 Snapshot 结构（§6.2.1）减少冗余
- 即使不改 Snapshot，prompt tokens 的成本也主要来自**input**，而 input token 价格通常远低于 output token

### 3.4 延迟分析

| 场景 | 当前（全 Layer 3） | 新方案（Layer 2） | 变化 |
|------|-------------------|------------------|------|
| **端到端延迟** | waitForBatch + Snapshot + LLM call(~2-3s) + report_done | 完全相同 | **无变化** |

**结论**：延迟不改善。但用户明确表示"如果可控可接受，就这么做"。

---

## 4. 硬规则式条件树（不用模糊词）

把分层决策从"偏好建议"改成"条件判断"：

```markdown
# report_done 调用前的强制检查清单

按顺序执行以下检查，满足任意一条则立即停止并确定层级：

检查 1：当前 SchedulerBatch 中，状态为 completed 的 explore 任务数量？
├─ 0 → 使用 Layer 3（综合分析）
├─ 1 → 进入检查 2
└─ 2+ → 进入检查 3

检查 2：[单 explore] 该任务的 Results 内容长度 > 0 且 Status == completed？
├─ 是 → 使用 Layer 2A（纯复制）
└─ 否 → 使用 Layer 3（失败或空结果需要解释）

检查 3：[多 explore] 阅读各 task Results，判断是否存在事实矛盾？
├─ 是 → 使用 Layer 3（矛盾必须调和）
└─ 否 → 使用 Layer 2B（分节引用）

违反以上层级判定规则的 report_done 调用会被系统拒绝。
```

**关键词约束**：
- ✅ "必须使用"、"不得改写"、"禁止添加"
- ❌ "优先使用"、"最好不要"、"强烈建议"

---

## 5. Layer 详解

### 5.1 Layer 2A：纯复制（Passthrough）

**定义**：Scheduler LLM 将 `task.Results[explorerAgentID]` 的原始字符串**逐字复制**到 `report_done` 的 `summary` 中，只添加极简的 framing（来源标注）。不添加任何解读、评论、衔接句。

**适用条件**（强制检查清单判定）：
1. 当前 batch 中只有一个 completed 的 explore 任务
2. 该任务的 Results 内容完整且可直接阅读

**system prompt 约束**：

```markdown
## Layer 2A（纯复制）的强制格式

当检查清单判定为 Layer 2A 时，summary 必须满足：

1. 以固定 framing 开头："以下为 explorer-[agentID] 的调查结果："
2. 紧接着必须是该任务 Results 字段的完整原文，逐字复制
3. 以固定 framing 结尾："（由 explorer-[agentID] 完成）"
4. 除此之外，summary 中不得出现任何其他字符
5. 不得添加任何解读、评论、衔接句、总结

示例：
```
以下为 explorer-7b52b232 的调查结果：

[此处必须是 task Results 的完整原文，一字不改]

（由 explorer-7b52b232 完成）
```

违反以上格式的 report_done 会被系统拒绝。
```

**校验机制**（`ValidateReportLayerHook`，§6.4）：

```go
func validateLayer2A(summary string, taskResult string) error {
    // 提取 framing 之间的内容
    // 校验是否与 taskResult 完全一致（逐字节）
    // 不一致 → 拒绝
}
```

**Token 成本**：
- Prompt tokens: ~4000（Board Snapshot）
- Completion tokens: ~原文长度 + 50（framing）
- 相比 Layer 3，completion tokens 下降 ~90%

### 5.2 Layer 2B：分节引用（Sectioned Reference）

**定义**：Scheduler LLM 为每个 explore 任务创建独立节，每节内容逐字复制对应任务的 Results 原文。可以添加节标题和极短的来源标注，但不得添加任何原创分析。

**适用条件**：
1. 多个 completed explore 任务
2. 各任务主题正交、无事实矛盾

**system prompt 约束**：

```markdown
## Layer 2B（分节引用）的强制格式

当检查清单判定为 Layer 2B 时，summary 必须满足：

1. 为每个 explore 任务创建独立节
2. 节标题格式：## [该任务 description 的前 10 个字符]（由 explorer-[agentID]）
3. 节内容：该任务 Results 字段的完整原文，逐字复制
4. 不得添加任何解读、评论、衔接句
5. 节之间用空行分隔

示例：
```
## 调查 docs/activate（由 explorer-7b52b232）
[此处必须是 task-xxx Results 的完整原文，一字不改]

## 探索 internal/tools（由 explorer-a1b2c3d4）
[此处必须是 task-yyy Results 的完整原文，一字不改]
```
```

**校验机制**：

```go
func validateLayer2B(summary string, exploreTasks []*model.Task) error {
    for _, task := range exploreTasks {
        for _, result := range task.Results {
            if !strings.Contains(summary, result) {
                return fmt.Errorf("Layer 2B 校验失败：summary 未包含 task %s Results 的完整原文", task.ID[:8])
            }
        }
    }
    return nil
}
```

### 5.3 Layer 3：综合分析（Synthesis）

**定义**：Scheduler LLM 全面理解所有 Explorer/Worker 的结果，进行综合、对比、提炼、因果推断，生成一段**原创的 narrative**。

**强制触发条件**（满足任一）：

| 条件 | 说明 |
|------|------|
| **A. 事实矛盾** | 两个 explore 任务对同一实体给出不同描述 |
| **B. 因果推断** | 用户请求含"为什么"/"原因"/"导致"，或结果间存在因果关联 |
| **C. 跨任务综合** | 需要把多个独立发现串联成更高层次结论 |
| **D. 含 Worker 任务** | batch 中有 Worker 任务（产出是文件修改，无法透传） |
| **E. 存在 failed 任务** | 需要向用户解释失败原因和影响范围 |
| **F. 用户明确要求分析** | 用户原话含"分析"/"对比"/"综合"/"评估" |

**责任边界**：
- Layer 2：Explorer 对内容准确性负责，Scheduler 负责呈现形式
- Layer 3：Scheduler 对内容准确性负责，因为它做了转述和推断

---

## 6. 超越 Prompt 的配套改动

Prompt 工程是起点，但要让分层模型**可靠地、可观测地**运行，需要在以下层面做配套改动。

### 6.1 工具层

#### 6.1.1 `get_task_result` 工具（暂不入计划）

> **决策（2026-04-26）**：暂不入实施计划，保留讨论空间。

**问题**：当前 Scheduler 只能通过被动的 Board Snapshot 获取 Explorer 结果。Snapshot 是全量 JSON，包含大量无关信息。

**设计**：
```yaml
get_task_result:
  description: "获取指定任务的 Results 内容"
  params:
    task_id: string  # 必填
    max_length: int  # 可选，截断到前 N 字符
  returns: "该 task Results 中各 agent 产出的拼接文本"
```

**保留讨论的理由**：
- 当前 Snapshot 读取全量结果未发现明显问题
- 如果 Phase 2 的 `completed_results` 优化足够好，按需读取的价值降低
- 未来触发条件：长时间 session 导致 Snapshot token 不可接受，或 Scheduler 同时管理 10+ 个 batch

**实现位置**：`internal/tools/scheduler.go` 新增工具，注册到 `SchedulerGroup`。

#### 6.1.2 `report_done` Schema 扩展（低侵入）

**设计**：增加可选的 `layer` 字段，让 Scheduler LLM **显式声明**自己走的是哪一层：
```yaml
report_done(
    summary: "...",
    layer: "layer_2a"  # 新增可选字段：layer_2a / layer_2b / layer_3
)
```

**价值**：
- 系统可以校验 `layer` 声明与实际 summary 内容是否一致
- 为后续统计和优化提供结构化数据
- 帮助调试

**实现**：`report_done` 的 schema 增加 `String("layer", ...)`，`reportDone()` 实现中增加校验逻辑。

---

### 6.2 数据层：Board Snapshot 与 Task 模型优化

#### 6.2.1 Board Snapshot 结构优化（中侵入）

**当前问题**：
- `taskSnapshot.Results` 嵌套在 `tasks[].results` 的深层路径中
- Snapshot 包含所有任务（包括与当前 batch 无关的已完成任务），token 冗余

**优化方案 A：增加顶层 `completed_results` 数组**

在 `boardSnapshot` 中增加一个扁平化的结果数组：
```go
type boardSnapshot struct {
    Mode              string                `json:"mode"`
    Trigger           triggerInfo           `json:"trigger"`
    Tasks             []taskSnapshot        `json:"tasks"`
    CompletedResults  []completedResult     `json:"completed_results,omitempty"`  // 新增
    Resources         resourceInfo          `json:"resources"`
    SessionHistory    []sessionEntry        `json:"session_history,omitempty"`
}

type completedResult struct {
    TaskID      string `json:"task_id"`
    AgentID     string `json:"agent_id"`
    EventType   string `json:"event_type"`
    Description string `json:"description"`
    Result      string `json:"result"`
    Preview     string `json:"preview,omitempty"`  // 前 200 字摘要
}
```

**价值**：
- LLM 可以直接扫 `completed_results` 找到需要的原文
- `Preview` 字段让 LLM 快速判断是否值得深入读取完整 Results

**优化方案 B：Snapshot 按需裁剪**

```go
// 只保留与当前 batch 相关 + 未完成的任务
var relevantTasks []*model.Task
for _, t := range tasks {
    if isInBatch(t.ID, batch) || !model.IsTerminal(t.Status) || isRecent(t, 5) {
        relevantTasks = append(relevantTasks, t)
    }
}
```

**价值**：长时间运行的 session 中，Board Snapshot 的 token 消耗大幅下降。

#### 6.2.2 Task 模型新增字段（低侵入）

```go
type Task struct {
    // ... 现有字段 ...
    ReportLayer string `json:"report_layer,omitempty"` // "layer_2a" / "layer_2b" / "layer_3" / ""
}
```

用于后续统计分析和 prompt 优化。

---

### 6.3 校验层：Hook 系统介入

#### 6.3.1 `ValidateReportLayerHook`（中侵入）

**设计哲学**：仿照 `ValidateExpectedHashHook`（Priority=20）和 `ValidateLineAnchorsHook`（Priority=25），新增一个专门校验 report_done 的 Hook。

**实现**：
```go
package builtin

type ValidateReportLayerHook struct {
    Store store.TaskStore
}

func (h *ValidateReportLayerHook) Priority() int { return 22 }
func (h *ValidateReportLayerHook) Phase() hook.Phase { return hook.PhaseToolPre }

func (h *ValidateReportLayerHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
    if hctx.ToolName != "report_done" {
        return hook.ToolHookDecision{Action: hook.Continue}
    }

    layer, _ := hctx.Args["layer"].(string)
    summary, _ := hctx.Args["summary"].(string)
    
    switch layer {
    case "layer_2a":
        return validateLayer2A(h.Store, hctx.TaskID, summary)
    case "layer_2b":
        return validateLayer2B(h.Store, hctx.TaskID, summary)
    case "layer_3":
        return validateLayer3(h.Store, hctx.TaskID, summary)
    default:
        inferred := inferLayer(h.Store, hctx.TaskID, summary)
        hctx.Args["layer"] = inferred
        return hook.ToolHookDecision{Action: hook.Continue}
    }
}
```

**Layer 2A 校验逻辑**：

```go
func validateLayer2A(s store.TaskStore, schedTaskID string, summary string) hook.ToolHookDecision {
    task, _ := s.GetTask(schedTaskID)
    // 找到唯一的 completed explore 任务
    var exploreResult string
    for _, id := range task.SchedulerBatch {
        t, _ := s.GetTask(id)
        if t != nil && t.EventType == "explore" && t.Status == model.TaskStatusCompleted {
            for _, r := range t.Results {
                exploreResult = r
                break
            }
        }
    }
    
    if exploreResult == "" {
        return hook.ToolHookDecision{Action: hook.Continue} // 无 explore，不校验
    }
    
    if !strings.Contains(summary, exploreResult) {
        return hook.ToolHookDecision{
            Action: hook.Abort,
            Error: fmt.Sprintf(
                "Layer 2A 校验失败：summary 未包含 explore 结果的完整原文。"+
                "Layer 2A 要求逐字复制，不得改写。缺失内容长度：%d 字符",
                len(exploreResult),
            ),
        }
    }
    
    return hook.ToolHookDecision{Action: hook.Continue}
}
```

**价值**：
- **强制约束**：LLM 无法绕过规则
- **自动纠错**：校验失败返回具体错误，LLM 在下一轮修正
- **可观测**：Abort 事件被 trace 记录

#### 6.3.2 `ReportLayerClassifierHook`（低侵入）

Post-call Hook，在 `report_done` 成功后自动记录层级信息。

---

### 6.4 配置层

#### 6.4.1 `report_layer_policy` 配置块（低侵入）

```yaml
report_layer_policy:
  default: "auto"
  layer_2a:
    enabled: true
    min_results_length: 50
    max_results_length: 8000
  layer_2b:
    enabled: true
  layer_3:
    forced_keywords: ["分析", "对比", "评估", "为什么", "原因", "说明"]
  validation:
    enabled: true
    strict_mode: false   # true = Abort; false = Warn
```

#### 6.4.2 运行时切换（中侵入）

通过 CLI 或 mailbox 消息在运行时切换策略：
```bash
$ agentgo --layer-policy=synthesis
/steer 从本轮开始，所有 report_done 必须使用 Layer 3
```

---

### 6.5 Agent 层：Explorer 输出规范

#### 6.5.1 结构化输出约定（低侵入）

> **决策（2026-04-26）**：不强制固定段落结构，改为要求 Markdown 结构正确、分点明确、来源可溯。

修改 `prompts/explorer.md`：

```markdown
# Explorer 输出规范

## 格式要求（强制）

1. **结构良好的 Markdown**：使用标题层级（## / ###）组织内容
2. **分点明确**：关键发现用列表（- 或 1.）分点呈现
3. **来源可溯**：每个 claim 必须标注来源工具及路径
   - 正确："模块 X 使用方案 Y（来源：read_file internal/x.go:12）"
   - 错误："模块 X 使用方案 Y"（无来源）
4. **不确定信息显式标记**：无法确认的信息用 [未验证] 标记
   - 正确："[未验证] 模块 Z 可能在 future 版本中移除"
   - 错误："模块 Z 将在 future 版本中移除"（无依据）

## 格式示例

```markdown
## 核心发现
- 模块 A 使用方案 X 处理并发（来源：read_file internal/a.go:34）
- 模块 B 依赖模块 C 的 v2 接口（来源：grep_search "import.*c/v2"）

## 详细分析
### 并发模型
[展开说明...]

### 依赖关系
[展开说明...]

## 限制
- [未验证] 方案 X 在高负载下的性能表现
- 未调查模块 D 的实现细节
```

## 输出长度控制
- 简单查询：不超过 200 字
- 中等调查：不超过 800 字
- 深度调查：不超过 2000 字
```

**价值**：
- Layer 2A 透传时，用户看到的是**有结构、有来源**的报告
- Layer 3 分析时，Scheduler LLM 可以通过 Markdown 结构快速提取关键信息（如只读标题和列表项）
- 比固定段落更灵活，比纯文本更有序

#### 6.5.2 输出长度控制（低侵入）

```markdown
## 输出长度控制

- 简单查询：输出不超过 200 字
- 中等调查：输出不超过 800 字
- 深度调查：输出不超过 2000 字

如果内容超过上限，优先保留"结论"段落的完整性，压缩"详细发现"。
```

---

### 6.6 可观测层

> **范围说明**：本节仅描述 v5 Scheduler 分层决策相关的可观测点。trace 系统的全面升级（含 v4 及更早历史回补：mailbox / hook abort 归因 / 失败原因分类 / 跨任务树形导航 / CLI 过滤跟随等）见 [§11 trace 系统升级](#11-trace-系统升级v5--历史回补)。本节列出的 3 个 EventKind 在 §11 中并入统一的事件清单。

#### 6.6.1 分层决策事件（低侵入）

```go
const (
    KindReportDone      EventKind = "report_done"
    KindLayerValidation EventKind = "layer_validation"
    KindLayerViolation  EventKind = "layer_violation"
)
```

#### 6.6.2 统计面板（中侵入）

```
=== Scheduler 汇报统计 ===
总 report_done 次数: 42
Layer 2A (纯复制):    18 (42.9%)  ── 校验通过: 17 | 校验失败: 1
Layer 2B (分节引用):  12 (28.6%)  ── 校验通过: 10 | 校验失败: 2
Layer 3 (综合分析):   12 (28.6%)

Completion tokens 节省: ~68%（相比全 Layer 3）
```

---

## 7. 渐进实施路线图

> 基于 Q1-Q3 的讨论结果，重新按 **P0（必须做）/ P1（有数据后决定）/ P2（暂不入计划）** 分级。
>
> **关键原则**：Hook 规则必须在"我们知道 Scheduler 不应当做什么"之后才能上。在此之前，只定义预期行为、收集数据、观察模式——不干预。

---

### P0：必须做（零代码，1 周）

#### P0-1：Prompt 工程验证（零代码，1-2 天）

**目标**：定义预期行为，不改任何代码。

**动作**：
1. 修改 `schedulerSystemPrompt`：
   - 增加 "report_done 的汇报策略分层" 段落（强制检查清单）
   - 增加 Layer 2A/2B/3 的正反面示例
   - 删除所有"优先"/"建议"/"倾向"等模糊词
   - **新增**：batch 中包含 Worker 任务时强制走 Layer 3 的规则
2. 修改 `prompts/explorer.md`：
   - 增加 Markdown 结构约束（标题层级、分点明确、来源标注、[未验证] 标记）
   - 增加输出长度控制

**成功标准**：
- Prompt 语义清晰、无歧义、可执行
- 无"用户看不到输出"的 regression

#### P0-2：观察期（零代码，3-5 天）

> **核心原则**：不干预，只观察。不上 Hook，不做校验，让 LLM 自由表现。

**目标**：收集足够多的真实行为数据，回答"Scheduler 在 prompt 约束下会做什么、不会做什么"。

**动作**：
1. 运行 **20-30 个典型任务**，覆盖以下场景：
   - 单 explore 简单查询（如"列出文件"）
   - 单 explore 中等调查（如"模块依赖关系"）
   - 多 explore 正交主题（如"A模块架构"+"B模块架构"）
   - 多 explore 存在矛盾（如"X用方案A"vs"X用方案B"）
   - batch 混有 Worker 任务
   - failed explore 任务
2. 对每次 `report_done`，人工或脚本记录以下数据：
   - Scheduler 声明的层级（从 summary 内容推断）
   - summary 是否包含 Results 原文（`strings.Contains` 检查）
   - summary 是否添加了原创分析
   - summary 的 framing 格式是否符合 prompt 要求
   - 用户反馈（是否满意、是否需要补充）
3. **不做的事**：
   - ❌ 不上 `ValidateReportLayerHook`（还不知道"不应当做什么"）
   - ❌ 不强制 `layer` 字段（还不知道 LLM 会如何声明）
   - ❌ 不做 Abort/重试循环（避免干扰自然行为模式）

**成功标准**：
- 收集到 **≥50 条 report_done 样本**
- 能够回答以下问题：
  - "LLM 在 Layer 2A 场景下，有多大比例会逐字复制原文？"
  - "LLM 最常见的'违规'模式是什么？"（如添加换行符、微调措辞、遗漏 framing）
  - "哪些边界场景 LLM 会误判层级？"
  - "prompt 中的哪些措辞最有效/最无效？"

**观察期结束后的决策点**：
```
如果 Layer 2A 合规率 >80%：
  → Hook 规则可以写得严格（逐字匹配）
  
如果 Layer 2A 合规率 50-80%：
  → Hook 规则需要宽松（允许 minor 差异，如换行符、空格）
  → 同时收紧 prompt
  
如果 Layer 2A 合规率 <50%：
  → prompt 需要重写
  → 考虑是否需要更极端的手段（如代码层辅助）
```

---

### P1：有数据后决定（低~中侵入，可选）

#### P1-1：校验层落地（中侵入，3-5 天）

> **前提**：P0-2 观察期数据充分，能够精确定义"不应当做什么"。

**目标**：让"逐字复制"从建议变成可强制执行的约束。

**动作**：
1. **基于观察数据定义 Hook 规则**：
   - 如果观察期显示 LLM 几乎 100% 逐字复制 → 规则：`strings.Contains(summary, result)` 严格匹配
   - 如果观察期显示 LLM 偶尔添加换行符 → 规则：规范化后匹配（trim space + normalize newline）
   - 如果观察期显示 LLM 偶尔微调措辞 → 规则：要求相似度 >95%（而非 100%）
2. 实现 `ValidateReportLayerHook`（Pre-call，Priority=22）
   - 规则来自观察数据，不是拍脑袋
3. 实现 `ReportLayerClassifierHook`（Post-call）
   - 记录每个 report_done 实际走的层级
4. 新增 `report_layer_policy` 配置块
   - `validation.enabled`：总开关
   - `validation.strict_mode`：true=Abort / false=Warn

**成功标准**：
- 误杀率 <5%（正确行为被 Abort 的比例）
- 漏杀率 <10%（违规行为未被检测的比例）

#### P1-2：数据层优化（低侵入，3-5 天）

**触发条件**：P0 阶段发现 Board Snapshot token 消耗过高（>5000 tokens）或 LLM 定位 Results 困难。

**动作**：
1. Board Snapshot 增加 `completed_results` 顶层数组
2. Task 模型增加 `ReportLayer` 字段
3. `report_done` schema 增加可选的 `layer` 字段

**不做的**：`get_task_result` 工具（Q2 决策：暂不入计划）

#### P1-3：可观测层完善（低侵入，3-5 天）

**触发条件**：需要量化数据来持续优化 prompt 和条件树。

**动作**：
1. Trace 系统新增事件类型：`KindReportDone` / `KindLayerValidation` / `KindLayerViolation`
2. CLI 增加分层统计面板
3. 基于统计数据调整条件树阈值

---

### P2：暂不入计划（保留讨论空间）

| 方向 | 说明 | 触发条件 |
|------|------|---------|
| **`get_task_result` 工具** | 按需读取特定 task 的 Results | Snapshot token 不可接受时 |
| **Explorer 结果强制落盘** | 系统自动将 Results 写入临时文件 | 需要文件系统级持久化时 |
| **`compose_report` 工具** | 显式组装报告结构 | Layer 2 校验通过率 <90% 时 |
| **混合模式（Worker + Explorer）** | Explorer 透传 + Worker 分析 | 实际场景出现后再评估 |

---

## 8. 与现有架构的兼容性

| V4 机制 | 与分层模型的关系 |
|---------|-----------------|
| **Hashline（§7）** | 正交。Explorer 的 `read_file` 输出带 hashline，Layer 2 透传时保留 |
| **错误码策略（§9）** | 正交。Explorer failed 时自动走 Layer 3 |
| **Tool Profiles** | 正交。Scheduler 仍拥有全量工具 |
| **Board Snapshot** | Phase 2 优化 Snapshot 结构 |
| **Hook 系统** | Phase 3 新增 `ValidateReportLayerHook` |

---

## 9. 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| Scheduler LLM 不遵守复制规则 | 中 | 中 | Phase 3 校验层强制约束 |
| Explorer 结构化输出增加 token | 中 | 低 | 不要求每个段落非空 |
| 校验过于严格导致频繁重试 | 中 | 中 | `strict_mode: false` 时 Warn 不 Abort |
| Board Snapshot 过滤丢弃 context | 低 | 高 | 保守过滤策略 |

---

## 10. 已决策问题

### Q1：Explorer 的结构化输出是否应强制？

**决策**：不强制固定段落，要求 Markdown 结构正确、分点明确、来源可溯。

**理由**：固定段落（结论/详细发现/未验证信息/限制）太限制 LLM 能力。替换为更宽松的约束：使用标题层级、列表分点、标注来源、标记未验证信息。这样既保留了结构清晰度，又给了 LLM 创作自由。

---

### Q2：`get_task_result` 工具是否必要？

**决策**：暂不入实施计划，保留讨论空间。

**理由**：当前 Snapshot 读取全量结果未发现明显问题。`get_task_result` 作为"按需读取"工具的价值，取决于未来两个因素是否触发：（1）长时间 session 后 Snapshot token 不可接受；（2）Scheduler 同时管理大量 batch。如果以上情况出现，可以回头再加。

---

### Q3：与 Worker 任务的交互

**决策**：
1. **复制仅限 Explorer**（纯文本产出），Worker（文件修改）不能复制
2. **batch 中包含 Worker 任务时，整批强制走 Layer 3**
3. 混合模式（Explorer 透传 + Worker 分析）暂不考虑，等实际场景出现后再评估

**额外方向（可选未来扩展）**：
Explorer 结果强制落盘——在 Explorer 完成时，系统自动将 `Results` 写入临时文件（如 `.agentgo/cache/explorer-results/{taskID}.md`），Worker/Scheduler 可通过 `read_file` 读取。优势：文本结果有文件系统持久化，可批量处理；风险：临时文件生命周期管理、与 Explorer "只读"角色的关系需要进一步明确。此方向作为 P2 保留，暂不实施。

---

### 决策影响总结

| 问题 | 决策 | 对架构的影响 |
|------|------|-------------|
| Q1 | 灵活 Markdown | Explorer prompt 无需死记硬背模板，降低认知负担 |
| Q2 | 暂不入计划 | Phase 2 删除 `get_task_result`，减少工具认知负担 |
| Q3 | Worker 强制 Layer 3 | 条件树增加硬性规则：batch 中有 Worker → 强制 Layer 3 |

---

### Q4：Hook System 何时上？

**核心原则**：当我们**不确定 Scheduler 不应当做什么**的时候，Hook System 不能急着上。

**讨论背景**：最初计划将 `ValidateReportLayerHook` 作为 P0（与 Prompt 同时上线），理由是"强制约束 LLM 行为"。但经过讨论后认识到，Hook 规则如果先于数据存在，就会成为"拍脑袋规则"——可能误杀正确行为，也可能写太松起不到约束作用。

**正确的顺序**：

```
Step 1: 定义预期行为（Prompt 工程）
    ↓
Step 2: 观察期（零干预，收集真实行为数据）
    ↓  回答"LLM 实际会怎么犯错"
Step 3: 基于数据定义 Hook 规则（精准匹配实际违规模式）
    ↓  规则来自观察，不是拍脑袋
Step 4: 上线 Hook（强制约束）
```

**如果 Hook 过早上会发生什么**：

| 场景 | 后果 |
|------|------|
| 规则太严（如"逐字包含"） | LLM 正确添加换行符 → 误杀 → 频繁 Abort→重试 → 用户体验差 |
| 规则太松 | 违规行为漏过 → Hook 形同虚设 |
| Prompt 与 Hook 打架 | Hook 拒绝 → 改 prompt → Hook 又拒绝 → 反复循环，无法收敛 |

**观察期的核心原则**（P0-2）：
- ✅ 改 Prompt 定义预期行为
- ✅ 跑任务收集样本（≥50 条 report_done）
- ✅ 记录 LLM 的"违规"模式（添加换行？微调措辞？遗漏 framing？）
- ❌ 不上 Hook
- ❌ 不做 Abort/重试
- ❌ 不强制 `layer` 字段

**观察期后的决策点**：

```
Layer 2A 合规率 >80%  → Hook 规则严格（逐字匹配）
Layer 2A 合规率 50-80% → Hook 规则宽松（允许 minor 差异）+ 收紧 prompt
Layer 2A 合规率 <50%   → prompt 重写，考虑更极端手段
```

**决策**：`ValidateReportLayerHook` 从 P0 降级到 P1-1，前提是 P0-2 观察期数据充分。

---

## 11. trace 系统升级（V5 + 历史回补）

> **版本子状态**：📝 设计阶段，与 §6.6 对齐。本节是 v5 范围内 trace 系统的**全面**升级，统一涵盖：
> - **v5 新增**：Scheduler 分层决策事件（§6.6.1 已述，本节并入）
> - **v4 历史回补**：Mailbox / Hook abort / Scheduler batch / Agent 生命周期 / 失败原因分类等当前 trace 留白
> - **CLI 升级**：过滤、跟随、JSON 导出、跨任务树形导航、跨任务异常检测

### 11.1 动机：现状盘点与历史故障映射

trace 当前作为**单任务 ledger** 已经成熟（15 个 EventKind、§11.8 S11 AST 不变量护栏、5 条 `detectAnomalies` 启发式、prompt dump 旁路），但作为**跨切面调试器**已跟不上 v4/v5 复杂度。多处历史故障在 trace 上根本不留痕：

| 历史故障 / 设计需求 | 当前 trace 是否覆盖 | 缺口性质 |
|--------------------|-------------------|---------|
| 2026-04-20 mail `chain_depth` always 0 | ❌ 无 mailbox 事件 | 整条子系统盲区 |
| §9.3 fail-fast 不变量（166 次重试事故） | ⚠️ `KindTaskFailed` 只有自由文本 `Reason`，分不清 4 条路径 | 失败原因塌陷 |
| ValidateLineAnchorsHook / ExpectedHashHook abort | ⚠️ 只表现为 `tool_result.error`，丢失 hook 名/优先级/phase | 决策点不归因 |
| Scheduler batch 起止与 board snapshot 决策 | ❌ 0 个事件 | v5 §6.6 主动承认这一缺口 |
| 级联取消（cancel_task / watchdog / user / cascade） | ⚠️ `KindTaskCancelled` 不区分来源 | 4 来源塌陷为 1 |
| Watchdog stale agent 排障 | ❌ 无 agent goroutine 生命周期事件 | 无法回放 |
| Roster 文件锁冲突 | ⚠️ 仅 `KindFileWriteQueued` 含等待事件，无 acquired/released | 半边事件 |
| 跨任务父子关系导航 | ⚠️ `published_by` / `depth` 字段已存但无 `tree` 命令 | CLI 缺位 |
| 机器可读导出（管道接 jq / 自动化分析） | ❌ list/show 只有 Unicode 表格 | CLI 缺位 |
| 2026-04-27 startup probe 与 provider fallback 不一致 | ❌ probe/fallback 决策都仅写 stdout/stderr，trace 全无 | 启动期决策盲区 |

> **启动期决策盲区——一个真实案例**
>
> 2026-04-27 排查"Explorer 报告 web_search 不可用"的实际事件链：
> 1. `setting.yaml` 配置 `search_api_provider: serper`，`search_api_key: ${SERPER_API_KEY}`，env 未设置
> 2. [internal/probe/web_search.go](internal/probe/web_search.go) `serperProbe` 检查 apiKey 空 → 报告 `web_search` Available=false
> 3. [internal/webtool/provider.go](internal/webtool/provider.go) `NewProvider("serper", url, "")` 同样检测 apiKey 空 → 但**静默回落 DuckDuckGo**，工具实际仍可用
> 4. Scheduler 看到 `unavailable_tools=["web_search"]` → 不派网络任务 → report_done 里告诉用户"web_search 不可用"
>
> 复盘时 `agentgo trace show <task>` 上**没有任何**关于这条决策链的事件——probe 结果走的是 `fmt.Printf`，fallback 决策走的是 `log.Println`，全部绕开了 trace。整条决策只能靠"再启动一次盯终端"才能复盘，体验极差。
>
> 这构成了一个新的盲区类别：**启动期工具健康决策**。§11.2 新增 `tool_health_probe` / `provider_fallback` 两条 EventKind 来覆盖。

**设计原则**：
- 这次升级一次性覆盖 v5 + 历史回补，避免拆 N 个小 PR
- 所有新增 EventKind 必须**同步扩展 §11.8 S11 AST 扫描清单**，让编译期不变量护栏覆盖新事件
- CLI 子命令优先**叠加**，不破坏现有 `list` / `show` 的兼容

---

### 11.2 新增 EventKind（含 §6.6.1 并入）

下表是统一后的事件清单，前 3 项与 §6.6.1 重复（v5 分层决策），后续为历史回补：

| Kind | 范围 | 关键字段 | 触发位置 | 历史/设计关联 |
|------|------|---------|---------|--------------|
| `report_done` | v5 | `agent_id`, `layer`, `summary_len`, `result_match` | `internal/tools/scheduler.go` | §6.6.1 |
| `layer_validation` | v5 | `layer`, `validator`, `passed`, `reason` | ValidateReportLayerHook | §6.3.1 |
| `layer_violation` | v5 | `layer`, `expected_len`, `actual_len`, `violation_type` | 同上 | §6.3.1 |
| `mail_sent` | v4 回补 | `from_agent`, `to_agent`, `chain_depth`, `message_kind`, `payload_len` | `internal/mailbox` send 路径 | mail chain_depth bug |
| `mail_received` | v4 回补 | `to_agent`, `chain_depth`, `message_kind`, `from_agent` | mailbox 投递回调 | 同上 |
| `hook_decision` | v4 回补 | `hook_name`, `hook_phase`, `hook_priority`, `tool_name`, `action`, `reason` | hook runner 在 Run 返回 Abort/Continue 后 | line_anchors / expected_hash 误判定位 |
| `scheduler_batch_start` | v4 回补 + v5 | `batch_id`, `batch_size`, `mode`, `trigger_type`, `tasks` | `internal/scheduler` 创建 batch 时 | v5 §6.6 决策可观测 |
| `scheduler_batch_end` | 同上 | `batch_id`, `completed_count`, `failed_count`, `cancelled_count`, `duration_ms` | scheduler `waitForBatchTerminal` 返回时 | 同上 |
| `agent_lifecycle` | v4 回补 | `agent_id`, `agent_kind`, `lifecycle_phase` (started/idle_timeout/exited), `idle_polls` | `agent.Run` 入口 / 退出 / IdleThreshold 触发 | watchdog stale agent 排障 |
| `roster_claim` | v4 回补 | `path`, `agent_id`, `action` (acquired/conflict/released), `holder_agent_id`（冲突时） | `internal/roster` TryClaim/Release | 现仅 file_write_queued |
| `tool_health_probe` | v4 回补 | `tool`, `provider`, `available`, `error`, `latency_ms` | [internal/bootstrap/bootstrap.go:345](internal/bootstrap/bootstrap.go#L345) `RunAll` 完成后逐条 emit | 2026-04-27 web_search 不可用排查 |
| `provider_fallback` | v4 回补 | `tool`, `requested_provider`, `actual_provider`, `reason` | [internal/webtool/provider.go](internal/webtool/provider.go) 每个 fallback 分支 emit | 同上，serper→DDG 静默回落 |

**字段命名约束**：所有新字段使用 `snake_case`，与既有 Event struct 一致（见 [internal/trace/event.go](internal/trace/event.go)）。

**特殊说明：启动期事件如何归档**
- 当前 `Writer.Emit` 要求 `event.TaskID != ""`，否则丢弃（[internal/trace/writer.go:62-65](internal/trace/writer.go#L62-L65)）
- 启动期事件没有 task_id，需要扩展 Writer 支持"系统级 trace 文件"（如 `_startup.jsonl` 固定文件名），或为启动期事件保留特殊 task_id（如 `__startup__`）
- 推荐方案：保留特殊 task_id `__startup__`，复用既有 Writer 路径，CLI `trace show __startup__` 即可查阅

---

### 11.3 既有 EventKind 字段扩展

不新增 Kind，**给现有事件加字段**——这是更轻的改动，但能解决最严重的几条历史 bug：

| 既有 Kind | 新增字段 | 取值 | 用途 |
|----------|---------|------|------|
| `task_failed` | `failure_class` | `unrecoverable` / `max_retries` / `panic` / `watchdog_timeout` / `unknown` | §9.3 fail-fast 路径区分（项目最大历史 bug） |
| `task_failed` | `error_code` | `model_not_found` / `invalid_api_key` / `insufficient_quota` 等 | §9.4 LLM 错误诊断映射的结构化记录 |
| `task_cancelled` | `cancel_source` | `user` / `cancel_task_tool` / `watchdog` / `cascade` | 4 种取消来源区分 |
| `task_cancelled` | `cancel_origin_task_id` | task_id（cascade 时为根触发任务） | 级联深度可溯 |
| `tool_result` | `aborted_by_hook` | hook name string | hook abort 归因（line_anchors / expected_hash 误判时直接定位） |
| `task_retry` | `retry_class` | `recoverable_error` / `max_loops` / `bad_response` | retry 触发条件区分 |

**向后兼容**：所有新字段 `omitempty`，老 trace 文件读取不破坏。

---

### 11.4 CLI 升级

#### 11.4.1 既有命令叠加 flag（不破坏兼容）

| 命令 | 新增 flag | 行为 |
|------|----------|------|
| `trace list` | `--json` | 输出 JSON 数组而非 Unicode 表格，可管道 jq |
| `trace list` | `--since=<duration>` | 仅列最近 N 时间内的任务（如 `--since=1h`） |
| `trace list` | `--status=<state>` | 仅列指定状态（completed/failed/cancelled/running） |
| `trace show <id>` | `--filter=<kinds>` | 仅展示指定 EventKind，逗号分隔 |
| `trace show <id>` | `--follow` | 类 `tail -f`，文件追加时实时打印 |
| `trace show <id>` | `--json` | 输出原始 JSONL 而非格式化文本 |

#### 11.4.2 新增子命令

| 命令 | 用途 | 实现要点 |
|------|------|---------|
| `trace tree <root_task_id>` | 沿 `published_by` / `parent_task_id` 链展开父子任务树 | 扫所有 .jsonl，按 `published_by` 反查；用 ASCII 树形输出 |
| `trace stats [--since=<dur>]` | 全局统计：fail-fast vs retry 比例、layer 分布、平均延迟、token 消耗、跨任务异常告警 | 聚合所有 .jsonl，输出表格 + WARNING 列表 |
| `trace tail` | 全局实时跟随（不限单任务） | 监控 dir，新文件/新行立即输出 |

#### 11.4.3 `trace stats` 输出示例

```
=== 全局统计（过去 1h）=================================
任务总数: 42  |  completed: 35  failed: 5  cancelled: 2

失败分类（task_failed.failure_class）:
  unrecoverable:    3  ← API key / model 错误，立即 fail-fast
  max_retries:      1
  panic:            1  ← agent.go:284 panic-recovery 触发
  watchdog_timeout: 0

Scheduler 汇报分层（v5）:
  layer_2a: 18 (42.9%)  validation_pass: 17 fail: 1
  layer_2b: 12 (28.6%)  validation_pass: 10 fail: 2
  layer_3:  12 (28.6%)

LLM 调用: 平均延迟 1834ms  prompt_tokens p50/p95: 3200/8100

=== 跨任务异常告警 =====================================
WARNING [FailFastViolation] task 7b52b232: failure_class=unrecoverable
        但同任务 retry_count=2，§9.3 不变量回归
WARNING [MailLoop] mail chain_depth 接近上限 (8/10)
        from=worker-3 to=worker-1 last 5min
```

---

### 11.5 跨任务异常检测器

`detectAnomalies` 当前仅在单任务文件内运行。新增**跨任务**检测器，注入 `trace stats` 输出：

| 检测器 | 触发条件 | 关联 |
|-------|---------|------|
| `RetryStorm` | 1 小时内 `KindTaskRetry` > 20 条 | 上游 LLM 抖动预警 |
| `FailFastViolation` | 单任务 `KindTaskFailed.failure_class=unrecoverable` 且同任务出现过 `KindTaskRetry` | §9.3 不变量回归告警（项目最大历史 bug） |
| `CascadeChain` | 单次 `cancel_task` 触发的级联取消深度 > 3 | 任务树结构异常 |
| `MailLoop` | `mail_sent.chain_depth` 达到 `MailChainMaxDepth` 的 80% | 邮箱死循环预警 |
| `HookAbortStorm` | 同一 `hook_name` 在 10 分钟内 abort > 5 次 | line_anchors 系统性失配，提示 LLM 行号哈希漂移 |
| `LayerViolationRate` | `layer_violation` / `report_done` > 20% | v5 prompt 失效信号 |

每个检测器以 fixture-based 单测保护（参考 [internal/trace/cli_test.go](internal/trace/cli_test.go) 的 fixture 模式）。

---

### 11.6 渐进路线

#### P0：事件回补（低侵入，1 周）

**目标**：所有历史故障 + v5 决策都能在 trace 上 grep 到。

**动作**：
1. 新增 9 条 EventKind（§11.2 后 9 项；前 3 项随 v5 §6.6 一起落）——含 7 条运行期事件 + 2 条启动期事件（`tool_health_probe` / `provider_fallback`）
2. 既有 6 个字段扩展（§11.3）
3. 在 [internal/agent/agent.go](internal/agent/agent.go) / [internal/mailbox](internal/mailbox/) / [internal/scheduler](internal/scheduler/) / [internal/hook](internal/hook/) / [internal/roster](internal/roster/) / [internal/bootstrap](internal/bootstrap/) / [internal/webtool](internal/webtool/) 接通 emit 点
4. Writer 扩展支持启动期事件归档（特殊 task_id `__startup__`，详见 §11.2 末尾说明）
5. **同步扩展 §11.8 S11 AST 不变量扫描清单**（[internal/agent/terminal_emit_symmetry_test.go](internal/agent/terminal_emit_symmetry_test.go)），保证 mailbox/hook/scheduler 的关键路径也被编译期不变量护栏覆盖

**成功标准**：
- 每条新 EventKind 至少 1 条端到端测试断言（仿照 [internal/agent/unrecoverable_failfast_test.go](internal/agent/unrecoverable_failfast_test.go) 的"不变量护栏"模式）
- §9.3 / mailbox chain_depth / hook abort 三类历史 bug 均能用单条 grep 命令复现验证
- 2026-04-27 web_search probe/fallback 不一致案例可用 `agentgo trace show __startup__` 一次性看完决策链

#### P1：CLI 与跨任务检测（中侵入，1 周）

**前提**：P0 已落地，事件覆盖完整。

**动作**：
1. `trace show` 增加 `--filter` / `--follow` / `--json`
2. `trace list` 增加 `--json` / `--since` / `--status`
3. 新增 `trace tree` / `trace stats` / `trace tail`
4. 实现 6 个跨任务检测器（§11.5）

**成功标准**：
- 4 个 CLI 子命令 + 6 个跨任务检测器全部有 fixture-based 单测
- 老脚本（管道 `tail -f` + jq）仍可用，无破坏性变更

#### P2：暂不入计划

| 方向 | 触发条件 |
|------|---------|
| JSONL → SQLite 索引 | trace 文件数 > 5000 或单文件 > 100MB |
| trace 与 [internal/session](internal/session/) replay 联动 | session replay 实际使用频率上升 |
| Web UI / Gantt 时间线 | CLI 满足不了多 worker 并发可视化时 |
| 跨进程聚合（多 agentgo 实例） | 出现集群部署需求时 |

---

### 11.7 与既有不变量的关系

#### §11.8 S11 AST 扫描（编译期）vs §11 trace 升级（运行期）

| 维度 | §11.8 S11 | §11 trace 升级 |
|------|----------|---------------|
| **阶段** | 编译期 / 单测 | 运行期 emit + 事后查询 |
| **目的** | 防止终结路径漏 emit | 让已 emit 的事件可查询、可关联、可告警 |
| **关系** | 正交。S11 守"必须 emit"，§11 让"emit 出来的事件有用" |
| **强约束** | 新增 EventKind 必须同步加入 S11 扫描清单（terminal_emit_symmetry_test.go） |

#### 与 §9.3 / §9.4 的协同

- §9.3 fail-fast 已在代码层修复，但**当前 trace 表达力不足以验证不变量**——`task_failed.Reason` 是自由文本，无法 grep 出"是否走了 fail-fast 路径"
- §11.3 新增 `failure_class=unrecoverable` 字段后，§9.3 不变量从"代码护栏 + 单测"升级为"代码护栏 + 单测 + 运行期可观测告警"（`FailFastViolation` 检测器）
- §9.4 的 7 条诊断映射，落地为 `task_failed.error_code` 结构化字段，后续可在 `trace stats` 中分布统计，发现哪个错误码最常见，反哺 prompt / 配置文档

---

## 13. 记忆系统与触发器重构（V6 方向）

> **状态**：📝 设计阶段，待后续讨论定稿
> **影响范围**：`internal/hook/agent.go` 重构、`internal/memory/` 新建、`internal/trigger/` 新建、`internal/agent/agent.go` 注入点迁移
> **前置阅读**：`docs/archived/rfc-go-rewrite.md` §2.1（ADK Memory / Trigger 设计意图）、`docs/archived/rfc-proactive-scheduler-and-event-system.md` §4.3（EventBus）

---

### 13.1 动机：当前 Agent Hook 的职责漂移

`internal/hook/agent.go` 在 Sprint 1（2026-04-12）落地时，Agent Hook 被实现为**上下文注入框架**（`TeamAwarenessHook` 的 `PhaseTaskStart` / `PhaseLoopPre` 注入团队快照、文件占用、目标锚点）。这与早期 RFC 中 "Trigger" 层（事件 → 动态拉起 Agent → 工作 → 销毁）的设计意图发生了漂移。

当前 Agent Hook 造成的三个架构负担：

| 负担 | 表现 | 根因 |
|------|------|------|
| **语义错位** | `AgentHook` 名字暗示 "Agent 生命周期 hook"，实际做的是 "LLM 上下文注入" | 命名与职责不匹配 |
| **职责过载** | `TeamAwarenessHook` 同时处理 TeamSnapshot + FileAwareness + GoalAnchor + token 预算截断 | 一个 hook 承载三种不同维度的感知信息 |
| **记忆缺失** | 所有 "注入内容" 都是临时文本，系统重启后项目知识全部丢失 | 没有持久化的 Memory 层 |

> **关键背景**：`InterfaceDesign.md` 明确标注 `#memory——本项目无对等的记忆持久化模块`。`TransferNote` 和 `LastHistory` 是任务级交接备忘，不构成真正的长期记忆。

---

### 13.2 核心设计决策

| # | 决策 | 说明 |
|---|---|---|
| **D1** | **Agent Hook 重新定位为 Trigger System** | Agent Hook 从 "上下文注入框架" 变为 "事件触发器"：事件匹配 → 选择 Agent 模板 → 构建 Task → `PublishTask` → 动态拉起临时 Agent → 任务完成后销毁 |
| **D2** | **上下文注入迁移到独立的 Memory System** | 新建 `internal/memory/` 包，负责长短期记忆的存储、检索、更新。Agent 从 Memory System 读取注入内容，而非从 Agent Hook |
| **D3** | **TeamAwarenessHook 拆分为 Memory System 的读写对** | TeamSnapshot / FileAwareness / GoalAnchor 不再作为 hook 注入，而是作为 `MemoryEntry` 被写入 Memory Store，再由 Agent 在 `processTask` 入口按需读取 |
| **D4** | **新增 Project Memory 持久化层** | 跨会话保留的记忆（项目约束文档、代码规范、常见错误模式），存储在 `.agentgo/memory/`；Session Memory 存储在 `.agentgo/sessions/sess-<id>/memory.jsonl`；Process Memory 保留在内存中 |
| **D5** | **常驻 Agent 与临时 Agent 共存** | `bootstrap` 启动的 Worker/Explorer/Scheduler 为常驻 Agent（与系统同生命周期）；Trigger System 拉起的 Agent 为临时 Agent（任务完成后 `IdleThreshold` 触发销毁） |

---

### 13.3 Memory System 架构

#### 13.3.1 作用域分层

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
| **Process** | 内存（map/slice） | 当前活跃 Agent 状态、board snapshot 缓存、实时文件占用 | 进程级 |
| **Session** | `.agentgo/sessions/sess-<id>/memory.jsonl` | 会话内积累的项目洞察、用户偏好、已确认事实、本次会话的约束调整 | Session 级 |
| **Project** | `.agentgo/memory/`（JSONL 或 SQLite） | 项目约束文档（"禁止直接操作 DB"）、代码规范、API 使用约定、常见错误模式 | 持久化 |

#### 13.3.2 记忆种类

```go
type MemoryKind string
const (
    KindConstraint   MemoryKind = "constraint"   // 项目级约束文档
    KindLearning     MemoryKind = "learning"     // 学习到的经验（失败/成功总结）
    KindPattern      MemoryKind = "pattern"      // 代码模式/项目结构洞察
    KindContext      MemoryKind = "context"      // 进程级上下文（TeamSnapshot 等）
    KindAgentState   MemoryKind = "agent_state"  // Agent 级状态快照
)
```

#### 13.3.3 存储接口

```go
type MemoryStore interface {
    // 写入
    Put(ctx context.Context, entry MemoryEntry) error
    // 文本检索 + 标签过滤
    Query(ctx context.Context, scope MemoryScope, kind MemoryKind, query string, limit int) ([]MemoryEntry, error)
    // 向量检索（可选，未来引入）
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

#### 13.3.4 Agent 侧的读取接入点

Memory System 的读取发生在 Agent 框架层，而非 Hook 层：

```go
// internal/agent/agent.go:processTask
func (a *Agent) processTask(ctx context.Context, taskID string) {
    // 替代原有的 runAgentInject(PhaseTaskStart)
    if a.Memory != nil {
        entries, _ := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext,
            "team_snapshot", 1)
        if len(entries) > 0 {
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
    Memory memory.MemoryStore // nil 时退化为 noop
}
```

---

### 13.4 Trigger System 架构

#### 13.4.1 与当前 Agent Hook 的根本区别

| 维度 | 当前 Agent Hook（注入器） | Trigger System（触发器） |
|---|---|---|
| **触发时机** | Agent 生命周期内部（TaskStart/LoopPre/LoopPost/TaskEnd） | 全局事件（EventChannel / Store 状态变更 / Cron / Webhook） |
| **行为** | 向 LLM history 注入文本 | 发布新 Task，动态拉起临时 Agent |
| **返回值** | `InjectContent string` | 无（副作用：`PublishTask` + `AgentPool.EnsureAgent`） |
| **Agent 生命周期** | 观察现有 Agent | 创建临时 Agent，任务完成后销毁 |
| **与 Memory 的关系** | 直接生成注入文本 | 读取 Memory System 辅助决策（可选） |

#### 13.4.2 TriggerRule 设计

```go
// internal/trigger/trigger.go

type TriggerRule struct {
    Name           string
    Enabled        bool
    EventType      string                    // 匹配什么事件（如 EventTaskCompleted）
    Condition      func(model.Event) bool    // 额外条件
    SelectAgent    func(model.Event) string  // 返回 Agent 模板名（如 "reviewer"）
    BuildPrompt    func(model.Event) string  // 构建任务描述
    SystemPrompt   string                    // 可选：覆盖默认 system prompt
    Priority       int
    MaxConcurrent  int                       // 最大并发数（防雪崩）
    DebounceSec    int                       // 防抖窗口
}

type Trigger struct {
    Store     store.TaskStore
    Rules     []TriggerRule
    AgentPool AgentPool        // Agent 工厂 + 生命周期管理
    Memory    memory.MemoryStore // 可选：读取记忆辅助决策
}

func (t *Trigger) HandleEvent(evt model.Event) {
    for _, rule := range t.Rules {
        if !rule.Enabled { continue }
        if !t.matches(rule, evt) { continue }
        if t.isDebounced(rule) { continue }
        if t.exceedsMaxConcurrent(rule) { continue }

        task := &model.Task{
            Description:  rule.BuildPrompt(evt),
            EventType:    rule.SelectAgent(evt),
            SystemPrompt: rule.SystemPrompt,
            EventSource:  "trigger:" + rule.Name,
            Priority:     rule.Priority,
        }
        t.Store.PublishTask(task)
        t.AgentPool.EnsureAgent(task.EventType) // 动态拉起
    }
}
```

#### 13.4.3 AgentPool：常驻 + 临时

```go
type AgentPool struct {
    // 常驻 Agent（bootstrap 创建，与系统同生命周期）
    Permanent map[string]*runner.Runner

    // 临时 Agent（Trigger 动态创建，IdleThreshold 到期后销毁）
    Ephemeral map[string]*EphemeralAgent
}

type EphemeralAgent struct {
    Runner       *runner.Runner
    TaskID       string
    CreatedAt    time.Time
    IdleThreshold int // 临时 Agent 设较小值（如 3），任务完成后自动退出
}
```

临时 Agent 生命周期：
```
Trigger 发布任务 → 临时 Agent 创建 → 认领任务 → processTask → SubmitResult → 空闲轮询 → IdleThreshold 触发 → 销毁
```

---

### 13.5 TeamAwarenessHook 迁移路径

当前 `TeamAwarenessHook` 的三个 section 需要拆分到不同位置：

| Section | 当前位置 | 迁移目标 | 理由 |
|---|---|---|---|
| **TeamSnapshot**（队友状态） | `PhaseTaskStart` / `PhaseLoopPre` 注入 | **Process Memory** 的定时写入 + Agent `processTask` 入口读取 | 团队状态是全局信息，不应在每个 Agent 的每轮循环里重复生成 |
| **FileAwareness**（文件占用） | `PhaseTaskStart` / `PhaseLoopPre` 注入 | **Process Memory** 的 Roster 监听写入 + Agent 按需读取 | Roster 变更时实时更新 Memory，Agent 读取缓存而非直接调 `ListClaims` |
| **GoalAnchor**（目标锚定） | `PhaseTaskStart` / `PhaseLoopPre` 注入 | **删除，由 Agent 自身维护** | `task.Description` 本身就是目标，不需要外部重复注入 |

迁移后 Agent 的 `processTask` 入口不再调用 `runAgentInject(PhaseTaskStart)` 获取 TeamSnapshot，而是从 `a.Memory` 读取：

```go
// 旧逻辑（Agent Hook 注入）
if injected := a.runAgentInject(ctx, hook.PhaseTaskStart, taskID, -1, false); injected != "" {
    history = append(history, HistoryEntry{IncomingMail: injected})
}

// 新逻辑（Memory System 读取）
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

### 13.6 当前设计对记忆扩展的约束清单

基于现有代码，Memory System 落地时需要正视以下约束：

| 约束 | 影响 | 缓解方案 |
|---|---|---|
| `AgentHookContext.Store` 是只读视图 | 旧 Agent Hook 无法写入记忆 | Memory System 独立存储，不依赖 Hook |
| `AgentHookResult.InjectContent` 是纯文本 | 结构化记忆需序列化 | Memory System 提供 `formatForLLM(entry) string` 统一序列化 |
| Agent 结构体无 Memory 字段 | Agent 无法直接访问记忆 | 新增 `Agent.Memory memory.MemoryStore`，nil 安全 |
| `HistoryEntry` 只有 `IncomingMail` | 记忆载体单一 | 保持 `IncomingMail` 作为统一注入载体，Memory System 负责格式化 |
| `TransferNote` 是任务级字符串 | 无法承载多维度跨任务记忆 | `TransferNote` 保留作为任务交接载体；跨任务记忆走 Memory System |
| Agent Hook 的 `PhaseLoopPost` / `PhaseTaskEnd` 是 Observe 类 | 无法注入内容，但可用于触发 Memory 写入 | Observe hook 可调用 `Memory.Put()` 做总结写入 |
| `AgentHook` 被要求无状态 | Hook 不能维护记忆缓存 | Memory System 是外部有状态服务，Hook 只持有客户端引用 |

---

### 13.7 与 V5 其他模块的关系

| V5 模块 | 与 Memory + Trigger 的关系 |
|---------|---------------------------|
| **Trace 系统（§11）** | `memory_put` / `memory_query` 可作为新的 EventKind 被 trace 记录；Trigger 的每次触发产生 `trigger_fired` 事件 |
| **Hook 系统（§6.3）** | Tool Hook 和 Mailbox Hook 保持护栏职责不变；Agent Hook 逐渐退化为 Trigger 或彻底移除 |
| **Board Snapshot** | Snapshot 中的 `resourceInfo` 可由 Process Memory 缓存提供，减少每次 `ScanAll` 的开销 |
| **Report Layer（§5）** | Layer 3 综合分析时，Scheduler 可读取 Project Memory 中的历史分析模式，辅助决策 |
| **EventChannel（§事件驱动）** | Trigger System 消费 EventChannel，与 Activator 并列但职责不同：Activator 服务 Scheduler，Trigger 服务动态 Agent |

---

## 12. 附录

### A. 术语表

| 术语 | 定义 |
|------|------|
| **Layer 2A (纯复制)** | Scheduler 逐字复制 Explorer 原文，只添加极简 framing |
| **Layer 2B (分节引用)** | Scheduler 为多个 Explorer 结果分节，每节逐字复制原文 |
| **Layer 3 (综合分析)** | Scheduler LLM 深度理解后原创总结 |
| **Board Snapshot** | `BuildBoardJSON()` 生成的全局任务状态 JSON |
| **ValidateReportLayerHook** | 校验 report_done 是否符合声明层级的 Pre-call Hook |
| **completed_results** | Board Snapshot 中扁平化的终态任务结果数组 |
| **Memory System** | 独立于 Agent/Hook 的记忆存储与检索层，支持 Process/Session/Project 三级作用域 |
| **Trigger System** | 事件驱动的动态 Agent 拉起机制，任务完成后自动销毁 |
| **Project Memory** | 跨会话持久化的项目级记忆（约束文档、代码规范、学习经验） |

### B. 参考代码位置

| 组件 | 文件路径 |
|------|---------|
| Scheduler 系统提示词 | `internal/scheduler/scheduler.go:99` |
| Explorer 系统提示词 | `prompts/explorer.md` |
| report_done 工具实现 | `internal/tools/scheduler.go:112` |
| Board Snapshot 构造 | `internal/scheduler/snapshot.go:172` |
| SchedulerExecutor | `internal/scheduler/executor.go:33` |
| 任务结果提交 | `internal/agent/agent.go:475` |
| LLM Executor | `internal/agent/llm_executor.go:99` |
| Hook 系统 | `internal/hook/` |
| Trace 系统 | `internal/trace/` |
| Agent Hook 框架 | `internal/hook/agent.go` |
| TeamAwarenessHook | `internal/hook/builtin/team_awareness.go` |

### C. 变更历史

| 日期 | 版本 | 变更 |
|------|------|------|
| 2026-04-26 | v0.1 | 初稿：三层模型（Layer 1/2/3） |
| 2026-04-26 | v0.2 | 重构：合并 Layer 1 到 Layer 2A（纯复制）；否决代码层短路；统一走 LLM 路径 |
| 2026-04-26 | v0.3 | 追加 §11 trace 系统升级：v5 分层决策事件 + v4 历史回补（mailbox / hook abort / scheduler batch / agent 生命周期 / failure_class / cancel_source）+ CLI 子命令（tree / stats / tail）+ 跨任务异常检测器；§6.6 收窄至 v5 范围并转引 §11；附录顺延为 §12 |
| 2026-04-27 | v0.4 | §11 增补"启动期决策盲区"：2026-04-27 排查 web_search 不可用时发现 probe（serperProbe 报 unavailable）与 webtool.NewProvider（静默回落 DDG）决策不一致，决策链全部走 fmt.Printf / log.Println，trace 完全无痕；§11.1 加表行 + 案例块；§11.2 增补 `tool_health_probe` / `provider_fallback` 两条 EventKind 与 Writer 的启动期归档约定；§11.6 P0 从 7 条 EventKind 升至 9 条 |
| 2026-04-29 | v0.5 | 新增 §13 记忆系统与触发器重构（V6 方向）：Agent Hook 重新定位为 Trigger System、独立 Memory System（Process/Session/Project 三级作用域）、TeamAwarenessHook 迁移路径、当前设计约束清单；附录 A/B 增补 Memory System / Trigger System / Project Memory 术语与参考代码位置 |
