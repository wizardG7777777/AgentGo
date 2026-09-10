package bootstrap

import (
	"agentgo/internal/model"
	"agentgo/internal/outcome"
	"agentgo/internal/store"
	"encoding/json"
	"fmt"
)

func (b *dataflowBridge) recoverOutcomes(tasks store.TaskStore) error {
	pending, err := b.outcomes.PendingIntents()
	if err != nil {
		return err
	}
	for _, p := range pending {
		binding, err := b.authority.SettleTerminalIntent(p.IntentRef)
		if err != nil {
			return err
		}
		record, err := b.outcomes.CommitIntent(p.IntentRef, outcome.TaskOutcome{}, binding.CheckpointRef, binding.CheckpointState)
		if err != nil {
			return err
		}
		if t, err := tasks.GetTask(record.Outcome.TaskID); err == nil && t != nil {
			if err := store.ApplyRecoveredTaskOutcome(tasks, t.ID, record.OutcomeRef, model.TaskStatus(record.Outcome.Status), record.Outcome.TaskResults, record.Outcome.Reason, record.Outcome.CommittedAt); err != nil {
				return err
			}
		}
	}
	all, err := tasks.ScanAll()
	if err != nil {
		return err
	}
	for _, t := range all {
		if t.RunID == "" {
			continue
		}
		record, ok, err := b.outcomes.GetByTask(t.ID)
		if err != nil {
			return err
		}
		if !ok {
			if t.OutcomeRef != "" {
				return fmt.Errorf("任务终态记录缺失: %s", t.ID)
			}
			continue
		}
		if record.Outcome.RunID != t.RunID || record.Outcome.GraphID != t.GraphID || record.Outcome.NodeID != t.NodeID {
			return fmt.Errorf("恢复 TaskOutcome 身份不匹配")
		}
		if err := store.ApplyRecoveredTaskOutcome(tasks, t.ID, record.OutcomeRef, model.TaskStatus(record.Outcome.Status), record.Outcome.TaskResults, record.Outcome.Reason, record.Outcome.CommittedAt); err != nil {
			return err
		}
	}
	return nil
}

func reconcileDataflowOutcomeRecords(sys *System) error {
	if sys == nil || sys.TaskOutcomeStore == nil {
		return nil
	}
	records, err := sys.TaskOutcomeStore.PendingDeliveries()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Outcome.GraphID == "" {
			if err := sys.TaskOutcomeStore.AckDelivery(record.OutcomeRef); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderDataflowFinalizationFallback(task *model.Task) (string, error) {
	if task == nil || task.FinalReportGraphID == "" {
		return "", fmt.Errorf("缺少图最终结果身份")
	}
	for _, input := range task.ContextInputs {
		if input.SourceRef != "dataflow-state:"+task.FinalReportGraphID {
			continue
		}
		var state struct {
			Status     string `json:"status"`
			Completion *struct {
				Summary     string `json:"summary"`
				Outcome     string `json:"outcome"`
				DeliveryRef string `json:"delivery_ref"`
			} `json:"completion"`
		}
		if err := json.Unmarshal([]byte(input.Content), &state); err != nil {
			return "", err
		}
		if state.Completion == nil {
			return "", fmt.Errorf("没有图完成回执")
		}
		return fmt.Sprintf("图 %s 已结束（%s）：%s。交付回执：%s", task.FinalReportGraphID, state.Completion.Outcome, state.Completion.Summary, state.Completion.DeliveryRef), nil
	}
	return "", fmt.Errorf("未绑定图完成事实")
}
