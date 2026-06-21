package scheduler

import (
	"strings"
	"testing"
)

// TestSchedulerSystemPrompt_AgentCapabilitiesFieldDescription verifies that the
// "你能看见什么" section describes the resources.agent_capabilities field structure.
// Validates: Requirements 9.1
func TestSchedulerSystemPrompt_AgentCapabilitiesFieldDescription(t *testing.T) {
	prompt := schedulerSystemPrompt

	requiredPhrases := []string{
		"agent_capabilities",
		"agent_type",
		"capabilities",
		"description",
	}
	for _, phrase := range requiredPhrases {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("schedulerSystemPrompt should contain %q in the agent_capabilities field description", phrase)
		}
	}

	// Verify it appears in the "你能看见什么" context section
	sectionStart := strings.Index(prompt, "你能看见什么")
	if sectionStart == -1 {
		t.Fatal("schedulerSystemPrompt should contain '你能看见什么' section")
	}
	sectionText := prompt[sectionStart:]
	if !strings.Contains(sectionText, "agent_capabilities") {
		t.Error("agent_capabilities should be described in the '你能看见什么' section")
	}
}

// TestSchedulerSystemPrompt_CapabilitiesRoutingGuidance verifies that the prompt
// contains capabilities-based routing guidance in the "路由指引" section.
// Validates: Requirements 9.2, 9.3
func TestSchedulerSystemPrompt_CapabilitiesRoutingGuidance(t *testing.T) {
	prompt := schedulerSystemPrompt

	// Check for capabilities-based routing section
	if !strings.Contains(prompt, "基于 capabilities 的路由决策") {
		t.Error("schedulerSystemPrompt should contain capabilities-based routing guidance section")
	}

	// R9.2: guidance to prefer agents whose capabilities match the task
	if !strings.Contains(prompt, "优先匹配能力") {
		t.Error("schedulerSystemPrompt should contain guidance to prefer capability-matching agents")
	}
	if !strings.Contains(prompt, "优先选择 capabilities 包含该能力的代理类型") {
		t.Error("schedulerSystemPrompt should instruct to prefer agents with matching capabilities")
	}

	// R9.3: guidance to avoid routing to agents lacking required capabilities
	if !strings.Contains(prompt, "避免能力不足的路由") {
		t.Error("schedulerSystemPrompt should contain guidance to avoid routing to capability-lacking agents")
	}
	if !strings.Contains(prompt, "避免将任务路由到该代理类型") {
		t.Error("schedulerSystemPrompt should instruct to avoid routing to agents without required capabilities")
	}
}

// TestSchedulerSystemPrompt_OnlyRouteToExistingAgentTypes verifies that the prompt
// contains constraints about only routing to existing agent types.
// Validates: Requirements 10.1, 10.2, 10.3, 10.4
func TestSchedulerSystemPrompt_OnlyRouteToExistingAgentTypes(t *testing.T) {
	prompt := schedulerSystemPrompt

	// R10.1: instruct to only choose from agent_capabilities and specialized_agents
	if !strings.Contains(prompt, "仅路由到已存在的代理类型") {
		t.Error("schedulerSystemPrompt should contain existing-agent-type-only constraint section")
	}
	if !strings.Contains(prompt, "仅从已知代理类型中选择") {
		t.Error("schedulerSystemPrompt should instruct to only select from known agent types")
	}

	// R10.2: instruct to check event_type before publishing
	if !strings.Contains(prompt, "发布前检查") {
		t.Error("schedulerSystemPrompt should instruct to check event_type before publishing")
	}

	// R10.3: instruct to call report_done when no matching agent exists
	if !strings.Contains(prompt, "report_done") {
		t.Error("schedulerSystemPrompt should mention report_done for when no matching agent exists")
	}
	if !strings.Contains(prompt, "无匹配时不发布") {
		t.Error("schedulerSystemPrompt should instruct not to publish when no matching agent type exists")
	}

	// R10.4: include example about non-existent event_type
	if !strings.Contains(prompt, "示例") {
		t.Error("schedulerSystemPrompt should include an example about non-existent event_type routing")
	}
	// The example should mention that a non-existent event_type should not be used
	if !strings.Contains(prompt, "specialized_agents") {
		t.Error("schedulerSystemPrompt should reference specialized_agents in the routing constraint")
	}
}

