package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	reactorbuiltin "agentgo/internal/reactor/builtin"
	"agentgo/internal/roster"
	"agentgo/internal/runner"
	"agentgo/internal/session"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

type idleLLM struct{}

func (idleLLM) Chat(context.Context, []llm.Message, []llm.ToolDef) (llm.Response, error) {
	return llm.Response{}, errors.New("idle test LLM must not be called")
}

type routeRecord struct {
	eventType    string
	planID       string
	count        int
	role         string
	capabilities []string
}

type fakeRoutes struct {
	mu             sync.Mutex
	routes         map[string]routeRecord
	registers      int
	beforeRegister func(key, eventType string, count int) error
}

func newFakeRoutes() *fakeRoutes { return &fakeRoutes{routes: make(map[string]routeRecord)} }

func (r *fakeRoutes) RegisterRoute(key, eventType, planID string, count int, role string, capabilities []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.beforeRegister != nil {
		if err := r.beforeRegister(key, eventType, count); err != nil {
			return err
		}
	}
	if _, exists := r.routes[key]; exists {
		return errors.New("duplicate route")
	}
	r.routes[key] = routeRecord{
		eventType: eventType, planID: planID, count: count, role: role,
		capabilities: append([]string(nil), capabilities...),
	}
	r.registers++
	return nil
}

type blockingEnsureStore struct {
	TeamStore
	entered chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

type queryObservingStore struct {
	store.TaskStore
	mu      sync.Mutex
	queries map[string]int
}

type failingQueryStore struct {
	store.TaskStore
	entered chan struct{}
	once    sync.Once
}

func (s *failingQueryStore) QueryAvailable(string) ([]*model.Task, error) {
	s.once.Do(func() { close(s.entered) })
	return nil, errors.New("simulated query failure")
}

func (s *queryObservingStore) QueryAvailable(eventType string) ([]*model.Task, error) {
	s.mu.Lock()
	s.queries[eventType]++
	s.mu.Unlock()
	return s.TaskStore.QueryAvailable(eventType)
}

func (s *queryObservingStore) queryCount(eventType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries[eventType]
}

func (s *blockingEnsureStore) Ensure(TeamSpec) (TeamSpec, bool, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return TeamSpec{}, false, s.err
}

func (r *fakeRoutes) UnregisterRoute(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.routes[key]
	delete(r.routes, key)
	return existed
}

func (r *fakeRoutes) snapshot() (map[string]routeRecord, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]routeRecord, len(r.routes))
	for k, v := range r.routes {
		v.capabilities = append([]string(nil), v.capabilities...)
		out[k] = v
	}
	return out, r.registers
}

