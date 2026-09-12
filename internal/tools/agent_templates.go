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

// AgentTemplateGroup 供编排者发现模板并创建 Team；初建使用 graph_request_id
// 与 apply_graph_change 的 request_id 绑定同一个未来图，不重新暴露草案工具。
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
	r.Register("provision_agent_team", "图外 Scheduler 从精确版本模板创建 Team。初建提供 graph_request_id，与后续 apply_graph_change(create) 的 request_id 完全一致；重规划任务继承其关联图。返回实际 graph_id 和 ready event_type，作为 agentTask.execution.route_ref；不创建图或脱图任务。",
		schema.Object().
			String("template_ref", "精确引用 namespace/name@version，例如 builtin/generalist@2", true).
			String("purpose", "该 Team 的职责", true).
			String("graph_id", "重规划时核对任务关联图；省略则继承。首次建图不填写此字段", false).
			String("graph_request_id", "初建时与 apply_graph_change 的 request_id 使用同一稳定值，不能与 graph_id 混用", false).
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
	for key, value := range args {
		switch key {
		case "template_ref", "purpose", "graph_id", "graph_request_id":
			if _, ok := value.(string); !ok {
				return "", fmt.Errorf("%s 必须是字符串", key)
			}
		case "replicas":
		default:
			return "", fmt.Errorf("provision_agent_team 不支持参数 %q", key)
		}
	}
	ref, _ := args["template_ref"].(string)
	purpose, _ := args["purpose"].(string)
	graphID, _ := args["graph_id"].(string)
	graphRequestID, _ := args["graph_request_id"].(string)
	ref, purpose, graphID = strings.TrimSpace(ref), strings.TrimSpace(purpose), strings.TrimSpace(graphID)
	if ref == "" || purpose == "" {
		return "", fmt.Errorf("template_ref and purpose are required")
	}
	bound := task.GraphID
	if bound == "" {
		bound = task.InterventionGraphID
	}
	if bound != "" {
		if graphRequestID != "" {
			return "", fmt.Errorf("重规划已有图不能声明新建 graph_request_id")
		}
		if graphID != "" && graphID != bound {
			return "", fmt.Errorf("graph_id 与当前 Graph 不一致，拒绝跨 Graph provision")
		}
		graphID = bound
	} else {
		if graphID != "" || strings.TrimSpace(graphRequestID) == "" || len(graphRequestID) > 256 {
			return "", fmt.Errorf("初建 Team 必须提供 graph_request_id，不能猜测 graph_id 或创建脱图 Team")
		}
		graphID = "graph-" + graphRequestKey(task.ID, graphRequestID)
	}
	if err := graph.ValidateGraphID(graphID); err != nil {
		return "", fmt.Errorf("graph_id 非法: %w", err)
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
	if task.FinalReportGraphID != "" || task.GraphID != "" {
		return nil, fmt.Errorf("只有图外 Scheduler 可以创建 Team")
	}
	return task, nil
}
