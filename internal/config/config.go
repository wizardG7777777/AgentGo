package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	MaxRetry            int `yaml:"max_retry" json:"max_retry"`
	DefaultConcurrency  int `yaml:"default_concurrency" json:"default_concurrency"`
	FIFOLimit           int `yaml:"fifo_limit" json:"fifo_limit"`
	WatchdogIntervalSec int `yaml:"watchdog_interval_sec" json:"watchdog_interval_sec"`
	SchedulerTickerSec  int `yaml:"scheduler_ticker_sec" json:"scheduler_ticker_sec"`
	SchedulerMaxLoops   int `yaml:"scheduler_max_loops" json:"scheduler_max_loops"`
	AgentMaxLoops       int `yaml:"agent_max_loops" json:"agent_max_loops"`
	EventChannelBuffer  int `yaml:"event_channel_buffer" json:"event_channel_buffer"`
	DefaultTimeoutSec   int `yaml:"default_timeout_sec" json:"default_timeout_sec"`
	AgentIdleThreshold  int    `yaml:"agent_idle_threshold" json:"agent_idle_threshold"`
	LLMBaseURL          string `yaml:"llm_base_url" json:"llm_base_url"`
	LLMAPIKey           string `yaml:"llm_api_key" json:"llm_api_key"`
	LLMModel            string `yaml:"llm_model" json:"llm_model"`
	LLMTimeoutSec       int    `yaml:"llm_timeout_sec" json:"llm_timeout_sec"`
	ExplorerModel       string `yaml:"explorer_model" json:"explorer_model"`
	ExplorerEventType   string `yaml:"explorer_event_type" json:"explorer_event_type"`
}

func DefaultConfig() *Config {
	return &Config{
		MaxRetry:           3,
		DefaultConcurrency: 2,
		FIFOLimit:          100,
		WatchdogIntervalSec: 30,
		SchedulerTickerSec: 10,
		SchedulerMaxLoops:  10,
		AgentMaxLoops:      50,
		EventChannelBuffer: 64,
		DefaultTimeoutSec:  300,
		AgentIdleThreshold: 0,
		LLMModel:           "gpt-4o",
		LLMTimeoutSec:      60,
		ExplorerModel:      "gpt-4o-mini",
		ExplorerEventType:  "explore",
	}
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json":
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	default:
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}
