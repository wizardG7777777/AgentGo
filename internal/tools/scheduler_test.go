package tools

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/model"
)

// ---- Register 注册 ----

func TestSchedulerGroup_Register_BothTools(t *testing.T) {
	reg := agent.NewToolRegistry()
	SchedulerGroup{
		Store:  newFakeStore(),
		Holder: &fakeHolder{id: "sched-1"},
	}.Register(reg)
	defs := reg.Defs()
	if len(defs) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["cancel_task"] || !names["report_done"] {
		t.Errorf("expected cancel_task + report_done, got %v", names)
	}
}

func TestSchedulerGroup_Register_NoStoreSkipsAll(t *testing.T) {
	reg := agent.NewToolRegistry()
	SchedulerGroup{}.Register(reg)
	if got := len(reg.Defs()); got != 0 {
		t.Errorf("expected 0 tools when Store=nil, got %d", got)
	}
}

func TestSchedulerGroup_Register_NoHolderSkipsReportDone(t *testing.T) {
	reg := agent.NewToolRegistry()
	SchedulerGroup{Store: newFakeStore()}.Register(reg)
	defs := reg.Defs()
	if len(defs) != 1 || defs[0].Name != "cancel_task" {
		t.Errorf("expected only cancel_task without Holder, got %v", defs)
	}
}

// ---- cancel_task ----

func TestSchedulerGroup_CancelTask_PendingTask(t *testing.T) {
	s := newFakeStore()
	// fakeStore.PublishTask 把 status 置为 pending
	parent := &model.Task{Description: "to cancel"}
	s.PublishTask(parent)

	g := SchedulerGroup{Store: s, Holder: &fakeHolder{id: "sched"}}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	out, err := reg.Dispatch(context.Background(), mkCall("cancel_task", map[string]any{
		"task_id": parent.ID,
		"reason":  "用户取消",
	}))
	if err != nil {
		t.Fatalf("cancel_task failed: %v", err)
	}
	if !strings.Contains(out, parent.ID) || !strings.Contains(out, "用户取消") {
		t.Errorf("output should mention task ID and reason: %q", out)
	}
}

func TestSchedulerGroup_CancelTask_MissingTaskID(t *testing.T) {
	s := newFakeStore()
	g := SchedulerGroup{Store: s, Holder: &fakeHolder{id: "sched"}}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("cancel_task", map[string]any{}))
	if err == nil || !strings.Contains(err.Error(), "task_id") {
		t.Errorf("expected missing task_id error, got %v", err)
	}
}

// ---- report_done ----

// schedTestStore extends fakeStore with realistic SchedulerBatch state for report_done tests.
// fakeStore's PublishTask doesn't track SchedulerBatch automatically; we manually set it.
type schedTestStore struct {
	*fakeStore
}

func newSchedTestStore() *schedTestStore { return &schedTestStore{fakeStore: newFakeStore()} }

func (s *schedTestStore) AppendSchedulerBatch(taskID, childTaskID string) error {
	t, ok := s.tasks[taskID]
	if !ok {
		return nil
	}
	for _, existing := range t.SchedulerBatch {
		if existing == childTaskID {
			return nil
		}
	}
	t.SchedulerBatch = append(t.SchedulerBatch, childTaskID)
	return nil
}

func (s *schedTestStore) ClearSchedulerBatch(taskID string) error {
	t, ok := s.tasks[taskID]
	if !ok {
		return nil
	}
	t.SchedulerBatch = nil
	return nil
}

func TestSchedulerGroup_ReportDone_BatchPendingRejected(t *testing.T) {
	s := newSchedTestStore()
	// scheduler 自身的 task
	schedTask := &model.Task{Description: "user request"}
	s.PublishTask(schedTask)
	// 一个 pending 的子任务
	child := &model.Task{Description: "child", Status: model.TaskStatusProcessing}
	s.tasks["child-1"] = child
	child.ID = "child-1"
	s.AppendSchedulerBatch(schedTask.ID, "child-1")

	g := SchedulerGroup{Store: s, Holder: &fakeHolder{id: schedTask.ID}}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("report_done", map[string]any{
		"summary": "本不该被允许的汇报",
	}))
	if err == nil {
		t.Fatal("expected report_done to be rejected when batch has pending tasks")
	}
	if !strings.Contains(err.Error(), "尚未完成") {
		t.Errorf("error should mention pending tasks: %v", err)
	}
}

