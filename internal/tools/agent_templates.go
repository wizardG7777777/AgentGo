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
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
)

// AgentTemplateGroup exposes Scheduler-only discovery and provisioning. It
// creates runtime Team resources, never DAG nodes; Scheduler must use the
// returned event_type in a later publish_task call.
type AgentTemplateGroup struct {
	Catalog     *agenttemplate.Catalog
	Provisioner agenttemplate.Provisioner
	Store       store.TaskStore
	Holder      TaskHolder
}

func (g AgentTemplateGroup) Register(r *agent.ToolRegistry) {
	if g.Catalog == nil {
		return
	}
	r.Register("list_agent_templates", "列出不可变 AgentTemplate；模板只是可用蓝图，不代表已有可认领任务的 runtime route。",
		schema.Object().Build(), g.list)
	if g.Provisioner == nil || g.Store == nil || g.Holder == nil {
		return
	}
	r.Register("provision_agent_team", "从一个精确版本模板创建按发起 controller 任务归属的 Agent Team。成功返回 ready event_type；必须等下一轮读到真实返回值后再发布 Task，禁止猜测 route。",
		schema.Object().
			String("template_ref", "精确引用 namespace/name@version，例如 builtin/generalist@1", true).
			String("purpose", "该 Team 的职责", true).
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
	task, err := g.currentController()
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
		ControllerTaskID: task.ID, TemplateRef: ref,
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

// currentController 取当前任务，并要求它是正在执行的 Scheduler 任务
// （EventType == "__scheduler__" 且 Status == processing）。
func (g AgentTemplateGroup) currentController() (*model.Task, error) {
	taskID := g.Holder.Get()
	if taskID == "" {
		return nil, fmt.Errorf("no current task context")
	}
	task, err := g.Store.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task.EventType != "__scheduler__" || task.Status != model.TaskStatusProcessing {
		return nil, fmt.Errorf("agent team provisioning requires a running Scheduler task")
	}
	return task, nil
}
