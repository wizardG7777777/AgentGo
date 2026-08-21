# SWE/集成测试问题清单 — 2026-08-21 02:09（第四轮 Flask SWE 复测）

> 管理约定见首份文档 `2026-08-20-1508-swe-round3-open-issues.md`。本文登记
> 第四轮批测（阶段 1+2 修复回归）新暴露的问题，并更新旧 ID 状态。

## 背景

第四轮 8 题 Flask SWE 批测（Nebius `deepseek-ai/DeepSeek-V4-Flash`，全角色统一，
禁网），成绩 **4/8 resolved**（ipv6-server-name 140s、secret-key-rotation 130s、
context-push-order 333s、ipv6-session-txn 511s——全部为真实补丁、零篡改、
judge 基线对照确认）。完整数据见 `/tmp/agentgo-swe/report.md` 第四轮章节。

阶段 1+2 修复回归**全部通过**：v2 图契约生效（校验器引导迭代修图）、静态
kind 路由（0 次模板 provision）、直答首击 100% 拒绝、图节点 nudge 生效、
0 图僵尸 / 0 writeback-failed / 0 篡改 / 0 级联取消、verifier 真实参与验收。

本轮失败全部收敛为两个新模式（P0）+ 两个已知域（提示词专场）。

---

## SWE-008 二次直答「确认放行」成为系统性逃逸后门（P0，**fixed(2026-08-21)**）

**现象**：SWE-001 设计的确认逃生口（纯文本收口首击拒绝、二次「经确认放行」
并落盘 text-only 报告）在弱模型面前沦为默认逃逸路径。同一模式三题复现：

| 考题 | 触发前奏 | 首击 | 二次 | 落盘内容 | 结果 |
|---|---|---|---|---|---|
| teardown-callbacks | submit_graph JSON 偏移 179 损坏 | 拒绝 | 纯文本→放行 | DSML 残片（30 字节） | 3 调用/patch=0 |
| pass-context-dispatch | DSML 工具名畸形（981 rune） | 拒绝 | 纯文本→放行 | 一句中间思考（143 字节） | 5 调用/patch=0 |
| automatic-options | submit_graph JSON 偏移 4272 损坏 + 历史压缩 | 拒绝 | 纯文本→放行 | —（另有 write_file 垃圾文件 `\nconstract note to user?? \n` 被 judge 计为 7 行「补丁」） | 11 调用 |

共同序列：**第一次工具失败 → 放弃结构化路径 → 纯文本 → 被拒 → 再纯文本
→ 放行下班**。3 题合计 19 次 LLM 调用、零有效产出，占本轮失败的 75%。

**根因**：确认放行只看「模型坚持第二次」，不审查内容质量、不区分任务性质。
对 scheduler 根任务而言，这等于「只要固执两次就可以不建图」。设计初衷是
防死锁的逃生口，现在成为成本最低的默认路径。

**取证修正（2026-08-21，turns.jsonl 逐轮还原）**：三题直答**没有一题是模型
故意逃逸**。turns.jsonl 显示模型每轮都「说」要重交图/调工具（teardown 的
"My JSON got corrupted last time. Let me resubmit a clean graph"），但工具
调用输出成了 `<semantics>`/`<｜DSML｜` 等原生/自造标记——OpenAI 兼容层
不认识，框架把「想调工具但格式崩盘」读成「纯文本直答」，确认放行把残片
当答复落盘。automatic-options turn 7 的 57K 字符幻觉（连项目名都记错）
证明模型此时已处格式紊乱状态。**SWE-008 与 SWE-009 是同一根因的两面**：
长 JSON 损坏 → 格式能力崩溃 → 误诊为直答 → 放行落盘垃圾。触发器是单次
completion 过长（7893 tokens），不是上下文总长（对照组 ipv6-server-name
4 次语义校验拒绝——JSON 结构完整——毫无崩坏迹象）。

**修复内容（2026-08-21 落地，全程无正文检测，判定全基于结构事实）**：
1. **三态状态机**（`internal/hook/builtin/scheduler_closure.go` +
   `internal/hook/natural_exit.go` + `internal/agent/agent.go`）：
   放行谓词 `exitCount≥1 且 toolFailed==false`——toolFailed 是本任务
   `ToolCallRecord.Success==false` 的存在性（账本机械事实、任务级单调、
   跨 attempt 累计）。纯问答（无失败记录）出口原样保留（首拒次放）；
   有失败记录时第二次拒绝改**格式提醒**（勿输出标记文本、以系统工具
   调用格式重新提交），第三次 `Retry` 交 agent 按 ErrRecoverable 换上下文
   （exitCount 清零重获梯度，MaxRetries 全局兜底）。report_done 的
   PreCall 路径刻意不加 toolFailed 前提——能调成 report_done 的模型
   格式能力未崩。
