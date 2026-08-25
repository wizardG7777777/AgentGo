package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/fulfillment"
	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
	"agentgo/internal/model"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
	"agentgo/internal/store"
	"agentgo/internal/terminaladapter"
)

type graphDocumentReader interface {
	Get(graphID string) (*graph.GraphDocument, bool)
}

type taskCheckpointReader interface {
	LoadCheckpoint(taskID string) (*loopcontract.ProgressCheckpoint, bool, error)
}

type taskCheckpointFinalizer interface {
	taskCheckpointReader
	SealCurrentForTerminal(taskID string) (*loopcontract.ProgressCheckpoint, bool, error)
}

type taskCheckpointUnknownFinalizer interface {
	SealPendingUnknownForTerminal(taskID string) (*loopcontract.ProgressCheckpoint, bool, error)
}

// graphTaskOutcomeAuthority 是 TaskStore terminal hook、live feed 与 Graph
// recovery 共用的唯一 TaskOutcome 边界。
type graphTaskOutcomeAuthority struct {
	graphs      graphDocumentReader
	outcomes    *outcomestore.Store
	checkpoints taskCheckpointReader
}

func newGraphTaskOutcomeAuthority(graphs graphDocumentReader, outcomes *outcomestore.Store,
	checkpoints taskCheckpointReader) *graphTaskOutcomeAuthority {
	return &graphTaskOutcomeAuthority{graphs: graphs, outcomes: outcomes, checkpoints: checkpoints}
}

func (a *graphTaskOutcomeAuthority) requiredForGraph(graphID string) (bool, *graph.GraphDocument, error) {
	if strings.TrimSpace(graphID) == "" {
		return false, nil, nil
	}
	if a == nil || a.graphs == nil {
		return false, nil, fmt.Errorf("TaskOutcome authority 缺少 Graph reader")
	}
	doc, ok := a.graphs.Get(graphID)
	if !ok || doc == nil {
		return false, nil, fmt.Errorf("TaskOutcome authority 找不到 Graph %s", graphID)
	}
	required, err := doc.RequiresTypedTaskOutcome()
	return required, doc, err
}

