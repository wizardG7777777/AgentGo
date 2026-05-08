# AgentGo 幻觉引用验收审计报告

**验收官**: Kimi Code CLI (验收代理)  
**审计日期**: 2026-05-01  
**审计范围**: AgentGo 全系统（`internal/` 30 个包 + `prompts/` + `docs/`）  
**审计目标**: 验证 Agent 是否存在幻觉引用（hallucinated citations），是否仅凭预训练知识回答问题，进而判定整个机制是否行之有效。  
**测试状态**: `go test ./...` — 29/30 包通过，`agentgo` 根包因启动测试缺少 API Key 失败（非功能性缺陷）。

---

## 一、验收结论（ TL;DR ）

| 验收项 | 判定 | 置信度 |
|--------|------|--------|
| Agent 是否存在幻觉引用风险？ | **存在，且无法被系统自动拦截** | 高 |
| Agent 是否可能仅凭预训练知识回答？ | **可能，系统无强制检索机制** | 高 |
| 整个防幻觉机制是否行之有效？ | **部分有效——对「文件/任务」幻觉硬约束成熟，对「引用/知识」幻觉仅软约束** | 高 |

**综合判定：当前系统不满足「幻觉引用安全」验收标准。**

具体而言：
- ✅ **文件操作层面**的幻觉（捏造文件、越权写入、未读先写）有**多层硬约束**兜底，机制成熟。
- ✅ **任务依赖层面**的幻觉（编造任务 ID）有 **Hook 硬拦截**，已验证有效。
- ❌ **网络检索引用层面**的幻觉（编造 URL、凭预训练知识回答、引用未实际访问的页面）**没有任何代码级约束**，完全依赖 LLM 对 Prompt 的自我遵守。
- ❌ **零自动化测试**覆盖「事实性问题 → 必须调用 web_search → 必须引用实际来源」这一核心用户故事。

---

## 二、幻觉分类与防御矩阵

我们将 Agent 可能产生的幻觉按「作用域」分为四类，逐一验收：

### 2.1 文件操作幻觉（File-Level Hallucination）

**定义**：Agent 声称写入了某文件、或基于未读取的文件内容进行修改，而实际上并未发生。

| 防御层 | 机制 | 类型 | 状态 |
|--------|------|------|------|
| Prompt | worker.md "先读后写"红线 + "禁止凭空捏造" | 软约束 | ✅ |
| Hook | `RequireReadBeforeWriteHook` (PreCall, Prio=30) — 未 read_file 则 Abort write/edit | **硬约束** | ✅ |
| Hook | `EnforceExpectedArtifactsHook` (PreCall, Prio=35) — 写入路径必须匹配 expected_artifacts | **硬约束** | ✅ |
| Hook | `ValidateExpectedHashHook` (PreCall, Prio=20) — 乐观并发哈希校验 | **硬约束** | ✅ |
| Hook | `RecordArtifactHook` (PostCall, Prio=950) — 实际写入自动记录 | **硬约束** | ✅ |
| Framework | `checkExpectedArtifacts` — 任务终态前校验产物契约 | **硬约束** | ✅ |
| Framework | `buildSchedulerArtifactsReport` — report_done 时并列系统校验块 | **可审计** | ✅ |
| Trace | `KindFileWritten` / `tool_call` 事件 — 事后可审计 | 可观测 | ✅ |

**验收判定**：🟢 **通过**。该领域具备业内罕见的「Prompt + Hook + Framework + Trace」四层防御，且均有测试覆盖（`hook/builtin/*_test.go` 14+ 用例、`agent/p1_fixes_test.go` 回归锁）。

### 2.2 任务依赖幻觉（Dependency-Level Hallucination）

**定义**：Agent 在 `publish_task` 时引用不存在的任务 ID 作为依赖。

| 防御层 | 机制 | 类型 | 状态 |
|--------|------|------|------|
| Prompt | schedulerSystemPrompt "禁止占位符" + "先发布再引用" | 软约束 | ✅ |
| Hook | `DependencyValidatorHook` (PreCall, Prio=25) — UUID 正则 + Store 存在性校验 | **硬约束** | ✅ |
| Tool | `meta.go` 最简 `GetTask` 兜底（禁用 Hook 时仍生效） | **硬约束** | ✅ |

