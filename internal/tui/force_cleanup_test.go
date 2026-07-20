package tui

import "testing"

// 未注册时 RunForceCleanup 是空操作（headless 场景）。
func TestRunForceCleanup_UnregisteredNoOp(t *testing.T) {
	RunForceCleanup()
}

// 注册后 RunForceCleanup 执行回调一次。
func TestRunForceCleanup_InvokesRegistered(t *testing.T) {
	called := 0
	RegisterForceCleanup(func() { called++ })
	defer RegisterForceCleanup(func() {}) // 复位，避免污染同包其他测试

	RunForceCleanup()
	if called != 1 {
		t.Fatalf("已注册回调应执行 1 次，实际 %d 次", called)
	}
}

// 回调 panic 不向上传播——强杀路径必须走完 exit。
func TestRunForceCleanup_PanicContained(t *testing.T) {
	RegisterForceCleanup(func() { panic("boom") })
	defer RegisterForceCleanup(func() {})

	RunForceCleanup() // 不应 panic
}
