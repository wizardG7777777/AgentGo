package llm

import (
	"context"
	"fmt"

	"agentgo/internal/invocation"
)

// defaultOutputBudget 是 Model Invocation 的第一版绝对安全上限。它不是 L2
// Context 的最终分类型 policy；零配置也不得退化为无界累积。
var defaultOutputBudget = invocation.OutputBudget{
	MaxContentBytes:            128 << 10,
	MaxReasoningBytes:          256 << 10,
	MaxExtraFieldBytes:         256 << 10,
	MaxToolNameBytes:           512,
	MaxToolArgumentsBytes:      64 << 10,
	MaxToolCalls:               16,
	MaxToolArgumentsTotalBytes: 128 << 10,
	MaxResponseBytes:           512 << 10,
	MaxCompletionTokens:        32 << 10,
}

func DefaultOutputBudget() invocation.OutputBudget { return defaultOutputBudget.Clone() }

func normalizeOutputBudget(in invocation.OutputBudget) invocation.OutputBudget {
	out := in.Clone()
	if out.MaxContentBytes <= 0 {
		out.MaxContentBytes = defaultOutputBudget.MaxContentBytes
	}
	if out.MaxReasoningBytes <= 0 {
		out.MaxReasoningBytes = defaultOutputBudget.MaxReasoningBytes
	}
	if out.MaxExtraFieldBytes <= 0 {
		out.MaxExtraFieldBytes = defaultOutputBudget.MaxExtraFieldBytes
	}
	if out.MaxToolNameBytes <= 0 {
		out.MaxToolNameBytes = defaultOutputBudget.MaxToolNameBytes
	}
	if out.MaxToolArgumentsBytes <= 0 {
		out.MaxToolArgumentsBytes = defaultOutputBudget.MaxToolArgumentsBytes
	}
	if out.MaxToolCalls <= 0 {
		out.MaxToolCalls = defaultOutputBudget.MaxToolCalls
	}
	if out.MaxToolArgumentsTotalBytes <= 0 {
		out.MaxToolArgumentsTotalBytes = defaultOutputBudget.MaxToolArgumentsTotalBytes
	}
	if out.MaxResponseBytes <= 0 {
		out.MaxResponseBytes = defaultOutputBudget.MaxResponseBytes
	}
	if out.MaxCompletionTokens <= 0 {
		out.MaxCompletionTokens = defaultOutputBudget.MaxCompletionTokens
	}
	return out
}

func outputBudgetFromContext(ctx context.Context, configured invocation.OutputBudget) invocation.OutputBudget {
	out := normalizeOutputBudget(configured)
	if binding, ok := invocation.ContextBindingFrom(ctx); ok {
		out = minOutputBudget(out, binding.OutputBudget)
	}
	if action, ok := invocation.OutputBudgetFrom(ctx); ok {
		out = minOutputBudget(out, action)
	}
	return out
}

func minOutputBudget(left, right invocation.OutputBudget) invocation.OutputBudget {
	if right.Validate() != nil {
		return left
	}
	min := func(a, b int64) int64 {
		if b < a {
			return b
		}
		return a
	}
	out := left.Clone()
	out.MaxContentBytes = min(out.MaxContentBytes, right.MaxContentBytes)
	out.MaxReasoningBytes = min(out.MaxReasoningBytes, right.MaxReasoningBytes)
	out.MaxExtraFieldBytes = min(out.MaxExtraFieldBytes, right.MaxExtraFieldBytes)
	out.MaxToolNameBytes = min(out.MaxToolNameBytes, right.MaxToolNameBytes)
	out.MaxToolArgumentsBytes = min(out.MaxToolArgumentsBytes, right.MaxToolArgumentsBytes)
	out.MaxToolCalls = min(out.MaxToolCalls, right.MaxToolCalls)
	out.MaxToolArgumentsTotalBytes = min(out.MaxToolArgumentsTotalBytes, right.MaxToolArgumentsTotalBytes)
	out.MaxResponseBytes = min(out.MaxResponseBytes, right.MaxResponseBytes)
	out.MaxCompletionTokens = min(out.MaxCompletionTokens, right.MaxCompletionTokens)
	if out.MaxExtraFieldBytesByName == nil {
		out.MaxExtraFieldBytesByName = make(map[string]int64)
	}
	for key, value := range right.MaxExtraFieldBytesByName {
		if existing, ok := out.MaxExtraFieldBytesByName[key]; !ok || value < existing {
			out.MaxExtraFieldBytesByName[key] = value
		}
	}
	return out
}

