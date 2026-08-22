package agent

import (
	"context"
	"errors"
	"strings"
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
	}, func(ctx context.Context, args map[string]any) (string, error) {
		return "", nil
	})

	r.Register("grep", "搜索内容", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string"},
		},
	}, func(ctx context.Context, args map[string]any) (string, error) {
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

func TestToolRegistryAllowlistDistinguishesNilAndEmpty(t *testing.T) {
	register := func(r *ToolRegistry) {
		r.Register("read_file", "读取文件", nil, func(context.Context, map[string]any) (string, error) {
			return "", nil
		})
	}

	compat := NewToolRegistryWithAllowlist(nil)
	register(compat)
	if compat.RegisteredCount() != 1 {
		t.Fatalf("nil allowlist should retain explicit compatibility semantics, got %d tools", compat.RegisteredCount())
	}

	denyAll := NewToolRegistryWithAllowlist([]string{})
	register(denyAll)
	if denyAll.RegisteredCount() != 0 {
		t.Fatalf("empty allowlist must deny all tools, got %d", denyAll.RegisteredCount())
	}
}

func TestToolRegistry_Dispatch_Success(t *testing.T) {
	r := NewToolRegistry()
	r.Register("echo", "回显", nil, func(ctx context.Context, args map[string]any) (string, error) {
		text, _ := args["text"].(string)
		return "echo: " + text, nil
	})

	result, err := r.Dispatch(context.Background(), llm.ToolCall{
		ID:        "call_1",
		Name:      "echo",
		Arguments: map[string]any{"text": "hello"},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "echo: hello" {
		t.Errorf("result = %q, want %q", result, "echo: hello")
	}
}

func TestToolRegistryNormalizeCallAppliesDeclaredDefaultsWithoutMutatingProviderArgs(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterWithDefaults("list_dir", "list", nil, map[string]any{"path": ".", "depth": 1},
		func(context.Context, map[string]any) (string, error) { return "", nil })
	original := llm.ToolCall{ID: "call-1", Name: "list_dir", Arguments: map[string]any{"depth": 2}}
	normalized := r.NormalizeCall(original)
	if normalized.Arguments["path"] != "." || normalized.Arguments["depth"] != 2 {
		t.Fatalf("normalized=%+v", normalized.Arguments)
	}
	if _, mutated := original.Arguments["path"]; mutated {
		t.Fatal("NormalizeCall 不得修改 provider 原始参数 map")
	}
	filtered := r.Filtered([]string{"list_dir"})
	if got := filtered.NormalizeCall(llm.ToolCall{Name: "list_dir"}).Arguments["path"]; got != "." {
		t.Fatalf("过滤 Lease 丢失工具默认值: %v", got)
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

// TestToolRegistry_Dispatch_MalformedToolName（2026-08-21 SWE-009）：模型往
// tool_call 名字段泄漏 DSML 等标记的畸形名，回执不回灌原名——改用与账本
// 一致的确定性占位并附格式提醒；合法 typo 名仍走 Did-You-Mean 建议。
func TestToolRegistry_Dispatch_MalformedToolName(t *testing.T) {
	r := NewToolRegistry()
	r.Register("grep_search", "搜索", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "ok", nil
	})

	cases := []struct {
		name    string
		rawName string
	}{
		{"DSML 长名", "grep_search>\n<｜DSML｜parameter pattern>path</｜DSML｜parameter>\n</｜DSML｜invoke>"},
		{"含换行控制符", "read_file\n<script>"},
		{"超长名", strings.Repeat("a", 200)},
		{"CJK 名", "读文件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Dispatch(context.Background(), llm.ToolCall{ID: "c1", Name: tc.rawName})
			if err == nil {
				t.Fatal("畸形名应报错")
			}
			msg := err.Error()
			if strings.Contains(msg, tc.rawName) {
				t.Errorf("回执不得回灌原始畸形名: %q", msg)
			}
			if !strings.Contains(msg, "malformed:") || !strings.Contains(msg, "标记字符") {
				t.Errorf("回执应含确定性占位与格式提醒: %q", msg)
			}
		})
	}

	// 合法 typo 名不受影响，仍给出 Did-You-Mean 建议
	_, err := r.Dispatch(context.Background(), llm.ToolCall{ID: "c2", Name: "grep_searh"})
	if err == nil || !strings.Contains(err.Error(), "grep_searh") {
		t.Errorf("合法 typo 名应保留原名与建议: %v", err)
	}
}

func TestToolRegistry_Dispatch_ToolError(t *testing.T) {
	r := NewToolRegistry()
	r.Register("fail_tool", "总是失败", nil, func(ctx context.Context, args map[string]any) (string, error) {
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
	r.Register("ctx_tool", "检查 context", nil, func(ctx context.Context, args map[string]any) (string, error) {
		receivedCtx = ctx
		return "ok", nil
	})

	ctx := context.WithValue(context.Background(), "key", "value")
	r.Dispatch(ctx, llm.ToolCall{ID: "call_1", Name: "ctx_tool"})

	if receivedCtx != ctx {
		t.Error("tool did not receive the correct context")
	}
}

func TestToolRegistry_Dispatch_DidYouMean(t *testing.T) {
	r := NewToolRegistry()
	r.Register("read_file", "读取文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "", nil
	})
	r.Register("write_file", "写入文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "", nil
	})
	r.Register("run_shell", "执行 shell", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "", nil
	})

	_, err := r.Dispatch(context.Background(), llm.ToolCall{
		ID:   "call_1",
		Name: "readFile", // typo：骆驼拼写，缺少下划线
	})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("expected 'Did you mean' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "read") || !strings.Contains(err.Error(), "file") {
		t.Errorf("expected 'read_file' (or highlighted variant) in error, got: %v", err)
	}
}

func TestToolRegistry_Dispatch_NoDidYouMeanWhenNoCandidate(t *testing.T) {
	r := NewToolRegistry()
	r.Register("aaa", "工具A", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "", nil
	})

	_, err := r.Dispatch(context.Background(), llm.ToolCall{
		ID:   "call_1",
		Name: "zzz_xxx_yyy", // 完全无关，无候选
	})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("expected no 'Did you mean' when no candidate, got: %v", err)
	}
}
