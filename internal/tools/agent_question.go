package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
)

const (
	// PurposeAgentQuestion 标识普通 Agent 向用户提出的结构化问题。
	// 它只把回答返回给原 Agent，不承载 Shell 授权或 Plan 控制副作用。
	PurposeAgentQuestion interaction.Purpose = "agent_question"

	// ResolutionHandlerAgentResponse 是 Bootstrap 无特权回答路由的稳定键。
	ResolutionHandlerAgentResponse = "agent_response"
)

type agentQuestionOption struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Description  string `json:"description,omitempty"`
	RequiresText bool   `json:"requires_text,omitempty"`
}

type agentQuestionResult struct {
	RequestID string `json:"request_id"`
	OptionID  string `json:"option_id"`
	Text      string `json:"text"`
}

func (g MetaGroup) requestUserInput(ctx context.Context, args map[string]any) (string, error) {
	if g.Interactions == nil {
		return "", fmt.Errorf("request_user_input: Interaction 服务不可用")
	}
	for key := range args {
		if key != "prompt" && key != "options_json" {
			return "", fmt.Errorf("request_user_input: 不支持参数 %q；仅允许 prompt 和 options_json", key)
		}
	}
	prompt, ok := args["prompt"].(string)
	prompt = strings.TrimSpace(prompt)
	if !ok || prompt == "" {
		return "", fmt.Errorf("request_user_input: prompt 必须是非空字符串")
	}
	optionsJSON, ok := args["options_json"].(string)
	if !ok || strings.TrimSpace(optionsJSON) == "" {
		return "", fmt.Errorf("request_user_input: options_json 必须是非空 JSON 字符串")
	}
	options, allowFreeText, err := parseAgentQuestionOptions(optionsJSON)
	if err != nil {
		return "", err
	}

	agentID := strings.TrimSpace(g.AgentID)
	contextAgentID := strings.TrimSpace(agent.AgentIDFromContext(ctx))
	if agentID == "" {
		agentID = contextAgentID
	} else if contextAgentID != "" && contextAgentID != agentID {
		return "", fmt.Errorf("request_user_input: Agent 绑定不匹配（runtime=%q, group=%q）", contextAgentID, agentID)
	}
	if agentID == "" {
		return "", fmt.Errorf("request_user_input: 无法确定当前 Agent ID")
	}
	taskID := strings.TrimSpace(agent.TaskIDFromContext(ctx))
	if taskID == "" && g.Holder != nil {
		taskID = strings.TrimSpace(g.Holder.Get())
	}
	if taskID == "" {
		return "", fmt.Errorf("request_user_input: 无法确定当前 Task ID")
	}

	sessionID := ""
	if g.SessionID != nil {
		sessionID = g.SessionID()
	}
	var expiresAt time.Time
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		expiresAt = deadline
	}
	request, err := g.Interactions.Create(ctx, interaction.CreateRequest{
		SessionID:     sessionID,
		Kind:          interaction.KindChoice,
		Purpose:       PurposeAgentQuestion,
		Prompt:        prompt,
		Options:       options,
		AllowFreeText: allowFreeText,
		Origin: interaction.Origin{
			Component: "agent",
			AgentID:   agentID,
			TaskID:    taskID,
		},
		Subject: interaction.Subject{
			Kind:   "task",
			ID:     taskID,
			TaskID: taskID,
		},
		Resolution: interaction.ResolutionSpec{
			Handler:  ResolutionHandlerAgentResponse,
			TargetID: taskID,
			AgentID:  agentID,
			TaskID:   taskID,
		},
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return "", fmt.Errorf("request_user_input: 创建 Interaction 失败: %w", err)
	}

	if g.InteractionWaitHook != nil {
		g.InteractionWaitHook(true)
		defer g.InteractionWaitHook(false)
	}
	resolved, err := g.Interactions.Await(ctx, request.ID)
	if err != nil {
		if ctx.Err() != nil {
			bestEffortInterruptAgentQuestion(g.Interactions, request.ID, "Agent 问题等待被取消或超时")
			return "", fmt.Errorf("request_user_input: 等待用户回答被取消或超时: %w", ctx.Err())
		}
		return "", fmt.Errorf("request_user_input: 用户回答未完成: %w", err)
	}
	if !matchesAgentQuestion(resolved, sessionID, agentID, taskID) {
		return "", fmt.Errorf("request_user_input: 用户回答与原 Agent/Task 请求不匹配")
	}
	if resolved.Response == nil {
		return "", fmt.Errorf("request_user_input: resolved Interaction 缺少回答")
	}
	result, err := json.Marshal(agentQuestionResult{
		RequestID: resolved.ID,
		OptionID:  resolved.Response.OptionID,
		Text:      resolved.Response.Text,
	})
	if err != nil {
		return "", fmt.Errorf("request_user_input: 编码回答失败: %w", err)
	}
	return string(result), nil
}

func parseAgentQuestionOptions(raw string) ([]interaction.Option, bool, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var inputs []agentQuestionOption
	if err := decoder.Decode(&inputs); err != nil {
		return nil, false, fmt.Errorf("request_user_input: options_json 无效: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("包含额外 JSON 值")
		}
		return nil, false, fmt.Errorf("request_user_input: options_json 无效: %w", err)
	}
	if len(inputs) < 2 || len(inputs) > 8 {
		return nil, false, fmt.Errorf("request_user_input: options_json 必须包含 2-8 个选项，当前为 %d", len(inputs))
	}
	options := make([]interaction.Option, 0, len(inputs))
	allowFreeText := false
	for _, input := range inputs {
		option := interaction.Option{
			ID:           input.ID,
			Label:        strings.TrimSpace(input.Label),
			Description:  strings.TrimSpace(input.Description),
			RequiresText: input.RequiresText,
		}
		options = append(options, option)
		allowFreeText = allowFreeText || option.RequiresText
	}
	return options, allowFreeText, nil
}

func matchesAgentQuestion(request interaction.Request, sessionID, agentID, taskID string) bool {
	if request.State != interaction.StateResolved ||
		request.Kind != interaction.KindChoice ||
		request.Purpose != PurposeAgentQuestion ||
		request.SessionID != sessionID ||
		request.Origin.Component != "agent" ||
		request.Origin.AgentID != agentID || request.Origin.TaskID != taskID ||
		request.Subject.Kind != "task" || request.Subject.ID != taskID || request.Subject.TaskID != taskID ||
		request.Resolution.Handler != ResolutionHandlerAgentResponse ||
		request.Resolution.TargetID != taskID ||
		request.Resolution.AgentID != agentID || request.Resolution.TaskID != taskID ||
		len(request.Metadata) != 0 {
		return false
	}
	selected, ok := request.SelectedOption()
	return ok && selected.ActionRef == ""
}

// bestEffortInterruptAgentQuestion 使用独立短上下文收尾，避免任务取消后留下
// 无消费者的 pending 请求。任何错误都只记录；工具仍保持 fail-closed。
func bestEffortInterruptAgentQuestion(interactions *interaction.Service, id, reason string) {
	if interactions == nil || id == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		request, err := interactions.Get(cleanupCtx, id)
		if err != nil || request.State.IsTerminal() {
			return
		}
		if _, err = interactions.Interrupt(cleanupCtx, id, request.Version, reason); err == nil {
			return
		}
		if !errors.Is(err, interaction.ErrVersionConflict) {
			log.Printf("[request_user_input] 收尾 Interaction %s 失败: %v", id, err)
			return
		}
	}
}
