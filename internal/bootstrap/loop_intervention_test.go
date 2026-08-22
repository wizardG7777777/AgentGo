package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/intervention"
	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
	"agentgo/internal/model"
	"agentgo/internal/outcome"
	"agentgo/internal/outcomestore"
	"agentgo/internal/policycatalog"
	"agentgo/internal/reactor"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

type loopInterventionStoreFake struct {
	commands map[string][]loopcontract.LoopInterventionRequested
	acks     []loopstore.InterventionAck
	ackErr   error
}

func (s *loopInterventionStoreFake) PendingInterventionsForTask(taskID string) ([]loopcontract.LoopInterventionRequested, error) {
	return append([]loopcontract.LoopInterventionRequested(nil), s.commands[taskID]...), nil
}

func (s *loopInterventionStoreFake) AckIntervention(taskID string, ack loopstore.InterventionAck) error {
	if s.ackErr != nil {
		return s.ackErr
	}
	commands := s.commands[taskID]
	for i, command := range commands {
		if command.CommandID == ack.CommandID {
			s.commands[taskID] = append(commands[:i], commands[i+1:]...)
			s.acks = append(s.acks, ack)
			return nil
		}
	}
	return errors.New("pending command 不存在")
}

type outcomeDeliveryFake struct {
	records map[string]outcomestore.Record
	pending map[string]bool
}

func (f *outcomeDeliveryFake) GetByRef(ref string) (outcomestore.Record, bool, error) {
	record, ok := f.records[ref]
	return record, ok, nil
}

func (f *outcomeDeliveryFake) PendingDeliveries() ([]outcomestore.Record, error) {
	var records []outcomestore.Record
	for ref, pending := range f.pending {
		if pending {
			records = append(records, f.records[ref])
		}
	}
	return records, nil
}

func TestLoopInterventionBridgeOrdersFeedWakeAndAck(t *testing.T) {
	tasks := newLoopInterventionTaskStore(t)
	source, command := publishTerminalInterventionSource(t, tasks, "source-1", false)
	loops := &loopInterventionStoreFake{commands: map[string][]loopcontract.LoopInterventionRequested{
		source.ID: {command},
	}}
	outcomes := newOutcomeDeliveryFake(source, true)
	bridge, err := newLoopInterventionBridge(tasks, loops, outcomes)
	if err != nil {
		t.Fatal(err)
	}

	// Graph terminal feed 尚未 Ack TaskOutcome 时拒绝物化 Scheduler wake。
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: source.ID}); err == nil {
		t.Fatal("TaskOutcome delivery pending 时 intervention 不得抢跑")
	}
	wakeID := intervention.WakeTaskID(command.CommandID)
	if _, err := tasks.GetTask(wakeID); !errors.Is(err, store.ErrTaskNotFound) {
		t.Fatalf("抢跑产生了 wake: %v", err)
	}
	if len(loops.acks) != 0 {
		t.Fatal("Ensure 前不得 Ack")
	}

	// terminal feed/Graph settlement 完成并 Ack Outcome 后才 ensure wake。
	outcomes.pending[source.OutcomeRef] = false
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: source.ID}); err != nil {
		t.Fatal(err)
	}
	wake, err := tasks.GetTask(wakeID)
	if err != nil {
		t.Fatalf("未发布 deterministic coordination wake: %v", err)
	}
	if wake.EventType != "__scheduler__" || wake.EventSource != loopInterventionWakeSource ||
		wake.ParentTaskID != source.ID || wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Fatalf("wake 不是 detached Scheduler coordination Task: %+v", wake)
	}
	if wake.RunID != source.RunID || wake.RunContract == source.RunContract ||
		wake.RunPhase != runcontract.PhaseRecovery || wake.ProgressContract == nil ||
		wake.ProgressContract.WorkClass != loopcontract.WorkCoordination {
		t.Fatalf("wake 未正确继承 Run/重编译 coordination contract: %+v", wake)
	}
	if len(loops.acks) != 0 {
		t.Fatal("PublishTask 成功不得立即 Ack intervention")
	}

	// 重复 source terminal event 只能命中同一个显式 ID，仍不 Ack。
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: source.ID}); err != nil {
		t.Fatal(err)
	}
	if len(loops.acks) != 0 {
		t.Fatal("重复 ensure 不得提前 Ack")
	}

	// Scheduler wake 自身形成 durable TaskOutcome 且 delivery 已完成后才 Ack。
	if err := tasks.ClaimTask("scheduler", wake.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.BlockProcessingTaskBySystem(wake.ID, "已完成协调裁决", "coordination_decided"); err != nil {
		t.Fatal(err)
	}
	wake, _ = tasks.GetTask(wake.ID)
	outcomes.add(wake, false)
	nested := command
	nested.CommandID = "command-nested-wake"
	nested.TaskID = wake.ID
	nested.AttemptID = wake.AttemptID
	nested.RunID = wake.RunID
	nested.GraphID, nested.NodeID, nested.ActivationID = "", "", ""
	nested.Contract = wake.ProgressContract.Ref
	loops.commands[wake.ID] = []loopcontract.LoopInterventionRequested{nested}
	if err := bridge.RunWithContext(context.Background(), trace.Event{Kind: trace.KindTaskBlocked, TaskID: wake.ID}); err != nil {
		t.Fatal(err)
	}
	if len(loops.acks) != 2 || loops.acks[0].CommandID != command.CommandID ||
		loops.acks[0].DecisionRef != wake.OutcomeRef || loops.acks[1].CommandID != nested.CommandID ||
		loops.acks[1].DecisionRef != wake.OutcomeRef {
		t.Fatalf("wake durable outcome 后未正确 Ack: %+v", loops.acks)
	}
	if _, err := tasks.GetTask(intervention.WakeTaskID(nested.CommandID)); !errors.Is(err, store.ErrTaskNotFound) {
		t.Fatalf("intervention wake 不得递归创建下一层 wake: %v", err)
	}
}

