package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentDeclaration 是单个代理类型的能力声明。
// 使用指针字段区分"未配置"（nil）和"显式空"（非 nil 但值为零值）。
type AgentDeclaration struct {
	// Capabilities 是能力标签列表。
	// nil = 未配置，使用默认值；非 nil 空切片 = 显式清空。
	Capabilities *[]string `yaml:"capabilities" json:"capabilities"`

	// Description 是人类可读的用途描述。
	// nil = 未配置，使用默认值；非 nil 空字符串 = 显式清空。
	Description *string `yaml:"description" json:"description"`
}

// 代理类型默认能力标签
var defaultCapabilities = map[string][]string{
	"explorer": {"codebase_read", "web_search", "message"},
	"worker":   {"code_edit", "shell_exec", "web_search", "subtask_publish", "message"},
}

// 代理类型默认描述
var defaultDescriptions = map[string]string{
	"explorer": "只读调查代理（Explorer），能力限定为 read_file / list_dir / grep_search / glob_search / web_search / web_fetch / send_message。不能写文件、执行 shell、或发布子任务，适合承担代码库调研、网页检索、事实核验等只读任务。",
	"worker":   "通用执行代理，拥有完整工具集，可读写文件、执行 shell 命令、发布子任务、搜索网络",
}

// WorkerDeclaration 是 workers 列表中单条 Worker 声明。
type WorkerDeclaration struct {
	ID      string `yaml:"id" json:"id"`
	Profile string `yaml:"profile" json:"profile"`
}

