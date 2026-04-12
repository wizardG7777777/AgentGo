package roster

import (
	"context"
	"time"

	"agentgo/internal/model"
)

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
	// WaitForRelease 阻塞等待 filePath 被当前持有者释放（FIFO 排队）。
	// 返回 nil 表示文件已释放，调用方应立即重试 TryClaim（可能被其他 agent 抢先）。
	// 返回 context.Canceled / context.DeadlineExceeded 表示放弃等待。
	// 返回 ErrWaitTimeout 表示 timeout <= 0（排队被禁用）。
	// 参见 nextUpgrade_v3.md §8.3 文件冲突排队。
	WaitForRelease(ctx context.Context, agentID string, filePath string, timeout time.Duration) error
}
