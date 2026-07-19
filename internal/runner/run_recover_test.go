package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/agent"
)

// TestRunAgentLoopWithRecover_PanicRestartsLoop 验证 E5 核心语义：
// runOnce（agent.Run 的测试替身）第一次调用 panic 时，监督包装会
// recover、退避、并第二次调用 runOnce，而不是让 runner goroutine 静默死亡。
func TestRunAgentLoopWithRecover_PanicRestartsLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	secondCall := make(chan struct{})
	runOnce := func(runCtx context.Context) {
		n := calls.Add(1)
		if n == 1 {
			panic("模拟轮询循环 panic（QueryAvailable 等路径）")
		}
		// 第二次进入即证明重启发生；取消 ctx 让包装尽快退出。
		close(secondCall)
		cancel()
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentLoopWithRecover(ctx, "w-test", runOnce)
	}()

	select {
	case <-secondCall:
	case <-time.After(5 * time.Second):
		t.Fatal("panic 后 runOnce 未被第二次调用——监督包装没有重启")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后监督包装未退出（goroutine 泄漏）")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("runOnce 调用次数=%d，want 2（panic 1 次 + 重启 1 次）", got)
	}
}

// TestRunAgentLoopWithRecover_NormalReturnDoesNotRestart 验证正常返回不触发重启：
// ctx 取消或 IdleThreshold 空闲回收都是 runOnce 的正常返回路径，
// 若此时重启，空闲回收语义会被破坏（agent 永远退不出）。
func TestRunAgentLoopWithRecover_NormalReturnDoesNotRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentLoopWithRecover(ctx, "w-test", func(runCtx context.Context) {
			calls.Add(1)
			// 正常返回（等价于 ctx 取消 / 空闲回收导致的退出）
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce 正常返回后监督包装未退出")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("runOnce 调用次数=%d，want 1（正常返回不应重启）", got)
	}
}

// TestRunAgentLoopWithRecover_CancelDuringBackoffStops 验证 panic 后的 1s
// 退避窗口内 ctx 被取消时，包装不再重启、随 ctx 退出（无 goroutine 泄漏）。
func TestRunAgentLoopWithRecover_CancelDuringBackoffStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAgentLoopWithRecover(ctx, "w-test", func(runCtx context.Context) {
			calls.Add(1)
			cancel() // panic 前先取消——退避 select 应命中 ctx.Done()
			panic("模拟轮询循环 panic")
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 已取消但监督包装在退避窗口内未退出")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("runOnce 调用次数=%d，want 1（ctx 取消后不应再重启）", got)
	}
}

// TestRunnerRun_SurvivesAgentPanic 端到端验证 Runner.Run 经过监督包装：
// 构造一个 Store 为 nil 的 agent——其轮询循环第一轮 QueryAvailable 必然
// panic（nil 解引用）。若包装生效，Runner.Run 会 recover + 退避重启，
// 直到 ctx 取消才退出；若未生效，Run 会在第一次 panic 后直接崩溃测试进程。
func TestRunnerRun_SurvivesAgentPanic(t *testing.T) {
	ag := agent.NewAgent("w-nilstore", "evt", nil, nil, nil, 0)
	rn := &Runner{agent: ag}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rn.Run(ctx)
	}()

	// 留出足够时间跨过至少一个 1s 退避周期：若第一次 panic 就杀死了
	// goroutine（旧行为），done 会提前关闭——这里先断言它"还活着在退避"。
	select {
	case <-done:
		t.Fatal("Runner.Run 在 agent 首轮 panic 后直接退出——监督包装未生效")
	case <-time.After(1500 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Runner.Run 未退出（goroutine 泄漏）")
	}
}
