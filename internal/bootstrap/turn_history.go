package bootstrap

import (
	"agentgo/internal/session"
	"agentgo/internal/ui"
)

func uiTurnsFromSession(records []session.TurnRecord) []ui.AgentTurn {
	if len(records) == 0 {
		return nil
	}
	turns := make([]ui.AgentTurn, len(records))
	for i, record := range records {
		turns[i] = ui.AgentTurn{
			ID:          record.ID,
			SessionID:   record.SessionID,
			AgentID:     record.AgentID,
			TaskID:      record.TaskID,
			Loop:        record.Loop,
			Text:        record.Text,
			Reasoning:   record.Reasoning,
			Status:      record.Status,
			ToolCalls:   append([]string(nil), record.ToolCalls...),
			StartedAt:   record.StartedAt,
			CompletedAt: record.CompletedAt,
			Error:       record.Error,
		}
	}
	return turns
}

func sessionTurnFromUI(turn ui.AgentTurn) session.TurnRecord {
	return session.TurnRecord{
		ID:          turn.ID,
		SessionID:   turn.SessionID,
		AgentID:     turn.AgentID,
		TaskID:      turn.TaskID,
		Loop:        turn.Loop,
		Text:        turn.Text,
		Reasoning:   turn.Reasoning,
		Status:      turn.Status,
		ToolCalls:   append([]string(nil), turn.ToolCalls...),
		StartedAt:   turn.StartedAt,
		CompletedAt: turn.CompletedAt,
		Error:       turn.Error,
	}
}
