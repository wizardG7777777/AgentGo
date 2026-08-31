package graph

// 本文件是 acceptance 节点终态结算的谱系核验（数据流时代的验收判定矩阵，
// 取代 G1b 的机器格式契约核验——command/file_hash/task_status 逐字比对已随
// 旧证据契约退役，详见 docs/design/scheduler-prompt-and-acceptance-redesign.md §4）：
//
// 判定矩阵（仅 acceptance 节点 completed 终态时进入；failed/blocked 无
// verdict 可采信，走统一结算）：
//   - verifier 经 submit_task_result 的 cited_evidence 参数引用自己实际消费
//     过的证据（EvidenceRef 或该 Evidence 携带的 typed CheckRef，逗号分隔）；服务端只做**谱系核验**：每个引用
//     必须属于本 acceptance activation 的上游 Input 谱系（各 InputBinding.
//     EvidenceRefs 的并集），或属于 verifier 自己本次任务产生的证据
//     （Execution.Evidence）——越谱系引用即造假实锤；
//   - valid（全部引用在谱系内，或未引用）：按自报 verdict 正常结算；
//   - disputed（任一引用越谱系）：不采信自报 verdict——节点终态 failed
//     （Result 不保留原始业务路由字段，自报结论移入 disputed_verdict，
//     越谱系引用记入 disputed_citations，verdict 驱动的边条件不会命中），
//     发 graph_change_requested 审计事件 + graph change 唤醒（注入 waker
//     时），绝不按自报 verdict 放行；
//   - 不引用证据不是过错：verdict 正常采信（无「unverifiable 判死」——
//     旧保守默认随证据格式契约一并退役）。
//
// 与 G1b 的本质差异：核验对象是「引用是否来自本次验收真实可见的数据」，
// 而不是「自报文本是否与账本尊逐字一致」——身份由系统按调用或内容事实
// 签发为不透明稳定引用，不依赖查询序数或 LLM 的格式纪律。
//
// GraphChangeWaker：disputed 时的「graph change 唤醒」最小依赖接口——
// 不采信自报 verdict 后由实现方按既有 graph change 机制（审计事件 +
// __scheduler__ 唤醒任务）交 Scheduler 裁决。

import (
	"context"
	"fmt"
	"log"
	"strings"

	"agentgo/internal/trace"
)

// 核验结论（trace AcceptancePayload.Status 的取值）。
const (
	// AcceptValid：全部引用在谱系内（或无引用），按自报 verdict 正常转移。
	AcceptValid = "valid"
	// AcceptDisputed：至少一个引用越出谱系，不采信自报 verdict。
	AcceptDisputed = "disputed"
	// AcceptEvidenceMissing：节点契约要求的输入证据缺失或不可解引用；即使
	// verifier 自报 pass 也按 blocked 结算并唤醒图变更。
	AcceptEvidenceMissing = "evidence_missing"
	// AcceptInvalidVerdict：completed acceptance 缺少合法 verdict，或 event
	// 字段依然存在。协议错误不允许经无条件/completed 边伪装通过。
	AcceptInvalidVerdict = "invalid_verdict"
)

// GraphChangeWaker 是 Graph Runtime 对「graph change 唤醒」能力的最小依赖
// （可选注入）。acceptance 谱系核验 disputed 时调用——实现方按既有 graph
// change 机制发布 __scheduler__ 唤醒任务（幂等键 <graphID>/<activationID>/change），
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
	Reason       string // 结构化原因码（acceptance_disputed / contract_no_outlet）
	Detail       string // 人类可读的原因摘要（不含证据正文）
	// MarkerKind 区分唤醒任务的幂等标记（<graphID>/<activationID>/<kind>）：
	// 空串 = "change"（request_replan 图路径与 acceptance 谱系核验共用查重）；
	// WakeMarkerNoOutlet = 终态契约 v2 两击升级的独立标记；
	// WakeMarkerWritebackFailed = SWE-002 终态回填失败回落的独立标记。
	MarkerKind string
}

// WakeMarkerNoOutlet 是终态契约 v2 两击升级唤醒的幂等标记种类：
// [graph-change-request: <graphID>/<activationID>/no-outlet]。
const WakeMarkerNoOutlet = "no-outlet"

