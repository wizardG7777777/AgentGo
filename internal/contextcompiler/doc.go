// Package contextcompiler 实现 L2 Context Engineering 的纯编译事务。
//
// 编译器保留完整 Fragment 与协议原子关系，仅校验整体模型容量、输出预留和
// wire 完整性；不执行局部限额、历史裁剪或正文引用替换。
// 生成运行时 payload 与不含正文的 ContextSnapshot。它不读取 Store、
// 不调用模型、不执行工具，也不修改 Graph。
package contextcompiler
