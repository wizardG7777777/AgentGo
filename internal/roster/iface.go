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
}
