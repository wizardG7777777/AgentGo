package graph

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQueuedGraphRequestCancelsWithoutWaitingForCurrentRequest(t *testing.T) {
	s, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		_ = s.WithRequest(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	defer func() { close(release); <-finished }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.WithRequest(ctx, func() error { t.Error("已取消请求不应修改图"); return nil })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("应返回取消：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("已取消请求仍等待上一请求")
	}
}
