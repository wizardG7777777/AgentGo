package llm

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptrace"
	"sync"
	"time"
)

// InvocationTimingSnapshot 是单次 Model Invocation 的脱敏客户端时序里程碑。
// DNS/Connect/TLS 是各阶段耗时，其余 *MS 里程碑以调用开始时的 monotonic
// clock 为原点；nil 表示当前协议/连接没有暴露该字段，绝不能解释为 0ms。
type InvocationTimingSnapshot struct {
	DNSMS                 *int64
	ConnectMS             *int64
	TLSMS                 *int64
	FirstResponseByteMS   *int64
	FirstSSEEventMS       *int64
	FirstReasoningDeltaMS *int64
	FirstTextDeltaMS      *int64
	FirstToolDeltaMS      *int64
	FirstModelDeltaMS     *int64
	CompletedMS           *int64
	MaxInterEventGapMS    *int64
	StreamEventCount      int
	ConnectAttempts       int
	ConnectFailures       int
	NetworkFamily         string
	ConnectionReused      *bool
}

func (s InvocationTimingSnapshot) HasAny() bool {
	return s.DNSMS != nil || s.ConnectMS != nil || s.TLSMS != nil ||
		s.FirstResponseByteMS != nil || s.FirstSSEEventMS != nil ||
		s.FirstModelDeltaMS != nil || s.CompletedMS != nil ||
		s.StreamEventCount > 0 || s.ConnectAttempts > 0 || s.NetworkFamily != "" ||
		s.ConnectionReused != nil
}

// InvocationTiming 在 transport/SSE 回调中累计时序；Snapshot 是唯一读取出口。
// 它不依赖 trace 包，避免 LLM transport 反向依赖持久化实现。
type InvocationTiming struct {
	mu      sync.Mutex
	started time.Time

	dnsStarted     time.Time
	connectStarted time.Time
	tlsStarted     time.Time
	lastSSEEvent   time.Time

	dnsMS                 *int64
	connectMS             *int64
	tlsMS                 *int64
	firstResponseByteMS   *int64
	firstSSEEventMS       *int64
	firstReasoningDeltaMS *int64
	firstTextDeltaMS      *int64
	firstToolDeltaMS      *int64
	firstModelDeltaMS     *int64
	completedMS           *int64
	maxInterEventGapMS    *int64
	streamEventCount      int
	connectAttempts       int
	connectFailures       int
	networkFamily         string
	connectionReused      *bool
}

func NewInvocationTiming(started time.Time) *InvocationTiming {
	if started.IsZero() {
		started = time.Now()
	}
	return &InvocationTiming{started: started}
}

type invocationTimingContextKey struct{}

// WithInvocationTiming 同时安装 accumulator 与 net/http/httptrace。openai-go 使用
// http.DefaultClient，Request context 会原样传入 transport，因此无需替换客户端。
func WithInvocationTiming(ctx context.Context, timing *InvocationTiming) context.Context {
	if timing == nil {
		return ctx
	}
	ctx = context.WithValue(ctx, invocationTimingContextKey{}, timing)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			timing.mu.Lock()
			if timing.dnsStarted.IsZero() {
				timing.dnsStarted = time.Now()
			}
			timing.mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			timing.mu.Lock()
			if timing.dnsMS == nil && !timing.dnsStarted.IsZero() {
				timing.dnsMS = durationMS(time.Since(timing.dnsStarted))
			}
			timing.mu.Unlock()
		},
		ConnectStart: func(_, _ string) {
			timing.mu.Lock()
			timing.connectAttempts++
			if timing.connectStarted.IsZero() {
				timing.connectStarted = time.Now()
			}
			timing.mu.Unlock()
		},
		ConnectDone: func(_, _ string, err error) {
			timing.mu.Lock()
			if err != nil {
				timing.connectFailures++
			} else if timing.connectMS == nil && !timing.connectStarted.IsZero() {
				timing.connectMS = durationMS(time.Since(timing.connectStarted))
			}
			timing.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			timing.mu.Lock()
			if timing.tlsStarted.IsZero() {
				timing.tlsStarted = time.Now()
			}
			timing.mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			timing.mu.Lock()
			if err == nil && timing.tlsMS == nil && !timing.tlsStarted.IsZero() {
				timing.tlsMS = durationMS(time.Since(timing.tlsStarted))
			}
			timing.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			timing.mu.Lock()
			if timing.connectionReused == nil {
				value := info.Reused
				timing.connectionReused = &value
			}
			if timing.networkFamily == "" {
				timing.networkFamily = connectionFamily(info.Conn)
			}
			timing.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			timing.mu.Lock()
			timing.setElapsedOnce(&timing.firstResponseByteMS, time.Now())
			timing.mu.Unlock()
		},
	})
}

