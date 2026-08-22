// Package contentstore 实现 L3 Harness 的大型正文持久化与授权解引用。
//
// ContentRef 只是内容身份与谱系，不授予读取权限。Resolve 必须同时通过机械
// scope 校验、非空 ExecutionLeaseRef 与调用方注入的显式授权回调。Store 不参与
// L2 的摘要/选择策略，也不把内容升级为 Artifact/Result/Evidence。
package contentstore