// WakeMarkerWritebackFailed 是终态回填失败回落唤醒的幂等标记种类（SWE-002
// 第三层防线）：[graph-change-request: <graphID>/<activationID>/writeback-failed]。
// 与 change / no-outlet 标记互不查重，同一 activation 可同时挂多种唤醒。
const WakeMarkerWritebackFailed = "writeback-failed"

// SetChangeWaker 注入 graph change 唤醒器（构造后、使用前调用）。
func (rt *Runtime) SetChangeWaker(w GraphChangeWaker) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.changeWaker = w
}

// settleAcceptanceLocked 是 acceptance 节点 completed 终态的谱系核验结算
// 路径（调用方须持 rt.mu；exec 已携带本任务的 Evidence，见 OnTaskTerminal）：
//  1. 从 Result 取 verdict 与 cited_evidence（逗号分隔的 EvidenceRef 清单）；
//  2. 谱系核验：引用 ∈ 上游 Input 谱系 ∪ 本任务自身证据 → valid；
//     任一越谱系 → disputed；
//  3. 无论结论如何先发 acceptance_completed 审计事件（不含证据正文）；
//  4. valid 按原状结算；disputed 不采信 verdict——Result 不保留原始
//     业务路由字段（防 $.verdict 边条件命中未核验结论），
//     节点置 failed，发 graph change 唤醒后走统一结算。
func (rt *Runtime) settleAcceptanceLocked(f TerminalFact, exec Execution) error {
	verdict, verdictErr := validateAcceptanceVerdictResult(f.Result)
	if verdictErr != "" {
		reason := fmt.Sprintf("验收节点 %s（activation %s）提交的 completed 结果违反 verdict 契约：%s",
			f.NodeID, f.ActivationID, verdictErr)
		trace.Emit(trace.Event{
			Kind: trace.KindAcceptanceCompleted, GraphID: f.GraphID, NodeID: f.NodeID,
			ActivationID: f.ActivationID, TaskID: f.TaskID,
			Acceptance: &trace.AcceptancePayload{
				Verdict: verdict, Status: AcceptInvalidVerdict, Checked: 0, Reason: reason,
			},
		})
		err := rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeFailed, map[string]any{
			"error": reason, "invalid_verdict": verdict,
			"verify_status": AcceptInvalidVerdict,
		})
		rt.wakeGraphChange(f, "acceptance_invalid_verdict", reason)
		return err
	}
	doc, err := rt.graph(f.GraphID)
	if err != nil {
		return err
	}
	node, ok := doc.Nodes[f.NodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, f.GraphID, f.NodeID)
	}
	node = nodeForExecution(node, exec)
	var requirements []EvidenceRequirement
	if node.Task != nil {
		requirements = node.Task.RequiredEvidence
	}
	missing := rt.missingEvidenceRequirements(f.GraphID, exec, requirements)
	if len(missing) > 0 {
		detail := formatEvidenceRequirements(missing)
		reason := "验收必需证据缺失或不可解引用: " + detail
		trace.Emit(trace.Event{
			Kind: trace.KindAcceptanceCompleted, GraphID: f.GraphID, NodeID: f.NodeID,
			ActivationID: f.ActivationID, TaskID: f.TaskID,
			Acceptance: &trace.AcceptancePayload{
				Verdict: verdict, Status: AcceptEvidenceMissing, Checked: 0, Reason: reason,
			},
		})
		err := rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeBlocked, map[string]any{
			"error": reason, "blocked_reason": reason,
			"missing_evidence": detail, "disputed_verdict": verdict,
			"verify_status": AcceptEvidenceMissing,
		})
		rt.wakeGraphChange(f, "acceptance_evidence_missing", reason)
		return err
	}
	cited := splitCitedEvidence(f.Result["cited_evidence"])
	allowed := rt.acceptanceEvidenceLineage(f.GraphID, exec)
	var outOfLineage []string
	for _, ref := range cited {
		if _, ok := allowed[ref]; !ok {
			outOfLineage = append(outOfLineage, ref)
		}
	}

	status := AcceptValid
	reason := ""
	if len(outOfLineage) > 0 {
		status = AcceptDisputed
		reason = fmt.Sprintf("引用证据越出本 activation 的输入谱系（不属于上游绑定证据，也不属于本任务自身证据）：%s",
			strings.Join(outOfLineage, ", "))
	}
	trace.Emit(trace.Event{
		Kind:         trace.KindAcceptanceCompleted,
		GraphID:      f.GraphID,
		NodeID:       f.NodeID,
		ActivationID: f.ActivationID,
		TaskID:       f.TaskID,
		Acceptance: &trace.AcceptancePayload{
			Verdict: verdict,
			Status:  status,
			Checked: len(cited),
			Reason:  reason,
		},
	})

	if status == AcceptValid {
		if doc.RequiresDelivery() && verdict == "pass" {
			if strings.TrimSpace(f.DeliveryRef) == "" || rt.deliveryCommitter == nil {
				reason := "Graph v3 acceptance pass 缺少 Delivery Transaction 或 L5 commit 协调器"
				return rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeBlocked, map[string]any{
					"blocked_reason": reason, "reason_code": "delivery_commit_unavailable",
				})
			}
			acceptanceOutcomeRef, _ := f.Result["_task_outcome_ref"].(string)
			commitRef, commitErr := rt.deliveryCommitter.CommitDelivery(context.Background(), f.DeliveryRef, acceptanceOutcomeRef)
			if commitErr != nil || strings.TrimSpace(commitRef) == "" {
				reason := "Delivery candidate promotion 未确认："
				if commitErr != nil {
					reason += commitErr.Error()
				} else {
					reason += "commit ref 为空"
				}
				return rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeBlocked, map[string]any{
					"blocked_reason": reason, "reason_code": "delivery_commit_unknown",
				})
			}
			exec.DeliveryRef = f.DeliveryRef
			exec.DeliveryCommitRef = commitRef
			f.Result = copyAcceptanceResult(f.Result)
			f.Result["_delivery_commit_ref"] = commitRef
		}
		return rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeCompleted, f.Result)
	}

	// disputed：不采信自报 verdict。Result 只保留核验事实，不保留
	// 原始业务路由字段（否则 $.verdict 边条件会把越谱系结论当作
	// 路由输入）；原结论留 disputed_verdict 供审计。
	failReason := fmt.Sprintf("验收节点 %s（activation %s）谱系核验 disputed：%s（自报 verdict=%q 不采信）",
		f.NodeID, f.ActivationID, reason, verdict)
	result := map[string]any{
		"error":              failReason,
		"disputed_verdict":   verdict,
		"disputed_citations": strings.Join(outOfLineage, ","),
		"verify_status":      AcceptDisputed,
	}
	err = rt.settleNodeLocked(f.GraphID, f.NodeID, exec, NodeFailed, result)
	rt.wakeGraphChange(f, "acceptance_disputed", reason)
	return err
}

