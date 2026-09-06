// Package contextruntime 实现 L2 的唯一运行服务：装配指令、任务、记忆、
// 有序历史与工具定义，封存完整 L1 Request，处理响应重放和输出订阅。
// 原子组/预算算法由 ContextCompiler 提供，读取与持久化依赖由 L3 注入。
// 本包不依赖 Agent、Graph 或 UI，不提供旧请求或历史转换入口。
package contextruntime
