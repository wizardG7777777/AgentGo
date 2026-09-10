package ui

import "context"

// GraphInputRequest 提交已声明输入的不可变版本，不是瞬时控制事件。
type GraphInputRequest struct {
	GraphID          string `json:"graph_id"`
	Port             string `json:"port"`
	Version          int64  `json:"version"`
	ExpectedRevision int64  `json:"expected_revision"`
	RequestID        string `json:"request_id"`
	Value            any    `json:"value"`
}

func (h *Hub) ProvideGraphInput(ctx context.Context, input GraphInputRequest) error {
	if h.deps.ProvideGraphInput == nil {
		return notAssembled("ProvideGraphInput")
	}
	return h.deps.ProvideGraphInput(ctx, input)
}
