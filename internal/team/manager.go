package team

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/google/uuid"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/runner"
	"agentgo/internal/trace"
)

// Manager provisions and recovers process-local runners from durable
// AgentTemplate TeamSpecs. All lifecycle mutations are serialized by opMu so
// route installation, limit accounting and idempotent reuse form one operation
// from Scheduler's point of view.
type Manager struct {
	deps         runner.RunnerDeps
	llmFactory   LLMFactory
	catalog      *agenttemplate.Catalog
	coordinator  *plan.Coordinator
	store        TeamStore
	routes       RouteRegistry
	maxInstances int

	opMu         sync.Mutex
	parentCtx    context.Context
	started      bool
	closed       bool
	active       map[string]*activeTeam
	cleanupWG    sync.WaitGroup
	shutdownDone chan struct{}
	// preservedShutdownMailboxIDs 是 ShutdownPreservingMailboxes 已停止但为
	// 最终 Session 快照暂留的动态邮箱；FinalizeShutdownMailboxes 再删除。
	preservedShutdownMailboxIDs map[string]struct{}
}

type activeTeam struct {
	spec                   TeamSpec
	agentIDs               []string
	tools                  []string
	runners                []*runner.Runner
	recoveredMailboxClaims map[string]string // agentID → eventType；失败回滚，正常终止删除
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
}

type mailboxCleanupMode uint8

const (
	cleanupMailboxUnregister mailboxCleanupMode = iota
	cleanupMailboxRollbackRecovered
	cleanupMailboxPreserveForSnapshot
)

// NewManager builds a stopped Manager. Start must succeed before Provision is
// allowed. maxInstances<=0 selects DefaultMaxInstances; values above the hard
// safety ceiling are clamped to HardMaxInstances.
func NewManager(
	deps runner.RunnerDeps,
	llmFactory LLMFactory,
	catalog *agenttemplate.Catalog,
	coordinator *plan.Coordinator,
	store TeamStore,
	routes RouteRegistry,
	maxInstances int,
) *Manager {
	if maxInstances <= 0 {
		maxInstances = DefaultMaxInstances
	}
	if maxInstances > HardMaxInstances {
		maxInstances = HardMaxInstances
	}
	return &Manager{
		deps: deps, llmFactory: llmFactory, catalog: catalog,
		coordinator: coordinator, store: store, routes: routes,
		maxInstances: maxInstances, active: make(map[string]*activeTeam),
		shutdownDone: make(chan struct{}),
	}
}

