# Explorer 重读浪费分析（2026-07-22）

状态：**修复已落地（2026-07-22 同日），首次复测暴露指标缺陷并已修正**。
方案 A（explorer 边读边记 prompt）、M1 结构化墓碑（snipStub）、
闸 1（read_file 缓存命中摘要 + `force_full`）、闸 2（截断元数据公告）
已全部实施；trace stats 的重读率检测**已按 path+offset 去重修正**
（此前把大文件顺序分页误计为重读，基线 72–87% 被高估；修正后同一批
历史任务为 34–61%，其中仍含重叠分页等真实浪费）。

## 1. 现象

2026-07-22 19:08–19:18 的并行调查测试（test-prompt-parallel-investigation.md，
3 个 explorer 调查任务 + 正式验收）全程无异常：零重试、零级联取消、零历史压缩
风暴、验收一轮通过（verifier 仅 2 次 LLM 调用）。但总消耗 861.6k tokens，
其中 84% 集中在三个 explorer 调查任务上：

| Task | Agent | LLM 调用 | prompt | completion | 合计 |
|---|---|---|---|---|---|
| ffd1c48a | scheduler-3a8efc5a（控制器） | 6 | 132.0k | 3.8k | 135.7k |
| b1b5f481 | explorer-1（调查 plan 包） | 19 | 247.5k | 5.5k | 253.0k |
| afb94b65 | explorer-1（调查 store 包） | 18 | 242.8k | 7.4k | 250.2k |
| 958e4aaf | explorer-1（调查 scheduler 包） | 19 | 218.0k | 4.6k | 222.6k |
| e4776b1e | verifier team（正式验收） | 2 | ~33k | — | ~33k |

## 2. 核心证据：重读循环

以 `b1b5f481` 为样本（`trace show b1b5f481`）：

- **19 个 loop 共发起 80 次 read_file，只涉及约 12 个文件**。
  `coordinator.go` 被完整读 3 遍 + 分页读 7 次；`store.go` 3 遍；
  `controller_authority.go` 4 遍；`acceptance.go` / `budget.go` /
  `runtime_summary.go` / `supersede_existing.go` 各 2–3 遍。
- **逐 loop prompt 震荡**：3.2k → 17.5k → 9.5k → 17.3k → 11.5k …（峰值仅 17.5k，
  无上下文膨胀），呈典型的"读入→被清→重读"锯齿。
- **每轮 completion 仅 84–321 tokens**（约 150 均值）：agent 阅读过程中几乎不写
  任何笔记；最后 loop 18 一次性输出 2735 tokens 的总报告（`tool_calls=0`）。

## 3. 机制链条

1. **Layer-1 压缩（`snipOldToolResults`，internal/agent）**：超过 keepRecent 的
   旧高输出工具页会被无 LLM 调用地清空。这是设计上"最便宜"的压缩层——但它
   对 read_file 结果和 run_shell 输出一视同仁。
2. **被清内容即"遗忘"**：explorer 不做中间笔记（每轮 completion ~150 tokens），
   文件内容被清后唯一的恢复手段是重新 read_file。
3. **重读成本复利**：每次重读不仅自身消耗，还会把内容再次注入后续每一轮
   prompt（直到再次被清）。19 轮 × 平均 13k prompt = 247.5k，其中粗估近一半
   来自重复读取——三个 explorer 任务合计约 250–350k 属于可避免开销
   （占本次运行总量的 30–40%）。

注意：这不是功能缺陷——任务正确完成、质量验收通过；这是 v4 时代
retry-budget 文档定义的"功能正确但资源浪费"类问题（见
docs/archived/retry-budget-and-transfer-note-dispatch-2026-04-25.md §遗留）。

## 4. 候选方案（按实施成本排序）

> 2026-07-22 修订：参考本地 Codex（codex-rs）源码调查后升级为"分层记忆 v2"
> （四层 + 两道闸），见 §4.5。原始四方案保留备查。

### 方案 A：prompt 层——边读边记笔记（成本最低，建议先行）

在 explorer 系统提示（prompts/explorer.md）中要求：每读完一批文件，先在回复里
写下结构化笔记（每个文件的核心事实 3–5 条），再继续读下一批。笔记是 assistant
消息，Layer-1 只清工具结果、不清 assistant 正文，因此笔记存活到最终报告。

- 优点：零代码改动，直接打断"被清→重读"循环；笔记还提升最终报告质量。
- 代价：每轮 completion 从 ~150 涨到 ~500–800 tokens，但远小于重读节省的 prompt。
- 风险：依赖模型遵从度；对非调查型任务无意义（prompt 按 kind 注入，不影响 worker）。
- 佐证：Codex 的 auto-compact 同样让模型自写 handoff summary（compact.rs +
  prompts/templates/compact/prompt.md），"模型自写压缩替身"是行业验证路线。

