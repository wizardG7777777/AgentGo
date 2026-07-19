package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWaitReady_ServerUp 服务可达时返回 true。
func TestWaitReady_ServerUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if !WaitReady(context.Background(), srv.URL+"/", 2*time.Second) {
		t.Fatal("healthz 可达应返回 true")
	}
}

// TestWaitReady_Timeout 服务不可达时按超时返回 false。
func TestWaitReady_Timeout(t *testing.T) {
	start := time.Now()
	if WaitReady(context.Background(), "http://127.0.0.1:1/", 300*time.Millisecond) {
		t.Fatal("不可达地址应返回 false")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("WaitReady 未按超时返回, elapsed=%v", elapsed)
	}
}

// TestWaitReady_CtxCancel ctx 取消时立即返回 false。
func TestWaitReady_CtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- WaitReady(ctx, "http://127.0.0.1:1/", 10*time.Second) }()
	cancel()
	select {
	case got := <-done:
		if got {
			t.Fatal("ctx 取消应返回 false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 WaitReady 未及时返回")
	}
}
