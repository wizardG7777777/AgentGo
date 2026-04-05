package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

const systemPrompt = `你是一个执行代理（Worker），负责执行具体的编码和文件操作任务。

你的职责：
- 读取项目文件，理解现有代码结构
- 搜索项目中的关键字和模式
- 使用 glob_search 发现项目文件结构
- 使用 edit_file 精准修改文件内容（优先于 write_file）
- 仅在创建新文件时使用 write_file
- 使用 run_shell 执行编译、测试、git 等命令
- 完成后返回简洁的执行结果摘要

你的工作方式：
- 先用 read_file、grep_search、glob_search 了解相关代码
- 修改文件时优先使用 edit_file（old_str + new_str 精准替换），避免全量重写
- 仅在创建全新文件时使用 write_file
- 用 run_shell 执行编译和测试验证修改结果
- 每次只修改与任务直接相关的文件
- 结果应简明扼要：说明做了什么修改，涉及哪些文件`

// Worker 是执行代理，负责认领和执行 scheduler 发布的执行任务。
type Worker struct {
	agent *agent.Agent
}

// New 创建执行代理。使用主 LLM 和读写工具集。
func New(s store.TaskStore, r roster.Roster, llmClient llm.Client, cfg *config.Config, cancelReg *store.TaskCancelRegistry) *Worker {
	const agentID = "worker-1"

	tools := agent.NewToolRegistry()
	registerWorkerTools(tools, r, agentID, cfg)

	executor := agent.NewLLMExecutor(llmClient, tools, systemPrompt)

	a := agent.NewAgent(
		agentID,
		"", // 空字符串，匹配 scheduler 发布的执行任务
		s, r, executor,
		cfg.AgentMaxLoops,
	)
	a.CancelRegistry = cancelReg
	a.MaxRetries = cfg.MaxRetry
	a.IdleThreshold = 0 // 预制代理不因空闲退出

	return &Worker{agent: a}
}

// Run 启动执行代理的轮询循环，阻塞直到 ctx 取消。
func (w *Worker) Run(ctx context.Context) {
	w.agent.Run(ctx)
}

