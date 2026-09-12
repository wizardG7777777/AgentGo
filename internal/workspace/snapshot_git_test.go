package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("未安装 Git，无法验证真实仓库隔离")
	}
	root := t.TempDir()
	t.Cleanup(func() {
		if err := removeTree(root); err != nil {
			t.Error(err)
		}
	})
	gitOK(t, root, "init", "--initial-branch=main")
	putSnapshotFile(t, filepath.Join(root, "source.txt"), "committed\n")
	gitOK(t, root, "add", ".")
	gitOK(t, root, "commit", "-m", "initial")
	return root
}

func gitOK(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := snapshotGit(root, "", args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func putSnapshotFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotGitSeparatesIndexAndUsesFrozenCandidate(t *testing.T) {
	root := gitFixture(t)
	mainHead := gitOK(t, root, "rev-parse", "HEAD")
	putSnapshotFile(t, filepath.Join(root, "source.txt"), "input-not-committed\n")
	mainIndex := gitOK(t, root, "write-tree")
	m := NewManager(root, nil)
	v, err := m.MaterializeAgentTask("first", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	write, err := v.WritePath(filepath.Join(root, "source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	putSnapshotFile(t, write, "candidate-a\n")
	snapshot, err := v.PrepareShellRoot()
	if err != nil {
		t.Fatal(err)
	}
	top := gitOK(t, snapshot, "rev-parse", "--show-toplevel")
	info, _ := os.Stat(top)
	want, _ := os.Stat(snapshot)
	if !os.SameFile(info, want) {
		t.Fatalf("Git 发现了其它根: %s", top)
	}
	if got := gitOK(t, snapshot, "show", "HEAD:source.txt"); got != "input-not-committed" {
		t.Fatalf("基线错误: %s", got)
	}
	if got := gitOK(t, snapshot, "rev-parse", "HEAD^"); got != mainHead {
		t.Fatal("源仓库调查历史丢失")
	}
	if diff := gitOK(t, snapshot, "diff", "--", "source.txt"); !strings.Contains(diff, "+candidate-a") {
		t.Fatalf("没有本节点 diff: %s", diff)
	}
	if remotes := gitOK(t, snapshot, "remote"); remotes != "" {
		t.Fatalf("副本不能保留指向主根的 remote: %s", remotes)
	}
	if _, err := os.Stat(filepath.Join(snapshot, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("不得借用宿主对象目录: %v", err)
	}
	gitOK(t, snapshot, "add", "source.txt")
	gitOK(t, snapshot, "commit", "-m", "task-only")
	if got := gitOK(t, root, "rev-parse", "HEAD"); got != mainHead {
		t.Fatal("副本提交修改了源 HEAD")
	}
	if got := gitOK(t, root, "write-tree"); got != mainIndex {
		t.Fatal("副本操作修改了源 index")
	}
	a, err := m.FreezeAgentTaskCandidate("first", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	_, tree, err := m.ReadCandidate(a, "graph", "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(tree, ".git")); !os.IsNotExist(err) {
		t.Fatal("候选包含 Git 元数据")
	}
	vb, err := m.MaterializeAgentTask("second", "graph", "run", a)
	if err != nil {
		t.Fatal(err)
	}
	write, err = vb.WritePath(filepath.Join(root, "source.txt"))
	if err != nil {
		t.Fatal(err)
	}
	putSnapshotFile(t, write, "candidate-b\n")
	for i := 0; i < 2; i++ {
		second, err := vb.PrepareShellRoot()
		if err != nil {
			t.Fatal(err)
		}
		if got := gitOK(t, second, "show", "HEAD:source.txt"); got != "candidate-a" {
			t.Fatalf("下游基线应为上游候选: %s", got)
		}
		if diff := gitOK(t, second, "diff"); !strings.Contains(diff, "-candidate-a") || !strings.Contains(diff, "+candidate-b") {
			t.Fatalf("下游差异错误: %s", diff)
		}
		if err := vb.discardShellRoot(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSnapshotShellRestoreAndDeletionDoNotResurrectOverlay(t *testing.T) {
	root := gitFixture(t)
	m := NewManager(root, nil)
	v, err := m.MaterializeAgentTask("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"source.txt": "dirty\n", "temporary.txt": "new\n"} {
		path, err := v.WritePath(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		putSnapshotFile(t, path, value)
	}
	snapshot, err := v.PrepareShellRoot()
	if err != nil {
		t.Fatal(err)
	}
	gitOK(t, snapshot, "restore", "source.txt")
	if err := os.Remove(filepath.Join(snapshot, "temporary.txt")); err != nil {
		t.Fatal(err)
	}
	if err := v.CaptureShellChanges(); err != nil {
		t.Fatal(err)
	}
	if len(v.mf.snapshot()) != 0 {
		t.Fatalf("恢复基线后仍有 dirty entries: %v", v.mf.snapshot())
	}
	if body, err := os.ReadFile(v.ReadPath(filepath.Join(root, "source.txt"))); err != nil || string(body) != "committed\n" {
		t.Fatalf("文件工具仍读取旧内容: %q %v", body, err)
	}
	if _, err := v.PrepareShellRoot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "temporary.txt")); !os.IsNotExist(err) {
		t.Fatal("删除的新文件被复活")
	}
	if ref, err := m.FreezeAgentTaskCandidate("task", "graph", "run", ""); err != nil || ref != "" {
		t.Fatalf("已完全撤销却生成新候选: %s %v", ref, err)
	}
	if err := os.Remove(filepath.Join(snapshot, "source.txt")); err != nil {
		t.Fatal(err)
	}
	if err := v.CaptureShellChanges(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(v.ReadPath(filepath.Join(root, "source.txt"))); !os.IsNotExist(err) {
		t.Fatal("删除源文件后不能回退读基线")
	}
	ref, err := m.FreezeAgentTaskCandidate("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.ApplyCandidate(ref, "graph", "run"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "source.txt")); !os.IsNotExist(err) {
		t.Fatal("文件删除未交付")
	}
}

func TestSnapshotGitSubdirectoryAndInheritedRouting(t *testing.T) {
	parent := gitFixture(t)
	project := filepath.Join(parent, "project")
	putSnapshotFile(t, filepath.Join(project, "only.txt"), "subtree\n")
	m := NewManager(project, nil)
	v, err := m.MaterializeAgentTask("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := v.PrepareShellRoot()
	if err != nil {
		t.Fatal(err)
	}
	if files := gitOK(t, snapshot, "ls-files"); files != "only.txt" {
		t.Fatalf("子目录副本混入宿主文件: %s", files)
	}
	env := append(os.Environ(), "GIT_DIR="+filepath.Join(parent, ".git"), "GIT_WORK_TREE="+parent, "GIT_INDEX_FILE="+filepath.Join(parent, ".git", "index"), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.worktree", "GIT_CONFIG_VALUE_0="+parent)
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir, cmd.Env = snapshot, SnapshotGitEnvironment(env, snapshot)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.EqualFold(filepath.Clean(strings.TrimSpace(string(out))), filepath.Clean(snapshot)) {
		t.Fatalf("继承变量绕过副本: %q %v", out, err)
	}
	if err := removeTree(filepath.Join(snapshot, ".git")); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir, cmd.Env = snapshot, SnapshotGitEnvironment(env, snapshot)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("缺少元数据时向上发现了宿主: %s", out)
	}
}

func TestSnapshotGitSupportsLinkedWorktreeAndRejectsBrokenMetadata(t *testing.T) {
	root := gitFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitOK(t, root, "worktree", "add", "--detach", linked, "HEAD")
	m := NewManager(linked, nil)
	v, err := m.MaterializeAgentTask("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeTree(linked); err != nil {
			t.Error(err)
		}
	})
	snapshot, err := v.PrepareShellRoot()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(snapshot, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("复制了指向宿主的 .git 指针: %v", err)
	}
	if content := gitOK(t, snapshot, "show", "HEAD:source.txt"); content != "committed" {
		t.Fatalf("linked worktree 输入丢失: %s", content)
	}
	broken := t.TempDir()
	putSnapshotFile(t, filepath.Join(broken, ".git"), "not a git directory\n")
	m = NewManager(broken, nil)
	v, err = m.MaterializeAgentTask("task", "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.PrepareShellRoot(); err == nil {
		t.Fatal("Git 元数据损坏时不能回退宿主发现")
	}
}

func TestPackedSnapshotGitUnderLongWorkspacePath(t *testing.T) {
	root := gitFixture(t)
	// 实际 SWE 的 packed objects 路径会超过 MAX_PATH；短临时目录不能覆盖此故障。
	gitOK(t, root, "gc", "--prune=now")
	parent := t.TempDir()
	t.Cleanup(func() {
		if err := removeTree(parent); err != nil {
			t.Error(err)
		}
	})
	taskID := "agent-task-0123456789abcdef0123456789abcdef"
	// 固定 Shell 根约 220 字节：pack 路径超过 260，但进程 cwd 仍在系统限制内。
	padding := 220 - len(filepath.Join(parent, ".agentgo", "workspaces", taskID, shellRootDirName)) - 1
	if padding < 1 {
		t.Fatal("测试临时父目录过长，无法独立验证 Git 文件长路径")
	}
	linked := filepath.Join(parent, strings.Repeat("p", padding))
	gitOK(t, root, "worktree", "add", "--detach", linked, "HEAD")
	m := NewManager(linked, nil)
	v, err := m.MaterializeAgentTask(taskID, "graph", "run", "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := v.PrepareShellRoot()
	if err != nil {
		t.Fatal(err)
	}
	packFiles, err := filepath.Glob(filepath.Join(snapshot, ".git", "objects", "pack", "*.idx"))
	if err != nil || len(packFiles) == 0 || len(packFiles[0]) <= 260 {
		t.Fatalf("测试没有覆盖超长 packed Git 路径: %v %v", packFiles, err)
	}
	if got := gitOK(t, snapshot, "config", "--local", "core.longpaths"); got != "true" {
		t.Fatal("运行时 Shell 未继承副本长路径支持")
	}
	if got := gitOK(t, snapshot, "show", "HEAD:source.txt"); got != "committed" {
		t.Fatalf("深层目录副本不可读: %s", got)
	}
}
