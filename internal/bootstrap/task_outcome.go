package bootstrap

import (
	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
	"agentgo/internal/store"
	"encoding/json"
	"fmt"
	"time"
)

type taskCheckpointReader interface {
	LoadCheckpoint(string) (*loopcontract.ProgressCheckpoint, bool, error)
}
type taskCheckpointFinalizer interface {
	taskCheckpointReader
	SealCurrentForTerminal(string) (*loopcontract.ProgressCheckpoint, bool, error)
}

type dataflowOutcomeAuthority struct {
	graphs          *graph.DataflowStore
	runtime         *graph.DataflowRuntime
	outcomes        *outcomestore.Store
	checkpoints     taskCheckpointReader
	freezeCandidate func(*model.Task) (string, error)
}

func (a *dataflowOutcomeAuthority) build(intent store.TerminalOutcomeIntent) (outcome.TaskOutcome, error) {
	t := intent.Task
	if t == nil || t.RunID == "" || t.RunContract == nil {
		return outcome.TaskOutcome{}, fmt.Errorf("终态任务缺少 Run 身份")
	}
	status, err := taskOutcomeStatus(t.Status)
	if err != nil {
		return outcome.TaskOutcome{}, err
	}
	value := graphTaskResult(t)
	plainText := t.Results[agent.StructuredResultStorageKey] == ""
	if t.GraphID != "" {
		s, ok, err := a.graphs.Get(t.GraphID)
		if err != nil {
			return outcome.TaskOutcome{}, err
		}
		if !ok || s.Definition.RunID != string(t.RunID) {
			return outcome.TaskOutcome{}, fmt.Errorf("任务 Graph/Run 身份不符")
		}
		exec, ok := s.Executions[t.NodeID]
		if !ok || exec.TaskID != t.ID || exec.ActivationID != t.ActivationID {
			return outcome.TaskOutcome{}, fmt.Errorf("任务不属于已冻结的 agentTask 执行")
		}
		if status == outcome.StatusCompleted && !plainText {
			if err := a.runtime.ValidateAgentTaskResult(t.GraphID, t.NodeID, value); err != nil {
				return outcome.TaskOutcome{}, err
			}
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return outcome.TaskOutcome{}, err
	}
	summary := intent.Summary
	if summary == "" {
		summary = taskOutcomeSummary(t)
	}
	// 索引摘要有存储边界；普通结果的完整原文仍在 Result/TaskResults/LastResponse。
	if len(summary) > outcome.SummaryMaxBytes {
		summary, _ = boundedEvidenceValue(summary, outcome.SummaryMaxBytes/4)
	}
	candidate := ""
	if t.GraphID != "" && status == outcome.StatusCompleted && a.freezeCandidate != nil {
		candidate, err = a.freezeCandidate(t)
		if err != nil {
			return outcome.TaskOutcome{}, err
		}
	}
	result := outcome.TaskOutcome{PlainText: plainText, Schema: outcome.SchemaCurrent, RunID: t.RunID, GraphID: t.GraphID, NodeID: t.NodeID, ActivationID: t.ActivationID, TaskID: t.ID, AttemptID: t.AttemptID, AttemptNo: t.AttemptNo, Status: status, Summary: summary, Result: raw, TaskResults: cloneTaskResults(t.Results), CandidateRef: candidate, CommittedAt: t.CompletedAt}
	if result.CommittedAt.IsZero() {
		result.CommittedAt = time.Now().UTC()
	}
	if status != outcome.StatusCompleted {
		result.Reason = t.Error
		if result.Reason == "" {
			result.Reason = summary
		}
		result.ReasonCode = intent.ReasonCode
		if result.ReasonCode == "" {
			result.ReasonCode = "task_" + string(status)
		}
	}
	for _, e := range assembleTaskEvidenceFromCalls(t, intent.ToolCalls) {
		result.EvidenceFacts = append(result.EvidenceFacts, outcomeEvidenceFact(e))
		result.EvidenceRefs = append(result.EvidenceRefs, e.Ref)
	}
	return result, nil
}

func (a *dataflowOutcomeAuthority) PrepareTerminalIntent(intent store.TerminalOutcomeIntent) (string, error) {
	value, err := a.build(intent)
	if err != nil {
		return "", err
	}
	record, err := a.outcomes.PrepareIntent(outcome.TerminalIntent{Schema: outcome.TerminalIntentSchemaCurrent, Candidate: value, PreparedAt: value.CommittedAt})
	if err != nil {
		return "", err
	}
	return record.IntentRef, nil
}
func (a *dataflowOutcomeAuthority) SettleTerminalIntent(ref string) (store.TerminalCheckpointBinding, error) {
	record, ok, err := a.outcomes.GetIntent(ref)
	if err != nil || !ok {
		return store.TerminalCheckpointBinding{}, fmt.Errorf("终态意图不可读取: %v", err)
	}
	candidate := record.Intent.Candidate
	if candidate.AttemptID == "" {
		return store.TerminalCheckpointBinding{CheckpointState: outcome.CheckpointStatePreAttempt}, nil
	}
	finalizer, ok := a.checkpoints.(taskCheckpointFinalizer)
	if !ok {
		return store.TerminalCheckpointBinding{}, fmt.Errorf("终态 checkpoint 服务未装配")
	}
	checkpoint, exists, err := finalizer.SealCurrentForTerminal(candidate.TaskID)
	if err != nil {
		return store.TerminalCheckpointBinding{}, err
	}
	if !exists {
		return store.TerminalCheckpointBinding{CheckpointState: outcome.CheckpointStateNotApplicable}, nil
	}
	if !checkpoint.Sealed || checkpoint.AttemptID != candidate.AttemptID {
		return store.TerminalCheckpointBinding{}, fmt.Errorf("终态 checkpoint 未封存或身份不一致")
	}
	return store.TerminalCheckpointBinding{CheckpointState: outcome.CheckpointStateSealed, CheckpointRef: checkpoint.CheckpointID}, nil
}
func (a *dataflowOutcomeAuthority) CommitTerminalOutcome(ref string, intent store.TerminalOutcomeIntent, binding store.TerminalCheckpointBinding) (string, error) {
	value, err := a.build(intent)
	if err != nil {
		return "", err
	}
	record, err := a.outcomes.CommitIntent(ref, value, binding.CheckpointRef, binding.CheckpointState)
	if err != nil {
		return "", err
	}
	return record.OutcomeRef, nil
}
