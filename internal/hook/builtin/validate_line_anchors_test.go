package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/hook"
	"agentgo/internal/tools/hashline"
)

func TestValidateLineAnchorsHook_Metadata(t *testing.T) {
	h := NewValidateLineAnchorsHook()
	if h.Name() != "validate-line-anchors" {
		t.Errorf("Name = %q, want validate-line-anchors", h.Name())
	}
	if h.Phase() != hook.PhasePreCall {
		t.Errorf("Phase = %v, want PhasePreCall", h.Phase())
	}
	if h.Priority() != 25 {
		t.Errorf("Priority = %d, want 25", h.Priority())
	}
	for _, tool := range []string{"write_file", "edit_file"} {
		if !h.Matches(tool) {
			t.Errorf("Matches(%q) = false, want true", tool)
		}
	}
	for _, tool := range []string{"read_file", "list_dir", "grep_search"} {
		if h.Matches(tool) {
			t.Errorf("Matches(%q) = true, want false", tool)
		}
	}
}

func TestValidateLineAnchorsHook_NoAnchorsContinues(t *testing.T) {
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args:     map[string]any{"path": "/any/foo.go", "old_str": "x", "new_str": "y"},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when no line_anchors", d.Action)
	}
}

func TestValidateLineAnchorsHook_EmptyAnchorsContinues(t *testing.T) {
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          "/any/foo.go",
			"old_str":       "x",
			"new_str":       "y",
			"line_anchors":  []any{},
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when line_anchors empty", d.Action)
	}
}

func TestValidateLineAnchorsHook_MissingPathContinues(t *testing.T) {
	// path 缺失 → 让 PathBoundary 报错
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"line_anchors": []any{"1#VK"},
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when path missing", d.Action)
	}
}

func TestValidateLineAnchorsHook_FileNotExistContinues(t *testing.T) {
	// 文件不存在 → 允许新建
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "never-existed.go")
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args: map[string]any{
			"path":          missing,
			"content":       "x",
			"line_anchors":  []any{"1#VK"},
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue for new-file write", d.Action)
	}
}

func TestValidateLineAnchorsHook_MatchSuccess(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "foo.go")
	content := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 构造正确的 line_anchors
	anchors := []any{
		hashline.FormatHashLine(1, "package main"),
	}

	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          fp,
			"old_str":       "package main",
			"new_str":       "package hashline",
			"line_anchors":  anchors,
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (reason: %s)", d.Action, d.AbortReason)
	}
}

func TestValidateLineAnchorsHook_HashMismatch(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "bar.go")
	content := "package main\n\nfunc main() {\n}\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 故意给错误的哈希
	anchors := []any{"1#ZZ"}

	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          fp,
			"old_str":       "x",
			"new_str":       "y",
			"line_anchors":  anchors,
		},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v, want Abort on hash mismatch", d.Action)
	}
	if !strings.Contains(d.AbortReason, "行哈希校验失败") {
		t.Errorf("AbortReason should contain '行哈希校验失败', got: %s", d.AbortReason)
	}
	if !strings.Contains(d.AbortReason, ">>>") {
		t.Errorf("AbortReason should contain '>>>' highlight, got: %s", d.AbortReason)
	}
	// 错误消息应包含当前正确的哈希（不是期望的 ZZ）
	if strings.Contains(d.AbortReason, "1#ZZ") {
		t.Errorf("AbortReason should not contain the wrong expected hash 1#ZZ, got: %s", d.AbortReason)
	}
}

func TestValidateLineAnchorsHook_LineOutOfRange(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "small.go")
	if err := os.WriteFile(fp, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	anchors := []any{"99#VK"}
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          fp,
			"line_anchors":  anchors,
		},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort on out-of-range", d.Action)
	}
}

func TestValidateLineAnchorsHook_ParseError(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "x.go")
	if err := os.WriteFile(fp, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	anchors := []any{"not-a-valid-ref"}
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          fp,
			"line_anchors":  anchors,
		},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort on parse error", d.Action)
	}
}