// validateAcceptanceVerdictResult 校验 acceptance 的 completed 输出协议。
// verdict 必须是 prompt 契约枚举 pass/fixable/failed。验收业务
// 结论只能经 $.verdict 路由；completed 结果出现 event 一律视为
// 协议错误，避免同一结论同时有两个权威字段。
func copyAcceptanceResult(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func validateAcceptanceVerdictResult(result map[string]any) (string, string) {
	raw, exists := result["verdict"]
	if !exists {
		return "", "缺少必填字符串字段 verdict"
	}
	verdict, ok := raw.(string)
	if !ok || verdict == "" || strings.TrimSpace(verdict) != verdict || !isValidAcceptanceVerdict(verdict) {
		return verdict, fmt.Sprintf("verdict 必须是 pass/fixable/failed 之一，实际为 %q", verdict)
	}
	if _, hasEvent := result["event"]; hasEvent {
		return verdict, "completed acceptance 结果不得携带 event；业务结论只能经 $.verdict 路由"
	}
	return verdict, ""
}

func isValidAcceptanceVerdict(verdict string) bool {
	switch verdict {
	case "pass", "fixable", "failed":
		return true
	default:
		return false
	}
}

// acceptanceEvidenceLineage 计算 acceptance activation 的合法证据谱系：
// 上游 Input 各绑定的 EvidenceRefs 及其 typed CheckRef 别名并集（经数据流
// 到达的上游证据），加上本 activation 自身任务产生的同类证据。CheckRef
// 只有在完整 EvidenceEntry 经 Result Store 解引用后才加入，不能靠猜中 ID 授权。
func (rt *Runtime) acceptanceEvidenceLineage(graphID string, exec Execution) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, in := range exec.Input {
		for _, evidence := range rt.resolvableInputEvidence(graphID, in) {
			allowed[evidence.Ref] = struct{}{}
			if evidence.CheckRef != "" {
				allowed[evidence.CheckRef] = struct{}{}
			}
		}
	}
	for _, e := range exec.Evidence {
		allowed[e.Ref] = struct{}{}
		if e.CheckRef != "" {
			allowed[e.CheckRef] = struct{}{}
		}
	}
	return allowed
}