func TestManagerProvisionIdempotenceLimitsShutdownAndRecovery(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-provision")
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, coordinator, durable, routes, 2)
	req := agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "map the codebase", Replicas: 2,
	}
	if _, err := manager.Provision(context.Background(), req); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Provision before Start err=%v, want ErrNotStarted", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first, err := manager.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("Provision(first): %v", err)
	}
	if first.Reused || first.Replicas != 2 || len(first.AgentIDs) != 2 || first.EventType != "team:"+first.TeamID {
		t.Fatalf("first result mismatch: %+v", first)
	}
	if manager.ActiveCount() != 2 {
		t.Fatalf("ActiveCount=%d, want 2", manager.ActiveCount())
	}
	routeSnapshot, registerCount := routes.snapshot()
	route := routeSnapshot[first.EventType]
	if route.planID != planID || route.count != 2 || !contains(route.capabilities, "read_file") || contains(route.capabilities, "code-investigation") {
		t.Fatalf("route must expose concrete tools, got %+v", route)
	}

	second, err := manager.Provision(context.Background(), req)
	if err != nil {
		t.Fatalf("Provision(idempotent): %v", err)
	}
	if !second.Reused || second.TeamID != first.TeamID || second.EventType != first.EventType {
		t.Fatalf("idempotent result mismatch: first=%+v second=%+v", first, second)
	}
	_, afterReuseRegisters := routes.snapshot()
	if afterReuseRegisters != registerCount {
		t.Fatalf("idempotent Provision registered another route: %d -> %d", registerCount, afterReuseRegisters)
	}

	_, err = manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/generalist@1", Purpose: "implement", Replicas: 1,
	})
	if !errors.Is(err, ErrProcessLimitExceeded) {
		t.Fatalf("process limit err=%v, want ErrProcessLimitExceeded", err)
	}
	_, err = manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/verifier@1", Purpose: "accept", Replicas: 2,
	})
	if err == nil {
		t.Fatal("verifier template accepted replicas above template max")
	}

	manager.Shutdown()
	stored, err := durable.Get(first.TeamID)
	if err != nil || stored.Status != StatusReady {
		t.Fatalf("Shutdown must preserve ready state: stored=%+v err=%v", stored, err)
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("Shutdown left runtime routes: %+v", current)
	}

	recoveredRoutes := newFakeRoutes()
	recovered := testManager(t, catalog, coordinator, durable, recoveredRoutes, 2)
	if err := recovered.Start(context.Background()); err != nil {
		t.Fatalf("recovery Start: %v", err)
	}
	if recovered.ActiveCount() != 2 {
		t.Fatalf("recovered ActiveCount=%d, want 2", recovered.ActiveCount())
	}
	if current, _ := recoveredRoutes.snapshot(); len(current) != 1 {
		t.Fatalf("recovery routes=%+v, want one", current)
	}
	recovered.Shutdown()
}

func TestManagerRecoveryClaimsV4UnreadMailboxWithoutDuplicateRegistration(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-mailbox-recovery")
	durable := NewMemoryStore()

	// 先产生一个持久化 ready TeamSpec，以获得真实的稳定 agentID。
	first := testManager(t, catalog, coordinator, durable, newFakeRoutes(), 1)
	t.Cleanup(first.Shutdown)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	result, err := first.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "recover unread mail", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	first.Shutdown()

	mailboxes := mailbox.NewRegistry(8)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := mailboxes.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: result.AgentIDs[0], EventType: result.EventType,
		// v4 持久化顺序是最新在前。
		Messages: []session.MessageSnapshot{
			{From: "scheduler", To: result.AgentIDs[0], Summary: "second", SentAt: now},
			{From: "scheduler", To: result.AgentIDs[0], Summary: "first", SentAt: now},
		},
	}}); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}

	deps := runner.RunnerDeps{
		Store:  store.NewMemoryTaskStore(nil, 100, 1, 30),
		Roster: roster.NewMemoryRoster(), MBRegistry: mailboxes,
		PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	recovered := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, durable, newFakeRoutes(), 1)
	t.Cleanup(recovered.Shutdown)
	if err := recovered.Start(context.Background()); err != nil {
		t.Fatalf("recovery Start: %v", err)
	}

	active, ok := recovered.active[result.TeamID]
	if !ok || len(active.runners) != 1 {
		t.Fatalf("recovered active Team mismatch: %+v", active)
	}
	msgs := active.runners[0].Agent().Mailbox.Drain()
	if len(msgs) != 2 || msgs[0].Summary != "first" || msgs[1].Summary != "second" {
		t.Fatalf("recovered unread FIFO mismatch: %+v", msgs)
	}
	if claimed, err := mailboxes.ClaimRecovered(result.AgentIDs[0], result.EventType); !errors.Is(err, mailbox.ErrRecoveredMailboxConflict) || claimed != nil {
		t.Fatalf("active Team mailbox re-claim = (%v, %v), want conflict", claimed, err)
	}
}

