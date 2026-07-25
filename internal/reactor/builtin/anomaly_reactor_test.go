package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// ---------- 测试替身 ----------

// fakeAnomalyStore 实现 anomalyStoreView，返回预设的任务与工具历史。
type fakeAnomalyStore struct {
	task    *model.Task
	taskErr error
	history []store.ToolCallRecord
}

func (f *fakeAnomalyStore) GetTask(string) (*model.Task, error) { return f.task, f.taskErr }
func (f *fakeAnomalyStore) GetToolCallHistory(string) []store.ToolCallRecord {
	return f.history
}

// fakeReplanRequester 记录收到的 ReplanRequest，可注入错误。
type fakeReplanRequester struct {
	mu       sync.Mutex
	requests []model.ReplanRequest
	err      error
}

func (f *fakeReplanRequester) RequestReplan(_ context.Context, req model.ReplanRequest) (*model.ReplanRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	cp := req
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("req-%d", len(f.requests)+1)
	}
	f.requests = append(f.requests, cp)
	return &cp, nil
}

func (f *fakeReplanRequester) snapshot() []model.ReplanRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.ReplanRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// captureReactor 同步捕获 KindError 事件（同步 → 在 trace.Emit 调用方
// goroutine 内执行，断言无需等待）。
type captureReactor struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *captureReactor) Name() string                 { return "capture" }
func (c *captureReactor) Subscribe() []trace.EventKind { return []trace.EventKind{trace.KindError} }
func (c *captureReactor) IsSync() bool                 { return true }
func (c *captureReactor) Priority() int                { return 500 }
func (c *captureReactor) Run(ev trace.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *captureReactor) snapshot() []trace.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]trace.Event, len(c.events))
	copy(out, c.events)
	return out
}

// installCapture 注册 KindError 捕获 Reactor 并设为全局 dispatcher，返回捕获器。
func installCapture(t *testing.T) *captureReactor {
	t.Helper()
	cap := &captureReactor{}
	reg := reactor.NewRegistry()
	if err := reg.Register(cap); err != nil {
		t.Fatalf("Register capture: %v", err)
	}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })
	return cap
}

// ---------- 构造工具历史的辅助 ----------

func okCall(tool string) store.ToolCallRecord {
	return store.ToolCallRecord{ToolName: tool, Success: true}
}
func errCall(tool string) store.ToolCallRecord {
	return store.ToolCallRecord{ToolName: tool, Success: false}
}

func completedEv(taskID string) trace.Event {
	return trace.Event{Kind: trace.KindTaskCompleted, TaskID: taskID, AgentID: "agent-1"}
}

// ---------- 元数据 / 接口 ----------

func TestAnomalyReactor_Metadata(t *testing.T) {
	r := NewAnomalyReactor(nil, nil)
	var _ reactor.Reactor = r
	if r.Name() != "runtime-anomaly" {
		t.Errorf("Name = %q", r.Name())
	}
	if r.IsSync() {
		t.Error("IsSync 应为 false（异步观察类）")
	}
	if p := r.Priority(); p < 0 || p > 1000 {
		t.Errorf("Priority 越界: %d", p)
	}
	kinds := r.Subscribe()
	if len(kinds) != 1 || kinds[0] != trace.KindTaskCompleted {
		t.Errorf("Subscribe = %v, 应只含 KindTaskCompleted", kinds)
	}
}

func TestAnomalyReactor_NilDepsNoPanic(t *testing.T) {
	r := NewAnomalyReactor(nil, nil)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Errorf("nil store 应静默 no-op，got err=%v", err)
	}
}

func TestAnomalyReactor_EmptyTaskIDNoPanic(t *testing.T) {
	r := NewAnomalyReactor(&fakeAnomalyStore{history: []store.ToolCallRecord{okCall("write_file")}}, &fakeReplanRequester{})
	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted}); err != nil {
		t.Errorf("空 TaskID 应静默 no-op，got err=%v", err)
	}
}

