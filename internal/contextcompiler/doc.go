// Package contextcompiler 实现 L2 Context Engineering 的纯编译事务。
//
// 编译器消费已冻结、已准备表现形式的 Fragment/AtomicGroup/Policy，按单项、
// 原子组、section、snapshot total、completion reserve 和绝对 wire bytes 顺序
// 执法，生成运行时 payload 与不含正文的 ContextSnapshot。它不读取 Store、
// 不调用模型、不执行工具，也不修改 Graph。
package contextcompiler
