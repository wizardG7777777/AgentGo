package runner

import "agentgo/internal/testmodel"

import (
	"path/filepath"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/contentstore"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// TestNewWiresContentStoreToAgent 钉住 L3 装配握手：共享 RunnerDeps 的同一
// ContentStore 必须到达静态/Team/Spawn Runner 内部 Agent，不能只停在 bootstrap。
func TestNewWiresContentStoreToAgent(t *testing.T) {
	content, err := contentstore.Open(filepath.Join(t.TempDir(), "content"), contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = content.Close() })

	rn := newTestRunner(t, config.AgentRuntimeConfig{
		InstanceID: "worker-content", Kind: "worker", AllowedTools: []string{"read_file"},
		TaskMaxRetries: 2,
	}, RunnerDeps{
		Store:  store.NewMemoryTaskStore(nil, 32, 1, 60),
		Roster: roster.NewMemoryRoster(), LLMClient: idleTestLLM{}, ContentStore: content, ContextRuntime: testmodel.Runtime(t),
	})
	if rn.Agent().ContentStore != content {
		t.Fatal("RunnerDeps.ContentStore 未透传到 Agent")
	}
}
