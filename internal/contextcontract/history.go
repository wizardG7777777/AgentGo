package contextcontract

import (
	"agentgo/internal/llm"
)

type ToolResult struct {
	ToolCallID string `json:"tool_call_id"` // 对应 tool call 的 ID
	Content    string `json:"content"`      // 工具执行结果（含错误信息）
}

// HistoryEntry 是 L2 当前历史格式，不接受缺少契约版本的磁盘历史。
type HistoryEntry struct {
	Schema            string              `json:"schema"`
	Replay            *llm.ProtocolReplay `json:"replay,omitempty"`
	TurnID            string              `json:"turn_id,omitempty"`
	Output            string              `json:"output"`
	ToolCalled        bool                `json:"tool_called"`
	AssistantContent  string              `json:"assistant_content"`
	ToolCalls         []llm.ToolCall      `json:"tool_calls"`
	ToolResults       []ToolResult        `json:"tool_results"`
	IncomingMail      string              `json:"incoming_mail,omitempty"`      // 非空时为收到的代理间邮件，注入为 user 角色消息
	SystemNotice      string              `json:"system_notice,omitempty"`      // L4/L1 控制提醒，注入为 system，禁止伪装成 user
	PromptTokens      int                 `json:"prompt_tokens,omitempty"`      // §11.7.3 实测锚定：本轮 LLM 调用的实测 prompt tokens
	CompletionTokens  int                 `json:"completion_tokens,omitempty"`  // §11.7.3 实测锚定：本轮 completion tokens
	Model             string              `json:"model,omitempty"`              // §11.7.3 模型切换基准重置：产生该条回复时使用的模型名
	ContextProjection string              `json:"context_projection,omitempty"` // L2 replay projection control
	// IncomingContext* 让 L3/L4 生成的控制面输入显式声明 L2 类型；空值只为
	// 历史记录保留 marker-based 兼容分类。正文仍在 IncomingMail，避免复制。
	IncomingContextKind      FragmentKind   `json:"incoming_context_kind,omitempty"`
	IncomingContextSection   ContextSection `json:"incoming_context_section,omitempty"`
	IncomingContextAuthority Authority      `json:"incoming_context_authority,omitempty"`
}
