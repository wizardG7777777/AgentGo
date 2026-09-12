# 2026-09-11 agentTask Flask-8 真实全量测试

状态：完整批次已结束，8/8 有最终判题，6/8 修复成功、2/8 因未交付而失败。此文记录当前二进制的实测事实；下面的实现问题尚未修复。

后续状态：用户随后要求停用正文引用/历史裁剪并允许普通文本结束，相关路径已按 [原文模式实施记录](../design/raw-context-and-plain-results.md) 更新。本报告保留原受测版本和原始成绩；各问题的当前状态以该记录及 KNOWN_ISSUES 为准。

## 受测身份与范围

- 源码提交：`f5d513367c70ddbe100104457801d2bffbf0dea7`。
- 当前 Windows 工作区重新执行 `go build -o agentgo.exe .`；二进制 SHA256：`1746127DC88DA439017B49397B2EB4C0902F0C3F495D86AFF012EA4E6017CFC2`。
- 正式入口：`py -3.13 -u -X utf8 scripts/swe_test_runner/runner.py batch --timeout 1200`；使用 Flask-8 的完整 tasks.csv，逐题执行目标红态、全量红态、AgentGo、最终全量 Judge。
- 测试根：`C:/Users/73524/AppData/Local/AgentGo/swe-agenttask-v6-batch-20260911-101831`。2026-09-11 18:18 AWST 启动。保留旧测试根，四个必需变量仅检查非空，不记录凭据。
- 批次开始后未修改生产代码、模型提示词、工具契约或判题规则，未向运行中的 Agent 注入修复建议。辅助 observe.py/analyze.py 仅在测试根读取运行证据。

## 全量结果

批次从 18:18:31 至 19:41:55 AWST，约 83 分 25 秒。所有题均完成四阶段流程，没有 not_run 或旧结果复用；Runner 退出码为 3，表示题目失败。协议为当前配置的 Responses SSE，实际运行平台为 Windows。

| 题目 | 最终判题 | pytest passed / failed / skipped | 补丁行数 | 执行秒数 | 已结算模型调用 | 架构审计 |
|---|---|---|---:|---:|---:|---|
| automatic-options | resolved | 494 / 0 / 0 | 25 | 516 | 139 | true |
| context-push-order | resolved | 487 / 0 / 2 | 13 | 401 | 84 | true |
| ipv6-server-name | resolved | 493 / 0 / 0 | 19 | 89 | 17 | true |
| ipv6-session-txn | resolved | 492 / 0 / 0 | 13 | 141 | 28 | true |
| pass-context-dispatch | failed | 487 / 3 / 0 | 0 | 1201 | 316 | null，执行未收口 |
| secret-key-rotation | resolved | 487 / 0 / 2 | 13 | 138 | 26 | true |
| session-access-tracking | failed | 489 / 1 / 0 | 0 | 1200 | 286 | null，执行未收口 |
| teardown-callbacks | resolved | 495 / 0 / 0 | 126 | 1202 | 281 | true，见时间边界说明 |

最终八题的 error events 均为 0、tampered 均为 false，模型契约检查 8/8 通过，基础设施错误为 0。逐 nodeid/phase 比较基线与最终失败集合，没有新增失败事件；两个失败题的失败集合未变。各题使用不同 Flask revision，不能把通过数之和当成一套测试的覆盖数量。

共结算 1177 次业务模型调用、19,393,440 prompt tokens、783,785 completion tokens；不包含批次及启动探针，cost_micros=0 不作为真实费用依据。两个失败 Run 各保留 1 次在途 Invocation 和 1 项 reservation，未结算工具为 0；其余六题无在途 Invocation/工具/reservation。没有 HTTP 错误或 llm_call_end 的 provider failure。两个 L2 编译阻塞发生在模型网络调用之前，单独列在下文。

按原调用去重，证据读取后再次被 L2 外置共 143 次：automatic-options 22、context-push-order 7、pass-context-dispatch 45、session-access-tracking 27、teardown-callbacks 42，其余为 0。

## 已发现的问题

### 1. 相同阻塞事实重复发布，引起图日志和规划任务膨胀

