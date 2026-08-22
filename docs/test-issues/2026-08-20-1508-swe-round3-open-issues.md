# SWE/集成测试问题清单 — 2026-08-20 15:08（第三轮 Flask SWE 复测）

> 本文是 `docs/test-issues/` 的第一份文档，也是该目录的管理约定模板。
>
> **目录用途**：集中管理集成测试与 SWE 式真实项目测试中发现的问题，与
> `docs/activate/KNOWN_ISSUES.md`（仓库通用限制）分工——这里记录测试战役
> 暴露的、需要阶段化修复的具体问题，修复闭环后在后续文档中标记并归档。
>
> **约定**：
> - 文件名以日期时间开头：`YYYY-MM-DD-HHMM-<主题>.md`（本地时区）；
> - 问题 ID：`SWE-NNN`，跨文档稳定引用，修复状态在新文档中更新；
> - 状态：`open`（未动手）/ `in-progress` / `fixed(<commit 或日期>)` / `wontfix(原因)` / `watch`（不修，持续观察）；
> - 阶段（stage）：修复分批依据，见文末阶段定义；每完成一个阶段，重跑
>   8 题 Flask 批测（`/tmp/agentgo-swe/harness/run_all.sh`）作为回归证据。

## 背景

第三轮 8 题 Flask SWE 批测（Nebius `deepseek-ai/DeepSeek-V4-Flash`，全角色统一，
禁网），成绩 **2/8 resolved**（ipv6-server-name、secret-key-rotation）。
完整数据与逐题取证见 `/tmp/agentgo-swe/report.md` 第三轮章节。
本轮已验证上一批修复全部生效（watchdog 零误杀、占位图零出现、空响应守卫
仅触发 1 次未致循环、judge 篡改检测抓住 2 题、scheduler 终报如实承认失败）。

以下为本轮新暴露 + 历史遗留的全部未闭环问题。

---

## SWE-001 纯文本自然退出绕过一切收口（P0，阶段 2，**fixed(2026-08-20)**）

**修复内容**（兜底 + 预防三件套，当日落地）：
- **兜底 1（scheduler 根任务）**：零证据收口审查从 `report_done` PreCall 移到
  processTask 终态判别处（`internal/agent/agent.go` 自然退出分支），端口
  `hook.NaturalExitReviewer`（`internal/hook/natural_exit.go`，置于 hook 包
  避免 gate(test)→builtin→agent→gate 导入环），实现复用
  `SchedulerClosureHook` 并与 Gate 共享 confirmed 状态——report_done 与
  纯文本两路径语义一致（第一次拒绝要求确认，第二次放行记
  scheduler_direct_answer）；
- **兜底 2（图节点任务）**：纯文本退出 = 未提交——注入
  submit_task_result 提醒（`maxUnstructuredExitNudges=2`），超限按可恢复
  错误收口（`react_loop_exit:unstructured_exit`，重试耗尽即 failed）；
- **预防 2（契约钉住）**：收口契约由系统从 `GraphID/GraphNodeKind` 派生写入
  `TaskMemory.Constraints`（验收节点追加 verdict 契约），每轮随 Task
  Memory 渲染注入，压缩碰不到；
- **预防 3（重试记忆）**：`taskmem.ApplyAttemptEnd` 把 attempt 终止原因
  （有界 160 rune、去重）写入 Failures 尾部，重试接手可见「上次怎么死的」。

**验证**：新增 `internal/agent/natural_exit_review_test.go`（nudge→可恢复、
审查拒绝/放行、非图任务回归）、`scheduler_closure_test.go` 四个
ReviewNaturalExit 用例、`task_memory_contract_test.go`、`taskmem` 的
TestApplyAttemptEnd；5 个旧图任务文本收口测试更正为结构化收口路径
（Finalized 信号 / submit_result 工具调用）；全量 go test 绿 + 二进制冒烟
（healthz 200、Gate 注册含 scheduler-closure-review、config doctor 0 错误）。

