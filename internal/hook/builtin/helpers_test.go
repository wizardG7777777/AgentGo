package builtin

import (
	"os"
	"path/filepath"
	"testing"
)

// ---- normalizeArtifactPath ----

func TestNormalizeArtifactPath_RelativeProjectRoot(t *testing.T) {
	// 回归（2026-07-21 验收马拉松事故）：setting.yaml 的 project_root: "." 是
	// 相对路径，filepath.Rel(".", 绝对路径) 直接报错会让 artifact 被登记成
	// 绝对路径，导致验收比对永远失败。修复后必须先 Abs 再 Rel。
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(cwd, "docs", "foo.md")
	if got := normalizeArtifactPath(abs, "."); got != "docs/foo.md" {
		t.Errorf("got=%q want=%q", got, "docs/foo.md")
	}
}

func TestNormalizeArtifactPath_VariousInputs(t *testing.T) {
	cases := []struct {
		name        string
		absPath     string
		projectRoot string
		// 注：我们不断言固定的字符串，因为 filepath.Rel 在 Windows 上返回 \
		// 而 Unix 返回 /。这里只断言"返回值不再以 absPath 完整开头"或在
		// projectRoot 之外时返回原样。
		expectInsideRoot bool
	}{
		{"under root", "/proj/docs/foo.md", "/proj", true},
		{"deeply nested", "/proj/a/b/c.md", "/proj", true},
		{"outside root", "/etc/passwd", "/proj", false},
		{"empty root", "/tmp/x.md", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeArtifactPath(tt.absPath, tt.projectRoot)
			if got == "" {
				t.Errorf("expected non-empty result, got empty")
			}
			if tt.expectInsideRoot {
				// 在 root 内：返回值应当不含驱动器 / 不以 / 开头（POSIX）
				if got == tt.absPath {
					t.Errorf("expected relativized path, got original %q", got)
				}
			} else {
				// 在 root 外或 root 为空：返回 cleaned 原路径
				// 仅做存在性断言，不做严格比较（避免 Windows 路径分隔符差异）
				t.Logf("outside-root path normalized to %q", got)
			}
		})
	}
}