2. **未知工具回执清洗**（`internal/agent/tool_registry.go`）：非法工具名
   （store.IsWellFormedToolName 判定）回执不回灌原名，改
   `malformed:<sha前12>` 占位 + 格式提醒（与账本占位一致），切断垃圾名
   回灌污染；合法 typo 名保留 Did-You-Mean 建议。
3. **正文 DSML 检测明确废弃**：正文出现 `<｜DSML｜` 可能是合法内容
   （任务本身就是写/讨论这些标记），正则无法区分讨论与泄漏——崩盘
   识别只走 toolFailed 状态事实。

**验证**：`scheduler_closure_test.go` 三态矩阵（含失败记录中途出现）、
`natural_exit_review_test.go` Retry 回滚与 toolFailed 接线、
`tool_registry_test.go` 四类畸形名清洗 + 合法名建议不回归；全量 43 包绿。
有效性以下一轮批测为回归证据。

**残余敞口**：SWE-003②（流式早夭——生成中途检出 JSON 已崩/DSML 标记
即提前中断）是治本项，二期与 SWE-003③ 一并评估。

**取证**：三题 `system.log` 的 `纯文本收口被零证据审查拒绝` → `零证据直答
经确认放行（scheduler_direct_answer，纯文本路径）` 序列；`.agentgo/reports/
text_only_*.md` 内容；automatic-options 的 `git status` 垃圾文件名。

## SWE-009 模型 DSML 格式泄漏致工具调用崩盘（P0，**fixed(2026-08-21)**，框架缓解面落地）

**现象**：DeepSeek-V4-Flash 在工具失败/长上下文压力下退化为输出原生
`<｜DSML｜tool_calls>` 标记格式（而非 OpenAI function calling JSON）：
- pass-context-dispatch loop=2：tool_call 名字段被 DSML 残片污染成 981
  rune 畸形名（`grep_search>\n<｜DSML｜parameter…`），框架清洗为
  `malformed:b3ce4e57959a`（SWE-002 第 2 层正常履职）并回执「未知工具」，
  但模型此后陷入格式混乱（连续输出残缺 DSML 块），两轮后走 SWE-008 逃逸；
- teardown-callbacks 的 text_only 落盘内容同样是 DSML 残片。

**框架缓解面（2026-08-21 已落地，见 SWE-008 修复内容第 2/3 条）**：
未知工具回执不回灌畸形原名（malformed 占位 + 格式提醒），切断回灌
污染；正文 DSML 检测废弃（改 toolFailed 状态事实）。
**仍 open**：SWE-003② 流式早夭（生成中途检出 DSML 标记提前中断）——
治本项，二期与 SWE-003③ 合并评估。

**关联**：SWE-003（长 JSON/DSML 同源模型侧失控）、SWE-008（崩盘后的逃逸
出口，取证后证实为同一根因）。

## SWE-006 read-before-write 摩擦（P2，**fixed(2026-08-21，v8.1)**）

本轮未观察到新案例。session-access-tracking 出现相邻形态：worker 幻觉拼接
越界路径（`session-access-session-access-tracking.py`）被 path-boundary
正确拒绝——边界机制正常，模型路径幻觉属模型侧。

**修复内容**（v8.1 提示词专场落地）：`prompts/worker.md` 与
`prompts/swe/worker.md` 均新增「工具被系统拒绝（Gate、路径边界、
先读后写等）时，读拒绝原因、补救后重试，不要因一次拒绝放弃原定路径」
纪律；同场落地「打转自查」（反复读同批文件无新结论即提交当前最优或
blocked，针对四轮 session-access-tracking 的 145 轮空转）。有效性以下
一轮批测为回归证据。

## SWE-007 级联取消的实际影响（watch，维持）

本轮触发 **0** 次（四轮累计：一轮未统计、二轮 0、三轮 0、四轮 0）。
4 个 resolved 全部 quiet 干净终态。维持 watch。

## SWE-010 watchdog 告警对 Graph 编排路径不可达（P1，**fixed(2026-08-21)**）

