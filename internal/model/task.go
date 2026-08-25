package model

import (
	"time"

	"agentgo/internal/fulfillment"
	"agentgo/internal/loopcontract"
	"agentgo/internal/runcontract"
)

type TaskStatus string

// TaskContextInputKind 是 L5/L3 交给 L2 的有类型上下文端口。它不使用
// ContextFragment 类型，避免 model 反向依赖 ContextCompiler；L2 adapter 在
// Invocation 前把它机械映射为对应 FragmentKind/Section。
type TaskContextInputKind string

const (
	TaskContextUpstreamResult   TaskContextInputKind = "upstream_result"
	TaskContextUpstreamEvidence TaskContextInputKind = "upstream_evidence"
)

func (k TaskContextInputKind) Valid() bool {
	return k == TaskContextUpstreamResult || k == TaskContextUpstreamEvidence
}

type TaskContextInput struct {
	Kind      TaskContextInputKind `json:"kind"`
	SourceRef string               `json:"source_ref"`
	Content   string               `json:"content"`
}

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusProcessing TaskStatus = "processing"
	TaskStatusCompleted  TaskStatus = "completed"
	TaskStatusCancelled  TaskStatus = "cancelled"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusBlocked    TaskStatus = "blocked"
)

// ValidTransitions defines the allowed state machine transitions.
var ValidTransitions = map[TaskStatus][]TaskStatus{
	TaskStatusPending:    {TaskStatusProcessing, TaskStatusCancelled, TaskStatusFailed, TaskStatusBlocked},
	TaskStatusProcessing: {TaskStatusCompleted, TaskStatusFailed, TaskStatusCancelled, TaskStatusBlocked, TaskStatusPending},
}