### 方案 B：压缩策略分层——read_file 结果豁免或宽限

Layer-1 对工具类型分级：read_file 这类"内容即资产"的结果给予更长的存活期
（或豁免 snip，只清 run_shell/grep 等高噪输出）；或按 kind 配置 keepRecent，
explorer 放宽。

- 优点：系统性解法，不依赖模型行为；我们的结构化工具让"按语义分级"成为可能
  ——Codex 没有专用 Read 工具（一切读取都是 shell 输出），做不到这一点。
- 代价：prompt 更大（逼近 compact_token_threshold 后触发 Layer-2 LLM 摘要，
  成本可能反而更高）；且 prompt 逐轮计费，全保留本身就是持续更贵的方案。
  需要实测阈值平衡。

### 方案 C：工具层——read_file 命中缓存时注入"内容已读过"提示

FileStateCache 已有按 path 的 LRU；read_file 重复命中时返回"该文件已在 loop N
读过（hash 未变），如需内容请回顾笔记/上一页"的短提示而非全文；确需全文时
提供显式逃生门（如 `force_full=true`）。

- 优点：直接掐灭重读的 token 成本。
- 代价：模型可能确实需要内容（笔记被清时）——需配合方案 A 才有安全网；
  改变工具语义，需评估对 require-read-before-write Gate 的 ReadSet 兼容（
  ReadSet 由 read-set-write Reactor 维护，不受影响，但需回归验证）。

### 方案 D：组合（A + C）

A 提供"笔记安全网"，C 提供"重读硬阻断"，两者互补。B 留作 A/C 效果不足时的后备。

### 4.5 分层记忆 v2（Codex 参照后的优化形态）

本地 Codex（codex-rs）源码调查（2026-07-22）给出三个可借鉴设计：

1. **截断是带元数据的公告**：Codex 截断工具输出时附加
   `Warning: truncated output (original token count: N)`（core/src/tools/context.rs），
   模型明确知道丢了什么、丢了多少。我们的 snip 占位符 `[已清空，内容过长]`
   没有路径、没有原大小、没有取回指引，模型的重读决策是盲目的。
2. **view-time 截断而非原地破坏**：Codex 的 ContextManager 在 `for_prompt()`
   组装时才裁剪历史条目（core/src/context_manager/history.rs:345-454），
   canonical 历史保留全文。我们的 `snipOldToolResults` 原地改写 history 切片。
   view-time 为将来"不读盘的回忆机制"（从 canonical 历史恢复）留路。
3. **auto-compact 的 handoff summary**：与我们 Layer-2 同构，佐证方案 A 路线。

优化后的分层结构（四层 + 两道闸）：

| 层/闸 | 机制 | 状态 |
|---|---|---|
| M0 原文热区 | 最近 keepRecent 条工具结果原样保留 | 现状 |
| M1 结构化墓碑 | snip 占位符升级为含 path/hash/原行数/首读 loop/取回指引的 stub | **新增** |
| M2 笔记层 | explorer 边读边记（方案 A），assistant 消息免疫 snip | 方案 A |
| M3 LLM 交接摘要 | Layer-2 compressHistory，与 Codex auto-compact 同构 | 现状 |
| 闸 1 · 注入端 | 缓存命中且 hash 未变 → 短 stub + `force_full` 逃生门（方案 C） | 方案 C |
| 闸 2 · 读取端 | 10000 字符截断 + offset/limit 分页；截断标记向 Codex 警告格式看齐（原行数/字符数 + 续读指引） | 现状小改 |

落地顺序：A（笔记）→ M1（墓碑）→ C（缓存 stub）→ 闸 2 标记增强。
可选演进：snip 改 view-time 裁剪（保留 canonical 全文），改动大，列为后备。
每一步用 `trace stats` 的重读率（同任务同 path read_file ≥ 2 次的占比）验证。

**实施记录（2026-07-22 全部落地）**：

| 项 | 落点 | 说明 |
|---|---|---|
| 方案 A | `prompts/explorer.md` + `internal/agenttemplate/prompts/explorer.md` | "边读边记"硬要求（笔记格式 / 只引用笔记与未清内容 / 禁重读） |
| M1 墓碑 | `internal/agent/agent.go` `snipStub` | 占位符升级为"工具 + 目标 + 原长度 + 取回指引" |
| 闸 1 | `internal/tools/local_read.go` `formatReadCacheStub` | 缓存命中且未变 → 摘要 stub；`force_full=true` 取全文 |
| 闸 2 | `internal/tools/local_read.go` 截断分支 | "已截断：本段原 N 字符；用 offset=K 续读（分页建议每次 200 行左右）" |
| Q3 检测 | `internal/trace/cli.go` `printStatsAnomalies` | 重读率 >30% WARNING；**已按 path+offset 去重修正** |
| 方案 B / view-time | 未实施 | 后备：A/C 实测不足时再评估 |

