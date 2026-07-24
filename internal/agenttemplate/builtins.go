package agenttemplate

import _ "embed"

//go:embed prompts/generalist.md
var generalistPrompt string

//go:embed prompts/explorer.md
var explorerPrompt string

//go:embed prompts/verifier.md
var verifierPrompt string

func builtinDefinitions() []unresolvedTemplate {
	return []unresolvedTemplate{
		{
			Name:         "generalist",
			Version:      1,
			Description:  "通用执行代理：可调查、修改项目并运行验证，遇到计划缺口时请求 Scheduler 重规划。",
			Capabilities: []string{"code-investigation", "file-editing", "command-execution", "web-research"},
			Tools: []string{
				"read_file", "list_dir", "grep_search", "glob_search",
				"write_file", "edit_file", "run_shell", "web_search", "web_fetch",
				"send_message", "request_replan", "submit_task_result",
			},
			SystemPrompt: generalistPrompt,
			Limits: fixedLimits(Limits{
				AgentMaxLoops: 12, TaskMaxRetries: 3,
				EnforceCompactTokenThreshold: 4000, ContextLimit: 16000, MaxReplicas: 4,
			}),
			SourceFile: "embed:prompts/generalist.md",
		},
		{
			Name:         "explorer",
			Version:      1,
			Description:  "只读调查代理：并行收集代码与网络证据，形成有来源的结论，不修改项目文件。",
			Capabilities: []string{"code-investigation", "web-research", "evidence-synthesis"},
			Tools: []string{
				"read_file", "list_dir", "grep_search", "glob_search",
				"web_search", "web_fetch", "send_message", "request_replan",
			},
			SystemPrompt: explorerPrompt,
			Limits: fixedLimits(Limits{
				AgentMaxLoops: 8, TaskMaxRetries: 2,
				EnforceCompactTokenThreshold: 3000, ContextLimit: 12000, MaxReplicas: 6,
			}),
			SourceFile: "embed:prompts/explorer.md",
		},
		{
			Name:         "verifier",
			Version:      1,
			Description:  "正式验收代理：不授予文件写工具，独立运行检查、测试和构建，并提交结构化验收结果。",
			Capabilities: []string{"acceptance-verification", "test-execution", "evidence-validation"},
			Tools: []string{
				"read_file", "list_dir", "grep_search", "glob_search", "run_shell",
				"web_search", "web_fetch", "submit_acceptance_result", "request_replan",
			},
			SystemPrompt: verifierPrompt,
			Limits: fixedLimits(Limits{
				AgentMaxLoops: 10, TaskMaxRetries: 2,
				EnforceCompactTokenThreshold: 3000, ContextLimit: 12000, MaxReplicas: 1,
			}),
			SourceFile: "embed:prompts/verifier.md",
		},
	}
}
