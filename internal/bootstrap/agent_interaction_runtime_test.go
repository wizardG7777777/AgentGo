package bootstrap

import (
	"context"
	"testing"

	"agentgo/internal/interaction"
	"agentgo/internal/tools"
)

func TestResolveInteractionAgentResponseCompletesWithoutPrivilegedEffect(t *testing.T) {
	service := interaction.NewService(nil)
	system := &System{Interactions: service}
	request, err := service.Create(context.Background(), interaction.CreateRequest{
		SessionID: "session-agent-question",
		Kind:      interaction.KindChoice,
		Purpose:   tools.PurposeAgentQuestion,
		Prompt:    "选择一个回答",
		Options: []interaction.Option{
			{ID: "first", Label: "第一项"},
			{ID: "second", Label: "第二项", RequiresText: true},
		},
		AllowFreeText: true,
		Origin: interaction.Origin{
			Component: "agent", AgentID: "worker-1", TaskID: "task-1",
		},
		Subject: interaction.Subject{Kind: "task", ID: "task-1", TaskID: "task-1"},
		Resolution: interaction.ResolutionSpec{
			Handler: tools.ResolutionHandlerAgentResponse, TargetID: "task-1",
			AgentID: "worker-1", TaskID: "task-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := system.resolveInteraction(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version,
		OptionID: "second", Text: "补充说明", RespondedBy: "tui",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != interaction.StateResolved || resolved.Response == nil {
		t.Fatalf("resolved = %+v", resolved)
	}
	if resolved.Response.OptionID != "second" || resolved.Response.Text != "补充说明" {
		t.Fatalf("response = %+v", resolved.Response)
	}
}
