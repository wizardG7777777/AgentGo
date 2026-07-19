package store

import (
	"context"
	"sync"
	"testing"

	"agentgo/internal/model"
)

// TestSetCancelRegistry_ConcurrentWithTerminalTransitions 覆盖 F10：
// setter 现在与所有 cancelRegistry 读取点处于同一同步域（s.mu）。
// 一个 goroutine 反复注入 registry，若干 goroutine 并发执行
// publish → claim → 终态转换（终态路径在 s.mu 下读 cancelRegistry）；
// 旧实现下这是未同步读写竞态。最后验证注入确实生效。
func TestSetCancelRegistry_ConcurrentWithTerminalTransitions(t *testing.T) {
	s, _ := newTestStore(4096, 1000)
	reg := NewTaskCancelRegistry()

	done := make(chan struct{})
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for {
			select {
			case <-done:
				return
			default:
				s.SetCancelRegistry(reg)
			}
		}
	}()

	var readerWG sync.WaitGroup
	for i := 0; i < 64; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			task := &model.Task{Description: "concurrent set/get"}
			if err := s.PublishTask(task); err != nil {
				t.Errorf("PublishTask: %v", err)
				return
			}
			if err := s.ClaimTask("agent-1", task.ID); err != nil {
				t.Errorf("ClaimTask: %v", err)
				return
			}
			if err := s.TransitionStateWithCancelSource(
				task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "user"); err != nil {
				t.Errorf("TransitionStateWithCancelSource: %v", err)
			}
		}()
	}
	readerWG.Wait()
	close(done)
	writerWG.Wait()

	// 注入生效验证：registry 已就位后，取消任务应取消 GetOrCreate 派生的 ctx。
	s.SetCancelRegistry(reg)
	task := &model.Task{Description: "set took effect"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	taskCtx := reg.GetOrCreate(context.Background(), task.ID)
	if err := s.TransitionStateWithCancelSource(
		task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "user"); err != nil {
		t.Fatalf("TransitionStateWithCancelSource: %v", err)
	}
	select {
	case <-taskCtx.Done():
	default:
		t.Error("任务取消后 registry 派生的 ctx 未被取消——setter 未生效")
	}
	if src := reg.Source(task.ID); src != "user" {
		t.Errorf("取消来源=%q，want user", src)
	}
}
