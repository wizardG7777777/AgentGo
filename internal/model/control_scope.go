package model

import (
	"fmt"
)

type ControlScopeKind string

const (
	ControlScopeTask          ControlScopeKind = "task"
	ControlScopeGraph         ControlScopeKind = "agent_task"
	ControlScopeFinalReport   ControlScopeKind = "final_report"
	ControlScopeRootAuthoring ControlScopeKind = "root_authoring"
	ControlScopePlanning      ControlScopeKind = "graph_planning"
)

func ClassifyControlScope(task *Task) (ControlScopeKind, error) {
	if task == nil {
		return "", fmt.Errorf("任务身份为空")
	}
	if task.FinalReportGraphID != "" {
		if task.GraphID != "" || task.InterventionGraphID != "" || task.EventType != "__scheduler__" || task.EventSource != "dataflow-planning" {
			return "", fmt.Errorf("图最终答复身份冲突")
		}
		return ControlScopeFinalReport, nil
	}
	if task.GraphID != "" {
		return ControlScopeGraph, nil
	}
	if task.InterventionGraphID != "" {
		if task.EventType != "__scheduler__" || task.EventSource != "dataflow-planning" {
			return "", fmt.Errorf("图规划身份冲突")
		}
		return ControlScopePlanning, nil
	}
	if task.EventSource == "dataflow-planning" {
		return "", fmt.Errorf("缺少图规划/最终答复身份")
	}
	if task.EventType == "__scheduler__" {
		return ControlScopeRootAuthoring, nil
	}
	return ControlScopeTask, nil
}
