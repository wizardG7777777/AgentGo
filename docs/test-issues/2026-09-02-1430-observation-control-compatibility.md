# Observation 控制链兼容性与可信评测修复

> 日期：2026-09-02  
> 状态：Closed / local and full live batch verified  
> 问题编号：SWE-119

## 1. 事故证据

mixed-model 基线中 flagship Worker 的 Observation checkpoint 把空 authority
投影成非法 JSON Schema `enum: []`；Responses `status=failed` 又被统一误分为
`provider_unavailable`，使 400 `InvalidParameter` 被重试并与普通架构事故混记。

## 2. 修复

- 发布 `progress:code-change/v8`：v8 使用 `reasoning=low`、`tool_choice=auto`、
  singleton ToolRouter 与 L3 required-action gate；v7 历史保持 none+exact。
- 工具注册、动态 authority 与 `agentgo probe observation` 共用
  `internal/observationcontract`；空 evidence/candidate 用 `maxItems=0`。
- `agents[*].observation_model` 省略时继承业务模型；ExecutionLease v2、
  InvocationContextBinding v2 与 trace 冻结实际模型和 capability。
- Run-scoped ControlCapabilityStore 只持久化 `invalid_request` /
  `protocol_incompatible`；timeout、429、5xx 不熔断。
- SWE result v3 独立报告 `model_contract_checks/incidents/compatible`，退出码 4；
  batch 后续题使用 `previous_model_contract_gate`。
- Graph 的 code-change fulfillment 版本闭集补入 v8，避免新版本 work 绕过
  FulfillmentContract；Observation v8 OutputBudget 允许有界 provider fan-out，
  L3 仍只执行首个调用并为尾部写 skipped receipt。

## 3. 验证

- `go test ./...`、`go vet ./...`、Windows `go build`；
- SWE Test Runner Python 单测 72 项；
- 真实三题 trace 均完整记录 Binding v2 的 effective model/capability/profile，
  result v3 与 active reservation=0 已落盘；ControlCapabilityStore 生产目录已装配。
- 真实矩阵：`qwen3.8-flash` / `qwen3.8-max` 的 v7/v8 × empty/populated
  八组均为 3/3。按固定规则继续让 Worker Observation 使用 flagship。

## 4. 最终完整 Flask-8 结果

当前 `.batch_start` 事务完成 8/8：`architecture_ok=8/8`、
`model_contract_compatible=8/8`、infra/not-run/hard-kill/provider-invalid/output-limit
均为 0，active reservation 总数为 0；业务 `task_resolved=4/8`。

| task | judge | calls | prompt | completion | patch lines |
|---|---:|---:|---:|---:|---:|
| automatic-options | resolved | 25 | 287363 | 8141 | 13 |
| context-push-order | failed | 28 | 263165 | 15922 | 0 |
| ipv6-server-name | resolved | 24 | 280556 | 8064 | 20 |
| ipv6-session-txn | resolved | 25 | 320225 | 9274 | 19 |
| pass-context-dispatch | failed | 44 | 694190 | 29967 | 0 |
| secret-key-rotation | resolved | 24 | 218818 | 9009 | 19 |
| session-access-tracking | failed | 49 | 697528 | 31935 | 0 |
| teardown-callbacks | failed | 36 | 558798 | 23419 | 0 |

合计 255 次调用、3,320,643 prompt tokens、135,731 completion tokens、
4,455 秒题目执行时间。Scheduler/Worker/Verifier 调用分别为 75/156/24；Worker
占 prompt tokens 70.5%。17 次 Invocation failure 分别为 output_truncated=7、
action_contract_rejected=4、attempt_deadline=4、malformed_response=1、
request_timeout=1；无 provider invalid/quota/rate-limit/server unavailable。

Observation v8 共 29 次 Invocation，其中 10 次在模型输出/期限层失败；19 次形成
tool call，12 次通过 handler，7 次因 stale evidence 或参数语义被拒。所有失败均按
有界修正/Recovery 收口，没有 retry storm、ControlCapability 污染或 active reservation。

四道业务失败具有同一终态：work@1 `decision_progress_stalled` → Recovery retry →
work@2 `invocation_deadline` → Recovery blocked。`session-access-tracking` 在 work@1
做过 3 次成功 edit 与 1 次成功 check，但未及时提交完成；其余三题没有成功 mutation。
这证明 SWE-119 的 Invocation/L2-L4 控制链已关闭，后续工作应单独进入 Explorer
证据交接与 Recovery candidate-state handoff，不再归入 Observation provider 兼容性。

fake Responses 端到端冒烟同步通过：Graph success、Delivery committed、Acceptance
读取并引用 typed CheckRef、39 次模型调用与 Run ledger 一致、active/cancel
reservation 均为 0。
