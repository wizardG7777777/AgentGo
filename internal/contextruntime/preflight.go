package contextruntime

import (
	"context"
	"fmt"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
)

type StaticPromptProfile struct {
	ProfileID        string
	ContextPolicyRef string
	SystemPrompt     string
	TeamAwareness    string
}

// ValidateStaticPrompt 只验证静态材料预算，不调用模型，也不建立虚假绑定。
func (r Runtime) ValidateStaticPrompt(ctx context.Context, p StaticPromptProfile) error {
	if !r.Ready() {
		return fmt.Errorf("L2 指令预检缺少必要依赖")
	}
	ref := p.ContextPolicyRef
	if ref == "" {
		ref = policycatalog.ContextDefaultCurrent
	}
	policy, ok := r.Policies.ContextPolicy(ref)
	if !ok {
		return fmt.Errorf("指令预检引用未知 Context policy")
	}
	replay, ok := r.Policies.ProviderReplayPolicy(policy.ReplayPolicyRef)
	if !ok {
		return fmt.Errorf("指令预检缺少 Replay policy")
	}
	r.Memory = nil
	r.TaskMemory = nil
	conversation, err := r.assemble(ctx, Input{Identity: llm.Identity{InvocationID: "preflight:" + p.ProfileID}, Instructions: Instructions{ProfileID: p.ProfileID, System: p.SystemPrompt, Team: p.TeamAwareness}})
	if err != nil {
		return err
	}
	if len(conversation) == 0 {
		return nil
	}
	_, err = r.Assembler.Compile(ctx, CompileInput{AttemptID: "preflight:" + p.ProfileID, InvocationID: "preflight:" + p.ProfileID, InstructionRef: "instructions:" + contextcontract.DigestBytes([]byte(p.SystemPrompt+p.TeamAwareness)), ExecutionLeaseRef: "preflight:no-execution", Conversation: conversation, ToolRouter: ToolRouterBinding{SnapshotID: "preflight:no-tools"}, BudgetPolicy: policy.Policy, ReplayPolicy: replay.Policy, ReplayPolicyRef: replay.Ref})
	return err
}
