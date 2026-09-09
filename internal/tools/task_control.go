package tools

import (
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"context"
	"fmt"
	"strings"
)

func validateFinalReportScope(task *model.Task) error {
	if task == nil {
		return nil
	}
	finalSignal := task.EventSource == "graph-ended" || strings.TrimSpace(task.FinalReportGraphID) != "" ||
		task.RunPhase == runcontract.PhaseFinalization
	if !finalSignal {
		return nil
	}
	if task.EventSource == "graph-ended" && strings.TrimSpace(task.FinalReportGraphID) == "" {
		return fmt.Errorf("graph-ended final-report 缺少 final_report_graph_id")
	}
	scope, err := model.ClassifyControlScope(task)
	if err != nil || scope != model.ControlScopeFinalReport {
		return fmt.Errorf("final_report_graph_id=%q 与 task scope 不一致: %v", task.FinalReportGraphID, err)
	}
	return nil
}

func GuardedCancel(_ context.Context, s store.TaskStore, targetTaskID, source string) error {
	if _, err := s.GetTask(targetTaskID); err != nil {
		return fmt.Errorf("取消任务失败 (id=%s): %w", targetTaskID, err)
	}
	err := store.TransitionStateWithCancelSource(s, targetTaskID, model.TaskStatusPending, model.TaskStatusCancelled, source)
	if err != nil {
		err = store.TransitionStateWithCancelSource(s, targetTaskID, model.TaskStatusProcessing, model.TaskStatusCancelled, source)
	}
	if err != nil {
		return fmt.Errorf("取消任务失败 (id=%s): %w", targetTaskID, err)
	}
	return nil
}