`pass-context-dispatch` 的调查节点没有候选版本，Scheduler 却为后续实现节点指定 `workspace_input=inventory/investigation`。Runtime 正确拒绝派发，保留 `invalid_input:workspace_input=... 未提供候选版本`。Scheduler 先后追加了三个带相同配置错误的节点。

随后 Runtime 每次 Step 都重新登记这些节点的 `execution_blocked`。`enqueuePlanning` 只对最后一个未确认事件去重：多个阻塞节点交替出现时，相同节点和原因仍会反复追加；已确认的相同原因也会再次触发。每次新增事件又持久化完整图快照，Bootstrap 持续创建额外规划任务。

直接代码：`internal/graph/dataflow_runtime.go::Step` 的 waiting 分支；`internal/graph/dataflow_store.go::enqueuePlanning`；`internal/bootstrap/dataflow_runtime.go::plan`。

实测中，图定义只有 6 个 revision，状态版本却达到 2831，规划确认序号达到 2806，图日志约 234 MB。模型在第 6 版移除三个无效等待节点后，日志增长和重复事件停止。不能将该现象解释为模型进行了数千次有效图变更。

本题最终日志封存为 235,810,870 bytes、2845 条快照记录、7 个定义 revision。按持久化事件 sequence 去重后共有 2811 个规划事件，其中 2804 个为 execution_blocked：impl-dispatch 987 次、impl-dispatch-2 986 次、impl-dispatch-ctx 831 次；其它事件只有 4 个 task_terminal 和 3 个 planning_needed。

后续修复应以“节点的等待原因/相关输入版本发生变化”为发布条件，按节点事实去重并明确确认语义；增加多个等待节点反复 Step、规划确认后仍无新事实的测试。不增加模型轮数上限或强制 record 关卡。

### 2. 检视接口缺少图节点的关键事实

`inspect_board/inspect_node` 以 TaskStore 为主要来源。`taskInspectionSummary` 返回 `outcome_ref`，没有图层的 `result_ref/candidate_ref`。`control_graph.complete` 却要求图的 ResultRef。`automatic-options` 和 `context-push-order` 均发生了把 OutcomeRef 传入完成接口而被拒绝的调用。

尚未派发的节点没有 Task，`inspect_node(graph_id,node_id)` 返回“未找到当前范围内的节点任务”。第五题的无效输入和等待原因实际存在于 Graph Store，但模型不能通过这个简短入口看到。UI 图快照可以显示该原因，模型工具与 UI 的可观测范围不一致。

完整 `read_graph_definition` 虽然携带 executions/results，但结果与工具证据混在大对象中。应从 Graph Store 投影精简节点状态、waiting_reason、ResultRef、CandidateRef、输入来源，Task/Attempt 作为实际存在时的执行事实附加；保留 OutcomeRef 与 ResultRef 的区别，不增加别名兼容。

代码：`internal/tools/inspection.go::taskInspectionSummary/inspectNode`、`internal/tools/graph_authoring.go::readGraphDefinition/controlGraph`、`internal/bootstrap/ui_graph.go`。

### 3. 证据解引用结果再次外置

`read_evidence` 默认读取 32 KiB、上限 64 KiB，然后把内容作为 JSON 字符串返回。L2 的工具结果片段限制为 48 KiB/12 Ki tokens；JSON 转义和协议编码也占预算。合法的读取页可能再次超限，被 `adapter.go` 转成新的 ContentRef。模型随后读取新的引用，形成嵌套解引用和重复检索。

已按工具 CallID 与 ContextSnapshot 的 source_ref 关联确认：第一题有 22 次不同的 read_evidence 调用因 fragment_limit_externalized 再次外置，第二题有 7 次。统计按原调用去重，不把同一历史在多轮 Snapshot 中重复出现算作新调用。

第五题同类调用为 45 次。该题还记录了 34 次 control_graph 拒绝；Scheduler 占已结算模型调用的 183/316，表明问题不只是业务修复模型花费较多轮次。

后续应确保每一页的实际编码结果能够进入当前 L2 请求，返回可继续读取的原引用和偏移；保留 UTF-8/跨字节分页、取消、授权与预算约束。不能通过取消预算或无限内联全部图日志解决。

代码：`internal/tools/content_ref.go`、`internal/contextruntime/adapter.go`、`internal/policycatalog/defaults.go`。