// Start recovers every durable ready TeamSpec. Recovery is fail-closed: a
// changed template digest or an over-limit ready set starts no runners and
// returns an error. Teams belonging to terminal Plans are durably stopped and
// skipped before digest validation.
func (m *Manager) Start(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return ErrManagerClosed
	}
	if m.started {
		return nil
	}
	if err := m.validateDependencies(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	specs, err := m.store.List()
	if err != nil {
		return fmt.Errorf("list durable teams: %w", err)
	}
	terminalPlans := make(map[string]string)
	ready := make([]TeamSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Status != StatusReady {
			continue
		}
		p, err := m.coordinator.Store().GetPlan(spec.PlanID)
		if err != nil {
			return fmt.Errorf("recover team %s plan %s: %w", spec.ID, spec.PlanID, err)
		}
		if model.IsPlanTerminal(p.Status) {
			terminalPlans[p.ID] = "plan_terminal:" + string(p.Status)
			continue
		}
		ready = append(ready, spec)
	}
	for planID, reason := range terminalPlans {
		if _, err := m.store.StopPlan(planID, reason); err != nil {
			return fmt.Errorf("stop terminal plan %s teams during recovery: %w", planID, err)
		}
	}

	total := 0
	prepared := make([]runtimePreparation, 0, len(ready))
	for _, spec := range ready {
		tmpl, err := m.catalog.Resolve(spec.TemplateRef)
		if err != nil {
			// 模板已被删除/改名：中止启动会让任何模板升级都变成硬故障。
			// 停用陈旧 Team，需要时由 Scheduler 重新 provision。
			log.Printf("[team] 恢复 Team %s 失败：模板 %s 不可用（%v），已标记 stopped；需要时 Scheduler 会重新 provision", spec.ID, spec.TemplateRef, err)
			if _, stopErr := m.store.SetStatus(spec.ID, StatusStopped, "template_unavailable:"+spec.TemplateRef); stopErr != nil {
				return fmt.Errorf("stop team %s with unavailable template: %w", spec.ID, stopErr)
			}
			continue
		}
		if tmpl.Digest != spec.TemplateDigest {
			// 模板内容或默认模型变化会改变 digest（升级路径的常态）。
			// 停用陈旧 Team 而不是中止启动；不静默复用旧 digest 的运行时。
			log.Printf("[team] Team %s 的模板 %s digest 已变化（stored=%s current=%s），已标记 stopped；需要时 Scheduler 会重新 provision",
				spec.ID, spec.TemplateRef, spec.TemplateDigest, tmpl.Digest)
			if _, stopErr := m.store.SetStatus(spec.ID, StatusStopped, "template_digest_changed:"+tmpl.Digest); stopErr != nil {
				return fmt.Errorf("stop team %s after digest change: %w", spec.ID, stopErr)
			}
			continue
		}
		if err := validateReplicas(tmpl, spec.Replicas); err != nil {
			return fmt.Errorf("recover team %s: %w", spec.ID, err)
		}
		total += spec.Replicas
		if total > m.maxInstances {
			return fmt.Errorf("%w: recovery requires %d instances, max=%d",
				ErrProcessLimitExceeded, total, m.maxInstances)
		}
		prep, err := m.prepare(spec, tmpl)
		if err != nil {
			return fmt.Errorf("recover team %s: %w", spec.ID, err)
		}
		prepared = append(prepared, prep)
	}

	// Construct every runner and register its process-local mailbox/activity
	// surface before publishing any route. A route is the Scheduler-visible
	// readiness fact, so exposing it earlier could make work routable to a Team
	// whose runtime has not been fully materialized yet.
	m.parentCtx = ctx
	materialized := make([]runtimeActivation, 0, len(prepared))
	for _, prep := range prepared {
		activation, err := m.materializePrepared(prep, true)
		if err != nil {
			var cleanupErr error
			for _, candidate := range materialized {
				cleanupErr = errors.Join(cleanupErr, m.discardMaterialized(candidate))
			}
			m.parentCtx = nil
			return errors.Join(fmt.Errorf("recover team %s materialize: %w", prep.spec.ID, err), cleanupErr)
		}
		materialized = append(materialized, activation)
	}

	registered := make([]string, 0, len(materialized))
	for _, activation := range materialized {
		if err := m.registerRoute(activation.team.spec, activation.tmpl); err != nil {
			for _, key := range registered {
				_ = m.routes.UnregisterRoute(key)
			}
			var cleanupErr error
			for _, candidate := range materialized {
				cleanupErr = errors.Join(cleanupErr, m.discardMaterialized(candidate))
			}
			m.parentCtx = nil
			return errors.Join(fmt.Errorf("recover team %s route: %w", activation.team.spec.ID, err), cleanupErr)
		}
		registered = append(registered, activation.team.spec.EventType)
	}

	for i, activation := range materialized {
		if err := m.startMaterialized(activation); err != nil {
			for _, key := range registered {
				_ = m.routes.UnregisterRoute(key)
			}
			var cleanupErr error
			for j, candidate := range materialized {
				if j <= i {
					cleanupErr = errors.Join(cleanupErr, m.stopMaterialized(candidate))
				} else {
					cleanupErr = errors.Join(cleanupErr, m.discardMaterialized(candidate))
				}
			}
			m.parentCtx = nil
			return errors.Join(fmt.Errorf("start recovered team %s: %w", activation.team.spec.ID, err), cleanupErr)
		}
	}
	for _, activation := range materialized {
		m.releaseMaterialized(activation)
	}
	m.started = true
	return nil
}

