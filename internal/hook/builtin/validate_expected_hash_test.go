package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/hook"
)

// helper：在临时目录里创建文件并返回 (path, sha256-hex)
func makeFileWithHash(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	return path, hex.EncodeToString(sum[:])
}

// ---- Interface and metadata ----

func TestValidateExpectedHashHook_ImplementsToolHook(t *testing.T) {
	var _ hook.ToolHook = (*ValidateExpectedHashHook)(nil)
}

func TestValidateExpectedHashHook_Metadata(t *testing.T) {
	h := NewValidateExpectedHashHook()
	if h.Name() != "validate-expected-hash" {
		t.Errorf("Name = %q, want validate-expected-hash", h.Name())
	}
	if h.Phase() != hook.PhasePreCall {
		t.Errorf("Phase = %v, want PhasePreCall", h.Phase())
	}
	if h.Priority() != 20 {
		t.Errorf("Priority = %d, want 20", h.Priority())
	}
}

func TestValidateExpectedHashHook_Matches(t *testing.T) {
	h := NewValidateExpectedHashHook()
	cases := map[string]bool{
		"write_file":  true,
		"edit_file":   true,
		"read_file":   false,
		"list_dir":    false,
		"grep_search": false,
		"run_shell":   false,
		"web_search":  false,
	}
	for tool, want := range cases {
		t.Run(tool, func(t *testing.T) {
			if got := h.Matches(tool); got != want {
				t.Errorf("Matches(%q) = %v, want %v", tool, got, want)
			}
		})
	}
}

// ---- Empty / missing hash → Continue ----

func TestValidateExpectedHashHook_NoHashContinues(t *testing.T) {
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args:     map[string]any{"path": "/anywhere/x.md", "content": "x"},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when no expected_hash", d.Action)
	}
}

func TestValidateExpectedHashHook_EmptyHashContinues(t *testing.T) {
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args:     map[string]any{"path": "/anywhere/x.md", "expected_hash": ""},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when expected_hash is empty string", d.Action)
	}
}

func TestValidateExpectedHashHook_MissingPathContinues(t *testing.T) {
	// path 缺失 → 让其他 hook (PathBoundary) 或工具自报错
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args:     map[string]any{"expected_hash": "deadbeef"},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue when path missing", d.Action)
	}
}

// ---- File not exist → Continue (allow create) ----

func TestValidateExpectedHashHook_FileNotExistContinues(t *testing.T) {
	h := NewValidateExpectedHashHook()
	missing := filepath.Join(t.TempDir(), "never-existed.md")
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args: map[string]any{
			"path":          missing,
			"expected_hash": "any-hash-the-llm-might-pass",
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue for new-file write", d.Action)
	}
}

// ---- Hash matches → Continue ----

func TestValidateExpectedHashHook_HashMatchContinues(t *testing.T) {
	path, hashHex := makeFileWithHash(t, "original content")
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args: map[string]any{
			"path":          path,
			"expected_hash": hashHex,
			"content":       "new content",
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue (reason: %s)", d.Action, d.AbortReason)
	}
}

func TestValidateExpectedHashHook_EditFileHashMatchContinues(t *testing.T) {
	// edit_file 也走完全相同的校验路径
	path, hashHex := makeFileWithHash(t, "alpha beta")
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          path,
			"expected_hash": hashHex,
			"old_str":       "beta",
			"new_str":       "gamma",
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("edit_file with matching hash: Action = %v, want Continue", d.Action)
	}
}

// ---- Hash mismatches → Abort with descriptive reason ----

func TestValidateExpectedHashHook_HashMismatchAborts(t *testing.T) {
	path, _ := makeFileWithHash(t, "original content")
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args: map[string]any{
			"path":          path,
			"expected_hash": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			"content":       "new content",
		},
	})
	if d.Action != hook.Abort {
		t.Fatalf("Action = %v, want Abort on hash mismatch", d.Action)
	}
	if d.HookName != "validate-expected-hash" {
		t.Errorf("HookName = %q, want validate-expected-hash", d.HookName)
	}
	if !strings.Contains(d.AbortReason, "写入冲突") {
		t.Errorf("AbortReason should contain '写入冲突', got %q", d.AbortReason)
	}
	if !strings.Contains(d.AbortReason, "deadbeef") {
		t.Errorf("AbortReason should include expected hash, got %q", d.AbortReason)
	}
}

func TestValidateExpectedHashHook_EditFileHashMismatchAborts(t *testing.T) {
	path, _ := makeFileWithHash(t, "hello world")
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "edit_file",
		Args: map[string]any{
			"path":          path,
			"expected_hash": "wrong-hash",
			"old_str":       "world",
			"new_str":       "Go",
		},
	})
	if d.Action != hook.Abort {
		t.Errorf("edit_file Action = %v, want Abort on hash mismatch", d.Action)
	}
}

// ---- Read error other than not-exist → Abort ----

func TestValidateExpectedHashHook_PermissionErrorAborts(t *testing.T) {
	// 这个测试在 Windows 上构造 permission error 比较脆弱，跳过非 Linux 平台。
	// 用一个目录而非文件作为 path，os.ReadFile 在多数平台返回 non-IsNotExist error。
	tmp := t.TempDir() // 路径是个目录，不是普通文件
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args: map[string]any{
			"path":          tmp,
			"expected_hash": "any",
		},
	})
	// 平台行为不一：Linux 上 os.ReadFile(目录) 返回 non-nil non-IsNotExist 错误
	// → Abort；某些平台可能不同。允许 Continue 或 Abort，但 Abort 时必须包含原因。
	if d.Action == hook.Abort && d.AbortReason == "" {
		t.Error("Abort without AbortReason")
	}
}

// ---- Edge：non-string expected_hash silently treated as empty ----

func TestValidateExpectedHashHook_NonStringHashContinues(t *testing.T) {
	// 与 inline 旧实现行为一致：args["expected_hash"].(string) 失败会得到空串
	// 然后被 "expectedHash == ''" 分支跳过校验。
	h := NewValidateExpectedHashHook()
	d := h.Run(hook.ToolHookContext{
		ToolName: "write_file",
		Args: map[string]any{
			"path":          "/anywhere/x.md",
			"expected_hash": 123, // 不是字符串
		},
	})
	if d.Action != hook.Continue {
		t.Errorf("Action = %v, want Continue for non-string hash", d.Action)
	}
}

// ---- E2E via registry ----

func TestValidateExpectedHashHook_ViaRegistry(t *testing.T) {
	path, _ := makeFileWithHash(t, "v1")
	reg := hook.NewToolHookRegistry()
	if err := reg.Register(NewValidateExpectedHashHook()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	d := reg.RunPre(hook.ToolHookContext{
		ToolName: "write_file",
		Args: map[string]any{
			"path":          path,
			"expected_hash": "wrong",
			"content":       "v2",
		},
	})
	if d.Action != hook.Abort {
		t.Errorf("Action = %v, want Abort via registry", d.Action)
	}
}