**残余敞口**：图变更/replan 唤醒任务的「说明理由不改图」文本收口仍合法，
若图持续 failed/僵尸态无再唤醒机制——保留为开放子项，随 SWE-004 提示词
重构一并评估。

**现象**：模型返回「有文本、零工具调用」时 ReAct 循环以
`react_loop_exit:natural` 自然退出，任务直接 `task_completed`，文本被
`emitTextOnlySubmissionIfNoArtifacts` 落成正式交付。收口 Gate
（`scheduler-closure-review`）是 `report_done` 的 PreCall 工具 hook，
对这条零工具调用路径**结构性不可达**（Gate 本身接线/匹配均正常，两轮
graph=0 逃逸 session 中 Gate 日志为 0）。

**受害面**（三轮取证）：
- scheduler 直答：automatic-options（12 调用）/ teardown-callbacks（7 调用）
  submit_graph JSON 连续损坏后纯文本收口，最终答复为无意义自言自语；
- worker 节点：pass-context-dispatch 的 impl 空转 123 轮后纯文本退出，
  未调 `submit_task_result` 仍被记 completed，图按 completed 边放行零交付节点；
- verifier 节点：context-push-order 的 acceptance 纯文本退出漏交 verdict
  → `invalid_verdict` → 图变更请求 → scheduler 又输出 134KB 纯文本不调
  `patch_graph` → 图僵尸。

**修复方向**：
1. 零证据审查从 `report_done` PreCall 移到任务终态落盘点
   （`text_only_submission` / `task_submitted` 判别处），路径无关；
2. 图节点自然退出时注入 nudge 要求 `submit_task_result` 结构化收口，
   有限次数后 fail-closed（把 acceptance 缺 verdict 的 `invalid_verdict`
   硬约束推广到声明了应报事件的 agent 节点）；
3. 与已有空响应守卫（空文本+零工具）合并，封死全部零工具出口。

## SWE-002 图终态回填零容错（P0，阶段 2，**fixed(2026-08-21)**）

**修复内容**（三层防线，当日落地）：
1. **evidence 装配归一**（`internal/bootstrap/graph_runtime.go`）：
   `evidenceKindOf` fallthrough 不再透传——形状非法归一 `unknown`、合法
   超长跑 `boundedEvidenceValue` 截断；新增 `evidenceToolNameOf` 把非法
   工具名归一为确定性占位 `malformed:<sha256前12>`；装配产物恒过
   `validateEvidenceEntryBounds`（测试直接断言）。
2. **ToolCallRecord 落库清洗**（`internal/store`，同关 SWE-003 残余①）：
   `AppendToolCall`（账本唯一写入点）持久化前清洗非法工具名，同一垃圾名
   同一占位、进程内去重告警；TaskMemory 身份/evidence/anomaly 启发式全部
   吃到干净名。
3. **回填失败不静默**：`graphTerminalSink` 新增 `FailTerminalWriteback`
   回落——节点标 failed（reason `graph_writeback_failed`）+ 幂等唤醒
   `[graph-change-request: <gid>/<aid>/writeback-failed]`（复用两击的
   GraphChangeWakeSpec 机制）；刻意不做盲重试（确定性数据错误重试必复发，
   瞬时 IO 失败走节点 failed + Scheduler 重激活更诚实，注释钉死该决策）；
   终态事实三态穷尽：成功回填 / 节点 failed+唤醒 / 回落再败（degraded
   另有 fail-closed）。

**验证**：事故形状端到端回归（200+ 字符 DSML 名入账后图正常推进、证据
层为占位）；新增 14 个测试/子测试；全量 42 包绿 + `go vet` 干净 +
二进制冒烟（config doctor 0 错误、系统就绪、graph-terminal-feed 注册成功）。

**现象**：`graph-terminal-feed` reactor 装配 activation result 时把工具名
原样塞进 evidence kind，模型 DSML 泄漏产生的畸形工具名（200+ 字符）撞上
128 rune 上限（`internal/graph/validate.go`），整条 activation result 被
拒写；reactor 失败后仅记日志、不重试、无死信、无告警升级
（`internal/bootstrap/graph_runtime.go`、`internal/reactor/reactor.go`）。
任务终态事实永久丢失 → 图僵尸 running → 系统静默停摆（pass-context-dispatch
的致命一击，harness 以 quiet 收割）。