// Provision creates or reuses a homogeneous Team. Controller authority is
// checked and held through the durable/route mutation via WithControllerLease.
func (m *Manager) Provision(ctx context.Context, req agenttemplate.ProvisionRequest) (agenttemplate.ProvisionResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return agenttemplate.ProvisionResult{}, ErrManagerClosed
	}
	if !m.started {
		return agenttemplate.ProvisionResult{}, ErrNotStarted
	}
	if m.parentCtx != nil {
		if err := m.parentCtx.Err(); err != nil {
			return agenttemplate.ProvisionResult{}, fmt.Errorf("team manager parent context is closed: %w", err)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return agenttemplate.ProvisionResult{}, err
	}
	req.PlanID = strings.TrimSpace(req.PlanID)
	req.ControllerTaskID = strings.TrimSpace(req.ControllerTaskID)
	req.TemplateRef = strings.TrimSpace(req.TemplateRef)
	req.Purpose = strings.TrimSpace(req.Purpose)
	if req.PlanID == "" || req.ControllerTaskID == "" || req.TemplateRef == "" || req.Purpose == "" {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("plan_id, controller_task_id, template_ref and purpose are required")
	}
	if req.Replicas == 0 {
		req.Replicas = 1
	}
	tmpl, err := m.catalog.Resolve(req.TemplateRef)
	if err != nil {
		return agenttemplate.ProvisionResult{}, err
	}
	req.TemplateRef = tmpl.Ref
	if err := validateReplicas(tmpl, req.Replicas); err != nil {
		return agenttemplate.ProvisionResult{}, err
	}

	var result agenttemplate.ProvisionResult
	err = m.coordinator.WithControllerLease(ctx, req.PlanID, req.ControllerTaskID, func() error {
		specs, err := m.store.List()
		if err != nil {
			return fmt.Errorf("list teams: %w", err)
		}
		existing, found := findIdempotent(specs, req)
		if found && existing.TemplateDigest != tmpl.Digest {
			return fmt.Errorf("%w: team=%s ref=%s stored=%s current=%s",
				ErrTemplateDigestMismatch, existing.ID, existing.TemplateRef, existing.TemplateDigest, tmpl.Digest)
		}
		if found && existing.Status != StatusReady {
			return fmt.Errorf("idempotent team %s is %s", existing.ID, existing.Status)
		}
		if found {
			existing.ControllerTaskID = req.ControllerTaskID
			if active, ok := m.active[existing.ID]; ok {
				stored, _, err := m.store.Ensure(existing)
				if err != nil {
					return fmt.Errorf("refresh team controller: %w", err)
				}
				existing = stored
				result = resultFor(existing, active.agentIDs, active.tools, true)
				return nil
			}
		}

		replicas := req.Replicas
		if m.activeInstanceCount()+replicas > m.maxInstances {
			return fmt.Errorf("%w: active=%d requested=%d max=%d",
				ErrProcessLimitExceeded, m.activeInstanceCount(), replicas, m.maxInstances)
		}
		if !found {
			id := uuid.NewString()
			existing = TeamSpec{
				ID: id, TemplateRef: tmpl.Ref, TemplateDigest: tmpl.Digest,
				PlanID: req.PlanID, ControllerTaskID: req.ControllerTaskID,
				Purpose: req.Purpose, EventType: "team:" + id,
				Replicas: replicas, Status: StatusReady,
			}
		}
		// Durability is established before runner/mailbox construction, and the
		// route is deliberately the final published readiness surface.
		stored, created, err := m.store.Ensure(existing)
		if err != nil {
			return fmt.Errorf("persist team: %w", err)
		}
		existing = stored
		if existing.TemplateDigest != tmpl.Digest {
			return fmt.Errorf("%w: team=%s ref=%s stored=%s current=%s",
				ErrTemplateDigestMismatch, existing.ID, existing.TemplateRef, existing.TemplateDigest, tmpl.Digest)
		}
		if existing.Status != StatusReady {
			return fmt.Errorf("idempotent team %s is %s", existing.ID, existing.Status)
		}
		prep, err := m.prepare(existing, tmpl)
		if err != nil {
			return m.markProvisionFailed(existing, "provision_failed:runtime_prepare", err)
		}
		activation, err := m.materializePrepared(prep, false)
		if err != nil {
			return m.markProvisionFailed(existing, "provision_failed:runtime_materialize", err)
		}
		if err := m.startMaterialized(activation); err != nil {
			cleanupErr := m.stopMaterialized(activation)
			return m.markProvisionFailed(existing, "provision_failed:runner_start", errors.Join(err, cleanupErr))
		}
		if err := m.registerRoute(existing, tmpl); err != nil {
			// RegisterRoute should be all-or-nothing, but remove the key
			// defensively before stopping the runtime in case an implementation
			// returned an error after a partial mutation.
			_ = m.routes.UnregisterRoute(existing.EventType)
			cleanupErr := m.stopMaterialized(activation)
			return m.markProvisionFailed(existing, "provision_failed:route_registration",
				errors.Join(fmt.Errorf("register team route: %w", err), cleanupErr))
		}
		m.releaseMaterialized(activation)
		active := m.active[existing.ID]
		result = resultFor(existing, active.agentIDs, active.tools, !created)
		return nil
	})
	if err != nil {
		return agenttemplate.ProvisionResult{}, err
	}
	return result, nil
}

