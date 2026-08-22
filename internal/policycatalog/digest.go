package policycatalog

import (
	"agentgo/internal/contextcontract"
	"agentgo/internal/loopcontract"
)

// ProgressContractDigest 返回 CompiledProgressContract 的稳定语义 digest。
// ContractDigest 自身先清空，避免循环身份；其它有序 rule/signal 顺序保留。
func ProgressContractDigest(contract loopcontract.CompiledProgressContract) (string, error) {
	canonical := contract
	canonical.Ref.ContractDigest = ""
	return contextcontract.StableDigest("agentgo.progress-profile/v1", canonical)
}
