package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// v5 Phase 3 引入 Agent 运行时状态机（ReactiveSystem.md §7）。
// 本文件覆盖 4 类测试：
//  1. SetState 单元行为：合法切换、自循环 no-op、非法切换返回 error
//  2. mustSetState：非法切换 panic
//  3. trace 自动 emit：每次合法切换同步写 KindAgentStateChanged
//  4. 端到端：processTask 跑一遍后 jsonl 中能看到完整状态链路

func TestSetState_LegalTransitions(t *testing.T) {
	cases := []struct {
		name string
		from AgentRuntimeState
		to   AgentRuntimeState
	}{
		{"idle->processing", AgentStateIdle, AgentStateProcessing},
		{"processing->waiting_approval", AgentStateProcessing, AgentStateWaitingApproval},
		{"processing->terminating", AgentStateProcessing, AgentStateTerminating},
		{"waiting_approval->processing", AgentStateWaitingApproval, AgentStateProcessing},
		{"waiting_approval->terminating", AgentStateWaitingApproval, AgentStateTerminating},
		{"terminating->idle", AgentStateTerminating, AgentStateIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{ID: "a"}
			a.stateGuard.state = tc.from
			if err := a.SetState(tc.to, "test", "task-test"); err != nil {
				t.Fatalf("SetState(%s→%s) returned error: %v", tc.from, tc.to, err)
			}
			if got := a.CurrentState(); got != tc.to {
				t.Errorf("CurrentState=%s, want %s", got, tc.to)
			}
		})
	}
}

func TestSetState_WaitingApprovalRejectedReturnsProcessing(t *testing.T) {
	// ReactiveSystem.md §7.3.6 要求专门护住 rejected 语义：
	// rejected 只是工具错误结果，agent 必须回到 processing，而不是 terminating。
	a := &Agent{ID: "a"}
	a.stateGuard.state = AgentStateWaitingApproval

	if err := a.SetState(AgentStateProcessing, "rejected", "task-rejected"); err != nil {
		t.Fatalf("waiting_approval -> processing with rejected should be legal: %v", err)
	}
	if got := a.CurrentState(); got != AgentStateProcessing {
		t.Errorf("CurrentState=%s, want processing", got)
	}
}

func TestSetState_IllegalTransitions(t *testing.T) {
	cases := []struct {
		name string
		from AgentRuntimeState
		to   AgentRuntimeState
	}{
		{"idle->terminating", AgentStateIdle, AgentStateTerminating},
		{"idle->waiting_approval", AgentStateIdle, AgentStateWaitingApproval},
		{"processing->idle", AgentStateProcessing, AgentStateIdle},
		{"terminating->processing", AgentStateTerminating, AgentStateProcessing},
		{"terminating->waiting_approval", AgentStateTerminating, AgentStateWaitingApproval},
		{"waiting_approval->idle", AgentStateWaitingApproval, AgentStateIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{ID: "a"}
			a.stateGuard.state = tc.from
			err := a.SetState(tc.to, "test", "task-test")
			if err == nil {
				t.Fatalf("SetState(%s→%s) should fail", tc.from, tc.to)
			}
			if got := a.CurrentState(); got != tc.from {
				t.Errorf("non-transitional state should remain %s, got %s", tc.from, got)
			}
		})
	}
}

func TestMustSetState_PanicOnIllegalTransitions(t *testing.T) {
	cases := []struct {
		name string
		from AgentRuntimeState
		to   AgentRuntimeState
	}{
		{"idle->terminating", AgentStateIdle, AgentStateTerminating},
		{"idle->waiting_approval", AgentStateIdle, AgentStateWaitingApproval},
		{"processing->idle", AgentStateProcessing, AgentStateIdle},
		{"terminating->processing", AgentStateTerminating, AgentStateProcessing},
		{"terminating->waiting_approval", AgentStateTerminating, AgentStateWaitingApproval},
		{"waiting_approval->idle", AgentStateWaitingApproval, AgentStateIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{ID: "a"}
			a.stateGuard.state = tc.from

			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("mustSetState(%s -> %s) should panic", tc.from, tc.to)
				}
				if !strings.Contains(fmt.Sprint(r), "illegal agent state transition") {
					t.Fatalf("panic value %v should contain illegal transition message", r)
				}
			}()
			a.mustSetState(tc.to, "test", "task-test")
		})
	}
}

