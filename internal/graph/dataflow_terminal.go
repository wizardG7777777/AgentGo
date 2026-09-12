package graph

import (
	"context"
	"fmt"
	"strings"
)

type AgentTaskTerminal struct {
	PlainText    bool            `json:"plain_text,omitempty"`
	Evidence     []EvidenceEntry `json:"evidence,omitempty"`
	TaskID       string          `json:"task_id"`
	AttemptID    string          `json:"attempt_id"`
	OutcomeRef   string          `json:"outcome_ref"`
	Status       string          `json:"status"`
	Value        map[string]any  `json:"value,omitempty"`
	CandidateRef string          `json:"candidate_ref,omitempty"`
	EvidenceRefs []string        `json:"evidence_refs,omitempty"`
	Error        string          `json:"error,omitempty"`
}

func (r *DataflowRuntime) ValidateAgentTaskResult(graphID, nodeID string, value map[string]any) error {
	s, ok, err := r.Store.Get(graphID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("图不存在")
	}
	exec, ok := s.Executions[nodeID]
	if !ok || exec.ActivationID == "" {
		return fmt.Errorf("节点没有冻结的执行")
	}
	return validateDataflowValue(exec.Definition.ResultSchema, value)
}

func (r *DataflowRuntime) RequestPlanning(graphID, nodeID, requestID, reason string) error {
	digest, _ := dataflowDigest([]string{nodeID, reason})
	_, err := r.Store.transact(graphID, func(s *DataflowSnapshot, exists bool) error {
		if !exists || s.Terminal() || s.Status == "finalizing" {
			return fmt.Errorf("图不接受规划请求")
		}
		if old, ok := s.Requests[requestID]; ok {
			if old.Action != "replan" || old.Digest != digest {
				return fmt.Errorf("规划请求身份冲突")
			}
			return nil
		}
		if _, ok := s.Executions[nodeID]; !ok {
			return fmt.Errorf("来源节点不存在")
		}
		s.Requests[requestID] = DataflowRequestReceipt{Action: "replan", Digest: digest, Revision: s.Definition.Revision}
		s.enqueuePlanning("replan_requested", nodeID, reason)
		return nil
	})
	return err
}

func dataflowTaskTerminal(status string) bool {
	switch status {
	case "completed", "failed", "blocked", "cancelled":
		return true
	}
	return false
}

// RecordTerminal 在 TaskOutcome 已持久化后接收结算，不重新执行工具或改写旧结果。
func (r *DataflowRuntime) RecordTerminal(ctx context.Context, graphID, nodeID string, fact AgentTaskTerminal) (DataflowSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return DataflowSnapshot{}, err
	}
	if !dataflowTaskTerminal(fact.Status) || fact.TaskID == "" || (fact.Status == "completed" && fact.AttemptID == "") || fact.OutcomeRef == "" {
		return DataflowSnapshot{}, fmt.Errorf("节点终态缺少身份或状态")
	}
	return r.Store.transact(graphID, func(s *DataflowSnapshot, exists bool) error {
		if !exists {
			return fmt.Errorf("图不存在")
		}
		exec, ok := s.Executions[nodeID]
		if !ok || exec.ActivationID == "" || exec.TaskID != fact.TaskID {
			return fmt.Errorf("任务终态不属于该节点执行")
		}
		if exec.OutcomeRef != "" {
			if exec.OutcomeRef != fact.OutcomeRef || exec.Status != fact.Status {
				return fmt.Errorf("节点已结算不同终态")
			}
			return nil
		}
		if s.Terminal() {
			return fmt.Errorf("图已终结，不可追加节点终态")
		}
		if fact.Status == "completed" {
			if err := validateTerminalValue(exec.Definition.ResultSchema, fact); err != nil {
				return fmt.Errorf("节点 %s 结果不满足契约: %w", nodeID, err)
			}
			value := AgentTaskResult{PlainText: fact.PlainText, Schema: AgentTaskResultSchema, RunID: s.Definition.RunID, GraphID: graphID, NodeID: nodeID, TaskID: exec.TaskID, ActivationID: exec.ActivationID, AttemptID: fact.AttemptID, Value: fact.Value, CandidateRef: fact.CandidateRef, Evidence: fact.Evidence, EvidenceRefs: fact.EvidenceRefs}
			digest, err := dataflowDigest(value)
			if err != nil {
				return err
			}
			value.Ref = "result:" + strings.TrimPrefix(digest, "sha256:")
			s.Results[nodeID] = value
		}
		exec.Status = fact.Status
		exec.AttemptID = fact.AttemptID
		exec.OutcomeRef = fact.OutcomeRef
		exec.Error = fact.Error
		exec.WaitingReason = ""
		s.Executions[nodeID] = exec
		s.enqueuePlanning("task_terminal", nodeID, fact.Status)
		settleDataflowCancellation(s)
		return nil
	})
}

