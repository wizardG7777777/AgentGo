package model

import (
	"fmt"
	"strings"

	"agentgo/internal/runcontract"
)

type ControlScopeKind string

const (
	ControlScopeLegacy        ControlScopeKind = "legacy"
	ControlScopeGraph         ControlScopeKind = "graph_activation"
	ControlScopeGraphRecovery ControlScopeKind = "graph_recovery"
	ControlScopeFinalReport   ControlScopeKind = "final_report"
	ControlScopeRootAuthoring ControlScopeKind = "root_authoring"
	ControlScopeIntervention  ControlScopeKind = "graph_intervention"
)

// ClassifyControlScope 是 L3 ToolRouter、L4 intervention 与 L5 bridge 共用的
// 唯一任务控制身份分类。它只读取持久化字段，不解析 Description marker。
func ClassifyControlScope(task *Task) (ControlScopeKind, error) {
	if task == nil {
		return ControlScopeLegacy, fmt.Errorf("ControlScope Task 不能为空")
	}
	finalID := strings.TrimSpace(task.FinalReportGraphID)
	finalSignal := finalID != "" || task.RunPhase == runcontract.PhaseFinalization || task.EventSource == "graph-ended"
	if finalSignal {
		if finalID == "" || task.RunPhase != runcontract.PhaseFinalization || task.EventSource != "graph-ended" ||
			task.GraphID != "" || task.InterventionGraphID != "" || task.EventType != "__scheduler__" {
			return ControlScopeLegacy, fmt.Errorf("final-report scope binding 不完整或冲突")
		}
		return ControlScopeFinalReport, nil
	}
	if task.GraphID != "" {
		if task.GraphNodeKind == "controller" && task.GraphControllerRole == "loop_recovery" {
			return ControlScopeGraphRecovery, nil
		}
		return ControlScopeGraph, nil
	}
	if task.InterventionGraphID != "" {
		if task.RunPhase != runcontract.PhaseRecovery || task.EventSource != TaskEventSourceLoopIntervention {
			return ControlScopeLegacy, fmt.Errorf("Graph intervention scope binding 不完整")
		}
		return ControlScopeIntervention, nil
	}
	if task.EventType == "__scheduler__" && task.RunContract != nil {
		return ControlScopeRootAuthoring, nil
	}
	return ControlScopeLegacy, nil
}
