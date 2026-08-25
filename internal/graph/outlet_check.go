package graph

// 本文件实现终态契约 v2（docs/design/graph-terminal-contract-v2.md §5/§6）的
// 提交期出路匹配检查与两击升级协议：
//
//   - 判定时机：submit_task_result 终态落盘前，由工具 handler 调用
//     CheckActivationOutlet，用该 activation 冻结定义的出边对「status 镜像
//     事件 + result 数据」预求值（与 settle 时 evalTransitionsLocked 同源，
//     边条件对同一冻结定义、同一形态 Result 求值，结果一致）；
//   - 第一击（自愈）：无匹配 → 拒绝提交（任务不 finalizing），返回结构化中文
//     错误：缺哪个字段、当前值、合法值域（从出边 path 条件派生）、result
//     示例形态；违例计数按 activation 持久化在 Execution.OutletCheck；
//   - 第二击（升级）：同一 activation 第 2 次仍无匹配 → 节点标 failed
//     （原因 contract_no_outlet，摘要含两次提交的有界截断值），发布幂等
//     no-outlet 唤醒任务请 Scheduler 裁决，返回不可重试的终态错误；
//   - v1 图一律不介入（CheckActivationOutlet 返回 nil）：v1 无匹配仍由
//     终态回填时 fail-closed，语义逐字节不变。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// OutletError 是提交期出路检查的结构化拒绝错误。Strikes=1 为首击（可修正
// 重交）；Escalated=true 为第二击已升级（节点已 failed 并唤醒 Scheduler
// 裁决），提交不可重试。Detail 是回执给 Agent 的结构化中文说明。
type OutletError struct {
	GraphID      string
	NodeID       string
	ActivationID string
	Strikes      int
	Escalated    bool
	Detail       string
}

func (e *OutletError) Error() string { return e.Detail }

// GraphSchema 返回图的 schema 版本；图不存在返回空串（调用方按非 v2 处理，
// 即不做提交期检查，保持引入前行为）。
func (rt *Runtime) GraphSchema(graphID string) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	doc, ok := rt.store.Get(graphID)
	if !ok {
		return ""
	}
	return doc.Schema
}