type Config struct {
	MaxRetry                int    `yaml:"max_retry" json:"max_retry"`
	DefaultConcurrency      int    `yaml:"default_concurrency" json:"default_concurrency"`
	FIFOLimit               int    `yaml:"fifo_limit" json:"fifo_limit"`
	WatchdogIntervalSec     int    `yaml:"watchdog_interval_sec" json:"watchdog_interval_sec"`
	SchedulerTickerSec      int    `yaml:"scheduler_ticker_sec" json:"scheduler_ticker_sec"`
	SchedulerMaxLoops       int    `yaml:"scheduler_max_loops" json:"scheduler_max_loops"`
	AgentMaxLoops           int    `yaml:"agent_max_loops" json:"agent_max_loops"`
	EventChannelBuffer      int    `yaml:"event_channel_buffer" json:"event_channel_buffer"`
	DefaultTimeoutSec       int    `yaml:"default_timeout_sec" json:"default_timeout_sec"`
	AgentIdleThreshold      int    `yaml:"agent_idle_threshold" json:"agent_idle_threshold"`
	LLMBaseURL              string `yaml:"llm_base_url" json:"llm_base_url"`
	LLMAPIKey               string `yaml:"llm_api_key" json:"llm_api_key"`
	LLMModel                string `yaml:"llm_model" json:"llm_model"`
	LLMTimeoutSec           int    `yaml:"llm_timeout_sec" json:"llm_timeout_sec"`
	// LLMProvider 指定 LLM provider 适配器。空串 / 未知名称 → fallback OpenAIProvider（no-op，向后兼容）。
	// 内置可选值："openai" / "deepseek-v4" / "deepseek-r1"。
	// 详见 internal/llm/provider.go 的 Provider 接口与 provider_builtin.go 的内置实现。
	LLMProvider string `yaml:"llm_provider" json:"llm_provider"`
	// ExplorerProvider 为 explorer 代理单独指定 provider。空串则 fallback 到 LLMProvider。
	ExplorerProvider  string `yaml:"explorer_provider" json:"explorer_provider"`
	ExplorerModel     string `yaml:"explorer_model" json:"explorer_model"`
	ExplorerEventType       string `yaml:"explorer_event_type" json:"explorer_event_type"`
	ShellTimeoutSec         int    `yaml:"shell_timeout_sec" json:"shell_timeout_sec"`
	CompactTokenThreshold   int    `yaml:"compact_token_threshold" json:"compact_token_threshold"`
	CompactKeepRecent       int    `yaml:"compact_keep_recent" json:"compact_keep_recent"`
	ProjectRoot             string `yaml:"project_root" json:"project_root"`
	MaxSubtaskDepth         int    `yaml:"max_subtask_depth" json:"max_subtask_depth"`
	WorkerCount             int    `yaml:"worker_count" json:"worker_count"`
	MailboxBufferSize       int    `yaml:"mailbox_buffer_size" json:"mailbox_buffer_size"`
	MailNotifierIntervalSec int    `yaml:"mail_notifier_interval_sec" json:"mail_notifier_interval_sec"`
	// MailNotifierEnabled 控制邮差通知器是否启动。Phase 2 完成后默认 true：
	// 4 项防御已经全部到位（ChainDepthLimitHook 截断深链 + PerAgentDedupHook
	// 镜像去重 + WakeContextExpandHook 上下文注入 + worker/explorer 提示词
	// 弱化"必回复"），邮件级联爆炸的根因都被堵住了。如有需要可用 yaml 强制
	// 关闭。详见 KNOWN_ISSUES.md。
	MailNotifierEnabled bool `yaml:"mail_notifier_enabled" json:"mail_notifier_enabled"`
	// MailChainMaxDepth 是邮件链跳数上限。worker 通过 send_message 触发的邮件
	// 继承"自己当前任务的 MailChainDepth + 1"；超过此阈值的消息仍然会投递到
	// 收件箱（保留可见性），但不会触发 mail-notifier 发布唤醒任务，从而打断
	// 邮件级联爆炸链。Phase 2 引入；用户 /steer 投递的初始邮件 ChainDepth=0。
	MailChainMaxDepth int `yaml:"mail_chain_max_depth" json:"mail_chain_max_depth"`
	// TransferNoteMaxTokens 是 TransferNote 单条最大 token 预算。agent 在生成
	// L1/L3 交接备忘时按此预算截断文本长度——按 1 token ≈ 2 runes 估算。
	// 默认 3000 对应 ~6000 字符中文或 ~12000 字符英文。
	// 参考 nextUpgrade_v3.md §8.4.6 的 token 预算规划。
	TransferNoteMaxTokens int `yaml:"transfer_note_max_tokens" json:"transfer_note_max_tokens"`
	// RosterWaitTimeoutSec 是文件冲突排队的最大等待时间（秒）。当 TryClaim 失败时，
	// 工具层调用 Roster.WaitForRelease 阻塞等待前任释放，超时后放弃并返回"���用"错误。
	// 设为 0 表示不排队（退回旧行为：立即返回错误）。
	// 参考 nextUpgrade_v3.md §8.3 文件冲突排队设计。
	RosterWaitTimeoutSec int `yaml:"roster_wait_timeout_sec" json:"roster_wait_timeout_sec"`
	// ProgressNotifyEnabled 控制进度通知功能是否启用。启用后，Worker Agent 在完成
	// 文件写入、子任务发布或任务过半时，通过 mailbox 向相关 Agent 发送轻量级进度消息。
	// 参考 nextUpgrade_v3.md §8.6 进度通知设计。
	ProgressNotifyEnabled bool `yaml:"progress_notify_enabled" json:"progress_notify_enabled"`

	SearchAPIProvider string `yaml:"search_api_provider" json:"search_api_provider"`
	SearchAPIURL      string `yaml:"search_api_url" json:"search_api_url"`
	SearchAPIKey      string `yaml:"search_api_key" json:"search_api_key"`

	// Shell 命令拦截配置（追加到默认规则）
	ShellBlacklist []string `yaml:"shell_blacklist" json:"shell_blacklist"`
	ShellGreylist  []string `yaml:"shell_greylist" json:"shell_greylist"`

	// 工具集分层配置（Tool Set Profiles，nextUpgrade_v3 §9.1）。
	// 命名工具集：profile_name → [tool_name, ...]
	// 留空时所有 agent 使用各自代码内置的默认工具集（向后兼容）。
	ToolProfiles map[string][]string `yaml:"tool_profiles" json:"tool_profiles"`
	// WorkerProfile / ExplorerProfile 指定各 agent 类型使用的工具集名称。
	// 留空 = 注册全部可用工具（向后兼容）。
	// 值必须是 ToolProfiles 中已定义的 key，否则启动报错。
	// Scheduler 不走 profile（其工具强耦合于一等代理角色，不开放配置）。
	WorkerProfile   string `yaml:"worker_profile" json:"worker_profile"`
	ExplorerProfile string `yaml:"explorer_profile" json:"explorer_profile"`

	// Workers 是 per-worker 工具集声明列表。
	// 非空时覆盖 WorkerCount + WorkerProfile 的旧行为。
	// 空/nil 时回退到旧行为（向后兼容）。
	Workers []WorkerDeclaration `yaml:"workers" json:"workers"`

	// AgentDeclarations 是代理能力声明配置。key 为代理类型名称（"worker" / "explorer"），
	// value 包含 capabilities 和 description。留空时使用内置默认值。
	AgentDeclarations map[string]AgentDeclaration `yaml:"agent_declarations" json:"agent_declarations"`

	// SessionRetentionDays 是 Session 保留天数。超过此天数的已关闭 Session 将被归档。
	SessionRetentionDays int `yaml:"session_retention_days" json:"session_retention_days"`
	// SessionArchiveMax 是最大归档 Session 数。超过此数量时删除最旧的归档。
	SessionArchiveMax int `yaml:"session_archive_max" json:"session_archive_max"`
}