type outputBudgetCounter struct {
	budget             invocation.OutputBudget
	phase              invocation.Phase
	contentBytes       int64
	reasoningBytes     int64
	extraBytes         map[string]int64
	toolNames          map[int64]int64
	toolArguments      map[int64]int64
	toolCalls          map[int64]struct{}
	toolArgumentsTotal int64
	totalBytes         int64
}

func newOutputBudgetCounter(budget invocation.OutputBudget, phase invocation.Phase) *outputBudgetCounter {
	return &outputBudgetCounter{
		budget:        normalizeOutputBudget(budget),
		phase:         phase,
		extraBytes:    make(map[string]int64),
		toolNames:     make(map[int64]int64),
		toolArguments: make(map[int64]int64),
		toolCalls:     make(map[int64]struct{}),
	}
}

func (c *outputBudgetCounter) addContent(fragment string) error {
	n := int64(len([]byte(fragment)))
	c.contentBytes += n
	c.totalBytes += n
	if c.contentBytes > c.budget.MaxContentBytes {
		return outputLimitError("content", c.contentBytes, c.budget.MaxContentBytes, c.phase)
	}
	return c.checkTotal()
}

func (c *outputBudgetCounter) addExtra(key, raw, reasoning string) error {
	n := int64(len([]byte(raw)))
	c.extraBytes[key] += n
	c.totalBytes += n
	if c.extraBytes[key] > c.budget.MaxExtraFieldBytes {
		return outputLimitError("extra_fields."+key, c.extraBytes[key], c.budget.MaxExtraFieldBytes, c.phase)
	}
	if namedLimit := c.budget.MaxExtraFieldBytesByName[key]; namedLimit > 0 && c.extraBytes[key] > namedLimit {
		return outputLimitError("extra_fields."+key, c.extraBytes[key], namedLimit, c.phase)
	}
	if reasoning != "" {
		c.reasoningBytes += int64(len([]byte(reasoning)))
		if c.reasoningBytes > c.budget.MaxReasoningBytes {
			return outputLimitError("reasoning", c.reasoningBytes, c.budget.MaxReasoningBytes, c.phase)
		}
	}
	return c.checkTotal()
}

func (c *outputBudgetCounter) addTool(index int64, name, arguments string) error {
	if _, seen := c.toolCalls[index]; !seen {
		c.toolCalls[index] = struct{}{}
		if int64(len(c.toolCalls)) > c.budget.MaxToolCalls {
			return outputLimitError("tool_calls.count", int64(len(c.toolCalls)), c.budget.MaxToolCalls, c.phase)
		}
	}
	nameBytes := int64(len([]byte(name)))
	argsBytes := int64(len([]byte(arguments)))
	c.toolNames[index] += nameBytes
	c.toolArguments[index] += argsBytes
	c.toolArgumentsTotal += argsBytes
	c.totalBytes += nameBytes + argsBytes
	if c.toolNames[index] > c.budget.MaxToolNameBytes {
		return outputLimitError(fmt.Sprintf("tool_calls[%d].name", index),
			c.toolNames[index], c.budget.MaxToolNameBytes, c.phase)
	}
	if c.toolArguments[index] > c.budget.MaxToolArgumentsBytes {
		return outputLimitError(fmt.Sprintf("tool_calls[%d].arguments", index),
			c.toolArguments[index], c.budget.MaxToolArgumentsBytes, c.phase)
	}
	if c.toolArgumentsTotal > c.budget.MaxToolArgumentsTotalBytes {
		return outputLimitError("tool_calls.arguments_total", c.toolArgumentsTotal,
			c.budget.MaxToolArgumentsTotalBytes, c.phase)
	}
	return c.checkTotal()
}

func (c *outputBudgetCounter) checkTotal() error {
	if c.totalBytes > c.budget.MaxResponseBytes {
		return outputLimitError("response_total", c.totalBytes, c.budget.MaxResponseBytes, c.phase)
	}
	return nil
}

func outputLimitError(field string, actual, limit int64, phase invocation.Phase) error {
	err := fmt.Errorf("模型响应字段 %s 超过硬上限：actual_bytes=%d limit_bytes=%d",
		field, actual, limit)
	failure := invocation.NewFailure(invocation.FailureOutputLimitExceeded,
		phase, invocation.OriginRuntime, err)
	if phase == invocation.PhaseStreamAccumulate || phase == invocation.PhaseStreamReceive {
		failure.Partial = true
		failure.UsageState = invocation.UsagePartial
	} else {
		failure.UsageState = invocation.UsageSettled
	}
	return &ErrRecoverable{Err: err, Failure: failure}
}
