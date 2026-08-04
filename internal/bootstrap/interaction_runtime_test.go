package bootstrap

import (
	"context"
	"testing"

	"agentgo/internal/interaction"
)

func TestSystemInterruptPendingInteractions(t *testing.T) {
	service := interaction.NewService(interaction.NewMemoryStore())
	created, err := service.Create(context.Background(), interaction.CreateRequest{
		ID: "ix_shutdown", Kind: interaction.KindConfirmation,
		Purpose: "shutdown_test", Prompt: "continue?",
		Options:    []interaction.Option{{ID: "continue", Label: "continue", ActionRef: "continue"}},
		Resolution: interaction.ResolutionSpec{Handler: "test_handler"},
	})
	if err != nil {
		t.Fatal(err)
	}
	system := &System{Interactions: service}
	system.interruptPendingInteractions("shutdown")
	stored, err := service.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != interaction.StateInterrupted || stored.StatusReason != "shutdown" {
		t.Fatalf("interaction after shutdown = %+v", stored)
	}
}