**修复方向**：
1. evidence 装配对未知/超长工具名归一截断（与 `boundedEvidenceValue` 同款）；
2. `ToolCallRecord` 落库前清洗含换行/标记字符的非法工具名，避免污染下游
   一切以工具名为键的逻辑；
3. `graph-terminal-feed` 失败必须重试或把节点显式推进 failed 并唤醒
   scheduler 裁决，禁止静默丢终态。

## SWE-003 长 JSON 生成失控 + 工具名污染（P1，阶段 3，**partially-fixed；残余拆分归档(2026-08-22)**）

**已落地（预防 1，检测提醒）**：
- `submit_graph` JSON 语法期失败的错误回执强制附带分批建议（「先交
  root+end 骨架图，再经 patch_graph 逐次扩展」），`internal/tools/graph_control.go`；
- 载荷超 `graphJSONAdviseThreshold=8000` rune 但合法时正常接受、返回值附
  温和提醒（阈值依据实测：成功图 2727–5762 字符；若损坏仍复现按计划收紧
  到 6000）；
- `internal/llm/client.go` 两条 tool_call 参数解析失败路径的错误携带载荷
  字符数，经 Task Memory 终止原因交接提醒下一 attempt。

**仍 open**：
1. ~~ToolCallRecord 落库前的非法工具名清洗~~ **fixed(2026-08-21)**：随
   SWE-002 第 2 层同处落地（`AppendToolCall` 清洗 + 确定性占位）；
2. 流式早夭检测（生成中途发现 args JSON 已崩即提前中断，省整轮超时）；
3. structured output / response_format 对长 JSON 的约束效果评估。

**2026-08-22 重分类**：第 2 项流式 hard cap/早夭正式并入
[`Context Snapshot / Item Budget`](../design/context-snapshot-item-budget.md)，完整
Graph JSON 生成路径并入 [`Graph Draft / Commit / Start`](../design/graph-draft-commit-start.md)，
malformed 后无界重试并入
[`Loop Progress Contract / Checkpoint / Deadline`](../design/loop-progress-checkpoint-and-deadline.md)。
第 3 项只保留为 provider structured-output capability 实验，不成为框架安全前提。

**现象**：DeepSeek-V4-Flash 单次 completion 冲到 16K–31K tokens 时
`submit_graph` 载荷 JSON 语法全部损坏（三次失败三种坏法：invalid char
')' / '?' / ']'），是 automatic-options / teardown-callbacks 两题 graph=0
的直接原因；另多次把 DSML 标记泄进 tool_call 名字段（`run_shell>\n<｜DSML｜
parameter...`），既浪费两段共 271s，又是 SWE-002 的污染源。

**修复方向**：
1. `submit_graph` 侧限定图规模或引导分段提交（节点数/字节数软上限 +
   超限明确报错而非吃进坏 JSON）；
2. 工具名清洗与 SWE-002 第 2 条同一处落地；
3. 评估 LLM 层 structured output / response_format 对长 JSON 的约束效果。

## SWE-004 图设计死胡同与终态语义（P1，阶段 3，**既有修复保持；残余并入 SWE-012(2026-08-22)**）

**设计已定稿（2026-08-20）**：`docs/design/graph-terminal-contract-v2.md`
——封闭终态 status 三值、图任务 event 废弃、业务路由全走数据字段、
提交期两击升级协议（节点自愈一次 → Scheduler 介入）、输出契约机械派生
钉入、graph schema v2 直接迁移 + legacy strict 渐进。三个子现象中的
子问题 2（自造 event）由本设计机械消灭；子问题 3（成败不分）的 end
outcome 落账另列；子问题 1（无返工回边）仍为提示词 doctrine 随重构专场。

**已落地（2026-08-21，五切片）**：
1. `internal/graph/validate.go`：v2 建图校验——agent/controller 出边仅
   `completed/failed/blocked/always` 系统事件，path 字段须在 description
   声明（阶段「输出契约」）；v1 规则逐字节不变；
