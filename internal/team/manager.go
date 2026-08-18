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
	store        TeamStore
	routes       RouteRegistry
	maxInstances int
	graphState   GraphStateResolver

	opMu         sync.Mutex
	parentCtx    context.Context
	started      bool
	closed       bool
	active       map[string]*activeTeam
	cleanupWG    sync.WaitGroup
	shutdownDone chan struct{}
	// suspending 是一次进行中的 SuspendAll 的完成信号（类比 shutdownDone）：
	// 非 nil 时并发 SuspendAll 等待其关闭，保证任何调用返回时 Manager 已静止。
	// 挂起可重复（解冻 Start 后可再次冻结），故每次挂起新建通道、收尾关闭。
	suspending chan struct{}
	// preservedShutdownMailboxIDs 是 ShutdownPreservingMailboxes 已停止但为
	// 最终 Session 快照暂留的动态邮箱；FinalizeShutdownMailboxes 再删除。
	preservedShutdownMailboxIDs map[string]struct{}
	// suspendedMailboxIDs 是 SuspendAll 已挂起但保留注册的邮箱（语义对照
	// preservedShutdownMailboxIDs，但属于可逆挂起路径）：等 session 快照导出
	// 后由 FinalizeSuspendedMailboxes 注销并清空；Start 成功重新物化后防御性
	// 清空，避免陈旧记录跨入下一个挂起周期。
	suspendedMailboxIDs map[string]struct{}
}

// SetGraphStateResolver installs the durable Graph lifecycle authority. It
// must be called before Start; graph-owned provisioning and recovery fail
// closed without it.
func (m *Manager) SetGraphStateResolver(resolver GraphStateResolver) error {
	if m == nil {
		return fmt.Errorf("team manager is nil")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.started {
		return fmt.Errorf("graph state resolver must be set before team manager start")
	}
	m.graphState = resolver
	return nil
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
		store: store, routes: routes,
		maxInstances: maxInstances, active: make(map[string]*activeTeam),
		shutdownDone: make(chan struct{}),
	}
}

// Start recovers every durable ready TeamSpec. Recovery is fail-closed: a
// changed template digest or an over-limit ready set starts no runners and
// returns an error. Teams whose controller task is already terminal or has
// been evicted from the task store are durably stopped and skipped before
// digest validation.
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
	ready := make([]TeamSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Status != StatusReady {
			continue
		}
		if spec.GraphID != "" {
			if m.graphState == nil {
				return fmt.Errorf("recover graph-owned team %s: graph state resolver is nil", spec.ID)
			}
			status, terminal, exists := m.graphState(spec.GraphID)
			if exists && terminal {
				reason := "graph_terminal:" + status
				log.Printf("[team] 恢复 Team %s 跳过：Graph %s 已终态（%s），已标记 stopped", spec.ID, spec.GraphID, status)
				if _, stopErr := m.store.SetStatus(spec.ID, StatusStopped, reason); stopErr != nil {
					return fmt.Errorf("stop team %s with terminal graph during recovery: %w", spec.ID, stopErr)
				}
				continue
			}
			if exists {
				ready = append(ready, spec)
				continue
			}
			// A crash may happen after provision_agent_team and before submit_graph.
			// Preserve that binding only while its provisioning controller remains
			// nonterminal so the resumed Scheduler can finish the submission.
			task, taskErr := m.deps.Store.GetTask(spec.ControllerTaskID)
			if taskErr == nil && !model.IsTerminal(task.Status) {
				ready = append(ready, spec)
				continue
			}
			log.Printf("[team] 恢复 Team %s 跳过：Graph %s 不存在且 provision controller 不再活跃，已标记 stopped", spec.ID, spec.GraphID)
			if _, stopErr := m.store.SetStatus(spec.ID, StatusStopped, "graph_binding_orphan"); stopErr != nil {
				return fmt.Errorf("stop orphan graph team %s during recovery: %w", spec.ID, stopErr)
			}
			continue
		}

		// Legacy Team belongs to its provisioning Scheduler task.
		task, err := m.deps.Store.GetTask(spec.ControllerTaskID)
		if err != nil {
			log.Printf("[team] 恢复 Team %s 跳过：controller 任务 %s 已淘汰或不存在（%v），已标记 stopped", spec.ID, spec.ControllerTaskID, err)
			if _, stopErr := m.store.StopController(spec.ControllerTaskID, "controller_missing"); stopErr != nil {
				return fmt.Errorf("stop team %s with missing controller during recovery: %w", spec.ID, stopErr)
			}
			continue
		}
		if model.IsTerminal(task.Status) {
			log.Printf("[team] 恢复 Team %s 跳过：controller 任务 %s 已终态（%s），已标记 stopped", spec.ID, spec.ControllerTaskID, task.Status)
			if _, stopErr := m.store.StopController(spec.ControllerTaskID, "controller_terminal:"+string(task.Status)); stopErr != nil {
				return fmt.Errorf("stop team %s with terminal controller during recovery: %w", spec.ID, stopErr)
			}
			continue
		}
		ready = append(ready, spec)
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
	// 防御性清空挂起邮箱记录：正常流程 session 层已在快照导出后调用
	// FinalizeSuspendedMailboxes；此处兜底遗漏，避免陈旧记录跨入下一个
	// 挂起周期（解冻 team 可能已用同 agentID 重新认领 recovered 邮箱，
	// 陈旧记录会让下次 FinalizeSuspendedMailboxes 误注销在用的邮箱）。
	m.suspendedMailboxIDs = nil
	return nil
}

