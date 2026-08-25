package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentgo/internal/fulfillment"
	"agentgo/internal/runcontract"
	"agentgo/internal/trace"
)

// 本文件实现 V6 §6 第 15–17 条的 Graph Runtime 引擎核心。引擎是纯组件：
// 不 import internal/store、internal/agent 等运行包，对任务公告板的依赖
// 收敛为 TaskBoard 小接口（测试用 fake 离线驱动）。
//
// 节点类型执行语义：
//   - controller / agent：每次进入创建新 activation（<nodeID>@<n>，序号
//     durable 单调），先 durable 写 activation 事实（ready）再发任务，
//     发布成功后置 running 并 durable task_id；
//   - router：激活即同步求值自己的 next（以上游 Result 为输入），不发任务；
//   - end：激活即图完成（completed + graph_ended 事件）；
//   - join：就绪性纯推导（不加持久状态）——每个 required input port 均有
//     一条实际选中边绑定到同一目标 activation 后归并完成；端口为单赋值，
//     并行来源使用不同端口表示 AND。旧无端口图仅在恢复时保留“全部静态源
//     终态”的兼容 barrier；
//   - wait_event：durable 挂起 waiting，OnExternalEvent 命中事件名时
//     以事件 data 为 Result 完成并求值转移；
//   - tool：durable executing 后同步调 ToolExecutor，成功/失败分别落
//     completed/failed；未注入 executor 时节点 failed（中文错误）；
//   - approval：durable waiting 后向 ApprovalGateway 发审批请求（requestID
//     记入 execution），OnApprovalDecided 以 approved/rejected 完成；
//     未注入 gateway 时保持「尚未实现」挂起（不报错以外的状态变化）；
//   - subgraph：把内联子图物化为独立图（graph_id = <父图ID>/<activationID>）
//     提交到同一 Store，父节点挂起等待，子图终态回调结算父节点；
//   - acceptance（C5c 起）：发任务型节点——与 controller/agent 同路径经
//     board 发布验收任务（默认路由 acceptance.verify，metadata["route"] 可
//     覆盖），验收判据由节点 task.title/description 携带；验收 agent 按
//     prompt 契约检查后经 submit_task_result 提交 completed，Result 只允许
//     verdict=pass/fixable/failed 三值，业务 event 不被采信；blocked 直接
//     使用任务状态且不填 verdict。终态经 feed 回填 TerminalFact 后，
//     acceptance.go 在结算前做证据谱系和必需证据核验：valid 按
//     $.verdict 转移；disputed / evidence_missing / invalid_verdict 均不
//     采信自报结论，并按 failed 或 blocked 结算后唤醒 graph change。
//
// 崩溃一致性纪律：activation 事实、任务发布结果、节点终态都经
// Store.SetExecutionAndStatus 单条 journal 记录生效；边选择经
// Store.RecordTransition durable 后再激活目标。两处两记录之间仍存在
// fsync 粒度的崩溃窗口（转移已记录、目标未激活 / 任务已发布、task_id 未
// durable），分别由 ResumeGraph 按转移中冻结的 TargetActivationID 补激活与
// TaskBoard 的 (graph_id, activation_id) 对账与幂等补发兜住。等待型节点
// （wait_event/approval/subgraph）从 waiting 恢复 running 再写终态的两条
// 记录之间同样存在窗口：ResumeGraph 把 running 的等待型节点退回 waiting
// 重等；executing 的 tool 节点语义为 unknown，按 V6 原则置 failed 不自动
// 重跑副作用。acceptance 与 controller/agent 同为任务型节点：running 恢复时
// 先查公告板，缺失则按原 activation 幂等补发，已终态则反向补结算 Graph；
// ready 恢复同样按原 activation 幂等补发。

// TaskBoard 是 Graph Runtime 对任务公告板的最小依赖。
//
// 实现要求：PublishGraphTask 必须以 (GraphID, ActivationID) 为幂等键——
// 进程在「任务已发布、task_id 尚未 durable」的崩溃窗口后，ResumeGraph 会
// 用同一 activation 补发，实现方必须去重而不是制造重复任务。
type TaskBoard interface {
	PublishGraphTask(spec TaskSpec) (taskID string, err error)
	LookupGraphTask(graphID, activationID, expectedTaskID string) (GraphTaskSnapshot, bool, error)
}

// GraphTaskTerminator is the optional control-plane half of TaskBoard. The
// Runtime invokes it after the Graph terminal status is durable and before it
// emits graph_ended, so Task state closure remains part of the Graph main flow
// instead of being driven by a Reactor response. Implementations must be
// idempotent: startup recovery may invoke the same operation again to close
// the crash window between the two durable stores.
//
// A termination failure cannot roll back the already-durable Graph outcome.
// Runtime records the failure as a trace error and still emits graph_ended so
// route/Team teardown is not skipped; startup reconciliation retries cleanup.
type GraphTaskTerminator interface {
	TerminateGraphTasks(graphID string) error
}

// GraphTaskSnapshot 是恢复期从 TaskBoard 查询到的最小任务事实。非终态任务
// 的 TerminalStatus 为空；终态只允许 completed/failed/blocked。Result 是
// TaskStore 持有的完整结构化结果，用于在 Graph journal 落后时补结算。
type GraphTaskSnapshot struct {
	TaskID string
	// NodeKind 是 Task 发布时冻结的 Graph 节点类型。空值只可能来自旧快照；
	// Runtime 对非空值与当前 activation 的冻结定义做一致性校验，防止恢复时
	// 把 acceptance 等角色错认成普通执行节点。
	NodeKind       NodeKind
	TerminalStatus NodeStatus
	OutcomeRef     string
	Result         map[string]any
	// Evidence 是任务终态时随快照携带的可观察证据（由公告板桥按
	// ToolCallRecord + Artifacts 组装）：恢复对账路径的 acceptance 谱系
	// 核验与 feed 路径同构，凭它取得 verifier 自身证据。
	Evidence    []EvidenceEntry
	Fulfillment *fulfillment.Record
}

// TaskSpec 是一次节点任务发布的完整描述。
type TaskSpec struct {
	GraphID                 string
	DefinitionDigestVersion string
	RunID                   runcontract.RunID
	RunContract             *runcontract.RunContract
	RunBudgetPermitRef      string
	ProgressContractRef     string
	ContextPolicyRef        string
	NodeID                  string
	ActivationID            string
	// NodeKind 随 activation 冻结并持久化到 model.Task，供执行租约按真实
	// Graph 角色派生控制通道；不能从 route 猜测（acceptance 可自定义 route）。
	NodeKind NodeKind
	// ControllerRole 仅对 controller 有效，随 activation 定义冻结。loop_recovery
	// controller 是 L5 的恢复裁决节点，不是普通 Scheduler/业务 Worker。
	ControllerRole ControllerRole
	// RecoverySourceTaskID 把恢复裁决 Task 精确绑定到触发它的终态 Graph Task；
	// L4 intervention ACK 与 L3 结果读取授权不得解析文本反推该关系。
	RecoverySourceTaskID string
	Title                string
	Description          string
	Route                string // 认领路由（resolveRoute 解析结果 = runner event_type）
	Tools                []string
	Model                string
	Isolation            string
	// Inputs 是本 activation 的持久化输入绑定（数据流）：发布时随任务描述
	// 注入执行上下文（有界摘要 + 证据引用），任务文本冻结后与图内事实一致。
	Inputs []InputBinding
	// RequiredEvidence 与 MissingEvidence 让任务桥把 acceptance 的证据契约和
	// 当前可解引用缺口显式注入 verifier；缺口存在时 verifier 应 blocked。
	RequiredEvidence []EvidenceRequirement
	MissingEvidence  []EvidenceRequirement
	// OutputContract 是终态契约 v2 的输出契约定界块（<output-contract>…），
	// 发布时由 Runtime 从本 activation 冻结出边机械派生（§5）；v1 图与无
	// path 条件节点恒为空串，任务桥不注入。
	OutputContract      string
	TypedOutputContract *NodeOutputContract
	FulfillmentContract *fulfillment.Contract
}

// 节点类型 → 默认认领路由（runner event_type）的映射常量。
const (
	// RouteScheduler 是 controller 节点的默认路由：Scheduler 队列的 event_type。
	RouteScheduler = "__scheduler__"
	// RouteDefaultQueue 是 agent 节点的默认路由：空串 = 默认队列（普通 Worker）。
	RouteDefaultQueue = ""
	// RouteAcceptance 是 acceptance 节点的默认路由：验收 runner 队列
	// （与 config.example.yaml 中 event_type="acceptance.verify" 的验收 agent 对齐）。
	RouteAcceptance = "acceptance.verify"
)

// resolveRoute 解析节点任务的认领路由（C5b）：
//  1. node.metadata["route"] 显式覆盖——非空（去空白后）即用；
//  2. 按节点类型取默认映射（见 Route* 常量）；
//  3. 其余类型不发任务，不涉及路由；防御性回落节点 kind 原名
//     （保持 C5a 透传行为，不在此隐造映射）。
func resolveRoute(node Node) string {
	if r := strings.TrimSpace(node.Metadata["route"]); r != "" {
		return r
	}
	switch node.Kind {
	case KindController:
		return RouteScheduler
	case KindAgent:
		return RouteDefaultQueue
	case KindAcceptance:
		return RouteAcceptance
	}
	return string(node.Kind)
}

// TerminalFact 是一个节点任务的终态事实，驱动 Graph Runtime 的转移求值。
type TerminalFact struct {
	GraphID      string
	NodeID       string
	ActivationID string
	TaskID       string
	Status       NodeStatus     // 仅接受 completed / failed / blocked
	Result       map[string]any // 结构化结果本体（转移求值的输入）
	// Evidence 是该任务终态时由回填方（bootstrap feed）从其 ToolCallRecord
	// 与 Artifacts 组装的可观察证据条目；结算时随 Execution 持久化，
	// 并经 EdgeInput.EvidenceRefs 进入下游输入谱系。非任务型节点恒空。
	Evidence    []EvidenceEntry
	Fulfillment *fulfillment.Record
}

// ToolExecutor 是 Graph Runtime 对工具执行能力的最小依赖（与 TaskBoard
// 同风格的解耦接口）。实现方负责把 name + args 映射到真实工具调用；
// 返回的 map 成为节点 Result（转移求值的输入）。
//
// 实现要求：ExecuteNodeTool 在 Runtime 锁内同步调用，不得回调 Runtime 的
// 任何公开方法（sync.Mutex 不可重入，会死锁）。
type ToolExecutor interface {
	ExecuteNodeTool(ctx context.Context, name string, args map[string]any) (map[string]any, error)
}

// ApprovalGateway 是 Graph Runtime 对人工审批能力的最小依赖。
//
// 实现要求：RequestApproval 必须以 (GraphID, ActivationID) 为幂等键——
// 进程在「请求已发出、requestID 尚未 durable」的崩溃窗口后，ResumeGraph
// 会用同一 activation 补发，实现方必须去重而不是制造重复审批。
// 与 ToolExecutor 一样在 Runtime 锁内同步调用，不得回调 Runtime。
type ApprovalGateway interface {
	RequestApproval(spec ApprovalSpec) (requestID string, err error)
}

// ApprovalSpec 是一次审批请求的完整描述。Title/Description 取自节点 task。
type ApprovalSpec struct {
	GraphID      string
	NodeID       string
	ActivationID string
	Title        string
	Description  string
}

// Runtime 是 Graph Runtime 引擎：直接解释内存中的 GraphDocument，驱动
// activation 创建、任务发布、边选择与图终态。全部公开方法串行于同一把锁，
// 与 Store 的 per 图串行变更叠加后，CAS 在正常路径不会冲突；冲突错误不
// 吞咽、直接上抛。同一 Runtime 实例管理 subgraph 物化出的父子两图。
type Runtime struct {
	store *Store
	board TaskBoard

	toolExec ToolExecutor    // nil 时 tool 节点激活即 failed（中文错误）
	approval ApprovalGateway // nil 时 approval 节点保持「尚未实现」挂起

	// acceptVerifier 已退役（G1b 机器格式契约核验随旧证据契约删除）：
	// acceptance 节点 completed 终态现在一律走引擎内生谱系核验
	//（acceptance.go 判定矩阵），changeWaker 是 disputed 时的 graph change
	// 唤醒器：nil 时只发 graph_change_requested 审计事件，不发布唤醒任务。
	changeWaker GraphChangeWaker

	// sessionIDProvider 提供当前活跃 session ID（Session 生命周期隔离）：
	// 提交落图时给 doc.SessionID 盖章，恢复后归并无归属历史图。惰性求值，
	// session 切换后自动取到新值；nil 时行为同今（归属恒为空串）。
	sessionIDProvider func() string

	// workLogProvider 按来源 Task ID 聚合渲染该任务的工具调用工作记录
	//（2026-08-21 上游摘要）：转移结算时调用一次、渲染结果随 EdgeInput
	// 冻结——下游任务发布时只读冻结文本，绝不按 task_id 回查源账本
	//（与「禁止 inspect_task_calls 旁路」红线同源）。nil 时 WorkLog 恒空。
	workLogProvider func(taskID string) string

	// results 是节点最新 Result 的纯内存缓存（graphID → nodeID → Result），
	// 只作进程内便捷读取。权威数据是 activation Result Store；重启、join、
	// router 与大结果恢复均通过稳定 ResultRef 解引用，绝不回退展示摘要。
	// 图到终态即 eviction。
	results map[string]map[string]map[string]any
	// waitTimers 只是进程内唤醒器；权威 deadline 存在 Execution.WaitDeadline
	// 并随 activation durable。恢复时按该 deadline 重建 timer 或立即超时。
	waitTimers map[string]*time.Timer

	// suspended 是被停驻图的纯内存闸门（Session 生命周期隔离，见
	// runtime_suspend.go）：停驻图的终态事实/审批裁决/外部事件/子图终态
	// 回调输入被吞掉，wait timer 全部停走；图数据与 journal 保持不动。
	// 不进 Store、进程重启即丢失——启动恢复后由控制面按 session 归属重建。
	suspended map[string]struct{}
	// pendingApprovals 暂存停驻期间到达的审批裁决（key 见 approvalKey）。
	// 审批裁决无 durable 面可重建（审批网关以 requestID 幂等去重，恢复不
	// 重发），必须内存暂存、解冻后回放，否则 waiting approval 节点永久悬挂。
	pendingApprovals map[string]approvalDecision
	// runBudgetGate 是跨 Task 的 Run 级可用额度权威。L5 只查询/预留
	// execution 启动资格，不从 Task-local ProgressCheckpoint 猜测余额。
	runBudgetGate   RunBudgetGate
	runStartPermits RunStartPermitAuthority

	// synchronousSteps 只统计一次外部 Runtime 调用中不会让出控制权的机械
	// activation（router/join/end/tool）。它不是业务 activation 预算；任务型
	// Agent 每次发布即让出，跨任务回边不累计。
	synchronousSteps int

	mu sync.Mutex
}

// RunBudgetGate 是 L5 retry 可行性检查所需的最小 Run authority。
type RunBudgetGate interface {
	CanReserve(runcontract.RunID, runcontract.BudgetUsage, time.Time) error
}

type RunStartPermitAuthority interface {
	ReserveExecutionPermit(runcontract.RunID, string, string, time.Time, time.Time) (string, error)
	ValidateExecutionPermit(runcontract.RunID, string, time.Time) error
}

// NewRuntime 创建 Graph Runtime。board 可为 nil——仅当图真正需要发布任务
// （controller/agent 节点激活）时才报错；纯 router/end 图可离板运行。
// ToolExecutor / ApprovalGateway 经 SetToolExecutor / SetApprovalGateway
// 可选注入（保持本签名兼容）。
func NewRuntime(store *Store, board TaskBoard) *Runtime {
	return &Runtime{
		store:            store,
		board:            board,
		results:          make(map[string]map[string]map[string]any),
		waitTimers:       make(map[string]*time.Timer),
		suspended:        make(map[string]struct{}),
		pendingApprovals: make(map[string]approvalDecision),
	}
}

// SetToolExecutor 注入 tool 节点的执行器（构造后、使用前调用）。
func (rt *Runtime) SetToolExecutor(ex ToolExecutor) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.toolExec = ex
}

// SetApprovalGateway 注入 approval 节点的审批网关（构造后、使用前调用）。
func (rt *Runtime) SetApprovalGateway(gw ApprovalGateway) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.approval = gw
}

// SetRunBudgetGate 注入跨 Scheduler/Activation 共享的 Run 预算权威。
func (rt *Runtime) SetRunBudgetGate(gate RunBudgetGate) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.runBudgetGate = gate
	if permits, ok := gate.(RunStartPermitAuthority); ok {
		rt.runStartPermits = permits
	}
}

// SetSessionIDProvider 注入当前活跃 session 的取值器（构造后、使用前调用）；
// nil 时行为同今——提交不盖章、归并为空操作。
func (rt *Runtime) SetSessionIDProvider(fn func() string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.sessionIDProvider = fn
}

// SetWorkLogProvider 注入上游工作记录的聚合渲染器（构造后、使用前调用）。
// 转移结算时对来源 Task ID 调用一次，渲染文本随 EdgeInput.WorkLog 冻结；
// nil 时行为同今（WorkLog 恒空，老图与测试直构路径不受影响）。
func (rt *Runtime) SetWorkLogProvider(fn func(taskID string) string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.workLogProvider = fn
}

// SubmitGraph 校验并提交一张图（durable），随后激活 root。
// 校验/落盘失败时发 graph_submission_rejected 事件并原样返回错误；
// root 激活失败（如未实现节点类型）时图已提交，错误一并返回。
func (rt *Runtime) SubmitGraph(doc *GraphDocument) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.synchronousSteps = 0
	return rt.submitGraphLocked(doc)
}

// PatchGraph 是 Scheduler 定义面变更的 Runtime 串行入口。调用 Store 前持有
// 与终态回填、审批、事件和恢复相同的锁，避免 patch 插入 state_version 的
// 读取与 CAS 写入之间。已创建 activation 的定义由 Execution.Definition
// 冻结；patch 仅影响后续 activation。
func (rt *Runtime) PatchGraph(graphID string, baseRevision int64, patch DefinitionPatch) (int64, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	doc, err := rt.graph(graphID)
	if err != nil {
		return 0, err
	}
	if doc.Revision != baseRevision {
		return 0, &RevisionConflictError{GraphID: graphID, Base: baseRevision, Current: doc.Revision}
	}
	if err := rt.freezePatchedActivationsLocked(graphID, patch); err != nil {
		return 0, err
	}
	return rt.store.PatchGraph(graphID, baseRevision, patch)
}

// submitGraphLocked 是 SubmitGraph 的锁内实现（调用方须持 rt.mu）；
// subgraph 物化子图时经此提交到同一 Store。
func (rt *Runtime) submitGraphLocked(doc *GraphDocument) error {
	graphID := ""
	if doc != nil {
		graphID = doc.GraphID
	}
	// Session 归属盖章：仅填充空值，不覆盖显式归属（如调用方已声明
	// session_id 的场景）；provider 为 nil 时保持空串（行为同今）。
	if doc != nil && doc.SessionID == "" && rt.sessionIDProvider != nil {
		doc.SessionID = rt.sessionIDProvider()
	}
	if err := rt.validateSingleTopLevelGraphLocked(doc); err != nil {
		trace.Emit(trace.Event{Kind: trace.KindGraphSubmissionRejected, GraphID: graphID, Error: err.Error()})
		return err
	}
	if err := rt.store.SubmitGraph(doc); err != nil {
		trace.Emit(trace.Event{Kind: trace.KindGraphSubmissionRejected, GraphID: graphID, Error: err.Error()})
		return err
	}
	stored, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	digest, _ := rt.store.Digest(graphID)
	trace.Emit(trace.Event{
		Kind:        trace.KindGraphSubmitted,
		GraphID:     graphID,
		Description: fmt.Sprintf("revision=%d digest=%s", stored.Revision, truncateDigest(digest)),
	})
	if stored.Status == GraphPending {
		if err := rt.store.SetGraphStatus(graphID, GraphRunning, stored.StateVersion); err != nil {
			return err
		}
	}
	return rt.activateLocked(graphID, stored.Root, nil)
}