// Shutdown cancels runtime runners and removes their ephemeral routes while
// deliberately leaving durable specs ready for the next process to recover.
func (m *Manager) Shutdown() {
	m.ShutdownPreservingMailboxes()
	m.FinalizeShutdownMailboxes()
}

// ShutdownPreservingMailboxes 取消 route 和 runner、等待所有 Team 执行退出，
// 但暂不注销邮箱。System.Shutdown 用它在稳定静止态导出最终 Session 快照，
// 防止普通 Shutdown 先删动态邮箱再保存而永久丢失未读邮件。
func (m *Manager) ShutdownPreservingMailboxes() {
	if m == nil {
		return
	}
	m.opMu.Lock()
	if m.shutdownDone == nil {
		m.shutdownDone = make(chan struct{})
	}
	if m.closed {
		done := m.shutdownDone
		m.opMu.Unlock()
		if done != nil {
			<-done
		} else {
			m.cleanupWG.Wait()
		}
		return
	}
	m.closed = true
	m.started = false
	if m.preservedShutdownMailboxIDs == nil {
		m.preservedShutdownMailboxIDs = make(map[string]struct{})
	}
	active := make([]*activeTeam, 0, len(m.active))
	for id, team := range m.active {
		delete(m.active, id)
		_ = m.routes.UnregisterRoute(team.spec.EventType)
		team.cancel()
		for _, agentID := range team.agentIDs {
			m.preservedShutdownMailboxIDs[agentID] = struct{}{}
		}
		active = append(active, team)
	}
	m.opMu.Unlock()
	for _, team := range active {
		if err := m.waitAndCleanup(team, cleanupMailboxPreserveForSnapshot); err != nil {
			log.Printf("[team] shutdown cleanup team=%s: %v", team.spec.ID, err)
		}
	}
	m.cleanupWG.Wait()
	close(m.shutdownDone)
}

// FinalizeShutdownMailboxes 删除为最终快照暂留的动态邮箱。它与
// ShutdownPreservingMailboxes 均幂等；直接调用 Shutdown 仍保留旧的一步式语义。
func (m *Manager) FinalizeShutdownMailboxes() {
	if m == nil {
		return
	}
	// 允许调用方直接/并发调用 Finalize；先等待两阶段关闭的第一阶段完成。
	m.ShutdownPreservingMailboxes()
	m.opMu.Lock()
	ids := make([]string, 0, len(m.preservedShutdownMailboxIDs))
	for agentID := range m.preservedShutdownMailboxIDs {
		ids = append(ids, agentID)
	}
	m.preservedShutdownMailboxIDs = nil
	m.opMu.Unlock()
	if m.deps.MBRegistry == nil {
		return
	}
	for _, agentID := range ids {
		_ = m.deps.MBRegistry.Unregister(agentID)
	}
}

func (m *Manager) ActiveCount() int {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.activeInstanceCount()
}

// Reactor implementation. Task terminal events provide a fallback Plan lookup
// for terminal paths that do not emit a dedicated plan event.
func (m *Manager) Name() string { return "agent-template-team-manager" }

func (m *Manager) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindAcceptanceCompleted,
		trace.KindPlanTerminal,
		trace.KindPlanRevisionChanged,
		trace.KindPlanPaused,
		trace.KindTaskCompleted,
		trace.KindTaskFailed,
		trace.KindTaskBlocked,
		trace.KindTaskCancelled,
	}
}

