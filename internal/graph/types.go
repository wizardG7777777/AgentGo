// Package graph 实现 V6 的 JSON GraphDocument 契约层与 Graph Runtime 引擎核心。
//
// 一张图由版本化 JSON GraphDocument 直接代表：Scheduler 提交的 JSON 经
// ParseAndValidate 校验后即为 Graph Runtime 的执行契约，不引入第二条
// GraphDraft → GraphIR 转换链。本包当前承载四件事：
//   - 类型化模型（types.go）与图/节点状态机；
//   - 完整输入校验链（validate.go，阶段顺序见 ParseAndValidate 注释）；
//   - 只覆盖执行语义字段的图摘要（digest.go）；
//   - 持久化（GraphStore：store.go / journal.go / recover.go）与执行引擎
//     （runtime.go：controller/agent/tool/router/join/approval/wait_event/
//     acceptance/subgraph/end 十种节点类型全部具备执行语义；acceptance 为
//     发任务型节点，默认路由 acceptance.verify）。
//
// Scheduler 工具与 bootstrap 接线在后续切片落地。
// 图允许回边（如 verify → implement 反馈环），本包不做 DAG 无环校验。
package graph

import (
	"encoding/json"
	"time"
)

// SchemaV1 是首版 GraphDocument 的 schema 标识，必须逐字匹配。
const SchemaV1 = "agentgo.graph/v1"

// SchemaV2 是终态契约 v2（封闭终态 + 数据流路由）的 schema 标识，必须逐字
// 匹配。v2 废弃 agent/controller 出边的业务事件名，业务路由全走 result
// 数据字段 + path 条件（权威设计见 docs/design/graph-terminal-contract-v2.md）。
const SchemaV2 = "agentgo.graph/v2"

// GraphDocument 是整张图的类型化模型（JSON 对外契约 + 进程内读写对象）。
//
// 字段所有权（由 GraphStore 的角色分离变更 API + CAS 强制，见 store.go）：
//   - Scheduler：写定义字段（Nodes 内的 Kind / Task / Capability / Next、Root、Revision）；
//   - Graph Runtime：写 Status（图与节点）与 StateVersion；
//   - Graph Runtime：提交落图时给 SessionID 盖章（此后只读，见 runtime.go）；
//   - 调度与认领系统：写节点的 Executor；
//   - Agent Loop / Harness：写节点的 Execution（结果与证据引用）。
type GraphDocument struct {
	Schema       string      `json:"schema"`        // 必须恰为 SchemaV1 或 SchemaV2
	GraphID      string      `json:"graph_id"`      // 图 ID，非空，字符集见校验链
	Revision     int64       `json:"revision"`      // 定义版本：任务定义/节点/连接/执行要求变化时 +1
	StateVersion int64       `json:"state_version"` // 状态版本：认领/进度/结果/审批/边选择变化时 +1
	Root         string      `json:"root"`          // 唯一的根节点 ID，必须指向真实节点
	Status       GraphStatus `json:"status"`        // 图状态（Graph Runtime 写）
	// SessionID 是图的 session 归属（Session 生命周期隔离）：Graph Runtime
	// 提交时经可注入的 sessionIDProvider 盖章；不属于执行语义，不进入
	// digest（digest.go 的 canonicalDoc 不收录本字段）。历史图可为空串；
	// 2026-08 起空归属图不再归并给当前 session，启动时按僵尸图停驻。
	SessionID string          `json:"session_id,omitempty"`
	Nodes     map[string]Node `json:"nodes"` // 节点表，键为节点 ID
}

