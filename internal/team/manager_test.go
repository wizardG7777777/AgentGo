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
	ownerScope   string
	count        int
	role         string
	capabilities []string
	claimantIDs  []string
}

type fakeRoutes struct {
	mu             sync.Mutex
	routes         map[string]routeRecord
	registers      int
	beforeRegister func(key, eventType string, count int) error
}

func newFakeRoutes() *fakeRoutes { return &fakeRoutes{routes: make(map[string]routeRecord)} }

func (r *fakeRoutes) RegisterRoute(key, eventType, ownerScope string, count int, role string, capabilities []string) error {
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
		eventType: eventType, ownerScope: ownerScope, count: count, role: role,
		capabilities: append([]string(nil), capabilities...),
	}
	r.registers++
	return nil
}

func (r *fakeRoutes) BindRouteClaimants(key string, agentIDs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.routes[key]
	if !ok {
		return errors.New("missing route")
	}
	record.claimantIDs = append([]string(nil), agentIDs...)
	r.routes[key] = record
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

func (s *failingQueryStore) QueryAvailable(string, string) ([]*model.Task, error) {
	s.once.Do(func() { close(s.entered) })
	return nil, errors.New("simulated query failure")
}

func (s *queryObservingStore) QueryAvailable(eventType, agentID string) ([]*model.Task, error) {
	s.mu.Lock()
	s.queries[eventType]++
	s.mu.Unlock()
	return s.TaskStore.QueryAvailable(eventType, agentID)
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
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-provision")
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManagerWithStore(t, catalog, taskStore, durable, routes, 2)
	req := agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "map the codebase", Replicas: 2,
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
	if route.ownerScope != model.TaskRouteScope(controllerID) || route.count != 2 ||
		!contains(route.capabilities, "read_file") || contains(route.capabilities, "code-investigation") ||
		len(route.claimantIDs) != 2 || route.claimantIDs[0] != first.AgentIDs[0] || route.claimantIDs[1] != first.AgentIDs[1] {
		t.Fatalf("route must expose concrete tools and controller ownership, got %+v", route)
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
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/generalist@1", Purpose: "implement", Replicas: 1,
	})
	if !errors.Is(err, ErrProcessLimitExceeded) {
		t.Fatalf("process limit err=%v, want ErrProcessLimitExceeded", err)
	}
	_, err = manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/verifier@1", Purpose: "accept", Replicas: 2,
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

	// 恢复与首次 provision 共用同一任务存储：controller 任务仍存活（非终态、
	// 未淘汰），ready TeamSpec 才能通过 Start 的 controller 检查被恢复。
	recoveredRoutes := newFakeRoutes()
	recovered := testManagerWithStore(t, catalog, taskStore, durable, recoveredRoutes, 2)
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
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-mailbox-recovery")
	durable := NewMemoryStore()

	// 先产生一个持久化 ready TeamSpec，以获得真实的稳定 agentID。
	first := testManagerWithStore(t, catalog, taskStore, durable, newFakeRoutes(), 1)
	t.Cleanup(first.Shutdown)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	result, err := first.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "recover unread mail", Replicas: 1,
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
		Store:  taskStore,
		Roster: roster.NewMemoryRoster(), MBRegistry: mailboxes,
		ProjectRoot: t.TempDir(),
	}
	recovered := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, newFakeRoutes(), 1)
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
	durable := NewMemoryStore()
	mailboxes := mailbox.NewRegistry(8)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-mailbox-shutdown-snapshot")
	deps := runner.RunnerDeps{
		Store:  taskStore,
		Roster: roster.NewMemoryRoster(), MBRegistry: mailboxes,
		ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, newFakeRoutes(), 1)
	t.Cleanup(manager.Shutdown)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "persist unread shutdown mail", Replicas: 1,
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
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-mailbox-start-rollback")
	durable := NewMemoryStore()
	first := testManagerWithStore(t, catalog, taskStore, durable, newFakeRoutes(), 1)
	t.Cleanup(first.Shutdown)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	result, err := first.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "rollback unread mail", Replicas: 1,
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
		TaskStore: taskStore,
		entered:   make(chan struct{}),
	}
	activity := agent.NewActivityTracker()
	deps := runner.RunnerDeps{
		Store: queryStore, Roster: roster.NewMemoryRoster(), Activity: activity,
		MBRegistry: mailboxes, ProjectRoot: t.TempDir(),
	}
	recovered := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, newFakeRoutes(), 1)
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
	durable := NewMemoryStore()
	mailboxes := mailbox.NewRegistry(8)
	activity := agent.NewActivityTracker()
	taskStore := &queryObservingStore{
		TaskStore: store.NewMemoryTaskStore(nil, 100, 1, 30),
		queries:   make(map[string]int),
	}
	controllerID := newControllerTask(t, taskStore, "controller-ready-order")
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
		ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, routes, 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()

	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "inspect", Replicas: 2,
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
	durable := NewMemoryStore()
	routeErr := errors.New("simulated route registration failure")
	routes := newFakeRoutes()
	routes.beforeRegister = func(string, string, int) error { return routeErr }
	mailboxes := mailbox.NewRegistry(8)
	activity := agent.NewActivityTracker()
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-route-failure")
	deps := runner.RunnerDeps{
		Store:  taskStore,
		Roster: roster.NewMemoryRoster(), Activity: activity, MBRegistry: mailboxes,
		ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, routes, 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
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
	recovered := testManager(t, catalog, durable, recoveredRoutes, 2)
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
	persistErr := errors.New("simulated persistence failure")
	storeBackend := &blockingEnsureStore{
		TeamStore: NewMemoryStore(), entered: make(chan struct{}),
		release: make(chan struct{}), err: persistErr,
	}
	routes := newFakeRoutes()
	mailboxes := mailbox.NewRegistry(8)
	activity := agent.NewActivityTracker()
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-persist-failure")
	deps := runner.RunnerDeps{
		Store:  taskStore,
		Roster: roster.NewMemoryRoster(), Activity: activity, MBRegistry: mailboxes,
		ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, storeBackend, routes, 2)
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
			ControllerTaskID: controllerID,
			TemplateRef:      "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
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

func TestManagerRecoveryStopsStaleDigestTeam(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 4)
	controllerID := newControllerTask(t, manager.deps.Store, "controller-digest")
	spec := testSpec("digest-team", controllerID, "investigate")
	spec.TemplateDigest = "sha256:stale"
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatalf("persist stale spec: %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start must tolerate stale digest, got %v", err)
	}
	defer manager.Shutdown()
	got, err := durable.Get(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStopped || !strings.HasPrefix(got.StopReason, "template_digest_changed:") {
		t.Fatalf("stale team = status %s reason %q, want stopped/template_digest_changed:", got.Status, got.StopReason)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("stale digest started %d agents", manager.ActiveCount())
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("stale digest installed routes: %+v", current)
	}
}

