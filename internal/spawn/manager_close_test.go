package spawn

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/runner"
)

// countingPublisher 记录 PublishTask 调用次数，并为任务分配 ID（模拟真实 store）。
type countingPublisher struct {
	mu    sync.Mutex
	count int
}

func (p *countingPublisher) PublishTask(task *model.Task) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	if task.ID == "" {
		task.ID = uuid.NewString()
	}
	return nil
}

func (p *countingPublisher) published() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count
}

// closedTestSpawnRequest 构造不依赖 system_prompt_file 的合法请求
// （SystemPromptSet=true 使 buildAdhocRuntime 不读文件），让测试聚焦
// closed 窗口语义而不是文件系统。
func closedTestSpawnRequest() SpawnRequest {
	return SpawnRequest{
		BaseKind:               "explorer",
		InitialTaskDescription: "x",
		Override:               RuntimeOverride{SystemPrompt: "x", SystemPromptSet: true},
	}
}

// TestManager_Spawn_AfterShutdown_PublishesNothing 钉住 F9：
// manager 已关闭时 Spawn 必须报错且不发布任何 initial_task——
// 旧实现在发布任务后才复查 closed，Shutdown 落进该窗口会留下
// 无人认领的孤儿 pending 任务。
func TestManager_Spawn_AfterShutdown_PublishesNothing(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer", Tools: []string{"read_file"}}}}
	pub := &countingPublisher{}
	m := NewManager(cfg, runner.RunnerDeps{}, fakeLLMFactory, pub)
	m.Shutdown()

	_, _, err := m.Spawn(context.Background(), closedTestSpawnRequest())
	if err == nil || !strings.Contains(err.Error(), "manager is closed") {
		t.Fatalf("expected manager-is-closed error, got %v", err)
	}
	if got := pub.published(); got != 0 {
		t.Fatalf("closed manager 不应发布任务，published=%d", got)
	}
	if got := m.ActiveCount(); got != 0 {
		t.Fatalf("closed manager 不应登记 spawn，ActiveCount=%d", got)
	}
}

// TestManager_Spawn_ConcurrentShutdown_NoOrphanTask 以竞速方式钉住 F9 窗口的
// 不变量：Spawn 与 Shutdown 并发时，返回错误的 Spawn 绝不允许已发布任务——
// published 数必须恰好等于成功 Spawn 数（closed 检查、发布、登记在同一临界区）。
func TestManager_Spawn_ConcurrentShutdown_NoOrphanTask(t *testing.T) {
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "explorer", Tools: []string{"read_file"}}}}
	pub := &countingPublisher{}
	m := NewManager(cfg, runner.RunnerDeps{}, fakeLLMFactory, pub)
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent() // 预取消：spawn 出的 runner goroutine 立即退出，不触碰 nil Store
	m.SetParentContext(parent)

	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := m.Spawn(context.Background(), closedTestSpawnRequest()); err == nil {
				successes.Add(1)
			}
		}()
	}
	m.Shutdown()
	wg.Wait()

	if got, want := pub.published(), int(successes.Load()); got != want {
		t.Fatalf("published=%d 但成功 Spawn=%d——存在已发布但无人认领的孤儿任务", got, want)
	}
	m.Shutdown() // 幂等复查：重复 Shutdown 不应 panic
}