func TestValidateLineAnchorsHook_ContextLines(t *testing.T) {
	// 验证失配时 ±2 上下文是否正确显示
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "ctx.go")
	lines := []string{"line1", "line2", "line3", "line4", "line5"}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// 第 3 行给错误哈希
	anchors := []any{"3#ZZ"}
	h := NewValidateLineAnchorsHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          fp,
			"line_anchors":  anchors,
		},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v, want Abort", d.Action)
	}
	// 应显示 1-5 行（3±2）
	for i := 1; i <= 5; i++ {
		if !strings.Contains(d.AbortReason, lines[i-1]) {
			t.Errorf("AbortReason should contain context line %d %q", i, lines[i-1])
		}
	}
}

func TestValidateLineAnchorsHook_ViaRegistry(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "reg.go")
	if err := os.WriteFile(fp, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := hook.NewToolHookRegistry()
	if err := reg.Register(NewValidateLineAnchorsHook()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// 错误的锚点
	d := reg.RunPre(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          fp,
			"line_anchors":  []any{"1#ZZ"},
		},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort via registry", d.Action)
	}
}

// TestValidateLineAnchorsHook_ChainOverridesExpectedHash 是 §7.7 互斥的双 hook 链路实测：
// expected_hash hook (prio=20) 与 line_anchors hook (prio=25) 都注册到 registry 时，
// 提供 line_anchors 的请求应当：
//   1. 让 expected_hash hook 在自检阶段直接 Continue（不论 expected_hash 是对是错）
//   2. 让 line_anchors hook 接管校验
//
// 现有 TestValidateExpectedHashHook_LineAnchorsSkips 只覆盖单 hook 单方面让位；
// 本测试用 registry 跑完整链路，把"哪一个 hook 在拒绝/放行"的决断暴露出来——
// 这是装配握手位测试，避免任何一侧的修改导致互斥失效。
func TestValidateLineAnchorsHook_ChainOverridesExpectedHash(t *testing.T) {
	tmp := t.TempDir()
	fp := filepath.Join(tmp, "chain.go")
	content := "alpha\nbeta\n"
	if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := hook.NewToolHookRegistry()
	if err := reg.Register(NewValidateExpectedHashHook()); err != nil {
		t.Fatalf("Register expected_hash: %v", err)
	}
	if err := reg.Register(NewValidateLineAnchorsHook()); err != nil {
		t.Fatalf("Register line_anchors: %v", err)
	}

	// 子用例 A：line_anchors 正确 + expected_hash 故意写错。
	// 期望：链路 Continue——expected_hash hook 见到 line_anchors 让位，line_anchors 验过。
	t.Run("LineAnchorsCorrect_ExpectedHashWrong_ChainContinues", func(t *testing.T) {
		correctAnchor := hashline.FormatHashLine(1, "alpha")
		d := reg.RunPre(hook.ToolHookContext{
			ToolName: "edit_file",
			Args: map[string]any{
				"path":          fp,
				"old_str":       "alpha",
				"new_str":       "ALPHA",
				"expected_hash": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", // 故意错
				"line_anchors":  []any{correctAnchor},
			},
		})
		if d.Action != hook.Continue {
			t.Errorf("Action = %v, want Continue (line_anchors 接管，expected_hash 让位)；reason: %s [hookName=%s]",
				d.Action, d.AbortReason, d.HookName)
		}
	})

	// 子用例 B：line_anchors 错误 + expected_hash 也错。
	// 期望：链路 Abort，且 abort 源必须是 line_anchors hook（不是 expected_hash）——
	// 后者必须仍然让位，否则就还原成"双校验都跑"的 stale 状态。
	t.Run("LineAnchorsWrong_ChainAbortsViaLineAnchorsHook", func(t *testing.T) {
		d := reg.RunPre(hook.ToolHookContext{
			ToolName: "edit_file",
			Args: map[string]any{
				"path":          fp,
				"old_str":       "alpha",
				"new_str":       "ALPHA",
				"expected_hash": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
				"line_anchors":  []any{"1#ZZ"}, // 错误锚点
			},
		})
		if d.Action != hook.Abort {
			t.Fatalf("Action = %v, want Abort", d.Action)
		}
		if d.HookName != "validate-line-anchors" {
			t.Errorf("Abort 源 hookName = %q, want validate-line-anchors（互斥退化迹象：expected_hash 重新介入了）",
				d.HookName)
		}
	})
}