// IsSync 返回 false（C2 修复，2026-07-18）：本 Reactor 的 Run 会读
// GetTask/GetPlan、竞争 opMu、并在 StopPlan 里做全量 JSON 重写 + 两次 fsync。
// 过去 IsSync=true 让这些工作全部压在 trace.Emit 调用方（agent 终态路径）
// 的 goroutine 上，磁盘抖动直接拖慢所有 agent。
//
// 改 async 的排序安全论证（改前核查结论）：
//  1. Registry 只保证 sync Reactor 按 priority 串行、先于 async 投递；订阅
//     同类事件的其他 Reactor 均不消费 team 状态——TaskEndCallbackReactor
//     (100, sync) 只触发任务结束回调，spawn.Manager (800, async) 与 950 档
//     观察类 Reactor（history/artifact/read-set-write）各自独立。
//  2. 终态事件的 Emit 方（agent 状态机、tools/plan_control）在 Emit 返回后
//     不再触碰任何 team 运行时状态。
//  3. 向已终态 Plan 发起 Provision 由 WithControllerLease 拦截（非 Running
//     即 ErrPlanPaused），与 Reactor 执行时机无关。
//  4. Run 本身幂等且由 opMu 串行化、带 closed 守卫：多个终态事件并发触发
//     或与 Shutdown 竞态都安全；Start 恢复时还会兜底清理终态 Plan 的残留
//     Team，事件与拆解之间崩溃不留泄漏。
//
// 代价：async Reactor 失败只记日志、不再向 trace 打 KindError。可接受——
// GetPlan/StopPlan 的错误是持久层故障，会在其他路径同样暴露。
func (m *Manager) IsSync() bool  { return false }
func (m *Manager) Priority() int { return 790 }

func (m *Manager) Run(ev trace.Event) error {
	planID := ""
	if ev.Plan != nil {
		planID = ev.Plan.PlanID
	}
	if planID == "" && ev.TaskID != "" && m.deps.Store != nil {
		if task, err := m.deps.Store.GetTask(ev.TaskID); err == nil {
			planID = task.PlanID
		}
	}
	if planID == "" {
		return nil
	}
	p, err := m.coordinator.Store().GetPlan(planID)
	if err != nil {
		return err
	}
	if !model.IsPlanTerminal(p.Status) {
		return nil
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return nil
	}
	reason := "plan_terminal:" + string(p.Status)
	if _, err := m.store.StopPlan(planID, reason); err != nil {
		return err
	}
	for id, team := range m.active {
		if team.spec.PlanID != planID {
			continue
		}
		delete(m.active, id)
		_ = m.routes.UnregisterRoute(team.spec.EventType)
		team.cancel()
		// 永不在这里等待：Run 可能在 registry 的 async goroutine 上执行，
		// 同步等待被 cancel 的 runner 退出会把拆解延迟耦合进事件路径；
		// 保持"取消即返回，清理交给独立 goroutine"。
		m.cleanupWG.Add(1)
		go func(team *activeTeam) {
			defer m.cleanupWG.Done()
			if err := m.waitAndCleanup(team, cleanupMailboxUnregister); err != nil {
				log.Printf("[team] terminal cleanup team=%s: %v", team.spec.ID, err)
			}
		}(team)
	}
	return nil
}

func (m *Manager) validateDependencies() error {
	switch {
	case m.catalog == nil:
		return fmt.Errorf("team manager catalog is nil")
	case m.coordinator == nil:
		return fmt.Errorf("team manager plan coordinator is nil")
	case m.store == nil:
		return fmt.Errorf("team manager store is nil")
	case m.routes == nil:
		return fmt.Errorf("team manager route registry is nil")
	case m.llmFactory == nil:
		return fmt.Errorf("team manager LLM factory is nil")
	case m.deps.Store == nil:
		return fmt.Errorf("team manager runner task store is nil")
	case m.deps.Roster == nil:
		return fmt.Errorf("team manager runner roster is nil")
	default:
		return nil
	}
}

type runtimePreparation struct {
	spec    TeamSpec
	tmpl    *agenttemplate.Template
	clients []llm.Client
}

// runtimeActivation is a fully constructed process-local Team whose runners,
// mailboxes and activity entries exist, but whose goroutines have not started
// and whose Scheduler route has not yet been published.
type runtimeActivation struct {
	team    *activeTeam
	tmpl    *agenttemplate.Template
	runners []*runner.Runner
	ctx     context.Context
	runGate chan struct{}
}

func (m *Manager) prepare(spec TeamSpec, tmpl *agenttemplate.Template) (runtimePreparation, error) {
	clients := make([]llm.Client, spec.Replicas)
	for i := range clients {
		clients[i] = m.llmFactory(tmpl.Model)
		if clients[i] == nil {
			return runtimePreparation{}, fmt.Errorf("LLM factory returned nil for model %q", tmpl.Model)
		}
	}
	return runtimePreparation{spec: spec, tmpl: tmpl, clients: clients}, nil
}