func (rt *Runtime) validateSingleTopLevelGraphLocked(doc *GraphDocument) error {
	if rt == nil || rt.store == nil || doc == nil || doc.RunID == "" || rt.runtimeGraphIsChildLocked(doc.GraphID) {
		return nil
	}
	for _, candidate := range rt.store.List() {
		if candidate.GraphID == doc.GraphID || rt.runtimeGraphIsChildLocked(candidate.GraphID) {
			continue
		}
		existing, ok := rt.store.Get(candidate.GraphID)
		if ok && existing.RunID != "" && existing.RunID == doc.RunID {
			return fmt.Errorf("graph: Run %s 已存在顶层业务 Graph %s，禁止启动第二个顶层 Graph %s；恢复必须使用 GraphChangeProposal",
				doc.RunID, existing.GraphID, doc.GraphID)
		}
	}
	return nil
}

// OnTaskTerminal 以任务终态事实驱动转移求值（V6 §6-15/17）。
//
// 处理顺序：校验事实对应当前在途 activation（过期 activation 的迟到事件
// 忽略并记 debug）→ durable 写 result_ref 与节点终态 → 按 next 顺序求值
// （先查幂等记录再 RecordTransition，durable 后激活目标）→ 无匹配出路时
// 图置 failed（带原因）。
func (rt *Runtime) OnTaskTerminal(f TerminalFact) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.synchronousSteps = 0
	doc, err := rt.graph(f.GraphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		log.Printf("[graph] DEBUG 图 %s 已是终态 %q，忽略迟到的终态事实（节点 %s activation %s）",
			f.GraphID, doc.Status, f.NodeID, f.ActivationID)
		return nil
	}
	if rt.isSuspendedLocked(f.GraphID) {
		// 停驻闸门：吞掉不推进。任务终态在公告板上有权威事实，解冻恢复时
		// 经 reconcileTaskLocked 对账回填（与 graph-terminal-feed 同语义）。
		log.Printf("[graph] DEBUG 图 %s 已停驻，吞掉终态事实（节点 %s activation %s，状态 %s）；解冻时经公告板对账回填",
			f.GraphID, f.NodeID, f.ActivationID, f.Status)
		return nil
	}
	node, ok := doc.Nodes[f.NodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, f.GraphID, f.NodeID)
	}
	ex := node.Execution
	if ex == nil || ex.ActivationID == "" || ex.ActivationID != f.ActivationID {
		log.Printf("[graph] DEBUG 图 %s 节点 %s 忽略过期 activation %q 的终态事实（当前 activation %q）",
			f.GraphID, f.NodeID, f.ActivationID, activationOf(node))
		return nil
	}
	if node.Status != NodeRunning {
		log.Printf("[graph] DEBUG 图 %s 节点 %s 状态 %q 非 running，忽略重复/迟到的终态事实（activation %s）",
			f.GraphID, f.NodeID, node.Status, f.ActivationID)
		return nil
	}
	if ex.TaskID != "" && f.TaskID != "" && ex.TaskID != f.TaskID {
		log.Printf("[graph] DEBUG 图 %s 节点 %s 终态事实的 task_id %q 与在途 %q 不符，忽略",
			f.GraphID, f.NodeID, f.TaskID, ex.TaskID)
		return nil
	}
	activeNode := nodeForExecution(node, *ex)
	switch f.Status {
	case NodeCompleted, NodeFailed, NodeBlocked:
	default:
		return fmt.Errorf("graph: 终态事实的节点状态 %q 非法（仅接受 completed/failed/blocked）", f.Status)
	}

	// 数据流证据：终态任务的可观察证据随 Execution 持久化（同一本
	// execution 经 settle 落盘，证据与终态同条 journal 记录生效）。
	exec := *ex
	hydrateExecutionEvidence(&exec)
	if len(f.Evidence) > 0 {
		exec.Evidence = appendEvidenceUnique(exec.Evidence, f.Evidence...)
		hydrateExecutionEvidence(&exec)
	}
	if f.Status == NodeCompleted && activeNode.ProgressContractRef == "progress:code-change/v5" {
		contract := &fulfillment.Contract{RequireWorkspaceChange: true, RequiredCheckIDs: []string{"verification"}}
		if f.Fulfillment == nil || f.Fulfillment.Validate(contract) != nil {
			reason := "contract_fulfillment_missing：mutating activation 缺少真实 workspace change 或晚于最后改动的 verification check"
			result := make(map[string]any, len(f.Result)+3)
			for key, value := range f.Result {
				result[key] = value
			}
			result["status"] = string(NodeBlocked)
			result["reason_code"] = "contract_fulfillment_missing"
			result["blocked_reason"] = reason
			return rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeBlocked, result)
		}
	}

	// acceptance 节点 completed 终态一律走引擎内生谱系核验（acceptance.go
	// 判定矩阵：引用越谱系即 disputed 判死，不引用不判死）。failed/blocked
	// 终态无 verdict 可采信，不核验。
	terminalResult := f.Result
	if activeNode.Kind == KindAcceptance {
		if f.Status == NodeCompleted {
			return rt.settleAcceptanceLocked(f, exec)
		}
		// failed/blocked 的路由权威是任务状态，不是 Agent 在
		// Result 中自填的业务结论。剔除 verdict/event 后，边求值
		// 才会稳定回落到 Runtime 产生的 failed/blocked 事件。
		terminalResult = make(map[string]any, len(f.Result))
		for key, value := range f.Result {
			if key != "verdict" && key != "event" {
				terminalResult[key] = value
			}
		}
	}

	// durable 写 Result 摘要 + 节点终态，随后走统一结算路径（转移求值 +
	// 目标激活 + join 就绪重推导）。
	return rt.settleNodeLocked(f.GraphID, f.NodeID, exec, f.Status, terminalResult)
}

// OnExternalEvent 投递一个外部事件：匹配该图中 status=waiting 且
// wait.event 相同的 wait_event 节点（可多个），以事件 data 为 Result 完成
// 节点并走转移求值；不匹配的事件忽略（返回 nil）。同一 activation 只
// resume 一次——重复投递时节点已离开 waiting，自然幂等。
//
// data 原样成为节点 Result：含 "event" 键时驱动下游事件形态转移条件，
// 否则下游按 completed 终态回落。
func (rt *Runtime) OnExternalEvent(graphID, event string, data map[string]any) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.synchronousSteps = 0
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		log.Printf("[graph] DEBUG 图 %s 已是终态 %q，忽略外部事件 %q", graphID, doc.Status, event)
		return nil
	}
	if rt.isSuspendedLocked(graphID) {
		// 停驻闸门：外部事件无 durable 面对账，吞掉即视为冻结期间未发生
		//（与进程停止期间的事件语义一致——事件是时点信号，不排队）。
		log.Printf("[graph] DEBUG 图 %s 已停驻，吞掉外部事件 %q（视为冻结期间未发生）", graphID, event)
		return nil
	}
	if strings.TrimSpace(event) == "" {
		return nil // 空事件名不匹配任何等待
	}
	var errs []error
	for _, id := range sortedNodeIDs(doc) {
		n := doc.Nodes[id]
		if n.Execution == nil {
			continue
		}
		activeNode := nodeForExecution(n, *n.Execution)
		if activeNode.Kind != KindWaitEvent || n.Status != NodeWaiting || activeNode.Wait == nil || activeNode.Wait.Event != event {
			continue
		}
		exec := *n.Execution
		rt.cancelWaitTimerLocked(graphID, exec.ActivationID)
		// waiting → running → completed（节点状态机不允许 waiting 直接到
		// completed；两条记录间的崩溃窗口由 ResumeGraph 退回 waiting 兜底）。
		if err := rt.writeNode(graphID, id, exec, NodeRunning); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := rt.writeTerminalLocked(graphID, id, exec, NodeCompleted, data); err != nil {
			errs = append(errs, err)
			continue
		}
		trace.Emit(trace.Event{
			Kind:         trace.KindGraphWaitResumed,
			GraphID:      graphID,
			NodeID:       id,
			ActivationID: exec.ActivationID,
			Description:  "event=" + event,
		})
		errs = append(errs, rt.evalTransitionsLocked(graphID, id, exec.ActivationID, NodeCompleted, data))
	}
	return errors.Join(errs...)
}

// OnApprovalDecided 投递一个审批裁决：匹配 (graphID, nodeID, activationID)
// 在途的 waiting approval 节点，以 Result={event: approved|rejected, text}
// 完成节点（配合下游 when event approved/rejected 条件）并走转移求值。
// 过期 activation 或节点已离开 waiting 的重复/迟到裁决忽略（返回 nil）。
//
// 停驻闸门：图已停驻时不推进，裁决暂存 pendingApprovals（审批裁决无
// durable 面可重建——恢复侧因 RequestID 已记录不重发请求，直接吞掉会让
// waiting approval 节点永久悬挂），解冻时经 ResumeGraphsForSession 回放。
func (rt *Runtime) OnApprovalDecided(graphID, nodeID, activationID string, approved bool, text string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.synchronousSteps = 0
	if rt.isSuspendedLocked(graphID) {
		rt.pendingApprovals[approvalKeyOf(graphID, nodeID, activationID)] = approvalDecision{
			graphID: graphID, nodeID: nodeID, activationID: activationID, approved: approved, text: text,
		}
		log.Printf("[graph] DEBUG 图 %s 已停驻，暂存审批裁决（节点 %s activation %s），解冻后回放",
			graphID, nodeID, activationID)
		return nil
	}
	return rt.onApprovalDecidedLocked(graphID, nodeID, activationID, approved, text)
}

// onApprovalDecidedLocked 是 OnApprovalDecided 的锁内实现（调用方须持
// rt.mu）；解冻回放复用本路径（activation/状态守卫天然过滤过期项）。
func (rt *Runtime) onApprovalDecidedLocked(graphID, nodeID, activationID string, approved bool, text string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		log.Printf("[graph] DEBUG 图 %s 已是终态 %q，忽略迟到的审批裁决（节点 %s activation %s）",
			graphID, doc.Status, nodeID, activationID)
		return nil
	}
	node, ok := doc.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
	}
	ex := node.Execution
	if ex == nil || ex.ActivationID == "" || ex.ActivationID != activationID {
		log.Printf("[graph] DEBUG 图 %s 节点 %s 忽略过期 activation %q 的审批裁决（当前 activation %q）",
			graphID, nodeID, activationID, activationOf(node))
		return nil
	}
	activeNode := nodeForExecution(node, *ex)
	if activeNode.Kind != KindApproval || node.Status != NodeWaiting {
		log.Printf("[graph] DEBUG 图 %s 节点 %s（%s）状态 %q 非 approval 等待，忽略重复/迟到的裁决（activation %s）",
			graphID, nodeID, node.Kind, node.Status, activationID)
		return nil
	}
	event := EventApproved
	if !approved {
		event = EventRejected
	}
	result := map[string]any{"event": event, "text": text}
	exec := *ex
	if err := rt.writeNode(graphID, nodeID, exec, NodeRunning); err != nil {
		return err
	}
	if err := rt.writeTerminalLocked(graphID, nodeID, exec, NodeCompleted, result); err != nil {
		return err
	}
	trace.Emit(trace.Event{
		Kind:         trace.KindGraphApprovalDecided,
		GraphID:      graphID,
		NodeID:       nodeID,
		ActivationID: activationID,
		Description:  event,
	})
	return rt.evalTransitionsLocked(graphID, nodeID, activationID, NodeCompleted, result)
}

// ============================================================
// 统一结算路径（调用方须持 rt.mu）
// ============================================================

// settleNodeLocked 是「节点获得终态」的统一结算：durable 写 Result 摘要 +
// 节点终态（单条 execution_status 记录），随后求值转移并激活目标，
// 最后重推导全图 join 就绪性。OnTaskTerminal 与 wait_event / approval /
// subgraph / tool 的内部终态共用此路径。调用方负责保证 exec 是当前在途
// activation 且节点状态机允许到达 status。
func (rt *Runtime) settleNodeLocked(graphID, nodeID string, exec Execution, status NodeStatus, result map[string]any) error {
	if err := rt.writeTerminalLocked(graphID, nodeID, exec, status, result); err != nil {
		return err
	}
	return rt.evalTransitionsLocked(graphID, nodeID, exec.ActivationID, status, result)
}

// writeTerminalLocked durable 写完整 Result、节点终态与「继续选择转移」标记。
// 终态与 Settlement 同条 journal 记录生效，恢复不能依赖展示用 ResultRef 猜条件。
func (rt *Runtime) writeTerminalLocked(graphID, nodeID string, exec Execution, status NodeStatus, result map[string]any) error {
	return rt.writeTerminalContinuationLocked(graphID, nodeID, exec, status, result, SettlementContinueTransitions, "")
}

// writeTerminalContinuationLocked 是所有「节点已终态、Graph 尚需续跑」路径的
// 唯一落盘入口。Settlement 永久保留；重启后的重复续跑由 TransitionRecord 与
// Graph 终态幂等，不另写 cleared 标记，避免制造新的两记录崩溃窗口。
func (rt *Runtime) writeTerminalContinuationLocked(graphID, nodeID string, exec Execution, status NodeStatus, result map[string]any, continuation SettlementContinuation, reason string) error {
	return rt.writeTerminalContinuationWithOutcomeLocked(graphID, nodeID, exec, status,
		result, continuation, "", reason)
}

func (rt *Runtime) writeTerminalContinuationWithOutcomeLocked(graphID, nodeID string,
	exec Execution, status NodeStatus, result map[string]any, continuation SettlementContinuation,
	outcome EndOutcome, reason string) error {
	if result == nil {
		result = map[string]any{}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return rt.failUnstorableTerminalResultLocked(graphID, nodeID, exec, status,
			fmt.Sprintf("终态 Result 无法序列化为 JSON object: %v", err))
	}
	if len(raw) > MaxDocumentBytes {
		return rt.failUnstorableTerminalResultLocked(graphID, nodeID, exec, status,
			fmt.Sprintf("终态 Result 为 %d 字节，超过 durable activation Result 上限 %d", len(raw), MaxDocumentBytes))
	}
	// 经 JSON 往返得到与 durable 字节一致、且不再受调用方后续修改影响的缓存。
	var durableResult map[string]any
	if err := json.Unmarshal(raw, &durableResult); err != nil || durableResult == nil {
		return rt.failUnstorableTerminalResultLocked(graphID, nodeID, exec, status,
			"终态 Result 不是 JSON object")
	}
	resultRecord := ActivationResult{
		Ref: activationResultRef(graphID, exec.ActivationID), NodeID: nodeID,
		ActivationID: exec.ActivationID, Result: append(json.RawMessage(nil), raw...),
		Evidence: append([]EvidenceEntry(nil), exec.Evidence...),
	}
	// Result Store 必须先于 terminal/transition durable：一旦随后边记录引用
	// ResultRef，重启与 snapshot 压缩后都必须已经可解引用。两记录间崩溃只
	// 留下一条无害的孤立 Result；任务公告板对账会幂等补写 terminal。
	if err := rt.store.RecordActivationResult(graphID, resultRecord); err != nil {
		return err
	}
	exec.ResultRef = resultRecord.Ref
	exec.ResultSummary = summarizeResult(durableResult)
	exec.Settlement = &TerminalSettlement{
		Status: status, ResultRef: resultRecord.Ref,
		Continuation: continuation, Outcome: outcome, Reason: reason,
	}
	if err := rt.writeNode(graphID, nodeID, exec, status); err != nil {
		return err
	}
	if rt.results[graphID] == nil {
		rt.results[graphID] = make(map[string]map[string]any)
	}
	rt.results[graphID][nodeID] = durableResult
	return nil
}

// failUnstorableTerminalResultLocked 把无法持久化的业务 Result 转换为有界、
// 可恢复的系统错误 Result，再将当前节点与 Graph durable fail。不能只返回
// marshal/size 错误：调用方已经处于 running，裸返回会制造永久僵尸节点。
func (rt *Runtime) failUnstorableTerminalResultLocked(graphID, nodeID string, exec Execution, originalStatus NodeStatus, detail string) error {
	reason := fmt.Sprintf("图 %s 节点 %s activation %s 的 %s；已拒绝业务终态 %s 并执行 fail-closed",
		graphID, nodeID, exec.ActivationID, detail, originalStatus)
	fallback := map[string]any{
		"error": reason, "original_status": string(originalStatus),
		"verify_status": "result_unstorable",
	}
	raw, _ := json.Marshal(fallback) // 固定小对象，序列化不可失败。
	ref := activationResultRef(graphID, exec.ActivationID)
	if existing, ok := rt.store.ResolveActivationResult(graphID, ref); ok {
		// 极窄崩溃窗口兼容：Result 已先落盘但 terminal 尚未写时，不改写不可变
		// 记录；节点失败原因仍由 Settlement.Reason 与 ResultSummary 持久化。
		ref = existing.Ref
	} else if err := rt.store.RecordActivationResult(graphID, ActivationResult{
		Ref: ref, NodeID: nodeID, ActivationID: exec.ActivationID, Result: raw,
		Evidence: append([]EvidenceEntry(nil), exec.Evidence...),
	}); err != nil {
		return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s；错误 Result 落盘失败: %w", reason, err))
	}
	exec.ResultRef = ref
	exec.ResultSummary = summarizeResult(fallback)
	exec.Settlement = &TerminalSettlement{
		Status: NodeFailed, ResultRef: ref,
		Continuation: SettlementContinueGraphFail, Reason: reason,
	}
	writeErr := rt.writeNode(graphID, nodeID, exec, NodeFailed)
	if rt.results[graphID] == nil {
		rt.results[graphID] = make(map[string]map[string]any)
	}
	rt.results[graphID][nodeID] = fallback
	return errors.Join(writeErr, rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
}

// evalTransitionsLocked 按节点 next 顺序求值转移（V6 §6-17）：
// transition.activation 只有 "new" 一种取值（缺省即新 activation），无须
// 分支；同一 (source_activation, transition_id) 已生效过的边跳过——重入/
// 重放幂等。无匹配出路时图置 failed（带原因）；已 durable 的转移逐一激活
// 目标后，重推导全图 join 就绪性（join 不挂在任何单条转移上， readiness
// 由节点状态 + 边选择记录纯推导）。
func (rt *Runtime) evalTransitionsLocked(graphID, nodeID, activationID string, status NodeStatus, result map[string]any) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
	}
	if node.Execution != nil && node.Execution.ActivationID == activationID {
		node = nodeForExecution(node, *node.Execution)
	}
	if err := rt.validateRecoveryRetryContract(graphID, doc, node, activationID, status, result); err != nil {
		return errors.Join(rt.failGraph(graphID, err.Error()), err)
	}
	// 数据流绑定：本 activation 的完整证据与稳定 ResultRef 随每条生效边
	// 进入 EdgeInput。老 journal/测试可能直接构造 Settlement 而未先写
	// ActivationResult，此处在任何 transition 前做幂等补写。
	var srcEvidence []EvidenceEntry
	srcTaskID := ""
	if node.Execution != nil && node.Execution.ActivationID == activationID {
		srcEvidence = node.Execution.Evidence
		srcTaskID = node.Execution.TaskID
	}
	// 上游工作记录（2026-08-21）：按来源 Task 聚合渲染一次，随每条生效边
	// 冻结进 EdgeInput——同一 activation 的多条边共享同一文本；无 provider
	// 或来源无 Task（内建节点/直构测试）时为空。
	workLog := ""
	if rt.workLogProvider != nil && srcTaskID != "" {
		workLog = rt.workLogProvider(srcTaskID)
	}
	resultRef := activationResultRef(graphID, activationID)
	if _, ok := rt.store.ResolveActivationResult(graphID, resultRef); !ok {
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return fmt.Errorf("graph: 序列化图 %s activation %s 的边输入 Result: %w", graphID, activationID, marshalErr)
		}
		if err := rt.store.RecordActivationResult(graphID, ActivationResult{
			Ref: resultRef, NodeID: nodeID, ActivationID: activationID,
			Result: raw, Evidence: append([]EvidenceEntry(nil), srcEvidence...),
		}); err != nil {
			return err
		}
	}
	var targets []TransitionRecord
	everMatched := false
	for i, tr := range node.Next {
		if !evalCondition(tr.When, status, result) {
			continue
		}
		everMatched = true
		if _, fired := rt.store.HasTransition(graphID, activationID, i); fired {
			continue
		}
		rec, err := rt.newTransitionRecordLocked(graphID, nodeID, activationID, i, tr.To, tr.TargetInput, tr.ReplayInputs)
		if err != nil {
			return err
		}
		if rec.ReplayInputs {
			rec.ReplayedInputs, err = rt.recoveryReplayInputs(graphID, rec, result)
			if err != nil {
				return err
			}
		}
		if detail := rt.inputBindingConflictDetail(graphID, rec); detail != "" {
			reason := "数据流输入单赋值冲突：" + detail
			return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
		}
		rec.Input = newEdgeInputWithRef(result, resultRef, srcEvidence)
		rec.Input.WorkLog = workLog
		sv, err := rt.stateVersion(graphID)
		if err != nil {
			return err
		}
		if err := rt.store.RecordTransition(graphID, rec, sv); err != nil {
			if errors.Is(err, ErrTransitionExists) {
				continue
			}
			return err
		}
		trace.Emit(trace.Event{
			Kind:         trace.KindGraphTransitionSelected,
			GraphID:      graphID,
			NodeID:       nodeID,
			ActivationID: activationID,
			Description:  fmt.Sprintf("next[%d] -> %s", i, tr.To),
		})
		targets = append(targets, rec)
	}
	if !everMatched {
		reason := fmt.Sprintf("节点 %s（activation %s）终态 %q 但无任何匹配的出路", nodeID, activationID, status)
		return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
	}

	// 逐一激活目标（转移记录已 durable；此处的崩溃窗口由 ResumeGraph 兜底）。
	var errs []error
	for _, rec := range targets {
		current, err := rt.graph(graphID)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if current.Status.IsTerminal() {
			break // 早到的 end/失败分支已收官，不得在终态 Graph 内再激活兄弟。
		}
		if err := rt.activateRecordedTransitionLocked(graphID, rec, result); err != nil {
			errs = append(errs, err)
		}
	}
	// 结算可能让某些 join 的最后一个入边源进入终态：重推导全图 join 就绪性
	// （覆盖「最后结算的源没有指向 join 的生效边」的就绪与 skipped 两种情形）。
	if current, err := rt.graph(graphID); err != nil {
		errs = append(errs, err)
	} else if !current.Status.IsTerminal() {
		errs = append(errs, rt.evaluateJoinsLocked(graphID))
		errs = append(errs, rt.evaluateAcceptancesLocked(graphID))
	}
	return errors.Join(errs...)
}

