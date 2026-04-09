package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/hook"
)

// ---- Interface and metadata ----

func TestPathBoundaryHook_ImplementsToolHook(t *testing.T) {
	var _ hook.ToolHook = (*PathBoundaryHook)(nil)
}

func TestPathBoundaryHook_Metadata(t *testing.T) {
	h := NewPathBoundaryHook("/project")
	if h.Name() != "path-boundary" {
		t.Errorf("Name = %q, want path-boundary", h.Name())
	}
	if h.Phase() != hook.PhasePreCall {
		t.Errorf("Phase = %v, want PhasePreCall", h.Phase())
	}
	if h.Priority() != 10 {
		t.Errorf("Priority = %d, want 10", h.Priority())
	}
}

func TestPathBoundaryHook_MatchesFileSystemTools(t *testing.T) {
	h := NewPathBoundaryHook("/project")
	cases := map[string]bool{
		// 应当匹配 — 文件系工具
		"read_file":   true,
		"list_dir":    true,
		"list_files":  true,
		"grep_search": true,
		"glob_search": true,
		"write_file":  true,
		"edit_file":   true,
		// 应当不匹配 — 决策：不包含 run_shell（无 path 参数）
		"run_shell": false,
		// 应当不匹配 — 网络/协作工具
		"web_search":   false,
		"web_fetch":    false,
		"publish_task": false,
		"send_message": false,
	}
	for tool, want := range cases {
		t.Run(tool, func(t *testing.T) {
			if got := h.Matches(tool); got != want {
				t.Errorf("Matches(%q) = %v, want %v", tool, got, want)
			}
		})
	}
}

// ---- Path validation: happy paths ----

func TestPathBoundaryHook_RelativePathInsideRootContinue(t *testing.T) {
	root := t.TempDir()
	h := NewPathBoundaryHook(root)
	d := h.Run(hook.ToolHookContext{
		ToolName: "read_file",
		Args:     map[string]any{"path": "docs/foo.md"},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (reason: %s)", d.Action, d.AbortReason)
	}
}

func TestPathBoundaryHook_AbsolutePathInsideRootContinue(t *testing.T) {
	root := t.TempDir()
	h := NewPathBoundaryHook(root)
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args:     map[string]any{"path": filepath.Join(root, "out.md")},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (reason: %s)", d.Action, d.AbortReason)
	}
}

func TestPathBoundaryHook_EmptyProjectRootContinues(t *testing.T) {
	// projectRoot 为空 → pathutil.ValidatePath 直接返回 nil error，hook Continue
	h := NewPathBoundaryHook("")
	d := h.Run(hook.ToolHookContext{
		ToolName: "read_file",
		Args:     map[string]any{"path": "/anywhere/file.md"},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when projectRoot is empty", d.Action)
	}
}

// ---- Path validation: abort paths ----

func TestPathBoundaryHook_TraversalEscapeAbort(t *testing.T) {
	root := t.TempDir()
	h := NewPathBoundaryHook(root)
	cases := []string{
		"../etc/passwd",
		"docs/../../../etc/shadow",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			d := h.Run(hook.ToolHookContext{
				ToolName: "read_file",
				Args:     map[string]any{"path": p},
			})
			if d.Action != hook.Abort {
				t.Errorf("Action = %v, want Abort for traversal path %q", d.Action, p)
			}
			if d.AbortReason == "" {
				t.Error("AbortReason should not be empty on Abort")
			}
			if d.HookName != "path-boundary" {
				t.Errorf("HookName = %q, want path-boundary", d.HookName)
			}
		})
	}
}

func TestPathBoundaryHook_AbsolutePathOutsideRootAbort(t *testing.T) {
	root := t.TempDir()
	h := NewPathBoundaryHook(root)

	// 选一个肯定不在 t.TempDir() 之下的绝对路径
	outside := filepath.Join(os.TempDir(), "definitely-outside-test-root", "x.md")
	// 防止 outside 凑巧在 root 下
	if strings.HasPrefix(outside, root) {
		t.Skip("test temp dirs nest unexpectedly, skip")
	}
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args:     map[string]any{"path": outside},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort for outside-root path %q", d.Action, outside)
	}
}

func TestPathBoundaryHook_SensitiveFileAbort(t *testing.T) {
	root := t.TempDir()
	h := NewPathBoundaryHook(root)
	cases := []string{
		filepath.Join(root, ".env"),
		filepath.Join(root, "secrets", ".ssh", "id_rsa"),
		filepath.Join(root, "config", "credentials"),
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			d := h.Run(hook.ToolHookContext{
				ToolName: "read_file",
				Args:     map[string]any{"path": p},
			})
			if d.Action != hook.Abort {
				t.Errorf("Action = %v, want Abort for sensitive path %q", d.Action, p)
			}
		})
	}
}

// ---- Args validation: missing / wrong type / empty ----

func TestPathBoundaryHook_MissingPathArgAbort(t *testing.T) {
	h := NewPathBoundaryHook("/project")
	d := h.Run(hook.ToolHookContext{
		ToolName: "read_file",
		Args:     map[string]any{}, // 没有 path key
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort when path missing", d.Action)
	}
}

func TestPathBoundaryHook_NonStringPathAbort(t *testing.T) {
	h := NewPathBoundaryHook("/project")
	cases := []any{
		123,
		nil,
		[]string{"a"},
		map[string]any{},
		true,
	}
	for _, v := range cases {
		t.Run("non-string", func(t *testing.T) {
			d := h.Run(hook.ToolHookContext{
				ToolName: "read_file",
				Args:     map[string]any{"path": v},
			})
			if d.Action != hook.Abort {
				t.Errorf("Action = %v, want Abort for non-string path %T", d.Action, v)
			}
		})
	}
}

func TestPathBoundaryHook_EmptyStringPathAbort(t *testing.T) {
	h := NewPathBoundaryHook("/project")
	d := h.Run(hook.ToolHookContext{
		ToolName: "read_file",
		Args:     map[string]any{"path": ""},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort for empty path", d.Action)
	}
}

// ---- E2E via registry: PathBoundary aborts before tool runs ----

func TestPathBoundaryHook_ViaRegistryAbort(t *testing.T) {
	root := t.TempDir()
	reg := hook.NewToolHookRegistry()
	if err := reg.Register(NewPathBoundaryHook(root)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := reg.RunPre(hook.ToolHookContext{
		ToolName: "write_file",
		Args:     map[string]any{"path": "../etc/passwd"},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort", d.Action)
	}
}
