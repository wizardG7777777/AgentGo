package roster

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/session"
)

func TestReleaseAllClaims(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/file1.go")
	r.TryClaim("agent-2", "/file2.go")
	r.TryClaim("agent-1", "/file3.go")

	r.ReleaseAllClaims()

	// 多个 agent 的占用全部空闲
	for _, fp := range []string{"/file1.go", "/file2.go", "/file3.go"} {
		if _, occupied, _ := r.IsOccupied(fp); occupied {
			t.Errorf("%s 在 ReleaseAllClaims 后应为空闲", fp)
		}
	}
	if agents, _ := r.ListAllAgents(); len(agents) != 0 {
		t.Errorf("ReleaseAllClaims 后 ListAllAgents 应为空，实际 %v", agents)
	}
	// 清空后可重新占用
	if ok, err := r.TryClaim("agent-3", "/file1.go"); err != nil || !ok {
		t.Error("ReleaseAllClaims 后应可重新 TryClaim")
	}
}

func TestReleaseAllClaims_Empty(t *testing.T) {
	r := NewMemoryRoster()
	em := &mockEmitter{}
	r.SetHistoryEmitter(em)

	r.ReleaseAllClaims() // 空 Roster 上调用不应 panic、不应 emit

	if events := em.Events(); len(events) != 0 {
		t.Errorf("空 Roster 的 ReleaseAllClaims 不应 emit 事件，实际 %d 条", len(events))
	}
}

func TestReleaseAllClaims_WakesWaiters(t *testing.T) {
	r := NewMemoryRoster()
	r.TryClaim("agent-1", "/file.go")

	done := make(chan error, 1)
	go func() {
		done <- r.WaitForRelease(context.Background(), "agent-2", "/file.go", 5*time.Second)
	}()

	// 等 waiter 确定性注册进等待队列（同包测试可直接观察内部簿记）
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.RLock()
		n := len(r.waiters["/file.go"])
		r.mu.RUnlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter 未在期限内注册进等待队列")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r.ReleaseAllClaims()

	// 等待中的 WaitForRelease 必须收到释放信号，不得挂到超时（waiter 泄漏）
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WaitForRelease 应在清空后返回 nil，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReleaseAllClaims 未唤醒等待中的 WaitForRelease（waiter 泄漏）")
	}

	// waiters 簿记清空
	r.mu.RLock()
	remaining := len(r.waiters)
	r.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("ReleaseAllClaims 后 waiters 应为空，实际 %d", remaining)
	}

	// 按 WaitForRelease 约定，唤醒后重试 TryClaim 应成功
	if ok, _ := r.TryClaim("agent-2", "/file.go"); !ok {
		t.Error("唤醒后 agent-2 应能 TryClaim 成功")
	}
}

func TestReleaseAllClaims_EmitsHistory(t *testing.T) {
	r := NewMemoryRoster()
	em := &mockEmitter{}
	r.SetHistoryEmitter(em)

	r.TryClaim("agent-1", "/file1.go")
	r.TryClaim("agent-2", "/file2.go")
	r.ReleaseAllClaims()

	// 与 ReleaseAll 同惯例：每个被释放的 claim 一条 roster_release
	releases := 0
	for _, ev := range em.Events() {
		if ev.EventType == session.HistEventRosterRelease {
			releases++
		}
	}
	if releases != 2 {
		t.Errorf("ReleaseAllClaims 应为每个 claim emit 一条 roster_release，实际 %d 条，期望 2", releases)
	}
}
