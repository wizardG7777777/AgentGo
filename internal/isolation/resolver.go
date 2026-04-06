package isolation

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

const resolverTimeout = 180 * time.Second

const resolverSystemPrompt = `你是一个 Git 冲突解决专家（ConflictResolver）。你的职责是解决 git merge 产生的冲突。

你的工作流程：
1. 使用 run_shell 执行 git diff 或 git status 查看冲突文件
2. 使用 read_file 读取冲突文件内容，理解双方的修改意图
3. 参考任务描述来理解修改的目的
4. 使用 edit_file 解决冲突标记（<<<<<<< / ======= / >>>>>>>）
5. 确保解决后的代码能正确融合双方的改动
6. 使用 run_shell 执行 git add 将解决后的文件标记为已解决

注意事项：
- 不要简单地选择一方丢弃另一方，除非确实有一方是错误的
- 解决后的代码应当同时保留双方有价值的修改
- 如果冲突涉及逻辑冲突（不仅是文本冲突），优先保证代码正确性`

// ConflictRequest 描述一个需要解决的合并冲突。
type ConflictRequest struct {
	TaskID       string     // 产生冲突的任务 ID
	WorktreePath string     // worktree 路径
	BranchName   string     // worktree 分支名
	TaskDesc     string     // 任务描述，帮助 resolver 理解上下文
	DoneCh       chan error // 完成通知：nil=成功, error=失败
}

// ConflictResolver 是冲突处理代理，随主程序启动，休眠直到收到冲突请求。
type ConflictResolver struct {
	requestCh chan ConflictRequest
	repoRoot  string
	llmClient llm.Client
	store     store.TaskStore
}

// NewConflictResolver 创建冲突处理代理。
func NewConflictResolver(repoRoot string, llmClient llm.Client, s store.TaskStore) *ConflictResolver {
	return &ConflictResolver{
		requestCh: make(chan ConflictRequest, 8),
		repoRoot:  repoRoot,
		llmClient: llmClient,
		store:     s,
	}
}

// Submit 提交一个冲突请求。调用方应在 req.DoneCh 上阻塞等待结果。
func (r *ConflictResolver) Submit(req ConflictRequest) {
	r.requestCh <- req
}

// Run 主循环：从 requestCh 读取冲突请求，逐个处理。无请求时阻塞休眠。
func (r *ConflictResolver) Run(ctx context.Context) {
	log.Println("[conflict-resolver] 冲突处理代理已启动，等待请求")
	for {
		select {
		case <-ctx.Done():
			log.Println("[conflict-resolver] 冲突处理代理退出")
			return
		case req := <-r.requestCh:
			r.handle(ctx, req)
		}
	}
}

// handle 处理单个冲突请求。
func (r *ConflictResolver) handle(ctx context.Context, req ConflictRequest) {
	log.Printf("[conflict-resolver] 开始处理冲突: task=%s, branch=%s", shortID(req.TaskID), req.BranchName)

	// 创建带 180s 超时的 context
	resolveCtx, cancel := context.WithTimeout(ctx, resolverTimeout)
	defer cancel()

	// Step 1: 在主仓库重新尝试 merge（产生冲突标记）
	cmd := exec.Command("git", "merge", "--no-ff", req.BranchName)
	cmd.Dir = r.repoRoot
	output, err := cmd.CombinedOutput()
	if err == nil {
		// 意外成功（可能其他变更已被合并），直接完成
		log.Printf("[conflict-resolver] 任务 %s 重试合并意外成功", shortID(req.TaskID))
		req.DoneCh <- nil
		return
	}

	if !strings.Contains(string(output), "CONFLICT") {
		// 不是冲突而是其他错误
		abortMerge(r.repoRoot)
		req.DoneCh <- fmt.Errorf("git merge 失败（非冲突）: %s", string(output))
		return
	}

	// Step 2: 用 LLM ReAct 循环解决冲突
	err = r.resolveWithLLM(resolveCtx, req)
	if err != nil {
		// 超时或 LLM 失败 → 回滚
		abortMerge(r.repoRoot)
		if resolveCtx.Err() != nil {
			log.Printf("[CRITICAL] 冲突处理超时 (%v): task=%s, branch=%s", resolverTimeout, shortID(req.TaskID), req.BranchName)
			req.DoneCh <- fmt.Errorf("冲突处理超时 (%v)", resolverTimeout)
		} else {
			log.Printf("[CRITICAL] 冲突处理失败: task=%s, err=%v", shortID(req.TaskID), err)
			req.DoneCh <- err
		}
		return
	}

	// Step 3: 确认所有冲突已解决，完成合并
	commitCmd := exec.Command("git", "commit", "--no-edit")
	commitCmd.Dir = r.repoRoot
	if output, err := commitCmd.CombinedOutput(); err != nil {
		abortMerge(r.repoRoot)
		req.DoneCh <- fmt.Errorf("冲突解决后 commit 失败: %w\n%s", err, string(output))
		return
	}

	log.Printf("[conflict-resolver] 任务 %s 冲突已解决并合并", shortID(req.TaskID))
	req.DoneCh <- nil
}