func TestManagerRecoveryStopsTeamWithUnavailableTemplate(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 4)
	controllerID := newControllerTask(t, manager.deps.Store, "controller-missing-template")
	spec := testSpec("missing-template-team", controllerID, "investigate")
	spec.TemplateRef = "builtin/deleted@99"
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatalf("persist spec with unavailable template: %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start must tolerate unavailable template, got %v", err)
	}
	defer manager.Shutdown()
	got, err := durable.Get(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStopped || !strings.HasPrefix(got.StopReason, "template_unavailable:") {
		t.Fatalf("team = status %s reason %q, want stopped/template_unavailable:", got.Status, got.StopReason)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("unavailable template started %d agents", manager.ActiveCount())
	}
}

// controller 任务已从任务存储淘汰（GetTask 失败）的 ready Team 在恢复时按
// controller 维度标记 stopped，不再恢复运行时。
func TestManagerRecoveryStopsTeamWithMissingController(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 4)
	spec := testSpec("missing-controller-team", "controller-never-published", "investigate")
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatalf("persist spec with missing controller: %v", err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start must tolerate missing controller, got %v", err)
	}
	defer manager.Shutdown()
	got, err := durable.Get(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStopped || got.StopReason != "controller_missing" {
		t.Fatalf("team = status %s reason %q, want stopped/controller_missing", got.Status, got.StopReason)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("missing controller started %d agents", manager.ActiveCount())
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("missing controller installed routes: %+v", current)
	}
}