2. `internal/graph/outlet_check.go` + `internal/tools/submit_result.go`：
   `Runtime.CheckActivationOutlet` 提交期出路预求值 + 两击协议（首击拒绝
   不 finalizing 可重交；次击节点 failed[contract_no_outlet] +
   `[graph-change-request: <gid>/<aid>/no-outlet]` 唤醒）；v2 图任务
   event 参数废弃（参数级拒绝不计两击）；计数按 activation 持久化；
3. `internal/graph/output_contract.go`：输出契约机械派生
   （eq>in>exists>ne），`<output-contract>` 块注入任务描述并钉入
   `TaskMemory.Constraints`；patch 改边后新 activation 按新冻结定义重派生；
4. scheduler 提示词 v8.0 v2 化（无 v1 残留）、工具描述同步、legacy
   publish_task strict 渐进标注、AGENTS.md 契约条目；
5. 测试矩阵：切片自测 + 专职 subagent 补 9 用例（混合出边短路、acceptance
   次击边界、controller 过检、嵌套 path 派生、patch 版本不交叉、无兜底
   fail-closed、结构错误不计两击等）。

**验证**：全量 `go test ./...` 43 包绿、`go vet` 干净、`config doctor`
0 错误、二进制冒烟（healthz 200、系统就绪）。

**残余敞口**：子问题 1（failed 边无返工回边）与子问题 3（end outcome
落账）不在本期——前者随 scheduler 提示词重构专场，后者另列设计。

**2026-08-22 重分类**：子问题 1 的 doctrine 与结构覆盖进入 GraphContract/
Proposal Acceptance；子问题 3 已由
[`Graph Draft / Commit / Start`](../design/graph-draft-commit-start.md) 的 typed
EndOutcome、GraphStatus 推导和 Outcome/Recovery/UI 切片吸收。SWE-004 不再建立
独立残余设计。相关生产实现已经落地；在 SWE-012 完成定义与外部回归满足前，
状态为 `subsumed-by-SWE-012 / implementation-landed / validation-open`。

**现象**（scheduler 生成图的设计缺陷，属提示词域）：
- failed 边只去 end 死节点、无返工回边——session-access-tracking 中 checker
  如实跑出测试失败后只能「收官」，verify 只挂 happy path 永远没被执行；
- checker 自造 `event="ready"` 无匹配出边 → 整张图 fail-closed
  （ipv6-session-txn 图 failed 的直接原因）；
- 「到达任意 end = graph completed」使成功收官与失败收官在终态信号上
  不可区分，harness 与用户都需读 scheduler 终报才知道真相。

**修复方向**：随 Scheduler 提示词系统重构一并处理
（`docs/design/scheduler-prompt-and-acceptance-redesign.md`）：失败路径
默认带返工回边预算、验收挂在所有实质路径之后、end 节点区分
`end_pass` / `end_fail` 语义。同文档还涵盖二轮遗留的「scheduler 亲自
长时间调查拖死全局」问题。

## SWE-005 harness 契约缝隙（P1，阶段 1，**fixed(2026-08-20)**）

**修复内容**（当日落地并逐条实测）：
1. **test patch 提交态**：`prepare_task.sh` 应用 test patch 后立即以 harness
   身份提交——`git status` 干净，`git checkout -- tests/` 退化为无害
   no-op（实测：执行三轮事故同款命令后 status 仍为空）；篡改检测阴性
   用例（真实改动测试文件）仍正确判 `test_tampered`，防线未削弱。
2. **评测脱污**：`setup_task.sh` 由共享对象库的 `git worktree` 改为
   `--no-local` 独立克隆，随后裁全部分支/标签引用 + reflog expire +
   `gc --prune=now` + 移除 origin remote——实测 `git show <fix-sha>` 已
   失败、fix^ 前历史完整保留、主仓库零接触；`.swe_fix_sha` 不再写入
   worktree，改写 `runs/<task>/fix_sha`（agent 不可达）。