func validateRecoveryRetryBudget(node Node, activationID string, status NodeStatus, result map[string]any) error {
	if ControllerRoleOf(node) != ControllerRoleLoopRecovery || status != NodeCompleted || result["decision"] != "retry" {
		return nil
	}
	limit, err := strconv.Atoi(strings.TrimSpace(node.Metadata[MetadataRecoveryMaxRetries]))
	if err != nil || limit <= 0 {
		return fmt.Errorf("graph: loop_recovery activation %s 缺少合法 recovery_max_retries", activationID)
	}
	_, sequence, ok := parseActivationID(activationID)
	if !ok {
		return fmt.Errorf("graph: loop_recovery activation_id=%q 非法", activationID)
	}
	if sequence > limit {
		return fmt.Errorf("graph: loop_recovery retry 预算已耗尽：activation=%s limit=%d；必须提交 decision=blocked",
			activationID, limit)
	}
	return nil
}

// newTransitionRecordLocked 在边选择落盘前冻结它指向的 activation。目标已有
// 在途 activation 时加入该 activation；否则复用尚未物化的 durable 预留，
// 或预留下一个新 ID。TargetActivationID 与边选择同条 journal 记录落盘。
func (rt *Runtime) newTransitionRecordLocked(graphID, sourceNodeID, sourceActivationID string, transitionID int, targetNodeID, targetInput string, replayInputs bool) (TransitionRecord, error) {
	doc, err := rt.graph(graphID)
	if err != nil {
		return TransitionRecord{}, err
	}
	target, ok := doc.Nodes[targetNodeID]
	if !ok {
		return TransitionRecord{}, fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, targetNodeID)
	}
	targetActivationID := ""
	if target.Execution != nil {
		switch target.Status {
		case NodeReady, NodeRunning, NodeWaiting:
			targetActivationID = target.Execution.ActivationID
		}
	}
	if targetActivationID == "" {
		targetActivationID = rt.pendingTargetActivationID(graphID, targetNodeID, target)
	}
	if targetActivationID == "" {
		targetActivationID, err = rt.store.NextActivationID(graphID, targetNodeID)
		if err != nil {
			return TransitionRecord{}, err
		}
	}
	return TransitionRecord{
		SourceNodeID:       sourceNodeID,
		SourceActivationID: sourceActivationID,
		TransitionID:       transitionID,
		TargetNodeID:       targetNodeID,
		TargetInput:        targetInput,
		ReplayInputs:       replayInputs,
		TargetActivationID: targetActivationID,
	}, nil
}

// inputBindingConflictDetail 防御绕过 authoring 校验进入的旧图/损坏记录。
// 普通节点的 activation 只能冻结一份输入；join/acceptance 的每个逻辑端口
// 只能绑定一次。没有 generation/correlation token 时，不同静态生产者也
// 不得在不同 activation 轮流占用同一普通输入/端口，否则迟到边会被误认成
// 下一轮数据。
func (rt *Runtime) inputBindingConflictDetail(graphID string, candidate TransitionRecord) string {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err.Error()
	}
	target, ok := doc.Nodes[candidate.TargetNodeID]
	if !ok {
		return fmt.Sprintf("目标节点 %s 不存在", candidate.TargetNodeID)
	}
	if target.Execution != nil {
		target = nodeForExecution(target, *target.Execution)
	}
	barrier := target.Kind == KindJoin || target.Kind == KindAcceptance
	logicalPort := candidate.TargetInput
	if barrier && logicalPort == "" {
		logicalPort = candidate.SourceNodeID // 旧无端口 barrier：每个来源视作独立端口
	}
	for _, existing := range rt.store.Transitions(graphID) {
		if existing.TargetNodeID != candidate.TargetNodeID ||
			(existing.SourceActivationID == candidate.SourceActivationID && existing.TransitionID == candidate.TransitionID) {
			continue
		}
		if barrier {
			existingPort := existing.TargetInput
			if existingPort == "" {
				existingPort = existing.SourceNodeID
			}
			if existingPort != logicalPort {
				continue
			}
			if existing.TargetActivationID == candidate.TargetActivationID {
				return fmt.Sprintf("barrier %s activation %s 的端口 %q 已由 %s/%s 绑定，不能再由 %s/%s 写入",
					candidate.TargetNodeID, candidate.TargetActivationID, logicalPort,
					existing.SourceNodeID, existing.SourceActivationID, candidate.SourceNodeID, candidate.SourceActivationID)
			}
			if existing.SourceNodeID != candidate.SourceNodeID || existing.TransitionID != candidate.TransitionID {
				return fmt.Sprintf("barrier %s 的端口 %q 曾由生产边 %s.next[%d] 写入；缺少 generation/correlation token 时不能改由 %s.next[%d] 写入下一 activation",
					candidate.TargetNodeID, logicalPort, existing.SourceNodeID, existing.TransitionID, candidate.SourceNodeID, candidate.TransitionID)
			}
			continue
		}
		if existing.TargetActivationID == candidate.TargetActivationID {
			return fmt.Sprintf("普通节点 %s activation %s 已冻结来自 %s/%s 的输入，不能再绑定 %s/%s",
				candidate.TargetNodeID, candidate.TargetActivationID,
				existing.SourceNodeID, existing.SourceActivationID, candidate.SourceNodeID, candidate.SourceActivationID)
		}
		if existing.SourceNodeID != candidate.SourceNodeID || existing.TransitionID != candidate.TransitionID {
			return fmt.Sprintf("普通节点 %s 曾由生产边 %s.next[%d] 激活；缺少 generation/correlation token 时不能把生产边切换为 %s.next[%d]",
				candidate.TargetNodeID, existing.SourceNodeID, existing.TransitionID, candidate.SourceNodeID, candidate.TransitionID)
		}
	}
	return ""
}

// pendingTargetActivationID 返回 durable transition 已预留、但目标节点尚未
// 物化的最早 activation。join 的多条入边会共享同一预留；回边恢复也借此
// 区分“上一 activation 已终态”和“新 activation 尚未创建”。
func (rt *Runtime) pendingTargetActivationID(graphID, nodeID string, node Node) string {
	current := 0
	if node.Execution != nil {
		if owner, n, ok := parseActivationID(node.Execution.ActivationID); ok && owner == nodeID {
			current = n
		}
	}
	bestN := 0
	bestID := ""
	for _, rec := range rt.store.Transitions(graphID) {
		if rec.TargetNodeID != nodeID || rec.TargetActivationID == "" {
			continue
		}
		owner, n, ok := parseActivationID(rec.TargetActivationID)
		if !ok || owner != nodeID || n <= current {
			continue
		}
		if bestN == 0 || n < bestN {
			bestN, bestID = n, rec.TargetActivationID
		}
	}
	return bestID
}

// activateRecordedTransitionLocked 补完一条已 durable 边的目标 activation。
// 已物化到相同或更新 activation 时幂等跳过；旧 activation 仍终态时按记录
// 中的 TargetActivationID 创建新 activation，而不是依赖 status=inactive。
func (rt *Runtime) activateRecordedTransitionLocked(graphID string, rec TransitionRecord, input map[string]any) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[rec.TargetNodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s（边来自 %s）", ErrNodeNotFound, graphID, rec.TargetNodeID, rec.SourceActivationID)
	}
	if rec.TargetActivationID == "" {
		// 兼容早期 journal：没有目标 activation 身份，无法区分“回边尚未
		// 激活”和“该边已创建且目标已终态”。保留旧的安全规则，只补从未
		// 激活的 inactive 目标，绝不因恢复猜测而重开 terminal activation。
		if node.Status != NodeInactive {
			return nil
		}
		return rt.activateLocked(graphID, rec.TargetNodeID, input)
	}
	if node.Execution != nil && node.Execution.ActivationID != "" {
		_, currentN, currentOK := parseActivationID(node.Execution.ActivationID)
		_, targetN, targetOK := parseActivationID(rec.TargetActivationID)
		if node.Execution.ActivationID == rec.TargetActivationID || (currentOK && targetOK && currentN > targetN) {
			return nil
		}
		switch node.Status {
		case NodeReady, NodeRunning, NodeWaiting:
			return fmt.Errorf("graph: 图 %s 节点 %s 已有在途 activation %s，不能补激活边记录目标 %s",
				graphID, rec.TargetNodeID, node.Execution.ActivationID, rec.TargetActivationID)
		}
	}
	// 仅在确实需要补物化目标时解引用；已物化目标幂等跳过，不因历史边缺少
	// 新 ResultRef 而误判。新记录优先走 activation Result Store，大结果和
	// 回边覆盖旧 Execution 后仍能精确恢复；旧记录只接受完整内联 Result。
	if input == nil {
		input, err = rt.resolveTransitionResult(graphID, rec)
		if err != nil {
			reason := fmt.Sprintf("补激活边 %s[%d] -> %s 时输入不可恢复: %v",
				rec.SourceActivationID, rec.TransitionID, rec.TargetNodeID, err)
			return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
		}
	}
	return rt.activateLockedWithReplay(graphID, rec.TargetNodeID, input, rec.ReplayedInputs)
}

func (rt *Runtime) recoveryReplayInputs(graphID string, rec TransitionRecord, result map[string]any) ([]InputBinding, error) {
	doc, err := rt.graph(graphID)
	if err != nil {
		return nil, err
	}
	recovery, ok := doc.Nodes[rec.SourceNodeID]
	if !ok || recovery.Execution == nil || recovery.Execution.ActivationID != rec.SourceActivationID ||
		ControllerRoleOf(nodeForExecution(recovery, *recovery.Execution)) != ControllerRoleLoopRecovery {
		return nil, fmt.Errorf("graph: replay_inputs 来源 %s/%s 不是当前 loop_recovery activation",
			rec.SourceNodeID, rec.SourceActivationID)
	}
	var failure InputBinding
	found := false
	for _, input := range recovery.Execution.Input {
		if input.TargetInput != "failure_context" {
			continue
		}
		if found {
			return nil, fmt.Errorf("graph: loop_recovery activation %s 存在多个 failure_context", rec.SourceActivationID)
		}
		failure, found = input, true
	}
	if !found || failure.SourceNodeID != rec.TargetNodeID {
		return nil, fmt.Errorf("graph: loop_recovery retry 目标 %s 与 failure_context 来源 %s 不一致",
			rec.TargetNodeID, failure.SourceNodeID)
	}
	source, ok := doc.Nodes[failure.SourceNodeID]
	if !ok || source.Execution == nil || source.Execution.ActivationID != failure.SourceActivationID {
		return nil, fmt.Errorf("graph: recovery source activation %s 不再可解引用", failure.SourceActivationID)
	}
	replayed := cloneInputBindings(source.Execution.Input)
	activeRecovery := nodeForExecution(recovery, *recovery.Execution)
	if strings.TrimSpace(activeRecovery.Metadata[MetadataRecoveryDeltaSchema]) == RecoveryDeltaSchemaV1 {
		delta, err := decodeRecoveryDelta(result)
		if err != nil {
			return nil, err
		}
		if _, definitionChanged := stringSet(delta.ChangedDimensions)["definition"]; definitionChanged {
			return replayed, nil
		}
		raw, err := json.Marshal(delta)
		if err != nil {
			return nil, fmt.Errorf("graph: 编码 recovery_directive: %w", err)
		}
		replayed = append(replayed, InputBinding{
			SourceNodeID: rec.SourceNodeID, SourceActivationID: rec.SourceActivationID,
			TargetInput: "recovery_directive",
			Summary: fmt.Sprintf("strategy=%s first_action=%s milestone=%s",
				delta.Strategy, delta.FirstRequiredAction, delta.ExpectedMilestone),
			Result: raw,
		})
	}
	return replayed, nil
}

// ResumeGraph 在进程重启后恢复一张图的执行（Store.Recover 完成后调用）：
//   - status=running 且带 execution.activation_id 的 controller/agent/acceptance
//     节点先与 TaskBoard 对账：在途任务只校准 task_id，任务缺失则按原 activation
//     幂等补发，任务已终态则把公告板事实反向补结算 Graph；
//   - ready 但未发任务的节点按 durable activation 重新发任务（TaskBoard
//     以 (graph_id, activation_id) 幂等去重，绝不重开新 activation）；
//   - router/end 属同步节点，ready/running 说明死在求值中途，幂等重执行
//     （已生效的边凭 RecordTransition 跳过，绝不重号）；
//   - running 的 tool 节点（executing 中断）语义为 unknown：置 failed
//     （进程重启时工具执行状态未知，不自动重跑副作用），交给转移/人工裁决；
//   - running 的 wait_event/approval/subgraph 节点（死在 waiting→终态的
//     结算中途）退回 waiting 重新等待；
//   - waiting 的 wait_event 节点保持等待（不重发事件）；waiting 的 approval
//     节点已记录 requestID 的不重复请求，未记录的补发（网关幂等键去重）；
//   - waiting 的 subgraph 父节点按 durable 记录处理子图：子图缺失则幂等
//     补物化，子图在途则递归 ResumeGraph，子图已终态则直接结算父节点；
//   - join 就绪性按节点状态 + Transitions 重推导（含 C3 遗留的 waiting join）；
//   - 已记录转移但目标 activation 尚未物化（两记录间的崩溃窗口）按记录
//     冻结的 TargetActivationID 补激活；旧 journal 无该字段时仅补 inactive。
//     生效边 durable 绑定的内联 Result 随补激活重建（EdgeInput），router
//     目标可精确重放条件；仅历史记录无内联或超大截断时按 always/已记录
//     边兜底。
//
// ResumeGraph 恢复一张图（幂等；进程重启后由 bootstrap 逐张调用）。
// 已停驻的图忽略——停驻图的解冻必须走 ResumeGraphsForSession（解除闸门 +
// 对账 + 回放审批裁决的整体动作），单独恢复会在闸门仍在时补发任务。
func (rt *Runtime) ResumeGraph(graphID string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.synchronousSteps = 0
	if rt.isSuspendedLocked(graphID) {
		log.Printf("[graph] DEBUG 图 %s 已停驻，忽略单独恢复（解冻须走 ResumeGraphsForSession）", graphID)
		return nil
	}
	return rt.resumeGraphLocked(graphID)
}

// CancelGraphTree durably cancels graphID and every materialized descendant
// subgraph. It is idempotent: terminal graphs keep their existing outcome, but
// are still traversed so an older/incomplete snapshot with a terminal ancestor
// cannot resume a live descendant after restart.
//
// Descendants are cancelled post-order and do not settle their parent
// subgraph nodes: the whole tree is already being torn down, so feeding a
// synthetic failed result upward could select transitions while cancellation
// is in progress. If the requested root itself newly becomes cancelled, its
// enclosing (non-cancelled) parent is notified exactly once through the normal
// subgraph completion guard.
func (rt *Runtime) CancelGraphTree(graphID, reason string) error {
	graphID = strings.TrimSpace(graphID)
	if graphID == "" {
		return fmt.Errorf("graph: graph_id 不能为空")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Graph cancelled by control plane"
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	_, cleanupErr, err := rt.cancelGraphTreeLocked(graphID, reason, true, make(map[string]struct{}))
	return errors.Join(cleanupErr, err)
}

// TerminateAll 取消 Store 中全部非终态图（含各自的物化子图闭包），返回本次
// 由在途迁移为 cancelled 的 graph ID 列表（按 ID 排序，结果确定）。供
// 「/new force」等控制面一次性终止当前 session 全部图运行时使用。
//
// 逐图复用 cancelGraphTreeLocked 的完整收尾语义：子图树后序取消且不向上
// 结算父节点、未收官节点 durable 取消、wait timer 停走、公告板任务尽力
// 清理，每张新终结的图照常发 graph_ended 事件（下游 team.Manager 靠它回收
// team），reason 经事件 Reason 字段与节点取消结果记录。notifyParent 恒为
// false：全部在途图都在拆除中，子图取消若向上结算父节点，合成结果可能在
// 拆除进行中选中转移。
//
// 幂等：快照时已终态的图跳过（保留既有终态结局，不重复发事件），连续调用
// 第二次返回空。seen 集跨树共享：顶层图取消时其物化子图已在同一棵树的后序
// 遍历中取消并登记，主循环扫到该子图 ID 时直接短路，不会重复清理公告板或
// 重复发事件。单图失败只记日志、不中断其余图的终止（控制面 best-effort；
// 公告板清理失败已由 terminateGraphTasksLocked 记日志，启动恢复时重试）。
func (rt *Runtime) TerminateAll(reason string) []string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "All Graphs terminated by control plane"
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()

	// live 记录快照时在途的图：返回列表按「快照在途 → 本次 cancelled」的
	// 真实迁移收集，树遍历中连带取消的物化子图同样计入。顶层图按 ID 排序后
	// 恒先于其子图处理（子图 ID 以父图 ID 为前缀），父树取消后子图经 seen
	// 短路，主循环的调用顺序不影响结果。
	snapshot := rt.store.List()
	live := make(map[string]struct{}, len(snapshot))
	for _, summary := range snapshot {
		if !summary.Status.IsTerminal() {
			live[summary.GraphID] = struct{}{}
		}
	}

	seen := make(map[string]struct{}, len(snapshot))
	for _, summary := range snapshot {
		if _, ok := live[summary.GraphID]; !ok {
			continue
		}
		if _, _, err := rt.cancelGraphTreeLocked(summary.GraphID, reason, false, seen); err != nil {
			log.Printf("[graph] ERROR: TerminateAll 终止图 %s 失败（继续终止其余图）: %v", summary.GraphID, err)
		}
	}

	terminated := make([]string, 0, len(live))
	for graphID := range live {
		if doc, ok := rt.store.Get(graphID); ok && doc.Status == GraphCancelled {
			terminated = append(terminated, graphID)
		}
	}
	sort.Strings(terminated)
	return terminated
}

