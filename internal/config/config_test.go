package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
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
	_, err := LoadConfig("/nonexistent/path/setting.yaml", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfig_PartialYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte("fifo_limit: 200\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	content := []byte(`{"fifo_limit": 50, "default_timeout_sec": 120}`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
	content := []byte(`default_concurrency: 4
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

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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

func TestDefaultConfig_ProgressNotifyEnabled(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.ProgressNotifyEnabled {
		t.Errorf("ProgressNotifyEnabled = %v, want true", cfg.ProgressNotifyEnabled)
	}
}

func TestDefaultConfig_ShellTimeoutSec(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ShellTimeoutSec != 30 {
		t.Errorf("ShellTimeoutSec = %d, want 30", cfg.ShellTimeoutSec)
	}
}

func TestLoadConfig_ShellTimeoutSec_YAMLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte("shell_timeout_sec: 60\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ShellTimeoutSec != 60 {
		t.Errorf("ShellTimeoutSec = %d, want 60", cfg.ShellTimeoutSec)
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

	cfg, err := LoadConfig(path, false)
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
	if cfg.DefaultConcurrency != 2 {
		t.Errorf("DefaultConcurrency = %d, want default 2", cfg.DefaultConcurrency)
	}
}

// === explicit=true 场景测试 ===

func TestLoadConfig_Explicit_FileNotExist_Error(t *testing.T) {
	_, err := LoadConfig("/nonexistent/setting.yaml", true)
	if err == nil {
		t.Fatal("expected error when explicit=true and file not found")
	}
}

func TestLoadConfig_Explicit_UnsupportedFormat_Error(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.toml")
	os.WriteFile(path, []byte("key = 'value'"), 0644)

	_, err := LoadConfig(path, true)
	if err == nil {
		t.Fatal("expected error for unsupported format with explicit=true")
	}
}

func TestLoadConfig_NonExplicit_UnsupportedFormat_DefaultConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.toml")
	os.WriteFile(path, []byte("key = 'value'"), 0644)

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DefaultConcurrency != 2 {
		t.Errorf("DefaultConcurrency = %d, want default 2", cfg.DefaultConcurrency)
	}
}

func TestLoadConfig_Explicit_ValidYML_OK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yml")
	os.WriteFile(path, []byte("fifo_limit: 99"), 0644)

	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.FIFOLimit != 99 {
		t.Errorf("FIFOLimit = %d, want 99", cfg.FIFOLimit)
	}
}

// === Tool Profiles 测试 ===

func TestResolveProfile_EmptyName_ReturnsNil(t *testing.T) {
	cfg := &Config{
		ToolProfiles: map[string][]string{
			"test_profile": {"read_file", "write_file"},
		},
	}

	tools, err := cfg.ResolveProfile("")
	if err != nil {
		t.Errorf("unexpected error for empty name: %v", err)
	}
	if tools != nil {
		t.Errorf("expected nil for empty name, got %v", tools)
	}
}

func TestResolveProfile_ProfileNotFound_ReturnsError(t *testing.T) {
	cfg := &Config{
		ToolProfiles: map[string][]string{
			"test_profile": {"read_file", "write_file"},
		},
	}

	_, err := cfg.ResolveProfile("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent profile")
	}
	if !containsSubstring(err.Error(), "未找到") {
		t.Errorf("error message should contain '未找到', got: %s", err.Error())
	}
}

func TestResolveProfile_ProfileFound_ReturnsTools(t *testing.T) {
	cfg := &Config{
		ToolProfiles: map[string][]string{
			"readonly": {"read_file", "list_dir", "grep_search"},
		},
	}

	tools, err := cfg.ResolveProfile("readonly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 3 {
		t.Errorf("expected 3 tools, got %d", len(tools))
	}
	expectedTools := map[string]bool{"read_file": true, "list_dir": true, "grep_search": true}
	for _, tool := range tools {
		if !expectedTools[tool] {
			t.Errorf("unexpected tool: %s", tool)
		}
	}
}

func TestResolveProfile_NilToolProfiles_ReturnsError(t *testing.T) {
	cfg := &Config{
		ToolProfiles: nil,
	}

	_, err := cfg.ResolveProfile("any_profile")
	if err == nil {
		t.Fatal("expected error when ToolProfiles is nil")
	}
	if !containsSubstring(err.Error(), "tool_profiles 未定义") {
		t.Errorf("error message should contain 'tool_profiles 未定义', got: %s", err.Error())
	}
}

func TestLoadConfig_ToolProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte(`
tool_profiles:
  worker_standard:
    - read_file
    - write_file
    - run_shell
  explorer_readonly:
    - read_file
    - list_dir
worker_profile: worker_standard
explorer_profile: explorer_readonly
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 验证 ToolProfiles
	if len(cfg.ToolProfiles) != 2 {
		t.Errorf("expected 2 profiles, got %d", len(cfg.ToolProfiles))
	}
	if len(cfg.ToolProfiles["worker_standard"]) != 3 {
		t.Errorf("expected 3 tools in worker_standard, got %d", len(cfg.ToolProfiles["worker_standard"]))
	}

	// 验证 profile 引用
	if cfg.WorkerProfile != "worker_standard" {
		t.Errorf("WorkerProfile = %q, want %q", cfg.WorkerProfile, "worker_standard")
	}
	if cfg.ExplorerProfile != "explorer_readonly" {
		t.Errorf("ExplorerProfile = %q, want %q", cfg.ExplorerProfile, "explorer_readonly")
	}

	// 验证 ResolveProfile
	tools, err := cfg.ResolveProfile("worker_standard")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tools) != 3 {
		t.Errorf("expected 3 tools, got %d", len(tools))
	}
}

func TestLoadConfig_ToolProfiles_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.json")
	content := []byte(`{
		"tool_profiles": {
			"minimal": ["read_file", "write_file"]
		},
		"worker_profile": "minimal"
	}`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.ToolProfiles["minimal"]) != 2 {
		t.Errorf("expected 2 tools in minimal profile, got %d", len(cfg.ToolProfiles["minimal"]))
	}
	if cfg.WorkerProfile != "minimal" {
		t.Errorf("WorkerProfile = %q, want %q", cfg.WorkerProfile, "minimal")
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstringHelper(s, substr))
}

func containsSubstringHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// === AgentDeclaration 单元测试 ===

func TestResolvedAgentDeclaration_DefaultsWhenEmpty(t *testing.T) {
	cfg := DefaultConfig()

	// explorer defaults
	caps, desc := cfg.ResolvedAgentDeclaration("explorer")
	expectedCaps := []string{"codebase_read", "web_search", "message"}
	if len(caps) != len(expectedCaps) {
		t.Fatalf("explorer caps len = %d, want %d", len(caps), len(expectedCaps))
	}
	for i, c := range caps {
		if c != expectedCaps[i] {
			t.Errorf("explorer caps[%d] = %q, want %q", i, c, expectedCaps[i])
		}
	}
	if desc == "" {
		t.Error("explorer default description should not be empty")
	}

	// worker defaults
	caps, desc = cfg.ResolvedAgentDeclaration("worker")
	expectedCaps = []string{"code_edit", "shell_exec", "web_search", "subtask_publish", "message"}
	if len(caps) != len(expectedCaps) {
		t.Fatalf("worker caps len = %d, want %d", len(caps), len(expectedCaps))
	}
	for i, c := range caps {
		if c != expectedCaps[i] {
			t.Errorf("worker caps[%d] = %q, want %q", i, c, expectedCaps[i])
		}
	}
	if desc != "通用执行代理，拥有完整工具集，可读写文件、执行 shell 命令、发布子任务、搜索网络" {
		t.Errorf("worker default description = %q, unexpected", desc)
	}
}

func TestResolvedAgentDeclaration_UnknownType(t *testing.T) {
	cfg := DefaultConfig()
	caps, desc := cfg.ResolvedAgentDeclaration("unknown_agent")
	if caps != nil {
		t.Errorf("unknown type caps = %v, want nil", caps)
	}
	if desc != "" {
		t.Errorf("unknown type desc = %q, want empty", desc)
	}
}

func TestResolvedAgentDeclaration_ExplicitEmpty(t *testing.T) {
	emptyCaps := []string{}
	emptyDesc := ""
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"explorer": {
			Capabilities: &emptyCaps,
			Description:  &emptyDesc,
		},
	}

	caps, desc := cfg.ResolvedAgentDeclaration("explorer")
	if caps == nil || len(caps) != 0 {
		t.Errorf("explicit empty caps should be non-nil empty slice, got %v", caps)
	}
	if desc != "" {
		t.Errorf("explicit empty desc should be empty string, got %q", desc)
	}
}

func TestResolvedAgentDeclaration_PartialOverride(t *testing.T) {
	customCaps := []string{"custom_cap"}
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker": {
			Capabilities: &customCaps,
			// Description nil → use default
		},
	}

	caps, desc := cfg.ResolvedAgentDeclaration("worker")
	if len(caps) != 1 || caps[0] != "custom_cap" {
		t.Errorf("caps = %v, want [custom_cap]", caps)
	}
	if desc != "通用执行代理，拥有完整工具集，可读写文件、执行 shell 命令、发布子任务、搜索网络" {
		t.Errorf("desc should be default, got %q", desc)
	}
}

func TestValidateAgentDeclarations_Valid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker":   {},
		"explorer": {},
	}
	if err := cfg.ValidateAgentDeclarations(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateAgentDeclarations_Invalid(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker": {},
		"hacker": {},
	}
	err := cfg.ValidateAgentDeclarations()
	if err == nil {
		t.Fatal("expected error for invalid agent type")
	}
	if !containsSubstring(err.Error(), "hacker") {
		t.Errorf("error should contain 'hacker', got: %s", err.Error())
	}
}

// === ValidateAgentDeclarations: worker/<profile> format tests ===

func TestValidateAgentDeclarations_WorkerProfileFormat_Accepted(t *testing.T) {
	tests := []struct {
		name string
		keys []string
	}{
		{"single worker profile", []string{"worker/worker_readonly"}},
		{"multiple worker profiles", []string{"worker/worker_readonly", "worker/worker_full"}},
		{"worker profile with base types", []string{"worker", "explorer", "worker/readonly"}},
		{"worker profile only", []string{"worker/my_profile"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.AgentDeclarations = make(map[string]AgentDeclaration)
			for _, k := range tt.keys {
				cfg.AgentDeclarations[k] = AgentDeclaration{}
			}
			if err := cfg.ValidateAgentDeclarations(); err != nil {
				t.Errorf("expected no error for keys %v, got: %v", tt.keys, err)
			}
		})
	}
}

func TestValidateAgentDeclarations_InvalidPrefixedKeys_Rejected(t *testing.T) {
	tests := []struct {
		name       string
		invalidKey string
	}{
		{"hacker prefix", "hacker/something"},
		{"explorer prefix", "explorer/readonly"},
		{"scheduler type", "scheduler"},
		{"random prefix", "foo/bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.AgentDeclarations = map[string]AgentDeclaration{
				tt.invalidKey: {},
			}
			err := cfg.ValidateAgentDeclarations()
			if err == nil {
				t.Fatalf("expected error for key %q, got nil", tt.invalidKey)
			}
			if !strings.Contains(err.Error(), tt.invalidKey) {
				t.Errorf("error should contain %q, got: %s", tt.invalidKey, err.Error())
			}
		})
	}
}

