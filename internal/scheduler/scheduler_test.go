package scheduler

import "agentgo/internal/testmodel"

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/contextruntime"
	"agentgo/internal/policycatalog"
)

func TestSchedulerCorePromptKeepsOnlyCrossPhaseAuthority(t *testing.T) {
	for _, want := range []string{
		"agentTask", "operation=create", "action=start", "update/add", "result_refs", "send_message 只传信息", "候选",
	} {
		if !strings.Contains(schedulerCorePrompt, want) {
			t.Errorf("Scheduler core prompt 缺少跨阶段不变量 %q", want)
		}
	}
	if SystemPrompt() != schedulerCorePrompt {
		t.Fatal("审计入口与生产 Scheduler core prompt 不同源")
	}
}

func TestSchedulerPhasePromptsAreClosedAndBounded(t *testing.T) {
	tests := map[string][]string{
		"scheduler:authoring":    {"创建并启动图", "内部完成校验和提交"},
		"scheduler:coordination": {"先检视事实", "无需变更"},
		"scheduler:final-report": {"图已结束", "不重启或修改图"},
		"agent:execution":        {"业务节点", "执行工具", "submit_task_result"},
	}

	for phase, wants := range tests {
		t.Run(phase, func(t *testing.T) {
			prompt := schedulerPromptForPhase(phase)
			if prompt == "" {
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
	runtime := contextruntime.Runtime{Assembler: contextruntime.NewAssembler(), Policies: catalog, Snapshots: testmodel.Runtime(t).Snapshots, Options: testmodel.Runtime(t).Options, Output: testmodel.Runtime(t).Output}
	if err := runtime.ValidateStaticPrompt(context.Background(), contextruntime.StaticPromptProfile{
		ProfileID: "scheduler-core", ContextPolicyRef: policycatalog.ContextDefaultCurrent,
		SystemPrompt: schedulerCorePrompt,
	}); err != nil {
		t.Fatalf("Scheduler core prompt 不兼容 Context v3: %v", err)
	}
	for _, phase := range []string{"scheduler:authoring", "scheduler:coordination", "scheduler:final-report", "agent:execution"} {
		if err := runtime.ValidateStaticPrompt(context.Background(), contextruntime.StaticPromptProfile{
			ProfileID: "scheduler-phase-" + phase, ContextPolicyRef: policycatalog.ContextDefaultCurrent,
			SystemPrompt: schedulerCorePrompt, TeamAwareness: schedulerPromptForPhase(phase),
		}); err != nil {
			t.Fatalf("phase=%s 不兼容 Context v3: %v", phase, err)
		}
	}
}

func TestSchedulerPromptVersionTracksPhaseArchitecture(t *testing.T) {
	const want = "embedded:v11-unified-graph-tools"
	if schedulerPromptVersion != want {
		t.Fatalf("schedulerPromptVersion=%q want=%q", schedulerPromptVersion, want)
	}
}
