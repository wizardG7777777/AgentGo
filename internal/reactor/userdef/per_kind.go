package userdef

import (
	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// kindFilteredReactor 是 §6.2 per-kind 过滤包装：仅当事件来源 agent 的 kind
// 命中 self.kind 时才把事件转发给 inner reactor。
//
// 触发条件：lookup(ev.AgentID) == self.kind。
// AgentID="" 或未注册的 ID 视为不匹配（lookup 返回 ""，与非空 self.kind 比较为 false）。
//
// 多粒度叠加（§6.2.3）：全局 reactor 与 per-kind reactor 互不覆盖，二者都注册到
// 同一 reactor.Registry，dispatcher 会全部投递；单个 reactor 失败不影响其他。
//
// 此 wrapper 复用 inner 的 Subscribe / Name / Priority / IsSync——只在 Run 时多一层 if。
type kindFilteredReactor struct {
	inner  reactor.Reactor
	kind   string
	lookup func(agentID string) string
}

func (k *kindFilteredReactor) Name() string                 { return k.inner.Name() }
func (k *kindFilteredReactor) Subscribe() []trace.EventKind { return k.inner.Subscribe() }
func (k *kindFilteredReactor) IsSync() bool                 { return k.inner.IsSync() }
func (k *kindFilteredReactor) Priority() int                { return k.inner.Priority() }

func (k *kindFilteredReactor) Run(ev trace.Event) error {
	if k.lookup(ev.AgentID) != k.kind {
		return nil // 不在 per-kind 范围，静默跳过
	}
	return k.inner.Run(ev)
}
