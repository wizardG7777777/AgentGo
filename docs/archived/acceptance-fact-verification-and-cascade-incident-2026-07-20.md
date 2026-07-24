# 验收事实核验失败循环 + 级联取消事故修复记录（2026-07-20）

## 事故概述

2026-07-20 晚，一个纯调查 Plan（plan `1cd4c704`，session `sess-268cb956`）暴露两个叠加缺陷：

1. **验收 5/5 PASS 却连续判 fail**：三次正式验收（`1bf9c109` / `2aef2720` / `c774aa23`）全部 criterion PASS，但控制面外部事实核验（`planAcceptanceVerifier.VerifyAcceptance`）逐字比对证据失败——verifier 提交的 `task_status` 证据是描述性长句、`command` 证据与实际执行过的 `run_shell` 命令串不一致（包括一次声称执行过 `ls -la` 的伪造证据，被精确匹配正确拦截）。Scheduler 拿不到可操作的指引，反复重开验收。
2. **排队任务被误取消 + 验收任务被级联取消**：探索任务 `5b3f6a99` 只是排队等待空闲 explorer（watchdog `claim_starvation` 仅观测告警），但告警上送 replan 时丢弃了 `Payload["reason"]` 上下文，诱发 Scheduler 主动 `cancel_task`；随后 watchdog 级联取消不区分 `PlanNodeRoleAcceptance`，直接 cancel 了两个验收任务（`41b2b8b7` / `410ed4e6`），破坏验收图。

## 修复内容

### P0 证据契约显性化（对 LLM 可见）

- `internal/tools/plan_control.go`：`submit_acceptance_result` description 与 `evidence_json` schema 写明全部硬规则（task_status 裸状态词、command 逐字一致、run 创建后执行、exit_code 匹配、一次性提交不可重交）。
- `internal/agenttemplate/prompts/verifier.md`：内嵌 verifier 模板从 3 行扩充为含完整证据格式契约。
- `internal/plan/acceptance.go` `formalContext`：验收任务描述强制追加证据硬规则，不再依赖 Scheduler 自由填写的 description。
- `prompts/program_verifier.md`：补 task_status 裸状态词规则与一次性提交语义。

### P1 同因失败熔断 + 可自纠正错误文本

- `internal/bootstrap/plan_runtime.go`：核验失败错误自带修正指引（保留 `"no successful run_shell fact"` 等既有断言子串）。
- `internal/plan/acceptance_fingerprint.go`（新）：外部核验失败生成规范化指纹 `extfact:<sha256>`（去除引号内容与 hex/UUID token，同类缺陷同指纹），写入 `AcceptanceResult.FailureFingerprint`。
- `internal/plan/acceptance.go` `EnsureAcceptanceRun`：同 epoch 连续 ≥2 次同指纹失败 → 不创建新 run，`pausePlan("acceptance_circuit_open")` + 高优 ReplanRequest + `ErrAcceptanceCircuitOpen`，Plan 挂起交用户决策。

### P2 告警上下文 + 级联取消角色豁免

- `internal/scheduler/activator.go`：`EventWatchdogAlert` 上送 replan 时保留真实 `reason_code` 与 `Detail` 原文。
- `internal/scheduler/scheduler.go` prompt：补两条指引——`claim_starvation` 类告警默认 `continue_waiting`，不要取消仅排队的任务；外部核验失败先定位证据格式问题，不要机械重开验收。
- `internal/watchdog/watchdog.go`：`guardAcceptanceCascade` 让 acceptance 角色的 dependent 免于级联取消，改为按租约告警一次（`acceptance_dependency_lost`）；processing 分支用 `clearPendingObservationExcept` 保留该观测避免重复告警。

## 验证证据

