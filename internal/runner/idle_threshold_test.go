package runner

import (
	"context"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// idleTestLLM 是 IdleThreshold 映射测试的最小 llm.Client 实现——
// New 只把 client 存进 executor，本测试不触发任何 Chat 调用。
type idleTestLLM struct{}

func (idleTestLLM) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	return llm.Response{Content: "ok"}, nil
}

// TestNewMapsIdleThresholdFromRuntimeConfig 验证 E3 接线末端：
// AgentRuntimeConfig.IdleThreshold 到达 Agent.IdleThreshold，
// 取代旧的硬编码 a.IdleThreshold = 0。
func TestNewMapsIdleThresholdFromRuntimeConfig(t *testing.T) {
	deps := RunnerDeps{
		Store:     store.NewMemoryTaskStore(nil, 32, 1, 60),
		Roster:    roster.NewMemoryRoster(),
		LLMClient: idleTestLLM{},
	}

	rn := New(config.AgentRuntimeConfig{
		InstanceID: "worker-1", Kind: "worker", AllowedTools: []string{"read_file"},
		TaskMaxRetries: 2, IdleThreshold: 7,
	}, deps)
	if got := rn.Agent().IdleThreshold; got != 7 {
		t.Errorf("Agent.IdleThreshold=%d，want 7（配置值应透传到 agent）", got)
	}

	// 构造点未赋值（零值）时保持旧行为：0 = 永不空闲退出。
	rn = New(config.AgentRuntimeConfig{
		InstanceID: "worker-2", Kind: "worker", AllowedTools: []string{"read_file"},
		TaskMaxRetries: 2,
	}, deps)
	if got := rn.Agent().IdleThreshold; got != 0 {
		t.Errorf("Agent.IdleThreshold=%d，want 0（未配置时保持永不空闲退出）", got)
	}
}