// GraphsForSession 返回归属于 sessionID 的图 ID 列表（按 ID 排序，结果
// 确定；Store.List 已按 graph_id 排序）。空串匹配无归属的历史图。
//
// 纯内存查询：Store.List 自带并发安全，故不持 rt.mu，避免与引擎变更串行
// 争抢。供 bootstrap 在恢复/回填时按 session 归属过滤——本切片只提供
// 查询面，不改任何恢复/回填流程。
func (rt *Runtime) GraphsForSession(sessionID string) []string {
	var out []string
	for _, sum := range rt.store.List() {
		if sum.SessionID == sessionID {
			out = append(out, sum.GraphID)
		}
	}
	return out
}

// resumeGraphLocked 是 ResumeGraph 的锁内实现（调用方须持 rt.mu）；
// subgraph 父节点恢复时递归恢复子图（嵌套深度受 MaxSubgraphDepth 限制，
// 子图 ID 严格延长父图 ID，不存在回环）。
func (rt *Runtime) resumeGraphLocked(graphID string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	if doc.Status == GraphPending {
		if err := rt.store.SetGraphStatus(graphID, GraphRunning, doc.StateVersion); err != nil {
			return err
		}
		doc, err = rt.graph(graphID)
		if err != nil {
			return err
		}
	}
	for _, rec := range rt.store.Transitions(graphID) {
		if detail := rt.inputBindingConflictDetail(graphID, rec); detail != "" {
			reason := "恢复时发现 durable 数据流输入单赋值冲突：" + detail
			return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
		}
	}
	var errs []error

	// root 从未激活（提交与 root 激活之间崩溃）：直接激活。
	if root, ok := doc.Nodes[doc.Root]; ok && root.Execution == nil && root.Status == NodeInactive {
		if err := rt.activateLocked(graphID, doc.Root, nil); err != nil {
			errs = append(errs, err)
		}
	}

	// 节点状态恢复（排序遍历，顺序确定）。
	doc, err = rt.graph(graphID)
	if err != nil {
		return err
	}
	for _, id := range sortedNodeIDs(doc) {
		currentDoc, getErr := rt.graph(graphID)
		if getErr != nil {
			errs = append(errs, getErr)
			break
		}
		if currentDoc.Status.IsTerminal() {
			break
		}
		n := currentDoc.Nodes[id]
		if n.Execution == nil || n.Execution.ActivationID == "" {
			continue
		}
		activeNode := nodeForExecution(n, *n.Execution)
		switch n.Status {
		case NodeRunning:
			switch activeNode.Kind {
			case KindRouter:
				errs = append(errs, rt.activateRouter(graphID, id, nil))
			case KindEnd:
				errs = append(errs, rt.activateEnd(graphID, id, nil))
			case KindTool:
				// executing 中断：副作用是否发生不可知（V6 原则：不自动重跑），
				// 置 failed 交给转移/人工裁决。
				errs = append(errs, rt.settleNodeLocked(graphID, id, *n.Execution, NodeFailed,
					map[string]any{"error": "进程重启时工具执行状态未知（不自动重跑副作用）"}))
			case KindWaitEvent, KindApproval, KindSubgraph:
				// 死在「waiting → running → 终态」的结算中途：退回 waiting 重等。
				if err := rt.writeNode(graphID, id, *n.Execution, NodeWaiting); err != nil {
					errs = append(errs, err)
				} else {
					refreshedDoc, getErr := rt.graph(graphID)
					if getErr != nil {
						errs = append(errs, getErr)
					} else if refreshed, ok := refreshedDoc.Nodes[id]; ok {
						errs = append(errs, rt.resumeWaitingLocked(graphID, id, refreshed))
					}
				}
			case KindController, KindAgent, KindAcceptance:
				errs = append(errs, rt.reconcileRunningTaskLocked(graphID, id, activeNode, *n.Execution))
			}
		case NodeReady:
			switch activeNode.Kind {
			case KindController, KindAgent, KindAcceptance:
				errs = append(errs, rt.reconcileTaskLocked(graphID, id, activeNode, *n.Execution))
			case KindRouter:
				errs = append(errs, rt.activateRouter(graphID, id, nil))
			case KindEnd:
				errs = append(errs, rt.activateEnd(graphID, id, nil))
			case KindTool:
				// 死在 running（executing）落盘之前：executor 从未被调用，安全重入。
				errs = append(errs, rt.activateTool(graphID, id, activeNode))
			case KindWaitEvent, KindApproval, KindSubgraph:
				// 死在挂起/等待进入中途：补完等待（副作用——审批请求、子图
				// 物化——按 execution 记录幂等补发）。
				errs = append(errs, rt.resumeWaitingLocked(graphID, id, activeNode))
			case KindJoin:
				// 死在就绪结算中途：由末尾的全图 join 重推导统一处理。
			default:
				// 未实现类型死在挂起中途：补完挂起（静默，首次激活已报错）。
				if err := rt.writeNode(graphID, id, *n.Execution, NodeWaiting); err != nil {
					errs = append(errs, err)
				}
			}
		case NodeWaiting:
			switch activeNode.Kind {
			case KindApproval, KindSubgraph:
				// 等待型节点的恢复副作用（审批补发 / 子图恢复或结算）。
				errs = append(errs, rt.resumeWaitingLocked(graphID, id, activeNode))
			case KindWaitEvent:
				errs = append(errs, rt.resumeWaitingLocked(graphID, id, activeNode))
			case KindAcceptance:
				// C5c 前挂起遗留（acceptance 曾以「尚未实现」挂起 waiting）：
				// 升级为任务型节点，按 durable activation 幂等补发任务
				// （board 以 (graph_id, activation_id) 去重，绝不重发）。
				errs = append(errs, rt.publishTask(graphID, id, activeNode, *n.Execution))
			default:
				// wait_event 保持等待（不重发事件）；C3 遗留的 waiting join
				// 由末尾的全图重推导处理。
			}
		case NodeCompleted, NodeFailed, NodeBlocked:
			// terminal 状态本身只证明节点结果已 durable；Settlement 还记录了
			// 边选择 / Graph 收官是否需要续跑。标记永久保留，重复恢复由
			// TransitionRecord 与 Graph 终态幂等。
			if n.Execution.Settlement != nil {
				errs = append(errs, rt.resumeTerminalSettlementLocked(graphID, id, *n.Execution))
			}
		}
	}
	if current, getErr := rt.graph(graphID); getErr != nil {
		errs = append(errs, getErr)
	} else if current.Status.IsTerminal() {
		return errors.Join(errs...)
	}

	// 已生效转移按记录绑定的目标 activation 对账。即使目标上一 activation
	// 已 completed/failed，记录指向更新的 ID 时也要补建（回边崩溃窗口）。
	for _, rec := range rt.store.Transitions(graphID) {
		if err := rt.activateRecordedTransitionLocked(graphID, rec, nil); err != nil {
			errs = append(errs, err)
		}
	}

	// join 就绪性按节点状态 + Transitions 重推导（崩溃恢复不改变推导输入）。
	errs = append(errs, rt.evaluateJoinsLocked(graphID))
	// acceptance 的 data-ready 同口径重评估（data-wait 中迟到的入边绑定
	// 可能已在崩溃前 durable）。
	errs = append(errs, rt.evaluateAcceptancesLocked(graphID))
	return errors.Join(errs...)
}

// resumeTerminalSettlementLocked 用 terminal 节点同条 durable 记录中的
// ResultRef 与 continuation 补完崩溃窗口。旧 journal 可回落到内联
// Result；没有 Settlement 时不猜。新记录的所有动作都可幂等重放。
func (rt *Runtime) resumeTerminalSettlementLocked(graphID, nodeID string, exec Execution) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	node, ok := doc.Nodes[nodeID]
	if !ok || node.Execution == nil || node.Execution.ActivationID != exec.ActivationID || node.Execution.Settlement == nil {
		return nil
	}
	settlement := node.Execution.Settlement
	if node.Status != settlement.Status {
		return fmt.Errorf("graph: 图 %s 节点 %s activation %s 的状态 %q 与 durable settlement %q 不一致",
			graphID, nodeID, exec.ActivationID, node.Status, settlement.Status)
	}
	raw := settlement.Result // legacy fallback
	if settlement.ResultRef != "" {
		stored, ok := rt.store.ResolveActivationResult(graphID, settlement.ResultRef)
		if !ok || stored.NodeID != nodeID || stored.ActivationID != exec.ActivationID {
			reason := fmt.Sprintf("图 %s 节点 %s activation %s 的 durable settlement ResultRef %q 不可解引用或来源不一致",
				graphID, nodeID, exec.ActivationID, settlement.ResultRef)
			return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
		}
		raw = stored.Result
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return fmt.Errorf("graph: 解码图 %s 节点 %s activation %s 的 durable settlement Result 失败: %v",
			graphID, nodeID, exec.ActivationID, err)
	}
	if rt.results[graphID] == nil {
		rt.results[graphID] = make(map[string]map[string]any)
	}
	rt.results[graphID][nodeID] = result
	switch settlement.Continuation {
	case SettlementContinueTransitions:
		return rt.evalTransitionsLocked(graphID, nodeID, exec.ActivationID, settlement.Status, result)
	case SettlementContinueGraphComplete:
		return rt.completeGraph(graphID)
	case SettlementContinueGraphOutcome:
		return rt.commitEndOutcome(graphID, nodeID, exec, settlement.Outcome, settlement.Reason)
	case SettlementContinueGraphFail:
		return rt.failGraph(graphID, settlement.Reason)
	case SettlementContinueNone:
		return nil
	default:
		return fmt.Errorf("graph: 图 %s 节点 %s activation %s 的 durable settlement continuation %q 非法",
			graphID, nodeID, exec.ActivationID, settlement.Continuation)
	}
}

// resumeWaitingLocked 补完等待型节点（wait_event/approval/subgraph）的等待
// 进入与恢复副作用：ready 的先进 waiting（durable），waiting 的按 execution
// 记录幂等补发副作用。approval 未注入网关时保持「尚未实现」挂起。
func (rt *Runtime) resumeWaitingLocked(graphID, nodeID string, node Node) error {
	exec := *node.Execution
	node = nodeForExecution(node, exec)
	if node.Kind == KindWaitEvent && node.Wait != nil && node.Wait.TimeoutSec > 0 && exec.WaitDeadline == nil {
		deadline := time.Now().UTC().Add(time.Duration(node.Wait.TimeoutSec) * time.Second)
		exec.WaitDeadline = &deadline
		sv, err := rt.stateVersion(graphID)
		if err != nil {
			return err
		}
		if err := rt.store.SetExecution(graphID, nodeID, exec, sv); err != nil {
			return err
		}
	}
	switch node.Status {
	case NodeReady:
		if err := rt.writeNode(graphID, nodeID, exec, NodeWaiting); err != nil {
			return err
		}
		// ready → waiting 是本轮首次 durable 等待事实：wait_event 补发
		// graph_wait_started（首次激活时未能发出）。
		if node.Kind == KindWaitEvent && node.Wait != nil {
			trace.Emit(trace.Event{
				Kind:         trace.KindGraphWaitStarted,
				GraphID:      graphID,
				NodeID:       nodeID,
				ActivationID: exec.ActivationID,
				Description:  "event=" + node.Wait.Event,
			})
		}
	case NodeWaiting:
	default:
		return nil
	}
	switch node.Kind {
	case KindWaitEvent:
		return rt.scheduleWaitTimeoutLocked(graphID, nodeID, exec)
	case KindApproval:
		if rt.approval == nil || exec.RequestID != "" {
			return nil // 未注入网关保持挂起；已记录 requestID 不重复请求
		}
		return rt.requestApprovalLocked(graphID, nodeID, node, exec)
	case KindSubgraph:
		return rt.ensureSubgraphLocked(graphID, nodeID, node, exec)
	}
	return nil
}

// ============================================================
// 激活路径（调用方须持 rt.mu）
// ============================================================

// activateLocked 进入一个节点：新 activation（inactive 首次进入；终态经回边
// 重进入）。节点已 ready/running/waiting 说明激活事实已 durable（重复进入
// 幂等忽略）。按 kind 分派执行语义。
func (rt *Runtime) activateLocked(graphID, nodeID string, input map[string]any) error {
	return rt.activateLockedWithReplay(graphID, nodeID, input, nil)
}

func (rt *Runtime) activateLockedWithReplay(graphID, nodeID string, input map[string]any, replay []InputBinding) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
	}
	switch node.Status {
	case NodeInactive, NodeCompleted, NodeFailed, NodeBlocked:
		// 新 activation（回边重进入 = 新 activation + 新任务，绝不重开旧的）
	default:
		log.Printf("[graph] DEBUG 图 %s 节点 %s 状态 %q，忽略重复进入", graphID, nodeID, node.Status)
		return nil
	}
	switch node.Kind {
	case KindController, KindAgent, KindAcceptance:
		return rt.activateTaskNode(graphID, nodeID, node, replay)
	case KindRouter:
		return rt.activateRouter(graphID, nodeID, input)
	case KindEnd:
		return rt.activateEnd(graphID, nodeID, input)
	case KindJoin:
		// join 的激活即就绪性评估：未就绪（其它入边源未终态）时保持
		// inactive，等后续结算触发重推导。
		return rt.evaluateJoinLocked(graphID, nodeID)
	case KindWaitEvent:
		return rt.activateWaitEvent(graphID, nodeID, node)
	case KindTool:
		return rt.activateTool(graphID, nodeID, node)
	case KindApproval:
		return rt.activateApproval(graphID, nodeID, node)
	case KindSubgraph:
		return rt.activateSubgraph(graphID, nodeID, node)
	default:
		return rt.suspendUnimplemented(graphID, nodeID, node)
	}
}

// activateTaskNode 激活任务型节点（controller/agent/acceptance）：先 durable
// 写 activation 事实（ready），再发任务；发任务失败把节点标记 failed 并报错
// （不出现「durable 说已激活但永远没有任务」）。路由与能力由 publishTask
// 统一解析（acceptance 默认路由 acceptance.verify，见 resolveRoute）。
// acceptance 是数据驱动的 barrier 节点，走 activateAcceptance 的 data-ready
// 门控路径，不在此直接发任务。
func (rt *Runtime) activateTaskNode(graphID, nodeID string, node Node, replay []InputBinding) error {
	if node.Kind == KindAcceptance {
		return rt.activateAcceptance(graphID, nodeID, node, replay)
	}
	exec, err := rt.activationFor(graphID, nodeID, node, phaseForKind(node.Kind))
	if err != nil {
		return err
	}
	exec.Input = mergeInputBindings(exec.Input, replay)
	hydrateExecutionEvidence(&exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
		return err
	}
	rt.emitActivationCreated(graphID, nodeID, exec)
	return rt.publishTask(graphID, nodeID, node, exec)
}

// activateAcceptance 激活 acceptance 节点：activation durable（ready）后不
// 立即发任务——acceptance 的必需输入（入边源集合）须有生效且绑定到本
// activation 的转移才进入 data-ready；未齐保持 data-wait，后续入边生效时
// 由 evaluateAcceptancesLocked 重评估（结算与 ResumeGraph 的收尾挂钩）。
// 单入边场景（implement→verify）边记录先于激活存在，评估立即就绪，行为
// 与直接发任务一致。
func (rt *Runtime) activateAcceptance(graphID, nodeID string, node Node, replay []InputBinding) error {
	exec, err := rt.activationFor(graphID, nodeID, node, phaseForKind(node.Kind))
	if err != nil {
		return err
	}
	exec.Input = mergeInputBindings(exec.Input, replay)
	hydrateExecutionEvidence(&exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
		return err
	}
	rt.emitActivationCreated(graphID, nodeID, exec)
	return rt.evaluateAcceptanceReadyLocked(graphID, nodeID)
}

// evaluateAcceptanceReadyLocked 评估 acceptance 节点的 data-ready：新图以
// task.required_inputs 声明输入端口，每个端口至少有一条实际选中且绑定到
// 当前 activation 的 TransitionRecord 即就绪。端口为单赋值；并行分支写
// 不同端口，迟到输入不会提前发布。互斥 OR 暂不直接汇合。
//
// 兼容：root acceptance 无入边/无 required_inputs 可直接发布；旧单入边图
// 等第一份绑定；旧多入边 durable 图仍按全部静态源终态且绑定的历史 barrier
// 语义恢复（新 authoring 已拒绝这种歧义形态）。
func (rt *Runtime) evaluateAcceptanceReadyLocked(graphID, nodeID string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	node, ok := doc.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
	}
	if node.Kind != KindAcceptance || node.Status != NodeReady || node.Execution == nil || node.Execution.TaskID != "" {
		return nil
	}
	exec := *node.Execution
	exec.Input = mergeInputBindings(exec.Input, rt.inputsFor(graphID, nodeID, exec.ActivationID))
	required := rt.requiredInputPorts(graphID, doc, nodeID, node)
	if len(required) > 0 {
		if !inputPortsReady(exec.Input, required) {
			return nil
		}
	} else {
		sources := rt.inEdgeSources(graphID, doc, nodeID)
		switch len(sources) {
		case 0: // root acceptance：零输入就是完整输入集合
		case 1:
			if len(exec.Input) == 0 {
				return nil
			}
		default:
			if !rt.legacyBarrierReady(graphID, doc, nodeID, exec.ActivationID, sources) {
				return nil
			}
		}
	}
	// 迟到的入边绑定可能晚于 activation 创建：发布前刷新输入快照，保证
	// 任务注入与谱系核验看到发布时刻的完整事实。若输入确有变化，必须先把
	// ready execution durable，再调用外部 TaskBoard：这样“任务已发布、
	// running 尚未落盘”的崩溃窗口恢复时仍能用同一份完整输入对账。
	hydrateExecutionEvidence(&exec)
	if !reflect.DeepEqual(exec, *node.Execution) {
		sv, err := rt.stateVersion(graphID)
		if err != nil {
			return err
		}
		if err := rt.store.SetExecution(graphID, nodeID, exec, sv); err != nil {
			return err
		}
	}
	node = nodeForExecution(node, exec)
	return rt.publishTask(graphID, nodeID, node, exec)
}

func (rt *Runtime) requiredInputPorts(graphID string, doc *GraphDocument, targetID string, node Node) []string {
	if node.Task != nil && len(node.Task.RequiredInputs) > 0 {
		return append([]string(nil), node.Task.RequiredInputs...)
	}
	return rt.inEdgePorts(graphID, doc, targetID)
}