func TestManagerShutdownPreservesUnreadMailboxUntilFinalSnapshot(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-mailbox-shutdown-snapshot")
	durable := NewMemoryStore()
	mailboxes := mailbox.NewRegistry(8)
	deps := runner.RunnerDeps{
		Store:  store.NewMemoryTaskStore(nil, 100, 1, 30),
		Roster: roster.NewMemoryRoster(), MBRegistry: mailboxes,
		PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, durable, newFakeRoutes(), 1)
	t.Cleanup(manager.Shutdown)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "persist unread shutdown mail", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	agentID := result.AgentIDs[0]
	for _, summary := range []string{"first", "second"} {
		if err := mailboxes.Send(mailbox.Message{
			From: "scheduler", To: agentID, Summary: summary, Content: summary,
			SentAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Send(%s): %v", summary, err)
		}
	}

	manager.ShutdownPreservingMailboxes()
	foundUnread := false
	for _, status := range mailboxes.ScanNonEmpty() {
		if status.AgentID == agentID && status.Count == 2 {
			foundUnread = true
			break
		}
	}
	if !foundUnread {
		t.Fatalf("pre-snapshot shutdown removed Team unread mailbox: %+v", mailboxes.ScanNonEmpty())
	}
	snaps := mailboxes.ExportSnapshot()
	if len(snaps) != 1 || snaps[0].OwnerID != agentID || len(snaps[0].Messages) != 2 {
		t.Fatalf("final snapshot missing Team unread mailbox: %+v", snaps)
	}

	// 模拟下一进程导入最终快照：稳定 ID 可重新认领，FIFO 不变。
	restarted := mailbox.NewRegistry(8)
	if err := restarted.ImportSnapshot(snaps); err != nil {
		t.Fatalf("restart ImportSnapshot: %v", err)
	}
	recovered, err := restarted.ClaimRecovered(agentID, result.EventType)
	if err != nil || recovered == nil {
		t.Fatalf("restart ClaimRecovered = (%v, %v)", recovered, err)
	}
	msgs := recovered.Drain()
	if len(msgs) != 2 || msgs[0].Summary != "first" || msgs[1].Summary != "second" {
		t.Fatalf("shutdown/restart FIFO mismatch: %+v", msgs)
	}

	manager.FinalizeShutdownMailboxes()
	for _, status := range mailboxes.ScanAll() {
		if status.AgentID == agentID {
			t.Fatal("FinalizeShutdownMailboxes did not unregister preserved mailbox")
		}
	}
}

func TestManagerRecoveryStartFailureRollsBackMailboxClaim(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-mailbox-start-rollback")
	durable := NewMemoryStore()
	first := testManager(t, catalog, coordinator, durable, newFakeRoutes(), 1)
	t.Cleanup(first.Shutdown)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	result, err := first.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "rollback unread mail", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	first.Shutdown()

	mailboxes := mailbox.NewRegistry(8)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := mailboxes.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: result.AgentIDs[0], EventType: result.EventType,
		Messages: []session.MessageSnapshot{
			{From: "scheduler", To: result.AgentIDs[0], Summary: "second", SentAt: now},
			{From: "scheduler", To: result.AgentIDs[0], Summary: "first", SentAt: now},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	queryStore := &failingQueryStore{
		TaskStore: store.NewMemoryTaskStore(nil, 100, 1, 30),
		entered:   make(chan struct{}),
	}
	activity := agent.NewActivityTracker()
	deps := runner.RunnerDeps{
		Store: queryStore, Roster: roster.NewMemoryRoster(), Activity: activity,
		MBRegistry: mailboxes, PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	recovered := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, durable, newFakeRoutes(), 1)
	t.Cleanup(recovered.Shutdown)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-queryStore.entered
		cancel()
	}()
	if err := recovered.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("recovery Start err=%v, want context.Canceled", err)
	}
	if got := len(activity.Snapshots()); got != 0 {
		t.Fatalf("failed Start left %d activity entries", got)
	}

	// startMaterialized 失败后必须可以重新认领同一邮箱，
	// 且未读邮件不得被 Unregister 丢失或改变 FIFO。
	reclaimed, err := mailboxes.ClaimRecovered(result.AgentIDs[0], result.EventType)
	if err != nil || reclaimed == nil {
		t.Fatalf("reclaim after failed Start = (%v, %v)", reclaimed, err)
	}
	msgs := reclaimed.Drain()
	if len(msgs) != 2 || msgs[0].Summary != "first" || msgs[1].Summary != "second" {
		t.Fatalf("failed Start changed recovered FIFO: %+v", msgs)
	}
}

