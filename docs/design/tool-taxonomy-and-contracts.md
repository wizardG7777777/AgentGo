# 工具分类与契约

状态：当前为 Graph v6 / agentTask 数据流图。旧 Graph 控制节点、强制 Proposal Acceptance、出路检查与独立检查工具已退役。此前讨论和验证原文见 [历史记录](../archived/tool-taxonomy-before-agenttask.md)，不作为当前调用说明。

## 1. 四类核心工具

工具目录仍为 13 项；注册全集与某个 Agent 实际授权不同。工具请求、实际执行、节点结果、图完成与外部测试判题必须分别记录。

| 工具名称 | 权责范围 | 定义 |
|---|---|---|
| run_shell | 当前任务工作视图中的通用命令执行 | 接收 command/working_dir/timeout_sec 等执行参数，返回启动、退出状态和输出；记录 ShellExec 和文件差异，不生成 pytest 判题 |
| read_file | 读取授权工作视图文件 | 路径及行范围，返回内容与版本信息；不能越过工作区/框架状态边界 |
| apply_change | 文件创建、覆盖、精确替换 | operation=create/write/replace；路径、内容或替换参数，经同一版本、逻辑路径锁、写入和产物记录链 |
| submit_task_result | 提交本 agentTask 的唯一结果 | summary、result、可选 blocked/blocked_reason；结果满足声明 schema 后进入 finalizing，后续工具被 fence |
| read_graph_definition | 读取图结构、状态及能力目录 | 无 graph_id 读 route_ref/工具目录；指定图读取当前定义和版本，节点分页必须固定 revision |
| apply_graph_change | 初建或增量修改同一图 | create 输入 objective/nodes；update 输入 graph_id/expected_revision/changes；内部机械校验和提交，无独立模型审批 |
| control_graph | 图生命周期与图级交付 | start/cancel/complete；complete 选择真实 ResultRef/候选、核对结算并实际交付后写终态 |
| request_replan | 向图外 Scheduler 登记规划请求 | request_id/reason；持久化请求事实，不直接修改图，不重开当前任务 |
| inspect_board | 检视当前 Run 工作与图事实 | 按 Graph/Agent/状态过滤，分页核对 snapshot_digest；也可检查请求回执 |
| inspect_node | 检视任务实例及其执行 | Task 或 Graph/Node 身份，返回状态、结果和完整事实引用 |
| read_evidence | 解引用已授权的大内容和证据 | Ref 与字节范围；持续核对 Session/Run/Graph/Task 和 Lease，不把任意路径当 Ref |
| send_message | Agent 间仅信息传递 | info/question/reply 和回复关联；不唤醒、不发布任务、不授予权限或修改图 |
| request_user_input | 当前任务请求用户输入 | 经统一 Interaction 服务等待回答；不生成 approval 节点 |

前四项为执行类，中间四项为编排类，后三项检视与最后两项通信分别如表排列。web_search/web_fetch 与可选 Team 工具属于显式扩展，仍须注册并授权。

## 2. agentTask 定义

一个节点表示确定任务及确定结果。kind 唯一为 agentTask，nodes 是包含 node_id 的列表。输入槽映射到 node_result 或版本化 graph_input；结果使用明确 JSON Schema 子集（type/properties/required/items/additionalProperties/enum）。

没有 root/next/when/end_outcome、特殊验收/控制角色或循环回边。普通任务直接支持多个输入。已激活节点的定义和输入冻结，完成结果不覆盖；迭代通过追加新节点实例表达。

创建可以只有调查，不要求所有未来交付路径已经设计好。缺输入等待，不能伪造数据或自动完成；当前工作做完后由持久化事件唤醒图外 Scheduler 继续规划。

## 3. 图工具参数

```text
read_graph_definition()
  -> 当前可用 route_ref 与工具目录；default 是默认工作队列

apply_graph_change(operation=create, request_id=R,
  definition={objective, constraints?, inputs?, nodes:[agentTask...]})
  -> graph_id, revision, source_request

control_graph(action=start, graph_id, expected_revision, request_id)

apply_graph_change(operation=update, graph_id, expected_revision, request_id,
  changes={add:[agentTask...], update:[尚未激活的任务...], remove:[未激活ID...]})

control_graph(action=complete, graph_id, expected_revision, request_id,
  outcome=success|failed|blocked, summary, result_refs:[实际引用...],
  selected_candidate_ref?, dispositions?)
```

相同请求身份不能换内容；重复请求不制造新图/任务/提交。更新冲突须重新读取版本。运行图新增就绪任务后自动派发，不再次 start。complete 不取消在途任务，不隐式忽略失败或未完成工作；需要取消时用 cancel 并核对结算。

## 4. 候选版本与交付

文件修改首先进入当前任务视图，冻结成不可变候选。下游的工作基线由输入引用决定，多候选显式 workspace_input。普通检查任务不会因名称或角色得到特殊提交能力。

complete 在图级串行提交选定候选：主根基线冲突拒绝覆盖，Effect unknown 不自动重放。Runtime 校验实际文件与回执，不替 Python 断言测试通过。

submit_task_result 不再接受 event/verdict/cited_evidence/request_replan 专属参数。检查结论写入普通 result，必要重规划单独调用 request_replan；写入 summary=通过不等于图完成。

## 5. Team 与权限

Team 初建使用 provision_agent_team(graph_request_id=R)，随后 apply_graph_change(create,request_id=R) 使用相同稳定值。只使用返回的真实 route_ref/event_type，不猜 worker-1 等 Agent 名称。图外规划任务继承目标图作用域，图内 agentTask 不能借角色标签获得编排权限。

## 6. 实现、删除和验证

当前文件索引见 [五层规范](five-layer-engineering-architecture.md)，版本拒绝见 [冻结基线](contract-freeze-2026-08-30.md)。agentTask 的完整实施范围见 [计划](dataflow-graph-simplification-proposal.md)。

删除覆盖旧 Graph 节点与分派、控制边、验收/审批/子图桥、强制 Proposal Acceptance、L4 intervention 投递链、旧结果字段和旧 /event 接口。保留并迁移的是工具/结果/证据身份、工作区、Effect 和唯一终态，不能用新 wrapper 保留旧入口。

本地两协议二进制和 Team 场景已完成实际文件交付；真实 SWE automatic-options 完整通过：494 passed、28 行补丁、task_resolved=true。其它七题未据此宣称通过。Python 仍是测试输入身份、失败集合比较和正式判题的唯一权威。

失败反馈使用显式 node_outcome 输入引用已持久化终态事实；它与 node_result 成功业务结果分开。追加的诊断/修复仍为 agentTask，不是恢复控制节点。
