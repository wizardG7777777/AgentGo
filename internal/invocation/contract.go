package invocation

import (
	"fmt"
	"strings"
)

// OutputBudget 是单次模型响应在调用前冻结的硬边界。
//
// 字节上限用于保护内存和 wire item；MaxCompletionTokens 同时下发给支持该字段的
// provider。零值由具体 ModelInvoker 解析为其版本化默认 policy，不能解释为无限。
type OutputBudget struct {
	MaxContentBytes            int64            `json:"max_content_bytes"`
	MaxReasoningBytes          int64            `json:"max_reasoning_bytes"`
	MaxExtraFieldBytes         int64            `json:"max_extra_field_bytes"`
	MaxExtraFieldBytesByName   map[string]int64 `json:"max_extra_field_bytes_by_name,omitempty"`
	MaxToolNameBytes           int64            `json:"max_tool_name_bytes"`
	MaxToolArgumentsBytes      int64            `json:"max_tool_arguments_bytes"`
	MaxToolCalls               int64            `json:"max_tool_calls"`
	MaxToolArgumentsTotalBytes int64            `json:"max_tool_arguments_total_bytes"`
	MaxResponseBytes           int64            `json:"max_response_bytes"`
	MaxCompletionTokens        int64            `json:"max_completion_tokens"`
}

func (b OutputBudget) Validate() error {
	for name, value := range map[string]int64{
		"max_content_bytes": b.MaxContentBytes, "max_reasoning_bytes": b.MaxReasoningBytes,
		"max_extra_field_bytes": b.MaxExtraFieldBytes, "max_tool_name_bytes": b.MaxToolNameBytes,
		"max_tool_arguments_bytes": b.MaxToolArgumentsBytes, "max_tool_calls": b.MaxToolCalls,
		"max_tool_arguments_total_bytes": b.MaxToolArgumentsTotalBytes,
		"max_response_bytes":             b.MaxResponseBytes, "max_completion_tokens": b.MaxCompletionTokens,
	} {
		if value <= 0 {
			return fmt.Errorf("Invocation OutputBudget %s 必须 > 0", name)
		}
	}
	for name, value := range b.MaxExtraFieldBytesByName {
		if strings.TrimSpace(name) == "" || value <= 0 || value > b.MaxExtraFieldBytes {
			return fmt.Errorf("Invocation OutputBudget extra field=%q limit=%d 无效", name, value)
		}
	}
	return nil
}

func (b OutputBudget) Clone() OutputBudget {
	out := b
	if b.MaxExtraFieldBytesByName != nil {
		out.MaxExtraFieldBytesByName = make(map[string]int64, len(b.MaxExtraFieldBytesByName))
		for key, value := range b.MaxExtraFieldBytesByName {
			out.MaxExtraFieldBytesByName[key] = value
		}
	}
	return out
}
