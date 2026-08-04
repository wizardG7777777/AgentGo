package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentgo/internal/trace"
)

// TraceEvent 是 trace.Event 面向 Web 前端的精简投影（KindTraceEvent 的载荷）。
//
// 只保留事件流面板需要的常用字段：kind / task / agent / loop / tool /
// message（错误、原因、任务描述的摘要位）以及少量高频数值字段。
// trace.Event 本身体积大且字段随 kind 高度稀疏（Args map、ShellExec 子结构等），
// 全量透传给浏览器没有收益——完整事件永远在 trace JSONL 里可查。
type TraceEvent struct {
	Kind                   string    `json:"kind"`
	TaskID                 string    `json:"task_id,omitempty"`
	AgentID                string    `json:"agent_id,omitempty"`
	Loop                   int       `json:"loop,omitempty"`
	Tool                   string    `json:"tool,omitempty"`
	CallID                 string    `json:"call_id,omitempty"`
	ArgsSummary            string    `json:"args_summary,omitempty"`
	Outcome                string    `json:"outcome,omitempty"` // running | success | error
	ResultLen              int       `json:"result_len,omitempty"`
	Message                string    `json:"message,omitempty"` // Error / Reason / Description 首个非空
	Path                   string    `json:"path,omitempty"`    // file_written 等文件事件
	DurationMS             int64     `json:"duration_ms,omitempty"`
	PromptTokens           int       `json:"prompt_tokens,omitempty"`
	CompletionTokens       int       `json:"completion_tokens,omitempty"`
	At                     time.Time `json:"at"`
}

// ProjectTraceEvent 把 trace.Event 投影为 TraceEvent（导出以便 dashboard
// 的 SSE 编码与测试直接使用同一投影逻辑）。
func ProjectTraceEvent(ev trace.Event) TraceEvent {
	msg := ev.Error
	if msg == "" {
		msg = ev.Reason
	}
	if msg == "" {
		msg = ev.Description
	}
	at := ev.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	projected := TraceEvent{
		Kind:             string(ev.Kind),
		TaskID:           ev.TaskID,
		AgentID:          ev.AgentID,
		Loop:             ev.Loop,
		Tool:             ev.Tool,
		CallID:           ev.CallID,
		ArgsSummary:      summarizeTraceArgs(ev.Args),
		ResultLen:        ev.ResultLen,
		Message:          msg,
		Path:             ev.Path,
		DurationMS:       ev.DurationMS,
		PromptTokens:     ev.PromptTokens,
		CompletionTokens: ev.CompletionTokens,
		At:               at,
	}
	switch ev.Kind {
	case trace.KindToolCall:
		projected.Outcome = "running"
	case trace.KindToolResult:
		if ev.Error != "" {
			projected.Outcome = "error"
		} else {
			projected.Outcome = "success"
		}
	}
	return projected
}

const traceArgsSummaryLimit = 180

// summarizeTraceArgs keeps the live UI useful without copying full tool
// payloads (file contents, prompts, shell output, credentials) into every SSE
// subscriber. Complete arguments remain available in the durable trace JSONL.
func summarizeTraceArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	ordered := make(map[string]any, len(keys))
	for _, key := range keys {
		ordered[key] = redactTraceValue(key, args[key])
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(ordered); err != nil {
		return "{...}"
	}
	return truncateTraceSummary(strings.TrimSpace(encoded.String()), traceArgsSummaryLimit)
}

func redactTraceValue(key string, value any) any {
	if isSensitiveTraceKey(key) {
		return "<redacted>"
	}
	if isVerboseTraceKey(key) {
		if text, ok := value.(string); ok {
			return fmt.Sprintf("<%s %d chars>", normalizedTraceKey(key), len([]rune(text)))
		}
		return "<omitted>"
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			out[childKey] = redactTraceValue(childKey, childValue)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, childValue := range typed {
			out[i] = redactTraceValue("", childValue)
		}
		return out
	case string:
		return truncateTraceSummary(typed, 80)
	default:
		return value
	}
}

func isSensitiveTraceKey(key string) bool {
	normalized := normalizedTraceKey(key)
	for _, marker := range []string{"password", "passwd", "secret", "token", "api_key", "apikey", "authorization", "credential"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func isVerboseTraceKey(key string) bool {
	normalized := normalizedTraceKey(key)
	for _, marker := range []string{"command", "content", "prompt", "system_prompt", "description", "body", "output", "patch"} {
		if normalized == marker || strings.HasSuffix(normalized, "_"+marker) {
			return true
		}
	}
	return false
}

func normalizedTraceKey(key string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
}

func truncateTraceSummary(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

// EmitTraceEvent 把一条 trace 事件包装为 KindTraceEvent 更新广播给全部订阅者。
//
// 速率安全：trace 含高频事件（llm_call_start/end、tool_call/result）。
// 本方法只做一次非阻塞扇出（与 Hub 其他广播同一
// drop-oldest 背压纪律），慢订阅者丢事件、快订阅者与 Hub 主循环均不受影响；
// 零订阅者时是纯 no-op。可在任意 goroutine 上并发调用（Reactor 分发路径如此）。
func (h *Hub) EmitTraceEvent(ev trace.Event) {
	projected := ProjectTraceEvent(ev)
	h.recordTrace(projected)
	if ev.Kind == trace.KindLLMCallEnd {
		h.recordLLMUsage(ev)
	}
	h.broadcast(Update{
		Kind:  KindTraceEvent,
		Trace: projected,
		At:    time.Now(),
	})
}
