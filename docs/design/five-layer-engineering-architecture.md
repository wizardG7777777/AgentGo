# AgentGo 五层工程架构规范

状态：L1/L2 核心重建已实施；已执行验证与未执行项见实施记录。旧实施记录见 [历史规范](../archived/five-layer-engineering-architecture-before-l1-l2-rebuild.md)。

## 职责与依赖

| 层级 | 责任 | 核心实现与接缝 |
|---|---|---|
| L1 Model Invocation Engineering | 校验完整请求、协议编码、HTTP/SSE、归一化响应与调用失败 | internal/llm；不依赖 Agent、Graph、记忆与 UI |
| L2 Context Engineering | 指令、目标、记忆、历史、工具与非文本装配；请求封存；响应重放检查与输出订阅 | internal/contextruntime、internal/contextcontract、internal/contextcompiler；存储由 L3 端口注入 |
| L3 Harness Engineering | 工具权限、Lease、ToolRouter、工具执行、Store、Effect 与工作环境 | internal/agent/execution_lease.go、tool_registry.go、tool_router_snapshot.go；internal/tools、store、effect、workspace、shell、gate |
| L4 Loop Engineering | Activation/Attempt/Turn、进展、重试、停止与 finalizing | internal/agent/agent.go::processTask、state.go、loop_progress.go、finalization.go；internal/runner 的认领与运行外壳 |
| L5 Graph Engineering | 图定义与 Activation 编排、输入输出、验收、交付 | internal/graph、internal/delivery；internal/bootstrap/graph_*.go；internal/scheduler 的图编排逻辑 |

五层是责任域，不与包一一对应。Memory 的召回、投影和语义更新属于 L2；memory/taskmem/session 的持久化属于 L3。LLMExecutor 的工具 gate/dispatch 属于 L3，其调用 L2 的接缝不拥有上下文组装权。Trace/UI 是横切面，不构成第六层。

## 一次调用

L4 给出当前阶段与调用身份，L3 冻结执行规格，L2 收集材料并裁剪编译，封存完整 Request 与 ContextSnapshot。Snapshot 持久化成功后才调用 L1。L1 不补提示词、模型或预算，不读取业务文件，不更改消息内容。HTTP 自动重试关闭，Responses 与 Chat Completions 均只支持 SSE，禁止协议自动回退。

外部 chunk → L1 SSE 解析与聚合 → L2 增量投影；L1 完整 Result → L2 重放检查和必需记录 → L3 工具 gate/执行 → 结算事实 → L4 决定下一轮。EOF 不等于成功，部分参数不得执行，模型调用完成不等于任务完成。finalizing 与 Effect unknown 规则保留；后续工具的 skipped receipt 补齐协议交换，不触发新动作。工具视图按角色/权限冻结，不再按 Observation/决策轮次切成单工具阶段。

## 请求与内容

L2 显式装配角色指令、任务目标、输出要求、长短期记忆、有序对话、typed 上游输入、工具 schema 和当前输入。角色文件在启动期加载，L2 决定覆盖与使用。稳定材料按其身份复用，每次调用封存自己的请求。Raw History 不变，工具交换保持原子。上下文容量与协议上限属于版本化 policy；不再按默认进展轮数强制报告、停止或交接。

Request 具备版本、身份、协议、模型、能力、完整输入、工具与选项。摘要覆盖全部模型语义；封存后不得修改。文本、图片、文件分别类型化；非文本需声明能力与预算，不能裁剪为半个附件。provider replay 带协议身份并遵守 RequiredExact/Optional。

## 不兼容切换

新 L1/L2 不读取或转换旧请求、Prompt Build、Binding 或旧 Context 策略。ContextSnapshot v2，Context v11，Replay v5。文件配置必须显式声明 llm.request_contract=agentgo.model-request/v1，llm.stream 已退役。旧历史拒绝进入执行，不删除磁盘文件；新 Context 使用独立版本目录。四类工具切换的新 Run/Lease/Session/Graph 版本与目录见 [当前冻结基线](contract-freeze-2026-08-30.md)；旧磁盘业务事实仍保留。

## 输出订阅

L2 提供 WatchModelOutput。eventCursor 中文为“流式事件游标”，英文为“SSE events cursor”，由 L2 管理进程代次、范围与事件位置。快照和后续增量原子衔接；事件缓存有界，过期或慢消费者明确重新同步。重启恢复完整文本，不逐 chunk 持久化。UI Hub 只转接，完整输出持久化不得依赖 UI 存在。

## 完成门槛

旧入口与兼容路径删除、全部调用方接入、分层/全量测试与 vet/build 通过、二进制 SSE 端到端产物验证、真实协议定向验证。不能用历史测试结果证明新实现完成。L1/L2 原重建证据见 [实施记录](l1-l2-rebuild.md)。当前四类工具切换按用户要求执行 Go、离线和本地二进制验证，真实 SWE 暂缓，阶段结果与删除对账见 [工具契约](tool-taxonomy-and-contracts.md)。

## 实现文件索引

