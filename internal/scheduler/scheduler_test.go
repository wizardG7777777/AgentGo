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
