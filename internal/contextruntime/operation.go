package contextruntime

import (
	"context"
	"encoding/json"

	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
	"github.com/google/uuid"
)

// InvokeOperation 为无业务任务的验证器和探针创建真实操作身份，仍经过同一编译与调用门。
func (r Runtime) InvokeOperation(ctx context.Context, p Instructions, conversation []ConversationItem, tools []llm.ToolDef, options llm.Options, invoker llm.Invoker) (llm.Result, error) {
	id := uuid.NewString()
	raw, err := json.Marshal(tools)
	if err != nil {
		return llm.Result{}, err
	}
	identity := llm.Identity{OperationID: id, InvocationID: id + "/invocation/1", AttemptID: id + "/attempt/1", TurnID: id + "/turn/1", ContextPolicyID: policycatalog.ContextDefaultCurrent}
	if r.SessionID != nil {
		identity.SessionID = r.SessionID()
	}
	if options.ProfileRef == "" {
		options.ProfileRef = p.ProfileID
	}
	limit := options.OutputBudget
	compiled, err := r.Compile(ctx, Input{Identity: identity, Instructions: p, Conversation: conversation, Options: options, OutputLimit: &limit, ToolRouter: ToolRouterBinding{SnapshotID: "operation-tools:" + contextcontract.DigestBytes(raw), Definitions: tools}, ExecutionLeaseRef: "operation-no-dispatch:" + id})
	if err != nil {
		return llm.Result{}, err
	}
	return r.InvokeCompiled(ctx, compiled, invoker)
}