**现象**（v8.1 专场摸排发现的机制事实）：watchdog 超时告警
（`model.EventWatchdogAlert`）在 `internal/scheduler/activator.go:260`
只翻译为无差别的 `BatchUpdateCh` 信号——无任务 ID payload，且只唤醒
`waitForBatchTerminal` 里阻塞的 legacy batch 等待路径。Graph 编排下
scheduler 由唤醒任务（graph_ended / graph-change-request）驱动，
**节点任务超时告警不会产生任何 scheduler 唤醒**：scheduler 既不知道
哪个任务超时、也不会被唤醒裁决。这就是四轮 session-access-tracking
worker 打转 145 轮/20 分钟无人介入的机制层原因（用户更早「有意让
scheduler 忽略警告」的共识，在机制上甚至是必然结果——信号根本到不了）。

**修复内容（2026-08-21 落地）**：
1. **接线**（`internal/scheduler/activator.go`）：EventWatchdogAlert 分支
   在保留 legacy BatchUpdateCh 信号的同时，发布 `__scheduler__` 唤醒
   任务——描述含幂等标记 `[watchdog-alert: <taskID>]` + 超时事实
   （graph 归属/运行时长/描述），同一超时任务已有未终态同标记唤醒时
   跳过（watchdog 一次性告警 + retry rearm 后的新告警是合法新唤醒）。
2. **裁决指引**（scheduler 提示词 v8.2「超时告警介入」）：查进展 →
   有实质推进继续等 / 无进展 send_message steer 收敛 / steer 后仍无救
   cancel_task 走失败路径；不得无限放任。
3. **前置验证**：节点任务 cancelled 经 `graphTerminalStatusOf`
   （`internal/bootstrap/graph_runtime.go:1095`）映射为节点 failed 并
   正常回填（既有测试覆盖，`TestGraphTaskResultCancelledStripsSuccessProtocolAndStructuredCarrier`
   等）——cancel_task 裁决不是空枪，无需机制补修。

**验证**：`activator_test.go` 唤醒任务发布/幂等/空 ID 防御三用例 +
提示词断言；全量 43 包绿。有效性以下一轮批测为回归证据（重点观察：
超时打转节点是否有人介入）。prompts/swe/worker.md 的「打转自查」纪律
（v8.1 已落地）是 worker 侧的互补缓解。

---

## 旧 ID 状态更新

- **SWE-001**：fixed(2026-08-20) → **部分回潮**：双闸的首击拒绝 100% 生效，
  但确认放行半闸成为新后门，回潮部分以 **SWE-008** 独立跟踪（SWE-001
  本体维持 fixed——它要解决的「绕过一切收口」已不再发生，现在是收口
  内部策略问题）。
- **SWE-002**：fixed(2026-08-21)，本轮 0 图僵尸 / 0 writeback-failed /
  畸形名清洗告警正常（pass-context-dispatch 981 rune 案例）。**回归通过**。
- **SWE-003**：① fixed；②③ open；新增邻接项 **SWE-009**。提示词侧已升级
  （v8.1）：「骨架图先行」从错误回执的被动建议升级为建图纪律（载荷超约
  8000 字符先交 root+end+首批节点骨架，patch_graph 逐次扩展）；四轮观测
  到的「收到建议不采纳」是否改善，以下一轮批测验证。
- **SWE-004**：五切片 fixed；本轮 v2 校验器 4 次拒绝后引导出合法图
  （ipv6-server-name）为最佳正面证据；**子问题 1（failed 边返工回边
  doctrine）fixed(2026-08-21，v8.1)**——失败路径默认指向可修复节点，
  failed → end 明确定性为放弃；残余子问题 3（end outcome 落账）不变。
- **SWE-005**：fixed，本轮 0 篡改、baseline/red_note 全量在位。**回归通过**。

## 新观察（不构成问题，供提示词专场参考）

- **worker 长程打转**（session-access-tracking，145 轮/2.26M tokens/红态=
  基线）：watchdog 告警化 + scheduler 有意忽略警告的设计取舍下，打转节点
  无任何外部介入直到 harness 挥刀。这是本轮唯一的 timeout 题。**这是已知
  敞口的首次完整取证**。worker 侧「打转自查」纪律已随 v8.1 落地
  （prompts/swe/worker.md）；机制层原因与接线方案另列 **SWE-010**。
- **scheduler 亲自调查**：automatic-options / pass-context-dispatch 中
  scheduler 用只读工具自行 grep/read 调查 4–5 轮才尝试建图，大图 JSON
  损坏后即逃逸。**v8.1 已落地建图前调查纪律**（理解目标 ≠ 亲自调查仓库；
  需要先摸清仓库时提交调查开头的最小图，结果到达后 patch 扩展），有效性
  以下一轮批测验证。