// Provision creates or reuses a homogeneous Team. GraphID 非空时 Graph 是
// lifecycle owner，ControllerTaskID 只保留 provision provenance；否则沿用
// legacy controller-task owner。opMu 把幂等查找、持久化与路由安装串行化。
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
	req.ControllerTaskID = strings.TrimSpace(req.ControllerTaskID)
	req.GraphID = strings.TrimSpace(req.GraphID)
	req.TemplateRef = strings.TrimSpace(req.TemplateRef)
	req.Purpose = strings.TrimSpace(req.Purpose)
	if req.ControllerTaskID == "" || req.TemplateRef == "" || req.Purpose == "" {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("controller_task_id, template_ref and purpose are required")
	}
	if req.GraphID != "" && m.graphState == nil {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("graph-owned team provisioning requires graph state resolver")
	}
	if req.GraphID != "" {
		status, terminal, exists := m.graphState(req.GraphID)
		if exists && terminal {
			return agenttemplate.ProvisionResult{}, fmt.Errorf(
				"cannot provision team for terminal graph %q (status=%s)", req.GraphID, status)
		}
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

	specs, err := m.store.List()
	if err != nil {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("list teams: %w", err)
	}
	existing, found := findIdempotent(specs, req)
	if found && existing.TemplateDigest != tmpl.Digest {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("%w: team=%s ref=%s stored=%s current=%s",
			ErrTemplateDigestMismatch, existing.ID, existing.TemplateRef, existing.TemplateDigest, tmpl.Digest)
	}
	if found && existing.Status != StatusReady {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("idempotent team %s is %s", existing.ID, existing.Status)
	}
	if found {
		if active, ok := m.active[existing.ID]; ok {
			return resultFor(existing, active.agentIDs, active.tools, true), nil
		}
	}

	replicas := req.Replicas
	if m.activeInstanceCount()+replicas > m.maxInstances {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("%w: active=%d requested=%d max=%d",
			ErrProcessLimitExceeded, m.activeInstanceCount(), replicas, m.maxInstances)
	}
	if !found {
		id := uuid.NewString()
		existing = TeamSpec{
			ID: id, TemplateRef: tmpl.Ref, TemplateDigest: tmpl.Digest,
			ControllerTaskID: req.ControllerTaskID,
			GraphID:          req.GraphID,
			Purpose:          req.Purpose, EventType: "team:" + id,
			Replicas: replicas, Status: StatusReady,
		}
	}
	// Durability is established before runner/mailbox construction, and the
	// route is deliberately the final published readiness surface.
	stored, created, err := m.store.Ensure(existing)
	if err != nil {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("persist team: %w", err)
	}
	existing = stored
	if existing.TemplateDigest != tmpl.Digest {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("%w: team=%s ref=%s stored=%s current=%s",
			ErrTemplateDigestMismatch, existing.ID, existing.TemplateRef, existing.TemplateDigest, tmpl.Digest)
	}
	if existing.Status != StatusReady {
		return agenttemplate.ProvisionResult{}, fmt.Errorf("idempotent team %s is %s", existing.ID, existing.Status)
	}
	prep, err := m.prepare(existing, tmpl)
	if err != nil {
		return agenttemplate.ProvisionResult{}, m.markProvisionFailed(existing, "provision_failed:runtime_prepare", err)
	}
	activation, err := m.materializePrepared(prep, false)
	if err != nil {
		return agenttemplate.ProvisionResult{}, m.markProvisionFailed(existing, "provision_failed:runtime_materialize", err)
	}
	if err := m.startMaterialized(activation); err != nil {
		cleanupErr := m.stopMaterialized(activation)
		return agenttemplate.ProvisionResult{}, m.markProvisionFailed(existing, "provision_failed:runner_start", errors.Join(err, cleanupErr))
	}
	if err := m.registerRoute(existing, tmpl); err != nil {
		// RegisterRoute should be all-or-nothing, but remove the key
		// defensively before stopping the runtime in case an implementation
		// returned an error after a partial mutation.
		_ = m.routes.UnregisterRoute(existing.EventType)
		cleanupErr := m.stopMaterialized(activation)
		return agenttemplate.ProvisionResult{}, m.markProvisionFailed(existing, "provision_failed:route_registration",
			errors.Join(fmt.Errorf("register team route: %w", err), cleanupErr))
	}
	m.releaseMaterialized(activation)
	active := m.active[existing.ID]
	if existing.GraphID != "" {
		trace.Emit(trace.Event{
			Kind: trace.KindTeamGraphBound, TaskID: existing.ControllerTaskID, GraphID: existing.GraphID,
			Description: fmt.Sprintf("team_id=%s event_type=%s replicas=%d reused=%t", existing.ID, existing.EventType, existing.Replicas, !created),
		})
	}
	return resultFor(existing, active.agentIDs, active.tools, !created), nil
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

// SuspendAll 冻结当前 session 的全部 team 运行时：取消 route、cancel runner、
// 等待退出，但【保留邮箱注册】（agentID 记入 suspendedMailboxIDs，未读邮件随
// session 快照导出后由 FinalizeSuspendedMailboxes 注销），且【不写任何
// durable 状态】（不调 StopGraph/StopController/SetStatus）——冻结 session 的
// TeamSpec 保持 StatusReady 原样，StatusStopped 只属于生命周期 owner 终态/消失
// 的回收语义（graph_ended、controller 终态、模板失效、恢复核对）。
//
// 冻结协议（对邮箱域 ≡ 进程重启）：SuspendAll（保留邮箱）→ 导出 session
// 快照（邮箱未读随快照归档）→ FinalizeSuspendedMailboxes 注销这些被挂起
// team 的邮箱 → 切换 → 解冻时清全部消息 + ImportSnapshot(目标快照) 使目标
// session 的 team 邮箱成为 recoveredUnclaimed → Start 经 ClaimRecovered 认领。
// 因此 Start 绝不会与「冻结保留的同 agentID 活跃邮箱」发生 fail-closed 冲突。
//
// 与 ShutdownPreservingMailboxes 的差异动机：那是进程关闭的【单向】路径
// （closed=true、close(shutdownDone)、保留的邮箱留给 Finalize 删除）；本方法
// 是 session 冻结的【可逆挂起】——不置 closed、不触碰 shutdownDone、不登记
// preservedShutdownMailboxIDs，仅复位 started=false 与 parentCtx=nil。解冻时
// team store 已被重绑到目标 session 目录（onSessionSwitched 钩子），重新
// Start 即复用进程启动恢复路径，从目标 session 的 ready 清单重新物化。
//
// 并发纪律与 ShutdownPreservingMailboxes 一致：opMu 串行化状态迁移，清理在
// 锁外同步执行，返回前 cleanupWG.Wait() 等齐事件路径派生的后台清理——调用
// 返回时 Manager 静止，可安全导出 session 快照。suspending 通道让并发的重复
// 调用等待首个挂起收尾（类比 Shutdown closed 分支等待 shutdownDone）。
//
// 幂等：无 active team（含从未 Start、重复调用）时仍复位 started/parentCtx
// 并等齐后台清理，但不产生其他副作用；closed（进程关闭进行中/完成后）时退化
// 为等待 ShutdownPreservingMailboxes 完成的 no-op——单向关闭语义优先，其清理
// 是挂起的超集，此处不再重复排空。
//
// 调用方纪律：SuspendAll → FinalizeSuspendedMailboxes →（store 重绑 + 目标
// 邮箱快照导入）→ Start 必须串行（bootstrap 经 snapshotMu 保证）；Start 与
// 进行中的挂起清理交错不在本 API 的保证范围内。若 session 层遗漏 Finalize，
// Start 成功后会防御性清空 suspendedMailboxIDs，但遗漏注销的邮箱本身不在
// 兜底范围内（那是协议违反）。
func (m *Manager) SuspendAll() {
	if m == nil {
		return
	}
	m.opMu.Lock()
	if m.closed {
		// 进程关闭路径先行：挂起请求来得太迟，按 ShutdownPreservingMailboxes
		// closed 分支的同一纪律等待关闭收尾后返回。
		done := m.shutdownDone
		m.opMu.Unlock()
		if done != nil {
			<-done
		} else {
			m.cleanupWG.Wait()
		}
		return
	}
	if m.suspending != nil {
		// 并发重复调用：等待首个挂起完成全部清理（含 cleanupWG）后返回。
		ch := m.suspending
		m.opMu.Unlock()
		<-ch
		return
	}
	// 无论有无 active team 都复位 started/parentCtx：否则「冻结一个零 team 的
	// session 后再解冻」时 Start 会因 started=true 直接返回，不会按重绑后的
	// 清单重新物化。
	m.started = false
	m.parentCtx = nil
	if len(m.active) == 0 {
		m.opMu.Unlock()
		// 无 team 可停，但 Run/stopGraphWithReason 可能刚派生尾部清理；
		// 等齐后返回，保持「返回即静止」的调用契约。
		m.cleanupWG.Wait()
		return
	}
	ch := make(chan struct{})
	m.suspending = ch
	if m.suspendedMailboxIDs == nil {
		m.suspendedMailboxIDs = make(map[string]struct{})
	}
	active := make([]*activeTeam, 0, len(m.active))
	for id, team := range m.active {
		delete(m.active, id)
		_ = m.routes.UnregisterRoute(team.spec.EventType)
		team.cancel()
		// 记录被保留邮箱的 agentID（对照 ShutdownPreservingMailboxes 登记
		// preservedShutdownMailboxIDs）：快照导出后由 FinalizeSuspendedMailboxes
		// 按此清单注销。
		for _, agentID := range team.agentIDs {
			m.suspendedMailboxIDs[agentID] = struct{}{}
		}
		active = append(active, team)
	}
	m.opMu.Unlock()
	log.Printf("[team] session 冻结：挂起 %d 个 active Team 运行时（route/runner 停止，邮箱保留注册，durable 保持 ready）", len(active))
	for _, team := range active {
		// cleanupMailboxPreserveForSnapshot：冻结期间邮箱必须保留注册，不得走
		// cleanupMailboxUnregister；activity/roster 随 runner 停止走常规清理。
		if err := m.waitAndCleanup(team, cleanupMailboxPreserveForSnapshot); err != nil {
			log.Printf("[team] suspend cleanup team=%s: %v", team.spec.ID, err)
		}
	}
	// 与 ShutdownPreservingMailboxes 同一纪律：等齐事件路径派生的后台清理，
	// 但绝不 close(shutdownDone)——那是进程关闭路径专属的完成信号。
	m.cleanupWG.Wait()
	m.opMu.Lock()
	m.suspending = nil
	m.opMu.Unlock()
	close(ch)
}

// FinalizeSuspendedMailboxes 注销 SuspendAll 为快照导出暂留的挂起 team 邮箱，
// 并清空记录。它是冻结协议的一环，调用时序：session 层必须在 session 快照
// 导出【之后】、公告板替换/切换【之前】调用——此后解冻路径（清全部消息 +
// ImportSnapshot(目标快照) + Start 的 ClaimRecovered）面对的邮箱域与进程
// 重启后完全一致。
//
// 幂等；无记录时是 no-op。closed（进程关闭进行中/完成后）时退化为纯 no-op
// （连记录也不清）：最终快照由 ShutdownPreservingMailboxes /
// FinalizeShutdownMailboxes 两阶段负责，本方法不与进程关闭路径争抢。
func (m *Manager) FinalizeSuspendedMailboxes() {
	if m == nil {
		return
	}
	m.opMu.Lock()
	if m.closed {
		m.opMu.Unlock()
		return
	}
	ids := make([]string, 0, len(m.suspendedMailboxIDs))
	for agentID := range m.suspendedMailboxIDs {
		ids = append(ids, agentID)
	}
	m.suspendedMailboxIDs = nil
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

// Reactor implementation. Scheduler controller 任务的终态事件触发其名下全部
// active Team 的拆除；Start 恢复路径兜底清理事件丢失期间残留的 Team。
func (m *Manager) Name() string { return "agent-template-team-manager" }

func (m *Manager) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindTaskCompleted,
		trace.KindTaskFailed,
		trace.KindTaskBlocked,
		trace.KindTaskCancelled,
		trace.KindGraphEnded,
	}
}

