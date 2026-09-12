# 原文上下文与普通文本节点结果

状态：已实现，Go 测试、vet、构建、68 项 Python 测试和本地二进制验证已通过。来自 2026-09-11 全量 SWE 后的用户明确调整；旧成绩及因果证据保留在 [全量报告](../test-issues/2026-09-11-agenttask-flask8-full.md)，不作为本次新实现的真实模型成绩。

## 已落实的行为

1. L2 不再把消息、工具结果、上游材料或 assistant 正文替换为 ContentRef，不再去重/摘要/裁剪 Raw History；已召回的 Session/依赖记忆正文也不按局部 rune 额度裁剪。输入图片/文件的授权读取与业务证据身份不属于此处退役的正文替换。
2. 删除 L2 片段、分区、原子组的自定义大小限额与“历史至少保留 3 轮”逻辑。保留已配置的模型整体窗口、输出额度及协议完整性检查；不能把一次输出拆成不完整的工具交换。SWE 所有角色继承 `default_context_window_tokens: 880000`，880K 按十进制配置。
3. read_file 重复读取仍返回所选范围的实际正文。删除缓存命中的摘要 stub 与 force_full 参数；缓存继续记录文件读取状态用于变更校验。工具自身显式 offset/limit 与读取范围语义保留。
4. agentTask 的最终纯文本可以正常结束。最后回复原样保存在 LastResponse 和 L2 输出账本；运行时自动形成带 plain_text 标记的 TaskOutcome/Graph Result。结构化 submit_task_result 仍可用，只有主动选择结构化提交才校验节点 result_schema。普通文本没有提交工具提醒次数关卡。
5. inspect_node 直接返回完整执行记录、最后回复及已生成的 ResultRef/CandidateRef；最终文本是 execution_records 的末条。删除“只返回 details_ref 再读取”的路径，不保留同一工具记录的重复正文副本。

普通节点结束不自动表示整张图的目标达到。Graph 的完成决定及实际文件交付继续走现有唯一终态事务。邮箱可以承载中途发现或结果通知，但不替代持久化节点记录；本次没有增加强制发邮件动作，也没有改变 send_message 的“不唤醒/不创建任务”规则。

## 删除与修改对账

| 类别 | 内容 |
|---|---|
| 删除 | contextruntime/history.go、history_projection_test.go；agent/tool_result_envelope.go |
| 删除 | ProjectHistory、历史最少保留轮数/摘要额度、重复读取外置；Assembler 外置及引用包装函数 |
| 删除 | Fragment/AtomicGroup 的 MaxSerializedBytes/MaxEstimatedTokens、SectionBudgets、Manifest 局部 BudgetLimit 及对应限额检查 |
| 删除 | Graph 纯文本退出的 maxUnstructuredExitNudges 与要求再次调用提交工具的循环 |
| 删除 | read_file 的 formatReadCacheStub、force_full 参数；inspect_node 的正文外置 |
| 重写 | Context policy、provider replay 的正文保真处理；普通文本终态映射；检视返回结构；角色输出说明 |
| 新增验证 | 大正文与重复历史全部进入请求；旧历史契约拒绝；纯文本一次结束；两种 SSE 协议的真实二进制纯文本节点与图交付；880K 配置约束 |

不删除 ContentStore、ResultRef/CandidateRef 或文件 ID：这些仍用于业务事实、身份校验和显式附件读取。本次取消的是用编号替换应交给模型的正文，不是取消图和存储的关联身份。

## 版本与存储

- ContextSnapshot：agentgo.context/v3；Context policy：context:default/v12；Replay：provider-replay:openai-compatible/v6；模型历史：agentgo.model-history/v2。
- TaskOutcome：agentgo.task-outcome/v5；TerminalIntent：agentgo.terminal-intent/v3；agentTask result：agentgo.agent-task-result/v2，增加运行时生成的 plain_text 标志。
- 新目录：context-snapshots-v3、model-probes-v3、task-outcomes-v4、graphs-v7。Graph 定义仍为 agentgo.graph/v6，目录代次不等同定义 schema。
- 旧目录原样保留，不读取旧投影历史作为新全文请求，不增加迁移或兼容包装。

## L3 调查结论与未完成事项

| 实现位置 | 已确认的问题 | 本次状态 |
|---|---|---|
| tools/graph_schema.go::graphNodeNativeSchema | workspace_input 由模型可见工具 schema 暴露，角色提示词没有单独要求填写；它用于多候选选择，SWE 单候选通常无需填写 | 解释来源；尚未更改多候选/字段设计 |
| workspace/shell_root.go::prepareShellRoot/copyProjectTree | Shell 副本嵌在主 worktree 内且排除 .git，没有独立 Git 工作树；Git 向上找到父 worktree | 已在后续 L3 修复中建立独立 Git 副本与输入基线 |
| tools/shell.go::resolveWorkingDir | cwd 按物理 shell snapshot 校验，而文件工具按项目逻辑根校验，相同绝对目录可能在工具间不可复用 | 已统一逻辑目录到任务副本的映射 |
| tools/local_read.go::readFile/formatReadFileResult | PathOverlayer 映射后把物理路径用于返回头；模型复用此路径时可能再被拼入另一个视图 | 已改为逻辑路径回显 |
| tools/local_read.go::缓存命中分支 | 不返回正文，要求模型回看历史或 force_full | 已删除 |
| tools/inspection.go::inspectNode | 完整结果隐藏在 details_ref，且缺少图级结果引用；Task 记录与图等待状态不是同一权威 | 完整正文/结果引用已直返；后续 L3 修复也覆盖未派发节点与等待原因 |

后续修复的文件、删除对账与独立验证见 [L3 工作区接缝](l3-workspace-seams.md)，下方原文模式验证仍保留当时事实。

L5 相同等待事件反复发布的问题未在本次更改范围内修复；没有更改 watchdog。全量 SWE 需用新二进制另开测试根重跑，不能复用旧 6/8 的成绩。

## 验证记录

- 原失败历史离线回放：Session 检查全部 23 轮、teardown 检查全部 20 轮均通过新 Assembler；逐条核对原工具结果正文一致，不再只留下最近 3 轮。工具结果分别约 153578 / 175622 估算 tokens，均在配置的整体模型容量内。
- 本地 Responses / Chat Completions SSE 二进制用例均通过普通纯文本结束、候选一致性和图级文件交付，结构化 result_schema 缺项不会阻止合法普通文本结果。
- 全量 go test ./...、go vet ./...、go build 通过；Python 68 项测试通过。Responses/Chat Completions 的普通文本场景均为 26 次本地调用，Responses 动态 Team 场景 27 次，均验证文件实际交付。长纯文本回复超过旧索引摘要限制时，完整正文仍保存在节点结果与 LastResponse，索引摘要不冒充完整正文。
- 本轮没有调用真实模型，未重跑完整 SWE；验证日志保存在本机 `%LOCALAPPDATA%/AgentGo/raw-context-*.log`，旧失败历史全文重放证据为 raw-context-replay-session.json / raw-context-replay-teardown.json。

后续状态（2026-09-12）：L5 等待事实去重、workspace_input 删除及测试终态判读已进一步修复；本文上面的未完成项为当时状态，当前实现及真实复测见 [剩余问题修复与复测](swe-remaining-repairs.md)。