### 4. Shell 隔离副本中的 Git 视图与实际代码不一致

Shell 工作区位于原 worktree 的 `.agentgo/workspaces/.../.workspace-shell`。`copyProjectTree` 排除 `.git`，但没有使普通 Git 命令形成独立的候选仓库视图。Git 会向上发现原 worktree，所以 `git diff -- src/flask/testing.py` 可返回空，而当前 Shell 目录中的文件已经从 `pop()` 改为 `pop(0)`。

第二题的真实输出同时给出了 `.workspace-shell` 的 Get-Location、主 worktree 的 `git rev-parse --show-toplevel`、空 diff 和实际文件差异。模型最终通过额外文件比对确认修改，最终 Judge 通过，说明本次不是没有写回；但默认 Git 观察面具有误导性。

后续需要为候选提供一致的 VCS/diff 语义，或明确阻断向父仓库发现并提供可用的候选差异；不能把父仓库无差异当作候选无修改。代码：`internal/workspace/shell_root.go::prepareShellRoot/copyProjectTree`。

### 5. L2 分区预算拒绝使正常检查节点阻塞，诊断字段未完整落盘

`session-access-tracking` 的 `verify-session-access-1` 在 2026-09-11 19:07:33 AWST 被运行时置为 blocked，原因是 `context_assembly_rejected / section_budget_exceeded`。这是 L2 编译拒绝，不是外部 HTTP 错误，也不是模型通过 submit_task_result 提交的业务复核结论。该节点实际绑定了修复候选 `candidate:5aa5d025a7ae4ed770a9b62158af706c7b3c86aaa4b67c639cd24c8f805bd09b`，没有候选缺失。

最后成功 Snapshot 对应 `agent-task-f073320965e1240f902824deddabad92/attempt-1/turn-23/invocation-23`。随后装配失败只在 Task/Graph 终态中保留通用错误文本。`ContextAssemblyFailure` 在编译器内有 Section/Actual/Limit，但其 Error() 没有这些值，运行日志不能直接说明哪个分区超出多少。首轮测试据此仅确认故障层与拒绝原因；本次补充历史重放的结果见下，不能用前一个成功 Snapshot 的占用冒充失败请求的实际值。

后续须保留装配失败的结构化 Section/Actual/Limit/Invocation 身份，并用本次历史重建确定性回归。调查重点包括 ProjectHistory 保留近期原子组后的实际编码开销、单片段外置与分区总量的关系，以及编译失败前是否还有合法投影空间；不能简单放大或删除全部预算。

Scheduler 随后新增 `verify-session-access-2`，替代检查完成；原 blocked 节点保持终态。这是新实例处理失败的实测路径，不代表 L2 故障被修复。

最后一题 `teardown-callbacks` 的 `verify-fix-1` 也复现相同 `context_assembly_rejected / section_budget_exceeded`，随后 `verify-fix-2` 完成。其原 blocked OutcomeRef 为 `outcome:sha256:32a7bde5e1163c9c66059a259dd24898ffbab535a664d4626aaf9e8f36d9a35a`。本批至少两个真实检查任务因此被运行时阻塞。

代码：`internal/contextruntime/history.go::ProjectHistory`、`internal/contextcompiler/compiler.go::enforceSectionBudgets`、`internal/contextcontract/failure.go::ContextAssemblyFailure.Error`、`internal/agent/llm_executor.go::contextAssemblyFailure`。

### 补充因果调查：L2 历史侧的离线重放

根据用户要求逐项解释原因，进一步从原始 Session 的 turns.jsonl 重建 assistant 内容、工具调用及 provider replay，再按 InvocationID/CallID 从 snapshot.json 的 ToolCallRecord 关联完整工具结果。调用当前未改动的 ProjectHistory 和 Assembler，不执行 Invoker，不访问模型，不写原日志。诊断 ContentStore 使用独立目录。

两个被阻塞节点都复现了 tool_results 分区超限：

| 节点 | 失败前真实历史轮数 | 生产策略保留的最近完整交换 | 历史侧重放估算 tokens | 分区上限 | 诊断中仅保留最后 2 轮后的 tokens |
|---|---:|---|---:|---:|---:|
| verify-session-access-1 | 23 | 21、22、23 | 33126 | 32768 | 20357，通过 |
| verify-fix-1 | 20 | 18、19、20 | 33200 | 32768 | 22031，通过 |