func TestAnomalyReactor_NoHistoryNoAnomaly(t *testing.T) {
	req := &fakeReplanRequester{}
	r := NewAnomalyReactor(&fakeAnomalyStore{task: &model.Task{PlanID: "p-1"}}, req)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := req.snapshot(); len(got) != 0 {
		t.Errorf("无工具历史不应报警，got %d 条请求", len(got))
	}
}

// ---------- 启发式 ②：fabricated_write ----------

func TestAnomalyReactor_FabricatedWriteHit(t *testing.T) {
	req := &fakeReplanRequester{}
	st := &fakeAnomalyStore{
		task:    &model.Task{ID: "t-1", PlanID: "p-1"},
		history: []store.ToolCallRecord{okCall("write_file"), okCall("write_file")},
	}
	r := NewAnomalyReactor(st, req)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := req.snapshot()
	if len(got) != 1 {
		t.Fatalf("应产生 1 条重规划请求，got %d", len(got))
	}
	rr := got[0]
	if rr.PlanID != "p-1" || rr.SourceTaskID != "t-1" {
		t.Errorf("PlanID/SourceTaskID = %q/%q", rr.PlanID, rr.SourceTaskID)
	}
	if rr.SourceEvent != "anomaly_reactor" {
		t.Errorf("SourceEvent = %q", rr.SourceEvent)
	}
	if rr.ReasonCode != anomalyCodeFabricatedWrite {
		t.Errorf("ReasonCode = %q", rr.ReasonCode)
	}
	if rr.Urgency != model.ReplanUrgencyNormal {
		t.Errorf("Urgency = %q, 应 normal", rr.Urgency)
	}
	wantKey := "anomaly_reactor|t-1|" + anomalyCodeFabricatedWrite
	if rr.IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", rr.IdempotencyKey, wantKey)
	}
	if !strings.Contains(rr.Detail, "read_file") {
		t.Errorf("Detail 应含人类可读说明: %q", rr.Detail)
	}
}

func TestAnomalyReactor_FabricatedWriteMiss(t *testing.T) {
	cases := map[string][]store.ToolCallRecord{
		"有 read_file 即不构成凭空写入":  {okCall("read_file"), okCall("write_file")},
		"失败的 read_file 也算有读取尝试": {errCall("read_file"), okCall("write_file")},
		"write_file 全部失败不算写入发生": {errCall("write_file")},
		"edit_file 不在本启发式写入集合内": {okCall("edit_file")},
		"list_dir 等其他工具不触发":     {okCall("list_dir"), okCall("grep_search")},
	}
	for name, history := range cases {
		req := &fakeReplanRequester{}
		st := &fakeAnomalyStore{task: &model.Task{ID: "t-1", PlanID: "p-1"}, history: history}
		r := NewAnomalyReactor(st, req)
		if err := r.Run(completedEv("t-1")); err != nil {
			t.Fatalf("%s: Run: %v", name, err)
		}
		if got := req.snapshot(); len(got) != 0 {
			t.Errorf("%s: 不应报警，got %d 条请求", name, len(got))
		}
	}
}

// ---------- 启发式 ③：tool_error_rate ----------

func TestAnomalyReactor_ToolErrorRate(t *testing.T) {
	build := func(errs, total int) []store.ToolCallRecord {
		h := make([]store.ToolCallRecord, 0, total)
		for i := 0; i < total; i++ {
			if i < errs {
				h = append(h, errCall("read_file"))
			} else {
				h = append(h, okCall("read_file"))
			}
		}
		return h
	}
	cases := []struct {
		name    string
		history []store.ToolCallRecord
		wantHit bool
	}{
		{"10 调用 4 错（40%）命中", build(4, 10), true},
		{"10 调用 3 错（30%，严格大于才不报）不命中", build(3, 10), false},
		{"5 调用 2 错（40%）命中", build(2, 5), true},
		{"5 调用 1 错（20%）不命中", build(1, 5), false},
		{"4 调用 4 错（100% 但样本不足 5）跳过", build(4, 4), false},
		{"0 调用不命中", nil, false},
	}
	for _, tc := range cases {
		req := &fakeReplanRequester{}
		st := &fakeAnomalyStore{task: &model.Task{ID: "t-1", PlanID: "p-1"}, history: tc.history}
		r := NewAnomalyReactor(st, req)
		if err := r.Run(completedEv("t-1")); err != nil {
			t.Fatalf("%s: Run: %v", tc.name, err)
		}
		got := req.snapshot()
		if tc.wantHit {
			if len(got) != 1 || got[0].ReasonCode != anomalyCodeToolErrorRate {
				t.Errorf("%s: 应命中 tool_error_rate，got %+v", tc.name, got)
			}
		} else if len(got) != 0 {
			t.Errorf("%s: 不应报警，got %+v", tc.name, got)
		}
	}
}

