// Package trace 提供任务级 JSONL 事件追踪系统，专为故障排查设计。
//
// 设计原则：
//   - 物理 JSONL 按 writer 打开时间命名；Task 重试可产生多个分片，CLI 按完整 TaskID 聚合
//   - 写入失败降级为 stderr WARNING，不中断主流程
//   - 二进制全开（无级别过滤），项目早期阶段以排查故障为优先
//   - 零第三方依赖，仅使用 stdlib
//
// 用法：
//
//	// 在 bootstrap 中初始化；traceDir 通常是 active Session 的 logs/
//	w, _ := trace.NewWriter(traceDir, 100)
//	trace.SetDefault(w)
//
//	// 在任意位置 emit 事件（包级 helper，零依赖注入）
//	trace.Emit(trace.Event{
//	    Kind:   trace.KindTaskClaimed,
//	    TaskID: taskID,
//	    AgentID: a.ID,
//	})
//
// 事后排查：
//
//	tail -f <trace-dir>/2026-04-08T04-17-06_321b561d.jsonl | jq
//	./agentgo trace list
//	./agentgo trace show 321b561d
package trace

import "time"

// EventKind 是事件的类型标签。
type EventKind string

const (
	// 任务生命周期
	KindTaskPublished EventKind = "task_published"
	KindTaskClaimed   EventKind = "task_claimed"
	KindTaskSubmitted EventKind = "task_submitted"
	KindTaskCompleted EventKind = "task_completed"

	// KindTextOnlySubmission：代理"什么都没落盘，仅吐出一份文字汇报"的判别事件。
	//
	// 触发条件（必须全部满足）：
	//   1. 任务已成功提交（task_submitted 已 emit）
	//   2. OutputLen > 0
	//   3. 该任务整个生命周期内 0 个 file_written 事件（task.Artifacts 为空）
	//
	// 与 KindTaskSubmitted 的关系：是它的判别衍生事件——所有 text_only_submission
	// 的同时也是 task_submitted；但 task_submitted 也包括"写了文件 + 文字汇报"的
	// 任务。Reactor 想专门捕捉"纯文字交付"场景时订阅此事件可避免在 when: 表达式
	// 里再做条件过滤。
	//
	// 使用场景（reactor 可订阅）：
	//   - 派发"补漏写文件"任务（让另一个代理把文字内容固化到 .agentgo/reports/）
	//   - 触发"产物形式校验"——如果 task.kind 期望产文件却走了文字路径，告警
	//   - 给 verifier 派"文字内容审核"任务（取代当前依赖 file_written 的链路）
	//
	// 字段：标准生命周期字段（TaskID/AgentID/OutputLen/LoopsUsed）。文字内容
	// 本身不进事件——reactor 需要时通过 store 查 task.LastResponse。
	KindTextOnlySubmission EventKind = "text_only_submission"

	// 非成功终态。2026-04-25 P1 #2 引入——此前 trace 没有 retry/failed/blocked/cancelled
	// 事件类型，排障时看到 trace 突然中断但不知道原因；新 EventKind 补齐账本。
	KindTaskRetry     EventKind = "task_retry"     // RetryRollback 触发（ErrRecoverable 类可恢复故障）
	KindTaskFailed    EventKind = "task_failed"    // terminateTask 终止（重试耗尽或不可恢复错误）
	KindTaskBlocked   EventKind = "task_blocked"   // 系统确认任务不可继续后的终态（无路由 / runtime fuse）
	KindTaskCancelled EventKind = "task_cancelled" // 外部 cancel（cancel_task 工具、watchdog、用户 /cancel）

	// AgentTemplate Team lifecycle. Graph-bound Teams emit the GraphID so the
	// graph trace can prove that resource binding predates node claims and that
	// cleanup follows graph_ended rather than the provisioning task terminal.
	KindTeamGraphBound EventKind = "team_graph_bound"
	KindTeamStopped    EventKind = "team_stopped"

	// KindRuntimeLoopFuseTriggered：emergency loop fuse 触发（V6，见
	// docs/nextUpgrade-V6.md §5 升级思路 8）。固定轮数上限删除后，ReAct 循环
	// 不再因到达轮数终止；本事件只在循环计数越过程序缺陷防御兜底
	// （agent.emergencyLoopFuse）时发出，随后任务进入 blocked 终态并登记
	// replan，绝不自动重跑同一 Task。它是运行时异常信号，不是正常终止条件。
	// payload：Loop=触发时的循环计数，Reason=兜底说明。
	KindRuntimeLoopFuseTriggered EventKind = "runtime_loop_fuse_triggered"
	// KindWatchdogObservation 是新 Loop 的只读 liveness 观测；不会直接迁移
	// Task 或合成 Scheduler 文本任务。
	KindWatchdogObservation EventKind = "watchdog_observation"

	// === V6 §5 结构化收尾（finalizing fence + 结构化 blocked）===

	// KindTaskFinalizing：submit_task_result 的结构化提交被接受（已校验、已
	// 标记 finalized）。从这一刻起同一响应中排在其后的工具调用被 fence 跳过
	// （KindToolCallSkipped），任务在下一轮 loop 顶部进入收尾事务。
	// payload：Transition.NewStatus=自述终态（completed/blocked）。
	KindTaskFinalizing EventKind = "task_finalizing"
	// KindToolCallSkipped：finalizing fence 拦截——submit_task_result 被接受
	// 后，同一 LLM 响应中排在其后的工具调用不再 dispatch、不产生副作用，
	// 调用者收到「已跳过」提示文本。payload：Tool / CallID / Reason=fence 原因。
	KindToolCallSkipped EventKind = "tool_call_skipped"
	// KindTaskResultCommitted：结构化提交的收尾事务已把终态 durable 提交
	// （submit_task_result 的 completed / blocked 两路）。终态由哪次权威
	// 收尾事务提交由本事件直接给出，不从 tool_result 或自由文本推断。
	// payload：Transition（PrevStatus=processing，NewStatus=终态，
	// Cause=submit_task_result / agent_reported_blocked）。
	KindTaskResultCommitted EventKind = "task_result_committed"

	// LLM 调用
	KindLLMCallStart EventKind = "llm_call_start"
	KindLLMCallEnd   EventKind = "llm_call_end"

	// 工具调用（每次调用产生 tool_call + tool_result 两条事件）
	KindToolCall   EventKind = "tool_call"
	KindToolResult EventKind = "tool_result"

	// 上下文压缩
	KindHistoryCompaction EventKind = "history_compaction"

	// KindContextManifestBuilt：每轮 LLM 调用前已 durable 的 L2
	// ContextSnapshot/Manifest 投影；legacy 调用可能仍只有旧 Manifest 摘要。
	// 每轮 LLM 调用恰好一条，带 TaskID 落任务分片。payload：
	//   - Loop：ReAct 循环轮次（transfer-note 压缩调用为 -1，与 llm_call_start 同口径）
	//   - PromptTokens：Manifest 估算的总 prompt tokens（rune/3 口径）
	//   - HistoryEntries：本轮历史条数
	//   - Description：逐段 JSON 摘要（id/source/authority/freshness/tokens/
	//     disposition/count 列表，不含正文）
	// 实测 prompt tokens 不在本事件重复——由同 (task_id, loop) 的 llm_call_end
	// 事件对账（估算↔实测偏差只观测，不告警）。
	KindContextManifestBuilt EventKind = "context_manifest_built"

	// 文件操作（write_file/edit_file 成功后发出，可审计落盘动作）
	KindFileWritten EventKind = "file_written"

	// 文件写入排队（TryClaim 冲突后等待前任释放，§8.3 文件冲突排队）
	KindFileWriteQueued EventKind = "file_write_queued"

	// 进度通知事件（文件写入 / 子任务发布 / 任务过半）
	KindProgressNotify EventKind = "progress_notify"

	// 通用错误事件（比 task_completed 严重的故障，但任务并未终止）
	KindError EventKind = "error"

	// === 工作区隔离（workspace isolation）===

	// 任务 workspace 物化完成（认领隔离任务时）。payload：Path=workspace 根路径。
	KindWorkspaceMaterialized EventKind = "workspace_materialized"

	// 任务 workspace 合并回主根完成。payload：Description 含逐文件结果摘要
	//（fast-forward / auto-merged 计数）。
	KindWorkspaceMerged EventKind = "workspace_merged"

	// 合并出现无法自动解决的冲突。payload：Path=冲突文件，
	// Description=冲突详情（区域数、基线/主根/workspace 三方哈希）。
	KindWorkspaceMergeConflict EventKind = "workspace_merge_conflict"

	// 任务 workspace 已清理（合并成功后或 Watchdog 孤儿清扫）。
	// payload：Path=workspace 根路径。
	KindWorkspaceCleaned EventKind = "workspace_cleaned"

	// === v5 Phase 2 新增（TraceUpgrade.md §4） ===

	// Agent 实例状态机变更（ReactiveSystem.md §7.2 引入的 4 状态机）。
	// 每次 SetState(newState, cause) 同步 emit。payload：Transition 子结构
	// （PrevState / NewState / Cause）。
	KindAgentStateChanged EventKind = "agent_state_changed"

	// Shell 工具执行结果（ToolUpgradePlan.md §2.9）。命令执行完才 emit。
	// payload：ShellExec 子结构 + 顶层 Tool="run_shell" / Args。
	KindShellExecuted EventKind = "shell_executed"

	// Shell 超时 — TimeoutHandler 即将决策（ToolUpgradePlan.md §2.8.5）。
	// payload：ShellTimeout 子结构（Decision 字段为空）。
	KindShellTimeoutPending EventKind = "shell_timeout_pending"

	// Shell 超时 — TimeoutHandler 已决策（truncate / wait / continue）。
	// payload：ShellTimeout 子结构（Decision 字段非空）。
	KindShellTimeoutResolved EventKind = "shell_timeout_resolved"

	// Reactor spawn 深度超过系统硬上限。用于阻断 spawn_agent reactor 级联爆炸。
	// payload：Depth 记录被拒绝的目标深度，Reason 记录触发原因。
	KindReactorSpawnDepthExceeded EventKind = "reactor_spawn_depth_exceeded"

	// === V6 §3 Task Memory（CM2，internal/taskmem）生命周期事件 ===

	// KindTaskMemoryCreated：任务开始时创建 Task Memory（含损坏降级新建），
	// Goal/Constraints 已初始化。payload：Description=JSON 段计数（不含正文）。
	KindTaskMemoryCreated EventKind = "task_memory_created"
	// KindTaskMemoryUpdated：settled Turn 出现实质变化、滚动更新已落盘。
	// 只在实质变化时发出——重复读取、无新增证据的轮次不发。payload：
	// Loop=轮次，Description=JSON 段计数（version/actions/facts/files/...）。
	KindTaskMemoryUpdated EventKind = "task_memory_updated"
	// KindTaskMemoryCheckpointed：强制 checkpoint——历史压缩前、Attempt
	// 结束前、任务终态（终态同时置 Sealed，Description 载 sealed=true）。
	// payload：Loop（attempt_end/terminal 为 -1），Reason=checkpoint 原因
	//（history_compaction / attempt_end / terminal:<status>），
	// Description=JSON 段计数。
	KindTaskMemoryCheckpointed   EventKind = "task_memory_checkpointed"
	KindObservationDeltaRecorded EventKind = "observation_delta_recorded"
	// KindObservationCheckpointFailed 记录一次机械 Observation Control
	// Invocation 未形成 durable delta。Reason 区分 provider 前的 framework
	// control preflight 故障与 provider 返回后的结构化提交无效；周期性失败即使
	// 恢复业务阶段也必须可观测，不能只靠终态 TaskOutcome 推断。
	KindObservationCheckpointFailed EventKind = "observation_checkpoint_failed"

	// === V6 §3 Session Memory（CM3，internal/memory/promotion.go +
	// internal/bootstrap/session_promotion.go）生命周期事件 ===

	// KindSessionMemoryPromotionProposed：任务终态事件到达，晋升器已从
	// Sealed Task Memory 开始筛选晋升候选。payload：TaskID，
	// Reason=终态（completed/blocked/failed/cancelled），Description=JSON
	// 摘要（Task Memory 段计数，不含正文）。
	KindSessionMemoryPromotionProposed EventKind = "session_memory_promotion_proposed"
	// KindSessionMemoryPromotionDecided：一次晋升收口（含跳过）。payload：
	// TaskID，Reason=终态，Description=JSON 摘要：decided（promoted/
	// already_promoted/no_candidates/no_task_memory/session_store_unavailable）、
	// entries=写入条数、superseded=被取代条数、keys=条目 Key 列表（不含正文）。
	KindSessionMemoryPromotionDecided EventKind = "session_memory_promotion_decided"
	// KindMemoryRecalled：processTask 任务入口召回 Session Memory 并注入
	// 上下文。payload：TaskID / AgentID，Description=JSON 摘要
	//（entries=注入条数、keys=条目 Key+State 列表，不含正文）。
	// 召回为空时不发——空召回是稳态（新会话），不产生事件噪音。
	KindMemoryRecalled EventKind = "memory_recalled"
	// KindMemoryEntryStateChanged：Session Memory 条目生命周期迁移
	//（supersede：同 Key 新结论取代旧条目）。payload：TaskID（触发迁移的
	// 来源任务），Description=JSON 摘要（key/old_id/new_id/new_state，
	// 不含正文）。
	KindMemoryEntryStateChanged EventKind = "memory_entry_state_changed"

	// === V6 §6 Graph Runtime 事件（internal/graph 引擎发出） ===

	// KindGraphSubmitted：GraphDocument 校验通过且 submit 事实已 durable 落盘。
	// payload：GraphID，Description 载 revision/digest 摘要。
	KindGraphSubmitted EventKind = "graph_submitted"
	// KindGraphSubmissionRejected：图提交被拒绝（校验失败或落盘失败），
	// 不产生任何节点激活。payload：GraphID（若能解析）+ Error=拒绝原因。
	KindGraphSubmissionRejected EventKind = "graph_submission_rejected"
	// KindNodeActivationCreated：节点新 activation 的 durable 事实已写入
	// （activation_id 已分配、节点置 ready）。回边重进入同样产生一条。
	// payload：GraphID / NodeID / ActivationID。
	KindNodeActivationCreated EventKind = "node_activation_created"
	// KindGraphTransitionSelected：一条出边对某 source activation 生效
	// （幂等记录 durable 之后发出）。payload：GraphID / NodeID（源节点）/
	// ActivationID（源 activation），Description 载 next 下标与目标节点。
	KindGraphTransitionSelected EventKind = "graph_transition_selected"
	// KindGraphEnded：图到达终态。经 end 节点完成时 Reason 为空；
	// 失败终态（节点无出路 / 任务发布失败）时 Reason 载中文原因。
	KindGraphEnded EventKind = "graph_ended"
	// KindGraphJoinResolved：join 节点满足就绪条件完成归并（每个必需输入
	// 端口均有实际选中边绑定到同一目标 activation；旧无端口图保留兼容 barrier）。
	// payload：GraphID / NodeID / ActivationID，Description 载入边计数
	// （生效入边 X/Y）。
	KindGraphJoinResolved EventKind = "graph_join_resolved"
	// KindGraphWaitStarted：wait_event / approval 节点进入 durable 等待。
	// payload：GraphID / NodeID / ActivationID，Description 载等待的事件名
	// 或 approval 的 requestID。
	KindGraphWaitStarted EventKind = "graph_wait_started"
	// KindGraphWaitResumed：wait_event 节点等到匹配的外部事件并恢复
	// （Result 已 durable、节点 completed）。payload：GraphID / NodeID /
	// ActivationID，Description 载命中的事件名。
	KindGraphWaitResumed EventKind = "graph_wait_resumed"
	// KindGraphApprovalDecided：approval 节点收到审批裁决并完成。
	// payload：GraphID / NodeID / ActivationID，Description 载
	// approved / rejected。
	KindGraphApprovalDecided EventKind = "graph_approval_decided"
	// KindGraphChangeRequested：图内节点任务经 request_replan 请求 graph
	// change（C5d，V6 Graph 变更流的入口审计事实）。该请求不经 Plan 控制面，
	// 系统会以 __scheduler__ 唤醒任务交给 Scheduler 用 patch_graph 裁决。
	// payload：GraphID / NodeID / ActivationID（三者定位请求的 activation），
	// TaskID=请求者任务，Reason=reason_code，Description=detail 摘要。
	KindGraphChangeRequested EventKind = "graph_change_requested"
	// KindGraphRevisionCommitted：patch_graph 以 base_revision CAS 应用
	// DefinitionPatch 成功，图定义面进入新 revision（C5d）。payload：
	// GraphID，Description 载 new_revision 与 patch 摘要（upsert/remove/root
	// 的节点清单，不复制整图 JSON）。
	KindGraphRevisionCommitted EventKind = "graph_revision_committed"
	// KindAcceptanceCompleted：acceptance 节点 completed 终态的服务端核验
	// 已完成（G1b，V6 §7.3 Graph 行命名）。valid 按自报 verdict 正常转移；
	// disputed/unverifiable 不采信 verdict（节点 failed + graph change
	// 唤醒）。payload：GraphID / NodeID / ActivationID / TaskID=验收任务，
	// Acceptance 子结构（自报 verdict / 核验 status / checked 数 / reason
	// 摘要），不含证据正文。
	KindAcceptanceCompleted EventKind = "acceptance_completed"

	// === V6 §2 Prompt 有序编译（P1a，internal/prompt + internal/agent/prompt_build.go）===

	// KindPromptCompiled：任务 attempt 开始时 Prompt 编译冻结（每个 attempt
	// 恰好一条，含重试新 attempt；输入不变则 Build.ID 稳定，重试天然复用
	// 同一 Build.ID）。payload：TaskID / AgentID，PromptBuildID=Build.ID，
	// Description=逐组件身份 JSON 摘要（id/version/digest/in_message，
	// 不含正文）。
	KindPromptCompiled EventKind = "prompt_compiled"

	// === V6 §2 /doctor agents 审计（P1b，internal/bootstrap/agent_audit.go）===

	// KindAgentAuditStarted：/doctor agents 触发、只读审计任务已发布给
	// Scheduler。payload：TaskID=审计任务，Description=JSON 摘要
	//（agents=被检 agent 数、snapshot_digest=能力快照 digest、
	// deterministic_warnings=确定性 warning 计数，不含 prompt 正文）。
	KindAgentAuditStarted EventKind = "agent_audit_started"
	// KindAgentAuditWarning：审计快照构建时发现的确定性 warning（当前仅
	// route_missing 类——kind 声明的 event_type 无 ready runner 认领）。
	// Scheduler 语义对照得出的「身份/权限」warning 走审计任务结果正文，
	// 不进 trace（自由文本非结构化账本）。payload：TaskID=审计任务，
	// Description=JSON 摘要（agent/type/evidence）。
	KindAgentAuditWarning EventKind = "agent_audit_warning"
	// KindAgentAuditCompleted：审计任务到达终态。payload：TaskID=审计任务，
	// Reason=终态，Description=JSON 摘要（agents/warnings 计数与类型计数/
	// snapshot_digest，不含 prompt 正文）。
	KindAgentAuditCompleted EventKind = "agent_audit_completed"

	// === V6 §4 ExecutionLease（H1，冻结执行租约）生命周期事件 ===

	// KindExecutionLeaseFrozen：任务首次被认领时执行租约已计算并冻结
	// （NodeRequirement ∩ RouteCeiling ∩ Policy）。payload：Lease 子结构
	//（digest/业务与控制工具数/模型/隔离/Synthetic）。
	KindExecutionLeaseFrozen EventKind = "execution_lease_frozen"
	// KindExecutionLeaseRejected：租约计算 fail-closed——显式声明的工具子集
	// 越出认领方注册全集（或 executor 无换入面），任务不降级执行。
	// payload：Lease 子结构（Cause 载原因、Missing 载缺失工具清单）。
	KindExecutionLeaseRejected EventKind = "execution_lease_rejected"
	// KindExecutionLeaseReused：重试回滚后重认领（含进程重启恢复）复用任务
	// 上的既有租约，不重新计算——Digest 与工具面不变。payload：Lease 子结构。
	KindExecutionLeaseReused EventKind = "execution_lease_reused"
	// KindExecutionLeaseRevoked：任务终态（含 finalizing 被接受）撤销租约，
	// 此后任何工具 dispatch 拒绝（与 finalizing fence 互补的防御层）。
	// payload：Lease 子结构（Cause 载撤销原因：terminal:<status> /
	// finalizing_accepted）。
	KindExecutionLeaseRevoked EventKind = "execution_lease_revoked"

	// === V6 §4 Suggestions（H2a，结构化拒绝建议）事件 ===

	// KindSuggestionsReturned：Gate Abort 且决策携带结构化建议时发出——每次
	// 拒绝每条建议一条。payload：Suggestion 子结构（reason_code / retryable /
	// 过滤后给出的建议数 / 被过滤掉的建议数与原因 / 同因同目标重复计数），
	// 不含建议正文（Label 属 prompt 内容，按 §4 思路 15 脱敏默认不落 trace）。
	KindSuggestionsReturned EventKind = "suggestions_returned"
	// KindSuggestionDisposition：上一轮给出的建议被下一轮工具调用结构化
	// 判定后的去向——adopted（工具名+参数逐项匹配且通过 Gate）/ abandoned
	//（调用了别的）/ repeated（再次触发同因同目标的同一拒绝）。只按
	// Suggestion.ID 与 SuggestedAction 结构匹配，不做自然语言猜测。
	// payload：Suggestion 子结构（suggestion_id / reason_code / disposition /
	// repeat_count）。
	KindSuggestionDisposition EventKind = "suggestion_disposition"

	// === V6 §4 Effect Journal（H2b，internal/effect）生命周期事件 ===

	// KindEffectPrepared：副作用执行前意图已落账（先落账再执行）。
	// payload：Effect 子结构（effect_id / kind / policy / target 摘要 /
	// args_digest）——不含完整参数/命令（脱敏纪律同账本）。
	KindEffectPrepared EventKind = "effect_prepared"
	// KindEffectSettled：副作用执行结果已记录（含恢复核验一致路径）。
	// payload：Effect 子结构（含 result_summary：exit code / bytes+hash /
	// 合并结果）。
	KindEffectSettled EventKind = "effect_settled"
	// KindEffectUnknown：副作用结果不可知（执行返回错误后外部状态不确定，
	// 或启动恢复时发现 prepared 后未见 settled）。payload：Effect 子结构
	//（reason 载「为什么不可知」）。
	KindEffectUnknown EventKind = "effect_unknown"
	// KindEffectRecoveryDecided：启动恢复对一条 unknown Effect 的裁决结论
	//（含依据）。payload：Effect 子结构（decision=verified_settled /
	// kept_unknown_mismatch / kept_unknown_unverifiable /
	// kept_unknown_manual / replayable_hold，reason=裁决依据）。
	KindEffectRecoveryDecided EventKind = "effect_recovery_decided"
)

