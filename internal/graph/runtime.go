package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

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
//   - join：就绪性纯推导（不加持久状态）——全部入边源的最新 activation
//     终态且 ≥1 条入边已 RecordTransition 生效时归并完成；全部源终态但
//     无入边生效时置 skipped（终态，不触发 next）；
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
//     prompt 契约检查后经 submit_task_result 写 Results（verdict/event），
//     终态经既有 feed 回填 TerminalFact。G1b 起注入 AcceptanceVerifier
//     时引擎在结算前对 Results["evidence"] 做服务端核验（见
//     acceptance.go）：valid 按 verdict 正常转移，disputed/unverifiable
//     不采信 verdict（节点 failed + graph change 唤醒）；未注入保持
//     C5c 契约自报行为。Plan 时代的熔断器仍未迁移，连续 disputed 靠
//     Scheduler 经 graph change 流裁决。
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

// GraphTaskSnapshot 是恢复期从 TaskBoard 查询到的最小任务事实。非终态任务
// 的 TerminalStatus 为空；终态只允许 completed/failed/blocked。Result 是
// TaskStore 持有的完整结构化结果，用于在 Graph journal 落后时补结算。
type GraphTaskSnapshot struct {
	TaskID         string
	TerminalStatus NodeStatus
	Result         map[string]any
}

// TaskSpec 是一次节点任务发布的完整描述。
type TaskSpec struct {
	GraphID      string
	NodeID       string
	ActivationID string
	Title        string
	Description  string
	Route        string // 认领路由（resolveRoute 解析结果 = runner event_type）
	Tools        []string
	Model        string
	Isolation    string
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

	// acceptVerifier 是 acceptance 节点 completed 终态的服务端核验器
	//（G1b，acceptance.go）：nil 时保持 C5c 契约自报行为（不核验）。
	// changeWaker 是核验 disputed/unverifiable 时的 graph change 唤醒器：
	// nil 时只发 graph_change_requested 审计事件，不发布唤醒任务。
	acceptVerifier AcceptanceVerifier
	changeWaker    GraphChangeWaker

	// results 是节点最新 Result 的纯内存缓存（graphID → nodeID → Result），
	// 供 join 归并取已生效源的结果本体。Result 本体尚未持久化（已知限制，
	// 随 Result 持久化切片解决），重启后缓存丢失，join 归并值退化为源的
	// result_ref 摘要字符串。图到终态即 eviction（join 只在图运行中就绪）。
	results map[string]map[string]map[string]any
	// waitTimers 只是进程内唤醒器；权威 deadline 存在 Execution.WaitDeadline
	// 并随 activation durable。恢复时按该 deadline 重建 timer 或立即超时。
	waitTimers map[string]*time.Timer

	mu sync.Mutex
}