- **verifier 误用被机械纠正**：verifier 把 verdict 放 result 顶层被拒
  （「系统保留键，请用同名专用参数」），随后正确使用——机械校验引导
  正确用法的正面案例。

## v8.1 提示词专场落地清单（2026-08-21）

| 改动 | 位置 | 对应问题 |
|---|---|---|
| 失败路径 doctrine（failed/blocked 出边默认返工回边或 repair；failed → end = 放弃） | `internal/scheduler/scheduler.go` 决策序第 3 条 | SWE-004 子问题 1 |
| 建图前调查纪律（理解目标 ≠ 亲自调查；调查开头的最小图 + patch 扩展） | 同上，统一 Graph 生命周期第 1 条 | 四轮 P2-D |
| 大图骨架先行（>8000 字符先交骨架再 patch） | 同上，制定图与更新图节 | SWE-003 提示词侧 |
| Gate 拒绝后补救重试 + 打转自查 | `prompts/worker.md`、`prompts/swe/worker.md` | SWE-006 + 打转 worker 侧 |
| 过期描述更正（watchdog 杀任务、plan_id 两处 V6 前残留） | `prompts/worker.md` | 文档卫生 |
| 版本 v8.0 → v8.1 + doctrine 短语断言 | `internal/scheduler/scheduler.go`、`scheduler_test.go` | — |

验证：全量 43 包 go test 绿、config doctor 0 错误、二进制冒烟（healthz
200、3 静态 kind ×8 runner 装配、系统就绪）。**提示词变更的有效性须以
下一轮 8 题批测为回归证据**（重点观察：scheduler 是否还亲自调查、大图
是否骨架先行、失败路径是否带返工、worker 打转是否自愈）。

## 阶段 4 落地清单（2026-08-21）

| 改动 | 位置 | 对应问题 |
|---|---|---|
| 直答/崩盘三态状态机（toolFailed 状态事实，无正文检测） | `internal/hook/builtin/scheduler_closure.go`、`internal/hook/natural_exit.go`、`internal/agent/agent.go` | SWE-008 |
| 未知工具回执清洗（malformed 占位 + 格式提醒，不回灌原名） | `internal/agent/tool_registry.go` | SWE-009 缓解 |
| **上游工作记录摘要**（转移结算时按来源 Task 聚合 ToolCallRecord，随 EdgeInput 冻结注入「## 上游输入」段——假成功的机械探测器；结算时冻结不违反「发布时禁止按 task_id 回查」红线） | `internal/graph/types.go`、`runtime.go`、`internal/bootstrap/graph_worklog.go`、`graph_runtime.go` | 用户设计（下游感知上游） |
| 介入闭环提示词 v8.2（下游上报指引 + Scheduler 四档裁决：返工→修正返工→controller 补位→降级失败） | `internal/scheduler/scheduler.go`、`prompts/worker.md`、`prompts/swe/worker.md`、`prompts/swe/verifier.md` | 介入闭环 |
| watchdog → Scheduler 唤醒接线 + 超时告警裁决指引 | `internal/scheduler/activator.go`、`scheduler.go` v8.2 | SWE-010 |

验证：全量 43 包 go test 绿（新增三态矩阵/回执清洗/WorkLog 冻结链/聚合
渲染/唤醒任务幂等 20+ 用例）、go vet 干净、config doctor 0 错误（SWE
配置；config.example.yaml 的 verifier 预存错误不变）、二进制冒烟
（healthz 200、系统就绪）。**有效性须以下一轮 8 题批测为回归证据**。

## 阶段定义更新

| 阶段 | 内容 | 问题 | 状态 |
|---|---|---|---|
| 1 | harness 快修 | SWE-005 | fixed(2026-08-20)，四轮回归通过 |
| 2 | 收口与回填机制 | SWE-001、SWE-002 | fixed，四轮回归通过（SWE-001 回潮部分另列 SWE-008） |
| 3 | Scheduler 提示词重构 + 模型侧适应 | SWE-003②③、SWE-004 残余、SWE-006 | **v8.1 落地（2026-08-21）**：SWE-004 子问题 1、SWE-006、SWE-003 提示词侧 fixed；残余 SWE-003②③（LLM 层）与 end outcome 落账 |
| 4 | 直答后门封堵 + DSML 缓解 + watchdog 接线 + 上游摘要与介入闭环 | SWE-008、SWE-009、SWE-010 | **fixed(2026-08-21)**，待下轮批测回归 |
| — | 持续观察项 | SWE-007 | watch |