func inputPortsReady(inputs []InputBinding, required []string) bool {
	bound := make(map[string]struct{}, len(inputs))
	for _, in := range inputs {
		if in.TargetInput != "" {
			bound[in.TargetInput] = struct{}{}
		}
	}
	for _, port := range required {
		if _, ok := bound[port]; !ok {
			return false
		}
	}
	return true
}

// legacyBarrierReady 只用于恢复旧多入边、无端口图。新提交/patch 已在
// authoring 校验拒绝该歧义形态，运行时不再把它当作推荐建图方式。
func (rt *Runtime) legacyBarrierReady(graphID string, doc *GraphDocument, targetID, targetActivationID string, sources []string) bool {
	records := rt.store.Transitions(graphID)
	for _, sourceID := range sources {
		source := doc.Nodes[sourceID]
		if !source.Status.IsTerminal() || source.Execution == nil {
			return false
		}
		bound := false
		for _, rec := range records {
			if rec.SourceNodeID == sourceID && rec.SourceActivationID == source.Execution.ActivationID &&
				rec.TargetNodeID == targetID && rec.TargetActivationID == targetActivationID {
				bound = true
				break
			}
		}
		if !bound {
			return false
		}
	}
	return true
}

// evaluateAcceptancesLocked 重推导全图 acceptance 节点的 data-ready（与
// evaluateJoinsLocked 同族：节点终态结算与 ResumeGraph 的收尾挂钩）。只
// 处理 ready 且尚未发任务的 acceptance activation。
func (rt *Runtime) evaluateAcceptancesLocked(graphID string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	var errs []error
	for _, id := range sortedNodeIDs(doc) {
		n := doc.Nodes[id]
		if n.Kind != KindAcceptance || n.Status != NodeReady || n.Execution == nil || n.Execution.TaskID != "" {
			continue
		}
		errs = append(errs, rt.evaluateAcceptanceReadyLocked(graphID, id))
	}
	return errors.Join(errs...)
}

// activateRouter 激活 router 节点：不发任务，激活即同步求值自己的 next
// （以上游 Result 为输入，事件名缺省按 completed 兜底）。
func (rt *Runtime) activateRouter(graphID, nodeID string, input map[string]any) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
	}
	exec, err := rt.activationFor(graphID, nodeID, node, "routing")
	if err != nil {
		return err
	}
	if err := rt.consumeSynchronousStepLocked(graphID, nodeID, exec.ActivationID); err != nil {
		return err
	}
	node = nodeForExecution(node, exec)
	if err := rt.enterRunning(graphID, nodeID, node, exec); err != nil {
		return err
	}
	if input == nil {
		input, err = rt.resolveExecutionInput(graphID, exec)
		if err != nil {
			reason := fmt.Sprintf("router 节点 %s（activation %s）的 durable Input 不可解引用: %v", nodeID, exec.ActivationID, err)
			return errors.Join(
				rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed, map[string]any{"error": reason}, SettlementContinueGraphFail, reason),
				rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
		}
	}

	// 先判定「完全无出路」以保留 router 的既有失败语义；真正的边选择统一
	// 交给 settleNodeLocked，使完整 input 与 terminal 状态先同条 durable，
	// 再逐条记录转移。崩溃后可用原 input 精确重放条件，而不是以 nil 猜测。
	everMatched := false
	for _, tr := range node.Next {
		if !evalCondition(tr.When, NodeCompleted, input) {
			continue
		}
		everMatched = true
		break
	}
	if !everMatched {
		reason := fmt.Sprintf("router 节点 %s（activation %s）无任何匹配的出路", nodeID, exec.ActivationID)
		if err := rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed,
			map[string]any{"error": reason}, SettlementContinueGraphFail, reason); err != nil {
			return err
		}
		return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
	}
	return rt.settleNodeLocked(graphID, nodeID, exec, NodeCompleted, input)
}

// activateEnd 激活 end 节点：收官即图完成（completed + graph_ended 事件）。
func (rt *Runtime) activateEnd(graphID, nodeID string, input map[string]any) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
	}
	exec, err := rt.activationFor(graphID, nodeID, node, "finalizing")
	if err != nil {
		return err
	}
	if err := rt.consumeSynchronousStepLocked(graphID, nodeID, exec.ActivationID); err != nil {
		return err
	}
	node = nodeForExecution(node, exec)
	if err := rt.enterRunning(graphID, nodeID, node, exec); err != nil {
		return err
	}
	if input == nil {
		input, err = rt.resolveExecutionInput(graphID, exec)
		if err != nil {
			reason := fmt.Sprintf("end 节点 %s（activation %s）的 durable Input 不可解引用: %v", nodeID, exec.ActivationID, err)
			return errors.Join(
				rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed, map[string]any{"error": reason}, SettlementContinueGraphFail, reason),
				rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
		}
	}
	outcome := node.EndOutcome
	if outcome == "" {
		outcome = EndSuccess // legacy GraphDocument compatibility
	}
	if !outcome.IsValid() {
		reason := fmt.Sprintf("end 节点 %s 声明非法 outcome=%q", nodeID, outcome)
		return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
	}
	if err := rt.writeTerminalContinuationWithOutcomeLocked(graphID, nodeID, exec, NodeCompleted,
		input, SettlementContinueGraphOutcome, outcome, ""); err != nil {
		return err
	}
	return rt.commitEndOutcome(graphID, nodeID, exec, outcome, "")
}

// suspendUnimplemented 兜底处理未获执行语义的节点类型：activation 事实
// durable 后挂起为 waiting（保持现场，等后续版本支持），并返回明确的中文
// 「尚未实现」错误。首批 10 种类型全部已获执行语义（C5c 起 acceptance 走
// 任务型路径），本分支只防御绕过校验链进入运行时的未知类型。
func (rt *Runtime) suspendUnimplemented(graphID, nodeID string, node Node) error {
	exec, err := rt.activationFor(graphID, nodeID, node, "suspended:"+string(node.Kind))
	if err != nil {
		return err
	}
	node = nodeForExecution(node, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
		return err
	}
	rt.emitActivationCreated(graphID, nodeID, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeWaiting); err != nil {
		return err
	}
	return fmt.Errorf("graph: 节点 %s 的类型 %q 尚未实现：已通过校验但运行时暂不支持，节点已挂起（waiting）",
		nodeID, node.Kind)
}

// ============================================================
// wait_event 节点
// ============================================================

// activateWaitEvent 激活 wait_event 节点：deadline 与 activation 一起
// durable，随后写 waiting、发 graph_wait_started 并安装进程内 timer。
// 恢复时按原 deadline 重建，过期则立即走 timeout 事件转移。
func (rt *Runtime) activateWaitEvent(graphID, nodeID string, node Node) error {
	exec, err := rt.activationFor(graphID, nodeID, node, "waiting")
	if err != nil {
		return err
	}
	node = nodeForExecution(node, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
		return err
	}
	rt.emitActivationCreated(graphID, nodeID, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeWaiting); err != nil {
		return err
	}
	trace.Emit(trace.Event{
		Kind:         trace.KindGraphWaitStarted,
		GraphID:      graphID,
		NodeID:       nodeID,
		ActivationID: exec.ActivationID,
		Description:  "event=" + node.Wait.Event,
	})
	return rt.scheduleWaitTimeoutLocked(graphID, nodeID, exec)
}

func waitTimerKey(graphID, activationID string) string {
	return graphID + "\x00" + activationID
}

func (rt *Runtime) cancelWaitTimerLocked(graphID, activationID string) {
	key := waitTimerKey(graphID, activationID)
	if timer := rt.waitTimers[key]; timer != nil {
		timer.Stop()
		delete(rt.waitTimers, key)
	}
}

func (rt *Runtime) cancelGraphWaitTimersLocked(graphID string) {
	prefix := graphID + "\x00"
	for key, timer := range rt.waitTimers {
		if strings.HasPrefix(key, prefix) {
			timer.Stop()
			delete(rt.waitTimers, key)
		}
	}
}

// scheduleWaitTimeoutLocked 根据 durable deadline 安装纯唤醒 timer。timer
// 本身不是权威状态；回调重新校验 graph/node/activation，随后先 durable 写
// timeout 终态与边选择。调用方持 rt.mu。
func (rt *Runtime) scheduleWaitTimeoutLocked(graphID, nodeID string, exec Execution) error {
	if exec.WaitDeadline == nil {
		return nil
	}
	rt.cancelWaitTimerLocked(graphID, exec.ActivationID)
	delay := time.Until(*exec.WaitDeadline)
	if delay <= 0 {
		return rt.settleWaitTimeoutLocked(graphID, nodeID, exec.ActivationID)
	}
	key := waitTimerKey(graphID, exec.ActivationID)
	rt.waitTimers[key] = time.AfterFunc(delay, func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		rt.synchronousSteps = 0
		delete(rt.waitTimers, key)
		if err := rt.settleWaitTimeoutLocked(graphID, nodeID, exec.ActivationID); err != nil {
			log.Printf("[graph] WARNING: 图 %s 节点 %s activation %s 超时结算失败: %v",
				graphID, nodeID, exec.ActivationID, err)
		}
	})
	return nil
}

func (rt *Runtime) settleWaitTimeoutLocked(graphID, nodeID, activationID string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	if rt.isSuspendedLocked(graphID) {
		// 停驻期间 timer 已停走：此为停走前已触发、等在 rt.mu 上的在途
		// 回调。deadline 是 durable wall-clock，解冻恢复时按原 deadline
		// 补结算（已过期则立即超时），此处直接吞掉。
		return nil
	}
	node, ok := doc.Nodes[nodeID]
	if !ok || node.Execution == nil || node.Execution.ActivationID != activationID || node.Status != NodeWaiting {
		return nil // 外部事件/其它终态已先结算，timer 迟到自然幂等
	}
	activeNode := nodeForExecution(node, *node.Execution)
	if activeNode.Kind != KindWaitEvent {
		return nil
	}
	exec := *node.Execution
	if err := rt.writeNode(graphID, nodeID, exec, NodeRunning); err != nil {
		return err
	}
	result := map[string]any{"event": EventTimeout}
	if err := rt.writeTerminalLocked(graphID, nodeID, exec, NodeCompleted, result); err != nil {
		return err
	}
	trace.Emit(trace.Event{
		Kind: trace.KindGraphWaitResumed, GraphID: graphID, NodeID: nodeID,
		ActivationID: activationID, Description: "event=" + EventTimeout,
	})
	return rt.evalTransitionsLocked(graphID, nodeID, activationID, NodeCompleted, result)
}

// ============================================================
// tool 节点
// ============================================================

// activateTool 激活 tool 节点：先 durable 写 activation 事实（ready）与
// executing（running），再同步调用 ToolExecutor；成功写 Result+completed，
// 失败写 Error+failed（节点失败是正常图语义，交给转移求值路由）。
// 未注入 executor 时节点 failed 并返回中文错误（不是挂起）。
// 恢复路径（ready 重入）复用 durable activation，executor 此前从未被调用。
func (rt *Runtime) activateTool(graphID, nodeID string, node Node) error {
	exec, err := rt.activationFor(graphID, nodeID, node, "executing")
	if err != nil {
		return err
	}
	if err := rt.consumeSynchronousStepLocked(graphID, nodeID, exec.ActivationID); err != nil {
		return err
	}
	node = nodeForExecution(node, exec)
	switch node.Status {
	case NodeInactive, NodeCompleted, NodeFailed, NodeBlocked:
		if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
			return err
		}
		rt.emitActivationCreated(graphID, nodeID, exec)
	case NodeReady:
		// 恢复路径：activation 已 durable，直接补进 executing。
	default:
		return fmt.Errorf("graph: 图 %s 节点 %s 状态 %q 不能激活 tool", graphID, nodeID, node.Status)
	}
	if err := rt.writeNode(graphID, nodeID, exec, NodeRunning); err != nil {
		return err
	}
	if rt.toolExec == nil {
		reason := fmt.Sprintf("节点 %s（activation %s）需要执行工具 %q 但未注入 ToolExecutor",
			nodeID, exec.ActivationID, node.Tool.Name)
		return errors.Join(
			rt.settleNodeLocked(graphID, nodeID, exec, NodeFailed, map[string]any{"error": reason}),
			fmt.Errorf("graph: %s", reason),
		)
	}
	result, err := rt.toolExec.ExecuteNodeTool(context.Background(), node.Tool.Name, node.Tool.Args)
	if err != nil {
		return rt.settleNodeLocked(graphID, nodeID, exec, NodeFailed,
			map[string]any{"error": fmt.Sprintf("工具 %q 执行失败: %v", node.Tool.Name, err)})
	}
	return rt.settleNodeLocked(graphID, nodeID, exec, NodeCompleted, result)
}

// ============================================================
// approval 节点
// ============================================================

// activateApproval 激活 approval 节点：durable 写 activation 事实与 waiting
// 后向 ApprovalGateway 发审批请求（requestID 记入 execution），发
// graph_wait_started。未注入 gateway 时保持「尚未实现」挂起（同 C3 现状：
// 返回明确中文错误，节点停留 waiting）。
func (rt *Runtime) activateApproval(graphID, nodeID string, node Node) error {
	exec, err := rt.activationFor(graphID, nodeID, node, "waiting")
	if err != nil {
		return err
	}
	node = nodeForExecution(node, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
		return err
	}
	rt.emitActivationCreated(graphID, nodeID, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeWaiting); err != nil {
		return err
	}
	if rt.approval == nil {
		return fmt.Errorf("graph: 节点 %s 的类型 %q 尚未实现：已通过校验但未注入 ApprovalGateway，节点已挂起（waiting）",
			nodeID, node.Kind)
	}
	return rt.requestApprovalLocked(graphID, nodeID, node, exec)
}

// requestApprovalLocked 向网关发出审批请求并把 requestID durable 进
// execution（waiting 不变，execution-only 写入）。「waiting 已 durable、
// requestID 未 durable」的崩溃窗口由 ResumeGraph 的补发（网关按
// (graph_id, activation_id) 幂等去重）兜住。
func (rt *Runtime) requestApprovalLocked(graphID, nodeID string, node Node, exec Execution) error {
	spec := ApprovalSpec{GraphID: graphID, NodeID: nodeID, ActivationID: exec.ActivationID}
	if node.Task != nil {
		spec.Title = node.Task.Title
		spec.Description = node.Task.Description
	}
	requestID, err := rt.approval.RequestApproval(spec)
	if err != nil {
		reason := fmt.Sprintf("节点 %s（activation %s）审批请求失败: %v", nodeID, exec.ActivationID, err)
		return errors.Join(
			rt.settleNodeLocked(graphID, nodeID, exec, NodeFailed, map[string]any{"error": err.Error()}),
			fmt.Errorf("graph: %s", reason),
		)
	}
	exec.RequestID = requestID
	sv, err := rt.stateVersion(graphID)
	if err != nil {
		return err
	}
	if err := rt.store.SetExecution(graphID, nodeID, exec, sv); err != nil {
		return err
	}
	trace.Emit(trace.Event{
		Kind:         trace.KindGraphWaitStarted,
		GraphID:      graphID,
		NodeID:       nodeID,
		ActivationID: exec.ActivationID,
		Description:  "approval request=" + requestID,
	})
	return nil
}

// ============================================================
// subgraph 节点
// ============================================================

// activateSubgraph 激活 subgraph 节点：durable 写 activation 事实与 waiting
// 后，把内联子图物化为独立图（graph_id = <父图ID>/<activationID>）提交到
// 同一 Store 并激活其 root；父节点挂起，等子图 graph_ended 回调结算
// （onChildGraphEnded）。子图 graph_id 幂等：已存在不重复 Submit。
func (rt *Runtime) activateSubgraph(graphID, nodeID string, node Node) error {
	exec, err := rt.activationFor(graphID, nodeID, node, "waiting")
	if err != nil {
		return err
	}
	node = nodeForExecution(node, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
		return err
	}
	rt.emitActivationCreated(graphID, nodeID, exec)
	if err := rt.writeNode(graphID, nodeID, exec, NodeWaiting); err != nil {
		return err
	}
	return rt.ensureSubgraphLocked(graphID, nodeID, node, exec)
}

// ensureSubgraphLocked 保证子图已物化并记入 execution（激活与恢复共用，
// 幂等）：先把子图 ID durable 进 execution（物化可能同步收官并立刻回调
// 结算父节点，回调守卫要求 ID 已记录）；深度超限或子图提交失败时父节点
// failed；子图已存在（含恢复后磁盘找回）不重复提交；子图已到终态（恢复
// 竞态：崩溃前子图已收官但父节点未结算）直接结算父节点。
func (rt *Runtime) ensureSubgraphLocked(graphID, nodeID string, node Node, exec Execution) error {
	childID := graphID + "/" + exec.ActivationID
	if depth := strings.Count(childID, "/") + 1; depth > MaxSubgraphDepth {
		reason := fmt.Sprintf("节点 %s（activation %s）物化子图深度 %d 超过上限 %d",
			nodeID, exec.ActivationID, depth, MaxSubgraphDepth)
		return errors.Join(
			rt.settleNodeLocked(graphID, nodeID, exec, NodeFailed, map[string]any{"error": reason}),
			fmt.Errorf("graph: %s", reason),
		)
	}
	if exec.ChildGraphID == "" {
		// 记录子图 ID（waiting 不变，execution-only 写入；恢复时也可由
		// 父图 ID + activationID 推导，双保险）。
		exec.ChildGraphID = childID
		sv, err := rt.stateVersion(graphID)
		if err != nil {
			return err
		}
		if err := rt.store.SetExecution(graphID, nodeID, exec, sv); err != nil {
			return err
		}
	}
	child, exists := rt.store.Get(childID)
	if !exists {
		childDoc := materializeSubgraphDoc(childID, node.Subgraph)
		if err := rt.submitGraphLocked(childDoc); err != nil {
			reason := fmt.Sprintf("节点 %s（activation %s）物化子图 %s 失败: %v",
				nodeID, exec.ActivationID, childID, err)
			return errors.Join(
				rt.settleNodeLocked(graphID, nodeID, exec, NodeFailed, map[string]any{"error": reason}),
				fmt.Errorf("graph: %s", reason),
			)
		}
		child, exists = rt.store.Get(childID)
	}
	if exists && child.Status.IsTerminal() {
		// 子图已终态（同步收官的回调可能已结算父节点，此处凭守卫幂等跳过；
		// 恢复路径则在此真正补结算）。
		return rt.settleSubgraphParentLocked(graphID, nodeID, exec.ActivationID, childID, child.Status)
	}
	return nil
}

// materializeSubgraphDoc 把内联子图规格物化为可提交的 GraphDocument：
// 全部节点运行状态归零（inactive、无 executor/execution），图状态 pending。
func materializeSubgraphDoc(childID string, spec *SubgraphSpec) *GraphDocument {
	nodes := make(map[string]Node, len(spec.Nodes))
	for id, n := range spec.Nodes {
		n.Status = NodeInactive
		n.Executor = nil
		n.Execution = nil
		nodes[id] = n
	}
	return &GraphDocument{
		Schema:  SchemaV1,
		GraphID: childID,
		Root:    spec.Root,
		Status:  GraphPending,
		Nodes:   nodes,
	}
}

// onChildGraphEnded 子图到达终态（graph_ended 已发出）后的父图回调：
// 从子图 ID（<父图ID>/<父节点activationID>）推导父图与父节点并结算。
// 非子图（ID 无 "/" 段）或父图/父节点已不在结算窗口内时静默忽略。
func (rt *Runtime) onChildGraphEnded(childID string, childStatus GraphStatus) error {
	slash := strings.LastIndexByte(childID, '/')
	if slash <= 0 || slash == len(childID)-1 {
		return nil
	}
	parentID, activationID := childID[:slash], childID[slash+1:]
	nodeID, _, ok := parseActivationID(activationID)
	if !ok {
		return nil
	}
	if rt.isSuspendedLocked(parentID) {
		// 停驻闸门：子图终态（如控制面冻结期取消子图）不推进父图。该事实
		// 可自愈——解冻恢复时 ensureSubgraphLocked 见子图已终态会补结算。
		log.Printf("[graph] DEBUG 父图 %s 已停驻，吞掉子图 %s 的终态回调（解冻恢复时补结算）", parentID, childID)
		return nil
	}
	return rt.settleSubgraphParentLocked(parentID, nodeID, activationID, childID, childStatus)
}

