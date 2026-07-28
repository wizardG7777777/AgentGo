package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSuite 在临时目录写套件与配套 prompt 文件，返回套件路径。
func writeSuite(t *testing.T, suiteYAML string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "suite.yaml")
	if err := os.WriteFile(path, []byte(suiteYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSuite_OK(t *testing.T) {
	path := writeSuite(t, `
name: core-v1
defaults:
  timeout_sec: 600
tasks:
  - name: t1
    smoke: true
    prompt_file: tasks/t1/prompt.md
    fixtures:
      - path: a/b.txt
        content: "hello"
    judges:
      - type: task_completed
      - type: file_contains
        path: a/b.txt
        pattern: hello
  - name: t2
    prompt: 内联提示词
    judges:
      - type: metric_bounds
        metric: total_tokens
        max: 1000
`, map[string]string{"tasks/t1/prompt.md": "去做事"})
	s, err := LoadSuite(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if s.Name != "core-v1" || len(s.Tasks) != 2 {
		t.Fatalf("套件要素不符: %+v", s)
	}
	if s.Tasks[0].Prompt != "去做事" {
		t.Fatalf("prompt_file 未内联: %q", s.Tasks[0].Prompt)
	}
	if s.Tasks[0].TimeoutSec != 600 || s.Tasks[1].TimeoutSec != 600 {
		t.Fatalf("defaults 未继承: %d / %d", s.Tasks[0].TimeoutSec, s.Tasks[1].TimeoutSec)
	}
}

func TestLoadSuite_DefaultTimeout(t *testing.T) {
	path := writeSuite(t, "name: x\ntasks:\n  - name: t\n    prompt: p\n", nil)
	s, err := LoadSuite(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tasks[0].TimeoutSec != 900 {
		t.Fatalf("缺省超时应为 900，实际 %d", s.Tasks[0].TimeoutSec)
	}
}

func TestLoadSuite_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{"缺套件名", "tasks:\n  - name: t\n    prompt: p\n", "缺少 name"},
		{"空任务表", "name: x\ntasks: []\n", "tasks 为空"},
		{"重名任务", "name: x\ntasks:\n  - name: t\n    prompt: p\n  - name: t\n    prompt: q\n", "重复"},
		{"缺提示词", "name: x\ntasks:\n  - name: t\n", "缺少 prompt"},
		{"双写提示词", "name: x\ntasks:\n  - name: t\n    prompt: p\n    prompt_file: f.md\n", "二选一"},
		{"未知判据", "name: x\ntasks:\n  - name: t\n    prompt: p\n    judges:\n      - type: llm_judge\n", "未知判据类型"},
		{"fixture 穿越", "name: x\ntasks:\n  - name: t\n    prompt: p\n    fixtures:\n      - path: ../../etc/x\n        content: y\n", "fixture 路径非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSuite(t, tc.yaml, nil)
			_, err := LoadSuite(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("应报含 %q 的错误，实际: %v", tc.wantSub, err)
			}
		})
	}
}

func TestLoadSuite_PromptFileMissing(t *testing.T) {
	path := writeSuite(t, "name: x\ntasks:\n  - name: t\n    prompt_file: 不存在.md\n", nil)
	_, err := LoadSuite(path)
	if err == nil || !strings.Contains(err.Error(), "prompt_file 读取失败") {
		t.Fatalf("缺失 prompt_file 应报错: %v", err)
	}
}
