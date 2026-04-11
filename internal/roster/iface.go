package roster

import "agentgo/internal/model"

type Roster interface {
	TryClaim(agentID string, filePath string) (bool, error)
	Release(agentID string, filePath string) error
	ReleaseAll(agentID string) error
	IsOccupied(filePath string) (occupiedBy string, occupied bool, err error)
	ListByAgent(agentID string) ([]model.Claim, error)
	// ListAllAgents 返回花名册中所有持有声明的代理 ID 列表。
	ListAllAgents() ([]string, error)
	// ListClaims 返回当前所有活跃的文件占用映射（agentID → filePath 列表）。
	// 快照语义——返回的 map 是调用时刻的拷贝，调用方修改不影响 Roster 内部状态。
	// 供 Agent Hook 的 FileAwareness section 查询团队文件占用状态。
	// 参见 hookview.go 中 RosterHookView 接口与 MemoryRoster 的实现。
	ListClaims() map[string][]string
}
