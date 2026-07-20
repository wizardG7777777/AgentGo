package tui

import "sync/atomic"

// forceCleanup 持有进程级"强杀前尽力恢复终端"回调。
// tea.Program 句柄只在 runWithIO 内部创建，外部（如 bootstrap 的 SIGINT
// 哨兵）拿不到；bubbletea 的 Program.Kill 会打断事件循环并恢复终端
// （退出 alt-screen、还原 termios），因此在创建 program 后注册到这里，
// 供哨兵在 os.Exit 前调用。atomic.Value 保证信号 goroutine 与 TUI
// goroutine 并发安全。
var forceCleanup atomic.Value // 存 func()

// RegisterForceCleanup 注册强杀前的终端恢复回调（后注册覆盖先注册）。
// 回调只服务于进程退出路径，进程消亡后注册点随之消失，无需注销。
func RegisterForceCleanup(f func()) {
	forceCleanup.Store(f)
}

// RunForceCleanup 执行已注册的终端恢复回调；未注册（headless 模式）
// 时为空操作。回调自身 panic 不向上传播——强杀路径必须走完 exit。
func RunForceCleanup() {
	v := forceCleanup.Load()
	if v == nil {
		return
	}
	f, ok := v.(func())
	if !ok || f == nil {
		return
	}
	defer func() { _ = recover() }()
	f()
}
