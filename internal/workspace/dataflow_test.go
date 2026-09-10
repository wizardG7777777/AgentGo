package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentTaskCandidatesPreserveVersionsAndCommit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(root, nil)
	v, err := m.MaterializeAgentTask("task-a", "graph-a", "run-a", "")
	if err != nil {
		t.Fatal(err)
	}
	write, err := v.WritePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(write, []byte("candidate-a"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := m.FreezeAgentTaskCandidate("task-a", "graph-a", "run-a", "")
	if err != nil || a == "" {
		t.Fatalf("冻结 A: %s %v", a, err)
	}
	vb, err := m.MaterializeAgentTask("task-b", "graph-a", "run-a", a)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(vb.ReadPath(path))
	if string(body) != "candidate-a" {
		t.Fatalf("下游必须读 A，实际 %q", body)
	}
	write, err = vb.WritePath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(write, []byte("candidate-b"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := m.FreezeAgentTaskCandidate("task-b", "graph-a", "run-a", a)
	if err != nil || b == a {
		t.Fatalf("冻结 B 必须是新版本: %v", err)
	}
	_, aTree, err := m.ReadCandidate(a, "graph-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(aTree, "source.txt"))
	if string(body) != "candidate-a" {
		t.Fatal("新实例改写了 A")
	}
	body, _ = os.ReadFile(path)
	if string(body) != "old" {
		t.Fatal("提交前主根不得变化")
	}
	if err = m.ApplyCandidate(b, "graph-a", "run-a"); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(path)
	if string(body) != "candidate-b" {
		t.Fatal("交付没有写回选定候选")
	}
}

func TestAgentTaskShellChangesAndLaterFileWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	_ = os.WriteFile(path, []byte("old"), 0600)
	m := NewManager(root, nil)
	v, err := m.MaterializeAgentTask("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	shellRoot, err := v.PrepareShellRoot()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(shellRoot, "source.txt"), []byte("shell-change"), 0600)
	if err = v.CaptureShellChanges(); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(v.ReadPath(path))
	if string(body) != "shell-change" {
		t.Fatal("Shell 修改没有回到工作视图")
	}
	write, err := v.WritePath(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(write, []byte("later-apply-change"), 0600)
	ref, err := m.FreezeAgentTaskCandidate("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	_, tree, err := m.ReadCandidate(ref, "graph", "run")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(filepath.Join(tree, "source.txt"))
	if string(body) != "later-apply-change" {
		t.Fatalf("旧 Shell 快照覆盖了后续写入: %q", body)
	}
}

func TestAgentTaskCandidateRejectsTamperingAndRootConflict(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "source.txt")
	_ = os.WriteFile(path, []byte("old"), 0600)
	_ = os.WriteFile(filepath.Join(root, "unchanged.txt"), []byte("unchanged"), 0600)
	m := NewManager(root, nil)
	v, _ := m.MaterializeAgentTask("task", "graph", "run", "")
	write, _ := v.WritePath(path)
	_ = os.WriteFile(write, []byte("new"), 0600)
	ref, err := m.FreezeAgentTaskCandidate("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.ReadCandidate(ref, "another-graph", "run"); err == nil {
		t.Fatal("候选跨图越权")
	}
	_ = os.WriteFile(path, []byte("external-change"), 0600)
	if err = m.ApplyCandidate(ref, "graph", "run"); err == nil {
		t.Fatal("不得覆盖外部主根修改")
	}
	_, tree, err := m.ReadCandidate(ref, "graph", "run")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tree, "unchanged.txt"), []byte("tampered"), 0600)
	if _, _, err = m.ReadCandidate(ref, "graph", "run"); err == nil {
		t.Fatal("候选摘要必须覆盖未修改文件")
	}
}
