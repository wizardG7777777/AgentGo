package reactor

import (
	"runtime"
	"strings"
)

// IsRunningOnReactorGoroutine 报告当前 goroutine 是否正在执行某个 Reactor.Run
// （即栈中有 *Registry.runSync 或 *Registry.runAsync 帧）。
//
// 用途（v5 Phase 5）：agent.SetState 调用此函数判断是否被 Reactor 路径触发，
// 命中时 panic——执行 ReactiveSystem.md §7.2.6 原则 4 "Reactor 不得驱动状态机"。
//
// Phase 4 实施时该约束以"接口注释 + code review"形式存在；Phase 5 用户 YAML
// Reactor 落地后，用户提供任意 Go 实现都可能踩到这条线，必须有运行期 guard。
//
// 实现：runtime.Callers 走栈，匹配 *Registry.runSync / runAsync 的函数名。
// SetState 不在热路径上，walk 成本（典型 10-30 帧）可接受。
//
// 误报边界：用户 Reactor 通过 goroutine.Go(...) 启动子 goroutine 后再调 SetState
// 的话，子 goroutine 栈不包含 runSync/runAsync 帧，本函数会返回 false——guard 不会拦。
// 这种"逃逸"路径属于明显的"绕过 reactor 边界"模式，code review 仍需关注。
func IsRunningOnReactorGoroutine() bool {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs) // 跳过 Callers 自身 + IsRunningOnReactorGoroutine 自身
	if n == 0 {
		return false
	}
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		// 仅匹配 runSync / runAsync——这是 Registry 中唯二调用 Reactor.Run 的方法。
		// 不绑定 module path（当前为 agentgo），避免未来模块改名后 guard 失效。
		if strings.Contains(frame.Function, "internal/reactor.(*Registry).runSync") ||
			strings.Contains(frame.Function, "internal/reactor.(*Registry).runAsync") {
			return true
		}
		if !more {
			break
		}
	}
	return false
}