以下目录按责任归属列出；混合包的接缝按函数划分，存储实现不归入 L2。

### L1 请求与协议

- [`internal/llm/budget_contract.go`](../../internal/llm/budget_contract.go)
- [`internal/llm/client.go`](../../internal/llm/client.go)
- [`internal/llm/content.go`](../../internal/llm/content.go)
- [`internal/llm/errors.go`](../../internal/llm/errors.go)
- [`internal/llm/failure.go`](../../internal/llm/failure.go)
- [`internal/llm/invocation_timing.go`](../../internal/llm/invocation_timing.go)
- [`internal/llm/output_budget.go`](../../internal/llm/output_budget.go)
- [`internal/llm/protocol.go`](../../internal/llm/protocol.go)
- [`internal/llm/reasoning.go`](../../internal/llm/reasoning.go)
- [`internal/llm/request.go`](../../internal/llm/request.go)
- [`internal/llm/responses_client.go`](../../internal/llm/responses_client.go)
- [`internal/llm/sse_transport.go`](../../internal/llm/sse_transport.go)
- [`internal/llm/tool_choice.go`](../../internal/llm/tool_choice.go)

### L2 运行与编译

- [`internal/contextruntime/adapter.go`](../../internal/contextruntime/adapter.go)
- [`internal/contextruntime/codec.go`](../../internal/contextruntime/codec.go)
- [`internal/contextruntime/doc.go`](../../internal/contextruntime/doc.go)
- [`internal/contextruntime/history.go`](../../internal/contextruntime/history.go)
- [`internal/contextruntime/media.go`](../../internal/contextruntime/media.go)
- [`internal/contextruntime/memory.go`](../../internal/contextruntime/memory.go)
- [`internal/contextruntime/operation.go`](../../internal/contextruntime/operation.go)
- [`internal/contextruntime/output.go`](../../internal/contextruntime/output.go)
- [`internal/contextruntime/preflight.go`](../../internal/contextruntime/preflight.go)
- [`internal/contextruntime/provider_replay.go`](../../internal/contextruntime/provider_replay.go)
- [`internal/contextruntime/runtime.go`](../../internal/contextruntime/runtime.go)
- [`internal/contextruntime/types.go`](../../internal/contextruntime/types.go)
- [`internal/contextcontract/digest.go`](../../internal/contextcontract/digest.go)
- [`internal/contextcontract/doc.go`](../../internal/contextcontract/doc.go)
- [`internal/contextcontract/failure.go`](../../internal/contextcontract/failure.go)
- [`internal/contextcontract/history.go`](../../internal/contextcontract/history.go)
- [`internal/contextcontract/history_codec.go`](../../internal/contextcontract/history_codec.go)
- [`internal/contextcontract/policy.go`](../../internal/contextcontract/policy.go)
- [`internal/contextcontract/types.go`](../../internal/contextcontract/types.go)
- [`internal/contextcontract/validate.go`](../../internal/contextcontract/validate.go)
- [`internal/contextcontract/vocabulary.go`](../../internal/contextcontract/vocabulary.go)
- [`internal/contextcompiler/compiler.go`](../../internal/contextcompiler/compiler.go)
- [`internal/contextcompiler/doc.go`](../../internal/contextcompiler/doc.go)
- [`internal/contextcompiler/types.go`](../../internal/contextcompiler/types.go)

### L3、L4、L5 接缝

- L3：`internal/agent/context_bridge.go` 交付执行规格；`llm_executor.go::Execute` 负责工具 gate/dispatch；`execution_lease.go`、`tool_router_snapshot.go`、`tool_registry.go` 负责权限，`tool_call_identity.go` 绑定执行事实。`runtime_facts.go` 提供运行事实，`task_memory.go` 从结算账本收集事实并调用记忆语义更新。
- L3 存储/环境：`internal/contextstore/store.go`、`internal/contentstore/store.go`、`internal/taskmem/store.go`、`internal/memory/*store.go`、`internal/session/turns.go`；`internal/store`、`effect`、`workspace`、`shell`、`gate`、`tools`。Task Memory 的 render/update 语义属于 L2；不再生成模型 Observation。
- L4：`internal/agent/agent.go::processTask`、`state.go`、`loop_progress.go`、`finalization.go`、`submit_state.go`；`internal/runner/runner.go` 的认领与运行外壳；`internal/loopcontract`、`loopcontrol`、`loopprogress`、`loopstore`。
- L5：`internal/graph`、`internal/delivery`；`internal/bootstrap/graph_runtime.go` 及其它 graph bridge；`internal/scheduler/activator.go`、`scheduler.go` 的图编排入口；`internal/tools/graph_authoring.go`、`graph_schema.go`、`graph_routes.go` 的受控图接口。
- 组装与观察：`internal/bootstrap/bootstrap.go`、`runtime_builder.go` 注入依赖；`internal/ui/model_output.go`、`internal/dashboard/server.go`、`internal/tui/app.go` 只消费 L2 输出；`internal/trace` 保持跨层审计。