func (a *graphTaskOutcomeAuthority) Commit(intent store.TerminalOutcomeIntent) (string, error) {
	task := intent.Task
	if task == nil {
		return "", nil
	}
	if task.RunID == "" && task.RunContract == nil {
		return "", nil
	}
	if task.RunID == "" || task.RunContract == nil {
		return "", fmt.Errorf("Task %s 的 RunID/RunContract binding 不完整", task.ID)
	}
	if a.outcomes == nil {
		return "", fmt.Errorf("新 Run Task 的 TaskOutcome Store 未装配")
	}
	var executionDoc *graph.GraphDocument
	if task.GraphID != "" {
		_, doc, err := a.requiredForGraph(task.GraphID)
		if err != nil {
			return "", err
		}
		if err := validateOutcomeTaskIdentity(task, doc); err != nil {
			return "", err
		}
		executionDoc = doc
	}

	status, err := taskOutcomeStatus(task.Status)
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(intent.Summary)
	if summary == "" {
		summary = taskOutcomeSummary(task)
	}
	structuredResult := graphTaskResult(task)
	if status == outcome.StatusCompleted && executionDoc != nil {
		node := executionDoc.Nodes[task.NodeID]
		contract := node.OutputContract
		if node.Execution != nil && node.Execution.Definition != nil {
			contract = node.Execution.Definition.OutputContract
		}
		if err := graph.ValidateNodeOutput(contract, summary, structuredResult); err != nil {
			return "", err
		}
	}
	result, err := json.Marshal(structuredResult)
	if err != nil {
		return "", fmt.Errorf("编码 TaskOutcome structured result: %w", err)
	}
	evidenceEntries := assembleTaskEvidenceFromCalls(task, intent.ToolCalls)
	evidenceFacts := make([]outcome.EvidenceFact, 0, len(evidenceEntries))
	evidenceRefs := make([]string, 0, len(evidenceEntries))
	artifactFacts := make([]outcome.ArtifactFact, 0)
	artifactRefs := make([]string, 0)
	artifactPathByRef := make(map[string]string, len(task.Artifacts))
	for _, path := range task.Artifacts {
		artifactPathByRef[evidenceArtifactRef(task, path)] = path
	}
	for _, entry := range evidenceEntries {
		evidenceRefs = append(evidenceRefs, entry.Ref)
		evidenceFacts = append(evidenceFacts, outcomeEvidenceFact(entry))
		if entry.Kind != "artifact" {
			continue
		}
		path := artifactPathByRef[entry.Ref]
		meta := task.ArtifactMeta[path]
		artifactRefs = append(artifactRefs, entry.Ref)
		artifactFacts = append(artifactFacts, outcome.ArtifactFact{
			Ref: entry.Ref, Path: path, SHA256: meta.SHA256, Bytes: meta.Bytes,
		})
	}

	checkpointRef, checkpointState, err := a.currentCheckpointRef(task)
	if err != nil {
		return "", err
	}
	observationRef, err := a.currentObservationRef(task)
	if err != nil {
		return "", err
	}
	reason, reasonCode := "", ""
	if status != outcome.StatusCompleted {
		reason = strings.TrimSpace(task.Error)
		if reason == "" {
			reason = summary
		}
		reasonCode = strings.TrimSpace(intent.ReasonCode)
		if reasonCode == "" {
			reasonCode = strings.TrimSpace(intent.Cause)
		}
		if reasonCode == "" {
			reasonCode = "task_" + string(status)
		}
	}
	fulfillmentRecord, err := fulfillmentFromTask(task)
	if err != nil {
		return "", err
	}
	schema := outcome.SchemaV1
	if task.FulfillmentContract != nil {
		schema = outcome.SchemaV2
	}
	value := outcome.TaskOutcome{
		Schema: schema, RunID: task.RunID,
		GraphID: task.GraphID, NodeID: task.NodeID, ActivationID: task.ActivationID,
		TaskID: task.ID, AttemptID: task.AttemptID, AttemptNo: task.AttemptNo,
		Status: status, Summary: summary, Result: result,
		TaskResults:  cloneTaskResults(task.Results),
		EvidenceRefs: evidenceRefs, ArtifactRefs: artifactRefs,
		EvidenceFacts: evidenceFacts, ArtifactFacts: artifactFacts,
		ReasonCode: reasonCode, Reason: reason, CheckpointRef: checkpointRef,
		ObservationDeltaRef: observationRef, CheckpointState: checkpointState,
		Fulfillment: fulfillmentRecord,
		CommittedAt: task.CompletedAt,
	}
	record, err := a.outcomes.Commit(value)
	if err != nil {
		return "", err
	}
	return record.OutcomeRef, nil
}

func (a *graphTaskOutcomeAuthority) PrepareTerminalIntent(intent store.TerminalOutcomeIntent) (string, error) {
	value, required, err := a.buildOutcomeCandidate(intent)
	if err != nil || !required {
		return "", err
	}
	preparedAt := intent.Task.CompletedAt
	if preparedAt.IsZero() {
		preparedAt = time.Now().UTC()
	}
	value.CommittedAt = time.Time{}
	value.CheckpointRef, value.CheckpointState = "", ""
	record, err := a.outcomes.PrepareIntent(outcome.TerminalIntent{
		Schema: outcome.TerminalIntentSchemaV1, Candidate: value, PreparedAt: preparedAt,
	})
	if err != nil {
		return "", err
	}
	return record.IntentRef, nil
}

