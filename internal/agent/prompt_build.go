package agent

// prompt_build.go 实现 V6 §2（P1a）Prompt 有序编译的 agent 侧接入。
//
// processTask 在每个 attempt 开始（含重试新 attempt）编译一次并冻结：
// 同一 attempt 的各轮 LLM 调用复用同一 Build（经 ctx 载体传给 executor，
// 并入每轮 context_manifest_built 事件的 prompt_build_id 字段）；
// 核心指令在任务执行中不改变——这是现状语义的钉住，不是新限制。
//
// 组件序列（顺序即 Build.Text 拼接顺序，与 buildMessages 的 system+user
// 首条布局一致）：
//  1. agent_role（InMessage）：system prompt 全文；Version=启动期装配的
//     来源版本（runner=system_prompt_file 内容 sha256 前 12，scheduler=
//     内嵌常量版本，team 模板=模板 Version）；task.SystemPrompt 覆盖时是
//     另一个 Build（Version=task-override，digest 随正文变化）。
//  2. base_contract（InMessage，仅 teamAwareness 非空时）：团队能力感知 +
//     纪律提醒静态块。
//  3. control_protocol（InMessage）：<task-context> 控制面块。
//  4. task_objective（InMessage）：任务描述 + 前置任务结果。
//  5. tool_guidance（带外）：当时工具清单摘要——来自冻结执行租约
//     （ToolUnion）或注册全集的工具名列表，非手写文本。工具定义经 LLM API
//     的 tools 协议通道下发而非消息字节，故不进入 Build.Text。
//  6. output_contract（带外）：任务结果提交协议简述（按控制通道工具派生）。
//
// Build.Text 与 buildMessages 的 system+user 首条逐字节一致（测试钉住）；
// 渲染路径完全复用 buildMessages，本文件只构造身份与观测产物。

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/prompt"
	"agentgo/internal/trace"
)

// PromptIdentityProvider 暴露 executor 持有的静态 prompt 身份，供
// processTask 编译 Prompt Build 时读取。*LLMExecutor 实现本接口；
// runner / scheduler 装配时把同一 executor 句柄赋给 Agent.PromptSource。
// nil 时 agent_role / base_contract 组件缺失（仅装配降级，不阻断任务）。
type PromptIdentityProvider interface {
	// SystemPrompt 返回启动期装配的静态 system prompt 全文。
	SystemPrompt() string
	// PromptVersion 返回来源版本（文件内容 sha256 前 12 / 模板 Version /
	// 内嵌常量版本）。
	PromptVersion() string
	// TeamAwareness 返回静态团队感知文本（注入 user 首条最前）。
	TeamAwareness() string
}

// 组件来源版本词表（协议段版本；renderer 变更时递增）。
const (
	promptVersionTaskContext    = "task-context/v1"
	promptVersionTaskDesc       = "task-description/v1"
	promptVersionTeamAwareness  = "team-awareness/v1"
	promptVersionSubmitContract = "submit-protocol/v1"
	// promptVersionTaskOverride 是 task.SystemPrompt 覆盖时 agent_role 的
	// 来源版本标记（正文 digest 仍覆盖真实内容）。
	promptVersionTaskOverride = "task-override"
)

// withPromptBuild 把冻结的 Prompt Build 挂到 ctx（processTask 每个 attempt
// 调用一次；之后每轮 loop 派生的 execCtx 共享同一 Build 值——值语义，
// 不可变，无需加锁）。
func withPromptBuild(ctx context.Context, build prompt.Build) context.Context {
	return context.WithValue(ctx, ctxPromptBuild, build)
}

func promptBuildFromContext(ctx context.Context) (prompt.Build, bool) {
	build, ok := ctx.Value(ctxPromptBuild).(prompt.Build)
	return build, ok
}

