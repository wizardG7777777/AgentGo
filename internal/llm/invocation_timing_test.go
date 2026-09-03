package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInvocationTimingRecordsFirstMilestonesAndLargestGap(t *testing.T) {
	start := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	timing := NewInvocationTiming(start)

	timing.observeEvent("response.created", start.Add(10*time.Millisecond))
	timing.observeDelta("reasoning", start.Add(20*time.Millisecond))
	timing.observeDelta("tool", start.Add(25*time.Millisecond))
	timing.observeDelta("text", start.Add(30*time.Millisecond))
	timing.observeEvent("response.output_item.done", start.Add(45*time.Millisecond))
	timing.observeEvent("response.completed", start.Add(80*time.Millisecond))

	got := timing.Snapshot()
	assertTimingMS(t, "first_sse_event_ms", got.FirstSSEEventMS, 10)
	assertTimingMS(t, "first_reasoning_delta_ms", got.FirstReasoningDeltaMS, 20)
	assertTimingMS(t, "first_tool_delta_ms", got.FirstToolDeltaMS, 25)
	assertTimingMS(t, "first_text_delta_ms", got.FirstTextDeltaMS, 30)
	assertTimingMS(t, "first_model_delta_ms", got.FirstModelDeltaMS, 20)
	assertTimingMS(t, "completed_ms", got.CompletedMS, 80)
	assertTimingMS(t, "max_inter_event_gap_ms", got.MaxInterEventGapMS, 35)
	if got.StreamEventCount != 3 {
		t.Fatalf("stream_event_count=%d, want 3", got.StreamEventCount)
	}
	if got.DNSMS != nil || got.ConnectMS != nil || got.TLSMS != nil || got.FirstResponseByteMS != nil {
		t.Fatalf("未执行 transport 时不应伪造网络里程碑: %+v", got)
	}

	// Snapshot 必须深拷贝指针，调用方不能反向修改 accumulator。
	*got.FirstModelDeltaMS = 999
	assertTimingMS(t, "snapshot copy", timing.Snapshot().FirstModelDeltaMS, 20)
}

func TestInvocationTimingHTTPTraceDistinguishesNewAndReusedConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	client := server.Client()

	request := func() InvocationTimingSnapshot {
		t.Helper()
		timing := NewInvocationTiming(time.Now())
		req, err := http.NewRequestWithContext(WithInvocationTiming(context.Background(), timing), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			_ = response.Body.Close()
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return timing.Snapshot()
	}

	first := request()
	if first.FirstResponseByteMS == nil || first.ConnectMS == nil {
		t.Fatalf("新连接缺少 transport 里程碑: %+v", first)
	}
	if first.ConnectAttempts != 1 || first.ConnectFailures != 0 || first.NetworkFamily != "ipv4" {
		t.Fatalf("新连接指标异常: %+v", first)
	}
	if first.ConnectionReused == nil || *first.ConnectionReused {
		t.Fatalf("首次连接不应标为复用: %+v", first)
	}
	if first.DNSMS != nil || first.TLSMS != nil {
		t.Fatalf("IP 明文 HTTP 不应伪造 DNS/TLS: %+v", first)
	}

	second := request()
	if second.FirstResponseByteMS == nil || second.ConnectionReused == nil || !*second.ConnectionReused {
		t.Fatalf("第二次请求应复用连接并保留首字节里程碑: %+v", second)
	}
	if second.ConnectMS != nil || second.ConnectAttempts != 0 {
		t.Fatalf("复用连接不应伪造 connect 阶段: %+v", second)
	}
}

func TestInvocationTimingEmptySnapshotKeepsUnavailableMilestonesAbsent(t *testing.T) {
	got := NewInvocationTiming(time.Now()).Snapshot()
	if got.HasAny() {
		t.Fatalf("空 accumulator 不应产生可观测时序: %+v", got)
	}
	if (*InvocationTiming)(nil).Snapshot().HasAny() {
		t.Fatal("nil accumulator 不应产生可观测时序")
	}
}

func assertTimingMS(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s=%v, want %d", name, got, want)
	}
}