func TestLoopInterventionBridgeIsTaskScopedAndGraphDetached(t *testing.T) {
	tasks := newLoopInterventionTaskStore(t)
	first, command1 := publishTerminalInterventionSource(t, tasks, "source-graph", true)
	second, command2 := publishTerminalInterventionSource(t, tasks, "source-other", false)
	loops := &loopInterventionStoreFake{commands: map[string][]loopcontract.LoopInterventionRequested{
		first.ID: {command1}, second.ID: {command2},
	}}
	outcomes := newOutcomeDeliveryFake(first, false)
	outcomes.add(second, false)
	bridge, _ := newLoopInterventionBridge(tasks, loops, outcomes)

	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: first.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.GetTask(intervention.WakeTaskID(command1.CommandID)); err != nil {
		t.Fatalf("当前 Task 的 graph intervention 未物化: %v", err)
	}
	if _, err := tasks.GetTask(intervention.WakeTaskID(command2.CommandID)); !errors.Is(err, store.ErrTaskNotFound) {
		t.Fatalf("定向 ensure 抢跑其它 Task: %v", err)
	}
	wake, _ := tasks.GetTask(intervention.WakeTaskID(command1.CommandID))
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" ||
		wake.InterventionGraphID != command1.GraphID || wake.InterventionNodeID != command1.NodeID ||
		wake.InterventionActivationID != command1.ActivationID ||
		!strings.Contains(wake.Description, "Graph graph-1") || !strings.Contains(wake.Description, "GraphChangeProposal") {
		t.Fatalf("Graph command 被错误绑定或缺少 Scheduler 裁决指引: %+v", wake)
	}
}

func TestLoopInterventionBridgeIgnoresLateHistoricalEvent(t *testing.T) {
	tasks := newLoopInterventionTaskStore(t)
	loops := &loopInterventionStoreFake{commands: map[string][]loopcontract.LoopInterventionRequested{}}
	outcomes := &outcomeDeliveryFake{records: map[string]outcomestore.Record{}, pending: map[string]bool{}}
	bridge, _ := newLoopInterventionBridge(tasks, loops, outcomes)
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: "old-session-task"}); err != nil {
		t.Fatal(err)
	}
	if len(loops.acks) != 0 {
		t.Fatal("迟到的历史 Session event 不得产生消费")
	}
}

func TestLoopInterventionBridgeRejectsDeterministicWakeCollision(t *testing.T) {
	tasks := newLoopInterventionTaskStore(t)
	source, command := publishTerminalInterventionSource(t, tasks, "source-collision", false)
	occupied := &model.Task{
		ID: intervention.WakeTaskID(command.CommandID), Description: "unrelated occupant",
		EventType: "__scheduler__", EventSource: loopInterventionWakeSource,
	}
	if err := tasks.PublishTask(occupied); err != nil {
		t.Fatal(err)
	}
	loops := &loopInterventionStoreFake{commands: map[string][]loopcontract.LoopInterventionRequested{
		source.ID: {command},
	}}
	bridge, _ := newLoopInterventionBridge(tasks, loops, newOutcomeDeliveryFake(source, false))
	err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: source.ID})
	if err == nil || !strings.Contains(err.Error(), "不一致事实占用") {
		t.Fatalf("deterministic ID 冲突必须 fail-closed: %v", err)
	}
	if len(loops.acks) != 0 {
		t.Fatal("冲突不得 Ack intervention")
	}
}

func TestLoopInterventionBridgeAckFailureLeavesPending(t *testing.T) {
	tasks := newLoopInterventionTaskStore(t)
	source, command := publishTerminalInterventionSource(t, tasks, "source-ack-fail", false)
	loops := &loopInterventionStoreFake{commands: map[string][]loopcontract.LoopInterventionRequested{
		source.ID: {command},
	}, ackErr: errors.New("fsync failed")}
	outcomes := newOutcomeDeliveryFake(source, false)
	bridge, _ := newLoopInterventionBridge(tasks, loops, outcomes)
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: source.ID}); err != nil {
		t.Fatal(err)
	}
	wake, _ := tasks.GetTask(intervention.WakeTaskID(command.CommandID))
	if err := tasks.ClaimTask("scheduler", wake.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.BlockProcessingTaskBySystem(wake.ID, "协调失败", "coordination_failed"); err != nil {
		t.Fatal(err)
	}
	wake, _ = tasks.GetTask(wake.ID)
	outcomes.add(wake, false)
	if err := bridge.Run(trace.Event{Kind: trace.KindTaskBlocked, TaskID: wake.ID}); err == nil {
		t.Fatal("Ack fsync 失败必须可见")
	}
	if len(loops.commands[source.ID]) != 1 || len(loops.acks) != 0 {
		t.Fatal("Ack 失败不得移除 pending intervention")
	}
}

