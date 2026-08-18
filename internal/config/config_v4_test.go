package config

import (
	"os"
	"path/filepath"
	"strconv"
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

func TestWatchdogPendingGraceDefaultsAndYAMLDecode(t *testing.T) {
	defaults := DefaultConfig()
	if defaults.ProjectRoot != "." {
		t.Fatalf("default project_root = %q, want .", defaults.ProjectRoot)
	}
	if got := defaults.Infra.Watchdog.PendingAlertGraceSec; got != 300 {
		t.Fatalf("default pending_alert_grace_sec = %d, want 300", got)
	}
	if got := defaults.Infra.Watchdog.UnroutableGraceSec; got != 300 {
		t.Fatalf("default unroutable_grace_sec = %d, want 300", got)
	}

	path := filepath.Join(t.TempDir(), "watchdog.yaml")
	data := []byte("infra:\n  watchdog:\n    pending_alert_grace_sec: 17\n    unroutable_grace_sec: 29\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Infra.Watchdog.PendingAlertGraceSec; got != 17 {
		t.Fatalf("decoded pending_alert_grace_sec = %d, want 17", got)
	}
	if got := cfg.Infra.Watchdog.UnroutableGraceSec; got != 29 {
		t.Fatalf("decoded unroutable_grace_sec = %d, want 29", got)
	}
}

func TestValidateRejectsEmptyToolProfile(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(prompt, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.LLM.DefaultModel = "test-model"
	cfg.ToolProfiles = map[string][]string{"none": {}}
	cfg.Agents = []AgentKind{{
		Kind: "worker", Replicas: 1, Profile: "none", SystemPromptFile: prompt,
		TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1,
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "空 profile") {
		t.Fatalf("empty tool profile should fail closed, got %v", err)
	}
}

func TestValidateReasoningEffortAcceptsOpenAIValues(t *testing.T) {
	for _, effort := range OpenAIReasoningEfforts {
		t.Run(effort, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.LLM.DefaultModel = "gpt-test"
			cfg.LLM.ReasoningEffort = effort
			if err := cfg.Validate(); err != nil {
				t.Fatalf("reasoning_effort=%q should be valid: %v", effort, err)
			}
		})
	}
}

func TestValidateReasoningEffortRejectsUnknownValue(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.DefaultModel = "gpt-test"
	cfg.LLM.ReasoningEffort = "ultra"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "llm.reasoning_effort") {
		t.Fatalf("Validate error = %v, want reasoning_effort validation error", err)
	}
}

func TestLoadConfigDecodesStreamingRequestPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm.yaml")
	data := []byte("llm:\n  default_model: gpt-test\n  reasoning_effort: high\n  stream: true\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.ReasoningEffort != "high" || !cfg.LLM.Stream {
		t.Fatalf("LLM request policy = %+v", cfg.LLM)
	}
}

func TestSessionRecoverySafetyDefaultsAndYAMLDecode(t *testing.T) {
	defaults := DefaultConfig()
	if got := defaults.SessionResumeMaxIdleSec; got != 3600 {
		t.Fatalf("default session_resume_max_idle_sec = %d, want 3600", got)
	}
	if got := defaults.SessionSnapshotIntervalSec; got != 30 {
		t.Fatalf("default session_snapshot_interval_sec = %d, want 30", got)
	}

	path := filepath.Join(t.TempDir(), "session.yaml")
	data := []byte("session_resume_max_idle_sec: 7200\nsession_snapshot_interval_sec: 15\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionResumeMaxIdleSec != 7200 || cfg.SessionSnapshotIntervalSec != 15 {
		t.Fatalf("decoded session safety config = %d/%d", cfg.SessionResumeMaxIdleSec, cfg.SessionSnapshotIntervalSec)
	}
}

func TestValidateSessionRecoverySafetyRejectsNegativeValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LLM.DefaultModel = "test-model"
	cfg.SessionResumeMaxIdleSec = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "session_resume_max_idle_sec") {
		t.Fatalf("negative resume max idle should fail, got %v", err)
	}
	cfg.SessionResumeMaxIdleSec = 1
	cfg.SessionSnapshotIntervalSec = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "session_snapshot_interval_sec") {
		t.Fatalf("negative snapshot interval should fail, got %v", err)
	}
}