前一个历史前缀各自可以编译，增加最后一次已经完成的工具交换后则被拒绝。两个失败的历史侧编码字节数分别为 99371、99594，仍低于 131072 字节上限；触发的是估算 token 维度。Session 题目前一个前缀的重放 tool_results 为 53491 bytes / 17832 tokens，与原成功 Snapshot 的 53497 / 17835 接近。

根因是策略和校验没有形成闭合过程：HistoryProjectionKeepRecent 固定为 3，ProjectHistory 在保留不足 3 轮时继续保留，超过预算也不能再删；单片段外置只处理单条结果自身超限，没有保证多条合计满足分区预算；编译器之后发现分区超限即返回错误，Runtime 没有针对这个错误继续寻找合法投影。历史预估按原文长度，正式装配还计算 JSON 包装/转义及混合字符估算，两个阶段的开销口径也不同。

这里的“3 轮”是历史保留下限，不是 Agent 执行次数上限。诊断改为 2 轮只是证明存在保持完整 tool-call/result 配对的可编译历史子集，不是建议把所有场景简单改成固定 2 轮。应让预算决定可保留的完整原子组数量，并保持当前必需交换、引用可读性和 Raw History 不变。

上述数值是历史侧重放值，不冒称完整现场请求逐字节重现：未重建当时的 system、记忆等静态材料，message_index 等包装位置有少量差异。但真实历史本身足以触发相同分区错误，且保留更少完整交换后可编译，已经确定了这一缺陷的可复现机制。现场完整 Section/Actual/Limit 的诊断落盘仍待修复。

诊断证据在本次测试根的 cause-replay-session.json、cause-replay-teardown.json、cause-replay-source.go、cause-replay.exe；源文件的仓库临时副本已清理，生产代码和原始 SWE 成绩均未改动。

### 五项问题如何相互影响

1. 调查结果只有文字而没有 CandidateRef，仍被指定为 workspace_input，导致未来节点无法派发。该字段表示“使用哪个代码候选作为工作目录基线”，不是“读取哪份调查报告”。等候不会让已经结算的文字结果自动变成候选，需要修正未来节点定义。
2. inspect_node 又无法查询没有 Task 的等待节点，缺少简短、可直接消费的原因反馈。Scheduler 因此追加了带同样问题的节点；多个等待节点同时存在后暴露事件去重缺陷。
3. 每次扫描把不变的等待状态当成新事件，导致更多快照与规划。这里只确认额外落盘、事件与规划开销，没有证据表明程序因日志大小崩溃。第五题移除无效节点后已恢复执行，最终挡住交付的是结果引用检索。
4. 工具摘要提供 OutcomeRef，完成接口要求 ResultRef。正确 ResultRef 实际存在于完整图状态，并非丢失；完整状态却混入大量历史和证据，经过 L2 外置后难以获取。模型确实提交了错误引用，程序的工具返回和预算投影共同放大了出错和重试成本。
5. 读取页超限再外置，形成 content 引用指向另一个读取结果的嵌套。最早确认的一个默认页返回 35294 bytes，虽然正文长度低于 48 KiB，也可能在 JSON 包装或 12 Ki token 维度超限；不能只检查原始页字节数。过多完整工具结果还触发了上述分区预算冲突。
6. Shell 内的 Git 自动向上发现父仓库，使“实际执行的候选文件”与“Git 正在比较的文件”不同。context-push-order 中模型需要额外比对 .workspace-input 与 .workspace-shell 才确认 pop()→pop(0)。本批未证明该问题单独导致失败，但它确实制造了错误观察和额外调查。

直接交付失败链为：修复候选存在 → 结果摘要缺关键引用 → 检索大段状态 → 读取结果反复外置 → complete 使用错误引用被拒 → 候选一直未提交主根 → 外部超时 → Judge 仍测到旧代码。L5 重复事件与 L2 检查阻塞分别增加了这条链路前段的无效规划和替代检查成本。

## 判读边界

