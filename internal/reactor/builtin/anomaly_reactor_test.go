package builtin

import (
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// ---------- 测试替身 ----------

// fakeAnomalyStore 实现 anomalyStoreView，返回预设的工具历史。
type fakeAnomalyStore struct {
	history []store.ToolCallRecord
}

func (f *fakeAnomalyStore) GetToolCallHistory(string) []store.ToolCallRecord {
	return f.history
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
	r := NewAnomalyReactor(nil)
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
	r := NewAnomalyReactor(nil)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Errorf("nil store 应静默 no-op，got err=%v", err)
	}
}

func TestAnomalyReactor_EmptyTaskIDNoPanic(t *testing.T) {
	cap := installCapture(t)
	r := NewAnomalyReactor(&fakeAnomalyStore{history: []store.ToolCallRecord{okCall("write_file")}})
	if err := r.Run(trace.Event{Kind: trace.KindTaskCompleted}); err != nil {
		t.Errorf("空 TaskID 应静默 no-op，got err=%v", err)
	}
	if evs := cap.snapshot(); len(evs) != 0 {
		t.Errorf("空 TaskID 不应告警，got %d 条", len(evs))
	}
}

func TestAnomalyReactor_NoHistoryNoAnomaly(t *testing.T) {
	cap := installCapture(t)
	r := NewAnomalyReactor(&fakeAnomalyStore{})
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if evs := cap.snapshot(); len(evs) != 0 {
		t.Errorf("无工具历史不应报警，got %d 条告警", len(evs))
	}
}

// ---------- 启发式 ②：fabricated_write ----------

func TestAnomalyReactor_FabricatedWriteHit(t *testing.T) {
	cap := installCapture(t)
	st := &fakeAnomalyStore{
		history: []store.ToolCallRecord{okCall("write_file"), okCall("write_file")},
	}
	r := NewAnomalyReactor(st)
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := cap.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应发 1 条 KindError 告警，got %d", len(evs))
	}
	ev := evs[0]
	if ev.Kind != trace.KindError || ev.TaskID != "t-1" || ev.AgentID != "agent-1" {
		t.Errorf("告警事件字段不对: %+v", ev)
	}
	if !strings.Contains(ev.Error, anomalyCodeFabricatedWrite) ||
		!strings.Contains(ev.Error, "WARNING anomaly_reactor") {
		t.Errorf("告警内容应含异常码与 WARNING 前缀: %q", ev.Error)
	}
	if !strings.Contains(ev.Error, "read_file") {
		t.Errorf("告警内容应含人类可读说明: %q", ev.Error)
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
		cap := installCapture(t)
		r := NewAnomalyReactor(&fakeAnomalyStore{history: history})
		if err := r.Run(completedEv("t-1")); err != nil {
			t.Fatalf("%s: Run: %v", name, err)
		}
		if evs := cap.snapshot(); len(evs) != 0 {
			t.Errorf("%s: 不应报警，got %d 条告警", name, len(evs))
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
		cap := installCapture(t)
		r := NewAnomalyReactor(&fakeAnomalyStore{history: tc.history})
		if err := r.Run(completedEv("t-1")); err != nil {
			t.Fatalf("%s: Run: %v", tc.name, err)
		}
		evs := cap.snapshot()
		if tc.wantHit {
			if len(evs) != 1 || !strings.Contains(evs[0].Error, anomalyCodeToolErrorRate) {
				t.Errorf("%s: 应命中 tool_error_rate，got %+v", tc.name, evs)
			}
		} else if len(evs) != 0 {
			t.Errorf("%s: 不应报警，got %+v", tc.name, evs)
		}
	}
}

func TestAnomalyReactor_ToolErrorRateDetail(t *testing.T) {
	cap := installCapture(t)
	history := []store.ToolCallRecord{
		errCall("run_shell"), errCall("read_file"), okCall("read_file"), okCall("list_dir"), okCall("read_file"),
	}
	r := NewAnomalyReactor(&fakeAnomalyStore{history: history})
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := cap.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应 1 条告警，got %d", len(evs))
	}
	if !strings.Contains(evs[0].Error, "40% (2/5)") {
		t.Errorf("告警内容应含错误率统计: %q", evs[0].Error)
	}
}

// ---------- 双异常并发 ----------