func TestAnomalyReactor_ToolErrorRateDetail(t *testing.T) {
	req := &fakeReplanRequester{}
	history := []store.ToolCallRecord{
		errCall("run_shell"), errCall("read_file"), okCall("read_file"), okCall("list_dir"), okCall("read_file"),
	}
	st := &fakeAnomalyStore{task: &model.Task{ID: "t-1", PlanID: "p-1"}, history: history}
	r := NewAnomalyReactor(st, req)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := req.snapshot()
	if len(got) != 1 {
		t.Fatalf("应 1 条请求，got %d", len(got))
	}
	if !strings.Contains(got[0].Detail, "40% (2/5)") {
		t.Errorf("Detail 应含错误率统计: %q", got[0].Detail)
	}
}

// ---------- 双异常并发 ----------

func TestAnomalyReactor_BothAnomaliesReportSeparately(t *testing.T) {
	req := &fakeReplanRequester{}
	// 5 次调用：write_file 全成功 + 2 次其他工具失败 → 同时命中 ②③
	history := []store.ToolCallRecord{
		okCall("write_file"), errCall("run_shell"), errCall("run_shell"), okCall("write_file"), okCall("write_file"),
	}
	st := &fakeAnomalyStore{task: &model.Task{ID: "t-1", PlanID: "p-1"}, history: history}
	r := NewAnomalyReactor(st, req)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := req.snapshot()
	if len(got) != 2 {
		t.Fatalf("两条异常应各报一次，got %d: %+v", len(got), got)
	}
	keys := map[string]bool{}
	for _, rr := range got {
		keys[rr.IdempotencyKey] = true
	}
	if !keys["anomaly_reactor|t-1|"+anomalyCodeFabricatedWrite] ||
		!keys["anomaly_reactor|t-1|"+anomalyCodeToolErrorRate] {
		t.Errorf("幂等键应各含 taskID+异常码: %v", keys)
	}
}

// ---------- 无 Plan 降级 ----------

func TestAnomalyReactor_NoPlanDegradesToTraceWarning(t *testing.T) {
	cap := installCapture(t)
	req := &fakeReplanRequester{}
	st := &fakeAnomalyStore{
		task:    &model.Task{ID: "t-1"}, // 无 PlanID
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st, req)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := req.snapshot(); len(got) != 0 {
		t.Errorf("无 Plan 任务不应请求重规划，got %d", len(got))
	}
	evs := cap.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应发 1 条 KindError 告警，got %d", len(evs))
	}
	if evs[0].Kind != trace.KindError || evs[0].TaskID != "t-1" {
		t.Errorf("告警事件字段不对: %+v", evs[0])
	}
	if !strings.Contains(evs[0].Error, anomalyCodeFabricatedWrite) ||
		!strings.Contains(evs[0].Error, "WARNING anomaly_reactor") {
		t.Errorf("告警内容应含异常码与 WARNING 前缀: %q", evs[0].Error)
	}
}

func TestAnomalyReactor_GetTaskErrorDegradesToTraceWarning(t *testing.T) {
	cap := installCapture(t)
	req := &fakeReplanRequester{}
	st := &fakeAnomalyStore{
		taskErr: fmt.Errorf("task 已被淘汰"),
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st, req)
	if err := r.Run(completedEv("t-gone")); err != nil {
		t.Fatalf("GetTask 失败不应报错（降级告警），got %v", err)
	}
	if got := req.snapshot(); len(got) != 0 {
		t.Errorf("任务查不到不应请求重规划，got %d", len(got))
	}
	if evs := cap.snapshot(); len(evs) != 1 {
		t.Errorf("应降级发 1 条 trace 告警，got %d", len(evs))
	}
}