**验收判定**：🟢 **通过**。2026-04-13 真实多 Worker 测试中曾出现 `dependencies="task-part1,task-part2,task-part3"` 占位符幻觉，经四层防御修复后，2026-04-14 复测零复发（`KNOWN_ISSUES.md` 有完整记录）。

### 2.3 网络检索引用幻觉（Citation-Level Hallucination）⭐ 核心验收项

**定义**：Agent 在回答中引用未实际检索到的 URL、或凭预训练知识捏造事实并伪造成检索结果。

| 防御层 | 机制 | 类型 | 状态 |
|--------|------|------|------|
| Prompt | worker.md / explorer.md "每个 claim 必须对应来源 URL" + "[未验证]"标注 | 软约束 | ⚠️ |
| Prompt | "禁止凭推断填补信息空白" | 软约束 | ⚠️ |
| Prompt | "至少使用不同关键词执行 3 次独立 web_search" | 软约束 | ⚠️ |
| Framework | `web_search` / `web_fetch` 工具结果原样注入 LLM 上下文 | 数据流 | ✅ |
| Framework | 工具结果包含 URL + 来源域名 | 数据流 | ✅ |
| Hook / Framework | **无** — 没有 Hook 校验 LLM 输出中的 URL 是否出现在工具结果中 | — | ❌ |
| Hook / Framework | **无** — 没有强制要求 Agent 在回答事实性问题前必须调用 web_search | — | ❌ |
| Trace | `KindToolCall` / `KindToolResult` 记录工具名和结果长度，但不记录完整内容 | 可观测 | ⚠️ |
| Test | **零个**自动化测试覆盖该场景 | — | ❌ |

**关键发现**：

1. **无结构化引用注册表**：`web_search` 返回的 `SearchResult` 包含 URL，但 `FormatResults` 将其渲染为纯文本后注入 LLM。系统没有为每条结果分配引用 ID、没有维护「引用 ID → URL」的注册表、没有要求 LLM 以引用 ID 形式标注来源。LLM 完全自由发挥，可以声称"[来源: https://example.com/article]"而系统无从验证该 URL 是否真实出现在任何一次 `web_search` 或 `web_fetch` 结果中。

2. **无"搜索→获取→验证"链强制**：Prompt 要求"每个来源 URL 都必须实际通过 web_fetch 读取过"，但代码层没有任何机制强制执行。Agent 完全可以在调用一次 `web_search` 拿到 5 条摘要后，直接编造第 6 条 URL 并声称已交叉验证。

3. **LLM 可以无视检索结果**：`buildMessages` 将工具结果作为 `role: "tool"` 消息注入 OpenAI tool-calling 协议，这是标准做法。但**标准做法不保证 LLM 遵守**——当上下文被 Layer 1/2/3 压缩后，旧工具结果可能被截断或替换为 `[已清空，内容过长]`，此时 LLM 极易退化为预训练知识作答。

4. **已知 P1 缺陷（未关闭）**：`KNOWN_ISSUES.md` 明确记录了一个活缺陷：Scheduler 的 `report_done` 声称 "Worker-B/C read log.md"，而实际上 Worker 从未调用 `read_file`。这说明 **Action-level 幻觉**（声称做了某事而实际未做）没有任何拦截机制——现有的 `buildSchedulerArtifactsReport` 只校验 *文件写入*，不校验 *文件读取* 或 *网络检索* 行为。

**验收判定**：🔴 **未通过**。这是本次验收的核心失败项。系统在该领域仅有 Prompt 软约束，无任何硬约束或自动化验证。

### 2.4 预训练知识替代幻觉（Knowledge-Level Hallucination）⭐ 核心验收项

**定义**：Agent 被问到事实性问题（如"2025 年诺贝尔物理学奖得主是谁？"），未调用任何检索工具，直接凭预训练知识回答——且该知识可能已过时或错误。