// CheckActivationOutlet 对属于 schema v2 图的任务做提交期出路匹配检查：
// 用该 activation 冻结定义的出边对 status（镜像系统事件）与 result（业务
// 数据）预求值；任一匹配即放行（返回 nil）。无匹配时按两击协议处理：
// 首击持久化计数并返回可重交的结构化错误；第二击把节点置 failed
// （contract_no_outlet）并发布 no-outlet 唤醒任务，返回不可重试的终态错误。
//
// 结构性强约束（图不存在、activation 不在途、节点非 running、status 非法）
// 返回普通错误，不计入两击。v1 图返回 nil（不介入）。
func (rt *Runtime) CheckActivationOutlet(graphID, nodeID, activationID string, status string, result map[string]any) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	doc, ok := rt.store.Get(graphID)
	if !ok {
		return fmt.Errorf("graph: 出路检查失败：图 %s 不存在", graphID)
	}
	if doc.Schema != SchemaV2 {
		return nil // v1 图不介入：无匹配出路仍由终态回填时 fail-closed（语义不变）
	}
	if doc.Status.IsTerminal() {
		return fmt.Errorf("graph: 图 %s 已是终态 %q，activation %s 的提交不会被采信", graphID, doc.Status, activationID)
	}
	node, ok := doc.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
	}
	ex := node.Execution
	if ex == nil || ex.ActivationID == "" || ex.ActivationID != activationID {
		return fmt.Errorf("graph: 图 %s 节点 %s 当前在途 activation 为 %q，activation %s 的提交不会被采信",
			graphID, nodeID, activationOf(node), activationID)
	}
	// 幂等终态拒绝：第二击已升级的 activation 不再重复计数/升级/唤醒。
	if ex.OutletCheck != nil && ex.OutletCheck.Escalated {
		return &OutletError{
			GraphID: graphID, NodeID: nodeID, ActivationID: activationID,
			Strikes: ex.OutletCheck.Strikes, Escalated: true,
			Detail: fmt.Sprintf("终态契约 v2：activation %s 已因两次提交均无匹配出路升级 Scheduler 裁决（contract_no_outlet），节点已 failed；提交不会被采信，请停止调用任何工具。", activationID),
		}
	}
	if node.Status != NodeRunning {
		return fmt.Errorf("graph: 图 %s 节点 %s 状态 %q 非 running，activation %s 的提交不会被采信",
			graphID, nodeID, node.Status, activationID)
	}
	var nodeStatus NodeStatus
	switch NodeStatus(status) {
	case NodeCompleted, NodeFailed, NodeBlocked:
		nodeStatus = NodeStatus(status)
	default:
		return fmt.Errorf("graph: 出路检查的 status %q 非法（仅 completed/failed/blocked）", status)
	}

	// 与 settle 同源的预求值：冻结定义出边 + 同一 evalCondition。
	activeNode := nodeForExecution(node, *ex)
	if err := rt.validateRecoveryRetryContract(graphID, doc, activeNode, activationID, nodeStatus, result); err != nil {
		return err
	}
	for _, tr := range activeNode.Next {
		if evalCondition(tr.When, nodeStatus, result) {
			return nil
		}
	}

	// 无匹配出路：进入两击协议。计数持久化在 Execution.OutletCheck（随
	// execution_status journal 记录 durable，activation 重进时归零）。
	submission := summarizeSubmission(nodeStatus, result)
	strikes := 1
	first := submission
	if ex.OutletCheck != nil && ex.OutletCheck.Strikes > 0 {
		strikes = ex.OutletCheck.Strikes + 1
		if ex.OutletCheck.FirstSubmission != "" {
			first = ex.OutletCheck.FirstSubmission
		}
	}
	exec := *ex
	if strikes < 2 {
		exec.OutletCheck = &OutletCheckState{Strikes: 1, FirstSubmission: first}
		if err := rt.store.SetExecution(graphID, nodeID, exec, doc.StateVersion); err != nil {
			return fmt.Errorf("graph: 持久化首击违例计数失败: %w", err)
		}
		return &OutletError{
			GraphID: graphID, NodeID: nodeID, ActivationID: activationID, Strikes: 1,
			Detail: buildOutletFirstStrikeDetail(nodeID, activationID, activeNode, nodeStatus, result),
		}
	}

	// 第二击（升级）：节点标 failed（原因 contract_no_outlet，含两次提交
	// 摘要），OutletCheck 与终态同条 journal 记录落盘；随后按统一结算路径
	// 求值转移（failed/always 兜底边照常生效，无兜底边则图 fail-closed，
	// 与 acceptance disputed 同语义），并发布幂等 no-outlet 唤醒任务。
	exec.OutletCheck = &OutletCheckState{Strikes: 2, FirstSubmission: first, Escalated: true}
	reason := fmt.Sprintf("节点 %s（activation %s）两次提交均无匹配出路（contract_no_outlet）；第一次提交：%s；第二次提交：%s",
		nodeID, activationID, first, submission)
	failResult := map[string]any{"error": reason, "contract_no_outlet": true}
	if err := rt.writeTerminalContinuationLocked(graphID, nodeID, exec, NodeFailed, failResult, SettlementContinueTransitions, reason); err != nil {
		return fmt.Errorf("graph: 第二击节点终态落盘失败: %w", err)
	}
	evalErr := rt.evalTransitionsLocked(graphID, nodeID, activationID, NodeFailed, failResult)
	rt.wakeGraphChangeKind(TerminalFact{
		GraphID: graphID, NodeID: nodeID, ActivationID: activationID,
		TaskID: exec.TaskID, Status: NodeFailed,
	}, "contract_no_outlet", reason, WakeMarkerNoOutlet)
	return errors.Join(&OutletError{
		GraphID: graphID, NodeID: nodeID, ActivationID: activationID,
		Strikes: 2, Escalated: true,
		Detail: fmt.Sprintf("终态契约 v2 出路检查第 2 击仍未通过，提交被拒绝且不可重试：%s。节点已置 failed，并已发布 no-outlet 唤醒任务升级 Scheduler 裁决（patch_graph 补边/改道/宣布图失败）；本任务将以 failed 终态收尾，请停止调用任何工具。", reason),
	}, evalErr)
}

// summarizeSubmission 生成一次提交（status + result）的有界摘要，供两击
// 计数持久化与失败原因对账（复用 result_ref 摘要的截断口径）。
func summarizeSubmission(status NodeStatus, result map[string]any) string {
	summary := summarizeResult(result)
	if summary == "" {
		summary = "{}"
	}
	return fmt.Sprintf("status=%s result=%s", status, summary)
}

