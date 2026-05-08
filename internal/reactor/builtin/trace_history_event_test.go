package builtin

import (
	"sync"
	"testing"

	"agentgo/internal/trace"
)

func TestTraceHistoryEventReactor_Defaults(t *testing.T) {
	r := NewTraceHistoryEventReactor()
	if r.Name() != "trace-history-event" {
		t.Errorf("name=%q", r.Name())
	}
	if r.IsSync() {
		t.Error("should be async")
	}
	if r.Priority() != 950 {
		t.Errorf("priority=%d want 950", r.Priority())
	}
	subs := r.Subscribe()
	if len(subs) != 2 {
		t.Fatalf("expected 2 kinds, got %d", len(subs))
	}
	wantKinds := map[trace.EventKind]bool{
		trace.KindHistoryCompaction: true,
		trace.KindHistoryTruncated:  true,
	}
	for _, k := range subs {
		if !wantKinds[k] {
			t.Errorf("unexpected kind %q", k)
		}
	}
}

func TestTraceHistoryEventReactor_CountsByKind(t *testing.T) {
	r := NewTraceHistoryEventReactor()
	r.Run(trace.Event{Kind: trace.KindHistoryCompaction})
	r.Run(trace.Event{Kind: trace.KindHistoryCompaction})
	r.Run(trace.Event{Kind: trace.KindHistoryTruncated})
	r.Run(trace.Event{Kind: trace.KindToolCall}) // 不订阅但被显式调用，应忽略不计

	if got := r.CompactionCount(); got != 2 {
		t.Errorf("CompactionCount=%d want 2", got)
	}
	if got := r.TruncationCount(); got != 1 {
		t.Errorf("TruncationCount=%d want 1", got)
	}
}

func TestTraceHistoryEventReactor_NeverErrors(t *testing.T) {
	r := NewTraceHistoryEventReactor()
	for _, k := range []trace.EventKind{
		trace.KindHistoryCompaction, trace.KindHistoryTruncated, trace.KindFileWritten,
	} {
		if err := r.Run(trace.Event{Kind: k}); err != nil {
			t.Errorf("Run kind=%q should not error, got %v", k, err)
		}
	}
}

func TestTraceHistoryEventReactor_ConcurrentCounts(t *testing.T) {
	// race detector 下应无 data race；atomic.Int64 保证计数安全
	r := NewTraceHistoryEventReactor()
	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Run(trace.Event{Kind: trace.KindHistoryCompaction})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				r.Run(trace.Event{Kind: trace.KindHistoryTruncated})
			}
		}()
	}
	wg.Wait()
	if got := r.CompactionCount(); got != int64(N*50) {
		t.Errorf("CompactionCount=%d want %d", got, N*50)
	}
	if got := r.TruncationCount(); got != int64(N*30) {
		t.Errorf("TruncationCount=%d want %d", got, N*30)
	}
}