func (a *graphTaskOutcomeAuthority) SettleTerminalIntent(intentRef string) (store.TerminalCheckpointBinding, error) {
	intent, ok, err := a.outcomes.GetIntent(intentRef)
	if err != nil || !ok {
		return store.TerminalCheckpointBinding{}, fmt.Errorf("读取 TerminalIntent %s: ok=%v err=%w", intentRef, ok, err)
	}
	candidate := intent.Intent.Candidate
	if candidate.AttemptID == "" {
		return store.TerminalCheckpointBinding{CheckpointState: outcome.CheckpointStatePreAttempt}, nil
	}
	finalizer, ok := a.checkpoints.(taskCheckpointFinalizer)
	if !ok || finalizer == nil {
		return store.TerminalCheckpointBinding{CheckpointState: outcome.CheckpointStateNotApplicable}, nil
	}
	deadline := time.Now().UTC().Add(5 * time.Second)
	if current, exists, loadErr := finalizer.LoadCheckpoint(candidate.TaskID); loadErr == nil && exists && current != nil {
		if hard := current.Deadlines.Attempt.HardDeadlineAt; !hard.IsZero() && hard.Before(deadline) {
			deadline = hard
		}
	}
	var checkpoint *loopcontract.ProgressCheckpoint
	var exists bool
	for {
		checkpoint, exists, err = finalizer.SealCurrentForTerminal(candidate.TaskID)
		if err == nil || !errors.Is(err, loopstore.ErrTerminalSettlementPending) {
			break
		}
		if !time.Now().UTC().Before(deadline) {
			unknownFinalizer, supported := a.checkpoints.(taskCheckpointUnknownFinalizer)
			if !supported {
				return store.TerminalCheckpointBinding{}, fmt.Errorf("等待 action settlement 至绝对 deadline 仍未完成，且无 ActionUnknown finalizer: %w", err)
			}
			checkpoint, exists, err = unknownFinalizer.SealPendingUnknownForTerminal(candidate.TaskID)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return store.TerminalCheckpointBinding{}, err
	}
	if !exists || checkpoint == nil {
		return store.TerminalCheckpointBinding{CheckpointState: outcome.CheckpointStateNotApplicable}, nil
	}
	if !checkpoint.Sealed {
		return store.TerminalCheckpointBinding{}, fmt.Errorf("Task %s terminal checkpoint 未 sealed", candidate.TaskID)
	}
	return store.TerminalCheckpointBinding{
		CheckpointRef: checkpoint.CheckpointID, CheckpointState: outcome.CheckpointStateSealed,
		ObservationDeltaRef: checkpoint.ObservationDeltaRef,
	}, nil
}

func (a *graphTaskOutcomeAuthority) CommitTerminalOutcome(intentRef string, refreshed store.TerminalOutcomeIntent,
	binding store.TerminalCheckpointBinding) (string, error) {
	value, required, err := a.buildOutcomeCandidate(refreshed)
	if err != nil || !required {
		return "", err
	}
	value.ObservationDeltaRef = binding.ObservationDeltaRef
	record, err := a.outcomes.CommitIntent(intentRef, value, binding.CheckpointRef, binding.CheckpointState)
	if err != nil {
		return "", err
	}
	return record.OutcomeRef, nil
}

func (a *graphTaskOutcomeAuthority) currentObservationRef(task *model.Task) (string, error) {
	if task == nil || task.ProgressContract == nil || strings.TrimSpace(task.AttemptID) == "" || a.checkpoints == nil {
		return "", nil
	}
	checkpoint, ok, err := a.checkpoints.LoadCheckpoint(task.ID)
	if err != nil {
		return "", err
	}
	if !ok || checkpoint == nil || checkpoint.AttemptID != task.AttemptID {
		return "", nil
	}
	return checkpoint.ObservationDeltaRef, nil
}

func (a *graphTaskOutcomeAuthority) buildOutcomeCandidate(intent store.TerminalOutcomeIntent) (outcome.TaskOutcome, bool, error) {
	task := intent.Task
	if task == nil || task.RunID == "" && task.RunContract == nil {
		return outcome.TaskOutcome{}, false, nil
	}
	if task.RunID == "" || task.RunContract == nil {
		return outcome.TaskOutcome{}, true, fmt.Errorf("Task %s 的 RunID/RunContract binding 不完整", task.ID)
	}
	if a == nil || a.outcomes == nil {
		return outcome.TaskOutcome{}, true, fmt.Errorf("新 Run Task 的 TaskOutcome Store 未装配")
	}
	var executionDoc *graph.GraphDocument
	if task.GraphID != "" {
		_, doc, err := a.requiredForGraph(task.GraphID)
		if err != nil {
			return outcome.TaskOutcome{}, true, err
		}
		if err := validateOutcomeTaskIdentity(task, doc); err != nil {
			return outcome.TaskOutcome{}, true, err
		}
		executionDoc = doc
	}
	status, err := taskOutcomeStatus(task.Status)
	if err != nil {
		return outcome.TaskOutcome{}, true, err
	}
	summary := strings.TrimSpace(intent.Summary)
	if summary == "" {
		summary = taskOutcomeSummary(task)
	}
	structuredResult := graphTaskResult(task)
	if status == outcome.StatusCompleted && executionDoc != nil {
		node := executionDoc.Nodes[task.NodeID]
		contract := node.OutputContract
		if node.Execution != nil && node.Execution.Definition != nil {
			contract = node.Execution.Definition.OutputContract
		}
		if err := graph.ValidateNodeOutput(contract, summary, structuredResult); err != nil {
			return outcome.TaskOutcome{}, true, err
		}
	}
	result, err := json.Marshal(structuredResult)
	if err != nil {
		return outcome.TaskOutcome{}, true, err
	}
	evidenceEntries := assembleTaskEvidenceFromCalls(task, intent.ToolCalls)
	evidenceFacts := make([]outcome.EvidenceFact, 0, len(evidenceEntries))
	evidenceRefs := make([]string, 0, len(evidenceEntries))
	artifactFacts := make([]outcome.ArtifactFact, 0)
	artifactRefs := make([]string, 0)
	artifactPathByRef := make(map[string]string, len(task.Artifacts))
	for _, path := range task.Artifacts {
		artifactPathByRef[evidenceArtifactRef(task, path)] = path
	}
	for _, entry := range evidenceEntries {
		evidenceRefs = append(evidenceRefs, entry.Ref)
		evidenceFacts = append(evidenceFacts, outcomeEvidenceFact(entry))
		if entry.Kind == "artifact" {
			path := artifactPathByRef[entry.Ref]
			meta := task.ArtifactMeta[path]
			artifactRefs = append(artifactRefs, entry.Ref)
			artifactFacts = append(artifactFacts, outcome.ArtifactFact{Ref: entry.Ref, Path: path, SHA256: meta.SHA256, Bytes: meta.Bytes})
		}
	}
	reason, reasonCode := "", ""
	if status != outcome.StatusCompleted {
		reason = strings.TrimSpace(task.Error)
		if reason == "" {
			reason = summary
		}
		reasonCode = strings.TrimSpace(intent.ReasonCode)
		if reasonCode == "" {
			reasonCode = strings.TrimSpace(intent.Cause)
		}
		if reasonCode == "" {
			reasonCode = "task_" + string(status)
		}
	}
	fulfillmentRecord, fulfillmentErr := fulfillmentFromTask(task)
	if fulfillmentErr != nil {
		return outcome.TaskOutcome{}, true, fulfillmentErr
	}
	schema := outcome.SchemaV1
	if task.FulfillmentContract != nil {
		schema = outcome.SchemaV2
	}
	return outcome.TaskOutcome{
		Schema: schema, RunID: task.RunID,
		GraphID: task.GraphID, NodeID: task.NodeID, ActivationID: task.ActivationID,
		TaskID: task.ID, AttemptID: task.AttemptID, AttemptNo: task.AttemptNo,
		Status: status, Summary: summary, Result: result, TaskResults: cloneTaskResults(task.Results),
		EvidenceRefs: evidenceRefs, ArtifactRefs: artifactRefs, EvidenceFacts: evidenceFacts, ArtifactFacts: artifactFacts,
		ReasonCode: reasonCode, Reason: reason,
		Fulfillment: fulfillmentRecord,
	}, true, nil
}

func fulfillmentFromTask(task *model.Task) (*fulfillment.Record, error) {
	if task == nil || task.FulfillmentContract == nil {
		return nil, nil
	}
	raw := strings.TrimSpace(task.Results[agent.FulfillmentStorageKey])
	if raw == "" {
		if task.Status == model.TaskStatusCompleted {
			return nil, fmt.Errorf("completed Task %s 缺少 fulfillment record", task.ID)
		}
		return nil, nil
	}
	var record fulfillment.Record
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, fmt.Errorf("解析 Task fulfillment: %w", err)
	}
	if err := record.Validate(task.FulfillmentContract); err != nil {
		return nil, err
	}
	return &record, nil
}

