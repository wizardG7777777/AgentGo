package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
)

// AgentTemplateGroup exposes Scheduler-only discovery and provisioning. It
// creates runtime Team resources, never DAG nodes; Scheduler must use the
// returned event_type in a later publish_task/ensure_acceptance_run call.
type AgentTemplateGroup struct {
	Catalog     *agenttemplate.Catalog
	Provisioner agenttemplate.Provisioner
	Coordinator *plan.Coordinator
	Store       store.TaskStore
	Holder      TaskHolder
}

func (g AgentTemplateGroup) Register(r *agent.ToolRegistry) {
	if g.Catalog == nil {
		return
	}
	r.Register("list_agent_templates", "列出不可变 AgentTemplate；模板只是可用蓝图，不代表已有可认领任务的 runtime route。",
		schema.Object().Build(), g.list)
	if g.Provisioner == nil || g.Coordinator == nil || g.Store == nil || g.Holder == nil {
		return
	}
	r.Register("provision_agent_team", "从一个精确版本模板创建 Plan-scoped Agent Team。成功返回 ready event_type；必须等下一轮读到真实返回值后再发布 Task，禁止猜测 route。",
		schema.Object().
			String("template_ref", "精确引用 namespace/name@version，例如 builtin/generalist@1", true).
			String("purpose", "该 Team 在当前 Plan 中的明确职责", true).
			Int("replicas", "同质副本数，默认 1，受模板和进程预算限制", false).
			Build(), g.provision)
}

func (g AgentTemplateGroup) list(_ context.Context, _ map[string]any) (string, error) {
	data, err := json.Marshal(g.Catalog.List())
	if err != nil {
		return "", fmt.Errorf("encode agent templates: %w", err)
	}
	return string(data), nil
}

func (g AgentTemplateGroup) provision(ctx context.Context, args map[string]any) (string, error) {
	controller, p, err := g.currentController()
	if err != nil {
		return "", err
	}
	ref, _ := args["template_ref"].(string)
	purpose, _ := args["purpose"].(string)
	ref = strings.TrimSpace(ref)
	purpose = strings.TrimSpace(purpose)
	if ref == "" || purpose == "" {
		return "", fmt.Errorf("template_ref and purpose are required")
	}
	replicas := 1
	if raw, exists := args["replicas"]; exists {
		switch value := raw.(type) {
		case int:
			replicas = value
		case float64:
			if value != math.Trunc(value) {
				return "", fmt.Errorf("replicas must be an integer")
			}
			if value < 1 || value > 32 {
				return "", fmt.Errorf("replicas must be between 1 and 32")
			}
			replicas = int(value)
		default:
			return "", fmt.Errorf("replicas must be an integer")
		}
	}
	if replicas < 1 || replicas > 32 {
		return "", fmt.Errorf("replicas must be between 1 and 32")
	}
	result, err := g.Provisioner.Provision(ctx, agenttemplate.ProvisionRequest{
		PlanID: p.ID, ControllerTaskID: controller.ID, TemplateRef: ref,
		Purpose: purpose, Replicas: replicas,
	})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode provision result: %w", err)
	}
	return string(data), nil
}

func (g AgentTemplateGroup) currentController() (*model.Task, *model.Plan, error) {
	taskID := g.Holder.Get()
	if taskID == "" {
		return nil, nil, fmt.Errorf("no current task context")
	}
	task, err := g.Store.GetTask(taskID)
	if err != nil {
		return nil, nil, err
	}
	if task.PlanID == "" || task.NodeRole != model.PlanNodeRoleController || task.EventType != "__scheduler__" {
		return nil, nil, fmt.Errorf("agent team provisioning requires a Scheduler controller task")
	}
	p, err := g.Coordinator.Store().GetPlan(task.PlanID)
	if err != nil {
		return nil, nil, err
	}
	if p.Status != model.PlanStatusRunning || p.ActiveDecisionTaskID != task.ID {
		return nil, nil, fmt.Errorf("controller task %s is not active for running plan %s", task.ID, p.ID)
	}
	return task, p, nil
}