// ResolveProfile 根据 profile 名称从 ToolProfiles 中查找工具列表。
//   - name 为空 → 返回 nil（意为"允许全部工具"，向后兼容）
//   - name 不存在于 ToolProfiles → 返回 error（配置笔误应立即暴露）
func (c *Config) ResolveProfile(name string) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	if c.ToolProfiles == nil {
		return nil, fmt.Errorf("tool profile %q 未找到：tool_profiles 未定义", name)
	}
	tools, ok := c.ToolProfiles[name]
	if !ok {
		return nil, fmt.Errorf("tool profile %q 未找到，可用的 profile: %v", name, profileKeys(c.ToolProfiles))
	}
	return tools, nil
}

// ResolvedAgentDeclaration 返回指定代理类型的最终声明（合并默认值）。
// 未配置的字段回退到默认值；显式空值（非 nil 零值）保持原样。
// 未知代理类型返回零值（nil capabilities, 空 description）。
func (c *Config) ResolvedAgentDeclaration(agentType string) ([]string, string) {
	defCaps, knownCaps := defaultCapabilities[agentType]
	defDesc, knownDesc := defaultDescriptions[agentType]
	if !knownCaps && !knownDesc {
		// 未知代理类型：返回零值
		return nil, ""
	}

	decl, exists := c.AgentDeclarations[agentType]
	if !exists {
		// 未配置该代理类型：返回完整默认值
		return defCaps, defDesc
	}

	// 合并逻辑：nil 字段回退默认值，非 nil 字段原样返回
	caps := defCaps
	if decl.Capabilities != nil {
		caps = *decl.Capabilities
	}
	desc := defDesc
	if decl.Description != nil {
		desc = *decl.Description
	}
	return caps, desc
}

// ResolvedWorkerDeclaration 返回指定 profile 的 Worker 能力声明（合并默认值）。
// 查找顺序：
//  1. agent_declarations["worker/<profile>"]（profile 为空时跳过）
//  2. agent_declarations["worker"]
//  3. 内置默认值
//
// 未配置的字段回退到默认值；显式空值（非 nil 零值）保持原样。
func (c *Config) ResolvedWorkerDeclaration(profile string) ([]string, string) {
	defCaps := defaultCapabilities["worker"]
	defDesc := defaultDescriptions["worker"]

	// 尝试 per-profile 声明：agent_declarations["worker/<profile>"]
	if profile != "" {
		key := "worker/" + profile
		if decl, ok := c.AgentDeclarations[key]; ok {
			caps := defCaps
			if decl.Capabilities != nil {
				caps = *decl.Capabilities
			}
			desc := defDesc
			if decl.Description != nil {
				desc = *decl.Description
			}
			return caps, desc
		}
	}

	// 回退到通用 worker 声明：agent_declarations["worker"]
	if decl, ok := c.AgentDeclarations["worker"]; ok {
		caps := defCaps
		if decl.Capabilities != nil {
			caps = *decl.Capabilities
		}
		desc := defDesc
		if decl.Description != nil {
			desc = *decl.Description
		}
		return caps, desc
	}

	// 回退到内置默认值
	return defCaps, defDesc
}