// controller 任务已终态的 ready Team 在恢复时同样被标记 stopped 并跳过——
// 恢复只接 controller 仍存活的 Team。digest 用真实值以隔离 controller 检查。
func TestManagerRecoveryStopsTeamWithTerminalController(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 4)
	controllerID := newControllerTask(t, manager.deps.Store, "controller-terminal-recovery")
	spec := testSpec("terminal-controller-team", controllerID, "investigate")
	tmpl, err := catalog.Resolve("builtin/explorer@1")
	if err != nil {
		t.Fatalf("resolve explorer template: %v", err)
	}
	spec.TemplateDigest = tmpl.Digest
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatalf("persist spec with terminal controller: %v", err)
	}
	terminalControllerTask(t, manager.deps.Store, controllerID, model.TaskStatusFailed)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start must tolerate terminal controller, got %v", err)
	}
	defer manager.Shutdown()
	got, err := durable.Get(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStopped || got.StopReason != "controller_terminal:failed" {
		t.Fatalf("team = status %s reason %q, want stopped/controller_terminal:failed", got.Status, got.StopReason)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("terminal controller started %d agents", manager.ActiveCount())
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("terminal controller installed routes: %+v", current)
	}
}

func TestManagerProvisionRejectsDurableDigestDrift(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	manager := testManager(t, catalog, durable, newFakeRoutes(), 4)
	controllerID := newControllerTask(t, manager.deps.Store, "controller-live-digest")
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	spec := testSpec("live-digest-team", controllerID, "investigate")
	spec.TemplateDigest = "sha256:stale"
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "investigate", Replicas: 2,
	})
	if !errors.Is(err, ErrTemplateDigestMismatch) {
		t.Fatalf("Provision err=%v, want ErrTemplateDigestMismatch", err)
	}
}

// controller 任务终态事件触发其名下 active Team 的拆除并持久化 stopped；
// stopped Team 永不被后续进程恢复。
func TestManagerTerminalControllerReactorStopsTeamAndRecoverySkipsIt(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 2)
	controllerID := newControllerTask(t, manager.deps.Store, "controller-terminal")
	subscribesTerminal := false
	for _, kind := range manager.Subscribe() {
		if kind == trace.KindTaskCompleted {
			subscribesTerminal = true
			break
		}
	}
	if !subscribesTerminal {
		t.Fatal("TeamManager does not subscribe to task terminal events")
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	terminalControllerTask(t, manager.deps.Store, controllerID, model.TaskStatusCompleted)
	if err := manager.Run(trace.Event{
		Kind:   trace.KindTaskCompleted,
		TaskID: controllerID,
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

	// controller 已终态的 Team 持久态为 stopped，即使此后模板 digest 变化
	// 也绝不会被恢复。
	recoveredRoutes := newFakeRoutes()
	recovered := testManager(t, catalog, durable, recoveredRoutes, 2)
	if err := recovered.Start(context.Background()); err != nil {
		t.Fatalf("terminal recovery: %v", err)
	}
	if recovered.ActiveCount() != 0 {
		t.Fatalf("terminal recovery started %d agents", recovered.ActiveCount())
	}
	recovered.Shutdown()
}

func TestManagerGraphTeamSurvivesControllerAndStopsAtGraphTerminal(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 2)
	graphStatus := "running"
	graphExists := true
	if err := manager.SetGraphStateResolver(func(graphID string) (string, bool, bool) {
		if graphID != "g-team-lifecycle" || !graphExists {
			return "", false, false
		}
		return graphStatus, graphStatus == "completed" || graphStatus == "failed" || graphStatus == "cancelled", true
	}); err != nil {
		t.Fatal(err)
	}
	controllerID := newControllerTask(t, manager.deps.Store, "graph owner")
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID, GraphID: "g-team-lifecycle",
		TemplateRef: "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
	})
	if err != nil {
		t.Fatalf("Provision graph Team: %v", err)
	}
	registered, _ := routes.snapshot()
	if got := registered[result.EventType].ownerScope; got != model.GraphRouteScope("g-team-lifecycle") {
		t.Fatalf("route owner=%q, want Graph scope", got)
	}

	terminalControllerTask(t, manager.deps.Store, controllerID, model.TaskStatusCompleted)
	if err := manager.Run(trace.Event{Kind: trace.KindTaskCompleted, TaskID: controllerID}); err != nil {
		t.Fatal(err)
	}
	if manager.ActiveCount() != 1 {
		t.Fatalf("controller terminal tore down Graph Team; active=%d", manager.ActiveCount())
	}
	if got, _ := durable.Get(result.TeamID); got.Status != StatusReady {
		t.Fatalf("Graph Team stopped with origin controller: %+v", got)
	}

	graphStatus = "completed"
	if err := manager.Run(trace.Event{Kind: trace.KindGraphEnded, GraphID: "g-team-lifecycle"}); err != nil {
		t.Fatal(err)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("graph terminal left active Team: %d", manager.ActiveCount())
	}
	if got, _ := durable.Get(result.TeamID); got.Status != StatusStopped || got.StopReason != "graph_terminal:completed" {
		t.Fatalf("Graph Team durable terminal mismatch: %+v", got)
	}
	if current, _ := routes.snapshot(); len(current) != 0 {
		t.Fatalf("graph terminal left route: %+v", current)
	}
}