func TestManagerDiscardBeforeStartRollsBackRecoveredMailboxClaim(t *testing.T) {
	mailboxes := mailbox.NewRegistry(4)
	now := time.Now().UTC().Format(time.RFC3339)
	if err := mailboxes.ImportSnapshot([]session.MailboxSnapshot{{
		OwnerID: "explorer-team-route-failure-1", EventType: "team:route-failure",
		Messages: []session.MessageSnapshot{
			{From: "scheduler", To: "explorer-team-route-failure-1", Summary: "second", SentAt: now},
			{From: "scheduler", To: "explorer-team-route-failure-1", Summary: "first", SentAt: now},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := mailboxes.ClaimRecovered("explorer-team-route-failure-1", "team:route-failure")
	if err != nil || claimed == nil {
		t.Fatalf("initial claim = (%v, %v)", claimed, err)
	}

	_, cancel := context.WithCancel(context.Background())
	manager := &Manager{deps: runner.RunnerDeps{MBRegistry: mailboxes}}
	activation := runtimeActivation{team: &activeTeam{
		spec:     TeamSpec{ID: "route-failure"},
		agentIDs: []string{"explorer-team-route-failure-1"},
		recoveredMailboxClaims: map[string]string{
			"explorer-team-route-failure-1": "team:route-failure",
		},
		cancel: cancel,
	}}
	if err := manager.discardMaterialized(activation); err != nil {
		t.Fatalf("discardMaterialized: %v", err)
	}
	reclaimed, err := mailboxes.ClaimRecovered("explorer-team-route-failure-1", "team:route-failure")
	if err != nil || reclaimed != claimed {
		t.Fatalf("reclaim after pre-start discard = (%v, %v), want %v", reclaimed, err, claimed)
	}
	msgs := reclaimed.Drain()
	if len(msgs) != 2 || msgs[0].Summary != "first" || msgs[1].Summary != "second" {
		t.Fatalf("pre-start discard changed recovered FIFO: %+v", msgs)
	}
}

func TestManagerProvisionPublishesRouteAfterDurableRuntimeReady(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-ready-order")
	durable := NewMemoryStore()
	mailboxes := mailbox.NewRegistry(8)
	activity := agent.NewActivityTracker()
	taskStore := &queryObservingStore{
		TaskStore: store.NewMemoryTaskStore(nil, 100, 1, 30),
		queries:   make(map[string]int),
	}
	routes := newFakeRoutes()
	routes.beforeRegister = func(_ string, eventType string, count int) error {
		teamID := strings.TrimPrefix(eventType, "team:")
		if _, err := durable.Get(teamID); err != nil {
			return fmt.Errorf("route published before TeamSpec durability: %w", err)
		}
		statuses := mailboxes.ScanAll()
		if len(statuses) != count {
			return fmt.Errorf("route published before all mailboxes were ready: got=%d want=%d", len(statuses), count)
		}
		for _, status := range statuses {
			if status.EventType != eventType {
				return fmt.Errorf("mailbox %s has route %q, want %q", status.AgentID, status.EventType, eventType)
			}
		}
		if got := len(activity.Snapshots()); got != count {
			return fmt.Errorf("route published before all runners registered activity: got=%d want=%d", got, count)
		}
		if got := taskStore.queryCount(eventType); got < count {
			return fmt.Errorf("route published before all runners entered claim loop: got=%d want=%d", got, count)
		}
		return nil
	}
	deps := runner.RunnerDeps{
		Store:  taskStore,
		Roster: roster.NewMemoryRoster(), Activity: activity, MBRegistry: mailboxes,
		PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, durable, routes, 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()

	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "inspect", Replicas: 2,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := durable.Get(result.TeamID); err != nil {
		t.Fatalf("ready route has no durable TeamSpec: %v", err)
	}
}

func TestManagerProvisionRouteFailureStopsSpecAndCleansRuntime(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-route-failure")
	durable := NewMemoryStore()
	routeErr := errors.New("simulated route registration failure")
	routes := newFakeRoutes()
	routes.beforeRegister = func(string, string, int) error { return routeErr }
	mailboxes := mailbox.NewRegistry(8)
	activity := agent.NewActivityTracker()
	deps := runner.RunnerDeps{
		Store:  store.NewMemoryTaskStore(nil, 100, 1, 30),
		Roster: roster.NewMemoryRoster(), Activity: activity, MBRegistry: mailboxes,
		PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, durable, routes, 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
	})
	if !errors.Is(err, routeErr) {
		t.Fatalf("Provision err=%v, want route registration failure", err)
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("failed route registration remained observable: %+v", current)
	}
	if got := mailboxes.ScanAll(); len(got) != 0 {
		t.Fatalf("failed route registration left mailboxes: %+v", got)
	}
	if got := len(activity.Snapshots()); got != 0 {
		t.Fatalf("failed route registration left %d activity entries", got)
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("failed route registration left %d active agents", got)
	}
	specs, err := durable.List()
	if err != nil || len(specs) != 1 {
		t.Fatalf("durable specs=%+v err=%v, want one explicit failed record", specs, err)
	}
	if specs[0].Status != StatusStopped || specs[0].StopReason != "provision_failed:route_registration" {
		t.Fatalf("failed TeamSpec remained recoverable: %+v", specs[0])
	}
	manager.Shutdown()

	recoveredRoutes := newFakeRoutes()
	recovered := testManager(t, catalog, coordinator, durable, recoveredRoutes, 2)
	if err := recovered.Start(context.Background()); err != nil {
		t.Fatalf("recovery after failed provision: %v", err)
	}
	if recovered.ActiveCount() != 0 {
		t.Fatalf("failed TeamSpec recovered %d ghost agents", recovered.ActiveCount())
	}
	if current, _ := recoveredRoutes.snapshot(); len(current) != 0 {
		t.Fatalf("failed TeamSpec recovered ghost route: %+v", current)
	}
	recovered.Shutdown()
}

func TestManagerProvisionPersistenceFailureNeverExposesRoute(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-persist-failure")
	persistErr := errors.New("simulated persistence failure")
	storeBackend := &blockingEnsureStore{
		TeamStore: NewMemoryStore(), entered: make(chan struct{}),
		release: make(chan struct{}), err: persistErr,
	}
	routes := newFakeRoutes()
	mailboxes := mailbox.NewRegistry(8)
	activity := agent.NewActivityTracker()
	deps := runner.RunnerDeps{
		Store:  store.NewMemoryTaskStore(nil, 100, 1, 30),
		Roster: roster.NewMemoryRoster(), Activity: activity, MBRegistry: mailboxes,
		PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, storeBackend, routes, 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(storeBackend.release) }) }
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
			PlanID: planID, ControllerTaskID: controllerID,
			TemplateRef: "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
		})
		done <- err
	}()
	select {
	case <-storeBackend.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Provision did not reach durable Ensure")
	}
	if current, registers := routes.snapshot(); len(current) != 0 || registers != 0 {
		t.Fatalf("route became observable while persistence was pending: routes=%+v registers=%d", current, registers)
	}
	if got := mailboxes.ScanAll(); len(got) != 0 {
		t.Fatalf("runtime materialized before durability: mailboxes=%+v", got)
	}

	release()
	select {
	case err := <-done:
		if !errors.Is(err, persistErr) {
			t.Fatalf("Provision err=%v, want persistence error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Provision did not return after persistence failure")
	}
	if current, registers := routes.snapshot(); len(current) != 0 || registers != 0 {
		t.Fatalf("failed Provision exposed a route: routes=%+v registers=%d", current, registers)
	}
	if got := mailboxes.ScanAll(); len(got) != 0 {
		t.Fatalf("failed Provision left runtime mailboxes: %+v", got)
	}
	if got := len(activity.Snapshots()); got != 0 {
		t.Fatalf("failed Provision left %d activity entries", got)
	}
	if got := manager.ActiveCount(); got != 0 {
		t.Fatalf("failed Provision left %d active agents", got)
	}
}