// TestSchedulerSystemPrompt_UnavailableToolsGuidance verifies that the
// schedulerSystemPrompt contains "unavailable_tools" guidance in the
// "你能看见什么" section, instructing the Scheduler to avoid assigning tasks
// that depend on unavailable tools and to suggest alternatives via report_done.
// Validates: Requirements 4.4
func TestSchedulerSystemPrompt_UnavailableToolsGuidance(t *testing.T) {
	prompt := schedulerSystemPrompt

	// The prompt must mention unavailable_tools
	if !strings.Contains(prompt, "unavailable_tools") {
		t.Fatal("schedulerSystemPrompt should contain 'unavailable_tools'")
	}

	// Verify it appears in the "你能看见什么" context section
	sectionStart := strings.Index(prompt, "你能看见什么")
	if sectionStart == -1 {
		t.Fatal("schedulerSystemPrompt should contain '你能看见什么' section")
	}
	sectionText := prompt[sectionStart:]
	if !strings.Contains(sectionText, "unavailable_tools") {
		t.Error("unavailable_tools should be described in the '你能看见什么' section")
	}

	// Verify guidance to avoid assigning tasks depending on unavailable tools
	if !strings.Contains(sectionText, "web_search") {
		t.Error("unavailable_tools guidance should mention web_search as an example")
	}
	if !strings.Contains(sectionText, "web_fetch") {
		t.Error("unavailable_tools guidance should mention web_fetch as an example")
	}

	// Verify guidance to suggest alternatives via report_done
	if !strings.Contains(sectionText, "report_done") {
		t.Error("unavailable_tools guidance should instruct to use report_done for suggesting alternatives")
	}
}

// ================================================================
// ⚠️  2026-04-20 回归锁（预期红态 —— 请勿删除断言！）
// ================================================================
//
// 本节下列测试中的 DoesNotClaimSingleTaskPerLoop 当前**故意失败**，用于锁定
// P0-1 "Scheduler publish_task 完全串行发布" 缺陷：在修复完成前它应保持红灯。
// 如果 CI 报此测试失败，**不是回归**，这是提醒 bug 还没修。修复路径：
// 改写 scheduler.go 第 243 行附近的"publish_task 是单次调用工具，一次只能发布
// 一个任务"陈述，明确说"每次调用创建一个任务；同一 reactLoop 内可并行多次
// 调用"，再补一个**纯独立无依赖**任务并行发布的示例（与现有"3 探索 + 1 汇总"
// 的依赖聚合示例形成对照）。
//
// ContainsParallelIndependentPublishGuidance 当前是绿的（现有 prompt 已覆盖），
// 作为回归锁防止修改时误删并行指引。
//
// ❌ 错误处理：删除断言 / 改 Skip / 弱化误导句子列表 —— 这样会掩盖 bug 信号
// ✅ 正确处理：修 scheduler.go 中的 schedulerSystemPrompt，此处自动变绿
//
// 背景（bug 现象）：2026-04-20 并发测试中 scheduler 把 3 个完全独立的子任务按 loop
// 0/1/2 串行发布（每 loop 只 publish 一个并等完），wall-clock 从预期 ~30s 拖到
// 14.5 min，所有并发场景事实上无法被测试触发。根因是 prompt 中"一次只能发布
// 一个任务"这句权威陈述与 llm_executor 的并行 tool call 能力矛盾，误导了 LLM。
//
// 该问题已修复；历史记录见 docs/archived/。
// ================================================================

// TestSchedulerSystemPrompt_DoesNotClaimSingleTaskPerLoop 断言 prompt 不再声称
// publish_task 每次只能发一个任务。该陈述与基础设施能力矛盾，会诱导 LLM 把
// 独立任务串行化发布。
func TestSchedulerSystemPrompt_DoesNotClaimSingleTaskPerLoop(t *testing.T) {
	prompt := schedulerSystemPrompt
	misleading := []string{
		"一次只能发布一个任务",
		"不支持“一次规划多个任务”",
		"不支持\"一次规划多个任务\"",
	}
	for _, phrase := range misleading {
		if strings.Contains(prompt, phrase) {
			t.Errorf("prompt 含误导性陈述 %q —— 该陈述与 llm_executor.go 并行 tool call 能力矛盾，"+
				"会诱导 LLM 把独立任务串行化。见 2026-04-20 历史问题记录 P0-1", phrase)
		}
	}
}