| 防御层 | 机制 | 类型 | 状态 |
|--------|------|------|------|
| Prompt | worker.md "使用 web_search 搜索网络信息" | 软约束 | ⚠️ |
| Prompt | schedulerSystemPrompt "B类：简单只读操作 → 自己调 web_search / web_fetch" | 软约束 | ⚠️ |
| Framework | **无** — 没有机制强制 Agent 在回答事实性问题前调用检索工具 | — | ❌ |
| Framework | **无** — 没有后处理器检测 Agent 输出是否包含"疑似事实性声明但无前置检索" | — | ❌ |
| Test | **零个** E2E 测试验证该场景 | — | ❌ |

**关键发现**：

当前系统的 ReAct 循环终止条件是 `!result.ToolCalled`（LLM 本轮未调用任何工具）。这意味着：如果 LLM 选择**不调用** `web_search`，直接输出自然语言回答，系统会**欣然接受**并标记任务完成。没有任何环节会问："你真的检索过了吗？"

这与文件操作领域形成鲜明对比——在文件领域，`RequireReadBeforeWriteHook` 会 Abort 未读先写；在知识领域，**没有任何等价物**会 Abort "未搜先答"。

**验收判定**：🔴 **未通过**。

---

## 三、Trace 系统可审计性评估

Trace 系统是事后发现幻觉的关键基础设施。评估如下：

| 能力 | 状态 | 说明 |
|------|------|------|
| 记录每次 LLM 调用 | ✅ | `llm_call_start` / `llm_call_end` 含 token 数、工具调用数 |
| 记录每次工具调用 | ✅ | `tool_call` / `tool_result` 含工具名、参数、耗时、结果长度 |
| 记录工具调用的**完整结果内容** | ❌ | 仅记录 `ResultLen`；完整内容需启用 `--dump-prompts`（默认关闭） |
| 事后审计「Agent 是否调用过 web_search」 | ⚠️ | 可以 grep `tool_call` 中的 `"tool":"web_search"`，但无法确认 LLM 是否基于该结果作答 |
| 事后审计「Agent 引用的 URL 是否真实存在」 | ❌ | 需要人工比对 LLM 输出与 `--dump-prompts` 文件，无自动化工具 |

**结论**：Trace 系统提供了「工具调用发生与否」的可观测性，但未提供「工具结果内容」和「引用-结果映射关系」的可观测性。对于幻觉引用审计，当前 Trace 是**必要但不充分**的。

---

## 四、测试覆盖缺口

本次审计对全项目 30 个包的测试进行了扫描，关键发现：

| 测试类型 | 数量 | 说明 |
|----------|------|------|
| 文件操作 Hook 测试 | 14+ | `enforce_expected_artifacts_test.go` / `require_read_before_write_test.go` 等 |
| 依赖验证 Hook 测试 | 13 | `dependency_validator_test.go` 覆盖占位符/时序错误/混合合法等 |
| 历史压缩测试 | 6 | `token_truncate_test.go` / `compress_test.go` |
| 崩溃路径测试 | 5 | `diagnose_test.go` / `unrecoverable_failfast_test.go` / `terminal_emit_symmetry_test.go` |
| **幻觉引用预防测试** | **0** | ❌ |
| **强制检索验证测试** | **0** | ❌ |
| **引用-来源一致性测试** | **0** | ❌ |
| **E2E: 事实性问题 → 工具调用 → 引用验证** | **0** | ❌ |

**结论**：项目在「文件/任务」幻觉领域测试覆盖充分，但在「引用/知识」幻觉领域**完全缺失自动化验证**。Prompt 中的反幻觉指令没有对应的回归测试锁住。

---

## 五、与已知缺陷（KNOWN_ISSUES）的交叉验证

`docs/activate/KNOWN_ISSUES.md` 记录了 28/29 项已修复缺陷，其中与幻觉直接相关的历史缺陷：

| 缺陷 | 修复状态 | 与本次审计的关联 |
|------|----------|------------------|
| Worker 凭空捏造任务结果 | ✅ 已修复 | 文件操作幻觉，已关闭 |
| Worker 任务完成但无文件产出 | ✅ 已修复 | report-only 失败模式，已关闭 |
| Scheduler 首次发布使用虚假依赖 ID | ✅ 已修复 | 依赖幻觉，已关闭 |
| Scheduler report_done 不基于 Artifacts | ✅ 已修复 | 产物声明幻觉，已关闭 |
| 邮件级联爆炸 | ✅ 已修复 | 通信幻觉（代理间无限礼貌回复），已关闭 |
| **report_done action-level 幻觉** | ⏳ **未关闭** | **Scheduler 声称 Worker 读了某文件而实际未读——正是 citation-level 幻觉的同类缺陷** |