func TestWireGraphRuntimeRegistersInterventionAfterTerminalFeed(t *testing.T) {
	root := t.TempDir()
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 64), 100, 1, 0)
	loops, err := loopstore.Open(filepath.Join(root, "loop"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loops.Close() })
	outcomes, err := outcomestore.New(filepath.Join(root, "outcomes"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outcomes.Close() })
	policies, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	registry := reactor.NewRegistry()
	t.Cleanup(func() { registry.Quiesce(0) })
	graphs, _, err := wireGraphRuntimeWithOutcome(
		&config.Config{ProjectRoot: root}, tasks, registry, nil, policies, func() string { return "" },
		outcomes, loops,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = graphs.Close() })

	subscribers := registry.Subscribers(trace.KindTaskBlocked)
	feedIndex, bridgeIndex := -1, -1
	for i, subscriber := range subscribers {
		switch subscriber.Name() {
		case "graph-terminal-feed":
			feedIndex = i
		case "loop-intervention-bridge":
			bridgeIndex = i
		}
	}
	if feedIndex < 0 || bridgeIndex < 0 || feedIndex >= bridgeIndex {
		t.Fatalf("production Reactor 顺序错误: feed=%d bridge=%d subscribers=%v", feedIndex, bridgeIndex, subscribers)
	}
}

func newLoopInterventionTaskStore(t *testing.T) *store.MemoryTaskStore {
	t.Helper()
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 64), 100, 1, 0)
	if err := store.SetTerminalOutcomeHook(tasks, func(intent store.TerminalOutcomeIntent) (string, error) {
		return "outcome:" + intent.Task.ID, nil
	}); err != nil {
		t.Fatal(err)
	}
	return tasks
}

func publishTerminalInterventionSource(t *testing.T, tasks *store.MemoryTaskStore,
	id string, graphTask bool) (*model.Task, loopcontract.LoopInterventionRequested) {
	t.Helper()
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ProgressContract(policycatalog.ProgressInvestigationV1)
	if !ok {
		t.Fatal("缺少 investigation ProgressContract")
	}
	now := time.Now().UTC()
	run := &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: runcontract.RunID("run-" + id), CreatedAt: now,
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
		RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
	}
	source := &model.Task{
		ID: id, Description: "source", RunID: run.RunID, RunContract: run,
		ContextPolicyRef: policycatalog.ContextDefaultV1, ProgressContract: &profile.Contract,
	}
	if graphTask {
		source.GraphID, source.NodeID, source.ActivationID, source.GraphNodeKind = "graph-1", "work", "work@1", "agent"
	}
	if err := tasks.PublishTask(source); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker", source.ID); err != nil {
		t.Fatal(err)
	}
	claimed, _ := tasks.GetTask(source.ID)
	command := loopcontract.LoopInterventionRequested{
		Schema:    loopcontract.InterventionSchemaV1,
		CommandID: "command-" + id, RunID: claimed.RunID,
		GraphID: claimed.GraphID, NodeID: claimed.NodeID, ActivationID: claimed.ActivationID,
		TaskID: claimed.ID, AttemptID: claimed.AttemptID, Contract: claimed.ProgressContract.Ref,
		ReasonCode:        loopcontract.InterventionNoProgressStalled,
		MissingMilestones: []string{"deliverable"}, BudgetUsed: runcontract.BudgetUsage{ModelCalls: 4},
		BudgetRemaining: runcontract.BudgetLimit{ModelCalls: 2}, CheckpointRef: "checkpoint-1", RequestedAt: now,
	}
	if err := command.Validate(); err != nil {
		t.Fatalf("测试 command 无效: %v", err)
	}
	if err := tasks.BlockProcessingTaskBySystem(source.ID, "停滞需介入", "loop_intervention_required"); err != nil {
		t.Fatal(err)
	}
	terminal, _ := tasks.GetTask(source.ID)
	return terminal, command
}

func newOutcomeDeliveryFake(task *model.Task, pending bool) *outcomeDeliveryFake {
	fake := &outcomeDeliveryFake{records: make(map[string]outcomestore.Record), pending: make(map[string]bool)}
	fake.add(task, pending)
	return fake
}

func (f *outcomeDeliveryFake) add(task *model.Task, pending bool) {
	f.records[task.OutcomeRef] = outcomestore.Record{
		OutcomeRef: task.OutcomeRef,
		Outcome:    outcome.TaskOutcome{TaskID: task.ID, RunID: task.RunID},
	}
	f.pending[task.OutcomeRef] = pending
}
