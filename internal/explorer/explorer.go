package explorer

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
	"agentgo/internal/worker"
)

const systemPrompt = `你是一个调查代理（Explorer），专门执行只读的信息检索和验证任务。

你的职责：
- 读取项目文件，了解代码结构和内容
- 搜索项目中的关键字和模式
- 验证历史结论是否仍然成立
- 返回简洁明确的调查结果

你的限制：
- 只能执行只读操作，不能修改任何文件
- 结果应简短明确：结论成立/结论已过时（附当前状态摘要）
- 不要猜测，只报告你实际观察到的内容`

// Explorer 是轻量级只读调查代理，内部组合 agent.Agent。
type Explorer struct {
	agent *agent.Agent
}

// New 创建调查代理。使用低成本 LLM 和只读工具集。
func New(s store.TaskStore, r roster.Roster, llmClient llm.Client, cfg *config.Config, cancelReg *store.TaskCancelRegistry) *Explorer {
	tools := agent.NewToolRegistry()
	registerReadOnlyTools(tools)

	executor := agent.NewLLMExecutor(llmClient, tools, systemPrompt)

	a := agent.NewAgent(
		"explorer-1",
		cfg.ExplorerEventType, // "explore"
		s, r, executor,
		cfg.AgentMaxLoops,
	)
	a.CancelRegistry = cancelReg
	a.MaxRetries = cfg.MaxRetry
	a.IdleThreshold = 0 // 预制代理不因空闲退出

	return &Explorer{agent: a}
}

// Run 启动调查代理的轮询循环，阻塞直到 ctx 取消。
func (e *Explorer) Run(ctx context.Context) {
	e.agent.Run(ctx)
}

// registerReadOnlyTools 注册只读工具集。
func registerReadOnlyTools(tools *agent.ToolRegistry) {
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

	tools.Register("glob_search", "递归搜索匹配 glob 模式的文件路径，支持 ** 递归通配符", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":  map[string]any{"type": "string", "description": "glob 模式，如 **/*_test.go"},
			"root_dir": map[string]any{"type": "string", "description": "搜索根目录，默认当前目录"},
		},
		"required": []any{"pattern"},
	}, worker.ToolGlobSearch)
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
	// 限制返回大小，避免超出 LLM 上下文
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
		// 跳过二进制和隐藏文件
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