// Node 是图中的一个节点。同一节点对象同时承载定义与运行状态，
// 各字段的写入者固定（见 GraphDocument 注释）。
type Node struct {
	// ---- 定义字段（Scheduler 写，进入 digest） ----
	Kind       NodeKind      `json:"kind"`                 // 节点类型，首批 10 种枚举
	Task       *NodeTask     `json:"task,omitempty"`       // 任务目标与输出契约
	Capability *Capability   `json:"capability,omitempty"` // 能力需求（本轮只做结构校验）
	Next       []Transition  `json:"next"`                 // 后续转移；end 必须为空，非 end 必须非空
	Wait       *WaitSpec     `json:"wait,omitempty"`       // wait_event 专属：事件等待声明
	Tool       *ToolSpec     `json:"tool,omitempty"`       // tool 专属：工具调用声明
	Subgraph   *SubgraphSpec `json:"subgraph,omitempty"`   // subgraph 专属：内联子图

	// ---- 运行状态字段（非 Scheduler 写，不进入 digest） ----
	Status    NodeStatus `json:"status"`    // 节点状态（Graph Runtime 写）
	Executor  *Executor  `json:"executor"`  // 认领者（调度/认领系统写），null 表示未认领
	Execution *Execution `json:"execution"` // 执行事实（Agent Loop / Harness 写），null 表示未开始

	// ---- 扩展字段（不参与执行语义，不进入 digest） ----
	Metadata   map[string]string          `json:"metadata,omitempty"`   // 只用于标签、展示与检索
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"` // 未知可选扩展原样保留，不参与运行
}

// NodeDefinition 是一次 activation 冻结的节点定义。Graph Runtime 在创建
// activation 时把当时 revision 下会影响本次执行的定义复制到 Execution；
// 后续 patch 可以更新 Node 的定义面，但在途 activation 的任务发布、等待、
// 验收与转移求值始终读取这份快照。新 activation 才读取 patch 后的 Node。
//
// Metadata/Extensions 虽不进入 Graph digest，仍一并冻结：metadata.route 会
// 影响任务路由，恢复补发时必须与首次激活保持一致；extensions 则保持对外
// activation 审计的完整性。
type NodeDefinition struct {
	Kind       NodeKind                   `json:"kind"`
	Task       *NodeTask                  `json:"task,omitempty"`
	Capability *Capability                `json:"capability,omitempty"`
	Next       []Transition               `json:"next"`
	Wait       *WaitSpec                  `json:"wait,omitempty"`
	Tool       *ToolSpec                  `json:"tool,omitempty"`
	Subgraph   *SubgraphSpec              `json:"subgraph,omitempty"`
	Metadata   map[string]string          `json:"metadata,omitempty"`
	Extensions map[string]json.RawMessage `json:"extensions,omitempty"`
}

// NodeTask 描述节点的任务目标与输出契约。
type NodeTask struct {
	Title       string `json:"title"`                 // 任务标题，非空
	Description string `json:"description,omitempty"` // 任务描述（可选）
	// RequiredInputs 显式声明 join / acceptance 的必需目标输入端口。
	// 非空时 Runtime 只在每个端口均被一条实际选中的转移绑定到当前
	// activation 后进入 data-ready；端口是单赋值，并行来源使用不同端口。
	// 互斥分支当前保持各自后续/各自 end，不能共享端口；OR mux
	// 等待 generation/correlation token 后再开放。空时仅保留旧单
	// 入边/历史图兼容语义。
	RequiredInputs []string `json:"required_inputs,omitempty"`
	// RequiredEvidence 声明 acceptance 对某个输入端口所需的可解引用证据种类。
	// 输入端口齐备但证据缺失时仍发布 verifier 任务并显式注入缺口；verifier
	// 必须 blocked，若仍自报 completed/pass，Runtime 不予采信。
	RequiredEvidence []EvidenceRequirement `json:"required_evidence,omitempty"`
}

type EvidenceRequirement struct {
	Input string `json:"input"`
	Kind  string `json:"kind"`
}

// Capability 声明节点的能力需求；本轮只做结构形状校验，工具名/模型名校验留给后续。
type Capability struct {
	Tools     []string `json:"tools,omitempty"`     // 工具名子集（名字校验留给后续）
	Model     string   `json:"model,omitempty"`     // 模型名覆盖
	Isolation string   `json:"isolation,omitempty"` // 执行隔离，仅允许 "workspace"
}

// IsolationWorkspace 是 capability.isolation 唯一合法值。
const IsolationWorkspace = "workspace"