func TestManagerStopsGraphBindingOrphanWhenSubmissionNeverBecameDurable(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 1)
	if err := manager.SetGraphStateResolver(func(string) (string, bool, bool) {
		return "", false, false // provision happened, but submit_graph was rejected
	}); err != nil {
		t.Fatal(err)
	}
	controllerID := newControllerTask(t, manager.deps.Store, "orphan binding")
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID, GraphID: "g-never-submitted",
		TemplateRef: "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalControllerTask(t, manager.deps.Store, controllerID, model.TaskStatusFailed)
	if err := manager.Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: controllerID}); err != nil {
		t.Fatal(err)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("orphan Graph binding left active agents: %d", manager.ActiveCount())
	}
	if got, _ := durable.Get(result.TeamID); got.Status != StatusStopped || got.StopReason != "graph_binding_orphan" {
		t.Fatalf("orphan Graph Team durable state=%+v", got)
	}
	if registered, _ := routes.snapshot(); len(registered) != 0 {
		t.Fatalf("orphan Graph binding left route: %+v", registered)
	}
}

func TestManagerLifecycleReactorUsesReliableAsyncDispatch(t *testing.T) {
	manager := &Manager{}
	if manager.IsSync() || !manager.ReliableAsync() {
		t.Fatal("Team lifecycle reactor must use the non-dropping reliable async lane")
	}
}

func TestManagerRejectsProvisionForTerminalGraph(t *testing.T) {
	catalog := testCatalog(t)
	durable := NewMemoryStore()
	routes := newFakeRoutes()
	manager := testManager(t, catalog, durable, routes, 1)
	if err := manager.SetGraphStateResolver(func(graphID string) (string, bool, bool) {
		if graphID == "g-already-ended" {
			return "completed", true, true
		}
		return "", false, false
	}); err != nil {
		t.Fatal(err)
	}
	controllerID := newControllerTask(t, manager.deps.Store, "terminal graph provision")
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown()

	_, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID, GraphID: "g-already-ended",
		TemplateRef: "builtin/explorer@1", Purpose: "must not leak", Replicas: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "terminal graph") {
		t.Fatalf("terminal Graph provision err=%v, want fail-closed rejection", err)
	}
	if manager.ActiveCount() != 0 {
		t.Fatalf("terminal Graph provision started %d agents", manager.ActiveCount())
	}
	if specs, listErr := durable.List(); listErr != nil || len(specs) != 0 {
		t.Fatalf("terminal Graph provision persisted specs=%+v err=%v", specs, listErr)
	}
	if registered, _ := routes.snapshot(); len(registered) != 0 {
		t.Fatalf("terminal Graph provision registered routes: %+v", registered)
	}
}

func TestManagerRecoveryUsesGraphStatusInsteadOfControllerTerminal(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "old graph controller")
	terminalControllerTask(t, taskStore, controllerID, model.TaskStatusCompleted)
	durable := NewMemoryStore()
	spec := testSpec("graph-recovery", controllerID, "recover")
	spec.GraphID = "g-recovery"
	tmpl, err := catalog.Resolve(spec.TemplateRef)
	if err != nil {
		t.Fatal(err)
	}
	spec.TemplateDigest = tmpl.Digest
	if _, _, err := durable.Ensure(spec); err != nil {
		t.Fatal(err)
	}

	manager := testManagerWithStore(t, catalog, taskStore, durable, newFakeRoutes(), 2)
	if err := manager.SetGraphStateResolver(func(string) (string, bool, bool) {
		return "running", false, true
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("recover nonterminal Graph Team: %v", err)
	}
	if manager.ActiveCount() != spec.Replicas {
		t.Fatalf("recovered active=%d, want %d", manager.ActiveCount(), spec.Replicas)
	}
	manager.Shutdown()

	terminal := testManagerWithStore(t, catalog, taskStore, durable, newFakeRoutes(), 2)
	if err := terminal.SetGraphStateResolver(func(string) (string, bool, bool) {
		return "failed", true, true
	}); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Start(context.Background()); err != nil {
		t.Fatalf("terminal recovery: %v", err)
	}
	defer terminal.Shutdown()
	if terminal.ActiveCount() != 0 {
		t.Fatalf("terminal Graph recovered agents: %d", terminal.ActiveCount())
	}
	if got, _ := durable.Get(spec.ID); got.Status != StatusStopped || got.StopReason != "graph_terminal:failed" {
		t.Fatalf("terminal Graph recovery mismatch: %+v", got)
	}
}

