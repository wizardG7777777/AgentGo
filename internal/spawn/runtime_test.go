package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func defaultBase(t *testing.T) (config.AgentKind, map[string][]string) {
	t.Helper()
	dir := t.TempDir()
	prompt := filepath.Join(dir, "sys.md")
	writeFile(t, prompt, "BASE SYSTEM PROMPT")
	return config.AgentKind{
		Kind:                         "explorer",
		EventType:                    "explore",
		Tools:                        []string{"read_file", "list_dir"},
		SystemPromptFile:             prompt,
		Model:                        "gpt-base",
		TaskMaxRetries:               3,
		EnforceCompactTokenThreshold: 50000,
	}, nil
}

func TestBuildAdhocRuntime_NoOverride_InheritsAll(t *testing.T) {
	base, profiles := defaultBase(t)
	rt, err := buildAdhocRuntime(base, config.LLMConfig{DefaultModel: "gpt-default"},
		profiles, RuntimeOverride{}, "instance-id", "adhoc:abc", 0)
	if err != nil {
		t.Fatalf("buildAdhocRuntime: %v", err)
	}
	if rt.InstanceID != "instance-id" || rt.EventType != "adhoc:abc" {
		t.Errorf("identity wrong: %+v", rt)
	}
	if rt.Kind != "explorer" {
		t.Errorf("Kind=%q want explorer (base kind preserved for trace)", rt.Kind)
	}
	if rt.SystemPrompt != "BASE SYSTEM PROMPT" {
		t.Errorf("SystemPrompt=%q want base content", rt.SystemPrompt)
	}
	if rt.Model != "gpt-base" {
		t.Errorf("Model=%q want gpt-base (base wins, llm default unused)", rt.Model)
	}
	if rt.TaskMaxRetries != 3 || rt.EnforceCompactTokenThreshold != 50000 {
		t.Errorf("base numeric fields not inherited: %+v", rt)
	}
	if len(rt.AllowedTools) != 2 {
		t.Errorf("AllowedTools wrong: %+v", rt.AllowedTools)
	}
}

func TestBuildAdhocRuntime_Override_AppliesNonZero(t *testing.T) {
	base, profiles := defaultBase(t)
	override := RuntimeOverride{
		SystemPrompt:    "OVERRIDE PROMPT",
		SystemPromptSet: true,
		Model:           "gpt-override",
		TaskMaxRetries:  9,
		// 其他字段零值=不覆盖
	}
	rt, err := buildAdhocRuntime(base, config.LLMConfig{}, profiles, override, "id", "adhoc:x", 0)
	if err != nil {
		t.Fatalf("buildAdhocRuntime: %v", err)
	}
	if rt.SystemPrompt != "OVERRIDE PROMPT" {
		t.Errorf("SystemPrompt not overridden: %q", rt.SystemPrompt)
	}
	if rt.Model != "gpt-override" {
		t.Errorf("Model not overridden: %q", rt.Model)
	}
	if rt.TaskMaxRetries != 9 {
		t.Errorf("TaskMaxRetries not overridden: %d", rt.TaskMaxRetries)
	}
	// 未 override 的字段保持 base
	if rt.EnforceCompactTokenThreshold != 50000 {
		t.Errorf("EnforceCompactTokenThreshold should stay base=50000, got %d", rt.EnforceCompactTokenThreshold)
	}
}

func TestBuildAdhocRuntime_ModelFallbackChain(t *testing.T) {
	base, profiles := defaultBase(t)
	base.Model = "" // 清空 base.Model 让回落 llmCfg.DefaultModel 生效
	rt, err := buildAdhocRuntime(base, config.LLMConfig{DefaultModel: "gpt-default"},
		profiles, RuntimeOverride{}, "id", "adhoc:x", 0)
	if err != nil {
		t.Fatalf("buildAdhocRuntime: %v", err)
	}
	if rt.Model != "gpt-default" {
		t.Errorf("Model=%q want gpt-default (override empty, base empty → llm default)", rt.Model)
	}
}

func TestBuildAdhocRuntime_SystemPromptSetButEmpty(t *testing.T) {
	// SystemPromptSet=true 但内容空——也算覆盖（清空 system prompt 的合法用法）
	base, profiles := defaultBase(t)
	rt, err := buildAdhocRuntime(base, config.LLMConfig{}, profiles,
		RuntimeOverride{SystemPrompt: "", SystemPromptSet: true},
		"id", "adhoc:x", 0)
	if err != nil {
		t.Fatalf("buildAdhocRuntime: %v", err)
	}
	if rt.SystemPrompt != "" {
		t.Errorf("explicit empty override should produce empty SystemPrompt, got %q", rt.SystemPrompt)
	}
}

func TestBuildAdhocRuntime_RejectsBaseWithoutToolsOrProfile(t *testing.T) {
	base := config.AgentKind{Kind: "broken", SystemPromptFile: "/dev/null"}
	_, err := buildAdhocRuntime(base, config.LLMConfig{}, nil, RuntimeOverride{}, "id", "adhoc:x", 0)
	if err == nil || !strings.Contains(err.Error(), "无法派生") {
		t.Errorf("expected derivation error, got %v", err)
	}
}

func TestBuildAdhocRuntime_RejectsUnknownProfile(t *testing.T) {
	base := config.AgentKind{Kind: "x", Profile: "ghost", SystemPromptFile: "/dev/null"}
	_, err := buildAdhocRuntime(base, config.LLMConfig{}, map[string][]string{"real": {"read_file"}},
		RuntimeOverride{}, "id", "adhoc:x", 0)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("expected unknown profile error, got %v", err)
	}
}

func TestBuildAdhocRuntime_ProfileExpansion(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "sys.md")
	writeFile(t, prompt, "x")
	base := config.AgentKind{Kind: "x", Profile: "explore", SystemPromptFile: prompt}
	rt, err := buildAdhocRuntime(base, config.LLMConfig{},
		map[string][]string{"explore": {"a", "b", "c"}},
		RuntimeOverride{}, "id", "adhoc:x", 0)
	if err != nil {
		t.Fatalf("buildAdhocRuntime: %v", err)
	}
	if len(rt.AllowedTools) != 3 || rt.AllowedTools[0] != "a" {
		t.Errorf("profile expansion wrong: %v", rt.AllowedTools)
	}
}

// TestBuildAdhocRuntime_IdleThresholdFromGlobalConfig 验证 E3 接线：
// 全局 agent_idle_threshold 经 buildAdhocRuntime 进入 AgentRuntimeConfig，
// 不再被丢弃（此前运行时一律硬编码 0，配置是死旋钮）。
func TestBuildAdhocRuntime_IdleThresholdFromGlobalConfig(t *testing.T) {
	base, profiles := defaultBase(t)
	rt, err := buildAdhocRuntime(base, config.LLMConfig{}, profiles,
		RuntimeOverride{}, "id", "adhoc:x", 3)
	if err != nil {
		t.Fatalf("buildAdhocRuntime: %v", err)
	}
	if rt.IdleThreshold != 3 {
		t.Errorf("IdleThreshold=%d want 3（全局 agent_idle_threshold 应透传）", rt.IdleThreshold)
	}

	// 全局 0（生产推荐默认）也原样透传，保持"永不空闲退出"。
	rt, err = buildAdhocRuntime(base, config.LLMConfig{}, profiles,
		RuntimeOverride{}, "id", "adhoc:x", 0)
	if err != nil {
		t.Fatalf("buildAdhocRuntime: %v", err)
	}
	if rt.IdleThreshold != 0 {
		t.Errorf("IdleThreshold=%d want 0", rt.IdleThreshold)
	}
}