// compilePromptBuild 按当前 attempt 的输入编译并冻结 Prompt Build。
// 输入全部取自任务开始时刻的快照事实：executor 静态 prompt（含
// task.SystemPrompt 覆盖判定）、冻结执行租约的工具面、任务本身与依赖结果。
func (a *Agent) compilePromptBuild(task *model.Task, depResults map[string]string, lease *model.ExecutionLease) prompt.Build {
	// agent_role / base_contract 来源：executor 静态身份（与
	// llm_executor.Execute 的 effectivePrompt 判定同序——任务级覆盖优先）。
	var effectivePrompt, promptVersion, teamAwareness string
	if a.PromptSource != nil {
		effectivePrompt = a.PromptSource.SystemPrompt()
		promptVersion = a.PromptSource.PromptVersion()
		teamAwareness = a.PromptSource.TeamAwareness()
	}
	if task.SystemPrompt != "" {
		effectivePrompt = task.SystemPrompt
		promptVersion = promptVersionTaskOverride
	}

	parts := make([]prompt.Component, 0, 6)
	if effectivePrompt != "" {
		parts = append(parts, prompt.Component{
			ID: prompt.ComponentAgentRole, Version: promptVersion,
			Text: effectivePrompt, InMessage: true,
		})
	}
	if teamAwareness != "" {
		// 与 buildMessages 的注入形态一致：teamAwareness 后接 "\n"。
		parts = append(parts, prompt.Component{
			ID: prompt.ComponentBaseContract, Version: promptVersionTeamAwareness,
			Text: teamAwareness + "\n", InMessage: true,
		})
	}
	parts = append(parts, prompt.Component{
		ID: prompt.ComponentControlProtocol, Version: promptVersionTaskContext,
		Text: renderTaskContextBlock(task), InMessage: true,
	})
	parts = append(parts, prompt.Component{
		ID: prompt.ComponentTaskObjective, Version: promptVersionTaskDesc,
		Text: renderTaskObjective(task, depResults), InMessage: true,
	})

	// tool_guidance（带外）：当时工具清单摘要——冻结租约的工具面优先
	//（首认领后各 attempt 复用同一租约，清单随之冻结）；记录型租约或无
	// 租约时回退注册全集；均无（ToolSwapper 未装配）时只留控制通道。
	toolNames := promptToolNames(a.ToolSwapper, lease)
	toolVersion := "allowlist"
	if lease != nil && lease.Digest != "" {
		toolVersion = "lease:" + lease.Digest
	}
	if len(toolNames) > 0 {
		parts = append(parts, prompt.Component{
			ID: prompt.ComponentToolGuidance, Version: toolVersion,
			Text: strings.Join(toolNames, ","), InMessage: false,
		})
	}

	// output_contract（带外）：任务结果提交协议简述，按控制通道派生。
	controlTools := []string{"submit_task_result"}
	if lease != nil && len(lease.ControlTools) > 0 {
		controlTools = lease.ControlTools
	}
	parts = append(parts, prompt.Component{
		ID: prompt.ComponentOutputContract, Version: promptVersionSubmitContract,
		Text: renderOutputContract(task, controlTools), InMessage: false,
	})

	return prompt.Compile(parts)
}

// renderTaskObjective 渲染 task_objective 组件正文：任务描述 + 前置任务
// 结果后缀，与 buildMessages 的 user 首条对应段同一形态（依赖结果按同一
// map 序循环；单元素/空依赖时逐字节稳定，与 buildMessages 自身的不确定性
// 口径一致）。
func renderTaskObjective(task *model.Task, depResults map[string]string) string {
	var sb strings.Builder
	sb.WriteString(task.Description)
	if len(depResults) > 0 {
		sb.WriteString("\n\n--- 前置任务结果 ---\n")
		keys := make([]string, 0, len(depResults))
		for depID := range depResults {
			keys = append(keys, depID)
		}
		sort.Strings(keys)
		for _, depID := range keys {
			result := depResults[depID]
			sb.WriteString(fmt.Sprintf("[%s] %s\n", depID, result))
		}
	}
	return sb.String()
}

// promptToolNames 返回 tool_guidance 组件的工具清单（排序、去重）：
// 冻结租约的 BusinessTools 非空时取 ToolUnion（业务 ∪ 控制）；否则取
// 注册全集（含 scheduler 控制面——其记录型租约 BusinessTools 为 nil，
// 但 swapper 已装配供观测）；最后回退租约控制通道。
func promptToolNames(swapper ToolRegistrySwapper, lease *model.ExecutionLease) []string {
	var names []string
	switch {
	case lease != nil && len(lease.BusinessTools) > 0:
		names = lease.ToolUnion()
	case swapper != nil:
		names = swapper.ToolRegistry().Names()
	case lease != nil:
		names = lease.ControlTools
	}
	if len(names) == 0 {
		return nil
	}
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

// renderOutputContract 渲染 output_contract 组件正文：按控制通道工具派生
// 的结果提交协议简述（身份/观测用，不进入消息字节）。
func renderOutputContract(task *model.Task, controlTools []string) string {
	has := func(name string) bool {
		for _, t := range controlTools {
			if t == name {
				return true
			}
		}
		return false
	}
	switch {
	case has("submit_task_result"):
		if task != nil && task.GraphNodeKind == "acceptance" {
			return "任务收尾须经 submit_task_result 提交结构化结果；completed 验收结论仅用 verdict=pass|fixable|failed 与 cited_evidence，证据不足用 status=blocked；禁止 event"
		}
		if task != nil && task.GraphID != "" {
			return "任务收尾须经 submit_task_result 提交结构化结果（status/summary）；业务路由字段只放入 result JSON object，禁止 event；阻塞必须给 blocked_reason"
		}
		return "任务收尾须经 submit_task_result 提交结构化结果（status/summary/result；阻塞必须给 blocked_reason）"
	case has("report_done"):
		return "任务收尾可经 report_done 显式汇报；自然文本回复即最终答案"
	default:
		return "自然文本回复即最终答案"
	}
}

// emitPromptCompiled 发射 prompt_compiled 事件：每个 attempt 恰好一条，
// 只载 Build.ID 与逐组件身份摘要（不含正文）。
func (a *Agent) emitPromptCompiled(taskID string, build prompt.Build) {
	trace.Emit(trace.Event{
		Kind:          trace.KindPromptCompiled,
		TaskID:        taskID,
		AgentID:       a.ID,
		PromptBuildID: build.ID,
		Description:   build.ComponentsSummaryJSON(),
	})
}