func TestAnomalyReactor_BothAnomaliesReportSeparately(t *testing.T) {
	cap := installCapture(t)
	// 5 次调用：write_file 全成功 + 2 次其他工具失败 → 同时命中 ②③
	history := []store.ToolCallRecord{
		okCall("write_file"), errCall("run_shell"), errCall("run_shell"), okCall("write_file"), okCall("write_file"),
	}
	r := NewAnomalyReactor(&fakeAnomalyStore{history: history})
	if err := r.Run(completedEv("t-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	evs := cap.snapshot()
	if len(evs) != 2 {
		t.Fatalf("两条异常应各报一次，got %d: %+v", len(evs), evs)
	}
	codes := map[string]bool{}
	for _, ev := range evs {
		if strings.Contains(ev.Error, anomalyCodeFabricatedWrite) {
			codes[anomalyCodeFabricatedWrite] = true
		}
		if strings.Contains(ev.Error, anomalyCodeToolErrorRate) {
			codes[anomalyCodeToolErrorRate] = true
		}
	}
	if !codes[anomalyCodeFabricatedWrite] || !codes[anomalyCodeToolErrorRate] {
		t.Errorf("两条告警应各含一个异常码: %v", codes)
	}
}

// ---------- 幂等防重 ----------

func TestAnomalyReactor_SameTaskSameCodeReportedOnce(t *testing.T) {
	cap := installCapture(t)
	st := &fakeAnomalyStore{
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st)
	// 模拟任务重试后再次 completed：同一 (taskID, code) 只报一次
	for i := 0; i < 3; i++ {
		if err := r.Run(completedEv("t-1")); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}
	if evs := cap.snapshot(); len(evs) != 1 {
		t.Errorf("同一任务同一异常码应只报一次，got %d 条告警", len(evs))
	}
}

func TestAnomalyReactor_DifferentTasksReportIndependently(t *testing.T) {
	cap := installCapture(t)
	st := &fakeAnomalyStore{
		history: []store.ToolCallRecord{okCall("write_file")},
	}
	r := NewAnomalyReactor(st)
	_ = r.Run(completedEv("t-1"))
	_ = r.Run(completedEv("t-2"))
	evs := cap.snapshot()
	if len(evs) != 2 {
		t.Fatalf("不同任务各自独立审计，应报 2 次，got %d", len(evs))
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.TaskID] = true
	}
	if !seen["t-1"] || !seen["t-2"] {
		t.Errorf("两条告警应分属 t-1/t-2: %v", seen)
	}
}

// ---------- 不直接改任务状态（Reactor 原则 4）----------

// TestAnomalyReactor_DoesNotMutateTaskState 用真实 MemoryTaskStore 验证：
// Reactor 跑完后任务的 Status / Results / Artifacts 原封不动——它对 Store
// 只有读操作（anomalyStoreView 结构上就不暴露写方法）。
func TestAnomalyReactor_DoesNotMutateTaskState(t *testing.T) {
	cap := installCapture(t)
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "anomaly no-mutate"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := taskStore.AppendToolCall(task.ID, store.ToolCallRecord{ToolName: "write_file", Success: true}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	r := NewAnomalyReactor(taskStore)
	if err := r.Run(completedEv(task.ID)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 异常确实命中（告警已发出）
	if evs := cap.snapshot(); len(evs) != 1 {
		t.Fatalf("前置条件：应命中 1 条异常告警，got %d", len(evs))
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

// ---------- 跨包握手端到端（trace.Emit → Registry.Dispatch → 异步 Reactor → KindError 告警）----------

// TestAnomalyReactor_E2E_DispatchToWarning 走真实派发链路：
// 全部用生产组件（MemoryTaskStore / reactor.Registry / trace.SetDefaultDispatcher），
// emit 一条 KindTaskCompleted，断言异常最终物化为一条 KindError 告警事件——这是
// "装配漏接"级别的验证：注册、订阅、异步派发、告警 emit 任何一环断掉本测试都会失败。
func TestAnomalyReactor_E2E_DispatchToWarning(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "anomaly e2e"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := taskStore.AppendToolCall(task.ID, store.ToolCallRecord{ToolName: "write_file", Success: true}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	cap := &captureReactor{}
	reg := reactor.NewRegistry()
	if err := reg.Register(NewAnomalyReactor(taskStore)); err != nil {
		t.Fatalf("Register anomaly: %v", err)
	}
	if err := reg.Register(cap); err != nil {
		t.Fatalf("Register capture: %v", err)
	}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })

	// 模拟主流程：任务完成
	trace.Emit(completedEv(task.ID))

	// Async Reactor：轮询等告警事件落到捕获器
	deadline := time.After(2 * time.Second)
	for {
		evs := cap.snapshot()
		if len(evs) == 1 {
			ev := evs[0]
			if ev.TaskID != task.ID || !strings.Contains(ev.Error, anomalyCodeFabricatedWrite) {
				t.Errorf("告警事件字段不对: %+v", ev)
			}
			return // 通过：emit → dispatch → reactor → 告警 全链路物化
		}
		if len(evs) > 1 {
			t.Fatalf("应只有 1 条告警，got %d", len(evs))
		}
		select {
		case <-deadline:
			t.Fatalf("2s 内未等到 KindError 告警（已捕获 %d 条）", len(evs))
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}
