package invocation

// 本文件定义一次已经由 L2 ContextCompiler 编译并持久化的模型请求绑定。
// Model Invocation 只消费该绑定，不重新解释 messages/tools，也不从日志推断
// Snapshot 身份。

import (
	"context"
	"fmt"
	"strings"
)

const ContextBindingSchemaV1 = "agentgo.invocation-context-binding/v1"

type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceFunction ToolChoiceMode = "function"
)

type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode,omitempty"`
	Name string         `json:"name,omitempty"`
}

func (c ToolChoice) Validate() error {
	switch c.Mode {
	case "", ToolChoiceAuto:
		if c.Name != "" {
			return fmt.Errorf("tool_choice auto 不得指定 name")
		}
	case ToolChoiceRequired:
		if c.Name != "" {
			return fmt.Errorf("tool_choice required 不得指定 name")
		}
	case ToolChoiceFunction:
		if strings.TrimSpace(c.Name) == "" || len([]rune(c.Name)) > 128 {
			return fmt.Errorf("tool_choice function name 无效")
		}
		for _, r := range c.Name {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return fmt.Errorf("tool_choice function name=%q 含非法字符", c.Name)
			}
		}
	default:
		return fmt.Errorf("tool_choice mode=%q 无效", c.Mode)
	}
	return nil
}

// ContextBinding 把即将发送的 provider request 钉在唯一 ContextSnapshot 上。
// EncodedRequestDigest 来自 ContextCompiler；它不是由 LLM client 重新编码猜测。
type ContextBinding struct {
	Schema               string       `json:"schema"`
	InvocationID         string       `json:"invocation_id"`
	ContextSnapshotID    string       `json:"context_snapshot_id"`
	ContextPolicyID      string       `json:"context_policy_id"`
	ToolRouterSnapshotID string       `json:"tool_router_snapshot_id"`
	EncodedRequestDigest string       `json:"encoded_request_digest"`
	OutputBudget         OutputBudget `json:"output_budget"`
	ToolChoice           ToolChoice   `json:"tool_choice,omitempty"`
	// ReasoningEffort 是本次 Invocation 的显式 wire override。空值使用
	// 全局模型配置；机械终态提交可冻结 none，避免 thinking provider
	// 拒绝 exact tool choice，不影响前面业务轮次的 thinking。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

func (b ContextBinding) Validate() error {
	if b.Schema != ContextBindingSchemaV1 {
		return fmt.Errorf("Invocation ContextBinding schema=%q，无效", b.Schema)
	}
	for name, value := range map[string]string{
		"invocation_id": b.InvocationID, "context_snapshot_id": b.ContextSnapshotID,
		"context_policy_id": b.ContextPolicyID, "tool_router_snapshot_id": b.ToolRouterSnapshotID,
		"encoded_request_digest": b.EncodedRequestDigest,
	} {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("Invocation ContextBinding %s 不能为空", name)
		}
		if len([]rune(value)) > 512 {
			return fmt.Errorf("Invocation ContextBinding %s 超过 512 rune", name)
		}
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("Invocation ContextBinding %s 含控制字符", name)
			}
		}
	}
	if err := b.OutputBudget.Validate(); err != nil {
		return err
	}
	if err := b.ToolChoice.Validate(); err != nil {
		return err
	}
	if b.ReasoningEffort != "" {
		valid := false
		for _, value := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
			if b.ReasoningEffort == value {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("Invocation reasoning_effort=%q 无效", b.ReasoningEffort)
		}
	}
	return nil
}

type contextBindingKey struct{}

// WithContextBinding 在真正调用 provider 前冻结绑定。非法绑定拒绝，不能把
// 空 SnapshotID 当作“兼容成功”。
func WithContextBinding(ctx context.Context, binding ContextBinding) (context.Context, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextBindingKey{}, binding), nil
}

// ContextBindingFrom 供 LLM client、Trace 接缝和测试读取只读值拷贝。
func ContextBindingFrom(ctx context.Context) (ContextBinding, bool) {
	if ctx == nil {
		return ContextBinding{}, false
	}
	binding, ok := ctx.Value(contextBindingKey{}).(ContextBinding)
	return binding, ok
}

// BindFailure 把 provider/transport 返回的 canonical Failure 绑定到发出该请求的
// Snapshot。兼容 wrapper 原样保留；不存在 canonical Failure 的外部错误不伪造。
// 非空且冲突的既有身份属于协议错误，不能静默改写。
func BindFailure(err error, binding ContextBinding) error {
	if err == nil {
		return nil
	}
	failure, ok := FromError(err)
	if !ok {
		return err
	}
	if failure.InvocationID != "" && failure.InvocationID != binding.InvocationID ||
		failure.SnapshotID != "" && failure.SnapshotID != binding.ContextSnapshotID ||
		failure.ProviderPolicy != "" && failure.ProviderPolicy != binding.ContextPolicyID {
		cause := fmt.Errorf("InvocationFailure identity 与 ContextBinding 冲突")
		conflict := NewFailure(FailureProtocolIncompatible, PhaseUsageSettle, OriginProtocol, cause)
		conflict.InvocationID = binding.InvocationID
		conflict.SnapshotID = binding.ContextSnapshotID
		conflict.ProviderPolicy = binding.ContextPolicyID
		return conflict
	}
	failure.InvocationID = binding.InvocationID
	failure.SnapshotID = binding.ContextSnapshotID
	failure.ProviderPolicy = binding.ContextPolicyID
	return err
}