func TestManagerRecoveryDigestMismatchFailsClosed(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-digest")
	durable := NewMemoryStore()
	spec := testSpec("digest-team", controllerID, "investigate")
	spec.PlanID = planID
	spec.TemplateDigest = "sha256:stale"
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatalf("persist stale spec: %v", err)
	}
	routes := newFakeRoutes()
	manager := testManager(t, catalog, coordinator, durable, routes, 4)
	err := manager.Start(context.Background())
	if !errors.Is(err, ErrTemplateDigestMismatch) {
		t.Fatalf("Start err=%v, want ErrTemplateDigestMismatch", err)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("digest mismatch started %d agents", manager.ActiveCount())
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("digest mismatch installed routes: %+v", current)
	}
}

func TestManagerProvisionRejectsDurableDigestDrift(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-live-digest")
	durable := NewMemoryStore()
	manager := testManager(t, catalog, coordinator, durable, newFakeRoutes(), 4)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	spec := testSpec("live-digest-team", controllerID, "investigate")
	spec.PlanID = planID
	spec.TemplateDigest = "sha256:stale"
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "investigate", Replicas: 2,
	})
	if !errors.Is(err, ErrTemplateDigestMismatch) {
		t.Fatalf("Provision err=%v, want ErrTemplateDigestMismatch", err)
	}
}

