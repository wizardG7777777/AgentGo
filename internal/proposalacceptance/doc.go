// Package proposalacceptance 实现 Graph DefinitionCompiler 的独立语义验收端口。
//
// Verifier 使用独立 LLM client、固定 system prompt、空工具快照和 L2 Context
// 编译链。Scheduler 只能提交 Draft，不能调用本包伪造 pass，也不能控制 verifier
// prompt 或最终 acceptance ref。
package proposalacceptance
