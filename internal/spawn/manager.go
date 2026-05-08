package spawn

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/google/uuid"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/runner"
	"agentgo/internal/trace"
)

// PublishStore 是 Manager 派发初始任务所需的最小接口。
type PublishStore interface {
	PublishTask(task *model.Task) error
}

// Manager 是 ad-hoc agent 的中央生命周期管理器。
//
// 同时实现 reactor.Reactor（订阅 task 终态事件）—— 当 spawn 出来的 runner 完成
// 它的 initial_task 时，匹配 TaskID 触发 cancel，runner goroutine 退出。
//
// goroutine 模型：每个 spawn 启动一个 runner goroutine，ctx 是 m.parentCtx 的派生 ctx；
// system shutdown（parentCtx 取消）会传播到所有 spawn；one_shot 终态则单独 cancel。
type Manager struct {
	cfg        *config.Config
	deps       runner.RunnerDeps
	llmFactory LLMFactory
	publisher  PublishStore

	baseKindMap map[string]config.AgentKind

	mu             sync.Mutex
	parentCtx      context.Context
	wg             sync.WaitGroup
	spawns         map[string]*activeSpawn // by spawnID
	spawnsByTaskID map[string]*activeSpawn // by initial task ID（终态事件路由用）
	agentKindByID  map[string]string       // by ad-hoc agent instanceID（per-kind reactor 路由用）
	closed         bool
}

type activeSpawn struct {
	id         string
	taskID     string
	cancel     context.CancelFunc
	eventType  string
	instanceID string // ad-hoc agent 的 InstanceID（runner 内部 agent.ID）
	baseKind   string // base_kind，§6.2.4：ad-hoc agent 继承 base_kind 的 per-kind reactor
}

// NewManager 构造 Manager。parentCtx 在 SetParentContext 注入；构造时使用
// context.Background 作为占位，避免空指针。
func NewManager(cfg *config.Config, deps runner.RunnerDeps, llmFactory LLMFactory, publisher PublishStore) *Manager {
	bm := make(map[string]config.AgentKind, len(cfg.Agents))
	for _, k := range cfg.Agents {
		bm[k.Kind] = k
	}
	return &Manager{
		cfg:            cfg,
		deps:           deps,
		llmFactory:     llmFactory,
		publisher:      publisher,
		baseKindMap:    bm,
		parentCtx:      context.Background(),
		spawns:         make(map[string]*activeSpawn),
		spawnsByTaskID: make(map[string]*activeSpawn),
		agentKindByID:  make(map[string]string),
	}
}

// SetParentContext 注入父 ctx——所有后续 spawn 出来的 runner ctx 都派生自此。
// system 启动时（System.Start）应在任何 reactor 触发前调用。
func (m *Manager) SetParentContext(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parentCtx = ctx
}

