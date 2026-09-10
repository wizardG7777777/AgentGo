# AgentGo 实施参考

本页描述当前四类工具与 agentTask 数据流链。历史机制参考已存入 [旧手册](archived/agents-reference-before-four-categories.md)，其中的 Observation、Check 和默认进展关卡不用于当前实现。约束入口为 [AGENTS.md](../AGENTS.md)。

## 启动与装配

1. main.go 读取 v4 YAML/JSON，展开环境变量并校验配置；角色文件在启动期按用户权限读取。
2. bootstrap 注入状态 Store、内容/Snapshot、Graph authoring/runtime、Delivery、Effect、workspace、消息与 UI；新目录与版本由冻结基线指定。
3. L2 对静态角色指令做预检；L1 transport 只持有连接、凭据和传输设置。启动工具探针验证所选 SSE 协议。
4. Scheduler 接收新用户请求并绑定 Run，使用图工具安排节点；Runner 根据静态或可选 Team 路由认领 Activation 对应的 Task。
5. Agent Loop 经 L3 冻结的工具/Lease 调用 L2；L2 持久化完整请求快照后调用 L1。完整响应与工具结算进入运行事实，最终 TaskOutcome 回填图。

未配置 deadline 的新 Run 不派生旧 verification/recovery/finalization 预留窗口，不再存在独立 Proposal Acceptance。传输超时、单条命令超时和用户显式限制独立处理。

## 主要实现入口

| 组件 | 文件/目录 | 职责 |
|---|---|---|
| 配置 | internal/config/config.go、doctor.go；config.example.yaml | 严格校验退役字段，核对提示词与实际工具能力 |
| L1 | internal/llm | 封存请求校验、编码、两协议 SSE、失败与时序 |
| L2 | internal/contextruntime、contextcompiler、contextcontract | 装配/投影/封存/响应记录与输出订阅 |
| 调用与工具 | internal/agent/llm_executor.go、tool_call_identity.go、tool_router_snapshot.go | 同一工具视图 advertise/dispatch，调用身份与执行回执 |
| Loop | internal/agent/agent.go、loop_progress.go、finalization.go | 每轮事实、错误/取消、唯一终态；无强制观察阶段 |
| 工具组 | internal/tools/known_tools.go、group.go | 核心 13 项及可选 Web/Team 能力 |
| 文件/Shell | internal/tools/local_read.go、local_write.go、shell.go；internal/shell | 文件边界、版本/锁、命令执行与结果事实 |
| 运行权限 | internal/agent/execution_lease.go、internal/gate | route/工具/策略交集；agentTask 没有验收类型特权 |
| 图工具 | internal/tools/graph_authoring.go、graph_schema.go | 初建/更新的原子校验提交与生命周期控制 |
| Graph/Delivery | internal/graph、internal/delivery；internal/bootstrap/dataflow_runtime.go、task_outcome.go | 输入就绪、唯一 Task、候选/结果与图级交付 |
| 检视 | internal/tools/inspection.go、content_ref.go、graph_evidence.go | 范围内读取事实与完整内容引用 |
| 通信 | internal/tools/meta.go、agent_question.go；internal/mailbox | 仅信息投递与独立用户交互 |
| 存储 | internal/store、loopstore、outcomestore、contextstore、contentstore、taskmem、session | 对应责任域事实持久化与恢复边界 |
| UI/Trace | internal/ui、dashboard、tui、trace | L2 输出消费、状态展示及事实诊断 |

五层不能按整个 agent/runner 包归类；函数职责与依赖方向见 [五层规范](design/five-layer-engineering-architecture.md)。TaskMemory 的 render/update 语义属于 L2，底层存储属于 L3；不再依赖模型 Observation。

## 图创建与变更

read_graph_definition 读取正式定义；apply_graph_change(create) 校验并提交新定义；control_graph(start) 启动。动态调整使用 apply_graph_change(update)，提供 expected_revision、稳定 request_id、changes（add/update/remove）。冲突或非法修改返回原因，正式图不变；模型不再依次调用草案创建/配置/校验/提交工具。

编排由图外 Scheduler 处理，agentTask 使用 request_replan 请求调整。无变更协调与 final-report 经 submit_task_result 按各自作用域收口；这不是旧 report_done/decision 工具的别名。消息不能代替图事务。

Graph 维持单赋值端口、角色能力与单 mutable producer 的 Delivery 基线。Tool 节点的机械执行桥仅开放 read_file；带 Shell/文件副作用的工作由有完整 L3 权限与执行事实的 Agent 节点执行。

## 执行与返回

apply_change 共享创建/覆盖/精确替换的路径验证、逻辑锁、哈希/行锚点验证、临时文件提交、产物与 Effect 记录。run_shell 的正常非零退出通过执行结果表达；框架拒绝/启动失败/超时/取消分别记录。结果被外置时通过 read_evidence 读取，不把正文截断后丢弃原文。

ShellExec v2 的 process_started 说明是否真正创建进程，exit_code 缺席表示没有可报告退出码；tool_dispatched 仅指 Registry 派发。所有这些字段通过调用身份关联，不能靠相邻日志或工具名去重。

submit_task_result 通过角色、产物与终态校验后进入 finalizing；同一响应的后续工具仅写 skipped receipt，不执行。Graph success 必须有真实结果与所需 Delivery 提交；命令成功、模型文字与文件存在本身不能替代该事务。

## 运行与历史

```text
go build -o agentgo.exe .
./agentgo -config setting.yaml
./agentgo config doctor
./agentgo trace list
./agentgo trace show <task-id>
```

--resume 和 /session 只进入可接受版本的历史，启动不自动读取 active-session 续跑。新的 Session 8、Lease v4、Run v3 与 Graph v6 使用当前目录；旧会话不静默转换，不删除旧磁盘数据。完整版本表见 [冻结基线](design/contract-freeze-2026-08-30.md)。

UI 的模型输出入口为 WatchModelOutput，eventCursor 由 L2 产生。TUI 默认 Chat inline，图/结果详情进入 alt screen；全屏期间定稿输出先排队，回 Chat 后写入 scrollback。模型轮次持久化不依赖 UI。

## 验证与资料

Go 全量测试、vet、构建与真实二进制产物是跨层变更基本证据。本地双协议脚本 scripts/local_fake_provider_smoke.py 验证新建图、动态更新、非法修改拒绝、连续读取、普通消息不创建接收者任务、文件/Shell、验收与 Delivery。它不运行 Flask 或真实 API。

SWE Test Runner 的环境、进程/批次和独立判题见 [README](../scripts/swe_test_runner/README.md)。当前用户暂缓真实 SWE；离线测试和本地 fixture 不能作为真实成功率。阶段验证与删除对账见 [工具契约第 13 章](design/tool-taxonomy-and-contracts.md)，仍未验证的事项记入 [KNOWN_ISSUES](activate/KNOWN_ISSUES.md)。

可选 Team 初建使用 `provision_agent_team(graph_request_id=R)`，随后 `apply_graph_change(create, request_id=R)` 使用同一个稳定值；图 ID 由运行时按调用者和请求身份派生，返回的 ready route 才能写入节点。图外规划任务扩展时继承目标图，不提供旧 task-scoped 模型入口。