// WaitSpec 是 wait_event 节点的事件等待声明。
type WaitSpec struct {
	Event string `json:"event"` // 等待的外部事件名，必填非空
	// TimeoutSec 是等待超时秒数（>0）；Runtime 把绝对 deadline 冻结进
	// Execution，进程重启后按原 deadline 恢复并以 event=timeout 求值。
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

// ToolSpec 是 tool 节点的工具调用声明：激活时由注入的 ToolExecutor
// 以 name + args 同步执行，返回值成为节点 Result。
type ToolSpec struct {
	Name string         `json:"name"`           // 工具名，必填非空
	Args map[string]any `json:"args,omitempty"` // 静态调用参数（JSON 可序列化）
}

// SubgraphSpec 是 subgraph 节点的内联子图：激活时物化为独立图提交到同一
// Store（graph_id = <父图ID>/<父节点activationID>），父节点挂起等待子图终态。
type SubgraphSpec struct {
	Root  string          `json:"root"`  // 子图根节点 ID，必须指向 Nodes 内真实节点
	Nodes map[string]Node `json:"nodes"` // 内联子图节点表（递归走完整语义校验）
}

// MaxSubgraphDepth 是 subgraph 嵌套最大深度：顶层图深度为 1，每嵌套/物化
// 一层 +1，超过即拒绝（校验期拒绝静态超限，物化期防御运行期超限）。
const MaxSubgraphDepth = 4

// Executor 记录节点的认领者。本轮只有 "agent" 一种类型。
type Executor struct {
	Type    string `json:"type"`     // 仅允许 "agent"
	AgentID string `json:"agent_id"` // 认领者 ID，非空
}

// ExecutorTypeAgent 是 executor.type 唯一合法值。
const ExecutorTypeAgent = "agent"

// Execution 记录节点的一次执行事实与结果引用。
type Execution struct {
	Phase              string          `json:"phase"`                         // 执行阶段（如 planning）
	TaskID             string          `json:"task_id,omitempty"`             // 本次激活创建的 Task ID
	ActivationID       string          `json:"activation_id,omitempty"`       // 激活 ID（回边重进节点时每次新建）
	DefinitionRevision int64           `json:"definition_revision,omitempty"` // 本 activation 冻结定义所属 revision
	Definition         *NodeDefinition `json:"definition,omitempty"`          // 本 activation 的定义快照
	// ResultRef 是 activation 级完整 Result 的稳定引用，可交给 Store.
	// ResolveActivationResult 解引用；ResultSummary 仅供 UI/日志展示。两者严禁
	// 混用，否则大结果、子图和重启恢复会把截断摘要误当数据引用。
	ResultRef     string `json:"result_ref,omitempty"`
	ResultSummary string `json:"result_summary,omitempty"`
	// Input 是本 activation 的持久化输入绑定集（数据流图语义）：activation
	// 创建时从指向本 activation 的已生效 TransitionRecord.Input 推导并随
	// activation 事实落盘，恢复后绑定不变。普通节点由实际选中边传入；
	// barrier 型输入按 target_input 端口由 join（或 acceptance 的 data-ready
	// 门控）归集，不依赖静态来源的 first-arrival 推断。
	Input []InputBinding `json:"input,omitempty"`
	// Evidence 是本 activation 终态结算时随 Settlement 持久化的可观察证据
	// 条目（任务型节点：由终态事实携带的工具调用事实与 artifact 引用组装；
	// 非任务型节点恒空）。EvidenceRefs 保留其 Ref 列表便于轻量引用。
	Evidence []EvidenceEntry `json:"evidence,omitempty"`
	// Settlement 是节点终态与其后续结算动作的 durable 输入。它与节点终态
	// 同条 execution_status journal 记录落盘；若进程死在终态落盘后、边选择或
	// Graph 收官前，ResumeGraph 用它幂等续跑，不能依赖截断的 ResultRef 猜条件。
	Settlement   *TerminalSettlement `json:"settlement,omitempty"`
	EvidenceRefs []string            `json:"evidence_refs,omitempty"` // 证据 / artifact 引用
	// WaitDeadline 是 wait_event activation 的绝对超时点。deadline 与
	// activation 事实同条 durable 记录落盘，恢复后按原时间继续等待；nil 表示
	// 不设超时。
	WaitDeadline *time.Time `json:"wait_deadline,omitempty"`
	// RequestID 是 approval 节点向 ApprovalGateway 发出的审批请求句柄
	// （空表示尚未请求；恢复时凭它避免重复请求，幂等键 = (graph_id, activation_id)）。
	RequestID string `json:"request_id,omitempty"`
	// ChildGraphID 是 subgraph 节点物化出的子图 ID
	// （<父图ID>/<父节点activationID>；空表示尚未物化）。
	ChildGraphID string `json:"child_graph_id,omitempty"`
	// OutletCheck 是终态契约 v2 提交期出路检查的两击持久态（仅 schema v2 图
	// 的任务型节点使用）。随 Execution durable（与 execution_status journal
	// 记录同条生效），崩溃恢复不丢；activation 以 "new" 重进时随 Execution
	// 整体替换自然归零。
	OutletCheck *OutletCheckState `json:"outlet_check,omitempty"`
}

// OutletCheckState 是终态契约 v2（docs/design/graph-terminal-contract-v2.md §6）
// 两击升级协议的按 activation 持久化计数态：「无匹配出路」的提交按 activation
// 计数，参数级错误（携带 event、status 越界、verdict 用于非 acceptance）不计入。
type OutletCheckState struct {
	// Strikes 是「无匹配出路」提交的次数：1 = 首击（已拒绝、可修正重交）；
	// 2 = 第二击（已升级 Scheduler 裁决）。
	Strikes int `json:"strikes,omitempty"`
	// FirstSubmission 是首击提交（status + result）的有界摘要，第二击的失败
	// 原因需对账两次提交内容。
	FirstSubmission string `json:"first_submission,omitempty"`
	// Escalated 为 true 表示第二击已升级（节点 failed + no-outlet 唤醒已发）；
	// 后续提交幂等拒绝，不重复升级、不重复唤醒。
	Escalated bool `json:"escalated,omitempty"`
}

// ============================================================
// 数据流（Result→Input 绑定与证据）
// ============================================================

// InputInlineMaxBytes 是 EdgeInput / InputBinding 内联完整 Result 的字节
// 上限；超限只携带摘要与证据引用（Truncated=true）。
const InputInlineMaxBytes = 32 << 10 // 32 KiB

// InputSummaryMaxRunes 是输入绑定有界摘要的 rune 上限。
const InputSummaryMaxRunes = 2048

// EvidenceSummaryMaxRunes 是单条证据摘要的 rune 上限。
const EvidenceSummaryMaxRunes = 200

// EvidenceCommandMaxRunes / EvidencePathMaxRunes 是结构化证据字段的独立
// 上限。命令与路径不能被压进 200-rune 展示摘要，否则 verifier 无法核对真实
// 退出事实或读取 artifact；超限时保留显式 truncated 标志，禁止静默截断。
const (
	EvidenceIdentityMaxRunes = 512
	EvidenceCommandMaxRunes  = 4096
	EvidencePathMaxRunes     = 4096
)

// EdgeInput 是一条实际生效转移绑定的源 activation 输出（数据流图的持久化
// InputRef）：与边选择同条 TransitionRecord journal 记录落盘，恢复后绑定
// 不变。Result 在不超过 InputInlineMaxBytes 时内联完整 durable Result；
// 超限时 Truncated=true、Result 为空；消费方必须以 ResultRef 解引用完整
// activation Result（证据另由 Evidence/EvidenceRefs 保持谱系），不得把
// 展示 Summary 当作原始数据。
type EdgeInput struct {
	Summary string `json:"summary,omitempty"`
	// ResultRef 指向 activation 级 durable Result Store；大结果不内联时仍可
	// 在重启、回边覆盖源节点最新 Execution 后精确解引用。
	ResultRef    string          `json:"result_ref,omitempty"`
	Evidence     []EvidenceEntry `json:"evidence,omitempty"`
	EvidenceRefs []string        `json:"evidence_refs,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Truncated    bool            `json:"truncated,omitempty"`
	// WorkLog 是来源 activation 对应 Task 的工具调用工作记录（2026-08-21
	// 上游摘要）：转移结算时由 Runtime 经注入的 provider 一次性聚合渲染并
	// 随本记录冻结——下游据此感知「上游实际做了什么」（机械事实，非上游
	// 自述），空串表示无 provider 或来源无调用记录可述。
	WorkLog string `json:"work_log,omitempty"`
}

// InputBinding 是目标 activation 的一份持久化输入绑定：activation 创建时
// 从指向本 activation 的已生效 TransitionRecord.Input 推导，随 activation
// 事实落盘。fan-out 的多个命中下游共享同一来源绑定；多上游输入必须经
// join 归并（或 acceptance 的 data-ready 门控），不得以截断摘要代替完整
// durable Result 充当归并值。
type InputBinding struct {
	SourceNodeID       string          `json:"source_node_id"`
	SourceActivationID string          `json:"source_activation_id"`
	TargetInput        string          `json:"target_input,omitempty"`
	Summary            string          `json:"summary,omitempty"`
	ResultRef          string          `json:"result_ref,omitempty"`
	Evidence           []EvidenceEntry `json:"evidence,omitempty"`
	EvidenceRefs       []string        `json:"evidence_refs,omitempty"`
	Result             json.RawMessage `json:"result,omitempty"`
	Truncated          bool            `json:"truncated,omitempty"`
	// WorkLog 透传自 TransitionRecord.Input.WorkLog（来源 Task 的工具调用
	// 工作记录，转移结算时冻结）。随本绑定落盘，恢复后不变。
	WorkLog string `json:"work_log,omitempty"`
}

// ActivationResult 是按 activation 保存的完整、不可变 Result 与证据事实。
// GraphDocument.Node.Execution 只保存节点最新 activation，因此回边会覆盖旧
// Execution；该记录属于 Store 的 entry 级簿记，并随 journal/snapshot 压缩
// 恢复。TransitionRecord.Input.ResultRef 是其稳定消费者引用。
type ActivationResult struct {
	Ref          string          `json:"ref"`
	NodeID       string          `json:"node_id"`
	ActivationID string          `json:"activation_id"`
	Result       json.RawMessage `json:"result"`
	Evidence     []EvidenceEntry `json:"evidence,omitempty"`
}

// EvidenceEntry 是随 activation 终态结算持久化的一条可观察证据。结构化字段
// 是 verifier 的消费权威，Summary 只是有界展示兜底；Ref 由调用/内容身份稳定
// 生成，绝不使用查询序数。Success 用指针区分“调用失败(false)”与 artifact 等
// “不适用(nil)”。
type EvidenceEntry struct {
	Ref     string `json:"ref"`
	Kind    string `json:"kind"` // shell / file_write / file_edit / read / web / artifact / 其它工具名
	Summary string `json:"summary,omitempty"`

	CallID   string `json:"call_id,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Success  *bool  `json:"success,omitempty"`

	Command          string `json:"command,omitempty"`
	CommandTruncated bool   `json:"command_truncated,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`

	// Path 对 file/read 工具表示其目标路径，对 kind=artifact 表示完整产物路径。
	Path          string `json:"path,omitempty"`
	PathTruncated bool   `json:"path_truncated,omitempty"`
}

// SettlementContinuation 声明节点终态落盘后仍须完成的 durable 动作。
type SettlementContinuation string

const (
	// SettlementContinueTransitions 按冻结定义和完整 Result 选择/补激活出边。
	SettlementContinueTransitions SettlementContinuation = "transitions"
	// SettlementContinueGraphComplete 用于 end 节点：补写 Graph completed。
	SettlementContinueGraphComplete SettlementContinuation = "graph_complete"
	// SettlementContinueGraphFail 用于控制面失败：补写 Graph failed。
	SettlementContinueGraphFail SettlementContinuation = "graph_fail"
	// SettlementContinueNone 用于 Graph 整体终态时被取消的其余
	// activation：取消事实需要 durable，但不得再选边或驱动 Graph。
	SettlementContinueNone SettlementContinuation = "none"
)

// TerminalSettlement 是一次 terminal activation 的可恢复结算事实。新记录
// 以 ResultRef 引用 activation Result Store 的完整 JSON 对象；Reason 供
// graph_fail 与 Graph 终态取消诊断。
// 该记录会保留在 Execution 中，后续 ResumeGraph 重放靠 TransitionRecord /
// Graph 终态自然幂等，不需要另写一个易产生新崩溃窗口的 cleared 标记。
type TerminalSettlement struct {
	Status NodeStatus `json:"status"`
	// ResultRef 指向 activation Result Store 的完整终态对象。Result 只用于
	// 恢复升级前的旧 journal；新记录不再把完整对象复制进 GraphDocument。
	ResultRef    string                 `json:"result_ref,omitempty"`
	Result       json.RawMessage        `json:"result,omitempty"`
	Continuation SettlementContinuation `json:"continuation"`
	Reason       string                 `json:"reason,omitempty"`
}

// Transition 是节点的一条出边。
type Transition struct {
	To         string `json:"to"`                   // 目标节点 ID，必须指向存在的节点
	Activation string `json:"activation,omitempty"` // 激活方式，仅允许 "new"（缺省即沿用常规激活）
	// TargetInput 是目标 join / acceptance 的输入端口。并行必需输入使用不同
	// 单赋值端口；互斥分支当前保持各自后续/各自 end。
	TargetInput string     `json:"target_input,omitempty"`
	When        *Condition `json:"when,omitempty"` // 转移条件，缺省表示无条件
}

// ActivationNew 是 transition.activation 唯一合法值：
// 回边重进目标节点时创建新的 activation_id / Task / Trace，不重开旧 Task。
const ActivationNew = "new"

// Condition 是转移条件，只允许两种形态之一，其它（字符串表达式、函数字段等
// 脚本式条件）一律拒绝：
//   - 事件形态：{ "event": "<事件名>" }；
//   - 条件形态：{ "path": "$.xxx", "operator": "eq|ne|in|exists", "value": <标量或字符串列表> }。
type Condition struct {
	Event    string          `json:"event,omitempty"`    // 事件形态：事件名枚举（含 always）
	Path     string          `json:"path,omitempty"`     // 条件形态：结果取值路径，必须 "$." 开头
	Operator string          `json:"operator,omitempty"` // 条件形态：eq / ne / in / exists
	Value    json.RawMessage `json:"value,omitempty"`    // 条件形态：比较值（exists 不得携带）
}

// 转移事件名枚举（IsValidEventName 为权威判定）。
const (
	EventReady     = "ready"
	EventCompleted = "completed"
	EventFixable   = "fixable"
	EventFailed    = "failed"
	EventBlocked   = "blocked"
	EventPass      = "pass"
	EventApproved  = "approved"
	EventRejected  = "rejected"
	EventTimeout   = "timeout"
	EventAlways    = "always"
)

// 转移条件操作符枚举（IsValidOperator 为权威判定）。
const (
	OpEq     = "eq"
	OpNe     = "ne"
	OpIn     = "in"
	OpExists = "exists"
)

// IsValidEventName 报告事件名是否属于转移事件枚举（含 always）。
func IsValidEventName(name string) bool {
	switch name {
	case EventReady, EventCompleted, EventFixable, EventFailed, EventBlocked,
		EventPass, EventApproved, EventRejected, EventTimeout, EventAlways:
		return true
	}
	return false
}

// IsValidOperator 报告操作符是否属于转移条件操作符枚举。
func IsValidOperator(op string) bool {
	switch op {
	case OpEq, OpNe, OpIn, OpExists:
		return true
	}
	return false
}

// ============================================================
// 节点类型枚举
// ============================================================

// NodeKind 是节点类型，首批 10 种；未知类型拒绝。
type NodeKind string

const (
	KindController NodeKind = "controller"
	KindAgent      NodeKind = "agent"
	KindTool       NodeKind = "tool"
	KindRouter     NodeKind = "router"
	KindJoin       NodeKind = "join"
	KindApproval   NodeKind = "approval"
	KindWaitEvent  NodeKind = "wait_event"
	KindAcceptance NodeKind = "acceptance"
	KindSubgraph   NodeKind = "subgraph"
	KindEnd        NodeKind = "end" // 图的结束节点（避免与命令行终端 terminal 混淆）
)

// IsValid 报告节点类型是否属于首批支持的枚举。
func (k NodeKind) IsValid() bool {
	switch k {
	case KindController, KindAgent, KindTool, KindRouter, KindJoin,
		KindApproval, KindWaitEvent, KindAcceptance, KindSubgraph, KindEnd:
		return true
	}
	return false
}

// ============================================================
// 图状态枚举与状态机
// ============================================================

// GraphStatus 是图状态枚举。
type GraphStatus string

const (
	GraphPending   GraphStatus = "pending"
	GraphRunning   GraphStatus = "running"
	GraphPaused    GraphStatus = "paused"
	GraphCompleted GraphStatus = "completed"
	GraphFailed    GraphStatus = "failed"
	GraphCancelled GraphStatus = "cancelled"
)

// IsValid 报告图状态是否是合法枚举值。
func (s GraphStatus) IsValid() bool {
	switch s {
	case GraphPending, GraphRunning, GraphPaused, GraphCompleted, GraphFailed, GraphCancelled:
		return true
	}
	return false
}

// IsTerminal 报告图状态是否为终态（终态无任何出边）。
func (s GraphStatus) IsTerminal() bool {
	switch s {
	case GraphCompleted, GraphFailed, GraphCancelled:
		return true
	}
	return false
}

// graphStatusTransitions 是图状态机的合法迁移表；缺项的 from 即终态。
var graphStatusTransitions = map[GraphStatus][]GraphStatus{
	GraphPending: {GraphRunning, GraphCancelled},
	GraphRunning: {GraphPaused, GraphCompleted, GraphFailed, GraphCancelled},
	GraphPaused:  {GraphRunning, GraphFailed, GraphCancelled},
}

// IsValidGraphStatusTransition 报告图状态迁移 from → to 是否合法。
func IsValidGraphStatusTransition(from, to GraphStatus) bool {
	if !from.IsValid() || !to.IsValid() {
		return false
	}
	for _, next := range graphStatusTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ============================================================
// 节点状态枚举与状态机
// ============================================================

// NodeStatus 是节点状态枚举。
type NodeStatus string

const (
	NodeInactive  NodeStatus = "inactive"
	NodeReady     NodeStatus = "ready"
	NodeRunning   NodeStatus = "running"
	NodeWaiting   NodeStatus = "waiting"
	NodeCompleted NodeStatus = "completed"
	NodeBlocked   NodeStatus = "blocked"
	NodeFailed    NodeStatus = "failed"
	NodeCancelled NodeStatus = "cancelled"
	NodeSkipped   NodeStatus = "skipped"
)

// IsValid 报告节点状态是否是合法枚举值。
func (s NodeStatus) IsValid() bool {
	switch s {
	case NodeInactive, NodeReady, NodeRunning, NodeWaiting, NodeCompleted,
		NodeBlocked, NodeFailed, NodeCancelled, NodeSkipped:
		return true
	}
	return false
}

// IsTerminal 报告节点状态是否为终态。终态是「一次 activation 的终态」：
// completed/failed 可因回边重进入（新 activation）被 Graph Runtime 显式
// 重置为 ready（见 nodeStatusTransitions 注释），但那是一次新 activation
// 的开始，不是旧 activation 的常规出边。
func (s NodeStatus) IsTerminal() bool {
	switch s {
	case NodeCompleted, NodeFailed, NodeCancelled, NodeSkipped:
		return true
	}
	return false
}

// nodeStatusTransitions 是节点状态机的合法迁移表；缺项的 from 即终态。
//
// 要点：inactive 必须先经 ready 才能 running；running 可挂起为 waiting
// （等事件/审批）再由 waiting 恢复 running；blocked 可由 replan 修回 ready；
// skipped（被路由绕过）与 completed/failed/cancelled 同为终态。
//
// Graph Runtime（V6 §6-16）补充的四条边：
//   - completed/failed → ready：回边重进入 = 新 activation，节点状态随新
//     activation 重置为 ready（旧 activation 的 execution 已被整体替换）；
//   - ready → failed：activation 已 durable 但任务发布失败；
//   - ready → waiting：activation 已 durable 但节点类型尚未实现，挂起等待。
var nodeStatusTransitions = map[NodeStatus][]NodeStatus{
	NodeInactive:  {NodeReady, NodeSkipped, NodeCancelled},
	NodeReady:     {NodeRunning, NodeFailed, NodeWaiting, NodeSkipped, NodeCancelled},
	NodeRunning:   {NodeWaiting, NodeCompleted, NodeBlocked, NodeFailed, NodeCancelled},
	NodeWaiting:   {NodeRunning, NodeFailed, NodeCancelled},
	NodeBlocked:   {NodeReady, NodeFailed, NodeCancelled},
	NodeCompleted: {NodeReady},
	NodeFailed:    {NodeReady},
}

// IsValidNodeStatusTransition 报告节点状态迁移 from → to 是否合法。
func IsValidNodeStatusTransition(from, to NodeStatus) bool {
	if !from.IsValid() || !to.IsValid() {
		return false
	}
	for _, next := range nodeStatusTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}