func (a *graphTaskOutcomeAuthority) currentCheckpointRef(task *model.Task) (string, string, error) {
	if task == nil || task.ProgressContract == nil {
		if task != nil && task.AttemptID != "" {
			return "", outcome.CheckpointStateNotApplicable, nil
		}
		return "", outcome.CheckpointStatePreAttempt, nil
	}
	if strings.TrimSpace(task.AttemptID) == "" {
		return "", outcome.CheckpointStatePreAttempt, nil
	}
	if a.checkpoints == nil {
		return "", "", fmt.Errorf("新 Run Task %s 缺少 ProgressCheckpoint reader", task.ID)
	}
	checkpoint, ok, err := a.checkpoints.LoadCheckpoint(task.ID)
	if err != nil {
		return "", "", fmt.Errorf("读取 Task %s 当前 ProgressCheckpoint: %w", task.ID, err)
	}
	if !ok || checkpoint == nil {
		// pre-claim blocked/cancelled 还没有 Attempt/Checkpoint；这是显式无
		// checkpoint，不伪造引用。claimed Task 必须已有当前 checkpoint。
		if strings.TrimSpace(task.AttemptID) == "" {
			return "", outcome.CheckpointStatePreAttempt, nil
		}
		return "", "", fmt.Errorf("Task %s 已进入 Attempt 但缺少 ProgressCheckpoint", task.ID)
	}
	if checkpoint.TaskID != task.ID || checkpoint.RunID != task.RunID || checkpoint.GraphID != task.GraphID ||
		checkpoint.NodeID != task.NodeID || checkpoint.ActivationID != task.ActivationID || checkpoint.AttemptID != task.AttemptID {
		return "", "", fmt.Errorf("Task %s ProgressCheckpoint lineage 与终态候选不一致", task.ID)
	}
	state := outcome.CheckpointStateCurrentUnsealed
	if checkpoint.Sealed {
		state = outcome.CheckpointStateSealed
	}
	return checkpoint.CheckpointID, state, nil
}