// Transition 承载所有"状态转移"语义，跨 task 状态机与 agent 状态机两个域。
//
// 字段填充约定：
//   - task lifecycle 事件（claimed / completed / failed / blocked / cancelled / retry）填 PrevStatus / NewStatus
//   - agent_state_changed 事件填 PrevState / NewState
//   - 不同时填两套——但定义在同一 struct 是当前简化处置（未来若发现混淆可拆）
type Transition struct {
	// Task 状态机（task_claimed / completed / failed / blocked / cancelled / retry）
	PrevStatus string `json:"prev_status,omitempty"` // pending / processing / completed / failed / blocked / cancelled
	NewStatus  string `json:"new_status,omitempty"`

	// Agent 状态机（agent_state_changed）
	PrevState string `json:"prev_state,omitempty"` // idle / processing / waiting_interaction / terminating
	NewState  string `json:"new_state,omitempty"`

	// 通用字段：结构化原因 enum，让 Reactor when 条件能精确匹配。
	// 示例值：
	//   - "task_claimed:<task_id>"            （idle → processing）
	//   - "interaction_wait_start"            （processing → waiting_interaction）
	//   - "interaction_wait_end"              （waiting_interaction → processing）
	//   - "react_loop_exit:natural" / ":panic" / ":runtime_loop_fuse"  （processing → terminating）
	//   - "task_end_hook_done"                （terminating → idle）
	//   - "runtime_loop_fuse" / "recoverable_error_retries_exhausted" /
	//     "non_recoverable_error" （processing → failed / blocked）
	Cause string `json:"cause,omitempty"`

	// task_cancelled 专用：取消来源（user / watchdog / scheduler / dependency_failure）。
	// ReactiveSystem.md §6.4.6 强调此字段必须结构化，否则 Reactor 写不了精准条件。
	CancelSource string `json:"cancel_source,omitempty"`

	// task_failed / task_retry 专用
	RetryCount int `json:"retry_count,omitempty"`
}

