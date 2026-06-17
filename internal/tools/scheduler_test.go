package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/model"
)

// ---- Register 注册 ----

func TestSchedulerGroup_Register_AllTools(t *testing.T) {
	reg := agent.NewToolRegistry()
	SchedulerGroup{
		Store:  newFakeStore(),
		Holder: &fakeHolder{id: "sched-1"},
	}.Register(reg)
	defs := reg.Defs()
	if len(defs) != 4 {
		t.Fatalf("expected 4 tools (cancel_task + report_done + report_progress + probe_directory), got %d", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["cancel_task"] || !names["report_done"] || !names["report_progress"] || !names["probe_directory"] {
		t.Errorf("expected cancel_task + report_done + report_progress + probe_directory, got %v", names)
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
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if len(defs) != 2 || !names["cancel_task"] || !names["probe_directory"] {
		t.Errorf("expected cancel_task + probe_directory without Holder, got %v", names)
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

// fakeFinalizationNotifier 是单测用的 FinalizationNotifier 实现，记录 MarkTaskFinalized
// 是否被调用过，让测试断言 reportDone 真的触发了"终止 reactLoop"信号。
type fakeFinalizationNotifier struct {
	marked bool
}

func (f *fakeFinalizationNotifier) MarkTaskFinalized() { f.marked = true }

func TestSchedulerGroup_ReportDone_NotifiesDoneOnSuccess(t *testing.T) {
	s := newSchedTestStore()
	schedTask := &model.Task{Description: "user request"}
	s.PublishTask(schedTask)
	// 一个 completed 子任务，让 report_done 通过硬拦截
	child := &model.Task{ID: "c1", Status: model.TaskStatusCompleted}
	s.tasks["c1"] = child
	s.AppendSchedulerBatch(schedTask.ID, "c1")

	notifier := &fakeFinalizationNotifier{}
	g := SchedulerGroup{
		Store:                s,
		Holder:               &fakeHolder{id: schedTask.ID},
		FinalizationNotifier: notifier,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("report_done", map[string]any{
		"summary": "done",
	}))
	if err != nil {
		t.Fatalf("report_done: %v", err)
	}

	if !notifier.marked {
		t.Error("FinalizationNotifier.MarkTaskFinalized should be called after successful report_done")
	}
}

func TestSchedulerGroup_ReportDone_DoesNotNotifyOnRejection(t *testing.T) {
	s := newSchedTestStore()
	schedTask := &model.Task{Description: "user request"}
	s.PublishTask(schedTask)
	// 一个 pending 子任务 → report_done 应当被硬拦截
	child := &model.Task{ID: "c1", Status: model.TaskStatusPending}
	s.tasks["c1"] = child
	s.AppendSchedulerBatch(schedTask.ID, "c1")

	notifier := &fakeFinalizationNotifier{}
	g := SchedulerGroup{
		Store:                s,
		Holder:               &fakeHolder{id: schedTask.ID},
		FinalizationNotifier: notifier,
	}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("report_done", map[string]any{
		"summary": "premature",
	}))
	if err == nil {
		t.Fatal("expected report_done to be rejected (batch has pending child)")
	}

	if notifier.marked {
		t.Error("FinalizationNotifier.MarkTaskFinalized should NOT be called when report_done is rejected")
	}
}

func TestSchedulerGroup_ReportDone_NilNotifierNoEffect(t *testing.T) {
	s := newSchedTestStore()
	schedTask := &model.Task{Description: "user request"}
	s.PublishTask(schedTask)

	// 不设置 FinalizationNotifier
	g := SchedulerGroup{Store: s, Holder: &fakeHolder{id: schedTask.ID}}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	// 不应 panic
	_, err := reg.Dispatch(context.Background(), mkCall("report_done", map[string]any{
		"summary": "done",
	}))
	if err != nil {
		t.Fatalf("report_done with nil notifier should not error: %v", err)
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

// ---- probe_directory ----

func TestProbeDirectory_BasicTree(t *testing.T) {
	// 创建临时目录结构
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "src"), 0o755)
	os.WriteFile(filepath.Join(tmp, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(tmp, "src", "app.go"), []byte("package src\n// app"), 0o644)
	os.WriteFile(filepath.Join(tmp, "README.md"), []byte("# readme"), 0o644)

	g := SchedulerGroup{Store: newFakeStore(), ProjectRoot: tmp}
	out, err := g.probeDirectory(context.Background(), map[string]any{"path": ".", "depth": 3.0})
	if err != nil {
		t.Fatalf("probeDirectory: %v", err)
	}

	// 综述行
	if !strings.Contains(out, "[综述]") {
		t.Errorf("missing [综述] header: %s", out)
	}
	if !strings.Contains(out, "文件: 3") {
		t.Errorf("expected 3 files in summary: %s", out)
	}
	if !strings.Contains(out, "文件夹: 1") {
		t.Errorf("expected 1 dir in summary: %s", out)
	}

	// 类型分布
	if !strings.Contains(out, ".go") {
		t.Errorf("expected .go in type distribution: %s", out)
	}
	if !strings.Contains(out, ".md") {
		t.Errorf("expected .md in type distribution: %s", out)
	}

	// 树形输出含文件大小
	if !strings.Contains(out, "main.go") {
		t.Errorf("expected main.go in tree: %s", out)
	}
	if !strings.Contains(out, "B") && !strings.Contains(out, "KB") {
		t.Errorf("expected file size in tree: %s", out)
	}
}

func TestProbeDirectory_DepthControl(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "a", "b", "c"), 0o755)
	os.WriteFile(filepath.Join(tmp, "a", "b", "c", "deep.txt"), []byte("deep"), 0o644)

	g := SchedulerGroup{Store: newFakeStore(), ProjectRoot: tmp}

	// depth=1 只看顶层
	out1, _ := g.probeDirectory(context.Background(), map[string]any{"path": ".", "depth": 1.0})
	if strings.Contains(out1, "deep.txt") {
		t.Errorf("depth=1 should not show deep.txt: %s", out1)
	}
	if !strings.Contains(out1, "a/") {
		t.Errorf("depth=1 should show top-level dir 'a/': %s", out1)
	}

	// depth=5 看到底
	out5, _ := g.probeDirectory(context.Background(), map[string]any{"path": ".", "depth": 5.0})
	if !strings.Contains(out5, "deep.txt") {
		t.Errorf("depth=5 should show deep.txt: %s", out5)
	}
}

func TestProbeDirectory_SkipsHidden(t *testing.T) {
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, ".git"), 0o755)
	os.WriteFile(filepath.Join(tmp, ".gitignore"), []byte("*.o"), 0o644)
	os.WriteFile(filepath.Join(tmp, "visible.go"), []byte("package x"), 0o644)

	g := SchedulerGroup{Store: newFakeStore(), ProjectRoot: tmp}
	out, _ := g.probeDirectory(context.Background(), map[string]any{"path": "."})

	if strings.Contains(out, ".git") {
		t.Errorf("should skip hidden dirs/files: %s", out)
	}
	if !strings.Contains(out, "visible.go") {
		t.Errorf("should show visible files: %s", out)
	}
	// 综述中也不该计入隐藏文件
	if !strings.Contains(out, "文件: 1") {
		t.Errorf("expected only 1 visible file in summary: %s", out)
	}
}

func TestProbeDirectory_EmptyDir(t *testing.T) {
	tmp := t.TempDir()

	g := SchedulerGroup{Store: newFakeStore(), ProjectRoot: tmp}
	out, err := g.probeDirectory(context.Background(), map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("probeDirectory: %v", err)
	}
	if !strings.Contains(out, "文件: 0") {
		t.Errorf("expected 0 files in summary for empty dir: %s", out)
	}
	if !strings.Contains(out, "无文件") {
		t.Errorf("expected 无文件 in type distribution for empty dir: %s", out)
	}
}

func TestProbeDirectory_PathValidation(t *testing.T) {
	tmp := t.TempDir()
	g := SchedulerGroup{Store: newFakeStore(), ProjectRoot: tmp}

	_, err := g.probeDirectory(context.Background(), map[string]any{"path": "../../etc/passwd"})
	if err == nil {
		t.Fatal("expected path traversal error")
	}
}

func TestProbeDirectory_Stats(t *testing.T) {
	tmp := t.TempDir()
	// 6 个 .go，2 个 .md，1 个 .yaml
	for i := 0; i < 6; i++ {
		os.WriteFile(filepath.Join(tmp, fmt.Sprintf("f%d.go", i)), []byte("go"), 0o644)
	}
	os.WriteFile(filepath.Join(tmp, "a.md"), []byte("md"), 0o644)
	os.WriteFile(filepath.Join(tmp, "b.md"), []byte("md"), 0o644)
	os.WriteFile(filepath.Join(tmp, "c.yaml"), []byte("yaml"), 0o644)

	g := SchedulerGroup{Store: newFakeStore(), ProjectRoot: tmp}
	out, _ := g.probeDirectory(context.Background(), map[string]any{"path": "."})

	if !strings.Contains(out, "文件: 9") {
		t.Errorf("expected 9 files: %s", out)
	}
	// .go 应排第一（数量最多）
	distLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[类型分布]") {
			distLine = line
			break
		}
	}
	if distLine == "" {
		t.Fatalf("missing [类型分布] line: %s", out)
	}
	goIdx := strings.Index(distLine, ".go")
	mdIdx := strings.Index(distLine, ".md")
	if goIdx < 0 || mdIdx < 0 || goIdx > mdIdx {
		t.Errorf(".go should appear before .md (higher count): %s", distLine)
	}
}

// ---- formatSize ----

func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1572864, "1.5 MB"},
	}
	for _, c := range cases {
		got := formatSize(c.bytes)
		if got != c.want {
			t.Errorf("formatSize(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}
