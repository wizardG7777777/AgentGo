package bootstrap

import (
	"context"
	"fmt"
	"sync"

	"agentgo/internal/llm"
	"agentgo/internal/trace"
)

// traceCaptureDispatcher 捕获测试期间的全部 trace 事件，供失败诊断。
type traceCaptureDispatcher struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *traceCaptureDispatcher) Dispatch(ev trace.Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

// planGateScriptedLLM 是集成测试的脚本化 LLM：按 responses 顺序返回，超出后
// 返回 "done" 文本响应（与 scheduler 集成测试的 mock 同型）。
type planGateScriptedLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	calls     int
	callLog   []string // 每次调用的诊断信息（消息数 / 工具数 / 末条消息摘要）
}

func (s *planGateScriptedLLM) Chat(_ context.Context, msgs []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := ""
	if len(msgs) > 0 {
		last = msgs[len(msgs)-1].Content
		if len(last) > 60 {
			last = last[:60]
		}
	}
	s.callLog = append(s.callLog, fmt.Sprintf("call#%d msgs=%d tools=%d last=%q", s.calls+1, len(msgs), len(tools), last))
	if s.calls < len(s.responses) {
		r := s.responses[s.calls]
		s.calls++
		return r, nil
	}
	s.calls++
	return llm.Response{Content: "done"}, nil
}
