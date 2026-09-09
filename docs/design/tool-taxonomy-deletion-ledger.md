# 四类工具重建：删除、迁移、重写、新增对账

对账基准：Git HEAD `4626bd3`。本表只覆盖本次工具重建，排除预先存在的 MemoryManageSystem/activate README 修改、旧 skill-routing 文档删除和 benchmark-logs。旧磁盘运行数据未清理，历史测试记录不作为新链路证明。

## 删除

- 模型工具与 handler：旧文件写入双入口、目录/搜索工具、publish_task、run_check、Observation、submit_change_decision、submit_recovery_decision、模型草案步骤、Scheduler 私有查询/汇报工具和 solo 脱图执行旁路。
- 控制实现：Observation schema/模型/探针/能力熔断，Agent 强制单工具阶段/周期阈值/默认阶段预留/首动作 gate，L2 控制与调查强制历史模式及 Observation 锚点重放。
- 测试程序：Python Observation 矩阵、CheckContract 生成、旧工具阶段/次数判定、旧存储目录 fallback。原断言对应的独立业务不变量由新入口测试承接。
- 配置：MaxSubtaskDepth 字段/默认值/注入；文件中显式 max_subtask_depth 拒绝。observation_model 已从当前租约和配置实现退役。

以下为实际整文件删除清单（含退役实现专用测试）：

- `internal/agent/recovery_action_gate_test.go`
- `internal/agent/recovery_evidence_gate.go`
- `internal/agent/recovery_first_action_benchmark_test.go`
- `internal/checkstore/store.go`
- `internal/checkstore/store_test.go`
- `internal/contextruntime/history_modes.go`
- `internal/controlcapability/store.go`
- `internal/controlcapability/store_test.go`
- `internal/observationcontract/schema.go`
- `internal/observationcontract/schema_test.go`
- `internal/observationprobe/cli.go`
- `internal/observationprobe/cli_test.go`
- `internal/runcontract/profiles.go`
- `internal/runcontract/profiles_test.go`
- `internal/scheduler/solo.go`
- `internal/taskmem/observation.go`
- `internal/taskmem/observation_test.go`
- `internal/tools/change_decision.go`
- `internal/tools/change_decision_test.go`
- `internal/tools/check.go`
- `internal/tools/check_test.go`
- `internal/tools/graph_control.go`
- `internal/tools/graph_control_test.go`
- `internal/tools/meta_capability_test.go`
- `internal/tools/observation.go`
- `internal/tools/observation_test.go`
- `internal/tools/recovery_decision.go`
- `internal/tools/scheduler.go`
- `internal/tools/scheduler_probe.go`
- `internal/tools/scheduler_result_test.go`
- `internal/tools/scheduler_test.go`

## 迁移

| 原职责 | 当前归属 |
|---|---|
| CheckStore 中通用 workspace revision 计算 | internal/executionfacts；不含测试识别/判分 |
| write_file/edit_file 的路径、锁、版本、写入、产物与 Effect | tools/local_write.go 的单一 applyChange 实现 |
| 旧 Graph 工具的路由/定义校验 | graph_routes.go、graph_schema.go；模型入口统一 graph_authoring.go |
| 完整执行记录与证据访问 | InspectionGroup/EvidenceGroup，关联当前 Run 与 Invocation/CallID |
| 测试范围/代码身份/pytest 计数 | Python test_identity.py、reporter 和 runner.py；不回填 AgentGo Check gate |
| 旧活动架构、profile、冻结说明 | docs/archived 原文；当前入口按新规范重写 |

## 重写

- L3–L5：graph_authoring.go 的 create/update/start/cancel 事务，ExecutionLease v3、角色工具视图、统一结果收口、信息投递与检视接缝。
- L4/记忆：progress 只记录事实；删除强制观察/决策的控制链。保留用户显式限制、真实失败、取消、Effect unknown 和 finalizing 优先级。
- L2 历史：Raw History 保持不变；同文件重复读取投影为 ContentRef，纠正路径键误写导致的跨文件去重，旧控制锚点明确拒绝。
- 通用执行事实：工具预留失败仍写未派发回执；Shell 绑定调用身份，保留超时部分输出和可选退出码；文件回执记录变更前后版本。
- Python：runtime_audit 按新版目录/身份/事务读取事实，结果 v4；正式 pytest 独立保存输入/环境与失败集合，Judge v2；批次不接受旧结果补默认通过值。
- 本地二进制 fixture：两个显式 SSE 协议，初建/动态更新/非法拒绝/连续读取/信息不唤醒/文件/Shell/验收/Delivery/final-report，不依靠旧工具 fake 回执。
- 文档与 YAML：AGENTS、五层规范、冻结基线、工具 profile、系统架构与参考手册、公共及测试配置、角色提示词。

## 新增

- 核心 API：apply_graph_change、control_graph、read_graph_definition、inspect_board、inspect_node、read_evidence、apply_change；所有入口按职责直接实现，不作旧名称转发。
- 调用身份：agent/tool_call_identity.go 与 ShellExec v2；记录 Registry 派发和进程启动的不同事实。
- 数据边界：Session 7、Run v3、Lease v3、ProgressContract v2、Graph v5、fulfillment v2 及独立版本目录。
- 测试：图事务幂等/并发/作用域，消息恢复及不唤醒，文件变更/版本冲突，连续合法调查，引用隔离与历史不可变，Shell 中断事实，无默认 Proposal deadline，Python 新采集与测试身份。

## 保留的边界

TaskOutcome/Effect/Graph 的历史 DTO、显式用户 Reactor 业务接口和只读 Trace 解码不等于旧模型工具仍可执行。当前核心 13 个模型工具以 known_tools.go 和实际角色注册为准，可选 Web/Team 单列；内部 Proposal Acceptance 属于图准入组件，不是 Agent 的强制 Observation 阶段。

watchdog 生产代码未修改，其两处测试仅适配共享函数签名与当前策略引用。真实 SWE 暂不运行；Go、离线测试和本地二进制的实际证据见 [工具契约第 13 章](tool-taxonomy-and-contracts.md)。全量本地检查已通过，证据见同一提交内的验证记录；本表不代替实际测试结果。

可选 Team 额外修复：Scheduler 视图与 controller Lease 暴露已启用能力，graph_request_id 与 create.request_id 共用图身份，模型入口不再创建旧 task-scoped Team。