// settleSubgraphParentLocked 结算 subgraph 父节点：守卫（父图在途、节点是
// waiting 的 subgraph、activation 匹配、记录的子图 ID 一致）全部满足才
// 生效——重复/迟到回调天然幂等。子图 completed 映射父节点 completed，
// failed/cancelled 映射 failed；Result={event, child_graph_id, child_result}
// 随后走转移求值（配合下游 when event completed/failed 条件）。
func (rt *Runtime) settleSubgraphParentLocked(parentID, nodeID, activationID, childID string, childStatus GraphStatus) error {
	doc, err := rt.graph(parentID)
	if err != nil {
		log.Printf("[graph] DEBUG 子图 %s 终态回调：父图 %s 不可读（%v），忽略", childID, parentID, err)
		return nil
	}
	if doc.Status.IsTerminal() {
		log.Printf("[graph] DEBUG 子图 %s 终态回调：父图 %s 已是终态 %q，忽略", childID, parentID, doc.Status)
		return nil
	}
	node, ok := doc.Nodes[nodeID]
	if !ok || node.Execution == nil {
		return nil
	}
	ex := node.Execution
	activeNode := nodeForExecution(node, *ex)
	if activeNode.Kind != KindSubgraph {
		return nil
	}
	if ex.ActivationID != activationID || node.Status != NodeWaiting {
		log.Printf("[graph] DEBUG 子图 %s 终态回调：父节点 %s 状态 %q / activation %q 不在结算窗口，忽略",
			childID, nodeID, node.Status, activationOf(node))
		return nil
	}
	if ex.ChildGraphID != "" && ex.ChildGraphID != childID {
		log.Printf("[graph] DEBUG 子图 %s 终态回调：父节点 %s 记录的子图为 %q，忽略",
			childID, nodeID, ex.ChildGraphID)
		return nil
	}
	event, status := EventCompleted, NodeCompleted
	switch childStatus {
	case GraphCompleted:
	case GraphBlocked:
		event, status = EventBlocked, NodeBlocked
	case GraphFailed, GraphCancelled:
		event, status = EventFailed, NodeFailed
	default:
		return fmt.Errorf("graph: 子图 %s 终态 status=%q 非法", childID, childStatus)
	}
	childOutcome := childOutcomeFromStatus(childStatus)
	if child, ok := rt.store.Get(childID); ok && child.Outcome != nil {
		childOutcome = child.Outcome.Outcome
	}
	result := map[string]any{
		"event":                event,
		"child_status":         string(childStatus),
		"child_outcome":        string(childOutcome),
		"child_graph_id":       childID,
		"child_result_ref":     rt.childResultRef(childID),
		"child_result_summary": rt.childResultSummary(childID),
		"child_result":         rt.childResultSummary(childID), // 兼容旧消费者
	}
	exec := *ex
	if err := rt.writeNode(parentID, nodeID, exec, NodeRunning); err != nil {
		return err
	}
	if err := rt.writeTerminalLocked(parentID, nodeID, exec, status, result); err != nil {
		return err
	}
	return rt.evalTransitionsLocked(parentID, nodeID, activationID, status, result)
}

func childOutcomeFromStatus(status GraphStatus) EndOutcome {
	switch status {
	case GraphCompleted:
		return EndSuccess
	case GraphFailed:
		return EndFailed
	case GraphBlocked:
		return EndBlocked
	case GraphCancelled:
		return EndCancelled
	default:
		return ""
	}
}

// childResultRef 返回子图 end activation 的稳定 ResultRef。升级中间态可能
// 仍把摘要写在 Execution.ResultRef；此时只在 activation Result Store 确认
// 可解引用后才返回，绝不把摘要伪装成引用。
func (rt *Runtime) childResultRef(childID string) string {
	child, ok := rt.store.Get(childID)
	if !ok {
		return ""
	}
	for _, id := range sortedNodeIDs(child) {
		n := child.Nodes[id]
		activeNode := n
		if n.Execution != nil {
			activeNode = nodeForExecution(n, *n.Execution)
		}
		if activeNode.Kind != KindEnd || n.Execution == nil {
			continue
		}
		if n.Execution.ResultRef != "" {
			if _, ok := rt.store.ResolveActivationResult(childID, n.Execution.ResultRef); ok {
				return n.Execution.ResultRef
			}
		}
		ref := activationResultRef(childID, n.Execution.ActivationID)
		if _, ok := rt.store.ResolveActivationResult(childID, ref); ok {
			return ref
		}
	}
	return ""
}

// childResultSummary 取子图 end activation 的展示摘要；新记录读取明确的
// ResultSummary，升级中间态/旧记录才兼容回落或按稳定引用重新生成。
func (rt *Runtime) childResultSummary(childID string) string {
	child, ok := rt.store.Get(childID)
	if !ok {
		return ""
	}
	for _, id := range sortedNodeIDs(child) {
		n := child.Nodes[id]
		activeNode := n
		if n.Execution != nil {
			activeNode = nodeForExecution(n, *n.Execution)
		}
		if activeNode.Kind == KindEnd && n.Execution != nil {
			if n.Execution.ResultSummary != "" {
				return n.Execution.ResultSummary
			}
			if n.Execution.ResultRef != "" {
				if stored, ok := rt.store.ResolveActivationResult(childID, n.Execution.ResultRef); ok {
					var result map[string]any
					if json.Unmarshal(stored.Result, &result) == nil {
						return summarizeResult(result)
					}
				}
				return n.Execution.ResultRef // 旧记录：该字段历史上就是摘要
			}
		}
	}
	return string(child.Status)
}

// ============================================================
// join 节点
// ============================================================

// evaluateJoinLocked 评估单个 join 节点的就绪性（readiness 纯推导，不加
// 持久状态）：
//   - 新图：每个 required input port 恰有一条实际选中边绑定到同一目标
//     activation 后归并完成；不同端口是 AND，每个端口都是单赋值；
//   - 兼容旧无端口图：全部静态源终态且至少一条入边生效时归并；全部源
//     终态但无入边生效时置 skipped（终态，不触发 next）；
//   - 否则保持现状，后续边生效或恢复收尾时再次纯推导。
//
// 可处理的状态：inactive（首次）、ready（就绪结算中途崩溃，就绪性单调，
// 重评估仍就绪）、completed/failed/blocked（回边后再就绪，新 activation）、
// waiting（C3 挂起遗留，复用原 activation）。
func (rt *Runtime) evaluateJoinLocked(graphID, joinID string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	node, ok := doc.Nodes[joinID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, joinID)
	}
	if node.Execution != nil {
		node = nodeForExecution(node, *node.Execution)
	}
	switch node.Status {
	case NodeInactive, NodeReady, NodeWaiting, NodeCompleted, NodeFailed, NodeBlocked:
	default:
		return nil // running/skipped/cancelled 不就绪评估
	}
	sources := rt.inEdgeSources(graphID, doc, joinID)
	targetActivationID := rt.pendingTargetActivationID(graphID, joinID, node)
	if node.Execution != nil && (node.Status == NodeReady || node.Status == NodeWaiting) {
		targetActivationID = node.Execution.ActivationID
	}
	requiredPorts := rt.requiredInputPorts(graphID, doc, joinID, node)
	ready, fired, merged, err := rt.joinReadiness(graphID, doc, joinID, targetActivationID, sources, requiredPorts)
	if err != nil {
		reason := fmt.Sprintf("join 节点 %s 无法解引用输入: %v", joinID, err)
		return errors.Join(rt.failGraph(graphID, reason), fmt.Errorf("graph: %s", reason))
	}
	if !ready {
		return nil
	}
	if len(fired) == 0 {
		if node.Status != NodeInactive {
			return nil
		}
		exec, err := rt.activationFor(graphID, joinID, node, "joining")
		if err != nil {
			return err
		}
		if err := rt.writeNode(graphID, joinID, exec, NodeSkipped); err != nil {
			return err
		}
		rt.emitActivationCreated(graphID, joinID, exec)
		log.Printf("[graph] DEBUG 图 %s join 节点 %s 全部 %d 个入边源已终态但无入边生效，置 skipped",
			graphID, joinID, len(sources))
		return nil
	}
	exec, err := rt.activationFor(graphID, joinID, node, "joining")
	if err != nil {
		return err
	}
	if err := rt.consumeSynchronousStepLocked(graphID, joinID, exec.ActivationID); err != nil {
		return err
	}
	if node.Status == NodeWaiting {
		// C3 挂起遗留：复用原 activation，waiting → running。
		if err := rt.writeNode(graphID, joinID, exec, NodeRunning); err != nil {
			return err
		}
	} else {
		if err := rt.enterRunning(graphID, joinID, node, exec); err != nil {
			return err
		}
	}
	if err := rt.writeTerminalLocked(graphID, joinID, exec, NodeCompleted, merged); err != nil {
		return err
	}
	total := len(sources)
	if len(requiredPorts) > 0 {
		total = len(requiredPorts)
	}
	trace.Emit(trace.Event{
		Kind:         trace.KindGraphJoinResolved,
		GraphID:      graphID,
		NodeID:       joinID,
		ActivationID: exec.ActivationID,
		Description:  fmt.Sprintf("生效输入 %d/%d", len(fired), total),
	})
	return rt.evalTransitionsLocked(graphID, joinID, exec.ActivationID, NodeCompleted, merged)
}

// evaluateJoinsLocked 重推导全图 join 节点的就绪性（节点结算与 ResumeGraph
// 的收尾挂钩）。只处理 inactive/ready/waiting 的 join：已归并的 join 不会
// 因同一入边集合重复触发（回边后的再就绪经 activateLocked 的新转移记录驱动）。
func (rt *Runtime) evaluateJoinsLocked(graphID string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	var errs []error
	for _, id := range sortedNodeIDs(doc) {
		n := doc.Nodes[id]
		activeNode := n
		if n.Execution != nil {
			activeNode = nodeForExecution(n, *n.Execution)
		}
		if activeNode.Kind != KindJoin {
			continue
		}
		if n.Status != NodeInactive && n.Status != NodeReady && n.Status != NodeWaiting {
			continue
		}
		errs = append(errs, rt.evaluateJoinLocked(graphID, id))
	}
	return errors.Join(errs...)
}

// joinReadiness 以目标 activation 的实际输入端口推导 barrier：requiredPorts
// 非空时每个端口至少一条实际选中边；空时只为旧图保留静态源 barrier。
// 归并键优先使用 target_input（数据流端口），旧记录回落 source_node_id。
// Result 必须来自 activation Result Store 或旧记录的完整内联值，绝不回退到
// 展示摘要 ResultRef。
func (rt *Runtime) joinReadiness(graphID string, doc *GraphDocument, joinID, targetActivationID string, sources, requiredPorts []string) (ready bool, fired []string, merged map[string]any, err error) {
	if targetActivationID == "" {
		allSourcesTerminal := len(sources) > 0
		for _, sourceID := range sources {
			if !doc.Nodes[sourceID].Status.IsTerminal() {
				allSourcesTerminal = false
				break
			}
		}
		if allSourcesTerminal || (len(sources) == 0 && len(requiredPorts) == 0) {
			return true, nil, map[string]any{}, nil
		}
		return false, nil, nil, nil
	}
	inputs := rt.inputsFor(graphID, joinID, targetActivationID)
	if len(requiredPorts) > 0 {
		if !inputPortsReady(inputs, requiredPorts) {
			return false, nil, nil, nil
		}
	} else {
		for _, sourceID := range sources {
			if !doc.Nodes[sourceID].Status.IsTerminal() {
				return false, nil, nil, nil
			}
		}
	}

	merged = make(map[string]any, len(inputs))
	for _, rec := range rt.store.Transitions(graphID) {
		if rec.TargetNodeID != joinID || rec.TargetActivationID != targetActivationID {
			continue
		}
		value, resolveErr := rt.resolveTransitionResult(graphID, rec)
		if resolveErr != nil {
			return false, nil, nil, resolveErr
		}
		key := rec.TargetInput
		if key == "" {
			key = rec.SourceNodeID
		}
		fired = append(fired, key)
		if previous, exists := merged[key]; exists {
			switch list := previous.(type) {
			case []any:
				merged[key] = append(list, value)
			default:
				merged[key] = []any{previous, value}
			}
		} else {
			merged[key] = value
		}
	}
	return true, fired, merged, nil
}