func (a *graphTaskOutcomeAuthority) FactForTask(ctx context.Context, task *model.Task) (graph.TerminalFact, bool, error) {
	if task == nil || task.GraphID == "" {
		return graph.TerminalFact{}, false, nil
	}
	required, doc, err := a.requiredForGraph(task.GraphID)
	if err != nil {
		return graph.TerminalFact{}, required, err
	}
	if strings.TrimSpace(task.OutcomeRef) == "" {
		if !required {
			return graph.TerminalFact{}, false, nil
		}
		return graph.TerminalFact{}, true, fmt.Errorf("新 Definition Graph Task %s 缺少 outcome_ref", task.ID)
	}
	record, ok, err := a.outcomes.GetByRef(task.OutcomeRef)
	if err != nil {
		return graph.TerminalFact{}, true, err
	}
	if !ok {
		return graph.TerminalFact{}, true, fmt.Errorf("Task %s 的 outcome_ref=%s 不可解引用", task.ID, task.OutcomeRef)
	}
	if err := validateOutcomeRecordIdentity(record, task.ID, task.GraphID, task.NodeID, task.ActivationID, doc); err != nil {
		return graph.TerminalFact{}, true, err
	}
	if modelStatusFromOutcome(record.Outcome.Status) != task.Status {
		return graph.TerminalFact{}, true, fmt.Errorf("Task %s status=%s 与 outcome status=%s 不一致", task.ID, task.Status, record.Outcome.Status)
	}
	fact, err := terminaladapter.ToTerminalFact(ctx, record, terminaladapter.Dependencies{})
	return fact, true, err
}

func (a *graphTaskOutcomeAuthority) FactByActivation(ctx context.Context, graphID, activationID, taskID string) (graph.TerminalFact, bool, error) {
	_, doc, err := a.requiredForGraph(graphID)
	if err != nil {
		return graph.TerminalFact{}, false, err
	}
	record, ok, err := a.outcomes.GetByActivation(graphID, activationID)
	if err != nil || !ok {
		return graph.TerminalFact{}, false, err
	}
	if err := validateOutcomeRecordIdentity(record, taskID, graphID, record.Outcome.NodeID, activationID, doc); err != nil {
		return graph.TerminalFact{}, true, err
	}
	fact, err := terminaladapter.ToTerminalFact(ctx, record, terminaladapter.Dependencies{})
	return fact, true, err
}

func (a *graphTaskOutcomeAuthority) FactByTaskID(ctx context.Context, taskID string) (graph.TerminalFact, bool, error) {
	if a == nil || a.outcomes == nil || strings.TrimSpace(taskID) == "" {
		return graph.TerminalFact{}, false, nil
	}
	record, ok, err := a.outcomes.GetByTask(taskID)
	if err != nil || !ok {
		return graph.TerminalFact{}, false, err
	}
	if record.Outcome.GraphID == "" {
		return graph.TerminalFact{}, false, nil
	}
	_, doc, err := a.requiredForGraph(record.Outcome.GraphID)
	if err != nil {
		return graph.TerminalFact{}, false, err
	}
	if err := validateOutcomeRecordIdentity(record, taskID, record.Outcome.GraphID,
		record.Outcome.NodeID, record.Outcome.ActivationID, doc); err != nil {
		return graph.TerminalFact{}, true, err
	}
	fact, err := terminaladapter.ToTerminalFact(ctx, record, terminaladapter.Dependencies{})
	return fact, true, err
}