// NewRuntime 创建 Graph Runtime。board 可为 nil——仅当图真正需要发布任务
// （controller/agent 节点激活）时才报错；纯 router/end 图可离板运行。
// ToolExecutor / ApprovalGateway 经 SetToolExecutor / SetApprovalGateway
// 可选注入（保持本签名兼容）。
func NewRuntime(store *Store, board TaskBoard) *Runtime {
	return &Runtime{
		store:      store,
		board:      board,
		results:    make(map[string]map[string]map[string]any),
		waitTimers: make(map[string]*time.Timer),
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

// SubmitGraph 校验并提交一张图（durable），随后激活 root。
// 校验/落盘失败时发 graph_submission_rejected 事件并原样返回错误；
// root 激活失败（如未实现节点类型）时图已提交，错误一并返回。
func (rt *Runtime) SubmitGraph(doc *GraphDocument) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
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

// OnTaskTerminal 以任务终态事实驱动转移求值（V6 §6-15/17）。
//
// 处理顺序：校验事实对应当前在途 activation（过期 activation 的迟到事件
// 忽略并记 debug）→ durable 写 result_ref 与节点终态 → 按 next 顺序求值
// （先查幂等记录再 RecordTransition，durable 后激活目标）→ 无匹配出路时
// 图置 failed（带原因）。
func (rt *Runtime) OnTaskTerminal(f TerminalFact) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	doc, err := rt.graph(f.GraphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		log.Printf("[graph] DEBUG 图 %s 已是终态 %q，忽略迟到的终态事实（节点 %s activation %s）",
			f.GraphID, doc.Status, f.NodeID, f.ActivationID)
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

	// G1b：acceptance 节点 completed 且已注入核验器时走服务端核验结算
	//（valid 放行 / disputed·unverifiable 不采信自报 verdict，见
	// acceptance.go）。failed/blocked 终态无 verdict 可采信，不核验。
	if activeNode.Kind == KindAcceptance && f.Status == NodeCompleted && rt.acceptVerifier != nil {
		return rt.settleAcceptanceLocked(f, *ex)
	}

	// durable 写 Result 摘要 + 节点终态，随后走统一结算路径（转移求值 +
	// 目标激活 + join 就绪重推导）。
	return rt.settleNodeLocked(f.GraphID, f.NodeID, *ex, f.Status, f.Result)
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
	doc, err := rt.graph(graphID)
	if err != nil {
		return err
	}
	if doc.Status.IsTerminal() {
		log.Printf("[graph] DEBUG 图 %s 已是终态 %q，忽略外部事件 %q", graphID, doc.Status, event)
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
func (rt *Runtime) OnApprovalDecided(graphID, nodeID, activationID string, approved bool, text string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
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
	if result == nil {
		result = map[string]any{}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("graph: 序列化图 %s 节点 %s activation %s 的终态 Result: %w", graphID, nodeID, exec.ActivationID, err)
	}
	if len(raw) > MaxDocumentBytes {
		return fmt.Errorf("graph: 图 %s 节点 %s activation %s 的终态 Result 为 %d 字节，超过 durable settlement 上限 %d；拒绝落终态以避免写入不可恢复的超大 journal",
			graphID, nodeID, exec.ActivationID, len(raw), MaxDocumentBytes)
	}
	// 经 JSON 往返得到与 durable 字节一致、且不再受调用方后续修改影响的缓存。
	var durableResult map[string]any
	if err := json.Unmarshal(raw, &durableResult); err != nil || durableResult == nil {
		return fmt.Errorf("graph: 图 %s 节点 %s activation %s 的终态 Result 不是 JSON 对象", graphID, nodeID, exec.ActivationID)
	}
	exec.ResultRef = summarizeResult(durableResult)
	exec.Settlement = &TerminalSettlement{
		Status: status, Result: append(json.RawMessage(nil), raw...),
		Continuation: continuation, Reason: reason,
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
		rec, err := rt.newTransitionRecordLocked(graphID, nodeID, activationID, i, tr.To)
		if err != nil {
			return err
		}
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
		if err := rt.activateRecordedTransitionLocked(graphID, rec, result); err != nil {
			errs = append(errs, err)
		}
	}
	// 结算可能让某些 join 的最后一个入边源进入终态：重推导全图 join 就绪性
	// （覆盖「最后结算的源没有指向 join 的生效边」的就绪与 skipped 两种情形）。
	errs = append(errs, rt.evaluateJoinsLocked(graphID))
	return errors.Join(errs...)
}

// newTransitionRecordLocked 在边选择落盘前冻结它指向的 activation。目标已有
// 在途 activation 时加入该 activation；否则复用尚未物化的 durable 预留，
// 或预留下一个新 ID。TargetActivationID 与边选择同条 journal 记录落盘。
func (rt *Runtime) newTransitionRecordLocked(graphID, sourceNodeID, sourceActivationID string, transitionID int, targetNodeID string) (TransitionRecord, error) {
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
		TargetActivationID: targetActivationID,
	}, nil
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
	return rt.activateLocked(graphID, rec.TargetNodeID, input)
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
//     上游 Result 本体不可恢复，router 目标仅 always/已记录边可确定性重放
//     （已知限制，随 Result 持久化切片解决）。
func (rt *Runtime) ResumeGraph(graphID string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.resumeGraphLocked(graphID)
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
	return errors.Join(errs...)
}

// resumeTerminalSettlementLocked 用 terminal 节点同条 durable 记录中的完整
// Result 与 continuation 补完崩溃窗口。旧 journal 没有 Settlement 时不猜；
// 新记录的所有动作都可幂等重放。
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
	var result map[string]any
	if err := json.Unmarshal(settlement.Result, &result); err != nil || result == nil {
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
	case SettlementContinueGraphFail:
		return rt.failGraph(graphID, settlement.Reason)
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
		return rt.activateTaskNode(graphID, nodeID, node)
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
func (rt *Runtime) activateTaskNode(graphID, nodeID string, node Node) error {
	exec, err := rt.activationFor(graphID, nodeID, node, phaseForKind(node.Kind))
	if err != nil {
		return err
	}
	if err := rt.writeNode(graphID, nodeID, exec, NodeReady); err != nil {
		return err
	}
	rt.emitActivationCreated(graphID, nodeID, exec)
	return rt.publishTask(graphID, nodeID, node, exec)
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
	node = nodeForExecution(node, exec)
	if err := rt.enterRunning(graphID, nodeID, node, exec); err != nil {
		return err
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
	node = nodeForExecution(node, exec)
	if err := rt.enterRunning(graphID, nodeID, node, exec); err != nil {
		return err
	}
	if err := rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeCompleted,
		input, SettlementContinueGraphComplete, ""); err != nil {
		return err
	}
	return rt.completeGraph(graphID)
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
	event := EventCompleted
	status := NodeCompleted
	if childStatus != GraphCompleted {
		event = EventFailed
		status = NodeFailed
	}
	result := map[string]any{
		"event":          event,
		"child_graph_id": childID,
		"child_result":   rt.childResultSummary(childID),
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

// childResultSummary 取子图结果摘要：优先子图 end 节点的 result_ref
// （最后一条转移的输入摘要），找不到时退化为子图终态串。
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
		if activeNode.Kind == KindEnd && n.Execution != nil && n.Execution.ResultRef != "" {
			return n.Execution.ResultRef
		}
	}
	return string(child.Status)
}

// ============================================================
// join 节点
// ============================================================

// evaluateJoinLocked 评估单个 join 节点的就绪性（readiness 纯推导，不加
// 持久状态）：
//   - 全部入边源节点（定义期静态集合）的最新 activation 均已终态，且 ≥1 条
//     入边已 RecordTransition 生效 → 归并完成（Result 以源节点 ID 为键
//     合并各已生效源的结果）并求值转移；
//   - 全部源已终态但无入边生效 → 置 skipped（终态，不触发 next；仅从未
//     激活过的 inactive join 可置 skipped）；
//   - 否则保持现状（其它源终态后由结算路径重推导）。
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
	allTerminal, fired, merged := rt.joinReadiness(graphID, doc, joinID, sources)
	if !allTerminal {
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
	trace.Emit(trace.Event{
		Kind:         trace.KindGraphJoinResolved,
		GraphID:      graphID,
		NodeID:       joinID,
		ActivationID: exec.ActivationID,
		Description:  fmt.Sprintf("生效入边 %d/%d", len(fired), len(sources)),
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

// joinReadiness 推导 join 的就绪输入：全部入边源的最新 activation 是否均
// 已终态、哪些源有生效入边（已 RecordTransition）、归并的 Result
// （源节点 ID → 结果本体；结果本体未持久化，内存缓存缺失时退化为源的
// result_ref 摘要字符串，随 Result 持久化切片解决）。
func (rt *Runtime) joinReadiness(graphID string, doc *GraphDocument, joinID string, sources []string) (allTerminal bool, fired []string, merged map[string]any) {
	records := rt.store.Transitions(graphID)
	for _, sid := range sources {
		if !doc.Nodes[sid].Status.IsTerminal() {
			return false, nil, nil
		}
	}
	merged = make(map[string]any, len(sources))
	for _, sid := range sources {
		sn := doc.Nodes[sid]
		if sn.Execution == nil || sn.Execution.ActivationID == "" {
			continue
		}
		sourceFired := false
		for _, rec := range records {
			if rec.SourceActivationID == sn.Execution.ActivationID && rec.TargetNodeID == joinID {
				sourceFired = true
				break
			}
		}
		if !sourceFired {
			continue
		}
		fired = append(fired, sid)
		if live, ok := rt.results[graphID][sid]; ok {
			merged[sid] = live
		} else {
			merged[sid] = sn.Execution.ResultRef
		}
	}
	return true, fired, merged
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

// publishTask 发布节点任务：成功则 durable task_id 并置 running；失败则
// 节点标记 failed、图置 failed 并返回中文错误。恢复路径以同一 activation
// 补发（TaskBoard 幂等键去重）。
func (rt *Runtime) publishTask(graphID, nodeID string, node Node, exec Execution) error {
	node = nodeForExecution(node, exec)
	spec := taskSpecFor(graphID, nodeID, node, exec)
	if rt.board == nil {
		reason := fmt.Sprintf("节点 %s（activation %s）需要发布任务但 TaskBoard 未配置", nodeID, exec.ActivationID)
		return errors.Join(
			rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed,
				map[string]any{"error": reason}, SettlementContinueGraphFail, reason),
			rt.failGraph(graphID, reason),
			fmt.Errorf("graph: %s", reason),
		)
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

func taskSpecFor(graphID, nodeID string, node Node, exec Execution) TaskSpec {
	spec := TaskSpec{
		GraphID:      graphID,
		NodeID:       nodeID,
		ActivationID: exec.ActivationID,
		Route:        resolveRoute(node),
	}
	if node.Task != nil {
		spec.Title = node.Task.Title
		spec.Description = node.Task.Description
	}
	if c := node.Capability; c != nil {
		spec.Tools = c.Tools
		spec.Model = c.Model
		spec.Isolation = c.Isolation
	}
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
		taskID, err := rt.board.PublishGraphTask(taskSpecFor(graphID, nodeID, node, exec))
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
	}
	if node.Status == NodeReady {
		if err := rt.writeNode(graphID, nodeID, exec, NodeRunning); err != nil {
			return err
		}
	}
	if node.Kind == KindAcceptance && fact.Status == NodeCompleted && rt.acceptVerifier != nil {
		return rt.settleAcceptanceLocked(fact, exec)
	}
	return rt.settleNodeLocked(graphID, nodeID, exec, fact.Status, fact.Result)
}

// ============================================================
// 转移条件求值
// ============================================================

// evalCondition 求值一条转移条件。when 缺省恒真；事件形态先取
// Result["event"]（非空字符串），缺省回落终态映射
// （completed/failed/blocked），"always" 恒真；条件形态对 Result 按
// $.field[.subfield] 路径取值后应用 eq/ne/in/exists。
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

// eventNameOf 求事件形态的当前事件名：Result["event"]（非空字符串）优先，
// 缺省回落节点终态映射（completed/failed/blocked）。
func eventNameOf(status NodeStatus, result map[string]any) string {
	if result != nil {
		if s, ok := result["event"].(string); ok && s != "" {
			return s
		}
	}
	switch status {
	case NodeCompleted:
		return EventCompleted
	case NodeFailed:
		return EventFailed
	case NodeBlocked:
		return EventBlocked
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
			return *node.Execution, nil
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
		Kind:       node.Kind,
		Task:       node.Task,
		Capability: node.Capability,
		Next:       node.Next,
		Wait:       node.Wait,
		Tool:       node.Tool,
		Subgraph:   node.Subgraph,
		Metadata:   node.Metadata,
		Extensions: node.Extensions,
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
	if err := rt.store.SetGraphStatus(graphID, GraphFailed, doc.StateVersion); err != nil {
		return err
	}
	rt.cancelGraphWaitTimersLocked(graphID)
	trace.Emit(trace.Event{Kind: trace.KindGraphEnded, GraphID: graphID, Reason: reason})
	delete(rt.results, graphID)
	return rt.onChildGraphEnded(graphID, GraphFailed)
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
	if err := rt.store.SetGraphStatus(graphID, GraphCompleted, doc.StateVersion); err != nil {
		return err
	}
	rt.cancelGraphWaitTimersLocked(graphID)
	trace.Emit(trace.Event{Kind: trace.KindGraphEnded, GraphID: graphID})
	delete(rt.results, graphID)
	return rt.onChildGraphEnded(graphID, GraphCompleted)
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
