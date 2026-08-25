package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	DefaultContextWindowTokens int64 = 1_048_576
	DefaultMaxCompletionTokens int64 = 65_536
	ProtocolOverheadTokens     int64 = 16_384
)

// ModelCapabilityConfig 是按精确模型名声明的上下文能力。它只描述模型容量，
// 不按 provider 名称推断 wire 行为。
type ModelCapabilityConfig struct {
	ContextWindowTokens int64 `yaml:"context_window_tokens" json:"context_window_tokens"`
	MaxCompletionTokens int64 `yaml:"max_completion_tokens" json:"max_completion_tokens"`
}

// ResolvedModelCapability 是启动期解析并冻结进 ExecutionLease 的能力事实。
type ResolvedModelCapability struct {
	Model               string
	ContextWindowTokens int64
	MaxCompletionTokens int64
	Digest              string
}

func (c LLMConfig) ResolveModelCapability(model string) (ResolvedModelCapability, error) {
	window := c.DefaultContextWindowTokens
	if window == 0 {
		window = DefaultContextWindowTokens
	}
	completion := c.DefaultMaxCompletionTokens
	if completion == 0 {
		completion = DefaultMaxCompletionTokens
	}
	model = strings.TrimSpace(model)
	if override, ok := c.ModelCapabilities[model]; ok {
		if override.ContextWindowTokens != 0 {
			window = override.ContextWindowTokens
		}
		if override.MaxCompletionTokens != 0 {
			completion = override.MaxCompletionTokens
		}
	}
	if err := validateModelCapability(model, window, completion); err != nil {
		return ResolvedModelCapability{}, err
	}
	payload := fmt.Sprintf("model=%s;window=%d;completion=%d;overhead=%d",
		model, window, completion, ProtocolOverheadTokens)
	sum := sha256.Sum256([]byte(payload))
	return ResolvedModelCapability{
		Model: model, ContextWindowTokens: window, MaxCompletionTokens: completion,
		Digest: hex.EncodeToString(sum[:])[:16],
	}, nil
}

func (c LLMConfig) validateModelCapabilities() error {
	if _, err := c.ResolveModelCapability(strings.TrimSpace(c.DefaultModel)); err != nil {
		return err
	}
	models := make([]string, 0, len(c.ModelCapabilities))
	for model := range c.ModelCapabilities {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("llm.model_capabilities 不得包含空模型名")
		}
		override := c.ModelCapabilities[model]
		base := c
		base.ModelCapabilities = map[string]ModelCapabilityConfig{model: override}
		if _, err := base.ResolveModelCapability(model); err != nil {
			return err
		}
	}
	return nil
}

func validateModelCapability(model string, window, completion int64) error {
	if window <= 0 || completion <= 0 {
		return fmt.Errorf("模型 %q 的 context_window_tokens/max_completion_tokens 必须为正数", model)
	}
	if window <= completion+ProtocolOverheadTokens {
		return fmt.Errorf("模型 %q 的 context_window_tokens=%d 必须大于 completion=%d + protocol reserve=%d",
			model, window, completion, ProtocolOverheadTokens)
	}
	return nil
}
