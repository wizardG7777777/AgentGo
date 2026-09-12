package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SnapshotGitEnvironment 清除会把 Git 路由到其它工作树/索引的继承变量。
// ceiling 即使在无 Git 项目中也生效，禁止 Git 自动向上发现宿主仓库。
// 不禁止用户显式调用 git -C；Shell 仍遵循原有的宿主能力契约。
func SnapshotGitEnvironment(env []string, root string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_PREFIX", "GIT_CONFIG",
			"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_NAMESPACE", "GIT_SHALLOW_FILE":
			continue
		}
		if strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GIT_CEILING_DIRECTORIES="+filepath.Dir(root))
}

// 只调用本地 Git。设置阶段忽略宿主配置、hooks、签名及隐式环境覆盖。
func snapshotGit(dir, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{
		"-c", "core.longpaths=true", "-c", "core.autocrlf=false", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=",
	}, args...)...)
	hideSnapshotGitWindow(cmd)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_AUTHOR_NAME=AgentGo", "GIT_AUTHOR_EMAIL=workspace@agentgo.invalid",
		"GIT_COMMITTER_NAME=AgentGo", "GIT_COMMITTER_EMAIL=workspace@agentgo.invalid")
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("workspace Git %s 失败: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func prepareSnapshotGit(projectRoot, target string) error {
	// 非 Git 项目无需安装 Git；有标记却损坏的仓库必须明确报错。
	marker := false
	for dir := projectRoot; ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			marker = true
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if !marker {
		return nil
	}
	top, err := snapshotGit(projectRoot, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	topInfo, err := os.Stat(top)
	if err != nil {
		return err
	}
	rootInfo, err := os.Stat(projectRoot)
	if err != nil {
		return err
	}
	if os.SameFile(topInfo, rootInfo) {
		// 不共享 .git 文件、index、refs、hardlinks 或 alternates；保留原历史供调查。
		if _, err = snapshotGit(projectRoot, "", "clone", "--local", "--no-hardlinks", "--dissociate", "--no-checkout", "--template=", "--", projectRoot, target); err != nil {
			return err
		}
		if _, err = snapshotGit(target, "", "remote", "remove", "origin"); err != nil {
			return err
		}
	} else {
		// ProjectRoot 只是宿主仓库的子目录时，其路径与宿主树不同；建立子树基线。
		if _, err = snapshotGit(target, "", "init", "--template=", "--initial-branch=agentgo-input"); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{"core.worktree", ".."}, {"core.longpaths", "true"}, {"core.autocrlf", "false"}, {"core.fsmonitor", "false"}, {"core.hooksPath", ".git/agentgo-empty-hooks"}, {"commit.gpgsign", "false"}} {
		if _, err = snapshotGit(target, "", "config", "--local", pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// baseline 在 overlay 覆盖前封存，含已冻结的输入候选；git diff 因此展示本节点差异。
// 使用 plumbing 创建基线，不触发用户 hook，不修改源仓库的 HEAD 或 index。
func sealSnapshotGit(root string) error {
	if _, err := os.Stat(filepath.Join(root, ".git")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	parent, parentErr := snapshotGit(root, "", "rev-parse", "--verify", "--quiet", "HEAD")
	if parentErr != nil {
		// 空仓库允许首个基线；其它错误不能当作无历史。
		var exitErr *exec.ExitError
		if !errors.As(parentErr, &exitErr) || exitErr.ExitCode() != 1 {
			return parentErr
		}
	}
	if _, err := snapshotGit(root, "", "read-tree", "--empty"); err != nil {
		return err
	}
	files, err := snapshotHashes(root)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	if len(paths) > 0 {
		if _, err = snapshotGit(root, strings.Join(paths, "\x00")+"\x00", "--literal-pathspecs", "add", "--force", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	tree, err := snapshotGit(root, "", "write-tree")
	if err != nil {
		return err
	}
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := snapshotGit(root, "AgentGo frozen task input\n", args...)
	if err != nil {
		return err
	}
	if _, err = snapshotGit(root, "", "update-ref", "refs/heads/agentgo-input", commit); err != nil {
		return err
	}
	_, err = snapshotGit(root, "", "symbolic-ref", "HEAD", "refs/heads/agentgo-input")
	return err
}