func timingFromContext(ctx context.Context) *InvocationTiming {
	timing, _ := ctx.Value(invocationTimingContextKey{}).(*InvocationTiming)
	return timing
}

// observeStreamEvent 记录服务端事件边界；response.created 只是首 SSE，不是 TTFT。
func observeStreamEvent(ctx context.Context, eventType string) {
	if timing := timingFromContext(ctx); timing != nil {
		timing.observeEvent(eventType, time.Now())
	}
}

func observeStreamDelta(ctx context.Context, kind string, nonEmpty bool) {
	if !nonEmpty {
		return
	}
	if timing := timingFromContext(ctx); timing != nil {
		timing.observeDelta(kind, time.Now())
	}
}

func markStreamCompleted(ctx context.Context) {
	if timing := timingFromContext(ctx); timing != nil {
		timing.mu.Lock()
		timing.setElapsedOnce(&timing.completedMS, time.Now())
		timing.mu.Unlock()
	}
}

func (t *InvocationTiming) observeEvent(eventType string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streamEventCount++
	t.setElapsedOnce(&t.firstSSEEventMS, now)
	if !t.lastSSEEvent.IsZero() {
		gap := now.Sub(t.lastSSEEvent).Milliseconds()
		if gap < 0 {
			gap = 0
		}
		if t.maxInterEventGapMS == nil || gap > *t.maxInterEventGapMS {
			value := gap
			t.maxInterEventGapMS = &value
		}
	}
	t.lastSSEEvent = now
	if eventType == "response.completed" {
		t.setElapsedOnce(&t.completedMS, now)
	}
}

func (t *InvocationTiming) observeDelta(kind string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch kind {
	case "reasoning":
		t.setElapsedOnce(&t.firstReasoningDeltaMS, now)
	case "text":
		t.setElapsedOnce(&t.firstTextDeltaMS, now)
	case "tool":
		t.setElapsedOnce(&t.firstToolDeltaMS, now)
	}
	t.setElapsedOnce(&t.firstModelDeltaMS, now)
}

func (t *InvocationTiming) setElapsedOnce(target **int64, now time.Time) {
	if *target != nil {
		return
	}
	value := now.Sub(t.started).Milliseconds()
	if value < 0 {
		value = 0
	}
	*target = &value
}

func (t *InvocationTiming) Snapshot() InvocationTimingSnapshot {
	if t == nil {
		return InvocationTimingSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return InvocationTimingSnapshot{
		DNSMS: cloneInt64(t.dnsMS), ConnectMS: cloneInt64(t.connectMS), TLSMS: cloneInt64(t.tlsMS),
		FirstResponseByteMS: cloneInt64(t.firstResponseByteMS), FirstSSEEventMS: cloneInt64(t.firstSSEEventMS),
		FirstReasoningDeltaMS: cloneInt64(t.firstReasoningDeltaMS), FirstTextDeltaMS: cloneInt64(t.firstTextDeltaMS),
		FirstToolDeltaMS: cloneInt64(t.firstToolDeltaMS), FirstModelDeltaMS: cloneInt64(t.firstModelDeltaMS),
		CompletedMS: cloneInt64(t.completedMS), MaxInterEventGapMS: cloneInt64(t.maxInterEventGapMS),
		StreamEventCount: t.streamEventCount, ConnectAttempts: t.connectAttempts, ConnectFailures: t.connectFailures,
		NetworkFamily: t.networkFamily, ConnectionReused: cloneBool(t.connectionReused),
	}
}

func durationMS(value time.Duration) *int64 {
	ms := value.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func connectionFamily(conn net.Conn) string {
	if conn == nil {
		return ""
	}
	address, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || address.IP == nil {
		return ""
	}
	if address.IP.To4() != nil {
		return "ipv4"
	}
	return "ipv6"
}