func TestManagerTerminalPlanReactorStopsTeamAndRecoverySkipsIt(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-terminal")
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, coordinator, durable, routes, 2)
	subscribesTerminal := false
	for _, kind := range manager.Subscribe() {
		if kind == trace.KindPlanTerminal {
			subscribesTerminal = true
			break
		}
	}
	if !subscribesTerminal {
		t.Fatal("TeamManager does not subscribe to plan_terminal")
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := coordinator.CompleteWithoutExecution(context.Background(), planID); err != nil {
		t.Fatalf("CompleteWithoutExecution: %v", err)
	}
	if err := manager.Run(trace.Event{
		Kind: trace.KindPlanTerminal,
		Plan: &trace.PlanTraceContext{PlanID: planID},
	}); err != nil {
		t.Fatalf("terminal Reactor: %v", err)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("terminal Reactor left %d active agents", manager.ActiveCount())
	}
	stored, err := durable.Get(result.TeamID)
	if err != nil || stored.Status != StatusStopped {
		t.Fatalf("terminal state not persisted: stored=%+v err=%v", stored, err)
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("terminal Reactor left routes: %+v", current)
	}
	manager.Shutdown()

	// Even a now-stale digest is ignored for a terminal Plan: stopped teams
	// must never be recovered simply because their old template changed.
	recoveredRoutes := newFakeRoutes()
	recovered := testManager(t, catalog, coordinator, durable, recoveredRoutes, 2)
	if err := recovered.Start(context.Background()); err != nil {
		t.Fatalf("terminal recovery: %v", err)
	}
	if recovered.ActiveCount() != 0 {
		t.Fatalf("terminal recovery started %d agents", recovered.ActiveCount())
	}
	recovered.Shutdown()
}

func TestManagerShutdownRemovesDynamicRuntimeSurfaces(t *testing.T) {
	catalog := testCatalog(t)
	coordinator, planID, controllerID := testPlan(t, "plan-cleanup")
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	activity := agent.NewActivityTracker()
	mailboxes := mailbox.NewRegistry(8)
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), Activity: activity,
		MBRegistry: mailboxes, PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, NewMemoryStore(), newFakeRoutes(), 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		PlanID: planID, ControllerTaskID: controllerID,
		TemplateRef: "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	agentID := result.AgentIDs[0]
	if _, ok := activity.Snapshot(agentID); !ok {
		t.Fatalf("dynamic agent %s missing from activity tracker", agentID)
	}
	if len(mailboxes.ScanAll()) != 1 {
		t.Fatalf("mailboxes before Shutdown=%+v, want one", mailboxes.ScanAll())
	}

	manager.Shutdown()
	if _, ok := activity.Snapshot(agentID); ok {
		t.Fatalf("Shutdown left ghost activity for %s", agentID)
	}
	if got := mailboxes.ScanAll(); len(got) != 0 {
		t.Fatalf("Shutdown left ghost mailboxes: %+v", got)
	}
}