// ---------- 幂等防重 ----------

func TestAnomalyReactor_SameTaskSameCodeReportedOnce(t *testing.T) {
	req := &fakeReplanRequester{}
	st := &fakeAnomalyStore{
		task:    &model.Task{ID: "t-1", PlanID: "p-1"},
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st, req)
	// 模拟任务重试后再次 completed：同一 (taskID, code) 只报一次
	for i := 0; i < 3; i++ {
		if err := r.Run(completedEv("t-1")); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}
	if got := req.snapshot(); len(got) != 1 {
		t.Errorf("同一任务同一异常码应只报一次，got %d 条请求", len(got))
	}
}

func TestAnomalyReactor_DifferentTasksReportIndependently(t *testing.T) {
	req := &fakeReplanRequester{}
	st := &fakeAnomalyStore{
		task:    &model.Task{ID: "t-1", PlanID: "p-1"},
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st, req)
	_ = r.Run(completedEv("t-1"))
	_ = r.Run(completedEv("t-2"))
	if got := req.snapshot(); len(got) != 2 {
		t.Errorf("不同任务各自独立审计，应报 2 次，got %d", len(got))
	}
}

// TestAnomalyReactor_CoordinatorIdempotencyAcrossInstances 用真实
// plan.Coordinator 验证：即使进程内防重被绕过（两个 Reactor 实例，各自的
// reported 集合为空），相同幂等键在 Coordinator 侧也只落一条 ReplanRequest。
func TestAnomalyReactor_CoordinatorIdempotencyAcrossInstances(t *testing.T) {
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: "p-1", RootTaskID: "t-root", Budget: model.PlanBudget{},
	}); err != nil {
		t.Fatalf("Create plan: %v", err)
	}
	st := &fakeAnomalyStore{
		task:    &model.Task{ID: "t-1", PlanID: "p-1"},
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	// 两个独立实例 → 进程内 reported 不共享，全靠 Coordinator 幂等键去重
	r1 := NewAnomalyReactor(st, coordinator)
	r2 := NewAnomalyReactor(st, coordinator)
	if err := r1.Run(completedEv("t-1")); err != nil {
		t.Fatalf("r1.Run: %v", err)
	}
	if err := r2.Run(completedEv("t-1")); err != nil {
		t.Fatalf("r2.Run: %v", err)
	}
	p, err := coordinator.Store().GetPlan("p-1")
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if len(p.PendingReplanRequests) != 1 {
		t.Fatalf("相同幂等键应只落 1 条 ReplanRequest，got %d", len(p.PendingReplanRequests))
	}
	for _, rr := range p.PendingReplanRequests {
		if rr.ReasonCode != anomalyCodeFabricatedWrite || rr.SourceEvent != "anomaly_reactor" {
			t.Errorf("落库请求字段不对: %+v", rr)
		}
		if rr.Urgency != model.ReplanUrgencyNormal {
			t.Errorf("Urgency = %q, 应 normal", rr.Urgency)
		}
	}
}

// ---------- Plan 终态 / 错误处理 ----------

func TestAnomalyReactor_PlanTerminalFallsBackToTraceWarning(t *testing.T) {
	cap := installCapture(t)
	req := &fakeReplanRequester{err: plan.ErrPlanTerminal}
	st := &fakeAnomalyStore{
		task:    &model.Task{ID: "t-1", PlanID: "p-1"},
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st, req)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Plan 终态应降级告警而非报错，got %v", err)
	}
	if evs := cap.snapshot(); len(evs) != 1 {
		t.Errorf("Plan 终态应降级发 1 条 trace 告警，got %d", len(evs))
	}
}

