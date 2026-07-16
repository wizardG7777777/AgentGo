package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadConfig_V4Sample 烟测：项目根的 setting.v4.yaml 能被 LoadConfig 解析、
// 且 Validate() 通过。这覆盖 nextUpgrade_v4.md §11.3 / §11.4 / §11.5 三节。
//
// 未设置 env var 时，${DEEPSEEK_API_KEY} 等被 os.ExpandEnv 替换为空串——
// 不影响结构校验通过。
func TestLoadConfig_V4Sample(t *testing.T) {
	// 测试运行时的 cwd 是包目录 internal/config，需要回到仓库根
	repoRoot := filepath.Join("..", "..")
	yamlPath := filepath.Join(repoRoot, "setting.v4.yaml")
	if _, err := os.Stat(yamlPath); err != nil {
		t.Skipf("setting.v4.yaml 不存在: %v", err)
	}

	// LoadConfig 内部用相对路径读 system_prompt_file，需要切换到 repoRoot
	origDir, _ := os.Getwd()
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("Chdir(%q): %v", repoRoot, err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	cfg, err := LoadConfig("setting.v4.yaml", true)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if len(cfg.Agents) == 0 {
		t.Fatal("setting.v4.yaml 应当包含至少一个 agent kind")
	}

	// Validate：验证 12 条规则
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}

	// 抽样断言关键字段被正确读入
	if cfg.LLM.DefaultModel == "" {
		t.Error("llm.default_model 未被读入")
	}
	if cfg.Scheduler.Model == "" {
		t.Error("scheduler.model 未被读入")
	}
	if cfg.Infra.Watchdog.IntervalSec == 0 {
		t.Error("infra.watchdog.interval_sec 未被读入")
	}
	if cfg.StartupProbe == "" {
		t.Error("startup_probe 字段未被读入")
	}
}