func TestSetState_SelfLoopNoop(t *testing.T) {
	// §7.3.3：自循环合法但 no-op，不 emit trace、不变 state
	a := &Agent{ID: "a"}
	a.stateGuard.state = AgentStateProcessing

	// 切换到独立 trace writer，确认 no-op 不 emit
	tmpDir := t.TempDir()
	w, err := trace.NewWriter(tmpDir, 100)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	originalDefault := trace.Default()
	trace.SetDefault(w)
	t.Cleanup(func() { trace.SetDefault(originalDefault) })

	if err := a.SetState(AgentStateProcessing, "self-loop", "task-noop"); err != nil {
		t.Fatalf("self-loop should be no-op, got error: %v", err)
	}
	if got := a.CurrentState(); got != AgentStateProcessing {
		t.Errorf("state should remain processing, got %s", got)
	}

	// 关闭 writer 后磁盘上不应有任何 KindAgentStateChanged 事件
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, ev := range readAllStateEvents(t, tmpDir) {
		if ev.Kind == trace.KindAgentStateChanged {
			t.Errorf("self-loop should not emit KindAgentStateChanged, got %+v", ev.Transition)
		}
	}
}

func TestSetState_EmitsTraceEvent(t *testing.T) {
	a := &Agent{ID: "agent-emit"}

	tmpDir := t.TempDir()
	w, err := trace.NewWriter(tmpDir, 100)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	originalDefault := trace.Default()
	trace.SetDefault(w)
	t.Cleanup(func() { trace.SetDefault(originalDefault) })

	if err := a.SetState(AgentStateProcessing, "task_claimed:t1", "t1"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := a.SetState(AgentStateTerminating, "react_loop_exit:natural", "t1"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if err := a.SetState(AgentStateIdle, "task_end_hook_done", "t1"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readAllStateEvents(t, tmpDir)
	var stateEvents []trace.Event
	for _, ev := range events {
		if ev.Kind == trace.KindAgentStateChanged {
			stateEvents = append(stateEvents, ev)
		}
	}
	if len(stateEvents) != 3 {
		t.Fatalf("expected 3 KindAgentStateChanged events, got %d", len(stateEvents))
	}

	want := []struct {
		prev, next, cause string
	}{
		{"idle", "processing", "task_claimed:t1"},
		{"processing", "terminating", "react_loop_exit:natural"},
		{"terminating", "idle", "task_end_hook_done"},
	}
	for i, w := range want {
		ev := stateEvents[i]
		if ev.Transition == nil {
			t.Errorf("event[%d] missing Transition", i)
			continue
		}
		if ev.Transition.PrevState != w.prev || ev.Transition.NewState != w.next {
			t.Errorf("event[%d]: prev=%s new=%s, want %s→%s",
				i, ev.Transition.PrevState, ev.Transition.NewState, w.prev, w.next)
		}
		if ev.Transition.Cause != w.cause {
			t.Errorf("event[%d]: cause=%q want %q", i, ev.Transition.Cause, w.cause)
		}
		if ev.AgentID != "agent-emit" {
			t.Errorf("event[%d]: agentID=%q want agent-emit", i, ev.AgentID)
		}
	}
}

func TestMustSetState_PanicOnIllegal(t *testing.T) {
	a := &Agent{ID: "a"}
	a.stateGuard.state = AgentStateIdle

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("mustSetState should panic on illegal transition")
		}
		s, ok := r.(string)
		if !ok || !strings.Contains(s, "illegal agent state transition") {
			t.Errorf("panic value %v should contain 'illegal agent state transition'", r)
		}
	}()
	// idle → terminating 是非法的
	a.mustSetState(AgentStateTerminating, "buggy", "task-bug")
}

// ── v5 Phase 5：Reactor 调用栈守卫（ReactiveSystem.md §7.2.6 原则 4）────

// TestSetState_PanicWhenCalledFromSyncReactor 验证：在 Sync Reactor.Run 内
// 调 SetState 必然触发 guard panic。
//
// 走法：reactor 内部用 defer recover 捕获 SetState 的 panic（runSync 自己也
// recover 但只记日志），然后通过 channel 把 panic 信息送出来断言。
func TestSetState_PanicWhenCalledFromSyncReactor(t *testing.T) {
	a := &Agent{ID: "agent-test"}
	a.stateGuard.state = AgentStateIdle

	panicked := make(chan any, 1)
	reg := reactor.NewRegistry()
	r := &fakeSyncReactor{
		name: "evil-state-mutator",
		kind: trace.KindTaskCompleted,
		runFn: func() {
			defer func() { panicked <- recover() }()
			_ = a.SetState(AgentStateProcessing, "from_reactor", "T-1")
		},
	}
	if err := reg.Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}

	reg.Dispatch(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-1"})

	select {
	case rec := <-panicked:
		if rec == nil {
			t.Fatal("Sync reactor should have panicked from SetState guard")
		}
		msg := fmt.Sprintf("%v", rec)
		if !strings.Contains(msg, "BUG") || !strings.Contains(msg, "Reactor goroutine") {
			t.Errorf("guard panic message wrong: %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("Sync reactor did not panic within 1s")
	}

	if got := a.CurrentState(); got != AgentStateIdle {
		t.Errorf("state changed to %s — guard should have blocked the transition", got)
	}
}

// TestSetState_PanicWhenCalledFromAsyncReactor 同上但走 Async 路径。
// runAsync 也 recover panic 但仅记日志，不写 trace；本测试通过 channel 接收
// fakeAsyncReactor 自身在 panic 前后是否完成来断言 guard 触发。
func TestSetState_PanicWhenCalledFromAsyncReactor(t *testing.T) {
	a := &Agent{ID: "agent-test"}
	a.stateGuard.state = AgentStateIdle

	panicked := make(chan any, 1)
	reg := reactor.NewRegistry()
	r := &fakeAsyncReactor{
		name: "evil-async",
		kind: trace.KindTaskCompleted,
		runFn: func() {
			defer func() {
				panicked <- recover()
			}()
			_ = a.SetState(AgentStateProcessing, "from_async_reactor", "T-1")
		},
	}
	if err := reg.Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}

	reg.Dispatch(trace.Event{Kind: trace.KindTaskCompleted, TaskID: "T-1"})

	select {
	case rec := <-panicked:
		msg := fmt.Sprintf("%v", rec)
		if rec == nil {
			t.Fatal("Async reactor should have panicked from SetState guard")
		}
		if !strings.Contains(msg, "BUG") || !strings.Contains(msg, "Reactor goroutine") {
			t.Errorf("guard panic message wrong: %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("async reactor did not panic within 1s")
	}

	if got := a.CurrentState(); got != AgentStateIdle {
		t.Errorf("state changed to %s — guard should have blocked the transition", got)
	}
}

// TestSetState_NoPanicFromMainFlow 反向验证：main flow（非 reactor 栈）调
// SetState 仍然正常工作。
func TestSetState_NoPanicFromMainFlow(t *testing.T) {
	a := &Agent{ID: "agent-test"}
	a.stateGuard.state = AgentStateIdle
	if err := a.SetState(AgentStateProcessing, "task_claimed", "T-1"); err != nil {
		t.Fatalf("main-flow SetState should succeed, got %v", err)
	}
	if got := a.CurrentState(); got != AgentStateProcessing {
		t.Errorf("state=%s want processing", got)
	}
}

func TestSetState_StateChangedReactorCanReadCurrentState(t *testing.T) {
	a := &Agent{ID: "agent-test"}
	a.stateGuard.state = AgentStateIdle

	reg := reactor.NewRegistry()
	stateSeen := make(chan AgentRuntimeState, 1)
	r := &fakeSyncReactor{
		name: "state-observer",
		kind: trace.KindAgentStateChanged,
		runFn: func() {
			stateSeen <- a.CurrentState()
		},
	}
	if err := reg.Register(r); err != nil {
		t.Fatalf("Register: %v", err)
	}

	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	done := make(chan error, 1)
	go func() {
		done <- a.SetState(AgentStateProcessing, "task_claimed", "T-1")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SetState: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SetState deadlocked while dispatching agent_state_changed")
	}

	select {
	case got := <-stateSeen:
		if got != AgentStateProcessing {
			t.Errorf("observer saw state=%s want processing", got)
		}
	case <-time.After(time.Second):
		t.Fatal("state observer reactor did not run")
	}
}

// fakeSyncReactor / fakeAsyncReactor 是 reactor.Reactor 的最小测试 stub。
type fakeSyncReactor struct {
	name  string
	kind  trace.EventKind
	runFn func()
}

func (s *fakeSyncReactor) Name() string                 { return s.name }
func (s *fakeSyncReactor) Subscribe() []trace.EventKind { return []trace.EventKind{s.kind} }
func (s *fakeSyncReactor) IsSync() bool                 { return true }
func (s *fakeSyncReactor) Priority() int                { return 500 }
func (s *fakeSyncReactor) Run(ev trace.Event) error     { s.runFn(); return nil }

type fakeAsyncReactor struct {
	name  string
	kind  trace.EventKind
	runFn func()
}

func (s *fakeAsyncReactor) Name() string                 { return s.name }
func (s *fakeAsyncReactor) Subscribe() []trace.EventKind { return []trace.EventKind{s.kind} }
func (s *fakeAsyncReactor) IsSync() bool                 { return false }
func (s *fakeAsyncReactor) Priority() int                { return 500 }
func (s *fakeAsyncReactor) Run(ev trace.Event) error     { s.runFn(); return nil }

func TestMustSetState_OKOnLegal(t *testing.T) {
	a := &Agent{ID: "a"}
	a.stateGuard.state = AgentStateIdle

	// 不应 panic
	a.mustSetState(AgentStateProcessing, "task_claimed:t1", "t1")
	if got := a.CurrentState(); got != AgentStateProcessing {
		t.Errorf("CurrentState=%s, want processing", got)
	}
}

func TestSetState_ZeroValueTreatedAsIdle(t *testing.T) {
	// 新建的 Agent 零值（runtimeState 字段为空串）应视作 Idle
	a := &Agent{ID: "a"}
	if got := a.CurrentState(); got != AgentStateIdle {
		t.Errorf("zero-value Agent should be idle, got %s", got)
	}
	// idle → processing 应合法
	if err := a.SetState(AgentStateProcessing, "test", "task-zv"); err != nil {
		t.Errorf("zero-value → processing should succeed, got %v", err)
	}
}

// TestE2E_AgentStateMachineLifecycle 端到端跑完一个 task，从 trace jsonl 验证
// 三条 KindAgentStateChanged 事件按 idle→processing→terminating→idle 顺序落盘。
//
// 这是 §7.3 SetState 系统在 processTask 内 6 个穿插点（v5 当前阶段接入 4 个非
// approval 点）的完整路径验证——保证：
//   - mustSetState(Processing) 在 processTask 入口确实被调用
//   - 合并 defer 内的 mustSetState(Terminating) 在 OnTaskEnd 之前跑
//   - 末尾 defer 的 mustSetState(Idle) 是最后一个 SetState
func TestE2E_AgentStateMachineLifecycle(t *testing.T) {
	s, r, _ := setup()

	tmpDir := t.TempDir()
	w, err := trace.NewWriter(tmpDir, 100)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	originalDefault := trace.Default()
	trace.SetDefault(w)
	t.Cleanup(func() { trace.SetDefault(originalDefault) })

	task := &model.Task{Description: "state machine e2e", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	// 极简 executor：单轮自然完成
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done", ToolCalled: false}, nil
	}

	ag := NewAgent("agent-e2e", "code", s, r, executor, 5)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ag.Run(ctx)

	// 等任务完成
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("等待任务完成超时")
		default:
		}
		got, gerr := s.GetTask(task.ID)
		if gerr != nil {
			t.Fatalf("GetTask: %v", gerr)
		}
		if got.Status == model.TaskStatusCompleted || got.Status == model.TaskStatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readAllStateEvents(t, tmpDir)
	var stateChain []*trace.Transition
	for _, ev := range events {
		if ev.Kind == trace.KindAgentStateChanged && ev.Transition != nil {
			stateChain = append(stateChain, ev.Transition)
		}
	}

	if len(stateChain) != 3 {
		t.Fatalf("expected 3 state transitions in lifecycle, got %d:\n%+v", len(stateChain), stateChain)
	}

	want := []struct {
		prev, next string
	}{
		{"idle", "processing"},
		{"processing", "terminating"},
		{"terminating", "idle"},
	}
	for i, w := range want {
		got := stateChain[i]
		if got.PrevState != w.prev || got.NewState != w.next {
			t.Errorf("transition[%d]: %s→%s, want %s→%s",
				i, got.PrevState, got.NewState, w.prev, w.next)
		}
	}

	// cause 字段应填充：
	//  [0] task_claimed:<id>
	//  [1] react_loop_exit:natural （单轮自然完成路径）
	//  [2] task_end_hook_done
	if !strings.HasPrefix(stateChain[0].Cause, "task_claimed:") {
		t.Errorf("transition[0] cause=%q, want prefix task_claimed:", stateChain[0].Cause)
	}
	if stateChain[1].Cause != "react_loop_exit:natural" {
		t.Errorf("transition[1] cause=%q, want react_loop_exit:natural", stateChain[1].Cause)
	}
	if stateChain[2].Cause != "task_end_hook_done" {
		t.Errorf("transition[2] cause=%q, want task_end_hook_done", stateChain[2].Cause)
	}

	terminatingIdx := indexEvent(events, func(ev trace.Event) bool {
		return ev.Kind == trace.KindAgentStateChanged &&
			ev.Transition != nil &&
			ev.Transition.PrevState == "processing" &&
			ev.Transition.NewState == "terminating"
	})
	completedIdx := indexEvent(events, func(ev trace.Event) bool {
		return ev.Kind == trace.KindTaskCompleted
	})
	if terminatingIdx < 0 || completedIdx < 0 {
		t.Fatalf("missing terminating or completed event: terminatingIdx=%d completedIdx=%d", terminatingIdx, completedIdx)
	}
	if terminatingIdx > completedIdx {
		t.Fatalf("processing->terminating must be emitted before task_completed; got terminatingIdx=%d completedIdx=%d",
			terminatingIdx, completedIdx)
	}
}

// TestE2E_AgentStateMachine_PanicPath 验证 panic 路径下 state 链路完整、cause=panic。
func TestE2E_AgentStateMachine_PanicPath(t *testing.T) {
	s, r, _ := setup()

	tmpDir := t.TempDir()
	w, err := trace.NewWriter(tmpDir, 100)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	originalDefault := trace.Default()
	trace.SetDefault(w)
	t.Cleanup(func() { trace.SetDefault(originalDefault) })

	task := &model.Task{Description: "panic e2e", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	// executor 触发 panic，验证合并 defer 走 panic 分支后状态链路依然完整
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		panic(errors.New("intentional panic for test"))
	}

	ag := NewAgent("agent-panic", "code", s, r, executor, 5)
	ag.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go ag.Run(ctx)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("panic e2e: 等待任务终态超时")
		default:
		}
		got, gerr := s.GetTask(task.ID)
		if gerr != nil {
			t.Fatalf("GetTask: %v", gerr)
		}
		if got.Status == model.TaskStatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readAllStateEvents(t, tmpDir)
	var stateChain []*trace.Transition
	for _, ev := range events {
		if ev.Kind == trace.KindAgentStateChanged && ev.Transition != nil {
			stateChain = append(stateChain, ev.Transition)
		}
	}

	// panic 路径仍应有完整 3 条切换：idle→processing → processing→terminating → terminating→idle
	if len(stateChain) != 3 {
		t.Fatalf("panic 路径应当有 3 条状态切换，实际 %d:\n%+v", len(stateChain), stateChain)
	}
	if stateChain[1].Cause != "react_loop_exit:panic" {
		t.Errorf("panic 路径下 processing→terminating cause 应为 react_loop_exit:panic, 实际 %q",
			stateChain[1].Cause)
	}

	terminatingIdx := indexEvent(events, func(ev trace.Event) bool {
		return ev.Kind == trace.KindAgentStateChanged &&
			ev.Transition != nil &&
			ev.Transition.PrevState == "processing" &&
			ev.Transition.NewState == "terminating"
	})
	failedIdx := indexEvent(events, func(ev trace.Event) bool {
		return ev.Kind == trace.KindTaskFailed
	})
	if terminatingIdx < 0 || failedIdx < 0 {
		t.Fatalf("missing terminating or failed event: terminatingIdx=%d failedIdx=%d", terminatingIdx, failedIdx)
	}
	if terminatingIdx > failedIdx {
		t.Fatalf("processing->terminating must be emitted before task_failed on panic path; got terminatingIdx=%d failedIdx=%d",
			terminatingIdx, failedIdx)
	}
}

// readAllStateEvents 读取 tmpDir 下所有 .jsonl 文件中的事件。
// 与 truncate_e2e_test.go 内部逻辑同型，因测试需求多次复用故抽出。
func readAllStateEvents(t *testing.T, tmpDir string) []trace.Event {
	t.Helper()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", tmpDir, err)
	}
	var all []trace.Event
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, ferr := os.Open(filepath.Join(tmpDir, e.Name()))
		if ferr != nil {
			t.Fatalf("Open: %v", ferr)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var ev trace.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			all = append(all, ev)
		}
		_ = f.Close()
	}
	return all
}

func indexEvent(events []trace.Event, match func(trace.Event) bool) int {
	for i, ev := range events {
		if match(ev) {
			return i
		}
	}
	return -1
}
