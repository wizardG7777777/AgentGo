// Package memory 提供 Agent 的长短期记忆管理（v5 Phase 1 引入，
// 取代 v4 时代 TeamAwarenessHook 的临时上下文注入方案）。
//
// 设计哲学（MemoryManageSystem.md §0）：
//  1. 记忆是一等公民——跨任务/会话/进程持久化
//  2. Agent 主动拉取，而非被动注入
//  3. 作用域分层：Process / Session / Project
//  4. 写入与读取解耦：写入由系统组件触发，读取由 Agent 框架层调度
//
// 当前 v5 首版仅实现 ScopeProcess 内存存储。Session / Project 留作 MM8/MM9。
package memory

import (
	"context"
	"time"
)

// Scope 是记忆条目的作用域分层。
type Scope int

const (
	// ScopeProcess 进程级：随系统重启清空，纯内存。
	// 典型内容：当前活跃 Agent 状态、board snapshot 缓存、实时文件占用。
	ScopeProcess Scope = iota
	// ScopeSession 会话级：session 结束清空，落盘到
	// .agentgo/sessions/sess-<id>/memory.jsonl（v5 不实现，预留）。
	ScopeSession
	// ScopeProject 项目级：跨会话持久化到 .agentgo/memory/（v5 不实现，预留）。
	ScopeProject
)

// String 返回 Scope 的字符串形态，便于日志与 trace 字段使用。
func (s Scope) String() string {
	switch s {
	case ScopeProcess:
		return "process"
	case ScopeSession:
		return "session"
	case ScopeProject:
		return "project"
	default:
		return "unknown"
	}
}

// Kind 是记忆条目的种类标签。详见 MemoryManageSystem.md §3.2。
type Kind string

const (
	// KindConstraint 项目级约束文档（如 "禁止直接操作 DB"）。
	KindConstraint Kind = "constraint"
	// KindLearning 学习到的经验（失败/成功总结）。
	KindLearning Kind = "learning"
	// KindPattern 代码模式 / 项目结构洞察。
	KindPattern Kind = "pattern"
	// KindContext 进程级上下文（TeamSnapshot / FileAwareness 等）。
	// v5 Phase 1 由 team-awareness 迁移过来后绝大多数条目是这个 Kind。
	KindContext Kind = "context"
	// KindAgentState Agent 级状态快照。
	KindAgentState Kind = "agent_state"
)

// Entry 是单条记忆。Embedding 字段为向量检索预留（v5.x 引入，当前版本不填充）。
type Entry struct {
	ID          string    `json:"id"`
	Scope       Scope     `json:"scope"`
	Kind        Kind      `json:"kind"`
	Key         string    `json:"key"`     // 检索键
	Content     string    `json:"content"` // 文本内容（或序列化后的结构化数据）
	Embedding   []float32 `json:"embedding,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Source      string    `json:"source,omitempty"` // 来源（agentID / taskID / user）
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	AccessCount int       `json:"access_count,omitempty"`
}

// Store 是 Memory 的统一存储接口。所有方法必须并发安全——v5 阶段
// 同一 store 可能被 scheduler / worker / roster 等多 goroutine 并发访问。
//
// nil Store 调用约定：调用方在调 Store 前判 nil；所有 Agent / hook
// 持有的 Store 引用都允许是 nil（退化为不记录任何记忆，等价于 v4 行为）。
type Store interface {
	// Put 写入或更新记忆条目。
	// 行为约定：
	//   - entry.ID 为空时由实现自行生成（推荐 scope:kind:key 形式）
	//   - 同 scope+kind+key 的已有条目会被覆盖（UpdatedAt 同步刷新）
	//   - CreatedAt 在首次写入时落定，后续 Put 不修改
	Put(ctx context.Context, entry Entry) error

	// Query 文本检索 + 标签过滤。query 为空时返回该 scope+kind 下全部条目（按
	// UpdatedAt 倒序，受 limit 截断）。
	//
	// 简单模式：当 query 是某条已存在 Entry 的 Key 时，实现返回精确匹配的那一条。
	// 这是 v5 Phase 1 的最小实用集——足够替代 team-awareness 三 section 的
	// 定点 query("team_snapshot" / "file_awareness" / "goal_anchor"）。
	// 全文检索 / 模糊匹配留作 v5.x 增量。
	Query(ctx context.Context, scope Scope, kind Kind, query string, limit int) ([]Entry, error)

	// QueryByVector 向量检索。v5 Phase 1 接口预留但默认实现返回 ErrNotImplemented。
	QueryByVector(ctx context.Context, scope Scope, embedding []float32, limit int) ([]Entry, error)

	// Delete 按 ID 删除。条目不存在时返回 nil（幂等）。
	Delete(ctx context.Context, id string) error

	// Clear 按作用域清空所有条目。Session 关闭、进程重启等场景使用。
	Clear(ctx context.Context, scope Scope) error
}