// Team lifecycle stays off trace.Emit because Store fsync may be slow, but it
// uses Registry's non-dropping reliable async lane rather than the lossy
// observational semaphore.
func (m *Manager) IsSync() bool { return false }

func (m *Manager) ReliableAsync() bool { return true }

func (m *Manager) Priority() int { return 790 }

func (m *Manager) Run(ev trace.Event) error {
	if ev.Kind == trace.KindGraphEnded {
		return m.stopGraph(ev.GraphID)
	}
	if ev.TaskID == "" || m.deps.Store == nil {
		return nil
	}
	task, err := m.deps.Store.GetTask(ev.TaskID)
	if err != nil {
		// 任务已淘汰/不存在时无从判定其是否为 controller 终态；残留 Team
		// 由 Start 恢复路径（controller_missing）兜底清理。
		return nil
	}
	if task.EventType != "__scheduler__" || !model.IsTerminal(task.Status) {
		return nil
	}
	// A Graph-scoped Team normally outlives this provisioning controller. The
	// only exception is a binding whose Graph never became durable (for example
	// submit_graph was rejected and the controller then terminated). Clean that
	// orphan immediately instead of leaking it until process restart.
	if m.graphState != nil {
		specs, listErr := m.store.List()
		if listErr != nil {
			return listErr
		}
		seenGraphs := make(map[string]struct{})
		for _, spec := range specs {
			if spec.Status != StatusReady || spec.GraphID == "" || spec.ControllerTaskID != task.ID {
				continue
			}
			if _, seen := seenGraphs[spec.GraphID]; seen {
				continue
			}
			seenGraphs[spec.GraphID] = struct{}{}
			status, terminal, exists := m.graphState(spec.GraphID)
			switch {
			case exists && terminal:
				if err := m.stopGraphWithReason(spec.GraphID, "graph_terminal:"+status); err != nil {
					return err
				}
			case !exists:
				if err := m.stopGraphWithReason(spec.GraphID, "graph_binding_orphan"); err != nil {
					return err
				}
			}
		}
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return nil
	}
	if !m.started {
		// 挂起窗口（SuspendAll 之后、Start 重新物化之前；进程未 Start 亦然）
		// 不得写 durable 回收状态：冻结 session 的 TeamSpec 必须保持 ready
		// 原样解冻。owner 在冻结期间到达终态的 Team，由解冻时 Start 的恢复
		// 核对（controller_terminal / controller_missing 分支）统一回收；
		// 此窗口 active 恒为空，运行时侧本来就无事可做。
		return nil
	}
	reason := "controller_terminal:" + string(task.Status)
	if _, err := m.store.StopController(task.ID, reason); err != nil {
		return err
	}
	for id, team := range m.active {
		if team.spec.GraphID != "" || team.spec.ControllerTaskID != task.ID {
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
				log.Printf("[team] controller terminal cleanup team=%s: %v", team.spec.ID, err)
			}
		}(team)
	}
	return nil
}

