package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	MaxRetry           int `yaml:"max_retry"`
	DefaultConcurrency int `yaml:"default_concurrency"`
	FIFOLimit          int `yaml:"fifo_limit"`
	WatchdogIntervalSec int `yaml:"watchdog_interval_sec"`
	SchedulerTickerSec int `yaml:"scheduler_ticker_sec"`
	SchedulerMaxLoops  int `yaml:"scheduler_max_loops"`
	AgentMaxLoops      int `yaml:"agent_max_loops"`
	EventChannelBuffer int `yaml:"event_channel_buffer"`
	DefaultTimeoutSec  int `yaml:"default_timeout_sec"`
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

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