**首次复测（2026-07-23 01:1x，总消耗 ~950k tokens，较基线未降）的教训**：

1. **指标误报**：旧规则按 path 计数，把 `memory.go`（1769 行）这类大文件
   的顺序分页（offset=335→649→1007→…）误计为重读。修正后重读 = 重复
   全文读 + 相同 offset 重复分页。
2. **分页/上限错配（新发现的真实浪费）**：模型按 300–400 行/页请求，而
   10000 字符上限 ≈ 250 行，每页都被截断并产生重叠重读（offset=335 被
   请求两次）。已在 limit 参数描述与截断公告中加入"分页建议每次 200 行
   左右"（放宽表述，非硬规则）。
3. **闸 1 被模型先发制人绕过**：模型从墓碑/工具描述中学会 `force_full`
   后对 4 个文件预传该参数，stub 未真正省到 token。机制正常但依赖模型
   接受 stub——DeepSeek 倾向取全文，需继续观察。
4. **方案 A 遵从度弱**：未出现结构化笔记（completion 仍 150–700/轮），
   但最终报告明显更详细——指令似乎转化为"更仔细阅读"而非"记笔记"。

## 5. 开放问题（2026-07-22 已全部闭环）

1. ~~方案 A 的笔记存活是否属实？~~ **已确认属实**：`snipOldToolResults`
   （agent.go）只替换 `ToolResults[j].Content`，不碰 `AssistantContent`；
   snip 目标为 run_shell/read_file/grep_search/glob_search/get_task_result。
   注意 Layer-2 compressHistory 会将旧条目整体折叠（笔记也会被折进摘要），
   但触发阈值高得多。
2. ~~keepRecent 当前值与触发点？~~ **已查清**：默认 `keepRecent=3`
   （agent.go:643，可按 kind 配置 CompactKeepRecent），snip **每个 loop 都
   执行**（agent.go:1037）。这精确解释了锯齿曲线：每轮并行读 3–6 个文件，
   上一批内容约一轮后即被清。snip 无专用 trace 事件的观测缺口确认存在
   （暂未补，重读率 stats 检测已覆盖主要复盘需求）。
3. ~~重读率能否纳入 trace stats 异常提示？~~ **已落地**：同任务 read_file
   重读占比 >30%（总读取 >= 4 次）时输出 WARNING，实测立即标出三个
   explorer 任务（72–87%）。

## 6. 复现命令

```bash
./agentgo trace stats                 # session 总量与 per-task 消耗
./agentgo trace show b1b5f481         # 逐 loop prompt 震荡 + 80 次 read_file 明细
```

## 7. 附录：Codex（codex-rs）上下文机制速查（2026-07-22 调查）

Codex 没有专用 Read/Grep/Glob 工具，主代理与 explorer 子代理都通过
`exec_command` 跑 shell 命令（cat/head/rg/find）读文件，输出以
FunctionCallOutput 进入历史，随后经三层裁剪：

1. **单次输出截断**：`max_output_tokens`（默认 ~10k）+ 模型级 truncation_policy；
   截断后附加 `Warning: truncated output (original token count: N)`。
   （core/src/tools/context.rs:413-441）
2. **历史条目级截断（view-time）**：ContextManager 在 `for_prompt()` 阶段按
   `tool_output_token_limit` 裁剪每个旧工具输出；canonical rollout 历史保持完整。
   （core/src/context_manager/history.rs:345-454）
3. **auto-compact**：达到 `model_auto_compact_token_limit` 时，旧历史被替换为
   模型自写的 handoff summary（保留最近用户消息 ≤ 20000 tokens）；
   pre-turn / mid-turn / 手动 /compact 三种触发。
   （core/src/compact.rs:241-389；session/turn.rs:815-832；prompts/templates/compact/prompt.md）

与 AgentGo 的对照：我们的 Layer-1（snip）对应其第 2 层但是原地破坏式；
Layer-2（compressHistory）对应其 auto-compact；read_file 的 10000 字符上限
对应其第 1 层。结构化工具（read_file/run_shell 类型可区分）是我们独有、
Codex 不具备的分级保留基础。

## 8. 关联

- 计量基础：2026-07-22 浪费可观测化专项（trace stats / WASTED 口径 /
  TUI 顶栏 session 总计 / Hub 进程级 token 累加器）。
- 历史定义：docs/archived/retry-budget-and-transfer-note-dispatch-2026-04-25.md
  （"功能正确但资源浪费是一类独立维度的 bug"）。