// IsValidTransition checks whether transitioning from one status to another is allowed.
func IsValidTransition(from, to TaskStatus) bool {
	allowed, ok := ValidTransitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// IsTerminal returns true if the status is a terminal state.
func IsTerminal(status TaskStatus) bool {
	return status == TaskStatusCompleted || status == TaskStatusCancelled || status == TaskStatusFailed || status == TaskStatusBlocked
}

type Task struct {
	ID string
	// RunID/RunContract 是一次用户请求的冻结运行身份和预算契约。Graph/子任务
	// 必须沿父链传递；旧任务为空时按 legacy/degraded 处理，不能伪造历史身份。
	RunID       runcontract.RunID
	RunContract *runcontract.RunContract
	RunPhase    runcontract.Phase
	// RunBudgetPermitRef 是 L5 Recovery 在创建下一 execution Activation 前
	// 预留的首个 model-call grant；仅目标 Task 可在首轮认领并消费。
	RunBudgetPermitRef string
	// ProgressContract 是框架编译并冻结的 L4 进展契约。Scheduler 只能从
	// framework catalog 选择；nil 表示 legacy/degraded Task，不启用 L4 enforcement。
	ProgressContract *loopcontract.CompiledProgressContract
	// ContextPolicyRef 是 L2 ContextCompiler 的冻结 policy identity。新任务必须
	// 来自 framework catalog；旧任务空值按 legacy/degraded 处理，不能在重试中
	// 静默切换 policy。
	ContextPolicyRef    string
	FulfillmentContract *fulfillment.Contract
	// AttemptID/AttemptNo 由 Store 在 pending→processing 的认领边界生成。每次
	// RetryRollback 后重新认领产生新 Attempt；同一 processing 任务的并发认领
	// 不得改写。
	AttemptID   string
	AttemptNo   int
	Description string
	// ContextInputs 是与 Task objective 分离的冻结数据输入。Graph Result/Evidence
	// 必须走这里进入 L2 upstream section，禁止再拼进 Description/user_task。
	ContextInputs  []TaskContextInput `json:"context_inputs,omitempty"`
	Priority       int
	Dependencies   []string
	Status         TaskStatus
	Agents         []string
	MaxConcurrency int
	Results        map[string]string
	Error          string
	RetryCount     int
	RetryReasons   []string
	LastHistory    []byte // JSON 序列化的历史记录，重试时恢复上下文
	// ExpectedDuration 只用于 SLO、UI 和 legacy Watchdog 兼容告警；它不是
	// deadline，也不得触发取消、重试或 Task 状态迁移。新 Loop 的停止权威是
	// RunContract/ProgressCheckpoint 中冻结的绝对 HardDeadlineAt。
	ExpectedDuration time.Duration
	// TimeoutSeconds 是旧快照/旧发布方的导入别名。Store 会把它迁移到
	// ExpectedDuration；新控制流不得直接读取它推导 deadline。
	// Deprecated: 仅用于 legacy import compatibility。
	TimeoutSeconds int

	// EventSource identifies the external trigger or legacy publisher label.
	// It is not a parent edge and must never be assumed to be a mailbox ID.
	EventSource string
	// Intervention* 是 L4→L5 coordination wake 的冻结控制作用域。wake 自身
	// 不是 Graph activation，故 GraphID/NodeID/ActivationID 必须保持空；这些
	// 独立字段让 Scheduler 机械区分“非图请求重建 Draft”与“现有 Graph 变更裁决”，
	// 禁止解析 Description marker 猜 lineage。
	InterventionGraphID      string `json:"intervention_graph_id,omitempty"`
	InterventionNodeID       string `json:"intervention_node_id,omitempty"`
	InterventionActivationID string `json:"intervention_activation_id,omitempty"`
	// FinalReportGraphID 是 graph-ended Scheduler 唤醒的冻结只读作用域。
	// wake 自身不是 Graph activation，不能占用 GraphID；read_graph 与
	// get_task_result 只允许读取此 Graph，禁止从 Description marker 猜目标。
	FinalReportGraphID string `json:"final_report_graph_id,omitempty"`
	// ParentTaskID is the explicit task-lineage edge used for plan inheritance
	// and topology inspection. Empty means this Task has no known parent.
	ParentTaskID string
	// ReplyToAgentID is the explicit mailbox route for lifecycle reports. It is
	// intentionally separate from ParentTaskID because task IDs are not agents.
	ReplyToAgentID string
	// BatchID groups sibling Tasks created by one scheduling decision. It is
	// observability metadata, not an execution dependency.
	BatchID string

	EventType     string
	TriggerRule   string
	SystemPrompt  string // 可选的自定义 system prompt，非空时覆盖 Worker 默认 prompt
	PartialOutput string // 执行中的部分输出，用于流式进度展示
	Depth         int    // 子任务嵌套深度，根任务为 0

	// GraphID / NodeID / ActivationID / GraphNodeKind 是 V6 Graph 归属身份：本任务由 Graph
	// Runtime 为图 graph_id 的节点 node_id 的某次 activation（<nodeID>@<n>）
	// 发布时写入，终态后由 graph-terminal-feed Reactor 凭它回填引擎
	// （OnTaskTerminal 的幂等身份）。GraphNodeKind 还供 ExecutionLease 按
	// 真实节点角色派生控制通道，不能由可覆盖的 EventType/route 猜测。普通任务
	// 四字段皆空；旧 Graph 快照缺少 GraphNodeKind 时执行层按最小权限处理。
	GraphID       string `json:"graph_id,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
	ActivationID  string `json:"activation_id,omitempty"`
	GraphNodeKind string `json:"graph_node_kind,omitempty"`
	// GraphControllerRole 是 controller activation 的冻结 L5 控制职责；空值为
	// 普通 controller。RecoverySourceTaskID 只在 loop_recovery controller 上
	// 非空，把恢复决策精确绑定到触发 intervention 的 Graph Task。
	GraphControllerRole  string `json:"graph_controller_role,omitempty"`
	RecoverySourceTaskID string `json:"recovery_source_task_id,omitempty"`
	// OutcomeRef 指向 L4 append-only TaskOutcome。新 authoring Graph 的终态
	// 必须先 durable 提交该事实，再把引用与 Task 状态一起生效；legacy 为空。
	OutcomeRef string `json:"outcome_ref,omitempty"`
	// GraphDefinitionDigestVersion 非空表示任务来自 authoring Definition；
	// ExecutionLease 据此移除 legacy controller 的 direct patch_graph 能力。
	GraphDefinitionDigestVersion string `json:"graph_definition_digest_version,omitempty"`
	// RouteScope freezes the runtime route-authorization owner used at both
	// publish validation and claim time (for example "graph:<graph_id>" or
	// "task:<controller_task_id>"). It is deliberately separate from lineage:
	// ParentTaskID/GraphID remain provenance, while this field is the durable
	// authority that prevents a listener registered for another Graph/request
	// from claiming the Task. Older snapshots derive the same scope from
	// GraphID/ParentTaskID when this field is empty.
	RouteScope string `json:"route_scope,omitempty"`

	// Capability 是本任务（DAG 节点）的能力声明。由 Scheduler 在 publish_task
	// 时按节点指定：Tools 非空表示当次任务只允许使用该工具子集（必须 ⊆ 认领
	// runner 的工具白名单，Store 在 QueryAvailable 过滤、agent 在 processTask
	// 入口 fail-closed 校验并换入过滤视图）；Model 非空表示当次任务的模型覆盖。
	// nil 表示无节点级能力约束，agent 按 kind 声明的完整能力执行（kind 退化为
	// 能力天花板）。发布后视为只读契约，执行期不修改。
	Capability *NodeCapability `json:"capability,omitempty"`

	// Lease 是 V6 §4（H1）的冻结执行租约：任务首次被认领时按
	// NodeRequirement ∩ RouteCeiling ∩ Policy 冻结的当次执行契约
	// （见 model/execution_lease.go）。nil 表示尚未冻结（旧快照恢复或任务
	// 从未被认领）——认领时按计算规则即时冻结；重试重认领/进程重启恢复
	// 复用既有租约而不重新计算；任务终态（含 finalizing 被接受）置
	// Revoked。发布后视为只读契约，仅 Revoked 位在撤销时翻转。
	Lease *ExecutionLease `json:"lease,omitempty"`

	// MailChainDepth 是该任务被第几层邮件唤醒。
	// 用户 /steer 触发的初始任务为 0；被 chain_depth=N 的邮件唤醒的任务为 N。
	// MetaGroup.sendMessage 在构造 outgoing message 时读取此值并 +1 写入 msg.ChainDepth；
	// MailNotifier 在发布 wake task 时根据收件箱内未读邮件的最大 ChainDepth 设置该字段。
	// Phase 2 引入；零值兼容现有任务。
	MailChainDepth int
	// MailboxTargetAgentID/MailboxSessionID 仅用于 mail-notifier 生成的定向
	// 唤醒任务。TargetAgentID 是认领硬约束，避免同 event_type 的其它副本
	// 抢走不属于自己的邮箱分区；SessionID 与 Task.RunID 一起绑定待消费邮件。
	// 普通任务与旧快照两字段全空，不改变既有路由语义。
	MailboxTargetAgentID string `json:"mailbox_target_agent_id,omitempty"`
	MailboxSessionID     string `json:"mailbox_session_id,omitempty"`

	// Artifacts 是任务执行期间通过 write_file/edit_file 实际写入的文件路径列表，
	// 路径为相对项目根的相对路径，自动去重。
	// 由 Store.AppendArtifact 在工具调用成功后写入。
	// 用途：下游依赖任务可以通过 Store.GetDependencyArtifacts 拿到这个列表，
	// 注入到自己的 user prompt 中，避免凭空捏造上游产出。
	Artifacts []string

	// ArtifactMeta 是 Artifacts 的并行元数据通道：key 为与 Artifacts 相同的
	// 相对路径，value 为登记时刻算出的内容 hash / 字节数。验收的 file_hash
	// 证据可直接读这里，不必事后重算 SHA256。
	//
	// 设计取舍（为什么不改 Artifacts 的类型）：Artifacts []string 被看板快照、
	// ExpectedArtifacts 合约校验、GetDependencyArtifacts 注入等大量代码按
	// 字符串列表消费，改类型牵连面过大；并行 map 与 ReadSet 同型，零值兼容——
	// 旧会话快照 / 旧 artifacts.jsonl 没有元数据时该字段为 nil/缺 key，
	// 消费方按"无元数据"降级即可。
	//
	// 写入路径：LocalWriteGroup 在 write/edit 返回前同步调
	// Store.AppendArtifactWithMeta，异步 record-artifact Reactor 仅兼容补登；
	// 查询路径：GetTask 等读 API 的克隆体上
	// 直接取 task.ArtifactMeta[path]。同一文件被重复写入时保留最新一次的
	// 元数据（last-wins，与 artifacts.jsonl 重放语义一致）。
	ArtifactMeta map[string]ArtifactMeta `json:"artifact_meta,omitempty"`

	// ExpectedArtifacts 是发布者声明的"本任务必须产出的文件路径"清单。
	// 任务结束时 agent.processTask 会校验 Artifacts 是否包含全部 ExpectedArtifacts，
	// 缺失则任务失败重试。这是 Level 3 的硬性合约校验。
	// 路径同样为相对项目根的相对路径。
	ExpectedArtifacts []string

	// LastResponse 是 agent 最近一次 LLM 非工具响应的原始文本（worker 的"我做完了"那句话）。
	// 在每次 worker 提交"无 tool call"响应时由 Store.RecordLastResponse 写入；
	// 与 Results 不同，即使 ExpectedArtifacts 校验失败导致任务回滚重试，
	// LastResponse 也会保留——scheduler 可以借此看到 worker 自述了什么，
	// 即便任务最终崩溃，也不至于只看到一个"重试次数耗尽"的空错误。
	LastResponse string

	// SchedulerBatch 是 scheduler agent 当前 reactLoop 跟踪的子任务 ID 列表。
	// 由 SchedulerGroup.publishTask 在每次发布时追加；report_done 时清空。
	// 仅在 EventType="__scheduler__" 任务上有意义；其他 task 该字段为空。
	//
	// 与 Dependencies 的关键差异：
	//   - Dependencies 用于 worker 任务间的依赖（watchdog 会级联取消失败的依赖）
	//   - SchedulerBatch 仅供 SchedulerExecutor 等待 batch 完成（终态而非严格 completed）
	//
	// Phase 3 引入；零值兼容现有调用方。
	SchedulerBatch []string

	// ReadSet 是 v5 Phase 6 引入的"任务级已读集合"显式状态
	// （ReactiveSystem.md §5.2.1）。
	//
	// key 为文件**绝对路径**（与 path-boundary 规范化结果一致），value 为
	// ReadInfo 元数据。由 read-set-write Reactor 在 read_file 成功事件触发时
	// 异步写入；require-read-before-write Gate 直接读，O(1) 替代反查日志。
	//
	// 任务结束（completed / failed / cancelled）后 ReadSet 不主动清理——与
	// Artifacts 同等待遇，留在任务对象中供事后查询；Store 历史压缩策略未来
	// 由专门模块处理。
	//
	// 跨任务不共享：同一 agent 跨任务时，前一任务读过的文件不影响后一任务的
	// 判定。跨任务"项目知识"由 MemoryManageSystem 的 Project Memory 承接。
	ReadSet map[string]ReadInfo `json:"read_set,omitempty"`

	// CreatedAt is the immutable publication timestamp. PendingSince is the
	// start of the current queue lease and is reset whenever a processing Task
	// genuinely returns to pending. A claimed Task has a zero PendingSince.
	CreatedAt    time.Time
	PendingSince time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
}

// Framework 拥有的 EventSource 稳定词表。跨层授权不得各自手写字符串。
const TaskEventSourceLoopIntervention = "loop-intervention"

// NodeCapability 是 DAG 节点级的能力声明，随 Task 携带。
// 三方（model / store / agent）共用的统一契约，字段语义：
//   - Tools 非空：当次任务的工具子集。核心不变式：节点工具集 ⊆ 认领 runner
//     白名单。Store.QueryAvailable 据此过滤不可认领的任务；agent.processTask
//     据此把 executor 的工具注册表换成过滤视图（越界则任务直接失败）。
//   - Model 非空：当次任务的模型覆盖，任务结束时恢复原值。
//
// 两字段都为空（或整体为 nil）时等价于"无节点能力约束"，各消费方按零开销
// 短路处理。
type NodeCapability struct {
	Tools     []string       `json:"tools,omitempty"`     // 非空 = 当次任务工具子集
	Model     string         `json:"model,omitempty"`     // 非空 = 当次任务模型覆盖
	Isolation *IsolationSpec `json:"isolation,omitempty"` // 非 nil = 当次任务启用执行隔离
}

// IsolationModeWorkspace 是目前唯一支持的隔离模式：写时复制 overlay。
const IsolationModeWorkspace = "workspace"

// IsolationSpec 声明节点的执行隔离方式。Mode 目前唯一合法值为
// IsolationModeWorkspace（"workspace"）：认领该任务的 Runner 在写时复制
// overlay 中执行——读穿透主根、写落任务专属 workspace
// （.agentgo/workspaces/<taskID>/），任务成功终态由控制面合并回主根
// （自动三路合并优先，冲突交 Scheduler 裁决兜底）。
type IsolationSpec struct {
	Mode string `json:"mode,omitempty"`
}

// ArtifactMeta 是 Task.ArtifactMeta 的 value 类型，记录单个产物文件在
// 登记时刻的内容元数据。两字段同为零值表示"无元数据"（旧日志 / 读取失败
// 降级），消费方应据此跳过 hash 证据比对而不是当作空文件。
type ArtifactMeta struct {
	SHA256 string `json:"sha256,omitempty"` // 登记时刻文件内容的 SHA256（hex）
	Bytes  int64  `json:"bytes,omitempty"`  // 登记时刻文件字节数
}

// IsZero 报告元数据是否为空（未登记或登记时读取失败降级）。
func (m ArtifactMeta) IsZero() bool { return m.SHA256 == "" && m.Bytes == 0 }

// ReadInfo 是 Task.ReadSet 的 value 类型，记录单个文件被读取的元数据。
type ReadInfo struct {
	FilePath   string    `json:"file_path"`              // 冗余存储绝对路径（与 map key 一致），便于 list 输出
	ReadAt     time.Time `json:"read_at"`                // 首次读取时间戳（首次写入时设定，后续不变）
	Loop       int       `json:"loop,omitempty"`         // 触发首次读取的 ReactLoop 轮次
	Hash       string    `json:"hash,omitempty"`         // 读取时刻的文件 SHA256（v5 Phase 6 暂不填，与 v4 §7 hashline 整合留作 v5.x 增量）
	LastReadAt time.Time `json:"last_read_at,omitempty"` // 最近一次读取时间戳（多次读取覆盖）
}
