package agent

import "context"

// ToolCallIdentity 是 L3 调用事实的关联身份，不携带权限或模型请求选项。
type ToolCallIdentity struct {
	RunID, TaskID, AttemptID, TurnID, InvocationID, CallID, ActionID string
}

type toolCallIdentityKey struct{}

func ToolCallIdentityFromContext(ctx context.Context) ToolCallIdentity {
	identity, _ := ctx.Value(toolCallIdentityKey{}).(ToolCallIdentity)
	return identity
}