- `internal/plan/acceptance_fingerprint_test.go`：指纹归一化稳定性、连续失败计数、epoch 复位（5 个子测试）。
- `internal/plan/acceptance_circuit_test.go`：连续 2 次同类失败后第 3 次 `EnsureAcceptanceRun` 返回 `ErrAcceptanceCircuitOpen`，Plan 进入 `PausedAwaitingDecision` 且信号含 `acceptance_circuit_open`。
- `internal/watchdog/watchdog_acceptance_cascade_test.go`：pending / processing 两条路径的豁免与每租约一次告警；普通任务级联取消行为不变。
- `internal/scheduler/activator_watchdog_test.go`：Payload 转发（reason_code + Detail + high urgency）与无 Payload 回退。
- 全量 `go test ./...` 通过（2026-07-21，Windows）。

## 后续：default_model 配置错误与 Team 恢复 digest 失配（2026-07-21）

- **配置错误**：`setting.yaml` 的 `llm.default_model: deepseek/deepseek-v4-pro` 是 OpenRouter 风格模型名，DeepSeek 原生 API 只接受 `deepseek-v4-pro`/`deepseek-v4-flash`。静态 kind 均有 per-kind model 覆盖所以正常；动态 provision 的 verifier team 回落 default_model（`bootstrap.go:222-228`）导致每次 LLM 调用 400。已改为 `deepseek-v4-pro`。
- **启动硬故障**：verifier.md 提示词修改 + default_model 修正都会改变模板 digest，导致 `team.Manager.Start` 恢复旧 Team 时 digest 失配直接中止启动（`ErrTemplateDigestMismatch`）。已改为：恢复时模板不可用或 digest 失配的 Team 标记 `stopped`（`template_unavailable:` / `template_digest_changed:` 原因）并告警，不再中止启动；显式 `Provision` 的 digest 校验保持 fail-closed 不变。
- 验证：`internal/team/manager_test.go` 新增 `TestManagerRecoveryStopsStaleDigestTeam` / `TestManagerRecoveryStopsTeamWithUnavailableTemplate`；全量 `go test ./...` 通过；`agentgo --config setting.yaml` 真实启动冒烟通过（陈旧 Team 被持久化标记 stopped，系统正常就绪）。

- 未放宽 `plan_runtime.go` 的逐字精确匹配——它是防证据伪造的防线，本次事故中正确拦截了 `ls -la` 伪造证据。
- 未改 `claim_starvation` 300s 阈值与 no-progress 护栏默认值。

## 后续 2：Scheduler 阶段性唤醒门控（2026-07-21）

- **问题**：Plan 内任何节点终态都无条件产生唤醒信号（`preparePlannedMutation` 的 `wake=true`），导致并行阶段中每个中间完成都唤醒 Scheduler 跑一整轮 LLM（重发全部历史），而结论往往只能是 `continue_waiting`。事故会话中 scheduler 单任务 26 轮、累计 1.32M prompt tokens，其中约 4 轮是纯空转。
- **修复**：`internal/plan/coordinator.go` 的 `applyTaskMutationOp` 增加门控——`task_completed` 且当前图内仍有其他非终态节点时不投递 ReplanRequest（节点事实照常持久化，信息不丢）；阶段内最后一个节点终态才一次性唤醒。失败/取消/验收类信号不受门控。批内按序应用保证同批多个完成只唤醒一次；`waitForPlanSignal` 的预算心跳在全部节点终态后兜底放行，不会死锁。`RecordTaskMutations` 新增 notified 返回值，bootstrap 只在真正投递时发射 `KindReplanRequested` trace。
- **验证**：`internal/plan/wake_gate_test.go`（中间完成门控 / 失败不门控 / 同批只醒一次）；`internal/bootstrap/plan_runtime_test.go` 原 `TestSchedulerWakesAfterOneTaskTerminalWhilePeerStillRuns` 按新语义重写为 `TestSchedulerWakesOnlyAfterPhaseTerminalNotIntermediateCompletion`；全量 `go test ./...` 通过。
- **回归提示词**：根目录 `test-prompt-parallel-investigation.md`（唤醒门控 + 证据契约）、`test-prompt-acceptance-evidence.md`（证据硬规则 + 熔断）、`test-prompt-team-recovery.md`（provision / 模型回落 / 重启恢复）。