// registerWorkerTools 注册执行代理的工具集（只读工具 + 写文件工具）。
// write_file 通过闭包捕获 roster 和 agentID，写入前声明文件锁，写入后释放。
func registerWorkerTools(tools *agent.ToolRegistry, r roster.Roster, agentID string, cfg *config.Config) {
	// 只读工具
	tools.Register("read_file", "读取指定文件的内容", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "文件路径"},
		},
		"required": []any{"path"},
	}, toolReadFile)

	tools.Register("list_files", "列出指定目录下的文件和子目录", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "目录路径"},
		},
		"required": []any{"path"},
	}, toolListFiles)

	tools.Register("grep_search", "在指定目录中搜索匹配的文本行", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":   map[string]any{"type": "string", "description": "搜索的文本模式"},
			"path":      map[string]any{"type": "string", "description": "搜索的目录或文件路径"},
			"max_lines": map[string]any{"type": "string", "description": "最大返回行数，默认 50"},
		},
		"required": []any{"pattern", "path"},
	}, toolGrepSearch)

	// 写文件工具（通过闭包接入 Roster 文件锁）
	tools.Register("write_file", "将内容写入指定文件（创建或覆盖）", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":          map[string]any{"type": "string", "description": "文件路径"},
			"content":       map[string]any{"type": "string", "description": "要写入的文件内容"},
			"expected_hash": map[string]any{"type": "string", "description": "可选，read_file 返回的 content_hash，用于乐观并发校验"},
		},
		"required": []any{"path", "content"},
	}, makeWriteFileTool(r, agentID))

	// 精准编辑工具（通过闭包接入 Roster 文件锁）
	tools.Register("edit_file", "精准替换文件中的指定内容（old_str → new_str），要求 old_str 在文件中恰好匹配一处", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":          map[string]any{"type": "string", "description": "文件路径"},
			"old_str":       map[string]any{"type": "string", "description": "要替换的原始内容"},
			"new_str":       map[string]any{"type": "string", "description": "替换后的新内容"},
			"expected_hash": map[string]any{"type": "string", "description": "可选，read_file 返回的 content_hash，用于乐观并发校验"},
		},
		"required": []any{"path", "old_str", "new_str"},
	}, makeEditFileTool(r, agentID))

	// Glob 搜索工具
	tools.Register("glob_search", "递归搜索匹配 glob 模式的文件路径，支持 ** 递归通配符", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":  map[string]any{"type": "string", "description": "glob 模式，如 **/*_test.go"},
			"root_dir": map[string]any{"type": "string", "description": "搜索根目录，默认当前目录"},
		},
		"required": []any{"pattern"},
	}, ToolGlobSearch)

	// Shell 执行工具
	tools.Register("run_shell", "在指定目录下执行 shell 命令，返回 stdout、stderr 和 exit code", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":     map[string]any{"type": "string", "description": "要执行的 shell 命令"},
			"working_dir": map[string]any{"type": "string", "description": "工作目录，默认当前目录"},
		},
		"required": []any{"command"},
	}, makeRunShellTool(cfg.ShellTimeoutSec))
}

// --- 辅助函数 ---

// computeSHA256 计算 data 的 SHA256 哈希并返回十六进制摘要字符串。
func computeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

const shellOutputLimit = 10000

// truncateKeepTail 截断字符串，保留尾部 limit 个字符。
// 当 len(output) <= limit 时原样返回；否则保留最后 limit 个字符并在前面添加截断提示。
func truncateKeepTail(output string, limit int) string {
	if len(output) <= limit {
		return output
	}
	truncated := len(output) - limit
	return fmt.Sprintf("[截断提示] 原始输出共 %d 字符，已截断前 %d 字符，仅保留最后 %d 字符\n%s",
		len(output), truncated, limit, output[truncated:])
}

// MatchGlob 判断文件的相对路径是否匹配 glob 模式。
// 当模式包含 ** 时，将模式按 ** 分割为 segments，逐段匹配路径组件。
// 当模式不包含 ** 时，仅对文件名部分调用 filepath.Match。
func MatchGlob(pattern, relPath string) (bool, error) {
	if !strings.Contains(pattern, "**") {
		// 无 ** 时，仅匹配文件名部分
		filename := filepath.Base(relPath)
		return filepath.Match(pattern, filename)
	}

	// 按 ** 分割模式为 segments
	segments := strings.Split(pattern, "**")

	// 将 relPath 拆分为路径组件
	parts := strings.Split(filepath.ToSlash(relPath), "/")

	// 处理前缀 segment（** 之前的部分）
	prefix := strings.Trim(segments[0], "/")
	// 处理后缀 segment（最后一个 ** 之后的部分）
	suffix := strings.Trim(segments[len(segments)-1], "/")

	// 如果有前缀，匹配路径前缀组件
	if prefix != "" {
		prefixParts := strings.Split(prefix, "/")
		if len(prefixParts) > len(parts) {
			return false, nil
		}
		for i, pp := range prefixParts {
			matched, err := filepath.Match(pp, parts[i])
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
		// 消耗已匹配的前缀部分
		parts = parts[len(prefixParts):]
	}

	// 如果有后缀，匹配路径后缀组件（从末尾开始）
	if suffix != "" {
		suffixParts := strings.Split(suffix, "/")
		if len(suffixParts) > len(parts) {
			return false, nil
		}
		for i := 0; i < len(suffixParts); i++ {
			matched, err := filepath.Match(suffixParts[len(suffixParts)-1-i], parts[len(parts)-1-i])
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
		// 消耗已匹配的后缀部分
		parts = parts[:len(parts)-len(suffixParts)]
	}

	// 处理中间 segments（两个 ** 之间的部分）
	for i := 1; i < len(segments)-1; i++ {
		mid := strings.Trim(segments[i], "/")
		if mid == "" {
			continue
		}
		midParts := strings.Split(mid, "/")
		found := false
		for start := 0; start <= len(parts)-len(midParts); start++ {
			allMatch := true
			for j, mp := range midParts {
				matched, err := filepath.Match(mp, parts[start+j])
				if err != nil {
					return false, err
				}
				if !matched {
					allMatch = false
					break
				}
			}
			if allMatch {
				parts = parts[start+len(midParts):]
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}

	return true, nil
}

// --- 工具实现 ---

// ToolGlobSearch 递归遍历目录树，返回匹配 glob 模式的文件路径列表。
// 支持 ** 递归通配符，跳过隐藏目录，结果上限 200 条。
// 导出供 Explorer 引用（worker.ToolGlobSearch）。
func ToolGlobSearch(ctx context.Context, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", fmt.Errorf("缺少 pattern 参数")
	}

	rootDir, _ := args["root_dir"].(string)
	if rootDir == "" {
		rootDir = "."
	}

	// 校验 root_dir 存在
	info, err := os.Stat(rootDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("目录不存在: %s", rootDir)
	}

	const resultLimit = 200
	var matches []string
	totalMatched := 0

	filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 跳过错误条目，继续遍历
		}

		// 跳过隐藏目录
		if info.IsDir() && strings.HasPrefix(info.Name(), ".") && info.Name() != "." {
			return filepath.SkipDir
		}

		// 只匹配文件
		if info.IsDir() {
			return nil
		}

		// 计算相对路径
		relPath, relErr := filepath.Rel(rootDir, path)
		if relErr != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		matched, matchErr := MatchGlob(pattern, relPath)
		if matchErr != nil {
			return nil // 跳过匹配错误
		}
		if matched {
			totalMatched++
			if len(matches) < resultLimit {
				matches = append(matches, relPath)
			}
		}
		return nil
	})

	if totalMatched == 0 {
		return "未找到匹配文件", nil
	}

	result := strings.Join(matches, "\n")
	if totalMatched > resultLimit {
		result += fmt.Sprintf("\n... 结果已截断，共匹配 %d 个文件，仅显示前 200 个", totalMatched)
	}
	return result, nil
}

// shellCommand 根据当前操作系统返回合适的 shell 执行器和参数。
// Windows: cmd /C；Unix: sh -c。
func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

// makeRunShellTool 返回一个带超时控制的 shell 执行工具函数。
// timeoutSec 为 0 时使用默认值 30 秒。
// 根据 runtime.GOOS 自动选择 shell 执行器（Windows: cmd /C；Unix: sh -c）。
func makeRunShellTool(timeoutSec int) agent.ToolFunc {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	return func(ctx context.Context, args map[string]any) (string, error) {
		command, _ := args["command"].(string)
		if command == "" {
			return "", fmt.Errorf("缺少 command 参数")
		}
		workingDir, _ := args["working_dir"].(string)

		timeout := time.Duration(timeoutSec) * time.Second
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		shell, shellArgs := shellCommand(command)
		cmd := exec.CommandContext(execCtx, shell, shellArgs...)
		if workingDir != "" {
			cmd.Dir = workingDir
		}

		output, err := cmd.CombinedOutput()
		outStr := truncateKeepTail(string(output), shellOutputLimit)

		exitCode := 0
		if err != nil {
			if execCtx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("命令执行超时（%d 秒）: %s", timeoutSec, command)
			}
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				return "", fmt.Errorf("启动命令失败: %w", err)
			}
		}

		return fmt.Sprintf("exit_code: %d\nstdout+stderr:\n%s", exitCode, outStr), nil
	}
}

// makeEditFileTool 返回一个接入 Roster 文件锁的精准编辑工具函数。
// 读取、匹配计数、替换写入三步在同一个 Roster 锁持有期间完成。
func makeEditFileTool(r roster.Roster, agentID string) agent.ToolFunc {
	return func(ctx context.Context, args map[string]any) (string, error) {
		path, _ := args["path"].(string)
		oldStr, _ := args["old_str"].(string)
		newStr, _ := args["new_str"].(string)
		expectedHash, _ := args["expected_hash"].(string)

		if path == "" {
			return "", fmt.Errorf("缺少 path 参数")
		}
		if oldStr == "" {
			return "", fmt.Errorf("缺少 old_str 参数")
		}

		// 通过 Roster 声明文件写入权
		claimed, err := r.TryClaim(agentID, path)
		if err != nil {
			return "", fmt.Errorf("文件锁声明失败: %w", err)
		}
		if !claimed {
			occupiedBy, _, _ := r.IsOccupied(path)
			return "", fmt.Errorf("文件 %s 正被代理 %s 占用，无法编辑", path, occupiedBy)
		}
		defer r.Release(agentID, path)

		// 读取文件
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("文件不存在: %s", path)
		}

		// 乐观并发校验：若提供了 expected_hash，校验文件内容哈希一致性
		if expectedHash != "" {
			currentHash := computeSHA256(data)
			if currentHash != expectedHash {
				return "", fmt.Errorf("编辑冲突：文件 %s 的内容已被其他代理修改（期望哈希 %s，当前哈希 %s）。请重新调用 read_file 获取最新内容后再试", path, expectedHash, currentHash)
			}
		}

		content := string(data)

		// 计数匹配
		count := strings.Count(content, oldStr)
		if count == 0 {
			return "", fmt.Errorf("未找到匹配内容，old_str 在文件中不存在")
		}
		if count > 1 {
			return "", fmt.Errorf("匹配到 %d 处，请提供更精确的 old_str", count)
		}

		// 执行替换
		newContent := strings.Replace(content, oldStr, newStr, 1)

		// 写回文件
		if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
			return "", fmt.Errorf("写入文件失败: %w", err)
		}

		oldLen := len(content)
		newLen := len(newContent)
		added := 0
		removed := 0
		if newLen > oldLen {
			added = newLen - oldLen
		} else {
			removed = oldLen - newLen
		}

		return fmt.Sprintf("文件已编辑: %s (字节变化: +%d/-%d)", path, added, removed), nil
	}
}

func toolReadFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("缺少 path 参数")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	// 哈希基于完整内容（截断前计算）
	hash := computeSHA256(data)
	content := string(data)
	if len(content) > 10000 {
		content = content[:10000] + "\n... (截断，文件过大)"
	}
	return content + "\ncontent_hash: " + hash, nil
}

func toolListFiles(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("读取目录失败: %w", err)
	}
	var sb strings.Builder
	for _, entry := range entries {
		if entry.IsDir() {
			sb.WriteString(fmt.Sprintf("[目录] %s/\n", entry.Name()))
		} else {
			sb.WriteString(fmt.Sprintf("[文件] %s\n", entry.Name()))
		}
	}
	return sb.String(), nil
}

func toolGrepSearch(ctx context.Context, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	searchPath, _ := args["path"].(string)
	if pattern == "" || searchPath == "" {
		return "", fmt.Errorf("缺少 pattern 或 path 参数")
	}

	maxLines := 50
	var results []string

	filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") || info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if len(results) >= maxLines {
				return filepath.SkipAll
			}
			if strings.Contains(line, pattern) {
				results = append(results, fmt.Sprintf("%s:%d: %s", path, i+1, line))
			}
		}
		return nil
	})

	if len(results) == 0 {
		return "未找到匹配项", nil
	}
	return strings.Join(results, "\n"), nil
}

// makeWriteFileTool 返回一个接入 Roster 的 write_file 工具函数。
// 写入前通过 TryClaim 声明文件锁，写入完成后 Release 释放。
// 如果文件已被其他代理占用，返回错误提示 LLM 稍后重试或换一个文件。
func makeWriteFileTool(r roster.Roster, agentID string) agent.ToolFunc {
	return func(ctx context.Context, args map[string]any) (string, error) {
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		expectedHash, _ := args["expected_hash"].(string)
		if path == "" {
			return "", fmt.Errorf("缺少 path 参数")
		}

		// 通过 Roster 声明文件写入权
		claimed, err := r.TryClaim(agentID, path)
		if err != nil {
			return "", fmt.Errorf("文件锁声明失败: %w", err)
		}
		if !claimed {
			occupiedBy, _, _ := r.IsOccupied(path)
			return "", fmt.Errorf("文件 %s 正被代理 %s 占用，无法写入", path, occupiedBy)
		}
		defer r.Release(agentID, path)

		// 乐观并发校验：若提供了 expected_hash 且文件已存在，校验哈希一致性
		if expectedHash != "" {
			existing, readErr := os.ReadFile(path)
			if readErr == nil {
				// 文件存在，计算当前内容哈希并比较
				currentHash := computeSHA256(existing)
				if currentHash != expectedHash {
					return "", fmt.Errorf("写入冲突：文件 %s 的内容已被其他代理修改（期望哈希 %s，当前哈希 %s）。请重新调用 read_file 获取最新内容后再试", path, expectedHash, currentHash)
				}
			}
			// 若文件不存在（os.IsNotExist），跳过校验，允许新建
		}

		// 确保父目录存在
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("创建目录失败: %w", err)
		}

		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("写入文件失败: %w", err)
		}

		return fmt.Sprintf("文件已写入: %s (%d 字节)", path, len(content)), nil
	}
}
