package llm

// Invoke 是生产 Model Invocation 的唯一请求入口。Chat 保留为 provider/SDK
// 兼容端口与测试 fake 接口；生产调用方必须先由 ContextCompiler 生成并持久化
// Snapshot，再携 ContextBinding 调用 Invoke。

import (
	"context"
	"fmt"

	"agentgo/internal/invocation"
)

type InvocationRequest struct {
	Binding  invocation.ContextBinding
	Messages []Message
	Tools    []ToolDef
}

func (r InvocationRequest) Validate() error {
	if err := r.Binding.Validate(); err != nil {
		return err
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("Model Invocation messages 不能为空")
	}
	return nil
}

// Invoke 校验并把 ContextBinding 附着到 provider ctx；底层 Client 不再需要
// import ContextCompiler，也不能成为第二条消息装配路径。
func Invoke(ctx context.Context, client Client, request InvocationRequest) (Response, error) {
	if client == nil {
		cause := fmt.Errorf("Model Invocation client 不能为空")
		return Response{}, invocation.NewFailure(invocation.FailureUnknown,
			invocation.PhaseRequestBuild, invocation.OriginRuntime, cause)
	}
	if err := request.Validate(); err != nil {
		return Response{}, invocation.NewFailure(invocation.FailureContextAssembly,
			invocation.PhaseRequestBuild, invocation.OriginRuntime, err)
	}
	bound, err := invocation.WithContextBinding(ctx, request.Binding)
	if err != nil {
		return Response{}, invocation.NewFailure(invocation.FailureContextAssembly,
			invocation.PhaseRequestBuild, invocation.OriginRuntime, err)
	}
	response, callErr := client.Chat(bound, request.Messages, request.Tools)
	return response, invocation.BindFailure(callErr, request.Binding)
}

// InvokeLegacy 是旧快照/隔离测试的显式兼容入口。它故意不接受 ContextBinding，
// 让生产代码审计可以机械区分 legacy，而不是由 nil 依赖静默落入第二条路径。
func InvokeLegacy(ctx context.Context, client Client, messages []Message, tools []ToolDef) (Response, error) {
	if client == nil {
		return Response{}, fmt.Errorf("legacy Model Invocation client 不能为空")
	}
	return client.Chat(ctx, messages, tools)
}