3. **全量红态基线**：prepare 期记录 `runs/<task>/baseline.json`，judge
   输出 `baseline` 字段并对 failed  verdict 附 `red_note`（与基线一致/
   重于基线=新增破坏/轻于基线=部分修复）。teardown-callbacks 基线实测
   19 failed + 481 errors——三轮的红鲱鱼今后自动标注。

**连带修复**：prepare 基线解析在 `pipefail+set -e` 下被 grep 无匹配误杀
（管道加 `|| true` 兜底）、commit message 的 `$TASK_ID）` 全角紧邻变量
解析 bug（同 preflight_probe 的 macOS bash 坑，已全量 grep 确认无残留）。

**验证**：四组端到端用例（提交态 no-op 证明 / 未改动基线判定 / 篡改阴性 /
恢复回归）+ teardown-callbacks 基线 + 全部 7 个脚本 `bash -n` 通过。

**现象**（测试设施问题，非 AgentGo 本体）：
1. `prepare_task.sh` 把 test patch 以**未提交工作区改动**预置，worker 在
   长时间自我怀疑后把「禁止修改 tests/」纪律用反，用
   `git checkout -- tests/` 回滚了 golden 测试（两题 test_tampered；
   讽刺的是两题 src 修复都做对了，一题与上游 fix 逐字节一致）；
2. golden fix commit 留在 worktree 对象库且 `.swe_fix_sha` 明文可见，
   worker 确实 `git show` 读过（评测污染面）；
3. teardown-callbacks 的 481 errors 是 base 代码 context 泄漏的固有全量
   红态（空补丁原地复现），prepare 期只跑 5 个测试文件做红态确认，
   全量基线缺失导致误判风险。

**修复方向**：test patch 改为提交态（commit 进 worktree，使 checkout 无害化）；
prepare 期 `git gc --prune=now` + 移除 `.swe_fix_sha` 明文；prepare 期记录
全量红态基线供 judge 对照。

## SWE-006 read-before-write 摩擦（P2，watch）

**现象**：session-access-tracking 的 worker 用 `sed` 读过 ctx.py 后直接
`edit_file` 被 read-before-write Gate 拒绝（它此前未用 `read_file`），
补读后再未重试该编辑，修复因此不完整。Gate 行为正确，但「被拒后不重试」
的恢复路径偏弱。随 SWE-004 提示词重构在 worker 提示词中强化「被拒后
补救重试」即可，不动机制。

## SWE-007 级联取消的实际影响（watch）

2026-08-19 起 watchdog 不再超时杀任务（只告警），级联取消保留。其真实
影响需靠后续大量真实测试观察：每轮报告单列「级联取消触发情况」一栏。
本轮未触发。

---

## 已闭环（本阶段不再跟踪，证据见 KNOWN_ISSUES.md）

- P0-1 模板装配漏接 → `agent_templates.enabled` 缺省 false 机制搁置
  （2026-08-20，v7.6 静态路由提示词）；
- **SWE-001 纯文本自然退出绕过收口 → 兜底双闸 + 预防三件套（2026-08-20
  修复，残余开放子项见该节）；SWE-003 的长 JSON 检测提醒部分同日落地
  （8000 rune 阈值 + 语法期分批建议 + 解析失败载荷入错）**；
- watchdog 超时误杀、controller lease 越权、空响应守卫、report_done
  收口 Gate（仅覆盖 report_done 路径，残余盲区即 SWE-001）、占位图校验。

## 阶段定义

| 阶段 | 内容 | 问题 | 状态 |
|---|---|---|---|
| 1 | harness 快修（最便宜，先恢复评测信号纯净度） | SWE-005 | **fixed(2026-08-20)** |
| 2 | 收口与回填机制（核心 runtime 正确性） | SWE-001、SWE-002 | **全部 fixed（阶段 2 完成）** |
| 3 | Scheduler 提示词重构 + 模型侧适应 | SWE-003、SWE-004、SWE-006 | SWE-003/004 部分落地 |
| — | 持续观察项 | SWE-007 | watch |
