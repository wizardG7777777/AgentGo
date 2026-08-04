package userdef

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// newReplanWakeStore 返回 request_replan 测试用的真实公告板
// （*store.MemoryTaskStore 天然满足 replanWakeStore 接口）。
func newReplanWakeStore() *store.MemoryTaskStore {
	return store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
}

// wakeTasks 返回公告板中全部 replan 唤醒任务（EventType == "__scheduler__" 且
// EventSource == "replan-request"）。
func wakeTasks(t *testing.T, st *store.MemoryTaskStore) []*model.Task {
	t.Helper()
	tasks, err := st.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	var out []*model.Task
	for _, task := range tasks {
		if task.EventType == requestReplanWakeEventType && task.EventSource == "replan-request" {
			out = append(out, task)
		}
	}
	return out
}

// traceCapture 同步捕获指定种类的 trace 事件（同步 → 在 trace.Emit 调用方
// goroutine 内执行，断言无需等待）。
type traceCapture struct {
	mu     sync.Mutex
	kinds  []trace.EventKind
	events []trace.Event
}

func (c *traceCapture) Name() string                 { return "trace-capture" }
func (c *traceCapture) Subscribe() []trace.EventKind { return c.kinds }
func (c *traceCapture) IsSync() bool                 { return true }
func (c *traceCapture) Priority() int                { return 500 }
func (c *traceCapture) Run(ev trace.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *traceCapture) snapshot() []trace.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]trace.Event(nil), c.events...)
}

// installTraceCapture 注册捕获 Reactor 并设为全局 dispatcher。
func installTraceCapture(t *testing.T, kinds ...trace.EventKind) *traceCapture {
	t.Helper()
	cap := &traceCapture{kinds: kinds}
	reg := reactor.NewRegistry()
	if err := reg.Register(cap); err != nil {
		t.Fatalf("Register capture: %v", err)
	}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })
	return cap
}

func loadRequestReplan(t *testing.T, yamlData string, deps Deps) reactor.Reactor {
	t.Helper()
	rs, err := Load([]byte(yamlData), "", "", deps)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("len=%d want 1", len(rs))
	}
	return rs[0]
}

const requestReplanYAML = `
reactors:
  - name: replan-on-failure
    on: task_failed
    request_replan:
      reason_code: terminal_task_failed
      urgency: high
      detail: 'literal ${event.task.id}'
`

func TestRequestReplan_Metadata(t *testing.T) {
	r := loadRequestReplan(t, requestReplanYAML, Deps{Store: newReplanWakeStore()})
	if r.Name() != "replan-on-failure" {
		t.Fatalf("Name=%q", r.Name())
	}
	if r.IsSync() {
		t.Fatal("request_replan user reactor must be async")
	}
	if r.Priority() != 500 {
		t.Fatalf("Priority=%d want 500", r.Priority())
	}
	if got := r.Subscribe(); len(got) != 1 || got[0] != trace.KindTaskFailed {
		t.Fatalf("Subscribe=%v want [task_failed]", got)
	}
}

func TestRequestReplan_PublishesWakeTask(t *testing.T) {
	cap := installTraceCapture(t, trace.KindTaskPublished)
	st := newReplanWakeStore()
	r := loadRequestReplan(t, requestReplanYAML, Deps{Store: st})

	ev := trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1", AgentID: "worker-2"}
	if err := r.Run(ev); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 发布通用 replan 唤醒任务
	wakes := wakeTasks(t, st)
	if len(wakes) != 1 {
		t.Fatalf("应发布 1 个唤醒任务，got %d", len(wakes))
	}
	wake := wakes[0]
	firstLine, _, _ := strings.Cut(wake.Description, "\n")
	if firstLine != "[replan-request: task-1/replan]" {
		t.Errorf("描述首行应为幂等标记，got %q", firstLine)
	}
	if !strings.Contains(wake.Description, "reason_code=terminal_task_failed") ||
		!strings.Contains(wake.Description, "urgency=high") {
		t.Errorf("描述应含 reason_code/urgency: %q", wake.Description)
	}
	if wake.ParentTaskID != "task-1" {
		t.Errorf("ParentTaskID = %q, want task-1", wake.ParentTaskID)
	}
	if wake.MaxConcurrency != 1 {
		t.Errorf("MaxConcurrency = %d, want 1", wake.MaxConcurrency)
	}
	// 唤醒任务刻意不带图身份，避免被 graph-terminal-feed 回填
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务不应带图身份: %+v", wake)
	}

	// 审计事实即唤醒任务的 task_published 事件（C6c 起不再单独 emit
	// replan 事件）；detail 不做模板渲染。
	evs := cap.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应 emit 1 条 task_published，got %d", len(evs))
	}
	if evs[0].TaskID != wake.ID {
		t.Errorf("task_published 应属于唤醒任务，got %+v", evs[0])
	}
	if !strings.Contains(evs[0].Description, "reason_code=terminal_task_failed") {
		t.Errorf("task_published 描述应透传 reason_code: %q", evs[0].Description)
	}
	if !strings.Contains(evs[0].Description, `literal ${event.task.id}`) {
		t.Errorf("detail 不应被模板渲染，got %q", evs[0].Description)
	}
}

