package team

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/config"
	"agentgo/internal/llm"
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

	opMu      sync.Mutex
	parentCtx context.Context
	started   bool
	closed    bool
	active    map[string]*activeTeam
	cleanupWG sync.WaitGroup
}

type activeTeam struct {
	spec     TeamSpec
	agentIDs []string
	tools    []string
	runners  []*runner.Runner
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

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
			return fmt.Errorf("recover team %s template: %w", spec.ID, err)
		}
		if tmpl.Digest != spec.TemplateDigest {
			return fmt.Errorf("%w: team=%s ref=%s stored=%s current=%s",
				ErrTemplateDigestMismatch, spec.ID, spec.TemplateRef, spec.TemplateDigest, tmpl.Digest)
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
		materialized = append(materialized, m.materializePrepared(prep))
	}

	registered := make([]string, 0, len(materialized))
	for _, activation := range materialized {
		if err := m.registerRoute(activation.team.spec, activation.tmpl); err != nil {
			for _, key := range registered {
				_ = m.routes.UnregisterRoute(key)
			}
			for _, candidate := range materialized {
				m.discardMaterialized(candidate)
			}
			m.parentCtx = nil
			return fmt.Errorf("recover team %s route: %w", activation.team.spec.ID, err)
		}
		registered = append(registered, activation.team.spec.EventType)
	}

	for i, activation := range materialized {
		if err := m.startMaterialized(activation); err != nil {
			for _, key := range registered {
				_ = m.routes.UnregisterRoute(key)
			}
			for j, candidate := range materialized {
				if j <= i {
					m.stopMaterialized(candidate)
				} else {
					m.discardMaterialized(candidate)
				}
			}
			m.parentCtx = nil
			return fmt.Errorf("start recovered team %s: %w", activation.team.spec.ID, err)
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
		activation := m.materializePrepared(prep)
		if err := m.startMaterialized(activation); err != nil {
			m.stopMaterialized(activation)
			return m.markProvisionFailed(existing, "provision_failed:runner_start", err)
		}
		if err := m.registerRoute(existing, tmpl); err != nil {
			// RegisterRoute should be all-or-nothing, but remove the key
			// defensively before stopping the runtime in case an implementation
			// returned an error after a partial mutation.
			_ = m.routes.UnregisterRoute(existing.EventType)
			m.stopMaterialized(activation)
			return m.markProvisionFailed(existing, "provision_failed:route_registration",
				fmt.Errorf("register team route: %w", err))
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
	m.opMu.Lock()
	if m.closed {
		m.opMu.Unlock()
		return
	}
	m.closed = true
	m.started = false
	active := make([]*activeTeam, 0, len(m.active))
	for id, team := range m.active {
		delete(m.active, id)
		_ = m.routes.UnregisterRoute(team.spec.EventType)
		team.cancel()
		active = append(active, team)
	}
	m.opMu.Unlock()
	for _, team := range active {
		m.waitAndCleanup(team)
	}
	m.cleanupWG.Wait()
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
		trace.KindTaskCancelled,
	}
}

func (m *Manager) IsSync() bool  { return true }
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
		// Never wait here: a synchronous trace Reactor may be executing on the
		// very runner being cancelled.
		m.cleanupWG.Add(1)
		go func(team *activeTeam) {
			defer m.cleanupWG.Done()
			m.waitAndCleanup(team)
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

func (m *Manager) materializePrepared(prep runtimePreparation) runtimeActivation {
	ctx, cancel := context.WithCancel(m.parentCtx)
	agentIDs := teamAgentIDs(prep.spec, prep.tmpl.Name)
	active := &activeTeam{
		spec: prep.spec, agentIDs: agentIDs,
		tools: append([]string(nil), prep.tmpl.Tools...), cancel: cancel,
	}
	runners := make([]*runner.Runner, 0, prep.spec.Replicas)
	for i := 0; i < prep.spec.Replicas; i++ {
		deps := m.deps
		deps.LLMClient = prep.clients[i]
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
	}
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

func (m *Manager) discardMaterialized(activation runtimeActivation) {
	if activation.team == nil {
		return
	}
	activation.team.cancel()
	m.waitAndCleanup(activation.team)
}

func (m *Manager) stopMaterialized(activation runtimeActivation) {
	if activation.team == nil {
		return
	}
	delete(m.active, activation.team.spec.ID)
	activation.team.cancel()
	m.waitAndCleanup(activation.team)
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

func (m *Manager) waitAndCleanup(team *activeTeam) {
	if team == nil {
		return
	}
	team.wg.Wait()
	for _, rn := range team.runners {
		rn.Close()
	}
	for _, agentID := range team.agentIDs {
		if m.deps.MBRegistry != nil {
			_ = m.deps.MBRegistry.Unregister(agentID)
		}
		if m.deps.Activity != nil {
			_ = m.deps.Activity.UnregisterAgent(agentID)
		}
		if m.deps.Roster != nil {
			_ = m.deps.Roster.ReleaseAll(agentID)
		}
	}
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
