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

// GraphDocument 是整张图的类型化模型（JSON 对外契约 + 进程内读写对象）。
//
// 字段所有权（由 GraphStore 的角色分离变更 API + CAS 强制，见 store.go）：
//   - Scheduler：写定义字段（Nodes 内的 Kind / Task / Capability / Next、Root、Revision）；
//   - Graph Runtime：写 Status（图与节点）与 StateVersion；
//   - 调度与认领系统：写节点的 Executor；
//   - Agent Loop / Harness：写节点的 Execution（结果与证据引用）。
type GraphDocument struct {
	Schema       string          `json:"schema"`        // 必须恰为 "agentgo.graph/v1"
	GraphID      string          `json:"graph_id"`      // 图 ID，非空，字符集见校验链
	Revision     int64           `json:"revision"`      // 定义版本：任务定义/节点/连接/执行要求变化时 +1
	StateVersion int64           `json:"state_version"` // 状态版本：认领/进度/结果/审批/边选择变化时 +1
	Root         string          `json:"root"`          // 唯一的根节点 ID，必须指向真实节点
	Status       GraphStatus     `json:"status"`        // 图状态（Graph Runtime 写）
	Nodes        map[string]Node `json:"nodes"`         // 节点表，键为节点 ID
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
	Title        string `json:"title"`                   // 任务标题，非空
	Description  string `json:"description,omitempty"`   // 任务描述（可选）
	OutputSchema string `json:"output_schema,omitempty"` // 结构化结果 schema 标识（可选）
}

// Capability 声明节点的能力需求；本轮只做结构形状校验，工具名/模型名校验留给后续。
type Capability struct {
	Tools     []string           `json:"tools,omitempty"`     // 工具名子集（名字校验留给后续）
	Model     string             `json:"model,omitempty"`     // 模型名覆盖
	Isolation string             `json:"isolation,omitempty"` // 执行隔离，仅允许 "workspace"
	Budget    map[string]float64 `json:"budget,omitempty"`    // 预算项，数值必须非负有限
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
	ResultRef          string          `json:"result_ref,omitempty"`          // 结构化结果引用
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
)

// TerminalSettlement 是一次 terminal activation 的可恢复结算事实。Result
// 保存完整 JSON 对象而非展示用摘要；Reason 只供 graph_fail 续跑复原诊断。
// 该记录会保留在 Execution 中，后续 ResumeGraph 重放靠 TransitionRecord /
// Graph 终态自然幂等，不需要另写一个易产生新崩溃窗口的 cleared 标记。
type TerminalSettlement struct {
	Status       NodeStatus             `json:"status"`
	Result       json.RawMessage        `json:"result"`
	Continuation SettlementContinuation `json:"continuation"`
	Reason       string                 `json:"reason,omitempty"`
}

// Transition 是节点的一条出边。
type Transition struct {
	To         string     `json:"to"`                   // 目标节点 ID，必须指向存在的节点
	Activation string     `json:"activation,omitempty"` // 激活方式，仅允许 "new"（缺省即沿用常规激活）
	When       *Condition `json:"when,omitempty"`       // 转移条件，缺省表示无条件
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
