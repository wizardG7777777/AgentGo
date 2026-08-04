package graph

// 本文件是 G1b 验收服务端核验的引擎侧契约与结算集成：
//   - EvidenceItem / VerifyOutcome / AcceptanceVerifier：acceptance 节点终态
//     结算时对 verifier 自报证据（Results["evidence"] JSON 数组）做服务端
//     核验的最小依赖接口（与 ToolExecutor / ApprovalGateway 同风格注入）；
//   - GraphChangeWaker：核验 disputed/unverifiable 时的「graph change 唤醒」
//     最小依赖接口——不采信自报 verdict 后由实现方按既有 graph change
//     机制（C5d：审计事件 + __scheduler__ 唤醒任务）交 Scheduler 裁决；
//   - Runtime.settleAcceptanceLocked：acceptance 节点 completed 终态的
//     核验结算路径（valid 放行 / disputed·unverifiable 不采信）。
//
// 行为契约（未注入 AcceptanceVerifier 时保持 C5c 现行为——verdict 契约自报，
// 引擎不做任何服务端核验）：
//   - valid：按自报 verdict 正常结算（转移求值不变）；
//   - disputed（证据核验失败）/ unverifiable（无证据、证据非法或类型未知）：
//     不采信自报 verdict——节点终态 failed（Result 不含 verdict/event 键，
//     自报结论移入 disputed_verdict，verdict 驱动的边条件不会命中），发
//     graph_change_requested 审计事件 + graph change 唤醒（注入 waker 时），
//     绝不按自报 verdict 放行。

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agentgo/internal/trace"
)

// 证据类型（EvidenceItem.Type 的合法取值）。
const (
	// EvidenceTypeCommand：value 是本次任务真实执行过的命令串（逐字，可去
	// 首尾空白）；服务端对照 Effect Journal 该任务的 shell 账核验。
	EvidenceTypeCommand = "command"
	// EvidenceTypeFileHash：value 是项目内文件路径（仅记录实际 hash）或
	// "路径=sha256"（重算比对，一致才过）。
	EvidenceTypeFileHash = "file_hash"
	// EvidenceTypeTaskStatus：value 是裸状态词（completed / failed /
	// blocked / cancelled / pass / fail），逐字命中词表才过。
	EvidenceTypeTaskStatus = "task_status"
)

// 核验结论（VerifyOutcome.Status 的合法取值）。
const (
	// VerifyValid：全部证据核验通过，按自报 verdict 正常转移。
	VerifyValid = "valid"
	// VerifyDisputed：至少一条证据核验失败，不采信自报 verdict。
	VerifyDisputed = "disputed"
	// VerifyUnverifiable：缺少可核验证据（无 evidence / JSON 非法 / 类型
	// 未知 / 核验设施不可用），保守处理同 disputed。
	VerifyUnverifiable = "unverifiable"
)

// EvidenceItem 是验收 agent 经 Results["evidence"] 自报的一条机器可核验
// 证据（JSON 数组元素）。Criterion 是判据名（人类可读，定位用）；Type 决定
// 服务端核验方式；ExpectExit 是 command 证据的可选期望退出码（缺省 0）。
type EvidenceItem struct {
	Criterion  string `json:"criterion"`
	Type       string `json:"type"`
	Value      string `json:"value"`
	ExpectExit *int   `json:"expect_exit,omitempty"`
}

// VerifyOutcome 是一次服务端核验的结论。Checked 是实际完成核验的证据条数
// （disputed/unverifiable 时可能小于总条数——首条失败即短路）。Reason 是
// 人类可读的原因摘要（disputed 时必须含「哪条证据为何失败」），不含证据正文。
type VerifyOutcome struct {
	Status  string // valid / disputed / unverifiable
	Reason  string
	Checked int
}

// AcceptanceVerifier 是 Graph Runtime 对验收证据服务端核验能力的最小依赖
// （与 TaskBoard / ToolExecutor 同风格的解耦接口）。taskID 是 acceptance
// 节点本次 activation 的任务 ID（核验 command 证据时据此查该任务的
// Effect Journal shell 账）。
//
// 实现要求：VerifyAcceptance 在 Runtime 锁内同步调用，不得回调 Runtime 的
// 任何公开方法（sync.Mutex 不可重入，会死锁）；返回 error 表示核验器自身
// 故障，引擎按 unverifiable 保守处理（不误判 valid）。
type AcceptanceVerifier interface {
	VerifyAcceptance(taskID string, verdict string, evidence []EvidenceItem) (VerifyOutcome, error)
}

// GraphChangeWaker 是 Graph Runtime 对「graph change 唤醒」能力的最小依赖
// （可选注入）。acceptance 核验 disputed/unverifiable 时调用——实现方按
// C5d 既有机制发布 __scheduler__ 唤醒任务（幂等键 <graphID>/<activationID>/change），
// 交 Scheduler 用 patch_graph 裁决。未注入时只发 graph_change_requested
// 审计事件（图仍按节点 failed 结算）。
type GraphChangeWaker interface {
	WakeGraphChange(spec GraphChangeWakeSpec) error
}

// GraphChangeWakeSpec 是一次 graph change 唤醒的完整描述。TaskID 是触发
// 唤醒的来源任务（验收任务），供唤醒任务挂 ParentTaskID。
type GraphChangeWakeSpec struct {
	GraphID      string
	NodeID       string
	ActivationID string
	TaskID       string
	Reason       string // 结构化原因码（acceptance_disputed / acceptance_unverifiable）
	Detail       string // 人类可读的原因摘要（不含证据正文）
}

