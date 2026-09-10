# agentTask 删除、迁移、重写与新增对账

这是当前工作树对既有基线的实际变更，不使用代码净行数代替入口退役检查。用户原有 MemoryManageSystem/activate README 修改和 skill-based-agent-routing 删除不计入本次。

| 类别 | 内容 |
|---|---|
| 删除 | 旧 Graph 节点、条件边、authoring 控制链、验收/审批/子图/工具节点桥；Proposal Acceptance 包；intervention 投递包及 L4 写入接口；旧结果 event/verdict/cited_evidence 和 /event 接口 |
| 迁移 | 普通结果/证据及摘要处理、派发与唯一终态纪律、工作区和 Effect 算法，迁入 dataflow 接缝和图级交付 |
| 重写 | graph/dataflow 运行时与存储；Graph 工具 schema；Bootstrap/Runner/Scheduler/终态转换；Delivery v2；SWE audit v5；配置、恢复、UI 与文档 |
| 新增 | 多输入与 DAG 校验、版本化外部输入、候选完整目录验证、图级 complete/取消结算、失败事实输入、增量/CAS/候选/恢复/交付回归 |

旧测试中的控制节点、模型审批、出路两击和回边预期随对应机制删除；工具执行身份、文件/Effect、任务状态、L1/L2 和取消等通用回归保留。新图/候选/图级交付有新的确定性与真实二进制验证。

## 已删除的跟踪文件

- `internal/bootstrap/delivery_commit_test.go`
- `internal/bootstrap/graph_acceptance.go`
- `internal/bootstrap/graph_acceptance_integration_test.go`
- `internal/bootstrap/graph_acceptance_test.go`
- `internal/bootstrap/graph_approval.go`
- `internal/bootstrap/graph_approval_test.go`
- `internal/bootstrap/graph_bridge_integration_test.go`
- `internal/bootstrap/graph_dataflow_test.go`
- `internal/bootstrap/graph_output_contract_test.go`
- `internal/bootstrap/graph_progress_contract_test.go`
- `internal/bootstrap/graph_runtime.go`
- `internal/bootstrap/graph_runtime_test.go`
- `internal/bootstrap/graph_tool.go`
- `internal/bootstrap/graph_tool_assembly_test.go`
- `internal/bootstrap/graph_tool_test.go`
- `internal/bootstrap/loop_intervention.go`
- `internal/bootstrap/loop_intervention_test.go`
- `internal/bootstrap/loop_recovery_integration_test.go`
- `internal/bootstrap/task_outcome_test.go`
- `internal/bootstrap/team_v1_migration.go`
- `internal/bootstrap/team_v1_migration_test.go`
- `internal/bootstrap/ui_graph_test.go`
- `internal/bootstrap/workspace_retention_test.go`
- `internal/delivery/types.go`
- `internal/delivery/types_test.go`
- `internal/graph/acceptance.go`
- `internal/graph/acceptance_test.go`
- `internal/graph/authoring_runtime.go`
- `internal/graph/authoring_runtime_test.go`
- `internal/graph/authoring_store.go`
- `internal/graph/authoring_store_test.go`
- `internal/graph/authoring_types.go`
- `internal/graph/contract_validate.go`
- `internal/graph/definition_compiler.go`
- `internal/graph/definition_compiler_test.go`
- `internal/graph/delivery_v3_test.go`
- `internal/graph/digest.go`
- `internal/graph/digest_test.go`
- `internal/graph/evidence_persistence_test.go`
- `internal/graph/fulfillment_runtime_test.go`
- `internal/graph/journal.go`
- `internal/graph/ledger_integrity_test.go`
- `internal/graph/minimum_validate.go`
- `internal/graph/outcome_recovery_test.go`
- `internal/graph/outlet_check.go`
- `internal/graph/outlet_check_test.go`
- `internal/graph/output_contract.go`
- `internal/graph/output_contract_test.go`
- `internal/graph/output_contract_validate.go`
- `internal/graph/output_contract_validate_test.go`
- `internal/graph/patch_version_test.go`
- `internal/graph/path.go`
- `internal/graph/path_test.go`
- `internal/graph/proposal_acceptance.go`
- `internal/graph/recover.go`
- `internal/graph/recovery_delta.go`
- `internal/graph/recovery_delta_benchmark_test.go`
- `internal/graph/recovery_delta_test.go`
- `internal/graph/recovery_runtime_test.go`
- `internal/graph/request_queue_test.go`
- `internal/graph/route_test.go`
- `internal/graph/runtime.go`
- `internal/graph/runtime_input_test.go`
- `internal/graph/runtime_nodes_test.go`
- `internal/graph/runtime_router_failure_test.go`
- `internal/graph/runtime_suspend.go`
- `internal/graph/runtime_suspend_test.go`
- `internal/graph/runtime_terminate_all_test.go`
- `internal/graph/runtime_test.go`
- `internal/graph/runtime_worklog_test.go`
- `internal/graph/session_ownership_test.go`
- `internal/graph/store.go`
- `internal/graph/store_test.go`
- `internal/graph/types.go`
- `internal/graph/types_test.go`
- `internal/graph/validate.go`
- `internal/graph/validate_test.go`
- `internal/graph/writeback_fallback.go`
- `internal/graph/writeback_fallback_test.go`
- `internal/hook/builtin/scheduler_closure.go`
- `internal/hook/builtin/scheduler_closure_test.go`
- `internal/intervention/identity.go`
- `internal/intervention/pump.go`
- `internal/intervention/pump_test.go`
- `internal/proposalacceptance/doc.go`
- `internal/proposalacceptance/output.go`
- `internal/proposalacceptance/prompt.go`
- `internal/proposalacceptance/types.go`
- `internal/proposalacceptance/verifier.go`
- `internal/proposalacceptance/verifier_test.go`
- `internal/tools/graph_authoring_test.go`
- `internal/tools/graph_board_test.go`
- `internal/tools/graph_routes.go`
- `internal/tools/plan_control_graph_test.go`
- `internal/tools/plan_control_test.go`
- `internal/tools/submit_outlet_test.go`
- `internal/tools/submit_result_test.go`

共 97 个跟踪文件。混合文件内被移除的旧函数另计，包括 submit_task_result 出路预求值、旧租约角色分派、Scheduler batch/downstream 等待、L4 intervention outbox 和终态控制字段。
