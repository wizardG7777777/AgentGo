package bootstrap

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// sentinelFixture 组装一个全部依赖注入的 sigSentinel，记录 cancel/exit/cleanup
// 的调用情况供断言。测试不发真实信号——直接驱动 onSignal / onCtxDone /
// onDeadline 状态机入口。
type sentinelFixture struct {
	s         *sigSentinel
	cancelled int
	exitCodes []int
	cleanups  int
	stderr    bytes.Buffer
}

func newSentinelFixture(isHeadless bool) *sentinelFixture {
	f := &sentinelFixture{}
	f.s = &sigSentinel{
		cancel: func() { f.cancelled++ },
		exitFn: func(code int) { f.exitCodes = append(f.exitCodes, code) },
		cleanupFn: func() {
			f.cleanups++
		},
		stderr:     &f.stderr,
		isHeadless: isHeadless,
	}
	return f
}

var sentinelT0 = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// headless 首次信号：打印提示 + 触发优雅关闭，不 exit。
func TestSigSentinel_FirstSignalHeadlessCancels(t *testing.T) {
	f := newSentinelFixture(true)
	f.s.onSignal(sentinelT0)

	if f.cancelled != 1 {
		t.Fatalf("headless 首次信号应触发 cancel，实际 %d 次", f.cancelled)
	}
	if len(f.exitCodes) != 0 {
		t.Fatalf("首次信号不应 exit，实际 %v", f.exitCodes)
	}
	if !strings.Contains(f.stderr.String(), "正在优雅关闭") {
		t.Fatalf("headless 首次信号应打印优雅关闭提示，实际 %q", f.stderr.String())
	}
}

// TUI 模式首次信号：交给 bubbletea 处理，哨兵只计数——不 cancel、不 exit、不打印。
func TestSigSentinel_FirstSignalTUINoOp(t *testing.T) {
	f := newSentinelFixture(false)
	f.s.onSignal(sentinelT0)

	if f.cancelled != 0 {
		t.Fatalf("TUI 模式首次信号不应触发 cancel，实际 %d 次", f.cancelled)
	}
	if len(f.exitCodes) != 0 {
		t.Fatalf("TUI 模式首次信号不应 exit，实际 %v", f.exitCodes)
	}
	if f.stderr.Len() != 0 {
		t.Fatalf("TUI 模式首次信号不应打印，实际 %q", f.stderr.String())
	}
}

// 窗口内第 2 次信号：恢复终端 + exit(130)。
func TestSigSentinel_DoubleSignalInWindowForceExits(t *testing.T) {
	f := newSentinelFixture(false)
	f.s.onSignal(sentinelT0)
	f.s.onSignal(sentinelT0.Add(2 * time.Second))

	if len(f.exitCodes) != 1 || f.exitCodes[0] != 130 {
		t.Fatalf("窗口内第 2 次信号应 exit(130)，实际 %v", f.exitCodes)
	}
	if f.cleanups != 1 {
		t.Fatalf("强杀前应调用 1 次终端恢复，实际 %d 次", f.cleanups)
	}
}

// 窗口边界：恰好 3 秒整仍属窗口内（≤ 判定）。
func TestSigSentinel_DoubleSignalAtWindowEdgeForceExits(t *testing.T) {
	f := newSentinelFixture(false)
	f.s.onSignal(sentinelT0)
	f.s.onSignal(sentinelT0.Add(sigSentinelDoubleWindow))

	if len(f.exitCodes) != 1 || f.exitCodes[0] != 130 {
		t.Fatalf("窗口边界的第 2 次信号应 exit(130)，实际 %v", f.exitCodes)
	}
}

// 窗口外第 2 次信号视为新的第 1 次（计数重置）：headless 下再次 cancel、
// 不 exit；其后窗口内第 3 次才强杀。
func TestSigSentinel_SignalOutsideWindowResetsCount(t *testing.T) {
	f := newSentinelFixture(true)
	f.s.onSignal(sentinelT0)
	f.s.onSignal(sentinelT0.Add(4 * time.Second))

	if len(f.exitCodes) != 0 {
		t.Fatalf("窗口外第 2 次信号不应 exit（应重置计数），实际 %v", f.exitCodes)
	}
	if f.cancelled != 2 {
		t.Fatalf("窗口外第 2 次信号应视为新的第 1 次并 cancel，实际 cancel %d 次", f.cancelled)
	}

	f.s.onSignal(sentinelT0.Add(5 * time.Second))
	if len(f.exitCodes) != 1 || f.exitCodes[0] != 130 {
		t.Fatalf("重置后窗口内再按一次应 exit(130)，实际 %v", f.exitCodes)
	}
}

// ctx 已 Done（关闭流程进行中）：任意信号立即强杀，即使距上次信号已超窗口。
func TestSigSentinel_SignalAfterCtxDoneForceExitsImmediately(t *testing.T) {
	f := newSentinelFixture(true)
	f.s.onSignal(sentinelT0)
	f.s.onCtxDone()
	f.s.onSignal(sentinelT0.Add(10 * time.Second))

	if len(f.exitCodes) != 1 || f.exitCodes[0] != 130 {
		t.Fatalf("ctx Done 后信号应立即 exit(130)，实际 %v", f.exitCodes)
	}
	if f.cleanups != 1 {
		t.Fatalf("强杀前应调用 1 次终端恢复，实际 %d 次", f.cleanups)
	}
}

// 关闭 deadline 到期进程仍存活（Shutdown 挂死）：恢复终端 + exit(1)。
func TestSigSentinel_DeadlineForceExits(t *testing.T) {
	f := newSentinelFixture(true)
	f.s.onCtxDone()
	f.s.onDeadline()

	if len(f.exitCodes) != 1 || f.exitCodes[0] != 1 {
		t.Fatalf("关闭 deadline 到期应 exit(1)，实际 %v", f.exitCodes)
	}
	if f.cleanups != 1 {
		t.Fatalf("deadline 强杀前应调用 1 次终端恢复，实际 %d 次", f.cleanups)
	}
	if !strings.Contains(f.stderr.String(), "关闭超时，强制退出") {
		t.Fatalf("deadline 强杀应打印超时说明，实际 %q", f.stderr.String())
	}
}

// cleanupFn 为 nil（headless 未注册终端恢复）时强杀路径不 panic。
func TestSigSentinel_NilCleanupSafe(t *testing.T) {
	f := newSentinelFixture(true)
	f.s.cleanupFn = nil
	f.s.onSignal(sentinelT0)
	f.s.onSignal(sentinelT0.Add(time.Second))

	if len(f.exitCodes) != 1 || f.exitCodes[0] != 130 {
		t.Fatalf("nil cleanup 下窗口内第 2 次信号仍应 exit(130)，实际 %v", f.exitCodes)
	}
}