// SetAcceptanceVerifier 注入 acceptance 节点的证据核验器（构造后、使用前
// 调用）。nil（未注入）时保持 C5c 现行为：verdict 契约自报，引擎不核验。
func (rt *Runtime) SetAcceptanceVerifier(v AcceptanceVerifier) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.acceptVerifier = v
}

// SetChangeWaker 注入 graph change 唤醒器（构造后、使用前调用）。
func (rt *Runtime) SetChangeWaker(w GraphChangeWaker) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.changeWaker = w
}

// settleAcceptanceLocked 是 acceptance 节点 completed 终态的核验结算路径
// （调用方须持 rt.mu；仅在已注入 AcceptanceVerifier 时由 OnTaskTerminal
// 分派进入）：
//  1. 从 Result 取 verdict 与 evidence（Results["evidence"] JSON 数组；
//     缺失/空白按无证据处理，JSON 非法直接 unverifiable，不再调核验器）；
//  2. 调核验器；核验器自身故障按 unverifiable 保守处理；
//  3. 无论结论如何先发 acceptance_completed 审计事件（不含证据正文）；
//  4. valid 按原状结算；disputed/unverifiable 不采信 verdict——Result
//     剔除自报 verdict/event（防 $.verdict / event 边条件命中未核验结论），
//     节点置 failed，发 graph change 唤醒后走统一结算。
func (rt *Runtime) settleAcceptanceLocked(f TerminalFact, exec Execution) error {
	verdict, _ := f.Result["verdict"].(string)

	var outcome VerifyOutcome
	var evidence []EvidenceItem
	evidenceRaw, _ := f.Result["evidence"].(string)
	switch {
	case strings.TrimSpace(evidenceRaw) == "":
		// 无 evidence：调核验器（由实现方给出 unverifiable 的权威措辞；
		//  nil 切片原样传递）。
		outcome = rt.callAcceptanceVerifier(f.TaskID, verdict, nil)
	case json.Unmarshal([]byte(evidenceRaw), &evidence) != nil:
		outcome = VerifyOutcome{
			Status: VerifyUnverifiable,
			Reason: "Results[\"evidence\"] 不是合法 JSON 数组，无法核验",
		}
	default:
		outcome = rt.callAcceptanceVerifier(f.TaskID, verdict, evidence)
	}

	trace.Emit(trace.Event{
		Kind:         trace.KindAcceptanceCompleted,
		GraphID:      f.GraphID,
		NodeID:       f.NodeID,
		ActivationID: f.ActivationID,
		TaskID:       f.TaskID,
		Acceptance: &trace.AcceptancePayload{
			Verdict: verdict,
			Status:  outcome.Status,
			Checked: outcome.Checked,
			Reason:  outcome.Reason,
		},
	})

	if outcome.Status == VerifyValid {
		return rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeCompleted, f.Result)
	}

	// disputed / unverifiable：不采信自报 verdict。Result 只保留核验事实——
	// 自报的 verdict/event 键剔除（否则 $.verdict / event 边条件会把未核验
	// 结论当作路由输入），原结论留 disputed_verdict 供审计。
	reason := fmt.Sprintf("验收节点 %s（activation %s）证据核验 %s：%s（自报 verdict=%q 不采信）",
		f.NodeID, f.ActivationID, outcome.Status, outcome.Reason, verdict)
	result := map[string]any{
		"error":            reason,
		"disputed_verdict": verdict,
		"verify_status":    outcome.Status,
	}
	rt.wakeGraphChange(f, "acceptance_"+outcome.Status, outcome.Reason)
	return rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeFailed, result)
}

// callAcceptanceVerifier 调核验器并把核验器故障归一为 unverifiable
// （V6 原则：核验设施不可用时绝不误判 valid）。
func (rt *Runtime) callAcceptanceVerifier(taskID, verdict string, evidence []EvidenceItem) VerifyOutcome {
	outcome, err := rt.acceptVerifier.VerifyAcceptance(taskID, verdict, evidence)
	if err != nil {
		return VerifyOutcome{Status: VerifyUnverifiable, Reason: "核验器执行失败: " + err.Error()}
	}
	return outcome
}

// wakeGraphChange 发 graph change 唤醒（C5d 既有机制）：先 emit
// graph_change_requested 审计事件，再经注入的 waker 发布 __scheduler__
// 唤醒任务。waker 未注入或发布失败只记日志——节点 failed 终态不因此推翻
// （与 reactor 错误「仅记日志，绝不中断主流程」同一纪律）。
func (rt *Runtime) wakeGraphChange(f TerminalFact, reasonCode, detail string) {
	trace.Emit(trace.Event{
		Kind: trace.KindGraphChangeRequested, TaskID: f.TaskID,
		GraphID: f.GraphID, NodeID: f.NodeID, ActivationID: f.ActivationID,
		Reason: reasonCode, Description: detail,
	})
	if rt.changeWaker == nil {
		log.Printf("[graph] DEBUG 图 %s 节点 %s 核验 %s，但未注入 GraphChangeWaker，仅记录审计事件",
			f.GraphID, f.NodeID, reasonCode)
		return
	}
	if err := rt.changeWaker.WakeGraphChange(GraphChangeWakeSpec{
		GraphID: f.GraphID, NodeID: f.NodeID, ActivationID: f.ActivationID,
		TaskID: f.TaskID, Reason: reasonCode, Detail: detail,
	}); err != nil {
		log.Printf("[graph] ERROR 图 %s 节点 %s graph change 唤醒任务发布失败: %v",
			f.GraphID, f.NodeID, err)
	}
}