// TestSchedulerSystemPrompt_ContainsParallelIndependentPublishGuidance 断言 prompt
// 明确指引"无依赖的独立任务应在同一轮 reactLoop 中并行 publish_task"。
// 当前 prompt 只有"3 独立探索 + 1 汇总"这种含聚合的示例，缺少**纯独立**批量并行例子。
func TestSchedulerSystemPrompt_ContainsParallelIndependentPublishGuidance(t *testing.T) {
	prompt := schedulerSystemPrompt
	// 必须同时出现以下两类关键词，才算覆盖"独立任务并行"这一场景：
	//   - 关系描述："无依赖" / "相互独立" / "独立任务"
	//   - 模式描述："同一轮" / "同一 reactLoop" / "同时调用 publish_task"
	hasIndependence := strings.Contains(prompt, "无依赖") ||
		strings.Contains(prompt, "相互独立") ||
		strings.Contains(prompt, "独立任务")
	hasParallelism := strings.Contains(prompt, "同一轮") ||
		strings.Contains(prompt, "同一个 reactLoop") ||
		strings.Contains(prompt, "同一 reactLoop") ||
		strings.Contains(prompt, "同时调用 publish_task")

	if !hasIndependence || !hasParallelism {
		t.Errorf("prompt 缺少独立任务并行发布的明确指引（独立关键词=%v, 并行关键词=%v）—— "+
			"2026-04-20 测试暴露 scheduler 把独立任务串行化，需在 prompt 中加入"+
			"明确的'无依赖任务应同轮并行 publish_task'示例。见历史问题记录 P0-1",
			hasIndependence, hasParallelism)
	}
}

// TestSchedulerSystemPrompt_PreservesUserOriginalConstraints 是 2026-04-23
// 随机测试暴露的 P2 "Scheduler 改写子任务 description 时丢失用户原始约束"
// 回归锁。
//
// 现象：用户 prompt 含明确否定约束"**不用撰写文字报告**"，但 scheduler 把
// 用户的顶层任务拆解为子任务时，子任务 description 变成"总结 / 撰写..."
// —— 负约束丢失，explorer/worker 按默认理解继续生成报告。
//
// 根因：schedulerSystemPrompt 未显式要求"拆分 / 改写子任务 description 时
// 必须保留用户的原始约束（尤其是否定性约束：不要、禁止、避免、不用 等）"。
// LLM 倾向于"润色"用户的话，但润色过程常丢失否定词。
//
// 本测试在修复前 🔴 RED：断言 schedulerSystemPrompt 含"保留用户原始约束 /
// 否定约束"相关规则。
//
// 修复方向：prompt 加一段明确的规则，类似：
//
//	"在将用户请求拆分为子任务时，必须**逐字保留**用户的否定性约束（如
//	'不要/禁止/避免/不用'等词）到子任务 description 中。不得以'更清晰的
//	表述'为由丢弃或弱化这些约束。"
func TestSchedulerSystemPrompt_PreservesUserOriginalConstraints(t *testing.T) {
	prompt := schedulerSystemPrompt

	// 一组：至少一个"保留原始意图/约束"的规则字样
	preserveSignals := []string{
		"保留用户",
		"保留原始",
		"原始约束",
		"逐字保留",
		"不得弱化",
		"不得改写用户",
		"用户的否定",
		"否定性约束",
		"否定约束",
	}
	// 二组：至少一个"否定性约束"例子，证明 prompt 作者意识到这类约束特殊
	negationExampleSignals := []string{
		"不要",
		"禁止",
		"避免",
		"不用",
		"don't",
		"avoid",
	}

	hasPreserveRule := false
	for _, k := range preserveSignals {
		if strings.Contains(prompt, k) {
			hasPreserveRule = true
			break
		}
	}
	// 第二组只要 prompt 里有任一否定词作为例子即可；目前 prompt 的"避免"
	// 是用在"避免能力不足的路由"语境下，不是"约束传递"语境。所以我们不
	// 靠第二组独立红，只用第一组作为主断言。
	hasAnyNegationExample := false
	for _, k := range negationExampleSignals {
		if strings.Contains(prompt, k) {
			hasAnyNegationExample = true
			break
		}
	}
	_ = hasAnyNegationExample // 仅作信息性 grep，不强断言

	if !hasPreserveRule {
		t.Errorf("schedulerSystemPrompt 缺少 `保留用户原始约束` 的规则段（期望含 %v 之一）—— "+
			"2026-04-23 随机测试暴露 scheduler 把用户 `不用撰写文字报告` 改写成 `总结...`，"+
			"否定约束丢失。见 2026-04-23 历史问题记录 P2",
			preserveSignals)
	}
}