**关键发现**：`KNOWN_ISSUES.md` 中唯一未关闭的与幻觉相关的 P1 缺陷，恰好落在本次审计失败的「Action-level / Citation-level」领域。这印证了审计结论——该领域是系统的结构性薄弱环节。

---

## 六、根因分析：为什么引用/知识幻觉无法被拦截

### 6.1 架构层面的根本差异

| 领域 | 幻觉是否可被「验证」 | 原因 |
|------|----------------------|------|
| 文件写入 | ✅ 可验证 | 文件系统是唯一真实来源，`checkExpectedArtifacts` 可以直接 `stat()` |
| 任务依赖 | ✅ 可验证 | Store 是唯一真实来源，`DependencyValidatorHook` 可以直接 `GetTask()` |
| 网络检索引用 | ❌ 不可验证 | LLM 输出是自由文本，系统没有 NLP 后处理器解析引用标记 |
| 预训练知识 | ❌ 不可验证 | LLM 的内部知识库是黑箱，系统无法区分「检索结果」vs「预训练记忆」 |

### 6.2 设计权衡的合理性与代价

项目团队在文件/任务领域选择了「Hook 硬约束」路线，取得了显著成效。但在引用/知识领域，他们选择了「Prompt 软约束」路线。这一选择有其合理性：

- **引用解析需要 NLP**：验证 `"[来源: URL]"` 是否出现在工具结果中，需要正则/NER 解析 LLM 输出，增加系统复杂度。
- **预训练知识检测是开放问题**：业界目前没有任何 Agent 框架能可靠区分 LLM 的回答来源是检索结果还是训练数据。
- **Prompt 工程在简单场景下足够**：对于配合度高的模型（如 GPT-4o），Prompt 约束通常能有效降低幻觉率。

**但代价是**：系统无法对引用/知识幻觉给出任何机械保证，验收官无法签字确认「Agent 不会编造引用」。

---

## 七、改进建议（按优先级排序）

### P0 — 必须实施（阻断验收项）

1. **引用验证 Hook（CitationVerifierHook）**
   - 在 `agent.processTask` 的 `!result.ToolCalled` 终止路径上，增加一个可选的 PostCall Hook 或后处理器。
   - 解析 LLM 最终输出中的 `[来源: URL]` 标记，与本次任务历史中所有 `web_search` / `web_fetch` 返回的 URL 集合做交集校验。
   - 若发现 LLM 引用了未检索到的 URL，将任务标记为 `failed` 或注入 `<validation-feedback>` 要求重试。
   - **实现复杂度**：中等（~100 行正则解析 + URL 归一化 + 集合比对）。

2. **强制检索决策门（RetrievalGate）**
   - 对于明确的事实性问题（可通过关键词匹配或 LLM 分类器判断），在 `buildMessages` 或 `TaskExecutor` 层增加规则：若任务历史中无 `web_search` / `web_fetch` 调用，则拒绝接受 `!result.ToolCalled` 的终止，强制 LLM 至少调用一次检索工具。
   - **实现复杂度**：低（关键词规则）到高（LLM 分类器）。

3. **E2E 幻觉测试基线**
   - 至少编写 1 个端到端测试：构造一个「需要检索才能回答的事实性问题」任务，Mock LLM 返回「无 tool call 的纯文本回答」，断言系统将该任务标记为 `failed` 或触发 `RetryRollback`。
   - 再编写 1 个测试：Mock LLM 调用 `web_search` 后返回包含编造 URL 的回答，断言 `CitationVerifierHook` 拦截成功。
   - **实现复杂度**：低（利用现有 Mock LLM client 基础设施）。

### P1 — 强烈建议（提升可观测性）