func TestValidateSessionRecoverySafetyRejectsDurationOverflow(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("32-bit int cannot express a time.Duration-overflowing second count")
	}
	tooLarge := int64(maxSessionDurationSeconds + 1)
	cfg := DefaultConfig()
	cfg.LLM.DefaultModel = "test-model"
	cfg.SessionResumeMaxIdleSec = int(tooLarge)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "session_resume_max_idle_sec") {
		t.Fatalf("overflowing resume max idle should fail, got %v", err)
	}
	cfg.SessionResumeMaxIdleSec = 1
	cfg.SessionSnapshotIntervalSec = int(tooLarge)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "session_snapshot_interval_sec") {
		t.Fatalf("overflowing snapshot interval should fail, got %v", err)
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
		if tool == "submit_task_result" {
			hasSubmit = true
			break
		}
	}
	if !hasSubmit {
		t.Fatalf("acceptance verifier profile %q cannot submit verdict/event results: %v", verifier.Profile, profile)
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
				TaskMaxRetries:               3,
				EnforceCompactTokenThreshold: 4000,
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
			{Kind: "worker", Replicas: 1, Profile: "p", SystemPromptFile: "x", TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1},
			{Kind: "worker", Replicas: 1, Profile: "p", SystemPromptFile: "x", TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1},
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
			{Kind: "", Replicas: 1, Profile: "p", SystemPromptFile: "x", TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1},
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
				SystemPromptFile: "x", TaskMaxRetries: 1, EnforceCompactTokenThreshold: 1,
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

func TestSchedulerBehaviorDefaultsAndYAMLOverrides(t *testing.T) {
	defaults := DefaultConfig()
	if defaults.Scheduler.AgentMaxLoops != 0 {
		t.Fatalf("default scheduler.agent_max_loops=%d want 0（V6 起不再填充默认值）",
			defaults.Scheduler.AgentMaxLoops)
	}
	if defaults.Scheduler.EnforceCompactTokenThreshold != DefaultSchedulerCompactTokenThreshold {
		t.Fatalf("default scheduler.enforce_compact_token_threshold=%d want %d",
			defaults.Scheduler.EnforceCompactTokenThreshold, DefaultSchedulerCompactTokenThreshold)
	}
	if defaults.Scheduler.ContextLimit != 0 {
		t.Fatalf("default scheduler.context_limit=%d want 0（V6 起不再填充默认值）",
			defaults.Scheduler.ContextLimit)
	}

	path := filepath.Join(t.TempDir(), "scheduler.yaml")
	data := []byte("llm:\n  default_model: test-model\nscheduler:\n  enforce_compact_token_threshold: 160000\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("scheduler behavior overrides should validate: %v", err)
	}
	if cfg.Scheduler.EnforceCompactTokenThreshold != 160000 {
		t.Fatalf("scheduler overrides not decoded: %+v", cfg.Scheduler)
	}

	modelOnlyPath := filepath.Join(t.TempDir(), "scheduler-model-only.yaml")
	if err := os.WriteFile(modelOnlyPath, []byte("llm:\n  default_model: test-model\nscheduler:\n  model: scheduler-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	modelOnly, err := LoadConfig(modelOnlyPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if modelOnly.Scheduler.AgentMaxLoops != 0 ||
		modelOnly.Scheduler.EnforceCompactTokenThreshold != DefaultSchedulerCompactTokenThreshold ||
		modelOnly.Scheduler.ContextLimit != 0 {
		t.Fatalf("model-only scheduler block lost compatibility defaults: %+v", modelOnly.Scheduler)
	}
}

// TestValidate_AgentMaxLoopsMigrationDiagnostic V6 迁移诊断：agent_max_loops
// 已于 V6 移除。显式设置（YAML 或直造）必须报错且文案含「已于 V6 移除」；
// 不设置（零值）则通过——零值是区分「显式设置」与「未设置」的唯一信号。
func TestValidate_AgentMaxLoopsMigrationDiagnostic(t *testing.T) {
	prompt := filepath.ToSlash(filepath.Join(t.TempDir(), "worker.md"))
	if err := os.WriteFile(prompt, []byte("worker"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("scheduler 块显式设置报错", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.LLM.DefaultModel = "test-model"
		cfg.Scheduler.AgentMaxLoops = 50
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "scheduler.agent_max_loops") ||
			!strings.Contains(err.Error(), "已于 V6 移除") {
			t.Fatalf("scheduler.agent_max_loops=50 应报 V6 迁移诊断，got %v", err)
		}
	})

	t.Run("agents 块显式设置报错", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.LLM.DefaultModel = "test-model"
		cfg.Agents = []AgentKind{{
			Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
			SystemPromptFile: prompt, AgentMaxLoops: 10, TaskMaxRetries: 1,
			EnforceCompactTokenThreshold: 1,
		}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "agent_max_loops") ||
			!strings.Contains(err.Error(), "已于 V6 移除") {
			t.Fatalf("agents[0].agent_max_loops=10 应报 V6 迁移诊断，got %v", err)
		}
	})

	t.Run("YAML 显式设置报错", func(t *testing.T) {
		yamlPath := filepath.Join(t.TempDir(), "legacy.yaml")
		data := []byte("llm:\n  default_model: test-model\nscheduler:\n  agent_max_loops: 47\n")
		if err := os.WriteFile(yamlPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(yamlPath, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "已于 V6 移除") {
			t.Fatalf("YAML 显式 agent_max_loops 应报 V6 迁移诊断，got %v", err)
		}
	})

	t.Run("不设置则通过", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.LLM.DefaultModel = "test-model"
		cfg.Agents = []AgentKind{{
			Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
			SystemPromptFile: prompt, TaskMaxRetries: 1,
			EnforceCompactTokenThreshold: 1,
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("未设置 agent_max_loops 应通过校验，got %v", err)
		}
	})
}

// TestValidate_ContextLimitMigrationDiagnostic V6 迁移诊断：context_limit
// 已于 V6 移除（固定上下文硬限截断层与 history_truncated 事件一并删除）。
// 显式设置（YAML 或直造）必须报错且文案含「已于 V6 移除」；
// 不设置（零值）则通过——零值是区分「显式设置」与「未设置」的唯一信号。
func TestValidate_ContextLimitMigrationDiagnostic(t *testing.T) {
	prompt := filepath.ToSlash(filepath.Join(t.TempDir(), "worker.md"))
	if err := os.WriteFile(prompt, []byte("worker"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("scheduler 块显式设置报错", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.LLM.DefaultModel = "test-model"
		cfg.Scheduler.ContextLimit = 240000
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "scheduler.context_limit") ||
			!strings.Contains(err.Error(), "已于 V6 移除") {
			t.Fatalf("scheduler.context_limit=240000 应报 V6 迁移诊断，got %v", err)
		}
	})

	t.Run("agents 块显式设置报错", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.LLM.DefaultModel = "test-model"
		cfg.Agents = []AgentKind{{
			Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
			SystemPromptFile: prompt, ContextLimit: 16000, TaskMaxRetries: 1,
			EnforceCompactTokenThreshold: 1,
		}}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "context_limit") ||
			!strings.Contains(err.Error(), "已于 V6 移除") {
			t.Fatalf("agents[0].context_limit=16000 应报 V6 迁移诊断，got %v", err)
		}
	})

	t.Run("YAML 显式设置报错", func(t *testing.T) {
		yamlPath := filepath.Join(t.TempDir(), "legacy.yaml")
		data := []byte("llm:\n  default_model: test-model\nscheduler:\n  context_limit: 240000\n")
		if err := os.WriteFile(yamlPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(yamlPath, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "已于 V6 移除") {
			t.Fatalf("YAML 显式 context_limit 应报 V6 迁移诊断，got %v", err)
		}
	})

	t.Run("不设置则通过", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.LLM.DefaultModel = "test-model"
		cfg.Agents = []AgentKind{{
			Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
			SystemPromptFile: prompt, TaskMaxRetries: 1,
			EnforceCompactTokenThreshold: 1,
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("未设置 context_limit 应通过校验，got %v", err)
		}
	})
}

func TestValidate_SchedulerBehaviorRejectsNegativeValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*SchedulerKind)
		field  string
	}{
		{"compact threshold", func(s *SchedulerKind) { s.EnforceCompactTokenThreshold = -1 }, "enforce_compact_token_threshold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.LLM.DefaultModel = "test-model"
			tc.mutate(&cfg.Scheduler)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("negative %s should fail, got %v", tc.field, err)
			}
		})
	}
}

