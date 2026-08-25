package config

import "testing"

func TestResolveModelCapabilityUsesLargeDefaultAndExactOverride(t *testing.T) {
	cfg := LLMConfig{DefaultModel: "large", ModelCapabilities: map[string]ModelCapabilityConfig{
		"small": {ContextWindowTokens: 131_072, MaxCompletionTokens: 16_384},
	}}
	large, err := cfg.ResolveModelCapability("large")
	if err != nil || large.ContextWindowTokens != 1_048_576 || large.MaxCompletionTokens != 65_536 || large.Digest == "" {
		t.Fatalf("默认能力档案错误: %+v err=%v", large, err)
	}
	small, err := cfg.ResolveModelCapability("small")
	if err != nil || small.ContextWindowTokens != 131_072 || small.MaxCompletionTokens != 16_384 || small.Digest == large.Digest {
		t.Fatalf("模型精确覆盖错误: %+v err=%v", small, err)
	}
}

func TestResolveModelCapabilityRejectsImpossibleReserve(t *testing.T) {
	cfg := LLMConfig{DefaultContextWindowTokens: 65_536, DefaultMaxCompletionTokens: 60_000}
	if _, err := cfg.ResolveModelCapability("bad"); err == nil {
		t.Fatal("completion+protocol reserve 超过窗口必须拒绝")
	}
}