`architecture_ok=true` 只表示当前 Python 审计的身份、持久化、工具结算、图完成与 Delivery 等断言通过。它没有检测上述重复事件或检索放大问题。因此全量 Judge 通过与仍存在框架缺陷可以同时成立。Shell 管道退出码拒绝、工具参数错误和 pytest 红态需要各自分类，不能统一算作框架崩溃。

## pass-context-dispatch 的失败链

本题在 1201 秒达到外部 1200 秒检查窗口，由 Test Runner 结束进程。316 次调用已结算、511 次工具均有结果；另有 1 次在途 Invocation 和 1 项 usage reservation。`model_contract_compatible=true`、`infrastructure_ok=true`、`evidence_issues=[]`；但 `execution_complete=false`，`architecture_ok=null`，不能记为架构通过或推理 API 失败。

过程为：调查完成 → 带错误 workspace_input 的未来节点等待 → 同原因重复规划事件 → 追加多个无效节点 → 第 5 版派发正确实现节点 → 第 6 版清理无效节点、事件增长停止 → 修复及普通检查完成 → Scheduler 找不到正确 ResultRef，反复提交 OutcomeRef/节点 ID/Activation ID → 第 7 版追加复核 → 复核完成但图仍 open → 外部超时。

实现节点 `implement-dispatch-ctx` 和检查节点 `verify-dispatch-ctx` 都引用 `candidate:882f545adc8e17bca7fbd5cc2e3cc35c82b58c0ba8bce1b48e455db4852616a0`。其工作区内完整 pytest 曾报告 488 passed、2 skipped，但没有 committed Graph Completion/Delivery 把候选交付至主根。最终 Python Judge 实际读取主根得到 487 passed、3 failed、0 skipped、patch_lines=0、tampered=false，失败项为 `test_bad_environ_raises_bad_request`、`test_environ_for_valid_idna_completes`、`test_suppressed_exception_logging`。

因此本次“没有有效最终修复”的直接含义是有候选修改却没有完成交付；并非 Agent 从未写入，也不能仅凭候选内测试输出认定最终任务成功。该题 Run 为 `run-swe-pass-context-dispatch-ad0bc496-c819-428e-ad03-c4f700c2c5cb`，完整证据保存在同名 runs/worktrees 子目录。

## session-access-tracking 的失败链

修复节点完成并保存候选 → 第一个检查节点因 L2 分区预算被置 blocked → Scheduler 新增第二个检查节点并完成 → 多次 complete 使用 OutcomeRef/ContentRef 等错误引用 → 新增第三个检查节点并完成 → 仍不能正确收口 → 1200 秒外部超时。

本题已结算 286 次模型调用。最终 Judge 为 489 passed、1 failed、0 skipped、patch_lines=0、tampered=false；唯一失败仍是 `tests/test_basic.py::test_session_accessed`，主根 RequestContext 尚无 `_session`。候选内曾报告 488 passed、2 skipped，但没有正式交付。最终 `architecture_ok=null`、`execution_complete=false`，保留 1 次在途 Invocation 和 1 项 reservation。`model_contract_compatible=true`、`infrastructure_ok=true`；L2 装配失败发生在发送模型请求之前，不能从 `invocation_failures={}` 推断本次没有运行时错误。

Run：`run-swe-session-access-tracking-6c33e1ea-3fe2-4609-8e78-03425f220de6`。本次未延长超时或人工填 ResultRef；后续修复使用独立测试根比较，保留本次正式成绩。

## teardown-callbacks 的时间边界

本题修复前基线为 19 failed、476 passed、481 error events；大量 teardown 报错在基线中已经存在，不能仅凭错误数认定候选造成新增破坏。模型修复后候选内完整 pytest 达到 495 passed，随后也遇到一次 L2 检查阻塞和长时间引用检索。

19:41:14.991 的 control_graph complete 成功，19:41:19.682 规划任务完成，19:41:32.351 最终答复任务完成。Runner 的 1200 秒外部 deadline 先于其 30 秒终态稳定观察窗口结束，因此记录 `process_terminal=external_hard_kill`、`wall_sec=1202`。最终审计确认图已成功、Delivery 已提交、所有 Task 终态、无在途调用或 reservation；实际主根 Judge 495 passed、126 行补丁、task_resolved=true。