func TestManagerRepeatedTerminalTeamsReleaseTaskEndCallbacks(t *testing.T) {
	catalog := testCatalog(t)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	callbacks := reactorbuiltin.NewTaskEndCallbackReactor()
	deps := runner.RunnerDeps{
		Store: store.NewMemoryTaskStore(nil, 100, 1, 30), Roster: roster.NewMemoryRoster(),
		PlanCoordinator: coordinator, TaskEndCallbacks: callbacks, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, NewMemoryStore(), newFakeRoutes(), 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Shutdown()

	for round := 0; round < 5; round++ {
		planID := fmt.Sprintf("callback-plan-%d", round)
		controllerID := "controller-" + planID
		if _, err := coordinator.Create(context.Background(), plan.CreateInput{
			PlanID: planID, RootTaskID: controllerID,
		}); err != nil {
			t.Fatalf("round %d create plan: %v", round, err)
		}
		if _, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
			PlanID: planID, ControllerTaskID: controllerID,
			TemplateRef: "builtin/explorer@1", Purpose: fmt.Sprintf("round-%d", round), Replicas: 2,
		}); err != nil {
			t.Fatalf("round %d Provision: %v", round, err)
		}
		if got := callbacks.CallbackCount(); got != 2 {
			t.Fatalf("round %d live callback count=%d, want 2", round, got)
		}
		if _, err := coordinator.CompleteWithoutExecution(context.Background(), planID); err != nil {
			t.Fatalf("round %d terminal plan: %v", round, err)
		}
		if err := manager.Run(trace.Event{
			Kind: trace.KindAcceptanceCompleted,
			Plan: &trace.PlanTraceContext{PlanID: planID},
		}); err != nil {
			t.Fatalf("round %d terminal Reactor: %v", round, err)
		}
		deadline := time.Now().Add(time.Second)
		for callbacks.CallbackCount() != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := callbacks.CallbackCount(); got != 0 {
			t.Fatalf("round %d cleanup left %d task-end callbacks", round, got)
		}
	}
}

func TestTeamAgentIDsPreserveCompleteDurableTeamIdentity(t *testing.T) {
	left := teamAgentIDs(TeamSpec{ID: "12345678-aaaa"}, "generalist")
	right := teamAgentIDs(TeamSpec{ID: "12345678-bbbb"}, "generalist")
	if len(left) != 0 || len(right) != 0 {
		t.Fatalf("zero-replica helper should produce no IDs: left=%v right=%v", left, right)
	}
	left = teamAgentIDs(TeamSpec{ID: "12345678-aaaa", Replicas: 1}, "generalist")
	right = teamAgentIDs(TeamSpec{ID: "12345678-bbbb", Replicas: 1}, "generalist")
	if left[0] == right[0] || !strings.Contains(left[0], "12345678-aaaa") || !strings.Contains(right[0], "12345678-bbbb") {
		t.Fatalf("durable Team identities collided: left=%v right=%v", left, right)
	}
}

func testCatalog(t *testing.T) *agenttemplate.Catalog {
	t.Helper()
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model",
		ValidateTools: func([]string) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("load test catalog: %v", err)
	}
	return catalog
}

func testPlan(t *testing.T, planID string) (*plan.Coordinator, string, string) {
	t.Helper()
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controllerID := "controller-" + planID
	created, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: planID, RootTaskID: controllerID,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return coordinator, created.ID, controllerID
}

func testManager(
	t *testing.T,
	catalog *agenttemplate.Catalog,
	coordinator *plan.Coordinator,
	durable TeamStore,
	routes RouteRegistry,
	maxInstances int,
) *Manager {
	t.Helper()
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(),
		PlanCoordinator: coordinator, ProjectRoot: t.TempDir(),
	}
	return NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, coordinator, durable, routes, maxInstances)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
