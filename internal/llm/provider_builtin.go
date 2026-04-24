package llm

import (
	"encoding/json"

	"github.com/openai/openai-go/v3/option"
)

// ============================================================================
// OpenAIProvider —— 标准 OpenAI 兼容后端。
// 所有 hook 均为 no-op。也是 GetProvider 未命中时的 fallback。
// ============================================================================

type OpenAIProvider struct{}

func (*OpenAIProvider) Name() string                              { return "openai" }
func (*OpenAIProvider) PrepareMessages(h []Message) []Message     { return h }
func (*OpenAIProvider) RequestOptions() []option.RequestOption    { return nil }

// ============================================================================
// DeepSeekV4Provider —— DeepSeek V4 系列（如 deepseek-v4-flash）。
//
// V4 的 thinking 模式返回 `reasoning_content` 字段，并要求下一轮请求里
// assistant 消息必须原样包含它，否则 400 "reasoning_content in the thinking
// mode must be passed back"。
//
// 层 1 的 ExtraFields 通用透传已经完整覆盖这个需求——此处保留 Provider 结构，
// PrepareMessages / RequestOptions 都是 no-op，仅作为"显式声明用 V4"的标志位
// 和未来加开关（比如 thinking:disabled）的挂点。
// ============================================================================

type DeepSeekV4Provider struct{}

func (*DeepSeekV4Provider) Name() string                           { return "deepseek-v4" }
func (*DeepSeekV4Provider) PrepareMessages(h []Message) []Message  { return h }
func (*DeepSeekV4Provider) RequestOptions() []option.RequestOption {
	return []option.RequestOption{
		option.WithJSONSet("thinking", map[string]string{"type": "enabled"}),
	}
}

// ============================================================================
// DeepSeekR1Provider —— DeepSeek R1 (deepseek-reasoner) 系列。
//
// R1 的约束与 V4 相反：响应里会返回 `reasoning_content`，但**下一轮请求必须
// 把历史 assistant 消息里的 reasoning_content 删掉**，否则 400。官方文档：
// "The API will return a 400 error if `reasoning_content` is included in
// input messages, so you must remove this field from previous responses
// before sending new requests in multi-round conversations."
//
// 实现：PrepareMessages 遍历 history，对每条 assistant 消息，从 ExtraFields
// 里删除 "reasoning_content"（保留其它 extras 不受影响）。
// ============================================================================

type DeepSeekR1Provider struct{}

func (*DeepSeekR1Provider) Name() string { return "deepseek-r1" }

func (*DeepSeekR1Provider) PrepareMessages(history []Message) []Message {
	out := make([]Message, len(history))
	for i, m := range history {
		out[i] = m
		if m.Role != "assistant" || len(m.ExtraFields) == 0 {
			continue
		}
		if _, has := m.ExtraFields["reasoning_content"]; !has {
			continue
		}
		cleaned := make(map[string]json.RawMessage, len(m.ExtraFields)-1)
		for k, v := range m.ExtraFields {
			if k == "reasoning_content" {
				continue
			}
			cleaned[k] = v
		}
		if len(cleaned) == 0 {
			cleaned = nil
		}
		out[i].ExtraFields = cleaned
	}
	return out
}

func (*DeepSeekR1Provider) RequestOptions() []option.RequestOption { return nil }

// ============================================================================
// init：注册所有内置 provider。
// ============================================================================

func init() {
	RegisterProvider(&OpenAIProvider{})
	RegisterProvider(&DeepSeekV4Provider{})
	RegisterProvider(&DeepSeekR1Provider{})
}
