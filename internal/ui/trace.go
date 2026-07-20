package ui

import (
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
	Kind             string    `json:"kind"`
	TaskID           string    `json:"task_id,omitempty"`
	AgentID          string    `json:"agent_id,omitempty"`
	Loop             int       `json:"loop,omitempty"`
	Tool             string    `json:"tool,omitempty"`
	Message          string    `json:"message,omitempty"` // Error / Reason / Description 首个非空
	Path             string    `json:"path,omitempty"`    // file_written 等文件事件
	DurationMS       int64     `json:"duration_ms,omitempty"`
	PromptTokens     int       `json:"prompt_tokens,omitempty"`
	CompletionTokens int       `json:"completion_tokens,omitempty"`
	At               time.Time `json:"at"`
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
	return TraceEvent{
		Kind:             string(ev.Kind),
		TaskID:           ev.TaskID,
		AgentID:          ev.AgentID,
		Loop:             ev.Loop,
		Tool:             ev.Tool,
		Message:          msg,
		Path:             ev.Path,
		DurationMS:       ev.DurationMS,
		PromptTokens:     ev.PromptTokens,
		CompletionTokens: ev.CompletionTokens,
		At:               at,
	}
}

// EmitTraceEvent 把一条 trace 事件包装为 KindTraceEvent 更新广播给全部订阅者。
//
// 速率安全：trace 含高频事件（llm_call_start/end、tool_call/result、
// token_stats）。本方法只做一次非阻塞扇出（与 Hub 其他广播同一
// drop-oldest 背压纪律），慢订阅者丢事件、快订阅者与 Hub 主循环均不受影响；
// 零订阅者时是纯 no-op。可在任意 goroutine 上并发调用（Reactor 分发路径如此）。
func (h *Hub) EmitTraceEvent(ev trace.Event) {
	projected := ProjectTraceEvent(ev)
	h.recordTrace(projected)
	h.broadcast(Update{
		Kind:  KindTraceEvent,
		Trace: projected,
		At:    time.Now(),
	})
}
