package ui

import "agentgo/internal/interaction"

// interactionItemFromRequest 只投影前端回答所需字段。服务器动作路由
// ActionRef、ResolutionSpec 与 Metadata 故意不在该类型中，避免客户端把
// “选择什么”升级为“执行什么”。
func interactionItemFromRequest(request interaction.Request) InteractionItem {
	item := InteractionItem{
		ID:            request.ID,
		Version:       request.Version,
		Kind:          string(request.Kind),
		Purpose:       string(request.Purpose),
		Prompt:        request.Prompt,
		AllowFreeText: request.AllowFreeText,
		SubjectKind:   request.Subject.Kind,
		SubjectID:     request.Subject.ID,
		PlanID:        request.Subject.PlanID,
		TaskID:        request.Subject.TaskID,
		AgentID:       request.Origin.AgentID,
		CreatedAt:     request.CreatedAt,
		ExpiresAt:     request.ExpiresAt,
	}
	if request.Options != nil {
		item.Options = make([]InteractionOption, 0, len(request.Options))
		for _, option := range request.Options {
			item.Options = append(item.Options, InteractionOption{
				ID: option.ID, Label: option.Label, Description: option.Description,
				RequiresText: option.RequiresText,
			})
		}
	}
	return item
}

func interactionItemsFromRequests(requests []interaction.Request) []InteractionItem {
	if len(requests) == 0 {
		return nil
	}
	items := make([]InteractionItem, 0, len(requests))
	for _, request := range requests {
		items = append(items, interactionItemFromRequest(request))
	}
	return items
}