// ShellExec 是 KindShellExecuted 事件的 sub-payload（ToolUpgradePlan.md §2.9）。
// 命令执行完才 emit；Command / ExitCode / DurationMS / Outcome 总是有值，excerpt 可选。
type ShellExec struct {
	Command       string `json:"command"`
	ExitCode      int    `json:"exit_code"`
	ExitCodeScope string `json:"exit_code_scope,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	Outcome       string `json:"outcome"`                  // success / failure / timeout
	StdoutExcerpt string `json:"stdout_excerpt,omitempty"` // 截断（前后各 N 字节），完整内容仍在 trace 文件
	StderrExcerpt string `json:"stderr_excerpt,omitempty"`
}

// ShellTimeout 是 KindShellTimeoutPending / Resolved 共用的 sub-payload。
// 靠 Decision 字段是否为空区分语义阶段：
//   - Decision == ""：Pending 阶段，TimeoutHandler 即将决策
//   - Decision != ""：Resolved 阶段，handler 已返回决策
type ShellTimeout struct {
	Command       string `json:"command"`
	ElapsedSec    int    `json:"elapsed_sec"`
	PreviousWaits int    `json:"previous_waits,omitempty"` // TimeoutHandler 已经 Wait 续命过几次

	// 仅 KindShellTimeoutResolved 填充
	Decision     string `json:"decision,omitempty"`      // truncate / wait / continue
	ExtraSeconds int    `json:"extra_seconds,omitempty"` // 仅 Decision=wait

	// 仅 KindShellTimeoutPending 填充（决策时可见的 partial 输出）
	StdoutExcerpt string `json:"stdout_excerpt,omitempty"`
	StderrExcerpt string `json:"stderr_excerpt,omitempty"`
}

// LeasePayload 是 execution_lease_* 事件（V6 §4 H1）的 sub-payload。
// 工具清单本身不进事件（噪音大）——只记计数与 digest，需要明细时经
// store 查 task.Lease。
type LeasePayload struct {
	Digest        string   `json:"digest,omitempty"`    // 租约稳定摘要（sha256 前 12）
	BusinessTools int      `json:"business_tools"`      // 业务工具数（交集结果）
	ControlTools  int      `json:"control_tools"`       // 控制通道工具数
	Model         string   `json:"model,omitempty"`     // 冻结模型
	Workspace     string   `json:"workspace,omitempty"` // "" | "workspace"
	Synthetic     bool     `json:"synthetic,omitempty"` // true = 需求为合成授予
	Cause         string   `json:"cause,omitempty"`     // rejected/revoked 的原因
	Missing       []string `json:"missing,omitempty"`   // rejected：越出注册全集的工具清单
	Attempt       int      `json:"attempt,omitempty"`   // 冻结时的执行尝试序号（1-based）
}

// SuggestionPayload 是 suggestions_returned / suggestion_disposition 事件
// （V6 §4 H2a）的 sub-payload。只载结构化计数与标识，不载建议正文
// （Label 属 prompt 侧内容，按 §4 思路 15 脱敏纪律不进 trace）。
type SuggestionPayload struct {
	SuggestionID string `json:"suggestion_id,omitempty"` // 稳定 ID（gate+原因码+目标 digest 前 8）
	ReasonCode   string `json:"reason_code,omitempty"`   // 稳定拒绝原因码
	Retryable    bool   `json:"retryable,omitempty"`     // 该建议是否可自动重试
	Offered      int    `json:"offered"`                 // 过滤后实际给出的候选动作数
	Filtered     int    `json:"filtered,omitempty"`      // 被 Harness 过滤掉的候选动作数
	FilterReason string `json:"filter_reason,omitempty"` // 过滤原因摘要（如 not_retryable / finalizing / over_limit）
	Disposition  string `json:"disposition,omitempty"`   // adopted / abandoned / repeated
	RepeatCount  int    `json:"repeat_count,omitempty"`  // 同一建议 ID 在本任务内第 N 次触发
}

// EffectPayload 是 effect_prepared / effect_settled / effect_unknown /
// effect_recovery_decided 事件（V6 §4 H2b）的 sub-payload。只载标识与摘要
// （effect_id / kind / policy / target / args_digest / result_summary），
// 不含完整参数/命令——与 Effect Journal 账本同一脱敏纪律。
type EffectPayload struct {
	EffectID      string `json:"effect_id,omitempty"`      // <taskID>-<seq>
	Kind          string `json:"kind,omitempty"`           // file_write / file_edit / shell / message / workspace_merge
	Policy        string `json:"policy,omitempty"`         // safe_replay / verify_first / manual_only / never_replay
	Status        string `json:"status,omitempty"`         // prepared / dispatched / settled / unknown
	Target        string `json:"target,omitempty"`         // 目标摘要：路径 / "cmd:<digest>" / 收件人
	ArgsDigest    string `json:"args_digest,omitempty"`    // 参数 sha256 前 12
	ResultSummary string `json:"result_summary,omitempty"` // exit code / bytes+hash / 合并结果
	Decision      string `json:"decision,omitempty"`       // recovery 裁决结论
	Reason        string `json:"reason,omitempty"`         // unknown 原因 / 裁决依据
}

// AcceptancePayload 是 acceptance_completed 事件（G1b 服务端核验）的
// sub-payload。只载结论性字段——自报 verdict、核验 status
// （valid/disputed/unverifiable）、实际核验条数与原因摘要；不含证据正文
// （命令串、hash 等属核验输入，与 Effect 同一脱敏纪律）。
type AcceptancePayload struct {
	Verdict string `json:"verdict,omitempty"` // 验收 agent 自报结论（pass/fail/fixable/...）
	Status  string `json:"status"`            // 核验结论：valid / disputed / unverifiable
	Checked int    `json:"checked"`           // 实际完成核验的证据条数
	Reason  string `json:"reason,omitempty"`  // 原因摘要（disputed 时含哪条证据为何失败）
}

// Event 是一条 trace 事件。所有字段除 Timestamp/Kind/TaskID 之外都是可选的，
// 由具体的事件类型按需填充。omitempty 让 JSON 输出保持简洁。
type Event struct {
	Timestamp time.Time `json:"ts"`
	Kind      EventKind `json:"kind"`
	TaskID    string    `json:"task_id"`

	// --- 统一事件身份（V6 §7.2） ---
	// SessionID 标识事件所属的 Session。由 Writer 集中盖戳：Emit 时若为空则
	// 补上 writer 绑定的 session id（bootstrap/session 切换时 SetSessionID 注入），
	// 发射方显式填写的值不被覆盖。无活跃 Session（trace 落 .agentgo/traces/）
	// 时保持空，omitempty 不输出。
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	ActionID  string `json:"action_id,omitempty"`
	// InvocationID 关联同一次 LLM 调用产生的一组事件（llm_call_start /
	// llm_call_end / context_manifest_built），由 LLMExecutor.Execute 每次调用
	// 按 <turnID>/invocation-<seq> 生成；Attempt/Turn lineage 与 executor 单调
	// 序号共同防止重试/恢复后撞 durable ContextSnapshot identity。
	InvocationID string `json:"invocation_id,omitempty"`

	// --- 通用字段 ---
	AgentID    string `json:"agent_id,omitempty"`
	Loop       int    `json:"loop,omitempty"`
	Error      string `json:"error,omitempty"`
	NotifyType string `json:"notify_type,omitempty"` // 进度通知类型：file_write / subtask / halfway
	Reason     string `json:"reason,omitempty"`      // 非成功终态事件（task_retry/failed/blocked/cancelled）的解释
	AttemptNo  int    `json:"attempt_no,omitempty"`  // task_retry 的第 N 次重试（1-based）；其它事件不填

	// --- 任务生命周期字段（task_published / task_claimed / task_submitted / task_completed） ---
	Description  string   `json:"description,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	EventType    string   `json:"event_type,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Depth        int      `json:"depth,omitempty"`
	PublishedBy  string   `json:"published_by,omitempty"`
	ParentTaskID string   `json:"parent_task_id,omitempty"`
	BatchID      string   `json:"batch_id,omitempty"`
	OutputLen    int      `json:"output_len,omitempty"`
	LoopsUsed    int      `json:"loops_used,omitempty"`

	// 节点能力覆盖（per-node NodeCapability，仅 task_published 填充）：
	// Scheduler 发布任务时为该 DAG 节点声明的工具子集 / 模型覆盖。
	// 未声明时缺省（omitempty），保持旧 jsonl 兼容。
	ToolsOverride []string `json:"tools_override,omitempty"`
	ModelOverride string   `json:"model_override,omitempty"`
	// IsolationOverride 非空 = 该节点声明了执行隔离模式（如 "workspace"）。
	IsolationOverride string `json:"isolation_override,omitempty"`

	// --- LLM 调用字段 ---
	PromptTokens     int    `json:"prompt_tokens,omitempty"`
	CompletionTokens int    `json:"completion_tokens,omitempty"`
	HistoryEntries   int    `json:"history_entries,omitempty"`
	ToolCallsCount   int    `json:"tool_calls_count,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
	DurationMS       int64  `json:"duration_ms,omitempty"`
	// ToolChoice/Reasoning 是脱敏的 provider request 控制面，用于证明
	// thinking 兼容降级是否真正进入 wire；不包含 prompt/参数/密钥。
	ToolChoiceMode  string `json:"tool_choice_mode,omitempty"`
	ToolChoiceName  string `json:"tool_choice_name,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Invocation failure 的稳定分类字段。Error 仅供展示，控制流不得解析它。
	FailureKind    string `json:"failure_kind,omitempty"`
	FailurePhase   string `json:"failure_phase,omitempty"`
	FailureOrigin  string `json:"failure_origin,omitempty"`
	TimeoutScope   string `json:"timeout_scope,omitempty"`
	ProviderCode   string `json:"provider_code,omitempty"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	UsageState     string `json:"usage_state,omitempty"`
	Partial        bool   `json:"partial,omitempty"`
	RecoveryAction string `json:"recovery_action,omitempty"`
	// PromptBuildID 是 V6 §2 P1a 的 prompt_build_id：prompt_compiled 事件
	// 载 Build.ID；context_manifest_built 事件并入同 attempt 冻结的
	// Build.ID（prompt_bound 不独立成事件，避免同频双账本）。
	PromptBuildID string `json:"prompt_build_id,omitempty"`
	// ToolRouterSnapshotID 证明本次 advertise 与 dispatch 使用同一冻结工具视图。
	ToolRouterSnapshotID string `json:"tool_router_snapshot_id,omitempty"`
	// ContextSnapshotID/ContextPolicyRef 绑定本次真实发送内容的 L2 durable authority。
	ContextSnapshotID string `json:"context_snapshot_id,omitempty"`
	ContextPolicyRef  string `json:"context_policy_ref,omitempty"`

	// --- 工具调用字段 ---
	Tool      string         `json:"tool,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	ResultLen int            `json:"result_len,omitempty"`

	// --- 文件操作字段 ---
	Path  string `json:"path,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	Hash  string `json:"hash,omitempty"`

	// --- 文件冲突排队字段（§8.3 file_write_queued） ---
	QueueLen int   `json:"queue_len,omitempty"` // 入队时的等待队列深度
	WaitMS   int64 `json:"wait_ms,omitempty"`   // 排队等待实际耗时（毫秒）

	// --- 历史压缩字段 ---
	PromptTokensBefore int    `json:"prompt_tokens_before,omitempty"`
	PromptTokensAfter  int    `json:"prompt_tokens_after,omitempty"`
	Strategy           string `json:"strategy,omitempty"`
	KeptEntries        int    `json:"kept_entries,omitempty"`

	// --- 结构化子载荷 ---
	// 四者均为指针 + omitempty：nil 时 JSON 完全不输出，保持旧 jsonl 兼容。
	Transition   *Transition        `json:"transition,omitempty"`    // 状态转移信息（task / agent 状态机）
	ShellExec    *ShellExec         `json:"shell_exec,omitempty"`    // Shell 执行结果
	ShellTimeout *ShellTimeout      `json:"shell_timeout,omitempty"` // Shell 超时信息（pending / resolved 共用）
	Lease        *LeasePayload      `json:"lease,omitempty"`         // 执行租约信息（execution_lease_* 事件）
	Suggestion   *SuggestionPayload `json:"suggestion,omitempty"`    // 结构化拒绝建议（suggestions_returned / suggestion_disposition）
	Effect       *EffectPayload     `json:"effect,omitempty"`        // 副作用账目（effect_prepared / settled / unknown / recovery_decided）
	Acceptance   *AcceptancePayload `json:"acceptance,omitempty"`    // 验收服务端核验结论（acceptance_completed）

	// --- Graph Runtime 字段（V6 §6 graph_* / node_activation_* 事件） ---
	// TaskID 为空而 GraphID 非空的事件由 Writer 归入 graph_<graph_id前8位>.jsonl
	// 分片（与 task 分片同目录）；两者皆空的事件丢弃。
	GraphID      string `json:"graph_id,omitempty"`
	GraphOutcome string `json:"graph_outcome,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
	ActivationID string `json:"activation_id,omitempty"`
}