type CompleteDataflowRequest struct {
	RequestID        string            `json:"request_id"`
	ExpectedRevision int64             `json:"expected_revision"`
	Outcome          string            `json:"outcome"`
	Summary          string            `json:"summary"`
	ResultRefs       []string          `json:"result_refs,omitempty"`
	CandidateRef     string            `json:"selected_candidate_ref,omitempty"`
	Dispositions     map[string]string `json:"dispositions,omitempty"`
}

func (r *DataflowRuntime) Complete(ctx context.Context, id string, request CompleteDataflowRequest) (DataflowSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return DataflowSnapshot{}, err
	}
	if err := dataflowIdentity("request_id", request.RequestID); err != nil {
		return DataflowSnapshot{}, err
	}
	if request.Outcome != "success" && request.Outcome != "failed" && request.Outcome != "blocked" {
		return DataflowSnapshot{}, fmt.Errorf("完成 outcome 必须为 success/failed/blocked")
	}
	if strings.TrimSpace(request.Summary) == "" {
		return DataflowSnapshot{}, fmt.Errorf("完成请求缺少结论")
	}
	digest, err := dataflowDigest(request)
	if err != nil {
		return DataflowSnapshot{}, err
	}
	state, err := r.Store.transact(id, func(s *DataflowSnapshot, exists bool) error {
		if !exists {
			return fmt.Errorf("图不存在")
		}
		if s.Completion != nil {
			if s.Completion.RequestID == request.RequestID && s.Completion.Digest == digest {
				return nil
			}
			return fmt.Errorf("图已有其它完成请求")
		}
		if _, ok := s.Requests[request.RequestID]; ok {
			return fmt.Errorf("request_id 已用于其它图操作")
		}
		if s.Status != "open" || s.Definition.Revision != request.ExpectedRevision {
			return fmt.Errorf("完成请求的图状态或版本不符")
		}
		for _, node := range s.Definition.Nodes {
			exec := s.Executions[node.NodeID]
			if !dataflowTaskTerminal(exec.Status) {
				return fmt.Errorf("节点 %s 尚未结算，不能静默结束", node.NodeID)
			}
			if exec.OutcomeRef == "" {
				return fmt.Errorf("节点 %s 缺少结算权威", node.NodeID)
			}
			if request.Outcome == "success" && exec.Status != "completed" && strings.TrimSpace(request.Dispositions[node.NodeID]) == "" {
				return fmt.Errorf("未成功节点 %s 缺少明确处置说明", node.NodeID)
			}
		}
		for nodeID, reason := range request.Dispositions {
			if _, ok := s.Executions[nodeID]; !ok || strings.TrimSpace(reason) == "" {
				return fmt.Errorf("处置引用 %s 无效", nodeID)
			}
		}
		if request.Outcome == "success" && len(request.ResultRefs) == 0 {
			return fmt.Errorf("成功必须选择实际结果")
		}
		results := map[string]AgentTaskResult{}
		for _, value := range s.Results {
			results[value.Ref] = value
		}
		candidates := map[string]bool{}
		seen := map[string]bool{}
		for _, ref := range request.ResultRefs {
			value, ok := results[ref]
			if !ok || seen[ref] {
				return fmt.Errorf("结果引用 %s 不存在或重复", ref)
			}
			seen[ref] = true
			if value.CandidateRef != "" {
				candidates[value.CandidateRef] = true
			}
		}
		candidate := request.CandidateRef
		if candidate != "" && !candidates[candidate] {
			return fmt.Errorf("选定候选不属于交付结果")
		}
		if candidate == "" {
			if len(candidates) > 1 {
				return fmt.Errorf("多个候选需要明确选择最终版本")
			}
			for ref := range candidates {
				candidate = ref
			}
		}
		if request.Outcome != "success" {
			candidate = ""
		}
		if candidate != "" && r.Delivery == nil {
			return fmt.Errorf("候选交付服务未装配")
		}
		if candidate != "" {
			if err := r.Delivery.ValidateCandidate(ctx, id, s.Definition.RunID, candidate); err != nil {
				return err
			}
		}
		s.Completion = &DataflowCompletion{Schema: CompletionSchema, RequestID: request.RequestID, Digest: digest, Outcome: request.Outcome, Summary: request.Summary, ResultRefs: request.ResultRefs, CandidateRef: candidate, Dispositions: request.Dispositions, Status: "prepared"}
		s.Status = "finalizing"
		return nil
	})
	if err != nil {
		return state, err
	}
	if state.Terminal() {
		return state, nil
	}
	return r.finishCompletion(ctx, id)
}