// buildOutletFirstStrikeDetail 渲染首击拒绝的结构化中文错误：缺哪个字段、
// 当前值、合法值域（从出边 path 条件派生）与 result 示例形态。
func buildOutletFirstStrikeDetail(nodeID, activationID string, node Node, status NodeStatus, result map[string]any) string {
	summary := summarizeResult(result)
	if summary == "" {
		summary = "{}"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "终态契约 v2 出路检查未通过（第 1 击，共 2 击）：status=%s、result=%s 未匹配节点 %s（activation %s）的任何出边，提交被拒绝——任务未 finalizing，可修正后重新提交。",
		status, summary, nodeID, activationID)
	b.WriteString("\n各出边条件与当前求值（满足任一即可）：")
	for i, tr := range node.Next {
		fmt.Fprintf(&b, "\n- next[%d] -> %s：%s", i, tr.To, describeOutletCondition(tr.When, status, result))
	}
	if domains := outletFieldDomains(node.Next); domains != "" {
		b.WriteString("\nresult 字段值域（从出边 path 条件派生）：" + domains)
	}
	if example := outletResultExample(node.Next); example != "" {
		b.WriteString("\nresult 示例形态：" + example)
	}
	b.WriteString("\n请修正 result 后重新调用 submit_task_result；v2 图任务不得携带 event（业务路由一律走 result 数据字段 + path 条件）。")
	return b.String()
}

// describeOutletCondition 渲染一条出边条件及其对当前提交（status + result）
// 的求值事实。
func describeOutletCondition(when *Condition, status NodeStatus, result map[string]any) string {
	if when == nil {
		return "无条件（恒匹配）"
	}
	if when.Event != "" {
		return fmt.Sprintf("event=%q（系统事件镜像：当前 status=%s 镜像事件 %q）",
			when.Event, status, eventNameOf(status, result))
	}
	cond := fmt.Sprintf("%s %s", when.Path, when.Operator)
	if when.Operator != OpExists {
		cond += " " + string(when.Value)
	}
	current, ok := valueAtPath(result, when.Path)
	currentText := "字段缺失"
	if ok {
		if raw, err := json.Marshal(current); err == nil {
			currentText = string(raw)
		}
	}
	return fmt.Sprintf("%s；当前值：%s", cond, currentText)
}

// outletFieldDomains 从出边 path 条件机械派生字段值域说明（逐边一条，
// 标注出边序号便于对账）。算子值域语义与输出契约派生共用
// conditionDomainText 一处实现（终态契约 v2 §5）。
func outletFieldDomains(next []Transition) string {
	var parts []string
	for i, tr := range next {
		when := tr.When
		if when == nil || when.Path == "" {
			continue
		}
		domain := conditionDomainText(when)
		if domain == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s（next[%d]）", when.Path, domain, i))
	}
	return strings.Join(parts, "；")
}

// conditionDomainText 渲染单条 path 条件的算子值域语义（eq/in/exists/ne）。
// 首击诊断（outletFieldDomains）与输出契约钉入（output_contract.go）共用
// 此实现；未知算子返回空串，由调用方跳过。
func conditionDomainText(when *Condition) string {
	switch when.Operator {
	case OpEq:
		return fmt.Sprintf("必须等于 %s", string(when.Value))
	case OpIn:
		return fmt.Sprintf("∈ %s", string(when.Value))
	case OpExists:
		return "必须存在"
	case OpNe:
		return fmt.Sprintf("必须不等于 %s（或字段缺失）", string(when.Value))
	}
	return ""
}

// outletResultExample 从首个可构造示例的 path 条件（优先 eq，其次 in /
// exists / ne）生成最小 result 示例形态。
func outletResultExample(next []Transition) string {
	for _, wantOp := range []string{OpEq, OpIn, OpExists, OpNe} {
		for _, tr := range next {
			when := tr.When
			if when == nil || when.Path == "" || when.Operator != wantOp {
				continue
			}
			var value any
			switch wantOp {
			case OpEq:
				if err := json.Unmarshal(when.Value, &value); err != nil {
					continue
				}
			case OpIn:
				var list []string
				if err := json.Unmarshal(when.Value, &list); err != nil || len(list) == 0 {
					continue
				}
				value = list[0]
			case OpExists:
				value = "<按出边要求填写>"
			case OpNe:
				value = fmt.Sprintf("<不等于 %s 的值>", string(when.Value))
			}
			example := buildExampleForPath(when.Path, value)
			if raw, err := json.Marshal(example); err == nil {
				return string(raw)
			}
		}
	}
	return ""
}

// buildExampleForPath 按 "$.a[.b]" 路径构造嵌套 object 示例。
func buildExampleForPath(path string, value any) map[string]any {
	root := map[string]any{}
	if !strings.HasPrefix(path, "$.") {
		return root
	}
	segs := strings.Split(path[2:], ".")
	cur := root
	for i, seg := range segs {
		if i == len(segs)-1 {
			cur[seg] = value
			break
		}
		next := map[string]any{}
		cur[seg] = next
		cur = next
	}
	return root
}