func TestLoadConfig_InvalidAgentDeclaration_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte(`
agent_declarations:
  explorer:
    capabilities:
      - codebase_read
  invalid_type:
    capabilities:
      - something
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path, false)
	if err == nil {
		t.Fatal("expected error for invalid agent type in config")
	}
	if !containsSubstring(err.Error(), "invalid_type") {
		t.Errorf("error should contain 'invalid_type', got: %s", err.Error())
	}
}

func TestLoadConfig_AgentDeclarations_YAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte(`
agent_declarations:
  explorer:
    capabilities:
      - codebase_read
      - web_search
    description: "custom explorer"
  worker:
    capabilities: []
    description: ""
`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// explorer: custom values
	caps, desc := cfg.ResolvedAgentDeclaration("explorer")
	if len(caps) != 2 || caps[0] != "codebase_read" || caps[1] != "web_search" {
		t.Errorf("explorer caps = %v, unexpected", caps)
	}
	if desc != "custom explorer" {
		t.Errorf("explorer desc = %q, want 'custom explorer'", desc)
	}

	// worker: explicit empty
	caps, desc = cfg.ResolvedAgentDeclaration("worker")
	if caps == nil || len(caps) != 0 {
		t.Errorf("worker caps should be explicit empty, got %v", caps)
	}
	if desc != "" {
		t.Errorf("worker desc should be explicit empty, got %q", desc)
	}
}

// === 属性测试（Property-Based Tests）===

// Feature: agent-capability-declaration, Property 1: config resolution merge
// **Validates: Requirements 1.2, 1.3, 1.4, 6.1, 6.2**
//
// 使用 rapid 生成随机 AgentDeclaration（Capabilities 和 Description 随机为 nil / 空 / 有值），
// 验证 ResolvedAgentDeclaration 的合并逻辑。
func TestProperty_ConfigResolutionMerge(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		agentType := rapid.SampledFrom([]string{"worker", "explorer"}).Draw(t, "agentType")

		// 生成随机 AgentDeclaration
		decl := drawAgentDeclaration(t)

		cfg := DefaultConfig()
		cfg.AgentDeclarations = map[string]AgentDeclaration{
			agentType: decl,
		}

		caps, desc := cfg.ResolvedAgentDeclaration(agentType)

		// 验证 Capabilities 合并逻辑
		if decl.Capabilities == nil {
			// nil → 使用默认值
			expectedCaps := defaultCapabilities[agentType]
			if len(caps) != len(expectedCaps) {
				t.Fatalf("nil Capabilities: caps len = %d, want %d", len(caps), len(expectedCaps))
			}
			for i, c := range caps {
				if c != expectedCaps[i] {
					t.Errorf("nil Capabilities: caps[%d] = %q, want %q", i, c, expectedCaps[i])
				}
			}
		} else {
			// 非 nil → 原样返回
			expected := *decl.Capabilities
			if len(caps) != len(expected) {
				t.Fatalf("non-nil Capabilities: caps len = %d, want %d", len(caps), len(expected))
			}
			for i, c := range caps {
				if c != expected[i] {
					t.Errorf("non-nil Capabilities: caps[%d] = %q, want %q", i, c, expected[i])
				}
			}
		}

		// 验证 Description 合并逻辑
		if decl.Description == nil {
			// nil → 使用默认值
			expectedDesc := defaultDescriptions[agentType]
			if desc != expectedDesc {
				t.Errorf("nil Description: desc = %q, want %q", desc, expectedDesc)
			}
		} else {
			// 非 nil → 原样返回
			if desc != *decl.Description {
				t.Errorf("non-nil Description: desc = %q, want %q", desc, *decl.Description)
			}
		}
	})
}

// Feature: agent-capability-declaration, Property 1 supplement: empty AgentDeclarations
// **Validates: Requirements 1.2, 1.3, 1.4, 6.1, 6.2**
//
// 当 agent_declarations 整体为空/nil 时，所有代理类型均返回各自的完整默认值。
func TestProperty_ConfigResolutionMerge_EmptyDeclarations(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		agentType := rapid.SampledFrom([]string{"worker", "explorer"}).Draw(t, "agentType")

		cfg := DefaultConfig()
		// AgentDeclarations is nil by default

		caps, desc := cfg.ResolvedAgentDeclaration(agentType)

		expectedCaps := defaultCapabilities[agentType]
		expectedDesc := defaultDescriptions[agentType]

		if len(caps) != len(expectedCaps) {
			t.Fatalf("empty declarations: caps len = %d, want %d", len(caps), len(expectedCaps))
		}
		for i, c := range caps {
			if c != expectedCaps[i] {
				t.Errorf("empty declarations: caps[%d] = %q, want %q", i, c, expectedCaps[i])
			}
		}
		if desc != expectedDesc {
			t.Errorf("empty declarations: desc = %q, want %q", desc, expectedDesc)
		}
	})
}

// Feature: agent-capability-declaration, Property 2: unknown agent type validation
// **Validates: Requirements 1.5**
//
// 使用 rapid 生成随机字符串作为代理类型名，验证非 "worker"/"explorer" 的名称必定触发错误。
func TestProperty_UnknownAgentTypeValidation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// 生成随机字符串，过滤掉合法名称
		name := rapid.String().Draw(t, "agentTypeName")
		if name == "worker" || name == "explorer" || strings.HasPrefix(name, "worker/") {
			return // skip valid names
		}

		cfg := DefaultConfig()
		cfg.AgentDeclarations = map[string]AgentDeclaration{
			name: {},
		}

		err := cfg.ValidateAgentDeclarations()
		if err == nil {
			t.Fatalf("expected error for agent type %q, got nil", name)
		}
		// Use fmt.Sprintf %q to match the formatting used in the error message
		quoted := fmt.Sprintf("%q", name)
		if !strings.Contains(err.Error(), quoted) {
			t.Errorf("error should contain %s, got: %s", quoted, err.Error())
		}
	})
}

// Feature: agent-capability-declaration, Property 2 supplement: valid names pass validation
// **Validates: Requirements 1.5**
func TestProperty_ValidAgentTypePassesValidation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// 随机选择合法名称的子集
		includeWorker := rapid.Bool().Draw(t, "includeWorker")
		includeExplorer := rapid.Bool().Draw(t, "includeExplorer")
		includeWorkerProfile := rapid.Bool().Draw(t, "includeWorkerProfile")

		cfg := DefaultConfig()
		cfg.AgentDeclarations = make(map[string]AgentDeclaration)
		if includeWorker {
			cfg.AgentDeclarations["worker"] = AgentDeclaration{}
		}
		if includeExplorer {
			cfg.AgentDeclarations["explorer"] = AgentDeclaration{}
		}
		if includeWorkerProfile {
			profileName := rapid.StringMatching(`[a-z][a-z0-9_]{0,19}`).Draw(t, "profileName")
			cfg.AgentDeclarations["worker/"+profileName] = AgentDeclaration{}
		}

		err := cfg.ValidateAgentDeclarations()
		if err != nil {
			t.Errorf("unexpected error for valid names: %v", err)
		}
	})
}

// drawAgentDeclaration 使用 rapid 生成随机 AgentDeclaration。
// Capabilities 和 Description 随机为 nil / 空 / 有值。
func drawAgentDeclaration(t *rapid.T) AgentDeclaration {
	var decl AgentDeclaration

	// Capabilities: nil / empty / non-empty
	capsChoice := rapid.IntRange(0, 2).Draw(t, "capsChoice")
	switch capsChoice {
	case 0:
		// nil - 未配置
		decl.Capabilities = nil
	case 1:
		// 显式空
		empty := []string{}
		decl.Capabilities = &empty
	case 2:
		// 有值
		n := rapid.IntRange(1, 5).Draw(t, "capsLen")
		caps := make([]string, n)
		for i := 0; i < n; i++ {
			caps[i] = rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, fmt.Sprintf("cap_%d", i))
		}
		decl.Capabilities = &caps
	}

	// Description: nil / empty / non-empty
	descChoice := rapid.IntRange(0, 2).Draw(t, "descChoice")
	switch descChoice {
	case 0:
		// nil - 未配置
		decl.Description = nil
	case 1:
		// 显式空
		empty := ""
		decl.Description = &empty
	case 2:
		// 有值
		desc := rapid.StringMatching(`[a-zA-Z0-9 ]{1,50}`).Draw(t, "desc")
		decl.Description = &desc
	}

	return decl
}

// === ValidateWorkers 单元测试 ===

func TestValidateWorkers_NilWorkers_ReturnsNil(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workers = nil
	if err := cfg.ValidateWorkers(); err != nil {
		t.Errorf("expected nil for nil workers, got: %v", err)
	}
}

func TestValidateWorkers_EmptyWorkers_ReturnsNil(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workers = []WorkerDeclaration{}
	if err := cfg.ValidateWorkers(); err != nil {
		t.Errorf("expected nil for empty workers, got: %v", err)
	}
}

func TestValidateWorkers_EmptyID_ReturnsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ToolProfiles = map[string][]string{"p1": {"read_file"}}
	cfg.Workers = []WorkerDeclaration{
		{ID: "ok-worker", Profile: "p1"},
		{ID: "", Profile: "p1"},
	}
	err := cfg.ValidateWorkers()
	if err == nil {
		t.Fatal("expected error for empty ID")
	}
	if !strings.Contains(err.Error(), "workers[1]") {
		t.Errorf("error should contain index 1, got: %s", err.Error())
	}
}

func TestValidateWorkers_DuplicateID_ReturnsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ToolProfiles = map[string][]string{"p1": {"read_file"}}
	cfg.Workers = []WorkerDeclaration{
		{ID: "dup", Profile: "p1"},
		{ID: "dup", Profile: "p1"},
	}
	err := cfg.ValidateWorkers()
	if err == nil {
		t.Fatal("expected error for duplicate ID")
	}
	if !strings.Contains(err.Error(), "dup") {
		t.Errorf("error should contain duplicate ID 'dup', got: %s", err.Error())
	}
}

func TestValidateWorkers_UnknownProfile_ReturnsError(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ToolProfiles = map[string][]string{"known": {"read_file"}}
	cfg.Workers = []WorkerDeclaration{
		{ID: "w1", Profile: "unknown_profile"},
	}
	err := cfg.ValidateWorkers()
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
	if !strings.Contains(err.Error(), "unknown_profile") {
		t.Errorf("error should contain profile name, got: %s", err.Error())
	}
}

func TestValidateWorkers_EmptyProfile_OK(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workers = []WorkerDeclaration{
		{ID: "w1", Profile: ""},
	}
	if err := cfg.ValidateWorkers(); err != nil {
		t.Errorf("empty profile should be valid, got: %v", err)
	}
}

func TestValidateWorkers_ValidConfig_ReturnsNil(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ToolProfiles = map[string][]string{
		"standard": {"read_file", "write_file"},
		"readonly": {"read_file"},
	}
	cfg.Workers = []WorkerDeclaration{
		{ID: "w1", Profile: "standard"},
		{ID: "w2", Profile: "readonly"},
		{ID: "w3", Profile: ""},
	}
	if err := cfg.ValidateWorkers(); err != nil {
		t.Errorf("expected nil for valid config, got: %v", err)
	}
}

// === Session Config 字段测试 ===

func TestDefaultConfig_SessionFields(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.SessionRetentionDays != 30 {
		t.Errorf("SessionRetentionDays = %d, want 30", cfg.SessionRetentionDays)
	}
	if cfg.SessionArchiveMax != 50 {
		t.Errorf("SessionArchiveMax = %d, want 50", cfg.SessionArchiveMax)
	}
}

func TestLoadConfig_SessionFields_YAMLOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte("session_retention_days: 7\nsession_archive_max: 100\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionRetentionDays != 7 {
		t.Errorf("SessionRetentionDays = %d, want 7", cfg.SessionRetentionDays)
	}
	if cfg.SessionArchiveMax != 100 {
		t.Errorf("SessionArchiveMax = %d, want 100", cfg.SessionArchiveMax)
	}
	// unspecified fields keep defaults
	if cfg.DefaultConcurrency != 2 {
		t.Errorf("DefaultConcurrency = %d, want default 2", cfg.DefaultConcurrency)
	}
}

func TestLoadConfig_SessionFields_PartialYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.yaml")
	content := []byte("session_retention_days: 14\n")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionRetentionDays != 14 {
		t.Errorf("SessionRetentionDays = %d, want 14", cfg.SessionRetentionDays)
	}
	// unspecified session field keeps default
	if cfg.SessionArchiveMax != 50 {
		t.Errorf("SessionArchiveMax = %d, want default 50", cfg.SessionArchiveMax)
	}
}

func TestLoadConfig_SessionFields_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "setting.json")
	content := []byte(`{"session_retention_days": 60, "session_archive_max": 25}`)
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SessionRetentionDays != 60 {
		t.Errorf("SessionRetentionDays = %d, want 60", cfg.SessionRetentionDays)
	}
	if cfg.SessionArchiveMax != 25 {
		t.Errorf("SessionArchiveMax = %d, want 25", cfg.SessionArchiveMax)
	}
}

// === ValidateWorkers 属性测试（Property-Based Tests）===

// Feature: per-worker-tool-profiles, Property: empty ID must always error
// **Validates: Requirements 1.4, 1.5, 1.6**
//
// 使用 rapid 生成随机 workers 列表，注入至少一条空 ID 的记录，
// 验证 ValidateWorkers() 返回非 nil error。
func TestProperty_ValidateWorkers_EmptyID(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Build a set of known profiles so non-empty profiles are valid
		profileCount := rapid.IntRange(1, 3).Draw(t, "profileCount")
		profiles := make(map[string][]string, profileCount)
		profileNames := make([]string, 0, profileCount)
		for i := range profileCount {
			name := fmt.Sprintf("profile_%d", i)
			profiles[name] = []string{"read_file"}
			profileNames = append(profileNames, name)
		}

		// Generate some valid workers before the injection point
		validCount := rapid.IntRange(0, 5).Draw(t, "validCount")
		workers := make([]WorkerDeclaration, 0, validCount+1)
		usedIDs := make(map[string]struct{})
		for i := range validCount {
			id := fmt.Sprintf("worker-%d", i)
			usedIDs[id] = struct{}{}
			prof := rapid.SampledFrom(append(profileNames, "")).Draw(t, fmt.Sprintf("profile_%d", i))
			workers = append(workers, WorkerDeclaration{ID: id, Profile: prof})
		}

		// Inject an empty-ID worker at a random position
		insertIdx := rapid.IntRange(0, len(workers)).Draw(t, "insertIdx")
		emptyProf := rapid.SampledFrom(append(profileNames, "")).Draw(t, "emptyProfile")
		emptyWorker := WorkerDeclaration{ID: "", Profile: emptyProf}
		workers = append(workers[:insertIdx], append([]WorkerDeclaration{emptyWorker}, workers[insertIdx:]...)...)

		cfg := DefaultConfig()
		cfg.ToolProfiles = profiles
		cfg.Workers = workers

		err := cfg.ValidateWorkers()
		if err == nil {
			t.Fatal("expected error for empty ID, got nil")
		}
		// Error message should reference the index of the empty-ID worker
		expectedIdx := fmt.Sprintf("workers[%d]", insertIdx)
		if !strings.Contains(err.Error(), expectedIdx) {
			t.Errorf("error should contain %q, got: %s", expectedIdx, err.Error())
		}
	})
}

// Feature: per-worker-tool-profiles, Property: duplicate ID must always error
// **Validates: Requirements 1.4, 1.5, 1.6**
//
// 使用 rapid 生成随机 workers 列表，注入至少一对重复 ID 的记录，
// 验证 ValidateWorkers() 返回非 nil error 且错误信息包含重复的 ID 值。
func TestProperty_ValidateWorkers_DuplicateID(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Build a set of known profiles
		profileCount := rapid.IntRange(1, 3).Draw(t, "profileCount")
		profiles := make(map[string][]string, profileCount)
		profileNames := make([]string, 0, profileCount)
		for i := range profileCount {
			name := fmt.Sprintf("profile_%d", i)
			profiles[name] = []string{"read_file"}
			profileNames = append(profileNames, name)
		}
		allProfiles := append(profileNames, "")

		// Generate a non-empty duplicate ID
		dupID := rapid.StringMatching(`[a-z][a-z0-9_-]{0,19}`).Draw(t, "dupID")

		// Generate some unique workers (none using dupID)
		prefixCount := rapid.IntRange(0, 3).Draw(t, "prefixCount")
		workers := make([]WorkerDeclaration, 0, prefixCount+2)
		for i := range prefixCount {
			id := fmt.Sprintf("other-%d", i)
			prof := rapid.SampledFrom(allProfiles).Draw(t, fmt.Sprintf("prefixProf_%d", i))
			workers = append(workers, WorkerDeclaration{ID: id, Profile: prof})
		}

		// Insert first occurrence of dupID
		prof1 := rapid.SampledFrom(allProfiles).Draw(t, "dupProf1")
		workers = append(workers, WorkerDeclaration{ID: dupID, Profile: prof1})

		// Optionally add more unique workers between the duplicates
		midCount := rapid.IntRange(0, 2).Draw(t, "midCount")
		for i := range midCount {
			id := fmt.Sprintf("mid-%d", i)
			prof := rapid.SampledFrom(allProfiles).Draw(t, fmt.Sprintf("midProf_%d", i))
			workers = append(workers, WorkerDeclaration{ID: id, Profile: prof})
		}

		// Insert second occurrence of dupID (the duplicate)
		prof2 := rapid.SampledFrom(allProfiles).Draw(t, "dupProf2")
		workers = append(workers, WorkerDeclaration{ID: dupID, Profile: prof2})

		cfg := DefaultConfig()
		cfg.ToolProfiles = profiles
		cfg.Workers = workers

		err := cfg.ValidateWorkers()
		if err == nil {
			t.Fatalf("expected error for duplicate ID %q, got nil", dupID)
		}
		if !strings.Contains(err.Error(), dupID) {
			t.Errorf("error should contain duplicate ID %q, got: %s", dupID, err.Error())
		}
	})
}

// Feature: per-worker-tool-profiles, Property: valid workers config always passes validation
// **Validates: Requirements 1.4, 1.5, 1.6, 1.7**
//
// 使用 rapid 生成合法的 workers 列表（唯一非空 ID、profile 引用已定义的 tool_profiles key 或空字符串），
// 验证 ValidateWorkers() 返回 nil。
func TestProperty_ValidateWorkers_ValidConfig(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Build a set of known profiles (1-5 profiles)
		profileCount := rapid.IntRange(1, 5).Draw(t, "profileCount")
		profiles := make(map[string][]string, profileCount)
		profileNames := make([]string, 0, profileCount)
		for i := range profileCount {
			name := fmt.Sprintf("profile_%d", i)
			toolCount := rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("toolCount_%d", i))
			tools := make([]string, toolCount)
			for j := range toolCount {
				tools[j] = fmt.Sprintf("tool_%d_%d", i, j)
			}
			profiles[name] = tools
			profileNames = append(profileNames, name)
		}
		// Valid profiles: all defined profile names + empty string (full tools)
		allValidProfiles := append(profileNames, "")

		// Generate a valid workers list (1-10 workers, unique non-empty IDs)
		workerCount := rapid.IntRange(1, 10).Draw(t, "workerCount")
		workers := make([]WorkerDeclaration, workerCount)
		for i := range workerCount {
			// Generate unique non-empty ID using index prefix to guarantee uniqueness
			suffix := rapid.StringMatching(`[a-z][a-z0-9_-]{0,9}`).Draw(t, fmt.Sprintf("idSuffix_%d", i))
			id := fmt.Sprintf("w%d-%s", i, suffix)
			prof := rapid.SampledFrom(allValidProfiles).Draw(t, fmt.Sprintf("prof_%d", i))
			workers[i] = WorkerDeclaration{ID: id, Profile: prof}
		}

		cfg := DefaultConfig()
		cfg.ToolProfiles = profiles
		cfg.Workers = workers

		err := cfg.ValidateWorkers()
		if err != nil {
			t.Fatalf("expected nil for valid config, got: %v", err)
		}
	})
}

// Feature: per-worker-tool-profiles, Property: unknown profile must always error
// **Validates: Requirements 1.4, 1.5, 1.6**
//
// 使用 rapid 生成随机 workers 列表，注入至少一条引用不存在 profile 的记录，
// 验证 ValidateWorkers() 返回非 nil error 且错误信息包含该 profile 名称。
func TestProperty_ValidateWorkers_UnknownProfile(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Build a set of known profiles
		profileCount := rapid.IntRange(1, 3).Draw(t, "profileCount")
		profiles := make(map[string][]string, profileCount)
		profileNames := make([]string, 0, profileCount)
		for i := range profileCount {
			name := fmt.Sprintf("profile_%d", i)
			profiles[name] = []string{"read_file"}
			profileNames = append(profileNames, name)
		}
		allProfiles := append(profileNames, "")

		// Generate an unknown profile name that is NOT in the known set and NOT empty
		unknownProfile := rapid.StringMatching(`unknown_[a-z]{1,10}`).Draw(t, "unknownProfile")

		// Generate some valid workers first (unique IDs, valid profiles)
		validCount := rapid.IntRange(0, 3).Draw(t, "validCount")
		workers := make([]WorkerDeclaration, 0, validCount+1)
		for i := range validCount {
			id := fmt.Sprintf("worker-%d", i)
			prof := rapid.SampledFrom(allProfiles).Draw(t, fmt.Sprintf("validProf_%d", i))
			workers = append(workers, WorkerDeclaration{ID: id, Profile: prof})
		}

		// Inject a worker with the unknown profile at a random position
		badWorkerID := fmt.Sprintf("bad-worker-%d", len(workers))
		insertIdx := rapid.IntRange(0, len(workers)).Draw(t, "insertIdx")
		badWorker := WorkerDeclaration{ID: badWorkerID, Profile: unknownProfile}
		workers = append(workers[:insertIdx], append([]WorkerDeclaration{badWorker}, workers[insertIdx:]...)...)

		cfg := DefaultConfig()
		cfg.ToolProfiles = profiles
		cfg.Workers = workers

		err := cfg.ValidateWorkers()
		if err == nil {
			t.Fatalf("expected error for unknown profile %q, got nil", unknownProfile)
		}
		if !strings.Contains(err.Error(), unknownProfile) {
			t.Errorf("error should contain unknown profile name %q, got: %s", unknownProfile, err.Error())
		}
	})
}

// --- ResolvedWorkerDeclaration tests ---

func TestResolvedWorkerDeclaration_DefaultsWhenNoDeclarations(t *testing.T) {
	cfg := DefaultConfig()

	caps, desc := cfg.ResolvedWorkerDeclaration("some_profile")
	expectedCaps := []string{"code_edit", "shell_exec", "web_search", "subtask_publish", "message"}
	if len(caps) != len(expectedCaps) {
		t.Fatalf("caps len = %d, want %d", len(caps), len(expectedCaps))
	}
	for i, c := range caps {
		if c != expectedCaps[i] {
			t.Errorf("caps[%d] = %q, want %q", i, c, expectedCaps[i])
		}
	}
	if desc == "" {
		t.Error("default description should not be empty")
	}
}

func TestResolvedWorkerDeclaration_EmptyProfile_SkipsPerProfile(t *testing.T) {
	customCaps := []string{"custom_cap"}
	customDesc := "custom worker"
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker/": {
			Capabilities: &customCaps,
			Description:  &customDesc,
		},
	}

	// Empty profile should NOT match "worker/" — it should fall through to "worker" → defaults
	caps, desc := cfg.ResolvedWorkerDeclaration("")
	expectedCaps := defaultCapabilities["worker"]
	if len(caps) != len(expectedCaps) {
		t.Fatalf("caps len = %d, want %d", len(caps), len(expectedCaps))
	}
	for i, c := range caps {
		if c != expectedCaps[i] {
			t.Errorf("caps[%d] = %q, want %q", i, c, expectedCaps[i])
		}
	}
	if desc != defaultDescriptions["worker"] {
		t.Errorf("desc = %q, want default", desc)
	}
}

func TestResolvedWorkerDeclaration_PerProfileHit(t *testing.T) {
	profileCaps := []string{"codebase_read", "web_search"}
	profileDesc := "只读执行代理"
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker/worker_readonly": {
			Capabilities: &profileCaps,
			Description:  &profileDesc,
		},
	}

	caps, desc := cfg.ResolvedWorkerDeclaration("worker_readonly")
	if len(caps) != 2 || caps[0] != "codebase_read" || caps[1] != "web_search" {
		t.Errorf("caps = %v, want [codebase_read web_search]", caps)
	}
	if desc != "只读执行代理" {
		t.Errorf("desc = %q, want 只读执行代理", desc)
	}
}

func TestResolvedWorkerDeclaration_FallbackToWorker(t *testing.T) {
	workerCaps := []string{"shell_exec", "message"}
	workerDesc := "fallback worker"
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker": {
			Capabilities: &workerCaps,
			Description:  &workerDesc,
		},
	}

	// No "worker/some_profile" entry → falls back to "worker"
	caps, desc := cfg.ResolvedWorkerDeclaration("some_profile")
	if len(caps) != 2 || caps[0] != "shell_exec" || caps[1] != "message" {
		t.Errorf("caps = %v, want [shell_exec message]", caps)
	}
	if desc != "fallback worker" {
		t.Errorf("desc = %q, want fallback worker", desc)
	}
}

func TestResolvedWorkerDeclaration_PerProfileOverridesWorker(t *testing.T) {
	workerCaps := []string{"shell_exec"}
	workerDesc := "generic worker"
	profileCaps := []string{"codebase_read"}
	profileDesc := "readonly worker"
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker": {
			Capabilities: &workerCaps,
			Description:  &workerDesc,
		},
		"worker/readonly": {
			Capabilities: &profileCaps,
			Description:  &profileDesc,
		},
	}

	// "worker/readonly" should take priority over "worker"
	caps, desc := cfg.ResolvedWorkerDeclaration("readonly")
	if len(caps) != 1 || caps[0] != "codebase_read" {
		t.Errorf("caps = %v, want [codebase_read]", caps)
	}
	if desc != "readonly worker" {
		t.Errorf("desc = %q, want readonly worker", desc)
	}
}

func TestResolvedWorkerDeclaration_PartialOverride_NilFieldsFallback(t *testing.T) {
	profileCaps := []string{"custom_cap"}
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker/partial": {
			Capabilities: &profileCaps,
			// Description nil → falls back to built-in default
		},
	}

	caps, desc := cfg.ResolvedWorkerDeclaration("partial")
	if len(caps) != 1 || caps[0] != "custom_cap" {
		t.Errorf("caps = %v, want [custom_cap]", caps)
	}
	if desc != defaultDescriptions["worker"] {
		t.Errorf("desc should be built-in default, got %q", desc)
	}
}

func TestResolvedWorkerDeclaration_ExplicitEmpty(t *testing.T) {
	emptyCaps := []string{}
	emptyDesc := ""
	cfg := DefaultConfig()
	cfg.AgentDeclarations = map[string]AgentDeclaration{
		"worker/empty": {
			Capabilities: &emptyCaps,
			Description:  &emptyDesc,
		},
	}

	caps, desc := cfg.ResolvedWorkerDeclaration("empty")
	if caps == nil || len(caps) != 0 {
		t.Errorf("explicit empty caps should be non-nil empty slice, got %v", caps)
	}
	if desc != "" {
		t.Errorf("explicit empty desc should be empty string, got %q", desc)
	}
}

// === ResolvedWorkerDeclaration 属性测试（Property-Based Tests）===

// Feature: per-worker-tool-profiles, Property: three-level fallback logic
// **Validates: Requirements 4.1, 4.2, 4.3**
//
// 使用 rapid 生成随机 profile 名称和 agent_declarations 配置，
// 验证 ResolvedWorkerDeclaration 的三级回退逻辑：
//  1. agent_declarations["worker/<profile>"] 存在时使用该条目
//  2. 不存在时回退到 agent_declarations["worker"]
//  3. 两者都不存在时回退到内置默认值
func TestProperty_ResolvedWorkerDeclaration_ThreeLevelFallback(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Generate a random non-empty profile name
		profile := rapid.StringMatching(`[a-z][a-z0-9_]{0,14}`).Draw(t, "profile")

		// Decide which levels of declarations to include (3 independent booleans)
		hasPerProfile := rapid.Bool().Draw(t, "hasPerProfile")
		hasWorker := rapid.Bool().Draw(t, "hasWorker")

		// Generate random declarations for each level
		perProfileDecl := drawAgentDeclaration(t)
		workerDecl := drawAgentDeclaration(t)

		cfg := DefaultConfig()
		cfg.AgentDeclarations = make(map[string]AgentDeclaration)

		if hasPerProfile {
			cfg.AgentDeclarations["worker/"+profile] = perProfileDecl
		}
		if hasWorker {
			cfg.AgentDeclarations["worker"] = workerDecl
		}

		caps, desc := cfg.ResolvedWorkerDeclaration(profile)

		defCaps := defaultCapabilities["worker"]
		defDesc := defaultDescriptions["worker"]

		if hasPerProfile {
			// Level 1: per-profile entry exists → use it
			expectedCaps := defCaps
			if perProfileDecl.Capabilities != nil {
				expectedCaps = *perProfileDecl.Capabilities
			}
			expectedDesc := defDesc
			if perProfileDecl.Description != nil {
				expectedDesc = *perProfileDecl.Description
			}
			assertCapsEqual(t, caps, expectedCaps, "per-profile hit")
			if desc != expectedDesc {
				t.Errorf("per-profile hit: desc = %q, want %q", desc, expectedDesc)
			}
		} else if hasWorker {
			// Level 2: no per-profile → fall back to "worker"
			expectedCaps := defCaps
			if workerDecl.Capabilities != nil {
				expectedCaps = *workerDecl.Capabilities
			}
			expectedDesc := defDesc
			if workerDecl.Description != nil {
				expectedDesc = *workerDecl.Description
			}
			assertCapsEqual(t, caps, expectedCaps, "worker fallback")
			if desc != expectedDesc {
				t.Errorf("worker fallback: desc = %q, want %q", desc, expectedDesc)
			}
		} else {
			// Level 3: neither exists → built-in defaults
			assertCapsEqual(t, caps, defCaps, "built-in defaults")
			if desc != defDesc {
				t.Errorf("built-in defaults: desc = %q, want %q", desc, defDesc)
			}
		}
	})
}

// assertCapsEqual is a test helper that compares two capability slices.
func assertCapsEqual(t *rapid.T, got, want []string, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: caps len = %d, want %d (got %v, want %v)", context, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: caps[%d] = %q, want %q", context, i, got[i], want[i])
		}
	}
}

// === 配置序列化 Round-Trip 属性测试 ===

// drawValidConfig generates a valid Config with random workers, tool_profiles,
// and agent_declarations suitable for round-trip serialization tests.
func drawValidConfig(t *rapid.T) *Config {
	cfg := DefaultConfig()

	// Generate 1-4 tool profiles with random tool lists
	profileCount := rapid.IntRange(1, 4).Draw(t, "profileCount")
	cfg.ToolProfiles = make(map[string][]string, profileCount)
	profileNames := make([]string, 0, profileCount)
	for i := range profileCount {
		name := fmt.Sprintf("prof_%d", i)
		toolCount := rapid.IntRange(1, 3).Draw(t, fmt.Sprintf("toolCount_%d", i))
		tools := make([]string, toolCount)
		for j := range toolCount {
			tools[j] = rapid.StringMatching(`[a-z][a-z0-9_]{1,12}`).Draw(t, fmt.Sprintf("tool_%d_%d", i, j))
		}
		cfg.ToolProfiles[name] = tools
		profileNames = append(profileNames, name)
	}

	// Valid profile choices: defined names + empty string (full tools)
	allValidProfiles := append(append([]string{}, profileNames...), "")

	// Generate 1-5 workers with unique non-empty IDs and valid profiles
	workerCount := rapid.IntRange(1, 5).Draw(t, "workerCount")
	cfg.Workers = make([]WorkerDeclaration, workerCount)
	for i := range workerCount {
		suffix := rapid.StringMatching(`[a-z][a-z0-9]{0,7}`).Draw(t, fmt.Sprintf("wSuffix_%d", i))
		cfg.Workers[i] = WorkerDeclaration{
			ID:      fmt.Sprintf("w%d-%s", i, suffix),
			Profile: rapid.SampledFrom(allValidProfiles).Draw(t, fmt.Sprintf("wProf_%d", i)),
		}
	}

	// Optionally generate agent_declarations with valid keys
	if rapid.Bool().Draw(t, "hasAgentDecl") {
		cfg.AgentDeclarations = make(map[string]AgentDeclaration)
		if rapid.Bool().Draw(t, "hasWorkerDecl") {
			cfg.AgentDeclarations["worker"] = drawAgentDeclaration(t)
		}
		if rapid.Bool().Draw(t, "hasExplorerDecl") {
			cfg.AgentDeclarations["explorer"] = drawAgentDeclaration(t)
		}
		// Optionally add a worker/<profile> declaration
		if len(profileNames) > 0 && rapid.Bool().Draw(t, "hasPerProfileDecl") {
			prof := rapid.SampledFrom(profileNames).Draw(t, "perProfileKey")
			cfg.AgentDeclarations["worker/"+prof] = drawAgentDeclaration(t)
		}
	}

	return cfg
}

// workersEqual checks semantic equivalence of two WorkerDeclaration slices.
func workersEqual(a, b []WorkerDeclaration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Profile != b[i].Profile {
			return false
		}
	}
	return true
}

// toolProfilesEqual checks semantic equivalence of two tool profile maps.
func toolProfilesEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
	}
	return true
}

// Feature: per-worker-tool-profiles, Property: YAML round-trip
// **Validates: Requirements 8.1**
//
// 使用 rapid 生成合法的 Config（包含随机 workers 列表、tool_profiles、agent_declarations），
// 序列化为 YAML → 写入临时文件 → LoadConfig 读回 → 比较 Workers 列表语义等价。
func TestProperty_ConfigRoundTrip_YAML(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := drawValidConfig(t)

		// Serialize to YAML
		data, err := yaml.Marshal(original)
		if err != nil {
			t.Fatalf("yaml.Marshal failed: %v", err)
		}

		// Write to temp file with .yaml extension
		dir, dirErr := os.MkdirTemp("", "yaml-roundtrip-*")
		if dirErr != nil {
			t.Fatalf("MkdirTemp failed: %v", dirErr)
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "roundtrip.yaml")
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		// LoadConfig reads back
		loaded, err := LoadConfig(path, true)
		if err != nil {
			t.Fatalf("LoadConfig failed: %v\nYAML content:\n%s", err, string(data))
		}

		// Compare Workers list semantic equivalence
		if !workersEqual(original.Workers, loaded.Workers) {
			t.Errorf("Workers mismatch after YAML round-trip:\noriginal: %+v\nloaded:   %+v", original.Workers, loaded.Workers)
		}

		// Compare ToolProfiles semantic equivalence
		if !toolProfilesEqual(original.ToolProfiles, loaded.ToolProfiles) {
			t.Errorf("ToolProfiles mismatch after YAML round-trip:\noriginal: %+v\nloaded:   %+v", original.ToolProfiles, loaded.ToolProfiles)
		}
	})
}

// Feature: per-worker-tool-profiles, Property: JSON round-trip
// **Validates: Requirements 8.2**
//
// 使用 rapid 生成合法的 Config（包含随机 workers 列表），
// 序列化为 JSON → 写入临时文件 → LoadConfig 读回 → 比较 Workers 列表语义等价。
func TestProperty_ConfigRoundTrip_JSON(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		original := drawValidConfig(t)

		// Serialize to JSON
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}

		// Write to temp file with .json extension
		dir, dirErr := os.MkdirTemp("", "json-roundtrip-*")
		if dirErr != nil {
			t.Fatalf("MkdirTemp failed: %v", dirErr)
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "roundtrip.json")
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		// LoadConfig reads back
		loaded, err := LoadConfig(path, true)
		if err != nil {
			t.Fatalf("LoadConfig failed: %v\nJSON content:\n%s", err, string(data))
		}

		// Compare Workers list semantic equivalence
		if !workersEqual(original.Workers, loaded.Workers) {
			t.Errorf("Workers mismatch after JSON round-trip:\noriginal: %+v\nloaded:   %+v", original.Workers, loaded.Workers)
		}

		// Compare ToolProfiles semantic equivalence
		if !toolProfilesEqual(original.ToolProfiles, loaded.ToolProfiles) {
			t.Errorf("ToolProfiles mismatch after JSON round-trip:\noriginal: %+v\nloaded:   %+v", original.ToolProfiles, loaded.ToolProfiles)
		}
	})
}
