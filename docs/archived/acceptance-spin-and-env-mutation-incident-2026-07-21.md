# 验收空转（6 次 AcceptanceRun）+ Scheduler 篡改工作区事故修复记录（2026-07-21）

## 事故概述

2026-07-21 晚，健康检查 Plan `2b208a28`（session `sess-268cb956`）走了 6 次正式验收才终结，scheduler 30 轮、约 153 万 prompt tokens。四层缺陷叠加：

1. **自相矛盾的验收标准**：Scheduler 把用户"执行 git status 并记录结果"过度解读为 `git-clean`（要求工作区无修改）标准。交付物 `docs/health-check.md` 本身是未跟踪文件——任务完成则标准永假。第 1 轮失败后 scheduler 经 `request_user_input` 挂起 1.5 小时等待用户。
2. **PASS 被自己的家务操作作废**：第 4 轮 PASS 后 scheduler 顺手 `supersede_tasks` 退休旧验收节点，GraphDigest 变化使 `finalize_plan` 被拒（"latest acceptance has not passed"），被迫重开第 5、6 轮。
3. **同因硬约束失败无熔断**：`command evidence target mismatch: git-clean` 连续出现 3 次（第 2、3、5 轮）。07-20 的熔断器只统计 extfact 指纹，硬约束失败（`criterionEvidenceReason` 类）没有指纹；且 epoch 含 GraphDigest——每次 `ensure_acceptance_run` 都改变 digest，熔断在验收重试循环里构造上永不触发。
4. **Scheduler 为通过验收篡改用户环境**：面对不可能标准，scheduler 两次执行 `git stash --include-untracked` 物理制造"干净工作区"，且只恢复了一次——用户工作区全部未提交修改（含当日全部修复成果）被困在两个 stash 条目中，事后经 `git checkout stash@{0} -- .` + `git checkout stash@{0}^3 -- .` 恢复。

## 修复内容

### prompt 层（`internal/scheduler/scheduler.go`、`internal/agenttemplate/prompts/verifier.md`）

- git 类标准只验证命令可执行（如 `git status` exit 0）；不得把"工作区干净"设为验收标准，除非用户明确要求。
- 验收 PASS 后必须立即 `finalize_plan`，中间不得插入任何图变更；旧节点退休必须在启动最终验收之前完成。
- 验收红线：禁止为通过验收而修改被验收对象或环境状态（git stash/clean/checkout 还原/删除被验收文件）；验收不通过只能修复实现、修正标准或问用户。
- verifier.md：验收 command_exit 类标准时直接执行 criterion 的 target 原文（逐字），让证据命令与 target 天然一致。

### 代码层（`internal/plan`）

- `acceptance_fingerprint.go`：新增 `hardc:` 前缀与 `HardConstraintFailureFingerprint`；`circuitFingerprint` 统一识别 extfact/hardc 两类控制面指纹（提交方自填指纹仍不计入）。
- `acceptance.go`：硬约束失败（`acceptanceConstraintReason` / 内建事实）在置 `Verdict=fail` 时自动生成 hardc 指纹。
- **epoch 语义变更**：`leadingExternalFactFailures` 的 epoch 不再包含 GraphDigest（仅 SpecID/SpecRevision）。理由：每次 `ensure_acceptance_run` 都发布新 runner task 改变 digest，digest 参与 epoch 会使熔断在验收重试循环里构造上永不触发（本次与 07-21 上午事故均证实）。Spec 修订仍复位熔断。

## 验证证据

- `internal/plan/acceptance_fingerprint_test.go`：epoch 测试按新语义重写（Spec 变化复位 / digest 变化不复位 / hardc 指纹计入 / 提交方指纹不计入）。
- `internal/plan/acceptance_circuit_test.go`：新增 `TestAcceptanceCircuitOpensAfterRepeatedIdenticalHardConstraintFailures`——command_exit 无证据同因失败 2 次后第 3 次 `EnsureAcceptanceRun` 返回 `ErrAcceptanceCircuitOpen`，Plan 进入 `PausedAwaitingDecision`。
- 全量 `go vet ./...` + `go test ./...` 通过（2026-07-21，Windows）。

## 未动的部分

- 未实现"finalize 容忍 PASS 后仅退休终态节点"的代码级方案（语义较深）；当前以 prompt 规则约束，若再犯再评估。
- `git stash` 等环境修改命令未加入 shell 黑名单——灰名单已覆盖 `git checkout .` 等部分形态，prompt 红线是第一层；是否把 `git stash`/`git clean` 列入灰名单待观察。

## 后续：任务被多 Agent 重复执行（MaxConcurrency 默认值错位，2026-07-22）

- **问题**：同一实施任务被 2-3 个 worker 同时认领并完整重复执行（同毫秒双 `task_claimed`、双份 LLM 循环、双写同一交付文件）。机制：`publish_task` 与 plan `TaskSpec` 均无并发字段，未指定的任务落进 store `default_concurrency` 兜底（用户配置为 3），把"任务级执行次数"语义交给了全局配置；Roster 串行化写与按 agent 分键的 Results 让重复执行完全无报错，浪费被静默吸收。
- **修复**：`internal/tools/meta.go` 的 `publish_task` 新增 `max_concurrency` 参数，未指定时显式置 1（不再落 store 兜底）；验收 runner 任务此前已在 `internal/bootstrap/plan_runtime.go` 硬编码 1，本次确认无需变更。store `default_concurrency` 降级为纯兼容兜底（仅影响非 plan 发布路径），`config.example.yaml` / `docs/yaml-config-guide.md` / `AGENTS.md` 默认值与语义说明同步改为 1。
- **验证**：`internal/tools/meta_test.go` 新增 `TestPublishTask_MaxConcurrencyDefaultOneAndExplicitOverride`；全量 `go vet ./...` + `go test ./...` 通过（2026-07-22，Windows）。
- **遗留**："系统将浪费完全藏起来"（重复执行无告警、token 消耗无可观测阈值）是用户标记的后续专项方向，本次未动。
