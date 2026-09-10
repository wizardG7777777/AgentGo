# 2026-09-09 SWE 批次：候选已修改，最终交付缺失

2026-09-11 只读核对本机 `AppData/Local/AgentGo/swe/runs` 与 `worktrees` 中的原始运行材料，未重跑真实模型或正式 Judge。原批次执行六题，零题解决，两题未运行。本记录不把 Agent 自述或内部 Shell pytest 当成正式外部判题。

## 写入与交付对账

| 题目 | 实际 apply_change 成功文件 | 磁盘与 Delivery 事实 |
|---|---|---|
| automatic-options | src/flask/sansio/app.py | 候选与 baseline 不同；主根仍等于 baseline；Delivery repairing |
| context-push-order | src/flask/testing.py | 同上；Delivery repairing；候选是正向遍历，主根仍是 LIFO pop |
| ipv6-server-name | src/flask/app.py | 候选与 baseline 不同；主根仍等于 baseline；Delivery repairing |
| secret-key-rotation | src/flask/sessions.py | 候选与 baseline 不同；主根仍等于 baseline；Delivery prepared |
| ipv6-session-txn | 无 apply_change | 实现节点因 L4 Assessment fingerprint.identity 含控制字符而 blocked；Delivery quarantined |
| pass-context-dispatch | 无业务文件/Shell 操作 | 实现节点的 WorkspaceManager/WorkspaceActivator 未装配，认领执行时 capability_violation；Delivery quarantined |

四题均由工具成功回执、隔离目录实际文件、`.workspace-manifest.json` baseline 哈希和最终主根文件哈希交叉确认。六题没有 committed Delivery，不能把最终 patch=0 解释成 apply_change 没有保存文件。

## 缺失的正式验收路径

从 `state/graph-authoring-v2/authoring.jsonl` 的正式 definition.body 核对：

- automatic-options，graph-c6e05d536ecffb1fd90161e13aca5e17：accept.kind=agent，虽然 metadata.role=acceptance。
- context-push-order，graph-387c5f2867ee8442ef0d6ebd6e388e5e：verify.kind=agent，且未声明 workspace 隔离。
- ipv6-server-name，graph-331db1a5c2dda8241f0af0a003d2053d：verify.kind=agent。
- secret-key-rotation，graph-49438d206738b29c464f43c4b36e7a72：fix_rotation 直接指向 end_ok，没有验收节点。

`internal/graph/acceptance.go` 的正式 acceptance 路径才调用 CommitDelivery。`internal/bootstrap/graph_runtime.go::prepareDeliveryTask` 对携带同一 Delivery 的后续普通 agent 调用 BeginRepair，对 acceptance 调用 BeginVerification。这与三个 repairing、一个 prepared 的磁盘状态吻合。普通节点名称、角色描述以及自然语言通过结论均不能代替 kind。

context-push-order 是明确的版本错位：implement 的 Shell 输出有 `487 passed, 2 skipped`，下游 verify 输出 `1 failed, 486 passed, 2 skipped`。候选 src/flask/testing.py 已保存正向遍历，主根仍保留旧 pop()。verify 认为“上游没有持久化”是模型对错位视图的解释，不是文件写入事实。

另外三题也有真实工具输出中的通过摘要：automatic-options 494 passed；ipv6-server-name 493 passed；secret-key-rotation 487 passed, 2 skipped。尚未重新用 Python 对候选输入身份进行正式验证，不能据此宣称四题修复已完全正确。

## 为什么轮数多

部分调用在反复修正图定义，而非调查源码。context-push-order 的原始 trace 有 14 次 apply_graph_change 请求，包含路由能力、出口与结构校验拒绝；运行中还有 Shell pipeline 拒绝、证据引用失败，以及普通节点填写 acceptance 专属 verdict 被拒绝。模型响应格式错误另行记录，不能统一归因为某个协议或模型能力不足。

四题已有 Task 最终均结束，图仍 running；长达 1200 秒的监控耗时还包含等待未完成交付，并非全部时间都在推理或调查。无正式 acceptance 的 mutating 图能够被提交和启动，是需要修复的跨层契约缺口。另两题是 L4 事实身份校验和 L3 执行面装配问题，需独立追踪，不能通过放宽文件写入或增加轮数解决。

## 本次改动与验证

终端不再输出包含全部 collected_nodeids 的 Judge JSON，只显示现有摘要、judge.json、pytest 日志与补丁路径；磁盘 JSON 内容和判题规则不变。SWE Test Runner 的 52 项 runner_test 离线测试通过。没有修改上述生产故障逻辑，也未修改或提升历史候选。