func TestRequestReplan_IdempotentWhileWakePending(t *testing.T) {
	st := newReplanWakeStore()
	r := loadRequestReplan(t, requestReplanYAML, Deps{Store: st})
	ev := trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1"}

	for i := 0; i < 3; i++ {
		if err := r.Run(ev); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}
	if wakes := wakeTasks(t, st); len(wakes) != 1 {
		t.Fatalf("同一标记的未终态唤醒任务存在时应幂等返回，got %d 个", len(wakes))
	}
}

func TestRequestReplan_RepublishesAfterWakeTerminal(t *testing.T) {
	st := newReplanWakeStore()
	r := loadRequestReplan(t, requestReplanYAML, Deps{Store: st})
	ev := trace.Event{Kind: trace.KindTaskFailed, TaskID: "task-1"}

	if err := r.Run(ev); err != nil {
		t.Fatalf("Run: %v", err)
	}
	wakes := wakeTasks(t, st)
	if len(wakes) != 1 {
		t.Fatalf("前置条件：应发布 1 个唤醒任务，got %d", len(wakes))
	}
	// 唤醒任务进入终态后，同一源任务再次请求应重新发布
	if err := st.TransitionState(wakes[0].ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatalf("TransitionState: %v", err)
	}
	if err := r.Run(ev); err != nil {
		t.Fatalf("Run after terminal: %v", err)
	}
	if wakes := wakeTasks(t, st); len(wakes) != 2 {
		t.Fatalf("原唤醒任务已终态，应再发布 1 个，got %d", len(wakes))
	}
}

func TestRequestReplan_GraphTaskSkipsPublish(t *testing.T) {
	cap := installTraceCapture(t, trace.KindError)
	st := newReplanWakeStore()
	graphTask := &model.Task{Description: "graph node", GraphID: "g-1", NodeID: "n-1"}
	if err := st.PublishTask(graphTask); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	r := loadRequestReplan(t, requestReplanYAML, Deps{Store: st})

	if err := r.Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: graphTask.ID}); err != nil {
		t.Fatalf("图任务应静默跳过（不报错），got %v", err)
	}
	if wakes := wakeTasks(t, st); len(wakes) != 0 {
		t.Fatalf("图任务不应发布通用唤醒任务，got %d", len(wakes))
	}
	evs := cap.snapshot()
	if len(evs) != 1 || evs[0].Kind != trace.KindError {
		t.Fatalf("图任务应仅 emit 1 条 KindError 提示，got %+v", evs)
	}
	if !strings.Contains(evs[0].Error, "g-1") || !strings.Contains(evs[0].Error, "replan-on-failure") {
		t.Errorf("提示应含图 ID 与 reactor 名: %q", evs[0].Error)
	}
}

func TestRequestReplan_WhenFiltersBeforePublish(t *testing.T) {
	cap := installTraceCapture(t, trace.KindTaskPublished)
	st := newReplanWakeStore()
	r := loadRequestReplan(t, `
reactors:
  - on: task_failed
    when: ${event.task.retry_count} >= 3
    request_replan:
      reason_code: retries_exhausted
      urgency: normal
`, Deps{Store: st})

	if err := r.Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T", AttemptNo: 2}); err != nil {
		t.Fatalf("Run when=false: %v", err)
	}
	if wakes := wakeTasks(t, st); len(wakes) != 0 {
		t.Fatalf("when=false 不应发布唤醒任务，got %d", len(wakes))
	}
	if evs := cap.snapshot(); len(evs) != 0 {
		t.Fatalf("when=false 不应发布任何任务，got %d", len(evs))
	}

	if err := r.Run(trace.Event{
		Kind:       trace.KindTaskFailed,
		TaskID:     "T",
		Transition: &trace.Transition{RetryCount: 3},
	}); err != nil {
		t.Fatalf("Run when=true: %v", err)
	}
	if wakes := wakeTasks(t, st); len(wakes) != 1 {
		t.Fatalf("when=true 应发布唤醒任务，got %d", len(wakes))
	}
}

