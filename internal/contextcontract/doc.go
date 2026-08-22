// Package contextcontract 定义 L2 Context Engineering 的稳定领域契约。
//
// 本包只描述候选上下文、协议原子组、预算政策、wire 元数据与已封存快照，
// 不读取 Store、不执行工具、不调用模型，也不依赖具体 provider SDK。正文与
// provider 参数由 contextcompiler/llm adapter 持有；durable DTO 默认只保存
// digest、尺寸、处置和不透明引用。
package contextcontract