所以此处 HARD_KILL 是监控停止方式，不能单独决定业务失败。它也暴露了输出表达的改进空间：应同时呈现 deadline 是否命中、业务交付是否已完成、是否仍有未结算工作。本次保留原始标记，没有重新解释或改写原始记录。

## 证据和后续验证

测试根下的 `batch.log`、`validation-identity.json`、`runs/summary.json` 是本次入口及汇总证据。每题 `runs/<task>/` 包含 result.json、judge.json、model.patch、snapshot.final.json、四阶段 pytest 报告及执行身份；`worktrees/<task>/.agentgo/` 保留图、ContextSnapshot、历史、候选与 Trace。`analysis.json` 是按调用/事件去重的补充分析，`final-verification.json` 记录八题身份、测试篡改检查、失败集合比较、未结算项和二进制哈希核对。结束后没有遗留 AgentGo 进程。

优先修复路径：补齐图节点的简明结果/候选/等待事实接口，确保解引用页面可进入 L2 请求；对 L2 分区预算拒绝做确定性重放并保存结构化诊断；修复等待事实重复发布；使 Shell 候选的 Git/diff 语义与真实工作视图一致。新增回归应覆盖大工具历史、多等待节点、未派发节点检视、候选已完成后的图级收口，以及临近外部 deadline 的已交付状态。修复后先执行离线回归，再用独立测试根复测失败题和完整 Flask-8。

本次仅更新测试记录和当前问题/验收说明，未修复上述生产实现。此成绩不替代 Chat Completions、图片/文件输入或其它操作系统的真实 SWE 验证。

## 后续问答中的边界澄清

- **workspace_input 没有被普遍强制填写**：它是 inputs 内的槽名称，不是 workspace 路径或新的独立操作。Scheduler 当时已经在 apply_graph_change(update) 中新增修复节点，额外把报告槽指定成代码基线。当前实现会自动选择唯一候选；没有候选时从项目主目录建立工作快照；仅多个不同候选需要明确选择。物理工作目录由 L3 创建。此前“Scheduler 却被要求填写”的可能印象应纠正。
- **检视、任务结论和图级交付是不同动作**：工具事实、候选冻结、TaskOutcome 和 Graph Result 的持久化已有自动链路。inspect_node 查询这些事实。submit_task_result 目前还承担模型交付语义结果和进入 finalizing 的入口；control_graph.complete 选择整图交付。Scheduler 不应二次撰写 worker 的结果，引用和登记可由生命周期回调装配；当前仍依赖模型搬运 ResultRef。
- **还有固定自然退出约束**：agent.go 的 maxUnstructuredExitNudges=2，Graph 任务未调用 submit_task_result 而纯文本结束时先提醒，第三次走可恢复错误。另有 maxEmptyResponseStreak=3 的空响应守卫。应明确列出这些仍在执行的约束，不能用“Observation 已删除”概括成所有次数约束均已删除；本批未证明这两条是失败原因。若改为最终结果自动登记，须同时处理旧的强制工具出口，避免新旧链路冲突。
- **预算发生在每次请求发送前**：ProjectHistory 派生下一请求的历史视图，Assembler/Compiler 按片段和分区检查，成功才封存并调用 L1。裁剪的是旧 assistant/工具交换的请求投影，不删除磁盘 Raw History。Git 历史显示普通分区限额来自 2026-08-23 的 f3cbe0c；历史保留常量在 2026-09-07 的 4626bd3 中收敛，后续没有移除。这些人为 Context 限额不等于模型实际窗口，也不等于已删除的 Observation/进展预算；Python 的 --timeout 1200 又是独立测试期限。
- **引用的用户新要求**：认可大正文首次外置，但主动读取必须得到正文，禁止读取返回再次变成可递归引用；无法保证时回退正文直送。停用全部模型上下文引用还是仅删除二次外置的实施范围已另行询问，当前未修改生产代码，不能声称循环已经删除。
- **Git 的父仓库仍是本题 Flask 仓库**：没有跑到 AgentGo 仓库。问题是 shell candidate 没有自己的 .git，Git 向上找到主 worktree；候选文件没有作为独立工作树被跟踪。不能把复制 .git 文件视为修复，尤其 linked worktree 的 .git 可能继续指向共享的工作区元数据。