4. **Trace 富化：可选记录完整工具结果**
   - 在 `Event` 结构中增加 `ResultExcerpt` 字段（如前 500 字符），或增加 `KindToolResultFull` 事件类型，使事后审计无需依赖 `--dump-prompts` 即可比对引用与来源。

5. **web_fetch 截断显式标注**
   - 在 `FetchURLWithMode` 返回的文本中，当发生截断时，在截断点前增加 `[内容已截断，原文超过 10000 字符]` 的显式标记，降低 LLM 因不知截断而产生误解的概率。

6. **SearXNG Source 字段修复**
   - `searxng.go` 当前将 `SearchResult.Source` 设为搜索引擎名（`r.Engine`），应改为结果域名（从 `r.URL` 提取），与其他 Provider 保持一致。

### P2 — 建议（长期优化）

7. **检索结果结构化引用 ID**
   - 将 `web_search` 结果渲染为带编号引用的格式（如 `[1] Title — URL`），并在 Prompt 中要求 LLM 使用引用编号而非自由文本 URL。后处理器只需验证编号是否在合法范围内。

8. **模型级引用对齐**
   - 对于支持 `citations` 字段的模型（如 Claude 3.5 Sonnet 的 `document` 工具、Gemini 的 `grounding`），在 `llm.Provider` 层增加引用对齐适配，让模型在 API 层面就承诺引用来源。

---

## 八、最终验收签字

| 验收维度 | 判定 | 说明 |
|----------|------|------|
| 文件操作幻觉防护 | ✅ **通过** | 四层硬约束，测试覆盖充分，已验证有效 |
| 任务依赖幻觉防护 | ✅ **通过** | Hook + Tool 双层校验，历史缺陷已关闭 |
| 网络检索引用幻觉防护 | ❌ **未通过** | 零硬约束，零测试，零自动化验证 |
| 预训练知识替代幻觉防护 | ❌ **未通过** | 无强制检索机制，LLM 可直接无工具作答 |
| Trace 可审计性 | ⚠️ **有条件通过** | 工具调用可观测，但结果内容需手动启用 dump |
| 整体机制有效性 | ⚠️ **部分有效** | 对工程幻觉（文件/任务）有效，对认知幻觉（引用/知识）无效 |

**验收官意见**：

AgentGo 项目在「工程可靠性」维度展现了极高的设计成熟度——Hook 系统、产物契约、Trace 审计、崩溃汇报等机制层层递进，文件操作和任务依赖领域的幻觉已被有效收敛。

但在「认知可靠性」维度——即 Agent 是否基于真实检索而非预训练知识作答、是否如实引用来源——系统仍停留在「Prompt 工程 + 信任 LLM」的初级阶段。这与当前业界整体水平一致（尚无 Agent 框架能机械保证引用真实性），但对于一个以「多 Agent 控制」为核心卖点的系统而言，**引用/知识幻觉的不可检测性是一个显著的架构缺口**。

**签字**：

> 本次验收 **有条件不通过**。建议在实施上述 P0 改进项（至少完成「引用验证 Hook」+「E2E 幻觉测试基线」）后，重新提交验收。
>
> — 验收官 Kimi Code CLI, 2026-05-01

---

## 附录：审计方法ology

1. **静态代码审查**：阅读 `internal/agent/`、`internal/hook/`、`internal/webtool/`、`internal/tools/`、`prompts/` 等核心文件，理解数据流和约束链。
2. **Prompt 审查**：提取 worker.md / explorer.md / schedulerSystemPrompt 中的所有反幻觉指令，评估其 enforceability。
3. **Hook 审查**：逐一审查 6 个 Tool Hook + 3 个 Mailbox Hook + 2 个 Agent Hook 的实现，判定其是否拦截幻觉行为。
4. **Trace 审查**：分析 `internal/trace/event.go` 的事件类型和字段，评估事后审计能力。
5. **测试扫描**：grep 全项目 `*_test.go` 中的 `hallucinat|cite|pretend|fake|fabricat|ground|verify` 关键词，统计覆盖度。
6. **已知缺陷交叉验证**：将审计发现与 `KNOWN_ISSUES.md` 中的历史缺陷逐一比对，确认修复状态。
7. **测试运行**：执行 `go test ./...` 验证系统基本稳定性。
