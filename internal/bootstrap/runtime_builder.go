package bootstrap

// runtime_builder.go 实现 nextUpgrade_v4.md §11.6.1 中提到的几个合成函数：
//   - buildAgentRuntime：从 AgentKind + LLMConfig 合成 AgentRuntimeConfig
//   - buildSchedulerRuntime：scheduler 路径的同名函数
//   - buildKindLLMClient：基于 LLMConfig 与 kind.Model 合并值构造 llm.Client
//   - resolveDependencies：按 AllowedTools 决定该 runner 需要哪些 deps（当前简化版
//     由 RunnerDeps 一并提供，未来可按工具收紧）
//
// 这些函数已被 Bootstrap 主流程调用（Phase C 切换完成后启用）。
// v4 kind-based 路径是当前唯一启动路径。

import (
	"fmt"
	"os"
	"strings"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
)

// buildKindLLMClient 基于 LLMConfig 与 per-kind model 覆盖值构造 llm.Client。
// kindModel 为空字符串时回落 LLMConfig.DefaultModel——这是 v4 §11.4 注释中
// "Model 缺省回落 LLM.DefaultModel" 的实际落地点。
//
// LLMConfig.Provider 用于区分 openai / deepseek-v4 / deepseek-r1 等非标 endpoint
// 的请求 quirks。空字符串时 SDKClient 内部 fallback 到 OpenAIProvider（详见
// internal/llm/provider.go）。
func buildKindLLMClient(llmCfg config.LLMConfig, kindModel string) llm.Client {
	model := kindModel
	if model == "" {
		model = llmCfg.DefaultModel
	}
	timeout := time.Duration(llmCfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return llm.NewSDKClient(
		llmCfg.BaseURL,
		llmCfg.APIKey,
		model,
		"", // system prompt 由 runner / scheduler 自管，不在 client 层注入
		llmCfg.Provider,
		timeout,
	)
}

// buildAgentRuntime 从 AgentKind 声明 + LLMConfig 默认值合成 AgentRuntimeConfig。
//
// 关键合成步骤：
//   - InstanceID：由 kind + replicaIndex 拼接（"worker-1"、"worker-2" ...）
//   - Model 优先级：kind.Model > llm.default_model
//   - SystemPrompt：从 kind.SystemPromptFile 读入到内存（启动期一次性读取，
//     运行时 prompt 不可变，与 nextUpgrade_v4.md §11.9"配置热重载"边界一致）
//   - AllowedTools：profile 名查 ToolProfiles 表 / 直接用 tools 字段
//
// replicaIndex 从 1 开始（与 v3 worker-1/worker-2 命名风格一致）。
func buildAgentRuntime(
	kind config.AgentKind,
	llmCfg config.LLMConfig,
	toolProfiles map[string][]string,
	allKinds []config.AgentKind,
	replicaIndex int,
) (config.AgentRuntimeConfig, error) {
	// 解析工具集（profile 名查表 / tools 字段直接使用）
	var allowed []string
	if kind.Profile != "" {
		toolList, ok := toolProfiles[kind.Profile]
		if !ok {
			return config.AgentRuntimeConfig{}, fmt.Errorf(
				"kind=%q 引用的 profile %q 不存在于 tool_profiles", kind.Kind, kind.Profile)
		}
		allowed = toolList
	} else if len(kind.Tools) > 0 {
		allowed = kind.Tools
	} else {
		return config.AgentRuntimeConfig{}, fmt.Errorf(
			"kind=%q 既未声明 profile 也未声明 tools——必须二选一", kind.Kind)
	}

	// 读入 system prompt 文件
	promptBytes, err := os.ReadFile(kind.SystemPromptFile)
	if err != nil {
		return config.AgentRuntimeConfig{}, fmt.Errorf(
			"kind=%q system_prompt_file=%q 读取失败: %w",
			kind.Kind, kind.SystemPromptFile, err)
	}

	// Model 合并：per-kind 覆盖 > 全局 default
	model := kind.Model
	if model == "" {
		model = llmCfg.DefaultModel
	}

	// 构建团队能力感知提示词：列出系统中所有 Agent 类型及其能力边界
	teamAwareness := buildTeamAwareness(kind, allKinds, toolProfiles)

	rt := config.AgentRuntimeConfig{
		InstanceID:                   fmt.Sprintf("%s-%d", kind.Kind, replicaIndex),
		Kind:                         kind.Kind,
		EventType:                    kind.EventType,
		AllowedTools:                 allowed,
		Model:                        model,
		SystemPrompt:                 string(promptBytes),
		AgentMaxLoops:                kind.AgentMaxLoops,
		TaskMaxRetries:               kind.TaskMaxRetries,
		EnforceCompactTokenThreshold: kind.EnforceCompactTokenThreshold,
		ContextLimit:                 kind.ContextLimit,
		TeamAwareness:                teamAwareness,
	}
	return rt, nil
}

// buildTeamAwareness 构建团队能力感知提示词。
// 列出系统中所有 Agent 类型（kind）的工具集与角色描述，帮助每个 Agent 了解
// 协作者的能力边界，避免指派超出对方能力的任务。
func buildTeamAwareness(
	myKind config.AgentKind,
	allKinds []config.AgentKind,
	toolProfiles map[string][]string,
) string {
	var b strings.Builder
	b.WriteString("\n# 团队能力感知（本次任务涉及以下 Agent 类型）\n\n")
	b.WriteString("本次任务由多类 Agent 协作完成。以下为系统中各类 Agent 的能力清单，\n")
	b.WriteString("供你在派发任务、请求协助或判断协作者能力边界时参考。\n")
	b.WriteString("请特别注意：**不要指派超出对方工具集范围的操作**。\n\n")

	for _, k := range allKinds {
		// 跳过 scheduler（不是任务执行者）
		if k.Kind == "scheduler" {
			continue
		}

		// 解析工具集
		var tools []string
		if k.Profile != "" {
			if t, ok := toolProfiles[k.Profile]; ok {
				tools = t
			}
		} else {
			tools = k.Tools
		}

		b.WriteString(fmt.Sprintf("## %s\n", k.Kind))
		if k.Description != "" {
			b.WriteString(fmt.Sprintf("- 角色：%s\n", strings.TrimSpace(k.Description)))
		}
		if k.EventType != "" {
			b.WriteString(fmt.Sprintf("- 事件类型：%s\n", k.EventType))
		}
		if len(tools) > 0 {
			b.WriteString(fmt.Sprintf("- 可用工具：%s\n", strings.Join(tools, ", ")))
		}
		b.WriteString("\n")
	}

	// 追加一条关于自己的提示
	b.WriteString("# 纪律提醒\n\n")
	b.WriteString("- 当你需要其他 Agent 执行写文件、运行命令等操作时，\n")
	b.WriteString("  必须先确认对方 Agent 类型拥有对应工具（见上表）。\n")
	b.WriteString("- **发布新任务前，务必确认对方是否拥有对应的工具**。\n")
	b.WriteString("  如果对方没有执行任务所需的工具，**禁止**发布该 Agent 无能力执行的任务。\n")
	b.WriteString("  例如：对方没有 write_file → 不应要求其写入文件；\n")
	b.WriteString("  对方没有 run_shell → 不应要求其执行命令。\n")
	b.WriteString("- 若目标 Agent 缺少所需工具，可尝试要求其以**其他方式**完成或交付任务：\n")
	b.WriteString("  如没有写入能力时，要求其以直接文字回复的形式回报；\n")
	b.WriteString("  如没有网络搜索能力时，要求其基于已有资料回答。\n")
	b.WriteString("- 不要假设「所有 Agent 都有相同工具集」——不同 kind 的工具配置可能不同。\n")
	b.WriteString("\n---\n")

	return b.String()
}

// buildSchedulerRuntime 为 scheduler 单例合成 AgentRuntimeConfig。
//
// nextUpgrade_v4.md §11.5.5 配置面收窄：scheduler 仅 model 字段允许外部覆盖，
// 其余字段（工具集 / 系统提示词 / 行为参数 / replicas）全部硬编码在 internal/scheduler。
// 因此本函数只负责"把外部可配的 model 字段合并好"，其余字段留空——scheduler
// 内部构造时按内置常量兜底。
func buildSchedulerRuntime(sched config.SchedulerKind, llmCfg config.LLMConfig) config.AgentRuntimeConfig {
	model := sched.Model
	if model == "" {
		model = llmCfg.DefaultModel
	}
	return config.AgentRuntimeConfig{
		InstanceID: "scheduler",
		Kind:       "scheduler",
		Model:      model,
		// 其余字段（AllowedTools / SystemPrompt / 行为参数）由 internal/scheduler 内部决定
	}
}
