package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"time"

	"agentgo/internal/tui"
)

const (
	// sigSentinelDoubleWindow 是"双击 Ctrl+C"判定窗口：第 2 次 SIGINT 距
	// 第 1 次不超过该时长即视为强制退出意图；超过则重置计数，视为新的第 1 次。
	sigSentinelDoubleWindow = 3 * time.Second
	// sigSentinelShutdownTimeout 是优雅关闭的宽限期：ctx 取消后进程仍未退出
	// 超过该时长，判定 Shutdown 挂死（H5 前科：wg.Wait 等待不听 ctx 的
	// goroutine），强制退出。
	sigSentinelShutdownTimeout = 5 * time.Second
)

// sigSentinel 是 SIGINT 哨兵的核心状态机，与 os/signal 解耦以便单测。
// 退出函数、终端恢复回调、输出目标、headless 判定全部注入；真实运行只是
// signal.Notify + 薄适配（见 startSigSentinel）。
//
// 状态机：
//  1. 第 1 次 SIGINT：headless 打印提示并 cancel 走优雅关闭；TUI 模式不动
//     （bubbletea 会把 ctrl+c 译成按键交给 TUI 自己处理，哨兵不重复 cancel）。
//  2. 第 2 次 SIGINT 距第 1 次 ≤ 窗口，或此刻 ctx 已 Done（关闭进行中）
//     → 恢复终端 → 打印说明 → exit(130)。
//  3. 第 2 次距第 1 次 > 窗口 → 视为新的第 1 次（计数重置）。
//  4. ctx.Done 后武装关闭 deadline：到期进程仍活着 → exit(1)。
type sigSentinel struct {
	cancel     context.CancelFunc
	exitFn     func(code int) // 测试注入假 exit；真实为 os.Exit
	cleanupFn  func()         // 强杀前的终端恢复（tui.RunForceCleanup）；可为 nil
	stderr     io.Writer      // 提示输出目标
	isHeadless bool           // TUI 模式下第 1 次信号交给 bubbletea，哨兵不动

	mu        sync.Mutex
	lastSigAt time.Time // 上次"第 1 次"信号时间；zero 表示无在途计数
	ctxDone   bool      // onCtxDone 已触发（关闭流程进行中）
}

// onSignal 处理一次 SIGINT 送达；now 由调用方取，便于测试注入假时钟。
func (s *sigSentinel) onSignal(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 关闭流程进行中（或已卡死）：任何后续信号都立即强杀——用户显然等不及了。
	if s.ctxDone {
		s.forceExitLocked(130, "关闭进行中再次收到 Ctrl+C，强制退出")
		return
	}
	// 窗口内第 2 次 → 强杀。
	if !s.lastSigAt.IsZero() && now.Sub(s.lastSigAt) <= sigSentinelDoubleWindow {
		s.forceExitLocked(130, "再次收到 Ctrl+C，强制退出")
		return
	}
	// 窗口外的信号视为新的第 1 次（计数重置）。
	s.lastSigAt = now
	if s.isHeadless {
		// headless 没有 bubbletea 的 SIGINT handler——第 1 次信号必须由
		// 哨兵自己触发优雅关闭，否则操作系统默认动作直接杀进程。
		fmt.Fprintln(s.stderr, "[信号] 收到 Ctrl+C，正在优雅关闭（再按一次强制退出）")
		s.cancel()
	}
	// TUI 模式：bubbletea 已把 ctrl+c 译成按键送进 TUI（由 TUI 弹警告并
	// 触发优雅退出），哨兵只记录计数，不重复 cancel。
}

// onCtxDone 记录关闭流程开始：此后任何 SIGINT 立即强杀（见 onSignal）。
func (s *sigSentinel) onCtxDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctxDone = true
}

// onDeadline 在关闭宽限期到期后被调用：此刻进程仍存活意味着 Shutdown
// 挂死，恢复终端后 exit(1)。
func (s *sigSentinel) onDeadline() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forceExitLocked(1, "关闭超时，强制退出")
}

// forceExitLocked 强杀路径：尽力恢复终端 → 打印说明 → exit。
// 调用时必须持有 s.mu。exitFn 在真实运行中是 os.Exit（不返回）；
// 测试中假 exit 返回后状态不再可信，属预期。
func (s *sigSentinel) forceExitLocked(code int, msg string) {
	if s.cleanupFn != nil {
		s.cleanupFn()
	}
	fmt.Fprintf(s.stderr, "[信号] %s\n", msg)
	s.exitFn(code)
}

// startSigSentinel 启动 SIGINT 哨兵 goroutine：保证任何状态下（TUI 事件
// 循环死锁、Shutdown 挂死）Ctrl+C 都能终止进程。
//
// Go 的 signal.Notify 允许多个注册并存、各自收到一份拷贝，因此哨兵与
// bubbletea 的 SIGINT handler 互不干扰。只监听 os.Interrupt——SIGTERM 在
// Windows 上不存在，不引入 syscall。
//
// 两个 goroutine 均不加入 s.wg：它们随进程退出而消亡，若纳入 wg 反而会
// 让 Shutdown 的 wg.Wait 被哨兵自己挂死。
//
// shutdownDone 是 System.Shutdown 的完成信号：优雅关闭一旦真正完成就撤防
// deadline 定时器。生产路径下进程随后退出、定时器自然失效；但测试/嵌入
// 场景里 Shutdown 返回后宿主进程仍在运行（如 agent_template_runtime_test
// 在 Shutdown 后继续断言），不撤防会让 5s 定时器误杀整个宿主进程。
// Shutdown 挂死时 shutdownDone 永不关闭，定时器照常触发 exit(1)。
func startSigSentinel(ctx context.Context, cancel context.CancelFunc, isHeadless bool, shutdownDone <-chan struct{}) {
	s := &sigSentinel{
		cancel:     cancel,
		exitFn:     os.Exit,
		cleanupFn:  tui.RunForceCleanup,
		stderr:     os.Stderr,
		isHeadless: isHeadless,
	}

	// 监听 SIGINT：第 1 次优雅、窗口内第 2 次（或关闭进行中任意一次）强杀。
	// 缓冲 2 避免快速双击时第二次信号被丢。
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		for range sigCh {
			s.onSignal(time.Now())
		}
	}()

	// 监听关闭 deadline：ctx 取消后 5s 进程仍存活 → Shutdown 挂死，强杀。
	go func() {
		<-ctx.Done()
		s.onCtxDone()
		t := time.NewTimer(sigSentinelShutdownTimeout)
		defer t.Stop()
		select {
		case <-t.C:
			s.onDeadline()
		case <-shutdownDone:
			// 优雅关闭已在宽限期内完成，撤防（见函数 doc 的测试场景说明）。
		}
	}()
}
