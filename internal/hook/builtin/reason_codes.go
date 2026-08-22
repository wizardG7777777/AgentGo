package builtin

// reason_codes.go 集中声明内置 Gate 的稳定拒绝原因码（V6 §4 H2a）。
//
// 原因码是跨版本稳定的机器可读标识（snake_case），供 Harness 过滤、
// 重复熔断计数、trace 结构化匹配与离线评测使用；修改既有原因码等于
// 破坏契约，新增拒绝场景一律追加新常量。
const (
	// === tool:preCall 域 ===

	// ReasonReadBeforeWrite：先读后写约束——写入前未成功 read_file 同路径。
	ReasonReadBeforeWrite = "read_before_write"
	// ReasonWriteConflict：乐观并发冲突——expected_hash 与当前内容不符。
	ReasonWriteConflict = "write_conflict"
	// ReasonWritePrecheckFailed：写前校验读取目标文件失败（权限等异常）。
	ReasonWritePrecheckFailed = "write_precheck_failed"
	// ReasonMissingExpectedArtifacts：写入路径不在任务声明的
	// expected_artifacts 内（应改写声明中的缺失产物路径）。
	ReasonMissingExpectedArtifacts = "missing_expected_artifacts"
	// ReasonInvalidToolArguments：工具参数缺失、类型错误或空值；调用者可按
	// schema/拒绝说明自行修正，不等同于真实路径越界。
	ReasonInvalidToolArguments = "invalid_tool_arguments"
	// ReasonPathOutOfBoundary：路径越出项目根边界 / 命中敏感文件 / 路径
	// 边界违规。
	ReasonPathOutOfBoundary = "path_out_of_boundary"
	// ReasonExecModePrefix：exec 模式拦截的前缀，实际原因码为
	// "exec_mode_" + 模式名（readonly / strict）。
	ReasonExecModePrefix = "exec_mode_"
	// ReasonExecModeReadonly：readonly 只读模式禁用写类工具。
	ReasonExecModeReadonly = "exec_mode_readonly"
	// ReasonExecModeStrict：strict 模式拦截（预留；当前 strict 不经本 Gate
	// 拒绝，走逐次审批 Interaction）。
	ReasonExecModeStrict = "exec_mode_strict"
	// ReasonDependencyNotReady：publish_task 的依赖未就绪（占位符 /
	// 未发布 / 已清理 / 参数形态非法）。
	ReasonDependencyNotReady = "dependency_not_ready"
	// ReasonLineAnchorStale：line_anchors 行哈希失配或无法解析。
	ReasonLineAnchorStale = "line_anchor_stale"

	// === mailbox 域（只填原因码，不产出建议） ===

	// ReasonMailChainDepthExceeded：邮件链跳数超过上限（级联循环防御）。
	ReasonMailChainDepthExceeded = "mail_chain_depth_exceeded"
	// ReasonWakeTaskDuplicate：同源同类型 pending 唤醒任务已存在。
	ReasonWakeTaskDuplicate = "wake_task_duplicate"
	// ReasonWakeNotWorthy：邮箱内容不值得独立唤醒（下沉自然 drain）。
	ReasonWakeNotWorthy = "wake_not_worthy"
)
