package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
)

// AgentTemplateGroup exposes Scheduler-only discovery and provisioning. It
// creates runtime Team resources, never Graph nodes. Graph-first callers bind
// the Team to their chosen graph_id before using the returned event_type in a
// node metadata.route; omitting graph_id preserves the legacy task scope.
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
	r.Register("provision_agent_team", "从精确版本模板创建 Agent Team。Graph-first 路径必须预先选定 graph_id 并在本调用传入，Team 将存活到该 Graph 终态；省略 graph_id 仅保留 legacy task-scoped 语义。成功返回 ready event_type，必须等下一轮读到真实值后再写入节点 route，禁止猜测。",
		schema.Object().
			String("template_ref", "精确引用 namespace/name@version，例如 builtin/generalist@1", true).
			String("purpose", "该 Team 的职责", true).
			String("graph_id", "Graph-first 路径的全局唯一 Graph ID；必须与随后 submit_graph 的 graph_id 完全一致。Graph controller 内可省略以自动继承当前 Graph", false).
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
	graphID, _ := args["graph_id"].(string)
	ref = strings.TrimSpace(ref)
	purpose = strings.TrimSpace(purpose)
	graphID = strings.TrimSpace(graphID)
	if ref == "" || purpose == "" {
		return "", fmt.Errorf("template_ref and purpose are required")
	}
	if task.GraphID != "" {
		if graphID == "" {
			graphID = task.GraphID
		} else if graphID != task.GraphID {
			return "", fmt.Errorf("graph_id %q 与当前 Graph %q 不一致，拒绝跨 Graph provision", graphID, task.GraphID)
		}
	}
	if graphID != "" {
		if err := graph.ValidateGraphID(graphID); err != nil {
			return "", fmt.Errorf("graph_id 非法: %w", err)
		}
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
		ControllerTaskID: task.ID, GraphID: graphID, TemplateRef: ref,
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
