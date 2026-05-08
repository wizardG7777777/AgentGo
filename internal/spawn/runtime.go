package spawn

import (
	"fmt"
	"os"

	"agentgo/internal/config"
)

// buildAdhocRuntime 把 BaseKind 派生的 AgentRuntimeConfig 与 Override 合并，
// 并强制 InstanceID / Kind / EventType 为 ad-hoc 路由所需的值。
//
// Profile / AllowedTools 始终来自 base kind——v5 首版不支持 override，
// 因为 RunnerDeps 的工具/Gate 集合在 bootstrap 阶段已固化。
//
// SystemPrompt 优先级：override.SystemPromptSet=true → override 内容；
// 否则读 base.SystemPromptFile（与 buildAgentRuntime 一致）。
func buildAdhocRuntime(
	base config.AgentKind,
	llmCfg config.LLMConfig,
	toolProfiles map[string][]string,
	override RuntimeOverride,
	instanceID string,
	eventType string,
) (config.AgentRuntimeConfig, error) {
	// 工具集解析：与 buildAgentRuntime 同一份逻辑（不做 override）
	var allowed []string
	switch {
	case base.Profile != "":
		toolList, ok := toolProfiles[base.Profile]
		if !ok {
			return config.AgentRuntimeConfig{}, fmt.Errorf(
				"base_kind=%q 引用的 profile %q 不存在于 tool_profiles", base.Kind, base.Profile)
		}
		allowed = toolList
	case len(base.Tools) > 0:
		allowed = base.Tools
	default:
		return config.AgentRuntimeConfig{}, fmt.Errorf(
			"base_kind=%q 既未声明 profile 也未声明 tools——无法派生 ad-hoc agent", base.Kind)
	}

	// SystemPrompt：override 优先，否则读 base 的文件
	var systemPrompt string
	if override.SystemPromptSet {
		systemPrompt = override.SystemPrompt
	} else {
		bytes, err := os.ReadFile(base.SystemPromptFile)
		if err != nil {
			return config.AgentRuntimeConfig{}, fmt.Errorf(
				"base_kind=%q system_prompt_file=%q 读取失败: %w",
				base.Kind, base.SystemPromptFile, err)
		}
		systemPrompt = string(bytes)
	}

	// Model 三级优先：override > base > llmCfg.DefaultModel
	model := override.Model
	if model == "" {
		model = base.Model
	}
	if model == "" {
		model = llmCfg.DefaultModel
	}

	rt := config.AgentRuntimeConfig{
		InstanceID:   instanceID,
		Kind:         base.Kind, // 保留 base kind 名以便 trace/log 归类
		EventType:    eventType,
		AllowedTools: allowed,
		Model:        model,
		SystemPrompt: systemPrompt,

		AgentMaxLoops:                pickInt(override.AgentMaxLoops, base.AgentMaxLoops),
		TaskMaxRetries:               pickInt(override.TaskMaxRetries, base.TaskMaxRetries),
		EnforceCompactTokenThreshold: pickInt(override.EnforceCompactTokenThreshold, base.EnforceCompactTokenThreshold),
		ContextLimit:                 pickInt(override.ContextLimit, base.ContextLimit),
	}
	return rt, nil
}

// pickInt 返回 override（非零）否则 base（含零）。零值视为"未覆盖"——
// 与 RuntimeOverride 文档约定一致。
func pickInt(override, base int) int {
	if override != 0 {
		return override
	}
	return base
}