// ValidateWorkers 校验 workers 列表的合法性。
// 检查项：空 ID、重复 ID、profile 引用是否存在于 ToolProfiles。
// workers 为空/nil 时返回 nil（向后兼容路径不校验）。
func (c *Config) ValidateWorkers() error {
	if len(c.Workers) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(c.Workers))
	for i, w := range c.Workers {
		if w.ID == "" {
			return fmt.Errorf("workers[%d] 的 id 不能为空", i)
		}
		if _, dup := seen[w.ID]; dup {
			return fmt.Errorf("workers 列表中存在重复的 id: %q", w.ID)
		}
		seen[w.ID] = struct{}{}

		if w.Profile != "" {
			if _, err := c.ResolveProfile(w.Profile); err != nil {
				return fmt.Errorf("workers[%d] (id=%q) 的 profile 无效: %w", i, w.ID, err)
			}
		}
	}
	return nil
}

// ValidateAgentDeclarations 校验 agent_declarations 中的代理类型名称。
// 允许 "worker"、"explorer" 和 "worker/<profile_name>" 格式的 key。
// 其他名称返回错误。
func (c *Config) ValidateAgentDeclarations() error {
	for name := range c.AgentDeclarations {
		if name == "worker" || name == "explorer" {
			continue
		}
		if strings.HasPrefix(name, "worker/") {
			continue
		}
		return fmt.Errorf("agent_declarations 包含无效的代理类型名称: %q（仅允许 \"worker\"、\"explorer\" 和 \"worker/<profile_name>\"）", name)
	}
	return nil
}

// profileKeys 返回 map 的所有 key（用于错误消息）。
func profileKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func DefaultConfig() *Config {
	return &Config{
		MaxRetry:                3,
		DefaultConcurrency:      2,
		FIFOLimit:               100,
		WatchdogIntervalSec:     30,
		SchedulerTickerSec:      10,
		SchedulerMaxLoops:       10,
		AgentMaxLoops:           50,
		EventChannelBuffer:      64,
		DefaultTimeoutSec:       300,
		AgentIdleThreshold:      0,
		LLMModel:                "gpt-4o",
		LLMTimeoutSec:           60,
		LLMProvider:             "openai", // 默认走标准 OpenAI 协议；deepseek-v4-flash 等非标后端需显式指定
		ExplorerModel:           "gpt-4o-mini",
		ExplorerEventType:       "explore",
		ShellTimeoutSec:         30,
		CompactTokenThreshold:   80000,
		CompactKeepRecent:       3,
		MaxSubtaskDepth:         1,
		WorkerCount:             1,
		MailboxBufferSize:       32,
		MailNotifierIntervalSec: 5,
		MailNotifierEnabled:     true, // Phase 2 完成；4 项防御已就绪，恢复默认启用
		MailChainMaxDepth:       3,    // Phase 2 新增；与 hookSystem.md §3.2 一致
		TransferNoteMaxTokens:   3000, // Sprint 3 #5 TransferNote ���认预算
		RosterWaitTimeoutSec:    30,   // §8.3 文件冲突排队默认等待 30 秒
		ProgressNotifyEnabled:   true, // §8.6 进度通知默认启用
		SearchAPIProvider:       "duckduckgo_html",
		SessionRetentionDays:    30,
		SessionArchiveMax:       50,
	}
}

// LoadConfig 加载配置文件。
// explicit 为 true 表示用户显式指定了路径：文件不存在或格式不支持时直接报错。
// explicit 为 false 表示使用默认路径：文件不存在或格式不支持时使用默认配置。
func LoadConfig(path string, explicit bool) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if explicit {
				return nil, fmt.Errorf("配置文件不存在: %s", path)
			}
			fmt.Fprintf(os.Stderr, "[警告] 默认配置文件 %s 不存在，使用内置默认配置\n", path)
			return cfg, nil
		}
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	case ".json":
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	default:
		if explicit {
			return nil, fmt.Errorf("不支持的配置文件格式: %s（仅支持 .yaml/.yml/.json）", ext)
		}
		return cfg, nil
	}

	// 校验 agent_declarations 中的代理类型名称
	if err := cfg.ValidateAgentDeclarations(); err != nil {
		return nil, err
	}

	// 校验 workers 列表（空 ID / 重复 ID / 未知 profile）
	if err := cfg.ValidateWorkers(); err != nil {
		return nil, err
	}

	return cfg, nil
}
