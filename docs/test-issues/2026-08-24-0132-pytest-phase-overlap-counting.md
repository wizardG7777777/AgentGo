# SWE pytest 阶段重叠计数

> 日期：2026-08-24<br>
> 问题：SWE-036<br>
> 范围：外部 SWE Python runner 结果采集，不属于 AgentGo 五层运行时<br>
> 状态：fixed / local-validation-complete

## 1. 事故

`teardown-callbacks` 的 pytest 原始摘要为 `19 failed, 476 passed, 481 errors`，
实际收集 495 个逻辑测试；JUnit suite 却报告 `tests=957`。旧 runner 把
failure/error/skipped 当作互斥集合，使用
`tests - failures - errors - skipped` 推算 `passed=457`，与 pytest 的
call-phase `passed=476` 不一致。

pytest 允许同一逻辑测试的 call 成功或失败后，teardown 再产生 error；call
outcome 与阶段 error 因而可以重叠。JUnit 还会为 call failure + teardown error
生成两个同名 testcase，并以内部状态修正 suite tests，不能从顶层属性无歧义恢复
collected/passed。

## 2. 修复

- pytest 进程显式加载仓库内 phase reporter，以
  `pytest_runtest_logreport`、`pytest_collectreport` 与
  `pytest_sessionfinish` 直接生成 `*.pytest.json`。
- `collected`、call `passed/failed`、`skipped/xfailed/xpassed` 与
  collection/setup/teardown error events 分别统计；JSON 保留兼容字段
  `tests=collected`，并声明 `count_semantics=pytest-phase-overlap/v1`。
- JUnit 只保留 traceback/测试名，并交叉核验 failures/errors/skipped；删除旧
  passed 减法公式。侧车缺失、非法或与 JUnit 冲突时 fail-closed。
- 终端改用 `collected` 与 `error_events` 标签，明确各字段不可相加推导总数。

## 3. 验证

- Python runner 单元测试覆盖 pass/fail、setup/teardown/collection error、
  call failure + teardown error、skip/xfail/xpass、非法侧车与 JUnit 冲突。
- 临时真实 pytest 工程得到 `collected=2 passed=1 failed=1 errors=2`，其中两条
  errors 均为 teardown 事件，JUnit 交叉校验通过。
- 现有 `teardown-callbacks` worktree 未调用模型重新采集，权威结果为
  `collected=495 passed=476 failed=19 error_events=481`，与 pytest 原始摘要一致；
  verdict 仍为 failed，证明修复只纠正观测计数，不改变任务判定。
