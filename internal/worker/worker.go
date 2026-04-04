package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
- 根据任务描述修改或创建文件
- 完成后返回简洁的执行结果摘要

你的工作方式：
- 先用 read_file 和 grep_search 了解相关代码
- 确认修改方案后用 write_file 写入文件
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
	registerWorkerTools(tools, r, agentID)

	executor := agent.NewLLMExecutor(llmClient, tools)

	a := agent.NewAgent(
		agentID,
		"",   // 空字符串，匹配 scheduler 发布的执行任务
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
func registerWorkerTools(tools *agent.ToolRegistry, r roster.Roster, agentID string) {
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
			"path":    map[string]any{"type": "string", "description": "文件路径"},
			"content": map[string]any{"type": "string", "description": "要写入的文件内容"},
		},
		"required": []any{"path", "content"},
	}, makeWriteFileTool(r, agentID))
}

// --- 工具实现 ---

func toolReadFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("缺少 path 参数")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	content := string(data)
	if len(content) > 10000 {
		content = content[:10000] + "\n... (截断，文件过大)"
	}
	return content, nil
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
