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

// ================================================================
// ⚠️  2026-04-20 回归锁（预期红态 —— 请勿删除断言！）
// ================================================================
//
// 本节下列测试当前**故意失败**，用于锁定 P1-1 "Hook 错误消息不足以让 LLM 自愈" 缺陷：
// 在修复完成前它们应保持红灯。如果 CI 报这两个测试失败，**不是回归**，这是提醒
// bug 还没修。修复路径：让 PathBoundaryHook 按工具名分派参数字段（glob_search
// 应检查 root_dir；write_file/read_file 等保持检查 path），并在缺参时把正确字段
// 名写进错误消息。
//
// ❌ 错误处理：删除断言 / 改 Skip / 弱化期望 —— 这样会抹掉 bug 信号
// ✅ 正确处理：修 path_boundary.go 让它尊重各工具的 schema，此处自动变绿
//
// 背景（bug 现象）：2026-04-20 并发测试中 explorer 连续 8 次调 glob_search 都传
// root_dir（工具 schema 声明的合法字段），每次都被 hook 以"缺少 path 参数"拒绝，
// LLM 读错误消息后无法修正，浪费 8 轮 loop + 大量 token。
//
// 参见：docs/activate/KNOWN_ISSUES.md "Hook 错误消息不足以让 LLM 自愈"
// ================================================================

// TestPathBoundaryHook_GlobSearch_AcceptsRootDir 断言 hook 尊重 glob_search 工具
// schema 声明的参数名（root_dir）。修复前本测试失败（hook 硬编码 path）。
func TestPathBoundaryHook_GlobSearch_AcceptsRootDir(t *testing.T) {
	root := t.TempDir()
	h := NewPathBoundaryHook(root)
	d := h.Run(hook.ToolHookContext{
		ToolName: "glob_search",
		Args:     map[string]any{"root_dir": root, "pattern": "**/*.md"},
	})
	if d.Action == hook.Abort {
		t.Errorf("glob_search with root_dir 应被接受（与 schema 一致），实际被拒: %s", d.AbortReason)
	}
}

// TestPathBoundaryHook_GlobSearch_MissingAllPathArgs_ErrorHintsCorrectField 断言
// 当 glob_search 完全缺少路径参数时，错误消息须提示正确字段名 root_dir，
// 让 LLM 能自愈而不是陷入重复错误循环。
func TestPathBoundaryHook_GlobSearch_MissingAllPathArgs_ErrorHintsCorrectField(t *testing.T) {
	h := NewPathBoundaryHook(t.TempDir())
	d := h.Run(hook.ToolHookContext{
		ToolName: "glob_search",
		Args:     map[string]any{"pattern": "**/*.md"},
	})
	if d.Action != hook.Abort {
		t.Fatalf("glob_search 完全缺参时应 Abort, got %v", d.Action)
	}
	if !strings.Contains(d.AbortReason, "root_dir") {
		t.Errorf("错误消息应提及正确字段名 root_dir 以便 LLM 自愈, 实际: %q", d.AbortReason)
	}
}
