package scheduler

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/contextadapter"
	"agentgo/internal/policycatalog"
)

func TestSchedulerCorePromptKeepsOnlyCrossPhaseAuthority(t *testing.T) {
	for _, want := range []string{
		"每个用户请求都必须形成持久化 Graph",
		"不在建图前亲自调查仓库",
		"仅在 graph-ended 终态唤醒后",
		"每轮只执行当前 <scheduler-phase> 允许的一个工具动作",
		"start_graph 成功只表示执行已交棒",
	} {
		if !strings.Contains(schedulerCorePrompt, want) {
			t.Errorf("Scheduler core prompt 缺少跨阶段不变量 %q", want)
		}
	}
	if len([]byte(schedulerCorePrompt)) > 8<<10 {
		t.Fatalf("Scheduler core prompt 不应重新膨胀成单体教程: bytes=%d", len([]byte(schedulerCorePrompt)))
	}
	if SystemPrompt() != schedulerCorePrompt {
		t.Fatal("审计入口与生产 Scheduler core prompt 不同源")
	}
}

func TestSchedulerPhasePromptsAreClosedAndBounded(t *testing.T) {
	tests := map[string][]string{
		"scheduler:draft-create":    {"create_graph_draft", "唯一动作"},
		"scheduler:draft-configure": {"configure_simple_graph_draft", "execution_class", "底层 Graph AST"},
		"scheduler:draft-validate":  {"validate_current_graph_draft", "独立 Proposal Acceptance"},
		"scheduler:draft-edit":      {"patch_graph_draft", "context_policy_ref=context:default/v7", "普通节点最多一条静态入边", "verdict=pass|fixable|failed"},
		"scheduler:draft-commit":    {"durable transaction cursor", "commit_current_graph_draft", "不启动执行"},
		"scheduler:start":           {"start_current_graph", "等待 graph-ended"},
		"scheduler:recovery":        {"read_graph", "propose_graph_change", "不得重放未知副作用"},
		"scheduler:graph-recovery":  {"failure_context", "propose_graph_change", "result.decision", "新的 work Activation"},
		"scheduler:final-report":    {"read_graph", "get_task_result", "自然语言"},
	}
	for phase, wants := range tests {
		t.Run(phase, func(t *testing.T) {
			prompt := schedulerPromptForPhase(phase)
			if prompt == "" || len([]byte(prompt)) > 8<<10 {
				t.Fatalf("phase prompt 为空或过大: bytes=%d", len([]byte(prompt)))
			}
			for _, want := range wants {
				if !strings.Contains(prompt, want) {
					t.Errorf("phase=%s 缺少 %q", phase, want)
				}
			}
		})
	}
	if got := schedulerPromptForPhase("unknown"); got != "" {
		t.Fatalf("未知 phase 不得猜测 Prompt: %q", got)
	}
}

func TestSchedulerCoreAndEveryPhasePassCurrentContextPolicy(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	runtime := agent.ContextRuntime{Adapter: contextadapter.New(), Policies: catalog}
	if err := runtime.ValidateStaticPrompt(context.Background(), agent.StaticPromptProfile{
		ProfileID: "scheduler-core", ContextPolicyRef: policycatalog.ContextDefaultCurrent,
		SystemPrompt: schedulerCorePrompt,
	}); err != nil {
		t.Fatalf("Scheduler core prompt 不兼容 Context v3: %v", err)
	}
	for _, phase := range []string{
		"scheduler:draft-create", "scheduler:draft-configure", "scheduler:draft-validate",
		"scheduler:draft-edit", "scheduler:draft-commit",
		"scheduler:start", "scheduler:recovery", "scheduler:graph-recovery", "scheduler:final-report",
	} {
		if err := runtime.ValidateStaticPrompt(context.Background(), agent.StaticPromptProfile{
			ProfileID: "scheduler-phase-" + phase, ContextPolicyRef: policycatalog.ContextDefaultCurrent,
			SystemPrompt: schedulerCorePrompt, TeamAwareness: schedulerPromptForPhase(phase),
		}); err != nil {
			t.Fatalf("phase=%s 不兼容 Context v3: %v", phase, err)
		}
	}
}

func TestSchedulerPromptVersionTracksPhaseArchitecture(t *testing.T) {
	const want = "embedded:v10.6-graph-recovery-controller"
	if schedulerPromptVersion != want {
		t.Fatalf("schedulerPromptVersion=%q want=%q", schedulerPromptVersion, want)
	}
}
