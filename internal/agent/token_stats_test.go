package agent

import (
	"sync"
	"testing"
)

// TestTokenStats_ConcurrentAddAndSnapshot 验证 A3 修复：
// ReAct 循环（写）与 UI 轮询（读）并发访问 TokenStats 不再构成数据竞争，
// 且最终累计值精确。需配合 `go test -race` 发挥完整效力。
func TestTokenStats_ConcurrentAddAndSnapshot(t *testing.T) {
	a := &Agent{ID: "test-agent"}

	const writers = 8
	const addsPerWriter = 500

	var writeWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writeWG.Add(1)
		go func() {
			defer writeWG.Done()
			for i := 0; i < addsPerWriter; i++ {
				a.AddTokenStats(2, 3)
			}
		}()
	}
	// 读方并发取快照：任何时刻快照内部必须自洽（CallCount 与总量同源）。
	stop := make(chan struct{})
	var readWG sync.WaitGroup
	readWG.Add(1)
	go func() {
		defer readWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				snap := a.TokenStatsSnapshot()
				if snap.TotalPromptTokens != int64(snap.CallCount)*2 {
					t.Errorf("快照不自洽: prompt=%d callCount=%d", snap.TotalPromptTokens, snap.CallCount)
					return
				}
				if snap.TotalCompletionTokens != int64(snap.CallCount)*3 {
					t.Errorf("快照不自洽: completion=%d callCount=%d", snap.TotalCompletionTokens, snap.CallCount)
					return
				}
			}
		}
	}()

	writeWG.Wait() // 等写方全部完成
	close(stop)
	readWG.Wait()

	got := a.TokenStatsSnapshot()
	wantCalls := writers * addsPerWriter
	if got.CallCount != wantCalls {
		t.Fatalf("CallCount = %d, want %d", got.CallCount, wantCalls)
	}
	if got.TotalPromptTokens != int64(wantCalls)*2 {
		t.Fatalf("TotalPromptTokens = %d, want %d", got.TotalPromptTokens, wantCalls*2)
	}
	if got.TotalCompletionTokens != int64(wantCalls)*3 {
		t.Fatalf("TotalCompletionTokens = %d, want %d", got.TotalCompletionTokens, wantCalls*3)
	}
}

// TestAddTokenStats_ReturnsUpdatedSnapshot 验证返回值即为累加后快照，
// 供写方 goroutine 复用（trace 事件），无需再次取锁。
func TestAddTokenStats_ReturnsUpdatedSnapshot(t *testing.T) {
	a := &Agent{ID: "test-agent"}

	s1 := a.AddTokenStats(10, 5)
	if s1.TotalPromptTokens != 10 || s1.TotalCompletionTokens != 5 || s1.CallCount != 1 {
		t.Fatalf("首次累加快照错误: %+v", s1)
	}
	s2 := a.AddTokenStats(7, 3)
	if s2.TotalPromptTokens != 17 || s2.TotalCompletionTokens != 8 || s2.CallCount != 2 {
		t.Fatalf("第二次累加快照错误: %+v", s2)
	}
	if got := a.TokenStatsSnapshot(); got != s2 {
		t.Fatalf("TokenStatsSnapshot() = %+v, want %+v", got, s2)
	}
}
