package agent

import (
	"context"
	"errors"
	"testing"

	"agentgo/internal/llm"
)

func TestToolRegistry_Register_And_Defs(t *testing.T) {
	r := NewToolRegistry()

	r.Register("read_file", "读取文件", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
	}, func(ctx context.Context, args map[string]string) (string, error) {
		return "", nil
	})

	r.Register("grep", "搜索内容", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string"},
		},
	}, func(ctx context.Context, args map[string]string) (string, error) {
		return "", nil
	})

	defs := r.Defs()
	if len(defs) != 2 {
		t.Fatalf("defs count = %d, want 2", len(defs))
	}
	if defs[0].Name != "read_file" {
		t.Errorf("defs[0].Name = %q, want %q", defs[0].Name, "read_file")
	}
	if defs[1].Name != "grep" {
		t.Errorf("defs[1].Name = %q, want %q", defs[1].Name, "grep")
	}
}

func TestToolRegistry_Dispatch_Success(t *testing.T) {
	r := NewToolRegistry()
	r.Register("echo", "回显", nil, func(ctx context.Context, args map[string]string) (string, error) {
		return "echo: " + args["text"], nil
	})

	result, err := r.Dispatch(context.Background(), llm.ToolCall{
		ID:        "call_1",
		Name:      "echo",
		Arguments: map[string]string{"text": "hello"},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "echo: hello" {
		t.Errorf("result = %q, want %q", result, "echo: hello")
	}
}

func TestToolRegistry_Dispatch_UnknownTool(t *testing.T) {
	r := NewToolRegistry()

	_, err := r.Dispatch(context.Background(), llm.ToolCall{
		ID:   "call_1",
		Name: "nonexistent",
	})

	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestToolRegistry_Dispatch_ToolError(t *testing.T) {
	r := NewToolRegistry()
	r.Register("fail_tool", "总是失败", nil, func(ctx context.Context, args map[string]string) (string, error) {
		return "", errors.New("tool failed")
	})

	_, err := r.Dispatch(context.Background(), llm.ToolCall{
		ID:   "call_1",
		Name: "fail_tool",
	})

	if err == nil || err.Error() != "tool failed" {
		t.Errorf("expected 'tool failed' error, got %v", err)
	}
}

func TestToolRegistry_Dispatch_Context(t *testing.T) {
	r := NewToolRegistry()

	var receivedCtx context.Context
	r.Register("ctx_tool", "检查 context", nil, func(ctx context.Context, args map[string]string) (string, error) {
		receivedCtx = ctx
		return "ok", nil
	})

	ctx := context.WithValue(context.Background(), "key", "value")
	r.Dispatch(ctx, llm.ToolCall{ID: "call_1", Name: "ctx_tool"})

	if receivedCtx != ctx {
		t.Error("tool did not receive the correct context")
	}
}
