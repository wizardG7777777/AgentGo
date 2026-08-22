// Package policycatalog 提供五层架构共享的版本化 framework policy catalog。
//
// Catalog 是只读权威：Scheduler/Prompt 只能引用已登记的 Context/Progress
// profile，不能在运行时扩大 hard cap、关闭 checkpoint 或发明新进展信号。
// Graph 仅通过 HasProgressContract/HasContextPolicy 核对稳定引用，不依赖下层实现。
package policycatalog