// resolvableInputEvidence 返回同时存在于 InputBinding 与其 ResultRef 指向的
// activation 记录中的 EvidenceEntry。只有 Ref 字符串、没有结构化条目或
// ResultRef 不可解引用时返回空，防止「猜中 ID」冒充消费过证据。
func (rt *Runtime) resolvableInputEvidence(graphID string, in InputBinding) []EvidenceEntry {
	if in.ResultRef == "" || len(in.Evidence) == 0 {
		return nil
	}
	stored, ok := rt.store.ResolveActivationResult(graphID, in.ResultRef)
	if !ok || stored.NodeID != in.SourceNodeID || stored.ActivationID != in.SourceActivationID {
		return nil
	}
	byRef := make(map[string]EvidenceEntry, len(stored.Evidence))
	for _, evidence := range stored.Evidence {
		if evidence.Ref != "" && evidence.Kind != "" {
			byRef[evidence.Ref] = evidence
		}
	}
	var out []EvidenceEntry
	for _, evidence := range in.Evidence {
		storedEvidence, exists := byRef[evidence.Ref]
		if !exists || storedEvidence.Kind != evidence.Kind || evidence.Kind == "" {
			continue
		}
		out = appendEvidenceUnique(out, storedEvidence)
	}
	return out
}

func (rt *Runtime) missingEvidenceRequirements(graphID string, exec Execution, requirements []EvidenceRequirement) []EvidenceRequirement {
	var missing []EvidenceRequirement
	for _, requirement := range requirements {
		found := false
		for _, in := range exec.Input {
			if in.TargetInput != requirement.Input {
				continue
			}
			for _, evidence := range rt.resolvableInputEvidence(graphID, in) {
				if evidence.Kind == requirement.Kind {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			missing = append(missing, requirement)
		}
	}
	return missing
}

func formatEvidenceRequirements(requirements []EvidenceRequirement) string {
	parts := make([]string, 0, len(requirements))
	for _, requirement := range requirements {
		parts = append(parts, fmt.Sprintf("input=%s kind=%s", requirement.Input, requirement.Kind))
	}
	return strings.Join(parts, ", ")
}

// splitCitedEvidence 解析 cited_evidence 提交值（逗号分隔的 EvidenceRef
// 清单；兼容 []any 形态），去空白、去空项。
func splitCitedEvidence(raw any) []string {
	var items []string
	switch v := raw.(type) {
	case string:
		items = strings.Split(v, ",")
	case []string:
		items = v
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok {
				items = append(items, s)
			}
		}
	}
	var out []string
	for _, it := range items {
		if s := strings.TrimSpace(it); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// wakeGraphChange 发 graph change 唤醒：先 emit graph_change_requested 审计
// 事件，再经注入的 waker 发布 __scheduler__ 唤醒任务。waker 未注入或发布
// 失败只记日志——节点 failed 终态不因此推翻（与 reactor 错误「仅记日志，
// 绝不中断主流程」同一纪律）。
func (rt *Runtime) wakeGraphChange(f TerminalFact, reasonCode, detail string) {
	rt.wakeGraphChangeKind(f, reasonCode, detail, "")
}

// wakeGraphChangeKind 是 wakeGraphChange 的带幂等标记种类变体：markerKind
// 为空沿用默认 "change" 标记；终态契约 v2 两击升级使用 WakeMarkerNoOutlet。
func (rt *Runtime) wakeGraphChangeKind(f TerminalFact, reasonCode, detail, markerKind string) {
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
		TaskID: f.TaskID, Reason: reasonCode, Detail: detail, MarkerKind: markerKind,
	}); err != nil {
		log.Printf("[graph] ERROR 图 %s 节点 %s graph change 唤醒任务发布失败: %v",
			f.GraphID, f.NodeID, err)
	}
}