// inEdgeSources 计算 join 对当前各 source activation 可见的入边集合：有
// activation 的节点读取冻结 Definition.Next，尚未激活的节点读取当前定义；
// 已 durable 且指向 join 的 transition 也纳入，避免 patch 改变 next 下标后
// 丢失旧 activation 的路由事实。
func (rt *Runtime) inEdgeSources(graphID string, doc *GraphDocument, joinID string) []string {
	set := make(map[string]struct{})
	for _, id := range sortedNodeIDs(doc) {
		node := doc.Nodes[id]
		if node.Execution != nil {
			node = nodeForExecution(node, *node.Execution)
		}
		for _, tr := range node.Next {
			if tr.To == joinID {
				set[id] = struct{}{}
				break
			}
		}
	}
	for _, rec := range rt.store.Transitions(graphID) {
		if rec.TargetNodeID != joinID {
			continue
		}
		source, ok := doc.Nodes[rec.SourceNodeID]
		if ok && source.Execution != nil && source.Execution.ActivationID == rec.SourceActivationID {
			set[rec.SourceNodeID] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// inEdgePorts 推导目标节点的输入端口契约：优先读取各来源当前 activation
// 冻结 Definition.Next，另把已 durable TransitionRecord.TargetInput 纳入，
// 防止 patch 后丢失在途 activation 的端口。不同端口构成 AND barrier；端口
// 是单赋值，当前不允许多条候选入边共享端口。
func (rt *Runtime) inEdgePorts(graphID string, doc *GraphDocument, targetID string) []string {
	set := make(map[string]struct{})
	for _, id := range sortedNodeIDs(doc) {
		node := doc.Nodes[id]
		if node.Execution != nil {
			node = nodeForExecution(node, *node.Execution)
		}
		for _, tr := range node.Next {
			if tr.To == targetID && tr.TargetInput != "" {
				set[tr.TargetInput] = struct{}{}
			}
		}
	}
	for _, rec := range rt.store.Transitions(graphID) {
		if rec.TargetNodeID == targetID && rec.TargetInput != "" {
			set[rec.TargetInput] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for port := range set {
		out = append(out, port)
	}
	sort.Strings(out)
	return out
}

// publishTask 发布节点任务：成功则 durable task_id 并置 running；失败则
// 节点标记 failed、图置 failed 并返回中文错误。恢复路径以同一 activation
// 补发（TaskBoard 幂等键去重）。
func (rt *Runtime) publishTask(graphID, nodeID string, node Node, exec Execution) error {
	node = nodeForExecution(node, exec)
	spec := rt.taskSpecFor(graphID, nodeID, node, exec)
	if rt.board == nil {
		reason := fmt.Sprintf("节点 %s（activation %s）需要发布任务但 TaskBoard 未配置", nodeID, exec.ActivationID)
		return errors.Join(
			rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed,
				map[string]any{"error": reason}, SettlementContinueGraphFail, reason),
			rt.failGraph(graphID, reason),
			fmt.Errorf("graph: %s", reason),
		)
	}
	if spec.ControllerRole == ControllerRoleLoopRecovery {
		sourceTaskID, err := rt.recoverySourceTaskID(graphID, spec)
		if err != nil {
			reason := fmt.Sprintf("恢复节点 %s（activation %s）无法绑定 intervention source Task: %v",
				nodeID, exec.ActivationID, err)
			return errors.Join(
				rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed,
					map[string]any{"error": reason}, SettlementContinueGraphFail, reason),
				rt.failGraph(graphID, reason),
				fmt.Errorf("graph: %s", reason),
			)
		}
		spec.RecoverySourceTaskID = sourceTaskID
	}
	taskID, err := rt.board.PublishGraphTask(spec)
	if err != nil {
		reason := fmt.Sprintf("节点 %s（activation %s）任务发布失败: %v", nodeID, exec.ActivationID, err)
		return errors.Join(
			rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed,
				map[string]any{"error": reason}, SettlementContinueGraphFail, reason),
			rt.failGraph(graphID, reason),
			fmt.Errorf("graph: %s", reason),
		)
	}
	exec.TaskID = taskID
	return rt.writeNode(graphID, nodeID, exec, NodeRunning)
}

// recoverySourceTaskID 从 Graph 的冻结数据流绑定解析 recovery controller 的
// 唯一来源 Task。它只消费 InputBinding/TaskBoard 权威事实，不解析模型文本。
func (rt *Runtime) recoverySourceTaskID(graphID string, spec TaskSpec) (string, error) {
	if rt.board == nil {
		return "", fmt.Errorf("TaskBoard 未配置")
	}
	var sourceActivationID string
	for _, input := range spec.Inputs {
		if input.TargetInput != "failure_context" {
			continue
		}
		if sourceActivationID != "" && sourceActivationID != input.SourceActivationID {
			return "", fmt.Errorf("failure_context 存在多个来源 activation")
		}
		sourceActivationID = input.SourceActivationID
	}
	if sourceActivationID == "" {
		return "", fmt.Errorf("缺少 failure_context InputBinding")
	}
	snapshot, found, err := rt.board.LookupGraphTask(graphID, sourceActivationID, "")
	if err != nil {
		return "", err
	}
	if !found || strings.TrimSpace(snapshot.TaskID) == "" || snapshot.TerminalStatus == "" {
		return "", fmt.Errorf("来源 activation %s 缺少终态 Task authority", sourceActivationID)
	}
	return snapshot.TaskID, nil
}

func (rt *Runtime) taskSpecFor(graphID, nodeID string, node Node, exec Execution) TaskSpec {
	spec := taskSpecFor(graphID, nodeID, node, exec)
	if doc, ok := rt.store.Get(graphID); ok {
		spec.DefinitionDigestVersion = doc.DefinitionDigestVersion
		spec.RunID = doc.RunID
		if doc.RunContract != nil {
			run := *doc.RunContract
			spec.RunContract = &run
		}
	}
	// TaskSpec 是一次性发布副本：大 Result 在 GraphDocument/TransitionRecord
	// 中仍只保留有界内联 + ResultRef，此处按引用临时展开给任务桥，由桥的总
	// 上下文上限决定实际注入量，不把全文重复写回 Graph journal。
	for i := range spec.Inputs {
		if spec.Inputs[i].ResultRef == "" {
			continue
		}
		if stored, ok := rt.store.ResolveActivationResult(graphID, spec.Inputs[i].ResultRef); ok {
			spec.Inputs[i].Result = append(json.RawMessage(nil), stored.Result...)
		}
	}
	for _, input := range spec.Inputs {
		if input.TargetInput != "recovery_directive" || len(input.Result) == 0 {
			continue
		}
		var delta RecoveryDelta
		if json.Unmarshal(input.Result, &delta) == nil && strings.TrimSpace(delta.StartPermitRef) != "" {
			spec.RunBudgetPermitRef = delta.StartPermitRef
		}
	}
	if node.Kind == KindAcceptance {
		spec.MissingEvidence = rt.missingEvidenceRequirements(graphID, exec, spec.RequiredEvidence)
	}
	// 终态契约 v2 §5：任务发布时从本 activation 冻结定义的出边机械派生
	// 输出契约（随任务描述钉入 TaskMemory）。v1 图不注入（零行为变化）；
	// 无 path 条件出边时 RenderOutputContract 返回空串，任务桥自然跳过。
	// 派生以冻结定义为准：patch_graph 只影响后续 activation，重进发布的新
	// 任务按当时冻结定义重新派生。
	if doc, ok := rt.store.Get(graphID); ok && doc.Schema == SchemaV2 {
		spec.OutputContract = RenderOutputContract(nodeForExecution(node, exec).Next)
	}
	return spec
}

func taskSpecFor(graphID, nodeID string, node Node, exec Execution) TaskSpec {
	effective := nodeForExecution(node, exec)
	spec := TaskSpec{
		GraphID:             graphID,
		NodeID:              nodeID,
		ActivationID:        exec.ActivationID,
		NodeKind:            effective.Kind,
		ControllerRole:      ControllerRoleOf(effective),
		Route:               resolveRoute(effective),
		ProgressContractRef: effective.ProgressContractRef,
		ContextPolicyRef:    effective.ContextPolicyRef,
		TypedOutputContract: effective.OutputContract,
	}
	if effective.Task != nil {
		spec.Title = effective.Task.Title
		spec.Description = effective.Task.Description
		spec.RequiredEvidence = append([]EvidenceRequirement(nil), effective.Task.RequiredEvidence...)
	}
	if effective.ProgressContractRef == "progress:code-change/v5" {
		spec.FulfillmentContract = &fulfillment.Contract{
			RequireWorkspaceChange: true, RequiredCheckIDs: []string{"verification"},
		}
	}
	if c := effective.Capability; c != nil {
		spec.Tools = c.Tools
		spec.Model = c.Model
		spec.Isolation = c.Isolation
	}
	spec.Inputs = append([]InputBinding(nil), exec.Input...)
	return spec
}

// reconcileRunningTaskLocked 对账 durable running activation 与 TaskBoard。
// Task 缺失时用同 activation 补发；Task 已终态但 Graph journal 尚未结算时
// 直接回填。expectedTaskID 交给 bridge 做 Effect unknown 等安全裁决。
func (rt *Runtime) reconcileRunningTaskLocked(graphID, nodeID string, node Node, exec Execution) error {
	return rt.reconcileTaskLocked(graphID, nodeID, node, exec)
}

// reconcileTaskLocked 对账 ready/running 任务型 activation 与 TaskBoard。
// ready 也必须先 lookup：它覆盖「Task 已发布、task_id/running 尚未 durable」
// 的崩溃窗口，并允许 bridge 用确定性 TaskID 把 Effect unknown 合成为 blocked，
// 而不是直接补发并重复副作用。
func (rt *Runtime) reconcileTaskLocked(graphID, nodeID string, node Node, exec Execution) error {
	if rt.board == nil {
		return fmt.Errorf("graph: 恢复图 %s 节点 %s 时 TaskBoard 未配置", graphID, nodeID)
	}
	snapshot, found, err := rt.board.LookupGraphTask(graphID, exec.ActivationID, exec.TaskID)
	if err != nil {
		return fmt.Errorf("graph: 查询图 %s 节点 %s activation %s 的任务: %w", graphID, nodeID, exec.ActivationID, err)
	}
	if !found {
		taskID, err := rt.board.PublishGraphTask(rt.taskSpecFor(graphID, nodeID, node, exec))
		if err != nil {
			return fmt.Errorf("graph: 恢复补发图 %s 节点 %s activation %s 的任务: %w", graphID, nodeID, exec.ActivationID, err)
		}
		exec.TaskID = taskID
		if node.Status == NodeReady {
			return rt.writeNode(graphID, nodeID, exec, NodeRunning)
		}
		sv, err := rt.stateVersion(graphID)
		if err != nil {
			return err
		}
		return rt.store.SetExecution(graphID, nodeID, exec, sv)
	}
	if snapshot.NodeKind != "" && snapshot.NodeKind != node.Kind {
		return fmt.Errorf("graph: 图 %s 节点 %s activation %s 的 Task 节点类型=%s，与冻结定义=%s 不一致",
			graphID, nodeID, exec.ActivationID, snapshot.NodeKind, node.Kind)
	}
	taskID := snapshot.TaskID
	if taskID == "" {
		taskID = exec.TaskID
	}
	if taskID == "" {
		return fmt.Errorf("graph: TaskBoard 找到图 %s activation %s，但 task_id 为空", graphID, exec.ActivationID)
	}
	if exec.TaskID != "" && snapshot.TaskID != "" && exec.TaskID != snapshot.TaskID {
		return fmt.Errorf("graph: 图 %s activation %s 的 durable task_id=%s 与 TaskBoard=%s 不一致",
			graphID, exec.ActivationID, exec.TaskID, snapshot.TaskID)
	}
	exec.TaskID = taskID
	if snapshot.TerminalStatus == "" {
		if node.Status == NodeRunning && node.Execution != nil && node.Execution.TaskID == taskID {
			return nil
		}
		if node.Status == NodeReady {
			return rt.writeNode(graphID, nodeID, exec, NodeRunning)
		}
		sv, err := rt.stateVersion(graphID)
		if err != nil {
			return err
		}
		return rt.store.SetExecution(graphID, nodeID, exec, sv)
	}
	switch snapshot.TerminalStatus {
	case NodeCompleted, NodeFailed, NodeBlocked:
	default:
		return fmt.Errorf("graph: TaskBoard 返回非法终态 %q（图 %s activation %s）",
			snapshot.TerminalStatus, graphID, exec.ActivationID)
	}
	fact := TerminalFact{
		GraphID: graphID, NodeID: nodeID, ActivationID: exec.ActivationID,
		TaskID: taskID, Status: snapshot.TerminalStatus, Result: snapshot.Result,
		Evidence:    snapshot.Evidence,
		Fulfillment: snapshot.Fulfillment,
	}
	if node.Status == NodeReady {
		if err := rt.writeNode(graphID, nodeID, exec, NodeRunning); err != nil {
			return err
		}
	}
	// 与正常 OnTaskTerminal 路径同构：先恢复输入谱系，再合并本任务证据。
	// 覆盖会丢失上游 lineage；而 recovery 若据此重写已先落盘的不可变
	// ActivationResult，还会制造“同 activation 改写结果”冲突。
	hydrateExecutionEvidence(&exec)
	if len(fact.Evidence) > 0 {
		exec.Evidence = appendEvidenceUnique(exec.Evidence, fact.Evidence...)
		hydrateExecutionEvidence(&exec)
	}
	if node.Kind == KindAcceptance && fact.Status == NodeCompleted {
		return rt.settleAcceptanceLocked(fact, exec)
	}
	return rt.settleNodeLocked(graphID, nodeID, exec, fact.Status, fact.Result)
}

// ============================================================
// 转移条件求值
// ============================================================

// evalCondition 求值一条转移条件。when 缺省恒真；事件形态中 failed /
// blocked 终态是绝对路由权威，只有 completed 才允许 Result["event"]
// （非空字符串）细分业务事件，缺省回落 completed；"always" 恒真；
// 条件形态对 Result 按 $.field[.subfield] 路径取值后应用 eq/ne/in/exists。
//
// 路径缺失语义（与 jq 的 null 语义对齐）：eq=false、ne=true、in=false、
// exists=false；exists 只判断键存在性（值为 null 也算存在）。
func evalCondition(when *Condition, status NodeStatus, result map[string]any) bool {
	if when == nil {
		return true
	}
	if when.Event != "" {
		if when.Event == EventAlways {
			return true
		}
		return when.Event == eventNameOf(status, result)
	}
	v, ok := valueAtPath(result, when.Path)
	switch when.Operator {
	case OpExists:
		return ok
	case OpEq:
		return ok && scalarEqual(v, when.Value)
	case OpNe:
		return !ok || !scalarEqual(v, when.Value)
	case OpIn:
		s, isStr := v.(string)
		return ok && isStr && inStringList(s, when.Value)
	}
	return false
}

// eventNameOf 求事件形态的当前事件名。failed / blocked 终态绝对优先，
// 防止节点用 Result["event"] 伪装成功；仅 completed 允许非空 event 细分
// 业务事件，缺省回落 completed。
func eventNameOf(status NodeStatus, result map[string]any) string {
	switch status {
	case NodeFailed:
		return EventFailed
	case NodeBlocked:
		return EventBlocked
	case NodeCompleted:
		if result != nil {
			if s, ok := result["event"].(string); ok && s != "" {
				return s
			}
		}
		return EventCompleted
	}
	return string(status)
}

// valueAtPath 按 $.field[.subfield] 路径从 Result 取值（仅穿越 object，
// 不下钻数组——与 C1 校验的路径形态一致）。
func valueAtPath(result map[string]any, path string) (any, bool) {
	if !strings.HasPrefix(path, "$.") {
		return nil, false
	}
	cur := any(result)
	for _, seg := range strings.Split(path[2:], ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// scalarEqual 比较 Result 取值与条件 value（JSON 标量）。两侧都经 JSON
// 规范化，消除 Go 侧 int 与 JSON float64 的数字类型差。
func scalarEqual(v any, raw json.RawMessage) bool {
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		return false
	}
	return reflect.DeepEqual(normalizeJSONValue(v), want)
}

// inStringList 判定 s 是否在条件 value（字符串数组）中。
func inStringList(s string, raw json.RawMessage) bool {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return false
	}
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// normalizeJSONValue 经 JSON 往返规范化一个 Go 值（int → float64 等）。
func normalizeJSONValue(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return out
}

// summarizeResult 生成 result_ref 的 Result 摘要（JSON 序列化并截断）。
func summarizeResult(result map[string]any) string {
	if len(result) == 0 {
		return ""
	}
	data, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	const maxLen = 240
	if len(data) > maxLen {
		return string(data[:maxLen]) + "…（已截断）"
	}
	return string(data)
}

// summarizeBounded 生成数据流绑定的有界摘要：JSON 序列化后按 rune 截断
// （上限 InputSummaryMaxRunes），供 EdgeInput.Summary / 任务注入使用。
func summarizeBounded(result map[string]any, maxRunes int) string {
	if len(result) == 0 {
		return ""
	}
	data, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	r := []rune(string(data))
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…（已截断）"
	}
	return string(r)
}

// newEdgeInput 为一条生效转移构造持久化输入绑定：完整 Result 不超过
// InputInlineMaxBytes 时内联，否则只携带摘要与证据引用（Truncated）。
// 返回指针永不为 nil——空结果也显式绑定（"绑定过"与"历史记录无字段"
// 由 nil 区分）。
func newEdgeInput(result map[string]any, evidence []EvidenceEntry) *EdgeInput {
	return newEdgeInputWithRef(result, "", evidence)
}

func newEdgeInputWithRef(result map[string]any, resultRef string, evidence []EvidenceEntry) *EdgeInput {
	if result == nil {
		result = map[string]any{}
	}
	in := &EdgeInput{
		Summary:   summarizeBounded(result, InputSummaryMaxRunes),
		ResultRef: resultRef,
		Evidence:  append([]EvidenceEntry(nil), evidence...),
	}
	for _, e := range evidence {
		if e.Ref != "" && !containsString(in.EvidenceRefs, e.Ref) {
			in.EvidenceRefs = append(in.EvidenceRefs, e.Ref)
		}
	}
	if raw, err := json.Marshal(result); err == nil && len(raw) <= InputInlineMaxBytes {
		in.Result = json.RawMessage(append([]byte(nil), raw...))
	} else {
		in.Truncated = true
	}
	return in
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func appendEvidenceUnique(dst []EvidenceEntry, entries ...EvidenceEntry) []EvidenceEntry {
	seen := make(map[string]struct{}, len(dst)+len(entries))
	for _, evidence := range dst {
		if evidence.Ref != "" {
			seen[evidence.Ref] = struct{}{}
		}
	}
	for _, evidence := range entries {
		if evidence.Ref == "" {
			continue
		}
		if _, exists := seen[evidence.Ref]; exists {
			continue
		}
		seen[evidence.Ref] = struct{}{}
		dst = append(dst, evidence)
	}
	return dst
}

// hydrateExecutionEvidence 把输入绑定中的完整 EvidenceEntry 聚合进本
// activation 的输出谱系。EvidenceRefs 兼容历史只引用记录，但没有对应
// EvidenceEntry 的引用仍视为不可解引用，acceptance 不会采信。
func hydrateExecutionEvidence(exec *Execution) {
	if exec == nil {
		return
	}
	for _, in := range exec.Input {
		exec.Evidence = appendEvidenceUnique(exec.Evidence, in.Evidence...)
		for _, ref := range in.EvidenceRefs {
			if ref != "" && !containsString(exec.EvidenceRefs, ref) {
				exec.EvidenceRefs = append(exec.EvidenceRefs, ref)
			}
		}
	}
	for _, evidence := range exec.Evidence {
		if evidence.Ref != "" && !containsString(exec.EvidenceRefs, evidence.Ref) {
			exec.EvidenceRefs = append(exec.EvidenceRefs, evidence.Ref)
		}
	}
}

func (rt *Runtime) resolveTransitionResult(graphID string, rec TransitionRecord) (map[string]any, error) {
	if rec.Input != nil && rec.Input.ResultRef != "" {
		stored, ok := rt.store.ResolveActivationResult(graphID, rec.Input.ResultRef)
		if !ok {
			return nil, fmt.Errorf("ResultRef %s 不可解引用", rec.Input.ResultRef)
		}
		if stored.NodeID != rec.SourceNodeID || stored.ActivationID != rec.SourceActivationID {
			return nil, fmt.Errorf("ResultRef %s 来源为 %s/%s，与边来源 %s/%s 不一致",
				rec.Input.ResultRef, stored.NodeID, stored.ActivationID, rec.SourceNodeID, rec.SourceActivationID)
		}
		var result map[string]any
		if err := json.Unmarshal(stored.Result, &result); err != nil || result == nil {
			return nil, fmt.Errorf("ResultRef %s 的完整 Result 非 JSON 对象: %v", rec.Input.ResultRef, err)
		}
		return result, nil
	}
	if rec.Input != nil && len(rec.Input.Result) > 0 {
		var result map[string]any
		if err := json.Unmarshal(rec.Input.Result, &result); err != nil || result == nil {
			return nil, fmt.Errorf("边 %s -> %s 的内联 Result 非 JSON 对象: %v", rec.SourceActivationID, rec.TargetNodeID, err)
		}
		return result, nil
	}
	return nil, fmt.Errorf("边 %s -> %s 没有可解引用 ResultRef 或完整内联 Result", rec.SourceActivationID, rec.TargetNodeID)
}

func (rt *Runtime) resolveExecutionInput(graphID string, exec Execution) (map[string]any, error) {
	if len(exec.Input) == 0 {
		return map[string]any{}, nil
	}
	if len(exec.Input) == 1 {
		in := exec.Input[0]
		rec := TransitionRecord{
			SourceNodeID: in.SourceNodeID, SourceActivationID: in.SourceActivationID,
			TargetActivationID: exec.ActivationID,
			Input:              &EdgeInput{ResultRef: in.ResultRef, Result: in.Result},
		}
		return rt.resolveTransitionResult(graphID, rec)
	}
	merged := make(map[string]any, len(exec.Input))
	for _, in := range exec.Input {
		rec := TransitionRecord{
			SourceNodeID: in.SourceNodeID, SourceActivationID: in.SourceActivationID,
			TargetActivationID: exec.ActivationID,
			Input:              &EdgeInput{ResultRef: in.ResultRef, Result: in.Result},
		}
		value, err := rt.resolveTransitionResult(graphID, rec)
		if err != nil {
			return nil, err
		}
		key := in.TargetInput
		if key == "" {
			key = in.SourceNodeID
		}
		merged[key] = value
	}
	return merged, nil
}

// inputsFor 推导一个 activation 的持久化输入绑定集：全部指向
// (nodeID, activationID) 的已生效 TransitionRecord，按
// (SourceNodeID, SourceActivationID, TransitionID) 升序输出。历史记录
// 无 Input 字段时只保留来源标识。activation 创建时快照进 Execution.Input，
// 恢复路径从同一 durable 记录重建，二者同源。
func (rt *Runtime) inputsFor(graphID, nodeID, activationID string) []InputBinding {
	var out []InputBinding
	for _, rec := range rt.store.Transitions(graphID) {
		if rec.TargetNodeID != nodeID || rec.TargetActivationID != activationID {
			continue
		}
		b := InputBinding{
			SourceNodeID: rec.SourceNodeID, SourceActivationID: rec.SourceActivationID,
			TargetInput: rec.TargetInput,
		}
		if rec.Input != nil {
			b.Summary = rec.Input.Summary
			b.ResultRef = rec.Input.ResultRef
			b.Evidence = append([]EvidenceEntry(nil), rec.Input.Evidence...)
			b.EvidenceRefs = append([]string(nil), rec.Input.EvidenceRefs...)
			if len(rec.Input.Result) > 0 {
				b.Result = append(json.RawMessage(nil), rec.Input.Result...)
			}
			b.Truncated = rec.Input.Truncated
			b.WorkLog = rec.Input.WorkLog
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourceNodeID != out[j].SourceNodeID {
			return out[i].SourceNodeID < out[j].SourceNodeID
		}
		return out[i].SourceActivationID < out[j].SourceActivationID
	})
	return out
}

func cloneInputBindings(inputs []InputBinding) []InputBinding {
	out := make([]InputBinding, 0, len(inputs))
	for _, input := range inputs {
		cloned := input
		cloned.Evidence = append([]EvidenceEntry(nil), input.Evidence...)
		cloned.EvidenceRefs = append([]string(nil), input.EvidenceRefs...)
		cloned.Result = append(json.RawMessage(nil), input.Result...)
		out = append(out, cloned)
	}
	return out
}

func mergeInputBindings(base, extra []InputBinding) []InputBinding {
	out := cloneInputBindings(base)
	seen := make(map[string]struct{}, len(out)+len(extra))
	keyOf := func(input InputBinding) string {
		return input.SourceNodeID + "\x00" + input.SourceActivationID + "\x00" + input.TargetInput
	}
	for _, input := range out {
		seen[keyOf(input)] = struct{}{}
	}
	for _, input := range cloneInputBindings(extra) {
		if _, exists := seen[keyOf(input)]; exists {
			continue
		}
		seen[keyOf(input)] = struct{}{}
		out = append(out, input)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TargetInput != out[j].TargetInput {
			return out[i].TargetInput < out[j].TargetInput
		}
		if out[i].SourceNodeID != out[j].SourceNodeID {
			return out[i].SourceNodeID < out[j].SourceNodeID
		}
		return out[i].SourceActivationID < out[j].SourceActivationID
	})
	return out
}

// ============================================================
// 紧急保险丝（只防御单次调用内的同步机械节点死循环，不可配置）
// ============================================================

// EmergencySynchronousStepFuse 是一次外部 Runtime 调用内允许的同步机械
// activation 数。它不跨 OnTaskTerminal 调用累计，因此长 /goal 中 Agent
// 回边可以产生任意多次 activation；只拦截 router/join/end/tool 不让出控制
// 权的程序性自旋，防止递归耗尽栈。
const EmergencySynchronousStepFuse = 128

func (rt *Runtime) consumeSynchronousStepLocked(graphID, nodeID, activationID string) error {
	rt.synchronousSteps++
	if rt.synchronousSteps <= EmergencySynchronousStepFuse {
		return nil
	}
	detail := fmt.Sprintf("一次 Runtime 调用内同步机械节点 activation 超过紧急上限 %d（当前 %s/%s）；判定为程序性同步死循环，Graph 已持久化 failed",
		EmergencySynchronousStepFuse, nodeID, activationID)
	log.Printf("[graph] ERROR 图 %s %s", graphID, detail)
	// 先 durable 终结 Graph，再发通用 graph-change wake。已有 transition 即使
	// 指向未物化 activation，也因 Graph terminal 不会在重启后继续自转。
	failErr := rt.failGraph(graphID, detail)
	rt.wakeGraphChange(TerminalFact{
		GraphID: graphID, NodeID: nodeID, ActivationID: activationID,
	}, "synchronous_activation_fuse", detail)
	return errors.Join(failErr, fmt.Errorf("graph: %s", detail))
}

// ============================================================
// 内部辅助（调用方须持 rt.mu）
// ============================================================

// activationFor 决定本次进入使用的 execution：节点已带 durable activation
// （ready/running/waiting 的恢复路径）则原样沿用；否则分配新序号并冻结当前
// revision 的节点定义。定义快照与 activation 的首次 ready 状态同条落盘。
func (rt *Runtime) activationFor(graphID, nodeID string, node Node, phase string) (Execution, error) {
	if node.Execution != nil && node.Execution.ActivationID != "" {
		switch node.Status {
		case NodeReady, NodeRunning, NodeWaiting:
			exec := *node.Execution
			hydrateExecutionEvidence(&exec)
			return exec, nil
		}
	}
	id := rt.pendingTargetActivationID(graphID, nodeID, node)
	if id == "" {
		var err error
		id, err = rt.store.NextActivationID(graphID, nodeID)
		if err != nil {
			return Execution{}, err
		}
	}
	doc, err := rt.graph(graphID)
	if err != nil {
		return Execution{}, err
	}
	exec := Execution{
		Phase:              phase,
		ActivationID:       id,
		DefinitionRevision: doc.Revision,
		Definition:         definitionFromNode(node),
	}
	// 数据流：activation 创建即快照其持久化输入绑定（与边选择记录同源，
	// 恢复重建结果一致）。
	exec.Input = rt.inputsFor(graphID, nodeID, id)
	hydrateExecutionEvidence(&exec)
	if node.Kind == KindWaitEvent && node.Wait != nil && node.Wait.TimeoutSec > 0 {
		deadline := time.Now().UTC().Add(time.Duration(node.Wait.TimeoutSec) * time.Second)
		exec.WaitDeadline = &deadline
	}
	return exec, nil
}

// freezePatchedActivationsLocked 为旧 journal/测试构造出的、尚未携带定义快照
// 的在途 activation 补一次 durable 快照。新 activation 在 activationFor 已
// 原生携带；该兼容路径保证首次 Runtime.PatchGraph 不会让旧在途节点改用新
// 定义。调用方持 rt.mu 且已核对 base revision。
func (rt *Runtime) freezePatchedActivationsLocked(graphID string, patch DefinitionPatch) error {
	if len(patch.UpsertNodes) == 0 {
		return nil
	}
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	for _, up := range patch.UpsertNodes {
		node, ok := doc.Nodes[up.ID]
		if !ok || node.Execution == nil || node.Execution.Definition != nil {
			continue
		}
		switch node.Status {
		case NodeReady, NodeRunning, NodeWaiting:
			exec := *node.Execution
			exec.DefinitionRevision = doc.Revision
			exec.Definition = definitionFromNode(node)
			if err := rt.store.SetExecution(graphID, up.ID, exec, doc.StateVersion); err != nil {
				return err
			}
			doc, err = rt.graph(graphID)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func definitionFromNode(node Node) *NodeDefinition {
	return &NodeDefinition{
		Kind:                node.Kind,
		Task:                node.Task,
		Capability:          node.Capability,
		Next:                node.Next,
		Wait:                node.Wait,
		Tool:                node.Tool,
		Subgraph:            node.Subgraph,
		EndOutcome:          node.EndOutcome,
		OutputContract:      node.OutputContract,
		ProgressContractRef: node.ProgressContractRef,
		ContextPolicyRef:    node.ContextPolicyRef,
		Metadata:            node.Metadata,
		Extensions:          node.Extensions,
	}
}

// nodeForExecution 把 activation 冻结定义覆盖到节点运行壳上。旧持久化记录
// 没有 Definition 时回落当前定义，保持向后兼容。
func nodeForExecution(node Node, exec Execution) Node {
	if exec.Definition == nil {
		return node
	}
	def := exec.Definition
	node.Kind = def.Kind
	node.Task = def.Task
	node.Capability = def.Capability
	node.Next = def.Next
	node.Wait = def.Wait
	node.Tool = def.Tool
	node.Subgraph = def.Subgraph
	node.EndOutcome = def.EndOutcome
	node.OutputContract = def.OutputContract
	node.ProgressContractRef = def.ProgressContractRef
	node.ContextPolicyRef = def.ContextPolicyRef
	node.Metadata = def.Metadata
	node.Extensions = def.Extensions
	return node
}

// enterRunning 把同步节点（router/end）推进到 running：新 activation 先经
// ready（durable activation 事实 + node_activation_created 事件），恢复
// 路径从 ready 直接补进 running；已 running 则不动。
func (rt *Runtime) enterRunning(graphID, nodeID string, node Node, exec Execution) error {
	switch node.Status {
	case NodeInactive, NodeCompleted, NodeFailed, NodeBlocked:
		if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
			return err
		}
		rt.emitActivationCreated(graphID, nodeID, exec)
		return rt.writeNode(graphID, nodeID, exec, NodeRunning)
	case NodeReady:
		return rt.writeNode(graphID, nodeID, exec, NodeRunning)
	case NodeRunning:
		return nil
	default:
		return fmt.Errorf("graph: 图 %s 节点 %s 状态 %q 不能进入 running", graphID, nodeID, node.Status)
	}
}

// emitActivationCreated 发 node_activation_created 事件（activation 事实
// 已 durable 之后）。
func (rt *Runtime) emitActivationCreated(graphID, nodeID string, exec Execution) {
	trace.Emit(trace.Event{
		Kind:         trace.KindNodeActivationCreated,
		GraphID:      graphID,
		NodeID:       nodeID,
		ActivationID: exec.ActivationID,
		Description:  "phase=" + exec.Phase,
	})
}

// writeNode 以最新 state_version 原子写 execution + 节点状态。
func (rt *Runtime) writeNode(graphID, nodeID string, exec Execution, to NodeStatus) error {
	sv, err := rt.stateVersion(graphID)
	if err != nil {
		return err
	}
	return rt.store.SetExecutionAndStatus(graphID, nodeID, exec, to, sv)
}

// failGraph 把图置 failed（带中文原因，graph_ended 事件的 Reason 载原因）。
// 若该图是 subgraph 物化子图，终态回调父图结算父节点。
func (rt *Runtime) failGraph(graphID, reason string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	descendantCleanupErr, err := rt.cancelDescendantGraphsLocked(graphID,
		fmt.Sprintf("ancestor Graph %s failed: %s", graphID, reason))
	if err != nil {
		return errors.Join(descendantCleanupErr, err)
	}
	if err := rt.cancelUnfinishedNodesLocked(graphID, "Graph failed: "+reason); err != nil {
		return errors.Join(descendantCleanupErr, err)
	}
	doc, err = rt.graph(graphID)
	if err != nil {
		return err
	}
	record := GraphOutcomeRecord{
		Outcome: EndFailed, Source: "runtime_failure", Reason: reason,
		DefinitionRevision: doc.Revision, CommittedAt: time.Now().UTC(),
	}
	if err := rt.store.CommitGraphOutcome(graphID, record, doc.StateVersion); err != nil {
		return err
	}
	rt.cancelGraphWaitTimersLocked(graphID)
	cleanupErr := errors.Join(descendantCleanupErr, rt.terminateGraphTasksLocked(graphID))
	trace.Emit(trace.Event{Kind: trace.KindGraphEnded, GraphID: graphID, GraphOutcome: string(EndFailed), Reason: reason})
	delete(rt.results, graphID)
	return errors.Join(cleanupErr, rt.onChildGraphEnded(graphID, GraphFailed))
}

// commitEndOutcome 将 end activation 的业务 outcome、GraphStatus 和 graph_ended
// 投影收敛在同一条 durable outcome 事实之后。Caller 必须持 rt.mu。
func (rt *Runtime) commitEndOutcome(graphID, nodeID string, exec Execution, outcome EndOutcome, reason string) error {
	if !outcome.IsValid() {
		return fmt.Errorf("graph: 非法 EndOutcome %q", outcome)
	}
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		if doc.Outcome != nil && doc.Outcome.Outcome == outcome {
			return nil
		}
		return fmt.Errorf("graph: 图 %s 已以 status=%s 终结，无法提交 outcome=%s", graphID, doc.Status, outcome)
	}
	if reason == "" && outcome != EndSuccess {
		reason = "Graph end outcome=" + string(outcome)
	}
	descendantCleanupErr, err := rt.cancelDescendantGraphsLocked(graphID,
		fmt.Sprintf("ancestor Graph %s outcome=%s", graphID, outcome))
	if err != nil {
		return errors.Join(descendantCleanupErr, err)
	}
	if err := rt.cancelUnfinishedNodesLocked(graphID, "Graph outcome: "+string(outcome)); err != nil {
		return errors.Join(descendantCleanupErr, err)
	}
	doc, err = rt.graph(graphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[nodeID]
	if !ok || node.Execution == nil || node.Execution.ActivationID != exec.ActivationID {
		return fmt.Errorf("graph: end 节点 %s/%s 缺少匹配的 durable execution", graphID, nodeID)
	}
	// writeTerminalContinuationWithOutcomeLocked 按值接收 exec；首次收官时
	// caller 手里的副本尚未携带刚落盘的 ResultRef/Settlement。outcome 必须
	// 从 Store 当前 activation 快照取证，不能把空引用写进 durable record。
	exec = *node.Execution
	record := GraphOutcomeRecord{
		Outcome: outcome, Source: "end", EndNodeID: nodeID,
		EndActivationID: exec.ActivationID, ResultRef: exec.ResultRef,
		Reason: reason, DefinitionRevision: exec.DefinitionRevision,
		CommittedAt: time.Now().UTC(),
	}
	if err := rt.store.CommitGraphOutcome(graphID, record, doc.StateVersion); err != nil {
		return err
	}
	rt.cancelGraphWaitTimersLocked(graphID)
	cleanupErr := errors.Join(descendantCleanupErr, rt.terminateGraphTasksLocked(graphID))
	trace.Emit(trace.Event{
		Kind: trace.KindGraphEnded, GraphID: graphID, GraphOutcome: string(outcome), Reason: reason,
	})
	delete(rt.results, graphID)
	return errors.Join(cleanupErr, rt.onChildGraphEnded(graphID, record.Status()))
}

// completeGraph 把图置 completed 并发 graph_ended 事件。
// 若该图是 subgraph 物化子图，终态回调父图结算父节点。
func (rt *Runtime) completeGraph(graphID string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		return nil
	}
	descendantCleanupErr, err := rt.cancelDescendantGraphsLocked(graphID,
		fmt.Sprintf("ancestor Graph %s completed", graphID))
	if err != nil {
		return errors.Join(descendantCleanupErr, err)
	}
	if err := rt.cancelUnfinishedNodesLocked(graphID, "Graph completed"); err != nil {
		return errors.Join(descendantCleanupErr, err)
	}
	doc, err = rt.graph(graphID)
	if err != nil {
		return err
	}
	// 只服务旧 settlement continuation 的恢复兼容；新旧 GraphDocument 的
	// end 激活均走 commitEndOutcome（空 end_outcome 按 success）。
	record := GraphOutcomeRecord{
		Outcome: EndSuccess, Source: "legacy_end",
		DefinitionRevision: doc.Revision, CommittedAt: time.Now().UTC(),
	}
	if err := rt.store.CommitGraphOutcome(graphID, record, doc.StateVersion); err != nil {
		return err
	}
	rt.cancelGraphWaitTimersLocked(graphID)
	cleanupErr := errors.Join(descendantCleanupErr, rt.terminateGraphTasksLocked(graphID))
	trace.Emit(trace.Event{Kind: trace.KindGraphEnded, GraphID: graphID, GraphOutcome: string(EndSuccess)})
	delete(rt.results, graphID)
	return errors.Join(cleanupErr, rt.onChildGraphEnded(graphID, GraphCompleted))
}

// cancelUnfinishedNodesLocked durably closes every activation that cannot
// continue once the Graph has decided its terminal outcome. This happens
// before graph status + graph_ended, so every observer sees a coherent
// terminal document rather than a terminal Graph containing running siblings.
// Caller must hold rt.mu.
func (rt *Runtime) cancelUnfinishedNodesLocked(graphID, reason string) error {
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	for _, nodeID := range sortedNodeIDs(doc) {
		current, err := rt.graph(graphID)
		if err != nil {
			return err
		}
		node := current.Nodes[nodeID]
		// blocked is a settled diagnostic/replan fact even though the node state
		// machine permits blocked -> ready. Preserve it when another branch/end
		// closes the Graph; only genuinely unfinished work is cancelled.
		if node.Status.IsTerminal() || node.Status == NodeBlocked {
			continue
		}
		if node.Execution == nil || strings.TrimSpace(node.Execution.ActivationID) == "" {
			if err := rt.store.SetNodeStatus(graphID, nodeID, NodeCancelled, current.StateVersion); err != nil {
				return fmt.Errorf("graph: 收官时取消未激活节点 %s/%s: %w", graphID, nodeID, err)
			}
			continue
		}
		result := map[string]any{"status": "cancelled", "reason": reason}
		if err := rt.writeTerminalContinuationLocked(graphID, nodeID, *node.Execution, NodeCancelled,
			result, SettlementContinueNone, reason); err != nil {
			return fmt.Errorf("graph: 收官时取消在途节点 %s/%s: %w", graphID, nodeID, err)
		}
	}
	return nil
}

// cancelDescendantGraphsLocked terminates every materialized child tree while
// leaving graphID itself untouched. It is used by completeGraph/failGraph
// before the parent outcome is committed. Caller must hold rt.mu.
func (rt *Runtime) cancelDescendantGraphsLocked(graphID, reason string) (cleanupErr error, err error) {
	doc, err := rt.graph(graphID)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{graphID: {}}
	var cleanupErrs []error
	for _, childID := range materializedChildGraphIDs(doc) {
		if _, exists := rt.store.Get(childID); !exists {
			// The child ID is durable before child submission. A crash in that
			// narrow window legitimately leaves no child Graph to cancel.
			continue
		}
		_, childCleanupErr, childErr := rt.cancelGraphTreeLocked(childID, reason, false, seen)
		if childCleanupErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("graph: 收官时清理子图 %s 任务: %w", childID, childCleanupErr))
		}
		if childErr != nil {
			return errors.Join(cleanupErrs...), fmt.Errorf("graph: 收官时取消子图 %s: %w", childID, childErr)
		}
	}
	return errors.Join(cleanupErrs...), nil
}

// cancelGraphTreeLocked is the post-order implementation shared by the public
// recovery/control-plane API and parent complete/fail teardown. newlyCancelled
// reports whether graphID itself crossed into GraphCancelled during this call.
// Caller must hold rt.mu.
func (rt *Runtime) cancelGraphTreeLocked(graphID, reason string, notifyParent bool, seen map[string]struct{}) (newlyCancelled bool, cleanupErr error, err error) {
	if _, ok := seen[graphID]; ok {
		return false, nil, nil
	}
	seen[graphID] = struct{}{}

	doc, err := rt.graph(graphID)
	if err != nil {
		return false, nil, err
	}
	var cleanupErrs []error
	for _, childID := range materializedChildGraphIDs(doc) {
		if _, exists := rt.store.Get(childID); !exists {
			continue
		}
		_, childCleanupErr, childErr := rt.cancelGraphTreeLocked(childID, reason, false, seen)
		if childCleanupErr != nil {
			cleanupErrs = append(cleanupErrs, childCleanupErr)
		}
		if childErr != nil {
			return false, errors.Join(cleanupErrs...), childErr
		}
	}

	// Repair older terminal documents as well: before sibling teardown existed,
	// a terminal Graph could retain ready/running/waiting nodes.
	if err := rt.cancelUnfinishedNodesLocked(graphID, reason); err != nil {
		return false, errors.Join(cleanupErrs...), err
	}
	doc, err = rt.graph(graphID)
	if err != nil {
		return false, errors.Join(cleanupErrs...), err
	}
	if doc.Status.IsTerminal() {
		cleanupErrs = append(cleanupErrs, rt.terminateGraphTasksLocked(graphID))
		return false, errors.Join(cleanupErrs...), nil
	}
	record := GraphOutcomeRecord{
		Outcome: EndCancelled, Source: "control_plane", Reason: reason,
		DefinitionRevision: doc.Revision, CommittedAt: time.Now().UTC(),
	}
	if err := rt.store.CommitGraphOutcome(graphID, record, doc.StateVersion); err != nil {
		return false, errors.Join(cleanupErrs...), err
	}
	rt.cancelGraphWaitTimersLocked(graphID)
	cleanupErrs = append(cleanupErrs, rt.terminateGraphTasksLocked(graphID))
	trace.Emit(trace.Event{Kind: trace.KindGraphEnded, GraphID: graphID, GraphOutcome: string(EndCancelled), Reason: reason})
	delete(rt.results, graphID)
	var parentErr error
	if notifyParent {
		parentErr = rt.onChildGraphEnded(graphID, GraphCancelled)
	}
	return true, errors.Join(cleanupErrs...), parentErr
}

// terminateGraphTasksLocked closes TaskBoard work owned by a terminal Graph.
// Caller holds rt.mu and the Graph status is already durable. The operation is
// deliberately best-effort across the GraphStore/TaskStore boundary: failure
// cannot undo the Graph decision, but it is made explicit before graph_ended
// and is retried by bootstrap recovery on the next start.
func (rt *Runtime) terminateGraphTasksLocked(graphID string) error {
	terminator, ok := rt.board.(GraphTaskTerminator)
	if !ok || terminator == nil {
		return nil
	}
	if err := terminator.TerminateGraphTasks(graphID); err != nil {
		log.Printf("[graph] ERROR: 终态图 %s 清理公告板任务失败（已持久化的 Graph 终态不回滚）: %v", graphID, err)
		trace.Emit(trace.Event{
			Kind: trace.KindError, GraphID: graphID,
			Error: fmt.Sprintf("终态图清理公告板任务失败（启动恢复将重试）: %v", err),
		})
		return err
	}
	return nil
}

func materializedChildGraphIDs(doc *GraphDocument) []string {
	if doc == nil {
		return nil
	}
	set := make(map[string]struct{})
	for _, node := range doc.Nodes {
		if node.Execution == nil {
			continue
		}
		if childID := strings.TrimSpace(node.Execution.ChildGraphID); childID != "" {
			set[childID] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for childID := range set {
		out = append(out, childID)
	}
	sort.Strings(out)
	return out
}

// graph 读图（不存在返回 ErrGraphNotFound 包装错误）。
func (rt *Runtime) graph(graphID string) (*GraphDocument, error) {
	doc, ok := rt.store.Get(graphID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrGraphNotFound, graphID)
	}
	return doc, nil
}

// stateVersion 读图当前 state_version。
func (rt *Runtime) stateVersion(graphID string) (int64, error) {
	doc, err := rt.graph(graphID)
	if err != nil {
		return 0, err
	}
	return doc.StateVersion, nil
}

// phaseForKind 给任务型节点决定 execution.phase。
func phaseForKind(kind NodeKind) string {
	switch kind {
	case KindController:
		return "planning"
	case KindAcceptance:
		return "verifying"
	}
	return "executing"
}

// activationOf 取节点当前 activation_id（无则返回 "<none>"，仅供日志）。
func activationOf(node Node) string {
	if node.Execution == nil || node.Execution.ActivationID == "" {
		return "<none>"
	}
	return node.Execution.ActivationID
}

// truncateDigest 截断 digest 供事件摘要展示。
func truncateDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
