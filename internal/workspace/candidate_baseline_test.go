package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInputCandidateBaselineUsesActualLineage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("root"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root, nil)
	makeCandidate := func(task, parent string) string {
		v, err := m.MaterializeAgentTask(task, "graph", "run", parent)
		if err != nil {
			t.Fatal(err)
		}
		write, err := v.WritePath(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(write, []byte(task), 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := m.FreezeAgentTaskCandidate(task, "graph", "run", parent)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	a := makeCandidate("a", "")
	b := makeCandidate("b", a)
	c := makeCandidate("c", a)
	for _, refs := range [][]string{{a, b}, {b, a, a}, {"", b}} {
		got, err := m.ResolveCandidateBaseline(context.Background(), "graph", "run", refs)
		if err != nil || got != b {
			t.Fatalf("没有选择真实后继版本: %s %v", got, err)
		}
	}
	if _, err := m.ResolveCandidateBaseline(context.Background(), "graph", "run", []string{b, c}); err == nil || !strings.Contains(err.Error(), "candidate_branch_conflict") {
		t.Fatalf("独立分支不得任取: %v", err)
	}
	if _, err := m.ResolveCandidateBaseline(context.Background(), "graph", "foreign-run", []string{a, b}); err == nil {
		t.Fatal("候选谱系不能跨 Run")
	}
	if ref, err := m.ResolveCandidateBaseline(context.Background(), "graph", "run", nil); err != nil || ref != "" {
		t.Fatal("纯文本输入不应要求代码候选")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.ResolveCandidateBaseline(ctx, "graph", "run", []string{a, b}); err == nil {
		t.Fatal("取消后仍读取候选")
	}
}