// failingWakeStore 实现 replanWakeStore，可注入发布/扫描错误。
type failingWakeStore struct {
	publishErr error
	scanErr    error
}

func (f *failingWakeStore) PublishTask(*model.Task) error     { return f.publishErr }
func (f *failingWakeStore) ScanAll() ([]*model.Task, error)   { return nil, f.scanErr }
func (f *failingWakeStore) GetTask(string) (*model.Task, error) {
	return nil, errors.New("未找到")
}

func TestRequestReplan_StoreErrorPropagates(t *testing.T) {
	ev := trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"}

	r := loadRequestReplan(t, requestReplanYAML, Deps{Store: &failingWakeStore{scanErr: errors.New("scan 挂了")}})
	if err := r.Run(ev); err == nil || !strings.Contains(err.Error(), "scan 挂了") {
		t.Fatalf("ScanAll 错误应传播，got %v", err)
	} else if !strings.Contains(err.Error(), "request_replan[replan-on-failure]") {
		t.Fatalf("错误应标识 reactor，got %v", err)
	}

	r = loadRequestReplan(t, requestReplanYAML, Deps{Store: &failingWakeStore{publishErr: errors.New("publish 挂了")}})
	if err := r.Run(ev); err == nil || !strings.Contains(err.Error(), "publish 挂了") {
		t.Fatalf("PublishTask 错误应传播，got %v", err)
	}
}

func TestRequestReplan_LoaderValidation(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		deps    Deps
		wantErr string
	}{
		{
			name:    "missing reason code",
			action:  "urgency: normal",
			deps:    Deps{Store: newReplanWakeStore()},
			wantErr: "reason_code",
		},
		{
			name:    "missing urgency",
			action:  "reason_code: task_failed",
			deps:    Deps{Store: newReplanWakeStore()},
			wantErr: "urgency",
		},
		{
			name:    "invalid urgency",
			action:  "reason_code: task_failed\n      urgency: critical",
			deps:    Deps{Store: newReplanWakeStore()},
			wantErr: "normal/high",
		},
		{
			name:    "missing store dependency",
			action:  "reason_code: task_failed\n      urgency: normal",
			deps:    Deps{},
			wantErr: "Deps.Store",
		},
		{
			name:   "store lacks wake-store capabilities",
			action: "reason_code: task_failed\n      urgency: normal",
			// countingStore 只有 PublishTask，不满足 replanWakeStore
			deps:    Deps{Store: &countingStore{}},
			wantErr: "PublishTask/ScanAll/GetTask",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yamlData := []byte("reactors:\n" +
				"  - on: task_failed\n" +
				"    request_replan:\n" +
				"      " + tt.action + "\n")
			_, err := Load(yamlData, "", "", tt.deps)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error=%v want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRequestReplan_RejectsAuthorityFieldsFromYAML(t *testing.T) {
	forbiddenFields := []string{
		"plan_id",
		"observed_revision",
		"observed_state_version",
		"idempotency_key",
	}
	for _, field := range forbiddenFields {
		t.Run(field, func(t *testing.T) {
			yamlData := []byte("reactors:\n" +
				"  - on: task_failed\n" +
				"    request_replan:\n" +
				"      reason_code: task_failed\n" +
				"      urgency: high\n" +
				"      " + field + ": forged\n")
			_, err := Load(yamlData, "", "", Deps{Store: newReplanWakeStore()})
			if err == nil {
				t.Fatalf("Load accepted forbidden request_replan field %q", field)
			}
			if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), field) {
				t.Fatalf("Load error=%v should identify forbidden field %q", err, field)
			}
		})
	}
}

func TestRequestReplan_IsMutuallyExclusiveWithOtherActions(t *testing.T) {
	_, err := Load([]byte(`
reactors:
  - on: task_failed
    request_replan:
      reason_code: task_failed
      urgency: normal
    call: send_message
    args:
      to: scheduler-1
      content: duplicate control path
`), "", "", Deps{
		Store:   newReplanWakeStore(),
		Mailbox: &fakeMailbox{},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one action") {
		t.Fatalf("Load error=%v; request_replan must be mutually exclusive", err)
	}
}