// resolveWithLLM 使用 LLM ReAct 循环解决冲突文件。
func (r *ConflictResolver) resolveWithLLM(ctx context.Context, req ConflictRequest) error {
	// 构建工具集（限定在 repoRoot）
	tools := agent.NewToolRegistry()
	registerResolverTools(tools, r.repoRoot)

	executor := agent.NewLLMExecutor(r.llmClient, tools, resolverSystemPrompt)

	// 构造一个虚拟 task 传入 executor
	task := &model.Task{
		Description: fmt.Sprintf(
			"解决 git merge 冲突。\n\n冲突分支: %s\n原始任务描述: %s\n\n请执行 git status 查看冲突文件，然后逐个解决。",
			req.BranchName, req.TaskDesc,
		),
	}

	// 简单的 ReAct 循环（最多 20 轮）
	var history []agent.HistoryEntry
	depResults := map[string]string{}

	for i := 0; i < 20; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		histCopy := make([]agent.HistoryEntry, len(history))
		copy(histCopy, history)

		result, err := executor(ctx, task, depResults, histCopy)
		if err != nil {
			return fmt.Errorf("LLM 调用失败: %w", err)
		}

		if !result.ToolCalled {
			// LLM 认为冲突已解决
			return nil
		}

		history = append(history, agent.HistoryEntry{
			Output:           result.Output,
			ToolCalled:       result.ToolCalled,
			AssistantContent: result.AssistantContent,
			ToolCalls:        result.ToolCalls,
			ToolResults:      convertToolResults(result.ToolResults),
		})
	}

	return fmt.Errorf("冲突解决超过最大循环次数 (20)")
}

func convertToolResults(results []agent.ToolResult) []agent.ToolResult {
	return results // 类型相同，直接返回
}

// registerResolverTools 注册冲突解决代理的工具集。
func registerResolverTools(tools *agent.ToolRegistry, repoRoot string) {
	tools.Register("read_file", "读取指定文件的内容", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "文件路径（相对于仓库根目录）"},
		},
		"required": []any{"path"},
	}, makeResolverReadFileTool(repoRoot))

	tools.Register("edit_file", "编辑文件内容（用于解决冲突标记）", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "文件路径"},
			"old_str": map[string]any{"type": "string", "description": "要替换的原始文本（包含冲突标记）"},
			"new_str": map[string]any{"type": "string", "description": "替换后的文本（解决冲突后的内容）"},
		},
		"required": []any{"path", "old_str", "new_str"},
	}, makeResolverEditFileTool(repoRoot))

	tools.Register("run_shell", "执行 shell 命令（用于 git status、git add 等）", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string", "description": "要执行的命令"},
		},
		"required": []any{"command"},
	}, makeResolverShellTool(repoRoot))
}

func makeResolverReadFileTool(repoRoot string) agent.ToolFunc {
	return func(ctx context.Context, args map[string]any) (string, error) {
		path, _ := args["path"].(string)
		if path == "" {
			return "", fmt.Errorf("缺少 path 参数")
		}
		absPath, err := resolvePath(repoRoot, path)
		if err != nil {
			return "", err
		}
		data, err := readFileBytes(absPath)
		if err != nil {
			return "", err
		}
		content := string(data)
		if len(content) > 10000 {
			content = content[:10000] + "\n... (截断)"
		}
		return content, nil
	}
}

func makeResolverEditFileTool(repoRoot string) agent.ToolFunc {
	return func(ctx context.Context, args map[string]any) (string, error) {
		path, _ := args["path"].(string)
		oldStr, _ := args["old_str"].(string)
		newStr, _ := args["new_str"].(string)
		if path == "" || oldStr == "" {
			return "", fmt.Errorf("缺少 path 或 old_str 参数")
		}
		absPath, err := resolvePath(repoRoot, path)
		if err != nil {
			return "", err
		}
		data, err := readFileBytes(absPath)
		if err != nil {
			return "", err
		}
		content := string(data)
		count := strings.Count(content, oldStr)
		if count == 0 {
			return "", fmt.Errorf("未找到匹配内容")
		}
		if count > 1 {
			return "", fmt.Errorf("匹配到 %d 处，请提供更精确的文本", count)
		}
		newContent := strings.Replace(content, oldStr, newStr, 1)
		if err := writeFileBytes(absPath, []byte(newContent)); err != nil {
			return "", err
		}
		return fmt.Sprintf("文件已编辑: %s", path), nil
	}
}

func makeResolverShellTool(repoRoot string) agent.ToolFunc {
	return func(ctx context.Context, args map[string]any) (string, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return "", fmt.Errorf("缺少 command 参数")
		}
		cmd := exec.CommandContext(ctx, shellName(), shellFlag(), command)
		cmd.Dir = repoRoot
		output, err := cmd.CombinedOutput()
		result := string(output)
		if len(result) > 10000 {
			result = result[len(result)-10000:]
		}
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("命令超时")
			}
			return fmt.Sprintf("exit_code: %v\nstdout+stderr:\n%s", err, result), nil
		}
		return fmt.Sprintf("exit_code: 0\nstdout+stderr:\n%s", result), nil
	}
}

// 平台相关的 shell 命令
func shellName() string {
	if isWindows() {
		return "cmd"
	}
	return "sh"
}

func shellFlag() string {
	if isWindows() {
		return "/C"
	}
	return "-c"
}

func isWindows() bool {
	return runtime.GOOS == "windows"
}

func abortMerge(repoRoot string) {
	cmd := exec.Command("git", "merge", "--abort")
	cmd.Dir = repoRoot
	cmd.Run()
}

func resolvePath(root, path string) (string, error) {
	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath = filepath.Join(root, path)
	}
	// 安全检查：确保路径在 repoRoot 内
	absRoot := filepath.Clean(root) + string(filepath.Separator)
	if !strings.HasPrefix(absPath, absRoot) && absPath != filepath.Clean(root) {
		return "", fmt.Errorf("路径超出仓库根目录范围: %s", path)
	}
	return absPath, nil
}

func readFileBytes(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}
	return data, nil
}

func writeFileBytes(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}
	return nil
}
