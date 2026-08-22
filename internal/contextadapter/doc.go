// Package contextadapter 把当前 AgentGo 的 llm.Message/ToolDef 语义映射到 L2
// ContextFragment/ProtocolAtomicGroup，再交给 contextcompiler 生成唯一的
// WireItem、Manifest 与 ContextSnapshot。
//
// 本包不 import agent，避免未来 Agent cutover 时形成依赖环。现有 HistoryEntry、
// ToolRouterSnapshot 只需在生产接线点映射成这里的中性 DTO。
package contextadapter
