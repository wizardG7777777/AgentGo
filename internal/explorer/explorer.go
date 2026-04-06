package explorer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"log"
	"sync"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/isolation"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/pathutil"
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
- 你可能在一个隔离的工作目录（worktree）中工作，不要尝试修改文件或执行写入命令
- 结果应简短明确：结论成立/结论已过时（附当前状态摘要）
- 不要猜测，只报告你实际观察到的内容

代理间通信规范：
- 使用 send_message 时，必须填写 summary（一句话重点）
- 向 scheduler 汇报调查结果时用 msg_type="info"
- 收到 <agent-mail type="question"> 时应尽快回复（msg_type="reply"）
- 收到 <agent-mail type="steer"> 时（尤其 from=user），应立即调整调查方向`

// Explorer 是轻量级只读调查代理，内部组合 agent.Agent。
type Explorer struct {
	agent *agent.Agent
}

// New 创建调查代理。使用低成本 LLM 和只读工具集 + send_message。
// wtManager 可为 nil，表示不启用 worktree 隔离。
func New(s store.TaskStore, r roster.Roster, llmClient llm.Client, cfg *config.Config, cancelReg *store.TaskCancelRegistry, mbRegistry *mailbox.Registry, wtManager *isolation.WorktreeManager) *Explorer {
	const agentID = "explorer-1"
	fileCache := agent.NewFileStateCache(50)
	workdir := &explorerWorkdirHolder{fallback: cfg.ProjectRoot}

	tools := agent.NewToolRegistry()
	registerExplorerTools(tools, workdir, fileCache, mbRegistry, agentID)

	executor := agent.NewLLMExecutor(llmClient, tools, systemPrompt)

	a := agent.NewAgent(
		agentID,
		cfg.ExplorerEventType, // "explore"
		s, r, executor,
		cfg.AgentMaxLoops,
	)
	a.CancelRegistry = cancelReg
	a.MaxRetries = cfg.MaxRetry
	a.IdleThreshold = 0 // 预制代理不因空闲退出
	a.CompactTokenThreshold = cfg.CompactTokenThreshold
	a.CompactKeepRecent = cfg.CompactKeepRecent
	a.FileCache = fileCache
	if mbRegistry != nil {
		a.Mailbox = mbRegistry.Register(agentID, cfg.ExplorerEventType)
		a.MailRegistry = mbRegistry
	}
	a.TeamSnapshot = func() string { return worker.BuildTeamSnapshot(agentID, s, mbRegistry) }
	// Worktree 生命周期（只读使用，不执行 commit/merge）
	if wtManager != nil {
		a.OnTaskStart = func(taskID string) {
			path, err := wtManager.Create(taskID)
			if err != nil {
				log.Printf("[explorer] worktree 创建失败: %v，使用主目录", err)
			} else {
				workdir.Set(path)
			}
		}
		a.OnTaskEnd = func(taskID string, success bool) {
			defer workdir.Set("")
			wtManager.Remove(taskID)
		}
	}

	return &Explorer{agent: a}
}

// explorerWorkdirHolder 与 worker 的 currentWorkdirHolder 等价，线程安全的工作目录持有器。
type explorerWorkdirHolder struct {
	mu       sync.Mutex
	dir      string
	fallback string
}

func (h *explorerWorkdirHolder) Set(dir string) { h.mu.Lock(); h.dir = dir; h.mu.Unlock() }
func (h *explorerWorkdirHolder) Get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dir != "" {
		return h.dir
	}
	return h.fallback
}

// Run 启动调查代理的轮询循环，阻塞直到 ctx 取消。
func (e *Explorer) Run(ctx context.Context) {
	e.agent.Run(ctx)
}

// registerExplorerTools 注册调查代理工具集（只读工具 + send_message）。
// workdir 提供动态工作目录（worktree 路径或 fallback 到 ProjectRoot），为路径安全校验。
// cache 为文件读取缓存，可为 nil。
func registerExplorerTools(tools *agent.ToolRegistry, workdir *explorerWorkdirHolder, cache *agent.FileStateCache, mbRegistry *mailbox.Registry, agentID string) {
	tools.Register("read_file", "读取指定文件的内容", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "文件路径"},
		},
		"required": []any{"path"},
	}, func(ctx context.Context, args map[string]any) (string, error) {
		path, _ := args["path"].(string)
		if path == "" {
			return "", fmt.Errorf("缺少 path 参数")
		}
		if root := workdir.Get(); root != "" {
			validPath, err := pathutil.ValidatePath(path, root)
			if err != nil {
				return "", err
			}
			path = validPath
		}

		// 缓存命中检查
		if cache != nil {
			if content, _, ok := cache.Get(path); ok {
				return content, nil
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读取文件失败: %w", err)
		}
		content := string(data)
		if len(content) > 10000 {
			content = content[:10000] + "\n... (截断，文件过大)"
		}

		// 写入缓存
		if cache != nil {
			cache.Put(path, content, "")
		}

		return content, nil
	})

	tools.Register("list_files", "列出指定目录下的文件和子目录", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "目录路径"},
		},
		"required": []any{"path"},
	}, func(ctx context.Context, args map[string]any) (string, error) {
		path, _ := args["path"].(string)
		if path == "" {
			path = "."
		}
		if root := workdir.Get(); root != "" {
			validPath, err := pathutil.ValidatePath(path, root)
			if err != nil {
				return "", err
			}
			path = validPath
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
	})

	tools.Register("grep_search", "在指定目录中搜索匹配的文本行", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":   map[string]any{"type": "string", "description": "搜索的文本模式"},
			"path":      map[string]any{"type": "string", "description": "搜索的目录或文件路径"},
			"max_lines": map[string]any{"type": "string", "description": "最大返回行数，默认 50"},
		},
		"required": []any{"pattern", "path"},
	}, func(ctx context.Context, args map[string]any) (string, error) {
		pattern, _ := args["pattern"].(string)
		searchPath, _ := args["path"].(string)
		if pattern == "" || searchPath == "" {
			return "", fmt.Errorf("缺少 pattern 或 path 参数")
		}
		if root := workdir.Get(); root != "" {
			validPath, err := pathutil.ValidatePath(searchPath, root)
			if err != nil {
				return "", err
			}
			searchPath = validPath
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
	})

	tools.Register("glob_search", "递归搜索匹配 glob 模式的文件路径，支持 ** 递归通配符", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":  map[string]any{"type": "string", "description": "glob 模式，如 **/*_test.go"},
			"root_dir": map[string]any{"type": "string", "description": "搜索根目录，默认当前目录"},
		},
		"required": []any{"pattern"},
	}, func(ctx context.Context, args map[string]any) (string, error) {
		rootDir, _ := args["root_dir"].(string)
		if rootDir != "" {
			if root := workdir.Get(); root != "" {
				validPath, err := pathutil.ValidatePath(rootDir, root)
				if err != nil {
					return "", err
				}
				args["root_dir"] = validPath
			}
		}
		return worker.ToolGlobSearch(ctx, args)
	})

	// 代理间通信工具
	if mbRegistry != nil {
		tools.Register("send_message", "向指定代理发送结构化消息（点对点或广播）", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":       map[string]any{"type": "string", "description": `收件人代理 ID（如 "worker-1"、"scheduler"），或 "*" 表示广播`},
				"content":  map[string]any{"type": "string", "description": "消息正文（详细内容）"},
				"summary":  map[string]any{"type": "string", "description": "一句话摘要，帮助收信方快速判断消息重点（建议始终填写）"},
				"msg_type": map[string]any{"type": "string", "enum": []any{"info", "question", "reply", "steer"}, "description": `消息类型：info=通知, question=提问/质疑（期望回复）, reply=回复先前消息, steer=纠偏指令。默认 info`},
				"priority": map[string]any{"type": "string", "enum": []any{"low", "normal", "high"}, "description": "优先级：low/normal/high，默认 normal"},
			},
			"required": []any{"to", "content"},
		}, worker.MakeSendMessageTool(mbRegistry, agentID))
	}
}
