# SWE L3 Shell 与 L5 final-report scope 残余

> 日期：2026-08-23<br>
> 问题：SWE-034 / SWE-035<br>
> 归层：L3 Harness + L4 Observation / L5 Graph finalization<br>
> 状态：fixed(2026-08-23) / external-validation-complete

## 1. 事故证据

第八轮 DeepSeek 8 题与 SWE-033 定向复跑暴露两个不影响 Graph durable 终态、
但会污染模型判断和架构门的残余：

1. Worker 经常执行 `pytest ... 2>&1 | tail -N`。POSIX `sh -c` 与 PowerShell
   默认都只保证最后一个 pipeline 段的退出码；`tail` 成功会把 pytest 失败投影为
   `exit_code=0`。L4 过去据此产生 `evaluation_passed`，TaskMemory/Evidence 也只
   保存裸 exit code。
2. graph-ended final-report 是 Graph 外的 Scheduler Task。它只有 Description
   marker，没有冻结 Graph 读取身份；`read_graph` 可跨图读取，而
   `get_task_result` 又把它归入 legacy scope 并拒绝目标 Graph Task。最新成功的
   `context-push-order` 与 `session-access-tracking` 均复现该拒绝，但旧
   `architecture_ok` 不检查 final-report Task。

## 2. SWE-034：Shell exit scope

- `internal/shell.HasPipeline` 以双方言词法边界识别真正 pipeline，忽略引号、转义、
  here-doc 正文、注释与逻辑 OR。
- `run_shell` 对 pipeline 默认 fail-closed；只有显式
  `accept_last_pipeline_exit_code=true` 才执行。
- 每次结果冻结 `exit_code_scope=whole_command|last_pipeline_command`；pipeline
  结果附明确警告，不能作为整条测试/构建成功证明。
- ExitCodeScope 随 ToolCallRecord、Session snapshot、TaskMemory、Graph Evidence、
  TaskOutcome 与 shell trace 持久化；旧快照空 scope 按历史 whole-command 兼容。
- L4 对 `last_pipeline_command + exit=0` 只投影 `verdict=ambiguous`，绝不产生
  `evaluation_passed`；TaskMemory 将其记录为歧义失败事实。

## 3. SWE-035：final-report Graph scope

- graph-ended wake 冻结 `FinalReportGraphID`，自身 `GraphID` 继续为空，避免伪装
  Graph Activation。
- `read_graph` 与 `get_task_result` 只允许目标 GraphID 精确等于该 scope；
  graph-ended Task 缺 scope、scope 与 Graph/intervention 身份混用均 fail-closed。
- scope 经 Task/Session snapshot 与安全 BoardTask 投影往返保真。
- SWE runner 在 Graph terminal 后等待 final-report Task 终态，并核验 scope、
  completed 状态、TaskOutcome commit；final-report 的 result-scope 拒绝进入
  `final_report_result_scope_failure` 架构事故门。

## 4. 验收条件

1. pipeline 默认不执行；显式执行的结果含 last-command scope，L4 不判 pass；
2. graph-ended wake 能读取同图 Graph/Task result，跨图与缺 scope 拒绝；
3. Session 恢复不丢 FinalReportGraphID/ExitCodeScope；
4. runner 对缺失、未完成或 scope 错误的 final-report 判 architecture failure；
5. focused/full/race/vet/build、Python runner 测试与真实二进制启动通过；
6. 真实系统测试产物包含 completed final-report 与非空 OutcomeRef，且 trace 中无
   `final_report_result_scope_failure`。

## 5. 完成证据

- focused/full/race/vet/build、Python runner 26 项测试通过。
- 真实 DeepSeek `ipv6-server-name` 单题中，Worker 首次提交
  `pytest ... | tail -15` 被 L3 明确拒绝，随后改用无 pipeline 的 targeted/full
  pytest；shell trace 与 TaskOutcome Evidence 均记录
  `exit_code_scope=whole_command`。
- 同一 Run 的 graph-ended final-report Task 冻结目标 GraphID，第一次
  `get_task_result` 因多 agent 结果要求 `agent_id`，第二次带精确 agent_id 成功；
  未再出现 legacy scope 拒绝。
- 最终产物：493 passed、patch 18 行、final-report `status=completed`、OutcomeRef
  非空且 delivery ACK；`final_report_present/scope_bound/terminal/completed/
  outcome_complete=true`，`architecture_ok=true / task_resolved=true`，进程无 hard kill。
