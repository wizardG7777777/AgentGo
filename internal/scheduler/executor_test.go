package scheduler

import (
	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/probe"
	"agentgo/internal/store"
	"agentgo/internal/testagent"
	"agentgo/internal/testmodel"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerExecutorBlocksLaterToolWhenControllerCancelledInSameResponse(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	root := &model.Task{Description: "scheduler controller", EventType: "__scheduler__", GraphID: "g-execution"}
	if err := taskStore.PublishTask(root); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler-1", root.ID); err != nil {
		t.Fatal(err)
	}
	var secondSideEffect int32
	toolReg := agent.NewToolRegistry()
	toolReg.Register("cancel_controller", "cancel current controller", nil, func(context.Context, map[string]any) (string, error) {
		err := store.TransitionStateWithCancelSource(taskStore, root.ID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler")
		return "cancelled", err
	})
	toolReg.Register("second_side_effect", "must be blocked", nil, func(context.Context, map[string]any) (string, error) {
		atomic.AddInt32(&secondSideEffect, 1)
		return "ran", nil
	})
	client := &scriptedLLM{responses: []testmodel.Fixture{{ToolCalls: []llm.ToolCall{{ID: "cancel-first", Name: "cancel_controller", Arguments: map[string]any{}}, {ID: "side-effect-second", Name: "second_side_effect", Arguments: map[string]any{}}}, FinishReason: llm.FinishReasonToolCalls}}}
	exec := &SchedulerExecutor{Inner: testagent.Wrap(agent.NewTurnExecutor(client, toolReg, nil, nil, testmodel.Runtime(t), contextruntime.Instructions{ProfileID: "test"}).Execute), Store: taskStore, Cfg: config.DefaultConfig()}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := exec.requireToolDispatch(cancelledCtx, root); err == nil || !strings.Contains(err.Error(), "context is no longer active") {
		t.Fatalf("cancelled dispatch context err=%v, want liveness rejection", err)
	}
	result, err := exec.Execute(context.Background(), root, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&secondSideEffect) != 0 {
		t.Fatal("second Scheduler side effect ran after the controller Task was cancelled")
	}
	if len(result.ToolResults) != 2 || !strings.Contains(result.ToolResults[1].Content, "not processing") {
		t.Fatalf("second tool did not receive durable controller lease rejection: %+v", result.ToolResults)
	}
}
func TestSchedulerExecutor_NoBatch_DirectExecute(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: make(chan struct{}), WaitTimeout: 100 * time.Millisecond}
	result, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result.Output != "ok" {
		t.Errorf("Output %q, want ok", result.Output)
	}
	if calls != 1 {
		t.Errorf("Inner called %d times, want 1", calls)
	}
}
func TestSchedulerExecutor_InjectsBoardSnapshotIntoHistory(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 2}}}
	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: make(chan struct{}), WaitTimeout: 100 * time.Millisecond}
	_, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	if mail == "" {
		t.Fatal("IncomingMail should be non-empty")
	}
	if capturedHistory[0].IncomingContextKind != contextcontract.FragmentRuntimeSnapshot || capturedHistory[0].IncomingContextSection != contextcontract.SectionRuntimeControl || capturedHistory[0].IncomingContextAuthority != contextcontract.AuthorityInformational {
		t.Fatalf("Scheduler board 未携带 typed L2 binding: %+v", capturedHistory[0])
	}
	if !strings.Contains(mail, `"worker_count": 2`) {
		t.Errorf("snapshot should contain worker_count, got: %s", mail)
	}
	if !strings.Contains(mail, `"exec_mode": "normal"`) {
		t.Errorf("snapshot should contain exec_mode=normal, got: %s", mail)
	}
}
func TestSchedulerExecutor_GraphScopeDrivesSnapshotAndDynamicRoutes(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 32), 64, 2, 60)
	controller := &model.Task{ID: "graph-controller", Description: "summarize", EventType: "__scheduler__", GraphID: "g-current", NodeID: "summarize", ActivationID: "summarize@1"}
	if err := s.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler", controller.ID); err != nil {
		t.Fatal(err)
	}
	sameGraph := &model.Task{ID: "same-graph-task", GraphID: controller.GraphID}
	completeSnapshotResultTask(t, s, sameGraph, map[string]string{"worker": "graph result"})
	registry := NewAgentRegistry()
	for _, route := range []struct {
		key, eventType, owner string
		capabilities          []string
	}{{key: "graph-default", owner: model.GraphRouteScope(controller.GraphID), capabilities: []string{"graph_default_tool"}}, {key: "legacy-default", owner: model.TaskRouteScope(controller.ID), capabilities: []string{"legacy_default_tool"}}, {key: "graph-team", eventType: "team:graph", owner: model.GraphRouteScope(controller.GraphID), capabilities: []string{"read_file"}}, {key: "legacy-team", eventType: "team:legacy", owner: model.TaskRouteScope(controller.ID), capabilities: []string{"read_file"}}, {key: "other-team", eventType: "team:other", owner: model.GraphRouteScope("g-other"), capabilities: []string{"read_file"}}} {
		if err := registry.RegisterRoute(route.key, route.eventType, route.owner, 1, route.key, route.capabilities); err != nil {
			t.Fatal(err)
		}
	}
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: &config.Config{}, AgentRegistry: registry}
	if _, err := exec.Execute(context.Background(), controller, nil, nil, llm.DefaultOutputBudget()); err != nil {
		t.Fatal(err)
	}
	if len(capturedHistory) != 1 {
		t.Fatalf("captured history len=%d, want 1", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	for _, want := range []string{sameGraph.ID, "graph result", "graph_default_tool", "team:graph"} {
		if !strings.Contains(mail, want) {
			t.Fatalf("Graph-scoped Scheduler snapshot omitted %q: %s", want, mail)
		}
	}
	for _, leaked := range []string{"legacy_default_tool", "team:legacy", "team:other"} {
		if strings.Contains(mail, leaked) {
			t.Fatalf("Graph-scoped Scheduler snapshot leaked route %q: %s", leaked, mail)
		}
	}
}
func TestSchedulerExecutor_ModesStoreLiveSwitch(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	schedTask := &model.Task{Description: "sched", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	modeStore := modes.DefaultStore()
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: make(chan struct{}), WaitTimeout: 100 * time.Millisecond, Modes: modeStore}
	modeStore.SetExec(modes.ExecStrict)
	modeStore.SetTopo(modes.TopoSolo)
	if _, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget()); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	for _, want := range []string{`"exec_mode": "strict"`, `"topo_mode": "solo"`} {
		if !strings.Contains(mail, want) {
			t.Errorf("快照缺少 %s，got: %s", want, mail)
		}
	}
	if strings.Contains(mail, `"mode":`) {
		t.Errorf("快照不应再含旧 \"mode\" 字段，got: %s", mail)
	}
}
func TestSchedulerExecutor_BatchUpdateChannelWakesWait(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)
	batchCh := make(chan struct{}, 1)
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: batchCh, WaitTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget())
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	s.SubmitResult("worker-1", child.ID, "done")
	batchCh <- struct{}{}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Execute did not unblock via channel signal")
	}
}
func TestSchedulerExecutor_TimeoutFallback(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	child := &model.Task{Description: "child"}
	s.PublishTask(child)
	s.ClaimTask("worker-1", child.ID)
	s.AppendSchedulerBatch(schedTask.ID, child.ID)
	batchCh := make(chan struct{})
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: batchCh, WaitTimeout: 100 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget())
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	s.SubmitResult("worker-1", child.ID, "done")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Execute did not unblock via timeout fallback")
	}
}
func TestSchedulerExecutor_BatchAllTerminalSkipsWait(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	schedTask := &model.Task{Description: "sched"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	c1 := &model.Task{Description: "c1"}
	s.PublishTask(c1)
	s.ClaimTask("worker-1", c1.ID)
	s.SubmitResult("worker-1", c1.ID, "done")
	s.AppendSchedulerBatch(schedTask.ID, c1.ID)
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: make(chan struct{}), WaitTimeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("Execute took %v with all-terminal batch, should be near-instant", elapsed)
	}
	if calls != 1 {
		t.Errorf("Inner called %d times, want 1", calls)
	}
}
func TestSchedulerExecutor_ToolHealth_PassedToSnapshot(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	th := probe.NewToolHealthStatus()
	th.Record(probe.ProbeResult{Tool: "web_search", Available: false, Error: "search_api_key 未配置"})
	th.Record(probe.ProbeResult{Tool: "web_fetch", Available: true})
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: make(chan struct{}), WaitTimeout: 100 * time.Millisecond, ToolHealth: th}
	_, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	if mail == "" {
		t.Fatal("IncomingMail should be non-empty")
	}
	if !strings.Contains(mail, `"unavailable_tools"`) {
		t.Errorf("snapshot should contain unavailable_tools field, got: %s", mail)
	}
	if !strings.Contains(mail, `"web_search"`) {
		t.Errorf("snapshot should list web_search as unavailable, got: %s", mail)
	}
	if strings.Contains(mail, `"web_fetch"`) {
		t.Errorf("snapshot should not list web_fetch (it's available), got: %s", mail)
	}
}
func TestSchedulerExecutor_ToolHealth_Nil_NoUnavailableTools(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	schedTask := &model.Task{Description: "scheduler", EventType: "__scheduler__"}
	s.PublishTask(schedTask)
	s.ClaimTask("scheduler-1", schedTask.ID)
	var calls int32
	var capturedHistory []contextcontract.HistoryEntry
	exec := &SchedulerExecutor{Inner: makeInnerExecutor(&calls, &capturedHistory), Store: s, Cfg: cfg, BatchUpdateCh: make(chan struct{}), WaitTimeout: 100 * time.Millisecond}
	_, err := exec.Execute(context.Background(), schedTask, nil, nil, llm.DefaultOutputBudget())
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(capturedHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(capturedHistory))
	}
	mail := capturedHistory[0].IncomingMail
	if strings.Contains(mail, `"unavailable_tools"`) {
		t.Errorf("snapshot should NOT contain unavailable_tools when ToolHealth is nil, got: %s", mail)
	}
}

func makeInnerExecutor(callCount *int32, capturedHistory *[]contextcontract.HistoryEntry) agent.TaskExecutor {
	return func(ctx context.Context, task *model.Task, deps map[string]string, history []contextcontract.HistoryEntry, actionBudget llm.OutputBudget) (agent.ExecuteResult, error) {
		atomic.AddInt32(callCount, 1)
		// 拷贝防止 caller 修改
		hCopy := make([]contextcontract.HistoryEntry, len(history))
		copy(hCopy, history)
		*capturedHistory = hCopy
		return agent.ExecuteResult{
			Output:     "ok",
			ToolCalled: false,
		}, nil
	}
}

// TestSchedulerExecutorBlocksLaterToolWhenControllerCancelledInSameResponse 验证
// ToolDispatchGuard 的活性检查：同一 LLM 响应中，前一个工具把 controller 任务
// 取消后，后一个工具的派发被 guard 拒绝（ctx 未取消 + 任务仍 processing 才放行）。