func (m *Manager) stopGraph(graphID string) error {
	graphID = strings.TrimSpace(graphID)
	if graphID == "" {
		return nil
	}
	status := "unknown"
	if m.graphState != nil {
		if resolved, terminal, exists := m.graphState(graphID); exists && terminal && resolved != "" {
			status = resolved
		}
	}
	reason := "graph_terminal:" + status
	return m.stopGraphWithReason(graphID, reason)
}

func (m *Manager) stopGraphWithReason(graphID, reason string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.closed {
		return nil
	}
	if !m.started {
		// 挂起窗口（含 graph_ended 与 orphan 扫描两个入口）：同 Run 的
		// controller 终态段，不得写 durable 回收状态；graph_terminal /
		// graph_binding_orphan 由解冻时 Start 的恢复核对兜底。
		return nil
	}
	_, err := m.store.StopGraph(graphID, reason)
	if err != nil {
		return err
	}
	var stoppedActive []TeamSpec
	for id, team := range m.active {
		if team.spec.GraphID != graphID {
			continue
		}
		delete(m.active, id)
		_ = m.routes.UnregisterRoute(team.spec.EventType)
		team.cancel()
		stoppedActive = append(stoppedActive, team.spec)
		m.cleanupWG.Add(1)
		go func(team *activeTeam) {
			defer m.cleanupWG.Done()
			if err := m.waitAndCleanup(team, cleanupMailboxUnregister); err != nil {
				log.Printf("[team] graph terminal cleanup team=%s: %v", team.spec.ID, err)
			}
		}(team)
	}
	for _, spec := range stoppedActive {
		trace.Emit(trace.Event{
			Kind: trace.KindTeamStopped, GraphID: graphID, Reason: reason,
			Description: fmt.Sprintf("team_id=%s event_type=%s", spec.ID, spec.EventType),
		})
	}
	return nil
}