func TestAnomalyReactor_RequesterTransientErrorPropagates(t *testing.T) {
	cap := installCapture(t)
	req := &fakeReplanRequester{err: errors.New("store 写入失败")}
	st := &fakeAnomalyStore{
		task:    &model.Task{ID: "t-1", PlanID: "p-1"},
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st, req)
	err := r.Run(completedEv("t-1"))
	if err == nil || !strings.Contains(err.Error(), "request_replan") {
		t.Errorf("瞬时错误应返回给 Async 路径记日志，got %v", err)
	}
	if evs := cap.snapshot(); len(evs) != 0 {
		t.Errorf("瞬时错误不应降级告警（避免重复信号），got %d 条", len(evs))
	}
}

// ---------- 不直接改任务状态（Reactor 原则 4）----------

// TestAnomalyReactor_DoesNotMutateTaskState 用真实 MemoryTaskStore 验证：
// Reactor 跑完后任务的 Status / Results / Artifacts 原封不动——它对 Store
// 只有读操作（anomalyStoreView 结构上就不暴露写方法）。
func TestAnomalyReactor_DoesNotMutateTaskState(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "anomaly no-mutate", PlanID: "p-1"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := taskStore.AppendToolCall(task.ID, store.ToolCallRecord{ToolName: "write_file", Success: true}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	req := &fakeReplanRequester{}
	r := NewAnomalyReactor(taskStore, req)
	if err := r.Run(completedEv(task.ID)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 异常确实命中（请求已发出）
	if got := req.snapshot(); len(got) != 1 {
		t.Fatalf("前置条件：应命中 1 条异常，got %d", len(got))
	}
	// 任务状态原封不动
	after, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if after.Status != model.TaskStatusPending {
		t.Errorf("Reactor 不得改变任务状态，Status = %s", after.Status)
	}
	if len(after.Results) != 0 {
		t.Errorf("Reactor 不得写任务结果，Results = %v", after.Results)
	}
	if len(after.Artifacts) != 0 {
		t.Errorf("Reactor 不得写产物清单，Artifacts = %v", after.Artifacts)
	}
}

// ---------- 跨包握手端到端（trace.Emit → Registry.Dispatch → 异步 Reactor → Coordinator）----------

// TestAnomalyReactor_E2E_DispatchToReplanRequest 走真实派发链路：
// 全部用生产组件（MemoryTaskStore / plan.Coordinator / reactor.Registry /
// trace.SetDefaultDispatcher），emit 一条 KindTaskCompleted，断言异常最终
// 物化为 Plan 的 PendingReplanRequests——这是"装配漏接"级别的验证：
// 注册、订阅、异步派发、控制面写入任何一环断掉本测试都会失败。
func TestAnomalyReactor_E2E_DispatchToReplanRequest(t *testing.T) {
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: "p-e2e", RootTaskID: "t-root", Budget: model.PlanBudget{},
	}); err != nil {
		t.Fatalf("Create plan: %v", err)
	}
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "anomaly e2e", PlanID: "p-e2e"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := taskStore.AppendToolCall(task.ID, store.ToolCallRecord{ToolName: "write_file", Success: true}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	reg := reactor.NewRegistry()
	if err := reg.Register(NewAnomalyReactor(taskStore, coordinator)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })

	// 模拟主流程：任务完成
	trace.Emit(completedEv(task.ID))

	// Async Reactor：轮询等重规划请求落库
	deadline := time.After(2 * time.Second)
	for {
		p, err := coordinator.Store().GetPlan("p-e2e")
		if err != nil {
			t.Fatalf("GetPlan: %v", err)
		}
		if len(p.PendingReplanRequests) == 1 {
			for _, rr := range p.PendingReplanRequests {
				if rr.ReasonCode != anomalyCodeFabricatedWrite || rr.SourceTaskID != task.ID {
					t.Errorf("落库请求字段不对: %+v", rr)
				}
			}
			return // 通过：emit → dispatch → reactor → coordinator 全链路物化
		}
		select {
		case <-deadline:
			t.Fatalf("2s 内未等到重规划请求落库（PendingReplanRequests=%d）", len(p.PendingReplanRequests))
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}
