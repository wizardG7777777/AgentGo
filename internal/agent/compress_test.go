package agent

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/llm"
)

func TestIsContextOverflow(t *testing.T) {
	contextWindow := llm.NewFailure(llm.FailureContextWindowExceeded,
		llm.PhaseResponseHeaders, llm.OriginProvider,
		errors.New("context window exceeded"))
	requestTimeout := llm.NewFailure(llm.FailureRequestTimeout,
		llm.PhaseRequestSend, llm.OriginTransport,
		context.DeadlineExceeded)
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"仅含 length 文本", errors.New("finish_reason=length"), false},
		{"仅含截断文本", errors.New("响应被截断"), false},
		{"仅含 context 文本", errors.New("context window exceeded"), false},
		{"typed context window", contextWindow, true},
		{"request deadline 阴性", requestTimeout, false},
		{"normal error", errors.New("rate limit exceeded"), false},
		{"auth error", errors.New("invalid api key"), false},
		{"wrapped typed context window", &ErrRecoverable{Err: contextWindow}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := llm.IsContextWindowExceeded(tt.err)
			if got != tt.expected {
				t.Errorf("IsContextWindowExceeded(%q) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}