func TestSchedulerGroup_ReportDone_AllTerminalSuccess(t *testing.T) {
	s := newSchedTestStore()
	schedTask := &model.Task{Description: "user request"}
	s.PublishTask(schedTask)
	// 一个已完成的子任务，含 artifacts
	child := &model.Task{
		ID:        "child-completed",
		Status:    model.TaskStatusCompleted,
		Artifacts: []string{"docs/foo.md", "docs/bar.md"},
	}
	s.tasks["child-completed"] = child
	s.AppendSchedulerBatch(schedTask.ID, "child-completed")

	g := SchedulerGroup{Store: s, Holder: &fakeHolder{id: schedTask.ID}}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	out, err := reg.Dispatch(context.Background(), mkCall("report_done", map[string]any{
		"summary": "所有任务已完成",
	}))
	if err != nil {
		t.Fatalf("report_done failed: %v", err)
	}
	if !strings.Contains(out, "已向用户报告完成") {
		t.Errorf("expected success acknowledgment, got %q", out)
	}
}

func TestSchedulerGroup_ReportDone_NoHolderError(t *testing.T) {
	s := newSchedTestStore()
	// holder 返回空字符串
	g := SchedulerGroup{Store: s, Holder: &fakeHolder{id: ""}}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("report_done", map[string]any{
		"summary": "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "无法获取当前 scheduler") {
		t.Errorf("expected holder error, got %v", err)
	}
}

func TestSchedulerGroup_ReportDone_ClearsBatch(t *testing.T) {
	s := newSchedTestStore()
	schedTask := &model.Task{Description: "user request"}
	s.PublishTask(schedTask)
	// 一个 completed 子任务
	child := &model.Task{ID: "c1", Status: model.TaskStatusCompleted}
	s.tasks["c1"] = child
	s.AppendSchedulerBatch(schedTask.ID, "c1")

	if len(s.tasks[schedTask.ID].SchedulerBatch) != 1 {
		t.Fatalf("setup: SchedulerBatch should be 1, got %d", len(s.tasks[schedTask.ID].SchedulerBatch))
	}

	g := SchedulerGroup{Store: s, Holder: &fakeHolder{id: schedTask.ID}}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("report_done", map[string]any{
		"summary": "done",
	}))
	if err != nil {
		t.Fatalf("report_done: %v", err)
	}

	if len(s.tasks[schedTask.ID].SchedulerBatch) != 0 {
		t.Errorf("SchedulerBatch should be cleared after report_done, got %v", s.tasks[schedTask.ID].SchedulerBatch)
	}
}

// ---- buildSchedulerArtifactsReport（事实校对块） ----

func TestBuildSchedulerArtifactsReport_HasArtifacts(t *testing.T) {
	s := newFakeStore()
	taskA := &model.Task{
		ID:        "task-a-id",
		Status:    model.TaskStatusCompleted,
		Artifacts: []string{"docs/foo.md", "docs/bar.md"},
	}
	taskB := &model.Task{
		ID:     "task-b-id",
		Status: model.TaskStatusCompleted,
	}
	s.tasks[taskA.ID] = taskA
	s.tasks[taskB.ID] = taskB

	report := buildSchedulerArtifactsReport(s, []string{taskA.ID, taskB.ID})

	if !strings.Contains(report, "实际产出（系统校验") {
		t.Errorf("missing header: %s", report)
	}
	if !strings.Contains(report, "docs/foo.md") || !strings.Contains(report, "docs/bar.md") {
		t.Errorf("missing artifacts: %s", report)
	}
	// 任务 B 必须显示"无文件产出"
	if !strings.Contains(report, "无文件产出") {
		t.Errorf("missing no-output marker: %s", report)
	}
}

func TestBuildSchedulerArtifactsReport_GetTaskFailureTolerated(t *testing.T) {
	s := newFakeStore()
	report := buildSchedulerArtifactsReport(s, []string{"nonexistent-task"})
	if !strings.Contains(report, "读取失败") {
		t.Errorf("expected 读取失败 marker, got: %s", report)
	}
}

func TestBuildSchedulerArtifactsReport_EmptyBatch(t *testing.T) {
	s := newFakeStore()
	if got := buildSchedulerArtifactsReport(s, nil); got != "" {
		t.Errorf("nil batch should produce empty string, got %q", got)
	}
	if got := buildSchedulerArtifactsReport(s, []string{}); got != "" {
		t.Errorf("empty batch should produce empty string, got %q", got)
	}
}
