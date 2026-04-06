package isolation

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreeManager 管理 git worktree 的创建、合并和清理。
type WorktreeManager struct {
	repoRoot string // 主仓库绝对路径
	baseDir  string // worktree 存放目录，默认 repoRoot/.worktrees
}

// NewWorktreeManager 创建 worktree 管理器。repoRoot 为主仓库路径。
func NewWorktreeManager(repoRoot string) *WorktreeManager {
	absRoot, _ := filepath.Abs(repoRoot)
	return &WorktreeManager{
		repoRoot: absRoot,
		baseDir:  filepath.Join(absRoot, ".worktrees"),
	}
}

// shortID 取 taskID 前 8 位作为 worktree 目录名。
func shortID(taskID string) string {
	if len(taskID) > 8 {
		return taskID[:8]
	}
	return taskID
}

// Path 返回指定 taskID 的 worktree 绝对路径（不检查是否存在）。
func (m *WorktreeManager) Path(taskID string) string {
	return filepath.Join(m.baseDir, shortID(taskID))
}

// branchName 返回 worktree 使用的分支名。
func (m *WorktreeManager) branchName(taskID string) string {
	return "_wt_" + shortID(taskID)
}

// Create 为指定 taskID 创建 worktree，返回 worktree 绝对路径。
func (m *WorktreeManager) Create(taskID string) (string, error) {
	wtPath := m.Path(taskID)
	branch := m.branchName(taskID)

	// 确保 baseDir 存在
	if err := os.MkdirAll(m.baseDir, 0o755); err != nil {
		return "", fmt.Errorf("创建 worktree 目录失败: %w", err)
	}

	// 如果路径已存在（残留），先清理
	if _, err := os.Stat(wtPath); err == nil {
		log.Printf("[worktree] 检测到残留 worktree %s，先清理", wtPath)
		m.Remove(taskID)
	}

	// 删除可能残留的同名分支
	cmd := exec.Command("git", "branch", "-D", branch)
	cmd.Dir = m.repoRoot
	cmd.Run() // 忽略错误（分支可能不存在）

	// git worktree add <path> -b <branch>
	cmd = exec.Command("git", "worktree", "add", wtPath, "-b", branch)
	cmd.Dir = m.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add 失败: %w\n%s", err, string(output))
	}

	log.Printf("[worktree] 已创建 worktree: %s (branch=%s)", wtPath, branch)
	return wtPath, nil
}

// CommitAndMerge 在 worktree 内 commit 所有变更，然后尝试 merge 到主分支。
// 返回 (merged, error)。merged=false 且 error=nil 表示有冲突需要 ConflictResolver。
func (m *WorktreeManager) CommitAndMerge(taskID, commitMsg string) (bool, error) {
	wtPath := m.Path(taskID)
	branch := m.branchName(taskID)

	// Step 1: 在 worktree 内 git add -A
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = wtPath
	if output, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add 失败: %w\n%s", err, string(output))
	}

	// Step 2: 检查是否有变更
	cmd = exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = wtPath
	if err := cmd.Run(); err == nil {
		// 无变更，跳过 commit 和 merge
		log.Printf("[worktree] 任务 %s 无文件变更，跳过合并", shortID(taskID))
		return true, nil
	}

	// Step 3: commit
	msg := fmt.Sprintf("task %s: %s", shortID(taskID), commitMsg)
	cmd = exec.Command("git", "commit", "-m", msg)
	cmd.Dir = wtPath
	if output, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git commit 失败: %w\n%s", err, string(output))
	}

	// Step 4: 在主仓库尝试 merge
	cmd = exec.Command("git", "merge", "--no-ff", branch)
	cmd.Dir = m.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		// 检查是否是合并冲突
		if strings.Contains(string(output), "CONFLICT") || strings.Contains(string(output), "conflict") {
			// 回滚合并，让 ConflictResolver 处理
			abortCmd := exec.Command("git", "merge", "--abort")
			abortCmd.Dir = m.repoRoot
			abortCmd.Run()
			log.Printf("[worktree] 任务 %s 合并冲突，需要 ConflictResolver", shortID(taskID))
			return false, nil
		}
		return false, fmt.Errorf("git merge 失败: %w\n%s", err, string(output))
	}

	log.Printf("[worktree] 任务 %s 合并成功", shortID(taskID))
	return true, nil
}

// Remove 清理指定 taskID 的 worktree 和分支。
func (m *WorktreeManager) Remove(taskID string) error {
	wtPath := m.Path(taskID)
	branch := m.branchName(taskID)

	// git worktree remove --force
	cmd := exec.Command("git", "worktree", "remove", wtPath, "--force")
	cmd.Dir = m.repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		// 如果目录不存在，不算错误
		if !os.IsNotExist(err) {
			log.Printf("[worktree] 清理 worktree %s 失败: %v\n%s", wtPath, err, string(output))
		}
	}

	// 清理分支
	cmd = exec.Command("git", "branch", "-D", branch)
	cmd.Dir = m.repoRoot
	cmd.Run() // 忽略错误

	return nil
}

// CleanupAll 清理 baseDir 下所有残留 worktree（Shutdown 时调用）。
func (m *WorktreeManager) CleanupAll() error {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wtPath := filepath.Join(m.baseDir, entry.Name())
		branch := "_wt_" + entry.Name()

		cmd := exec.Command("git", "worktree", "remove", wtPath, "--force")
		cmd.Dir = m.repoRoot
		cmd.Run()

		cmd = exec.Command("git", "branch", "-D", branch)
		cmd.Dir = m.repoRoot
		cmd.Run()

		log.Printf("[worktree] 清理残留 worktree: %s", wtPath)
	}

	// 清理 baseDir 本身（如果为空）
	os.Remove(m.baseDir)
	return nil
}

// RepoRoot 返回主仓库路径。
func (m *WorktreeManager) RepoRoot() string {
	return m.repoRoot
}