// Spawn 创建 ad-hoc runner + 派发 initial_task，返回 spawnID 与 taskID。
//
// 错误回滚：PublishTask 成功且 active spawn 登记完成后才启动 runner；
// 因此发布失败不会留下半启动 goroutine。
//
// ctx 参数：用于 reactor 触发时的请求上下文（取消语义，例如父事件 timeout）。
// 真正绑定到 runner 的 ctx 派生自 m.parentCtx，与 ctx 无关——避免 reactor 短生命周期取消把 runner 拖死。
func (m *Manager) Spawn(_ context.Context, req SpawnRequest) (string, string, error) {
	if req.Lifecycle != "" && req.Lifecycle != "one_shot" {
		return "", "", fmt.Errorf("spawn_agent: lifecycle %q not implemented (v5.x; only one_shot)", req.Lifecycle)
	}
	if req.Depth > ReactorSpawnMaxDepth {
		trace.Emit(trace.Event{
			Kind:   trace.KindReactorSpawnDepthExceeded,
			TaskID: req.SourceTaskID,
			Depth:  req.Depth,
			Reason: fmt.Sprintf("reactor spawn depth %d exceeds max %d", req.Depth, ReactorSpawnMaxDepth),
		})
		return "", "", fmt.Errorf("spawn_agent: reactor spawn depth %d exceeds max %d", req.Depth, ReactorSpawnMaxDepth)
	}
	base, ok := m.baseKindMap[req.BaseKind]
	if !ok {
		return "", "", fmt.Errorf("spawn_agent: unknown base_kind %q", req.BaseKind)
	}
	if req.InitialTaskDescription == "" {
		return "", "", fmt.Errorf("spawn_agent: initial_task description is empty after rendering")
	}
	if m.llmFactory == nil {
		return "", "", fmt.Errorf("spawn_agent: LLMFactory is nil")
	}
	if m.publisher == nil {
		return "", "", fmt.Errorf("spawn_agent: publisher is nil")
	}

	spawnID := uuid.NewString()
	eventType := "adhoc:" + spawnID
	instanceID := fmt.Sprintf("%s-adhoc-%s", base.Kind, spawnID[:8])

	rt, err := buildAdhocRuntime(base, m.cfg.LLM, m.cfg.ToolProfiles, req.Override, instanceID, eventType)
	if err != nil {
		return "", "", fmt.Errorf("spawn_agent: build runtime: %w", err)
	}

	// 构造独立 LLMClient（per-spawn）
	kindLLM := m.llmFactory(rt.Model)
	deps := m.deps
	deps.LLMClient = kindLLM

	rn := runner.New(rt, deps)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", "", fmt.Errorf("spawn_agent: manager is closed")
	}
	parentCtx := m.parentCtx
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(parentCtx)

	// 投递 initial_task
	task := &model.Task{
		Description: req.InitialTaskDescription,
		EventType:   eventType,
		Depth:       req.Depth,
	}
	if err := m.publisher.PublishTask(task); err != nil {
		cancel()
		return "", "", fmt.Errorf("spawn_agent: publish initial task: %w", err)
	}

	sp := &activeSpawn{
		id:         spawnID,
		taskID:     task.ID,
		cancel:     cancel,
		eventType:  eventType,
		instanceID: instanceID,
		baseKind:   base.Kind,
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		return "", "", fmt.Errorf("spawn_agent: manager is closed")
	}
	m.spawns[spawnID] = sp
	m.spawnsByTaskID[task.ID] = sp
	m.agentKindByID[instanceID] = base.Kind
	m.mu.Unlock()

	// 登记 active spawn 后再启动 runner，避免极短任务的终态事件早于 spawnsByTaskID
	// 写入，从而错过 one_shot 清理。
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		rn.Run(ctx)
	}()

	log.Printf("[spawn] ad-hoc agent %q started (base_kind=%s, task=%s, event_type=%s)",
		instanceID, base.Kind, task.ID, eventType)
	return spawnID, task.ID, nil
}

// ActiveCount 返回当前活跃 spawn 数量（测试 / 调试用）。
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.spawns)
}

// KindOf 返回 ad-hoc agent 的 base_kind；agentID 不属于本 Manager 时返回 ""。
//
// 用于 §6.2 per-kind reactor 路由：spawned ad-hoc agent 触发的事件应被 base_kind
// 的 per-kind reactor 看到（§6.2.4 决议）。bootstrap 把此函数注入到 userdef.Deps.AgentKindOf
// 链上，与静态 cfg.Agents 的 InstanceID→Kind 映射合并。
func (m *Manager) KindOf(agentID string) string {
	if agentID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.agentKindByID[agentID]
}

// Shutdown 取消所有活跃 spawn 的 ctx 并等待 runner goroutine 退出。
// system 关闭时调用——也可在 SetParentContext 的 ctx 取消时被动触发，但显式 Shutdown
// 提供同步等待语义，避免主程序在 runner 仍在跑时退出。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.closed = true
	for _, sp := range m.spawns {
		sp.cancel()
	}
	m.spawns = nil
	m.spawnsByTaskID = nil
	m.agentKindByID = nil
	m.mu.Unlock()
	m.wg.Wait()
}

// ── reactor.Reactor 实现：订阅 task 终态事件触发 one_shot 销毁 ────────

func (m *Manager) Name() string { return "spawn-manager" }

func (m *Manager) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindTaskCompleted,
		trace.KindTaskFailed,
		trace.KindTaskCancelled,
	}
}

func (m *Manager) IsSync() bool  { return false }
func (m *Manager) Priority() int { return 800 }

// Run 接收 task 终态事件——若 TaskID 命中某个 spawn，则 cancel 它的 ctx 触发 one_shot 销毁。
//
// 不在意未匹配的 TaskID（普通 worker 任务也会发这些事件，filter by membership 即可）。
func (m *Manager) Run(ev trace.Event) error {
	if ev.TaskID == "" {
		return nil
	}
	m.mu.Lock()
	sp, ok := m.spawnsByTaskID[ev.TaskID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.spawnsByTaskID, ev.TaskID)
	delete(m.spawns, sp.id)
	m.mu.Unlock()
	sp.cancel()
	log.Printf("[spawn] ad-hoc agent (spawn=%s, task=%s) lifecycle=one_shot, cleaning up after %s",
		sp.id, ev.TaskID, ev.Kind)
	return nil
}