func TestLoadConfigExampleIncludesRunnableAcceptanceVerifier(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	origDir, _ := os.Getwd()
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("Chdir(%q): %v", repoRoot, err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	cfg, err := LoadConfig("config.example.yaml", true)
	if err != nil {
		t.Fatalf("LoadConfig(config.example.yaml): %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.example.yaml Validate: %v", err)
	}
	var verifier *AgentKind
	for i := range cfg.Agents {
		if cfg.Agents[i].EventType == "acceptance.verify" {
			verifier = &cfg.Agents[i]
			break
		}
	}
	if verifier == nil {
		t.Fatal("config.example.yaml lacks an acceptance.verify agent")
	}
	profile, err := cfg.ResolveProfile(verifier.Profile)
	if err != nil {
		t.Fatal(err)
	}
	hasSubmit := false
	for _, tool := range profile {
		if tool == "submit_acceptance_result" {
			hasSubmit = true
			break
		}
	}
	if !hasSubmit {
		t.Fatalf("acceptance verifier profile %q cannot submit formal results: %v", verifier.Profile, profile)
	}
}

// TestValidate_RejectsBackslashPath 规则 9：v4 路径字段不允许反斜杠。
func TestValidate_RejectsBackslashPath(t *testing.T) {
	cfg := &Config{
		Agents: []AgentKind{
			{
				Kind:                         "worker",
				Replicas:                     1,
				Profile:                      "any",
				SystemPromptFile:             `prompts\worker.md`, // 反斜杠！
				AgentMaxLoops:                10,
				TaskMaxRetries:               3,
				EnforceCompactTokenThreshold: 4000,
				ContextLimit:                 16000,
			},
		},
		ToolProfiles: map[string][]string{"any": {"read_file"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("应当拒绝反斜杠路径")
	}
}

// TestValidate_RejectsDuplicateKind 规则 3：kind 唯一。
func TestValidate_RejectsDuplicateKind(t *testing.T) {
	cfg := &Config{
		Agents: []AgentKind{
			{Kind: "worker", Replicas: 1, Profile: "p", SystemPromptFile: "x", AgentMaxLoops: 1, TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1, ContextLimit: 1},
			{Kind: "worker", Replicas: 1, Profile: "p", SystemPromptFile: "x", AgentMaxLoops: 1, TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1, ContextLimit: 1},
		},
		ToolProfiles: map[string][]string{"p": {"read_file"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("应当拒绝重复 kind")
	}
}

// TestValidate_RejectsEmptyKind 规则 12：kind 非空。
func TestValidate_RejectsEmptyKind(t *testing.T) {
	cfg := &Config{
		Agents: []AgentKind{
			{Kind: "", Replicas: 1, Profile: "p", SystemPromptFile: "x", AgentMaxLoops: 1, TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1, ContextLimit: 1},
		},
		ToolProfiles: map[string][]string{"p": {"read_file"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("应当拒绝空 kind")
	}
}

// TestValidate_ProfileToolsMutex 规则 5：profile 与 tools 互斥。
func TestValidate_ProfileToolsMutex(t *testing.T) {
	cfg := &Config{
		Agents: []AgentKind{
			{
				Kind: "w", Replicas: 1,
				Profile: "p", Tools: []string{"read_file"}, // 同时给两者
				SystemPromptFile: "x", AgentMaxLoops: 1, TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1, ContextLimit: 1,
			},
		},
		ToolProfiles: map[string][]string{"p": {"read_file"}},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("应当拒绝 profile + tools 同时声明")
	}
}

// TestValidate_StartupProbeInvalid 校验 startup_probe 取值。
func TestValidate_StartupProbeInvalid(t *testing.T) {
	cfg := &Config{StartupProbe: "ping"} // "tcp" / "off" 之外
	if err := cfg.Validate(); err == nil {
		t.Error("应当拒绝 startup_probe=ping")
	}
}

func TestValidate_SchedulerOnlyRequiresAndAcceptsModel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.DefaultModel = "gpt-test"
	cfg.Agents = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("LLM-only Scheduler config should be valid: %v", err)
	}

	cfg.LLM.DefaultModel = ""
	cfg.Scheduler.Model = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "缺少模型") {
		t.Fatalf("empty Scheduler-only config should fail on model, got %v", err)
	}
}

func TestValidate_StaticAgentsStillRequireSchedulerModel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents = []AgentKind{{
		Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
		SystemPromptFile: filepath.Join("..", "..", "prompts", "worker.md"),
		AgentMaxLoops:    1, TaskMaxRetries: 1,
		EnforceCompactTokenThreshold: 1, ContextLimit: 1,
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "Scheduler 配置缺少模型") {
		t.Fatalf("static Agent model cannot replace the Scheduler/template default model, got %v", err)
	}
}

func TestValidate_StaticAgentNeedsOwnOrLLMDefaultModel(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(prompt, []byte("worker"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Scheduler.Model = "scheduler-model"
	cfg.Agents = []AgentKind{{
		Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
		SystemPromptFile: prompt, AgentMaxLoops: 1, TaskMaxRetries: 1,
		EnforceCompactTokenThreshold: 1, ContextLimit: 1,
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "agents[0].model") {
		t.Fatalf("scheduler.model must not silently become the static Agent model, got %v", err)
	}
	cfg.Agents[0].Model = "worker-model"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit per-Agent model should be valid: %v", err)
	}
}

func TestValidate_InvalidStaticAgentDoesNotDowngradeToSchedulerOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.DefaultModel = "gpt-test"
	cfg.Agents = []AgentKind{{Kind: "broken", Replicas: 0}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "replicas") {
		t.Fatalf("invalid non-empty agents must remain a hard error, got %v", err)
	}
}

// TestExpandEnv_EmptyKeyOnUnset 验证 os.ExpandEnv 在 env 未设时把 ${VAR}
// 替换为空串——这是 §11.3 文档中的预期行为，烟测中无 KEY 仍能跑通。
func TestExpandEnv_EmptyKeyOnUnset(t *testing.T) {
	t.Setenv("TEST_AGENTGO_NEVER_SET_KEY", "")
	expanded := os.ExpandEnv("api_key: ${TEST_AGENTGO_NEVER_SET_KEY}\n")
	if expanded != "api_key: \n" {
		t.Errorf("os.ExpandEnv 未按预期替换为空串: got %q", expanded)
	}
}

// === §7 HashlineEnabled 配置测试 ===

func TestLoadConfig_HashlineEnabled_DefaultTrue(t *testing.T) {
	// 不写 hashline_enabled 时，默认值应为 true
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.yaml")
	content := `
llm:
  base_url: http://example.com
  api_key: key
  default_model: m
agents:
  - kind: worker
    replicas: 1
    system_prompt_file: prompts/w.md
    agent_max_loops: 10
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    context_limit: 16000
infra:
  watchdog:
    interval_sec: 30
  mail_notifier:
    enabled: true
    interval_sec: 5
  store:
    event_channel_buffer: 64
    fifo_limit: 100
    default_concurrency: 2
    default_timeout_sec: 300
  roster:
    wait_timeout_sec: 30
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.HashlineEnabled == nil || !*cfg.HashlineEnabled {
		t.Errorf("HashlineEnabled 未设时应默认 true, got %v", cfg.HashlineEnabled)
	}
}

func TestLoadConfig_HashlineEnabled_ExplicitFalse(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.yaml")
	content := `
hashline_enabled: false
llm:
  base_url: http://example.com
  api_key: key
  default_model: m
agents:
  - kind: worker
    replicas: 1
    system_prompt_file: prompts/w.md
    agent_max_loops: 10
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    context_limit: 16000
infra:
  watchdog:
    interval_sec: 30
  mail_notifier:
    enabled: true
    interval_sec: 5
  store:
    event_channel_buffer: 64
    fifo_limit: 100
    default_concurrency: 2
    default_timeout_sec: 300
  roster:
    wait_timeout_sec: 30
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.HashlineEnabled == nil || *cfg.HashlineEnabled {
		t.Errorf("HashlineEnabled 显式 false 时应为 false, got %v", cfg.HashlineEnabled)
	}
}

func TestLoadConfig_HashlineEnabled_ExplicitTrue(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cfg.yaml")
	content := `
hashline_enabled: true
llm:
  base_url: http://example.com
  api_key: key
  default_model: m
agents:
  - kind: worker
    replicas: 1
    system_prompt_file: prompts/w.md
    agent_max_loops: 10
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
    context_limit: 16000
infra:
  watchdog:
    interval_sec: 30
  mail_notifier:
    enabled: true
    interval_sec: 5
  store:
    event_channel_buffer: 64
    fifo_limit: 100
    default_concurrency: 2
    default_timeout_sec: 300
  roster:
    wait_timeout_sec: 30
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.HashlineEnabled == nil || !*cfg.HashlineEnabled {
		t.Errorf("HashlineEnabled 显式 true 时应为 true, got %v", cfg.HashlineEnabled)
	}
}
