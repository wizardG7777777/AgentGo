package scheduler

import (
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// TestSchedulerBundle_New_IdleThresholdStaysZero 钉住 E3 的 scheduler 决策：
// 全局 agent_idle_threshold 只作用于 runner 构造的任务执行类 agent；
// scheduler 是必须常驻的预制代理（空闲退出会导致无人派发/汇总用户请求），
// 即使配置了全局阈值也保持 0（永不空闲退出）。
func TestSchedulerBundle_New_IdleThresholdStaysZero(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()
	cfg.AgentIdleThreshold = 9 // 全局配置非 0：scheduler 也不应消费它

	bundle := New(s, r, &scriptedLLM{}, ch, cfg, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if bundle == nil || bundle.Agent == nil {
		t.Fatal("New returned nil Bundle")
	}
	if got := bundle.Agent.IdleThreshold; got != 0 {
		t.Errorf("scheduler Agent.IdleThreshold=%d，want 0（常驻 daemon 不消费全局 agent_idle_threshold）", got)
	}
}
