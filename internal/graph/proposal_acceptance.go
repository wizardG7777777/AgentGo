package graph

import "context"

// ProposalAcceptanceInput 是独立规划验收器唯一可见的输入。它不包含 Scheduler
// 隐藏状态或 Runtime 写权限；验收器只能比较原请求、GraphContract 与规范化
// Definition candidate。
type ProposalAcceptanceInput struct {
	ProposalID         string
	GraphID            string
	DefinitionRevision int64
	RequestRef         string
	RequestDigest      string
	Contract           GraphContract
	ContractDigest     string
	Definition         GraphDefinitionBody
	DefinitionDigest   string
}

// ProposalAcceptanceDecision 是独立验收结果。Ref 必须稳定指向该次验收事实；
// pass 缺 Ref 会被 compiler fail-closed，避免“默认 true”成为自我批准后门。
type ProposalAcceptanceDecision struct {
	Verdict  ProposalAcceptanceVerdict
	Ref      string
	Issues   []ValidationIssue
	Warnings []ValidationIssue
}

// ProposalAcceptancePort 是 C2 compiler 对独立语义验收的最小端口。生产实现可
// 使用受 L2/L4 预算约束的 verifier；internal/graph 只依赖本接口，可用 fake
// 完全离线测试。
type ProposalAcceptancePort interface {
	EvaluateProposal(ctx context.Context, input ProposalAcceptanceInput) (ProposalAcceptanceDecision, error)
}

// DefinitionPolicyResolver 验证 Definition 引用的 L4 ProgressContract 与 L2
// ContextPolicy 是否真实存在。Compiler 只核对 ref，不解析下层 policy 内容。
type DefinitionPolicyResolver interface {
	HasProgressContract(ref string) bool
	HasContextPolicy(ref string) bool
}