func TestManagerShutdownRemovesDynamicRuntimeSurfaces(t *testing.T) {
	catalog := testCatalog(t)
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	controllerID := newControllerTask(t, taskStore, "controller-cleanup")
	activity := agent.NewActivityTracker()
	mailboxes := mailbox.NewRegistry(8)
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), Activity: activity,
		MBRegistry: mailboxes, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, NewMemoryStore(), newFakeRoutes(), 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	result, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
		ControllerTaskID: controllerID,
		TemplateRef:      "builtin/explorer@1", Purpose: "inspect", Replicas: 1,
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
	callbacks := reactorbuiltin.NewTaskEndCallbackReactor()
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 30)
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(),
		TaskEndCallbacks: callbacks, ProjectRoot: t.TempDir(),
	}
	manager := NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, NewMemoryStore(), newFakeRoutes(), 2)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Shutdown()

	for round := 0; round < 5; round++ {
		controllerID := newControllerTask(t, taskStore, fmt.Sprintf("controller-callback-%d", round))
		if _, err := manager.Provision(context.Background(), agenttemplate.ProvisionRequest{
			ControllerTaskID: controllerID,
			TemplateRef:      "builtin/explorer@1", Purpose: fmt.Sprintf("round-%d", round), Replicas: 2,
		}); err != nil {
			t.Fatalf("round %d Provision: %v", round, err)
		}
		if got := callbacks.CallbackCount(); got != 2 {
			t.Fatalf("round %d live callback count=%d, want 2", round, got)
		}
		terminalControllerTask(t, taskStore, controllerID, model.TaskStatusCompleted)
		if err := manager.Run(trace.Event{
			Kind:   trace.KindTaskCompleted,
			TaskID: controllerID,
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

// newControllerTask 在任务存储中发布一个 __scheduler__ controller 任务并返回
// 其 ID。Team 的归属（owner）即该任务：恢复与终态拆解路径都凭它判定生命周期。
func newControllerTask(t *testing.T, taskStore store.TaskStore, description string) string {
	t.Helper()
	task := &model.Task{Description: description, EventType: "__scheduler__", Priority: 1}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("publish controller task: %v", err)
	}
	return task.ID
}

// terminalControllerTask 把 controller 任务沿合法迁移推进到指定终态
// （completed 需经 processing；cancelled/failed 可从 pending 直达）。
func terminalControllerTask(t *testing.T, taskStore store.TaskStore, taskID string, to model.TaskStatus) {
	t.Helper()
	task, err := taskStore.GetTask(taskID)
	if err != nil {
		t.Fatalf("get controller task %s: %v", taskID, err)
	}
	from := task.Status
	if to == model.TaskStatusCompleted && from == model.TaskStatusPending {
		if err := taskStore.TransitionState(taskID, from, model.TaskStatusProcessing); err != nil {
			t.Fatalf("start controller task %s: %v", taskID, err)
		}
		from = model.TaskStatusProcessing
	}
	if err := taskStore.TransitionState(taskID, from, to); err != nil {
		t.Fatalf("terminate controller task %s to %s: %v", taskID, to, err)
	}
}

// testManager 构造使用独立任务存储的 Manager。涉及跨 Manager 恢复同一批
// TeamSpec 的用例应改用 testManagerWithStore 共享存储——恢复路径会检查
// controller 任务是否仍存活。
func testManager(
	t *testing.T,
	catalog *agenttemplate.Catalog,
	durable TeamStore,
	routes RouteRegistry,
	maxInstances int,
) *Manager {
	t.Helper()
	return testManagerWithStore(t, catalog, store.NewMemoryTaskStore(nil, 100, 1, 30), durable, routes, maxInstances)
}

func testManagerWithStore(
	t *testing.T,
	catalog *agenttemplate.Catalog,
	taskStore store.TaskStore,
	durable TeamStore,
	routes RouteRegistry,
	maxInstances int,
) *Manager {
	t.Helper()
	deps := runner.RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), ProjectRoot: t.TempDir(),
	}
	return NewManager(deps, func(string) llm.Client { return idleLLM{} },
		catalog, durable, routes, maxInstances)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
