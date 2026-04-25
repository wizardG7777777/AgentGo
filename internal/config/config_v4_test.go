package config

import (
	"os"
	"path/filepath"
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

// TestExpandEnv_EmptyKeyOnUnset 验证 os.ExpandEnv 在 env 未设时把 ${VAR}
// 替换为空串——这是 §11.3 文档中的预期行为，烟测中无 KEY 仍能跑通。
func TestExpandEnv_EmptyKeyOnUnset(t *testing.T) {
	t.Setenv("TEST_AGENTGO_NEVER_SET_KEY", "")
	expanded := os.ExpandEnv("api_key: ${TEST_AGENTGO_NEVER_SET_KEY}\n")
	if expanded != "api_key: \n" {
		t.Errorf("os.ExpandEnv 未按预期替换为空串: got %q", expanded)
	}
}