func TestValidate_StaticAgentsStillRequireSchedulerModel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agents = []AgentKind{{
		Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
		SystemPromptFile:             filepath.ToSlash(filepath.Join("..", "..", "prompts", "worker.md")),
		TaskMaxRetries:               1,
		EnforceCompactTokenThreshold: 1,
	}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "Scheduler 配置缺少模型") {
		t.Fatalf("static Agent model cannot replace the Scheduler/template default model, got %v", err)
	}
}

func TestValidate_StaticAgentNeedsOwnOrLLMDefaultModel(t *testing.T) {
	// 规则 9 仅允许 forward slash：Windows 上 filepath.Join 产生反斜杠，
	// 会在到达本测试意图（模型校验）之前先被规则 9 拒绝，故统一 ToSlash。
	prompt := filepath.ToSlash(filepath.Join(t.TempDir(), "worker.md"))
	if err := os.WriteFile(prompt, []byte("worker"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Scheduler.Model = "scheduler-model"
	cfg.Agents = []AgentKind{{
		Kind: "worker", Replicas: 1, Tools: []string{"read_file"},
		SystemPromptFile: prompt, TaskMaxRetries: 1,
		EnforceCompactTokenThreshold: 1,
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
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
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
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
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
    task_max_retries: 3
    enforce_compact_token_threshold: 4000
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