func (m *Manager) validateDependencies() error {
	switch {
	case m.catalog == nil:
		return fmt.Errorf("team manager catalog is nil")
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
			Model: prep.tmpl.Model, SystemPrompt: prep.tmpl.SystemPrompt,
			TaskMaxRetries:               prep.tmpl.TaskMaxRetries,
			EnforceCompactTokenThreshold: prep.tmpl.EnforceCompactTokenThreshold,
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
	ownerScope := model.TaskRouteScope(spec.ControllerTaskID)
	if spec.GraphID != "" {
		ownerScope = model.GraphRouteScope(spec.GraphID)
	}
	if err := m.routes.RegisterRoute(spec.EventType, spec.EventType, ownerScope, spec.Replicas, role,
		append([]string(nil), tmpl.Tools...)); err != nil {
		return err
	}
	if err := m.routes.BindRouteClaimants(spec.EventType, teamAgentIDs(spec, tmpl.Name)); err != nil {
		_ = m.routes.UnregisterRoute(spec.EventType)
		return fmt.Errorf("bind team route claimants: %w", err)
	}
	return nil
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
		ownerMatches := spec.GraphID == req.GraphID
		if req.GraphID == "" {
			ownerMatches = spec.GraphID == "" && spec.ControllerTaskID == req.ControllerTaskID
		}
		if ownerMatches && spec.TemplateRef == req.TemplateRef &&
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
		GraphID:        spec.GraphID,
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