func (r *DataflowRuntime) finishCompletion(ctx context.Context, id string) (DataflowSnapshot, error) {
	lock := r.stepLock(id)
	lock.Lock()
	defer lock.Unlock()
	state, ok, err := r.Store.Get(id)
	if err != nil {
		return state, err
	}
	if !ok || state.Completion == nil {
		return state, fmt.Errorf("完成意图不存在")
	}
	if state.Terminal() {
		return state, nil
	}
	intent := state.Completion
	if intent.Status == "unknown" {
		return state, fmt.Errorf("交付结果无法确认，禁止自动重放: %s", intent.Error)
	}
	receipt := intent.DeliveryRef
	if intent.CandidateRef != "" && receipt == "" {
		if r.Delivery == nil {
			return state, fmt.Errorf("交付服务未装配")
		}
		completionID, _ := dataflowDigest([]string{id, intent.RequestID, intent.Digest})
		receipt, err = r.Delivery.CommitCandidate(ctx, completionID, id, intent.CandidateRef)
		if err != nil || receipt == "" {
			reason := "交付没有返回已结算回执"
			if err != nil {
				reason = err.Error()
			}
			updated, saveErr := r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
				s.Completion.Status = "unknown"
				s.Completion.Error = reason
				return nil
			})
			if saveErr != nil {
				return updated, saveErr
			}
			return updated, fmt.Errorf("图交付未确认: %s", reason)
		}
	}
	return r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
		if s.Terminal() {
			return nil
		}
		if s.Status != "finalizing" || s.Completion == nil || s.Completion.Digest != intent.Digest {
			return fmt.Errorf("完成事务状态冲突")
		}
		s.Completion.Status = "committed"
		s.Completion.DeliveryRef = receipt
		switch intent.Outcome {
		case "success":
			s.Status = "completed"
		case "failed":
			s.Status = "failed"
		case "blocked":
			s.Status = "blocked"
		}
		s.enqueuePlanning("graph_terminal", "", s.Status)
		return nil
	})
}

func (r *DataflowRuntime) AcknowledgePlanning(id string, through int64) error {
	_, err := r.Store.transact(id, func(s *DataflowSnapshot, exists bool) error {
		if !exists {
			return fmt.Errorf("图不存在")
		}
		if through <= s.PlanningAcknowledged {
			return nil
		}
		last := s.PlanningAcknowledged
		if len(s.PlanningEvents) > 0 {
			last = s.PlanningEvents[len(s.PlanningEvents)-1].Sequence
		}
		if through > last {
			return fmt.Errorf("不能确认未生成的规划事件")
		}
		s.PlanningAcknowledged = through
		events := s.PlanningEvents[:0]
		for _, e := range s.PlanningEvents {
			if e.Sequence > through {
				events = append(events, e)
			}
		}
		s.PlanningEvents = events
		return nil
	})
	return err
}

func (r *DataflowRuntime) Cancel(ctx context.Context, id, requestID, reason string) (DataflowSnapshot, error) {
	if err := dataflowIdentity("request_id", requestID); err != nil {
		return DataflowSnapshot{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return DataflowSnapshot{}, fmt.Errorf("取消需要原因")
	}
	digest, _ := dataflowDigest([]string{"cancel", reason})
	state, err := r.Store.transact(id, func(s *DataflowSnapshot, exists bool) error {
		if !exists {
			return fmt.Errorf("图不存在")
		}
		if old, ok := s.Requests[requestID]; ok {
			if old.Action != "cancel" || old.Digest != digest {
				return fmt.Errorf("取消身份冲突")
			}
			return nil
		}
		if s.Terminal() || s.Status == "finalizing" {
			return fmt.Errorf("图已终结或正在提交")
		}
		s.Status = "cancelling"
		s.Completion = &DataflowCompletion{Schema: CompletionSchema, RequestID: requestID, Digest: digest, Outcome: "cancelled", Summary: reason, Status: "prepared"}
		s.Requests[requestID] = DataflowRequestReceipt{Action: "cancel", Digest: digest, Revision: s.Definition.Revision}
		return nil
	})
	if err != nil {
		return state, err
	}
	if state.Terminal() {
		return state, nil
	}
	for _, exec := range state.Executions {
		if exec.ActivationID != "" && !dataflowTaskTerminal(exec.Status) {
			if r.Board == nil {
				return state, fmt.Errorf("取消执行面未装配")
			}
			if err := r.Board.CancelAgentTask(ctx, exec.TaskID, reason); err != nil {
				return state, err
			}
		}
	}
	return r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
		for _, exec := range s.Executions {
			if exec.ActivationID != "" && !dataflowTaskTerminal(exec.Status) {
				return nil
			}
		}
		settleDataflowCancellation(s)
		return nil
	})
}

func settleDataflowCancellation(s *DataflowSnapshot) {
	if s.Status != "cancelling" {
		return
	}
	for _, e := range s.Executions {
		if e.ActivationID != "" && !dataflowTaskTerminal(e.Status) {
			return
		}
	}
	s.Status = "cancelled"
	s.Completion.Status = "committed"
	s.enqueuePlanning("graph_terminal", "", "cancelled")
}

// 普通文本只要求完整正文；结构化提交仍校验节点声明的 schema。
func validateTerminalValue(schema map[string]any, fact AgentTaskTerminal) error {
	if fact.PlainText {
		text, ok := fact.Value["summary"].(string)
		if !ok || strings.TrimSpace(text) == "" || len(fact.Value) != 1 {
			return fmt.Errorf("普通文本结果缺少唯一 summary 正文")
		}
		return nil
	}
	return validateDataflowValue(schema, fact.Value)
}