func (m *Manager) materializePrepared(prep runtimePreparation, claimRecoveredMailboxes bool) (runtimeActivation, error) {
	ctx, cancel := context.WithCancel(m.parentCtx)
	agentIDs := teamAgentIDs(prep.spec, prep.tmpl.Name)
	claimedMailboxes := make([]*mailbox.Mailbox, len(agentIDs))
	recoveredClaims := make(map[string]string)
	if claimRecoveredMailboxes && m.deps.MBRegistry != nil {
		for i, agentID := range agentIDs {
			mb, err := m.deps.MBRegistry.ClaimRecovered(agentID, prep.spec.EventType)
			if err != nil {
				cancel()
				var rollbackErr error
				for claimedAgentID, eventType := range recoveredClaims {
					rollbackErr = errors.Join(rollbackErr,
						m.deps.MBRegistry.RollbackRecoveredClaim(claimedAgentID, eventType))
				}
				return runtimeActivation{}, errors.Join(err, rollbackErr)
			}
			if mb != nil {
				claimedMailboxes[i] = mb
				recoveredClaims[agentID] = prep.spec.EventType
			}
		}
	}
	active := &activeTeam{
		spec: prep.spec, agentIDs: agentIDs,
		tools:                  append([]string(nil), prep.tmpl.Tools...),
		recoveredMailboxClaims: recoveredClaims, cancel: cancel,
	}
	runners := make([]*runner.Runner, 0, prep.spec.Replicas)
	for i := 0; i < prep.spec.Replicas; i++ {
		deps := m.deps
		deps.LLMClient = prep.clients[i]
		// 只交接上面已原子认领的邮箱；无快照邮箱和新
		// Provision 都保持 nil，由 runner.New 走严格 Register。
		deps.ClaimedMailbox = claimedMailboxes[i]
		rt := config.AgentRuntimeConfig{
			InstanceID: agentIDs[i], Kind: "template:" + prep.tmpl.Name,
			EventType: prep.spec.EventType, AllowedTools: append([]string(nil), prep.tmpl.Tools...),
			PlanIDScope: prep.spec.PlanID,
			Model:       prep.tmpl.Model, SystemPrompt: prep.tmpl.SystemPrompt,
			AgentMaxLoops: prep.tmpl.AgentMaxLoops, TaskMaxRetries: prep.tmpl.TaskMaxRetries,
			EnforceCompactTokenThreshold: prep.tmpl.EnforceCompactTokenThreshold,
			ContextLimit:                 prep.tmpl.ContextLimit,
			TeamAwareness: fmt.Sprintf("Template team %s has %d homogeneous replicas for: %s",
				prep.tmpl.Ref, prep.spec.Replicas, prep.spec.Purpose),
		}
		runners = append(runners, runner.New(rt, deps))
	}
	active.runners = runners
	return runtimeActivation{
		team: active, tmpl: prep.tmpl, runners: runners, ctx: ctx,
		runGate: make(chan struct{}),
	}, nil
}

func (m *Manager) startMaterialized(activation runtimeActivation) error {
	active := activation.team
	active.wg.Add(len(activation.runners))
	m.active[active.spec.ID] = active
	ready := make(chan struct{}, len(activation.runners))
	for _, rn := range activation.runners {
		go func(rn *runner.Runner) {
			defer active.wg.Done()
			rn.RunWithReady(activation.ctx, func() {
				ready <- struct{}{}
				select {
				case <-activation.runGate:
				case <-activation.ctx.Done():
				}
			})
		}(rn)
	}
	for range activation.runners {
		select {
		case <-ready:
		case <-activation.ctx.Done():
			return activation.ctx.Err()
		}
	}
	return nil
}

func (m *Manager) releaseMaterialized(activation runtimeActivation) {
	close(activation.runGate)
}

func (m *Manager) discardMaterialized(activation runtimeActivation) error {
	if activation.team == nil {
		return nil
	}
	activation.team.cancel()
	return m.waitAndCleanup(activation.team, cleanupMailboxRollbackRecovered)
}

func (m *Manager) stopMaterialized(activation runtimeActivation) error {
	if activation.team == nil {
		return nil
	}
	delete(m.active, activation.team.spec.ID)
	activation.team.cancel()
	return m.waitAndCleanup(activation.team, cleanupMailboxRollbackRecovered)
}