func (a *graphTaskOutcomeAuthority) AckNonGraphTask(taskID string) (bool, error) {
	if a == nil || a.outcomes == nil || strings.TrimSpace(taskID) == "" {
		return false, nil
	}
	record, ok, err := a.outcomes.GetByTask(taskID)
	if err != nil || !ok || record.Outcome.GraphID != "" {
		return false, err
	}
	if err := a.outcomes.AckDelivery(record.OutcomeRef); err != nil {
		return true, err
	}
	return true, nil
}

func (a *graphTaskOutcomeAuthority) AckTask(taskID string) (bool, error) {
	if a == nil || a.outcomes == nil || strings.TrimSpace(taskID) == "" {
		return false, nil
	}
	record, ok, err := a.outcomes.GetByTask(taskID)
	if err != nil || !ok {
		return false, err
	}
	return true, a.outcomes.AckDelivery(record.OutcomeRef)
}

func (a *graphTaskOutcomeAuthority) AckRef(outcomeRef string) error {
	if a == nil || a.outcomes == nil || strings.TrimSpace(outcomeRef) == "" {
		return nil
	}
	return a.outcomes.AckDelivery(outcomeRef)
}

func (a *graphTaskOutcomeAuthority) ReconcileTasks(tasks store.TaskStore) error {
	if a == nil || a.outcomes == nil || tasks == nil {
		return nil
	}
	all, err := tasks.ScanAll()
	if err != nil {
		return err
	}
	for _, task := range all {
		if task == nil || task.RunID == "" && task.RunContract == nil {
			continue
		}
		if task.RunID == "" || task.RunContract == nil {
			return fmt.Errorf("Task %s 的 RunID/RunContract binding 不完整", task.ID)
		}
		record, ok, getErr := a.outcomes.GetByTask(task.ID)
		if getErr != nil {
			return getErr
		}
		if !ok {
			if cause, guarded := recoveryGuardCause(task.Error); guarded && model.IsTerminal(task.Status) && task.OutcomeRef == "" {
				ref, commitErr := a.Commit(store.TerminalOutcomeIntent{
					Task: task, Cause: cause, ReasonCode: cause, Summary: task.Error,
				})
				if commitErr != nil {
					return fmt.Errorf("为恢复守卫 Task %s 提交 TaskOutcome: %w", task.ID, commitErr)
				}
				record, ok, getErr = a.outcomes.GetByRef(ref)
				if getErr != nil || !ok {
					return fmt.Errorf("恢复守卫 Task %s 的 OutcomeRef 不可解引用: %w", task.ID, getErr)
				}
			}
		}
		if !ok {
			if model.IsTerminal(task.Status) || task.OutcomeRef != "" {
				return fmt.Errorf("新 Run Task %s 已是 %s/ref=%q，但 OutcomeStore 缺记录",
					task.ID, task.Status, task.OutcomeRef)
			}
			continue
		}
		if task.GraphID != "" {
			_, doc, requireErr := a.requiredForGraph(task.GraphID)
			if requireErr != nil {
				return requireErr
			}
			if err := validateOutcomeRecordIdentity(record, task.ID, task.GraphID, task.NodeID, task.ActivationID, doc); err != nil {
				return err
			}
		} else if record.Outcome.GraphID != "" || record.Outcome.TaskID != task.ID || record.Outcome.RunID != task.RunID {
			return fmt.Errorf("非 Graph Task %s 的 TaskOutcome identity 不一致", task.ID)
		}
		if task.OutcomeRef != "" && task.OutcomeRef != record.OutcomeRef {
			return fmt.Errorf("Task %s snapshot outcome_ref=%s 与 Store=%s 冲突", task.ID, task.OutcomeRef, record.OutcomeRef)
		}
		if err := store.ApplyRecoveredTaskOutcome(tasks, task.ID, record.OutcomeRef,
			modelStatusFromOutcome(record.Outcome.Status), record.Outcome.TaskResults,
			record.Outcome.Reason, record.Outcome.CommittedAt); err != nil {
			return err
		}
		if task.GraphID == "" {
			if err := a.outcomes.AckDelivery(record.OutcomeRef); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecoverPendingIntents resumes crashes after durable fence but before Outcome/
// Task CAS. It runs after Session Task import and before dispatcher/Runner activation.
func (a *graphTaskOutcomeAuthority) RecoverPendingIntents(tasks store.TaskStore) error {
	if a == nil || a.outcomes == nil || tasks == nil {
		return nil
	}
	pending, err := a.outcomes.PendingIntents()
	if err != nil {
		return err
	}
	for _, prepared := range pending {
		binding, err := a.SettleTerminalIntent(prepared.IntentRef)
		if err != nil {
			return fmt.Errorf("恢复 TerminalIntent %s settlement: %w", prepared.IntentRef, err)
		}
		candidate := prepared.Intent.Candidate
		task, getErr := tasks.GetTask(candidate.TaskID)
		if getErr != nil || task == nil {
			record, commitErr := a.outcomes.CommitIntent(prepared.IntentRef, outcome.TaskOutcome{},
				binding.CheckpointRef, binding.CheckpointState)
			if commitErr != nil {
				return commitErr
			}
			_ = record // Graph delivery 可在 Task 已淘汰时按 OutcomeStore 恢复。
			continue
		}
		if task.AttemptID != candidate.AttemptID || task.AttemptNo != candidate.AttemptNo ||
			model.IsTerminal(task.Status) && task.Status != modelStatusFromOutcome(candidate.Status) {
			return fmt.Errorf("恢复 TerminalIntent %s 时 Task Attempt/Status 漂移", prepared.IntentRef)
		}
		task.Status = modelStatusFromOutcome(candidate.Status)
		task.Results = cloneTaskResults(candidate.TaskResults)
		task.Error = candidate.Reason
		task.CompletedAt = prepared.Intent.PreparedAt
		task.Agents = nil
		calls, err := tasks.QueryToolCalls(task.ID, "")
		if err != nil {
			return err
		}
		ref, err := a.CommitTerminalOutcome(prepared.IntentRef, store.TerminalOutcomeIntent{
			Task: task, ToolCalls: calls, Cause: candidate.ReasonCode,
			ReasonCode: candidate.ReasonCode, Summary: candidate.Summary,
		}, binding)
		if err != nil {
			return err
		}
		record, ok, err := a.outcomes.GetByRef(ref)
		if err != nil || !ok {
			return fmt.Errorf("恢复 TerminalIntent outcome ref=%s: ok=%v err=%w", ref, ok, err)
		}
		if err := store.ApplyRecoveredTaskOutcome(tasks, task.ID, ref, modelStatusFromOutcome(record.Outcome.Status),
			record.Outcome.TaskResults, record.Outcome.Reason, record.Outcome.CommittedAt); err != nil {
			return err
		}
	}
	return nil
}

func recoveryGuardCause(reason string) (string, bool) {
	for _, cause := range []string{
		"no_auto_resume", "effect_recovery_quarantine", "execution_lease_recovery_quarantine",
	} {
		if strings.HasPrefix(strings.TrimSpace(reason), cause+":") {
			return cause, true
		}
	}
	return "", false
}

func validateOutcomeTaskIdentity(task *model.Task, doc *graph.GraphDocument) error {
	if task == nil || doc == nil || strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.NodeID) == "" ||
		strings.TrimSpace(task.ActivationID) == "" || strings.TrimSpace(string(task.RunID)) == "" {
		return fmt.Errorf("新 Definition Graph Task 缺少 Run/Graph/Node/Activation/Task identity")
	}
	if task.RunID != doc.RunID || task.GraphDefinitionDigestVersion != doc.DefinitionDigestVersion {
		return fmt.Errorf("Task %s 的 Run/Definition identity 与 Graph %s 不一致", task.ID, doc.GraphID)
	}
	node, ok := doc.Nodes[task.NodeID]
	if !ok || node.Execution == nil || node.Execution.ActivationID != task.ActivationID {
		return fmt.Errorf("Task %s 不属于 Graph %s 当前 durable activation", task.ID, doc.GraphID)
	}
	if node.Execution.TaskID != "" && node.Execution.TaskID != task.ID {
		return fmt.Errorf("Task %s 与 Graph execution.task_id=%s 不一致", task.ID, node.Execution.TaskID)
	}
	return nil
}

func validateOutcomeRecordIdentity(record outcomestore.Record, taskID, graphID, nodeID, activationID string,
	doc *graph.GraphDocument) error {
	value := record.Outcome
	if taskID != "" && value.TaskID != taskID || value.GraphID != graphID || value.NodeID != nodeID || value.ActivationID != activationID {
		return fmt.Errorf("TaskOutcome identity 与预期 Task/Graph/Node/Activation 不一致")
	}
	if doc == nil || value.RunID != doc.RunID {
		return fmt.Errorf("TaskOutcome RunID 与 Graph 不一致")
	}
	return nil
}

func taskOutcomeStatus(status model.TaskStatus) (outcome.Status, error) {
	switch status {
	case model.TaskStatusCompleted:
		return outcome.StatusCompleted, nil
	case model.TaskStatusFailed:
		return outcome.StatusFailed, nil
	case model.TaskStatusBlocked:
		return outcome.StatusBlocked, nil
	case model.TaskStatusCancelled:
		return outcome.StatusCancelled, nil
	default:
		return "", fmt.Errorf("Task status=%s 不是终态", status)
	}
}

func modelStatusFromOutcome(status outcome.Status) model.TaskStatus {
	return model.TaskStatus(status)
}

func taskOutcomeSummary(task *model.Task) string {
	if task == nil {
		return "Task terminal"
	}
	if text := strings.TrimSpace(task.LastResponse); text != "" {
		return text
	}
	if text := strings.TrimSpace(task.Error); text != "" {
		return text
	}
	keys := make([]string, 0, len(task.Results))
	for key := range task.Results {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if text := strings.TrimSpace(task.Results[key]); text != "" {
			return text
		}
	}
	if text := strings.TrimSpace(task.Description); text != "" {
		return text
	}
	return "Task terminal status=" + string(task.Status)
}

func outcomeEvidenceFact(entry graph.EvidenceEntry) outcome.EvidenceFact {
	fact := outcome.EvidenceFact{
		Ref: entry.Ref, Kind: entry.Kind, Summary: entry.Summary,
		CallID: entry.CallID, ToolName: entry.ToolName,
		Command: entry.Command, CommandTruncated: entry.CommandTruncated,
		Path: entry.Path, PathTruncated: entry.PathTruncated,
	}
	if entry.Success != nil {
		value := *entry.Success
		fact.Success = &value
	}
	if entry.ExitCode != nil {
		value := *entry.ExitCode
		fact.ExitCode = &value
	}
	fact.ExitCodeScope = entry.ExitCodeScope
	return fact
}

func cloneTaskResults(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// replayPendingTaskOutcomes 在 dispatcher/Runner 激活前消费 durable delivery
// outbox。Session 模式的历史图按“进入会话不自动续跑”保持 pending；无
// Session 模式正常重放。非 Graph outcome 在 Task projection 成功后直接 ack。
func replayPendingTaskOutcomes(sys *System) error {
	if sys == nil || sys.TaskOutcomeStore == nil || sys.Store == nil {
		return nil
	}
	authority := newGraphTaskOutcomeAuthority(sys.GraphStore, sys.TaskOutcomeStore, sys.LoopStore)
	feed := newGraphFeedReactor(sys.Store, sys.GraphRuntime, authority)
	allowGraphReplay := currentSessionID(sys) == ""
	for {
		pending, err := sys.TaskOutcomeStore.PendingDeliveries()
		if err != nil {
			return err
		}
		handled := false
		for _, record := range pending {
			if record.Outcome.GraphID == "" {
				if err := sys.TaskOutcomeStore.AckDelivery(record.OutcomeRef); err != nil {
					return err
				}
				handled = true
				continue
			}
			if !allowGraphReplay {
				continue
			}
			fact, found, factErr := authority.FactByTaskID(context.Background(), record.Outcome.TaskID)
			if factErr != nil {
				return factErr
			}
			if !found {
				return fmt.Errorf("pending Graph TaskOutcome %s 无法转换为 TerminalFact", record.OutcomeRef)
			}
			task, _ := sys.Store.GetTask(record.Outcome.TaskID)
			if err := feed.deliverTypedOutcome(task, fact); err != nil {
				return err
			}
			handled = true
		}
		if !handled {
			return nil
		}
	}
}
