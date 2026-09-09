# 工具与 Agent Profile

当前工具目录与实施进度见 [工具契约](design/tool-taxonomy-and-contracts.md)。旧 profile 说明已归档至 [历史版本](archived/tool-profiles-before-four-categories.md)。当前已注册新入口，旧名称没有兼容别名。

## 核心目录

| 类别 | 工具 |
|---|---|
| 执行 | run_shell、read_file、apply_change、submit_task_result |
| 编排 | read_graph_definition、apply_graph_change、control_graph、request_replan |
| 检视 | inspect_board、inspect_node、read_evidence |
| 通信 | send_message、request_user_input |

web_search/web_fetch、list_agent_templates/provision_agent_team 是可选能力，不属于核心 13 项。目录权威在 [known_tools.go](../internal/tools/known_tools.go)；不能以目录数量代替实际注册与授权检查。

## 配置

每个 agents 项必须在 profile 引用和 tools 内联列表中二选一。profile 只是工具名集合，不会使工具获得越过路径、任务或图作用域的权限。

```yaml
tool_profiles:
  executor:
    - read_file
    - apply_change
    - run_shell
    - inspect_node
    - read_evidence
    - send_message
    - request_replan
    - submit_task_result
agents:
  - kind: worker
    replicas: 1
    event_type: ""
    profile: executor
    model: ${SWE_FLAG_SHIP_MODEL}
    system_prompt_file: prompts/worker.md
    task_max_retries: 3
```

完整可加载示例见 [config.example.yaml](../config.example.yaml)。测试配置为 [setting.v4.yaml](../setting.v4.yaml)、[setting.test-concurrent.yaml](../setting.test-concurrent.yaml)、[setting.swe-flask.yaml](../setting.swe-flask.yaml)；最后一个由 Python 渲染占位符，不直接启动模板。

文件配置必须显式包含 llm.request_contract=agentgo.model-request/v1。协议只接受 responses/chat_completions，均为 SSE。llm.stream、observation_model 和 max_subtask_depth 退役；旧文件须删除这些字段后使用新工具名，不能靠设零或隐藏 fallback 继续旧行为。

## 角色权限

- Scheduler 原始请求使用图工具和检视/通信；图中的 controller 处理编排。solo 模式执行工作也通过显式 agent 节点，不恢复脱图执行旁路。
- Worker 可声明文件读写和 Shell。Explorer 可用 Shell 搜索，但不给 apply_change 不代表 Shell 在操作系统层面只读；环境隔离与角色指令分别负责各自边界。
- Verifier/acceptance 的只读闭集由 agent.IsAcceptanceToolAllowed 统一提供给 Graph 路由和 ExecutionLease 校验，包含 read_file、inspect_board、inspect_node、read_graph_definition、read_evidence、可选 web 工具及 submit_task_result。授权通过后仍必须实际注册该工具。
- 普通 Graph agent 不能通过自选 capability 获得 apply_graph_change/control_graph；修改图要 request_replan，由有编排权限的主体应用。节点 kind 是角色权威，不根据自定义 route 字符串猜权限。
- per-node capability 是 route 能力与运行策略内的子集，越界明确拒绝。新的 ExecutionLease v3 冻结本次能力，ToolRouter 同时约束模型看到的 schema 和实际 dispatch。

内置模板为 builtin/generalist@2、builtin/explorer@2、builtin/verifier@2。可选 Team 必须先注册到实际图作用域且达到 ready，再提供路由；模板声明不代表活跃 Agent 状态。

## 执行与检视

apply_change 统一创建、覆盖与替换；旧 write_file/edit_file 已删除。所有 run_shell 调用经统一通道记录关联身份、实际派发、进程状态、退出码作用域、耗时与输出。取消和超时不伪造成功退出，命令结果不等于 Python 判题结论。

inspect_board/inspect_node 是读取运行事实，不创建工作、不推进状态。read_evidence 按调用方的 Session/Run/Task/Graph 作用域解引用，不能拿 provider call_id 或任意路径冒充 EvidenceRef。

send_message 仅投递信息，返回投递回执；不承诺已读，不唤醒、不打断、不创建 Task/Activation。用户交互使用 request_user_input；未回答不等于批准。

## 删除与验证

run_check、record_observation_delta、submit_change_decision、旧模型草案步骤、旧消息发布任务与旧结果查询入口退役。依赖这些工具的提示词、模板、测试和配置必须同步迁移，不能只改 schema 名称。

配置检查使用 `agentgo config doctor`；Go 与本地二进制验证见工具契约第 13 章。真正的 SWE 评测仍由外部 Python 程序承担，本次不执行真实 SWE。

可选 Team 初建使用 `provision_agent_team(graph_request_id=R)`，随后 `apply_graph_change(create, request_id=R)` 使用同一个稳定值；图 ID 由运行时按调用者和请求身份派生，返回的 ready route 才能写入节点。图内 controller 扩展时继承当前图，不提供旧 task-scoped 模型入口。
