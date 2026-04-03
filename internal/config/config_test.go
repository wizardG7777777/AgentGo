package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxRetry != 3 {
		t.Errorf("MaxRetry = %d, want 3", cfg.MaxRetry)
	}
	if cfg.DefaultConcurrency != 2 {
		t.Errorf("DefaultConcurrency = %d, want 2", cfg.DefaultConcurrency)
	}
	if cfg.FIFOLimit != 100 {
		t.Errorf("FIFOLimit = %d, want 100", cfg.FIFOLimit)
	}
	if cfg.DefaultTimeoutSec != 300 {
		t.Errorf("DefaultTimeoutSec = %d, want 300", cfg.DefaultTimeoutSec)
	}
	if cfg.EventChannelBuffer != 64 {
		t.Errorf("EventChannelBuffer = %d, want 64", cfg.EventChannelBuffer)
	}
}

func TestLoadConfig_FileNotExist(t *testing.T) {
	cfg, err := LoadConfig("/nonexistent/path/setting.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxRetry != 3 {
		t.Errorf("MaxRetry = %d, want default 3", cfg.MaxRetry)
	}
}

func TestLoadConfig_PartialYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte("max_retry: 5\nfifo_limit: 200\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxRetry != 5 {
		t.Errorf("MaxRetry = %d, want 5", cfg.MaxRetry)
	}
	if cfg.FIFOLimit != 200 {
		t.Errorf("FIFOLimit = %d, want 200", cfg.FIFOLimit)
	}
	// unspecified fields keep defaults
	if cfg.DefaultConcurrency != 2 {
		t.Errorf("DefaultConcurrency = %d, want default 2", cfg.DefaultConcurrency)
	}
	if cfg.DefaultTimeoutSec != 300 {
		t.Errorf("DefaultTimeoutSec = %d, want default 300", cfg.DefaultTimeoutSec)
	}
}

func TestLoadConfig_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.json")
	content := []byte(`{"max_retry": 7, "fifo_limit": 50, "default_timeout_sec": 120}`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxRetry != 7 {
		t.Errorf("MaxRetry = %d, want 7", cfg.MaxRetry)
	}
	if cfg.FIFOLimit != 50 {
		t.Errorf("FIFOLimit = %d, want 50", cfg.FIFOLimit)
	}
	if cfg.DefaultTimeoutSec != 120 {
		t.Errorf("DefaultTimeoutSec = %d, want 120", cfg.DefaultTimeoutSec)
	}
	// unspecified fields keep defaults
	if cfg.DefaultConcurrency != 2 {
		t.Errorf("DefaultConcurrency = %d, want default 2", cfg.DefaultConcurrency)
	}
}

func TestLoadConfig_FullYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte(`max_retry: 5
default_concurrency: 4
fifo_limit: 200
watchdog_interval_sec: 15
scheduler_ticker_sec: 5
scheduler_max_loops: 20
agent_max_loops: 100
event_channel_buffer: 128
default_timeout_sec: 600
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxRetry != 5 {
		t.Errorf("MaxRetry = %d, want 5", cfg.MaxRetry)
	}
	if cfg.DefaultConcurrency != 4 {
		t.Errorf("DefaultConcurrency = %d, want 4", cfg.DefaultConcurrency)
	}
	if cfg.AgentMaxLoops != 100 {
		t.Errorf("AgentMaxLoops = %d, want 100", cfg.AgentMaxLoops)
	}
	if cfg.EventChannelBuffer != 128 {
		t.Errorf("EventChannelBuffer = %d, want 128", cfg.EventChannelBuffer)
	}
	if cfg.DefaultTimeoutSec != 600 {
		t.Errorf("DefaultTimeoutSec = %d, want 600", cfg.DefaultTimeoutSec)
	}
}

func TestDefaultConfig_LLMFields(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.LLMModel != "gpt-4o" {
		t.Errorf("LLMModel = %q, want %q", cfg.LLMModel, "gpt-4o")
	}
	if cfg.LLMTimeoutSec != 60 {
		t.Errorf("LLMTimeoutSec = %d, want 60", cfg.LLMTimeoutSec)
	}
	if cfg.ExplorerModel != "gpt-4o-mini" {
		t.Errorf("ExplorerModel = %q, want %q", cfg.ExplorerModel, "gpt-4o-mini")
	}
	if cfg.ExplorerEventType != "explore" {
		t.Errorf("ExplorerEventType = %q, want %q", cfg.ExplorerEventType, "explore")
	}
	if cfg.LLMBaseURL != "" {
		t.Errorf("LLMBaseURL = %q, want empty", cfg.LLMBaseURL)
	}
	if cfg.LLMAPIKey != "" {
		t.Errorf("LLMAPIKey = %q, want empty", cfg.LLMAPIKey)
	}
}

func TestLoadConfig_WithLLMFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte(`llm_base_url: "https://api.openai.com"
llm_api_key: "sk-test"
llm_model: "gpt-4o-mini"
llm_timeout_sec: 30
explorer_model: "gpt-3.5-turbo"
explorer_event_type: "investigate"
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMBaseURL != "https://api.openai.com" {
		t.Errorf("LLMBaseURL = %q, want %q", cfg.LLMBaseURL, "https://api.openai.com")
	}
	if cfg.LLMAPIKey != "sk-test" {
		t.Errorf("LLMAPIKey = %q, want %q", cfg.LLMAPIKey, "sk-test")
	}
	if cfg.LLMModel != "gpt-4o-mini" {
		t.Errorf("LLMModel = %q, want %q", cfg.LLMModel, "gpt-4o-mini")
	}
	if cfg.LLMTimeoutSec != 30 {
		t.Errorf("LLMTimeoutSec = %d, want 30", cfg.LLMTimeoutSec)
	}
	if cfg.ExplorerModel != "gpt-3.5-turbo" {
		t.Errorf("ExplorerModel = %q, want %q", cfg.ExplorerModel, "gpt-3.5-turbo")
	}
	if cfg.ExplorerEventType != "investigate" {
		t.Errorf("ExplorerEventType = %q, want %q", cfg.ExplorerEventType, "investigate")
	}
	// 未设置的字段保持默认值
	if cfg.MaxRetry != 3 {
		t.Errorf("MaxRetry = %d, want default 3", cfg.MaxRetry)
	}
}
