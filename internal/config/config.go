package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	ExplorerModel           string `yaml:"explorer_model" json:"explorer_model"`
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
	SearchAPIProvider       string `yaml:"search_api_provider" json:"search_api_provider"`
	SearchAPIURL            string `yaml:"search_api_url" json:"search_api_url"`
	SearchAPIKey            string `yaml:"search_api_key" json:"search_api_key"`
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
		SearchAPIProvider:       "duckduckgo_html",
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

	return cfg, nil
}