func (m *Manager) markProvisionFailed(spec TeamSpec, reason string, cause error) error {
	if _, err := m.store.SetStatus(spec.ID, StatusStopped, reason); err != nil {
		return fmt.Errorf("%w; additionally failed to mark TeamSpec %s stopped: %v", cause, spec.ID, err)
	}
	return cause
}

func (m *Manager) registerRoute(spec TeamSpec, tmpl *agenttemplate.Template) error {
	role := tmpl.Description
	if role == "" {
		role = "template=" + tmpl.Ref
	}
	if spec.Purpose != "" {
		role = spec.Purpose + " — " + role
	}
	return m.routes.RegisterRoute(spec.EventType, spec.EventType, spec.PlanID, spec.Replicas, role,
		append([]string(nil), tmpl.Tools...))
}

func (m *Manager) activeInstanceCount() int {
	total := 0
	for _, team := range m.active {
		total += team.spec.Replicas
	}
	return total
}

func (m *Manager) waitAndCleanup(team *activeTeam, mailboxMode mailboxCleanupMode) error {
	if team == nil {
		return nil
	}
	team.wg.Wait()
	for _, rn := range team.runners {
		rn.Close()
	}
	var cleanupErr error
	for _, agentID := range team.agentIDs {
		if m.deps.MBRegistry != nil {
			eventType, recovered := team.recoveredMailboxClaims[agentID]
			switch {
			case mailboxMode == cleanupMailboxPreserveForSnapshot:
				// Runner 已停止，邮箱不再被消费；保留到最终快照导出完成。
			case mailboxMode == cleanupMailboxRollbackRecovered && recovered:
				// materialize/route/start 失败不删除已恢复邮箱；只撤销
				// claim，让后续重试能重新认领同一 FIFO 未读队列。
				cleanupErr = errors.Join(cleanupErr,
					m.deps.MBRegistry.RollbackRecoveredClaim(agentID, eventType))
			default:
				_ = m.deps.MBRegistry.Unregister(agentID)
			}
		}
		if m.deps.Activity != nil {
			_ = m.deps.Activity.UnregisterAgent(agentID)
		}
		if m.deps.Roster != nil {
			_ = m.deps.Roster.ReleaseAll(agentID)
		}
	}
	return cleanupErr
}

func findIdempotent(specs []TeamSpec, req agenttemplate.ProvisionRequest) (TeamSpec, bool) {
	for _, spec := range specs {
		if spec.PlanID == req.PlanID && spec.TemplateRef == req.TemplateRef &&
			spec.Purpose == req.Purpose && spec.Replicas == req.Replicas {
			return spec, true
		}
	}
	return TeamSpec{}, false
}

func validateReplicas(tmpl *agenttemplate.Template, replicas int) error {
	if replicas <= 0 {
		return fmt.Errorf("replicas must be positive")
	}
	if tmpl.MaxReplicas > 0 && replicas > tmpl.MaxReplicas {
		return fmt.Errorf("template %s allows at most %d replicas, requested %d",
			tmpl.Ref, tmpl.MaxReplicas, replicas)
	}
	return nil
}

func teamAgentIDs(spec TeamSpec, templateName string) []string {
	ids := make([]string, spec.Replicas)
	for i := range ids {
		// TeamSpec.ID is already the durable unique identity. Do not truncate or
		// normalize it: either operation can make two valid TeamSpecs collide in
		// mailbox/activity/roster registries after recovery.
		ids[i] = fmt.Sprintf("%s-team-%s-%d", templateName, spec.ID, i+1)
	}
	return ids
}

func resultFor(spec TeamSpec, agentIDs, tools []string, reused bool) agenttemplate.ProvisionResult {
	return agenttemplate.ProvisionResult{
		TeamID: spec.ID, EventType: spec.EventType, TemplateRef: spec.TemplateRef,
		TemplateDigest: spec.TemplateDigest, AgentIDs: append([]string(nil), agentIDs...),
		Tools: append([]string(nil), tools...), Replicas: spec.Replicas, Reused: reused,
	}
}

// Compile-time checks keep the public runtime and Reactor contracts aligned.
var _ agenttemplate.Provisioner = (*Manager)(nil)

var _ interface {
	Name() string
	Subscribe() []trace.EventKind
	Run(trace.Event) error
	IsSync() bool
	Priority() int
} = (*Manager)(nil)
