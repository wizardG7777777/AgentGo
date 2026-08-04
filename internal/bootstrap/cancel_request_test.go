package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/ui"
)

// newCancelRequestFixture 构造带取消注册表的公告板；注册表用于断言结构化
// 取消来源（与 cancel_task_test.go 的用法一致）。
func newCancelRequestFixture(t *testing.T) (*store.MemoryTaskStore, *store.TaskCancelRegistry) {
	t.Helper()
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reg := store.NewTaskCancelRegistry()
	s.SetCancelRegistry(reg)
	return s, reg
}

// publishCancelable 发布任务并在取消注册表中登记，使取消来源可回溯。
func publishCancelable(t *testing.T, s store.TaskStore, reg *store.TaskCancelRegistry, task *model.Task) {
	t.Helper()
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask(%s): %v", task.ID, err)
	}
	reg.GetOrCreate(context.Background(), task.ID)
}

func mustStatus(t *testing.T, s store.TaskStore, taskID string, want model.TaskStatus) {
	t.Helper()
	got, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask(%s): %v", taskID, err)
	}
	if got.Status != want {
		t.Fatalf("任务 %s 状态 = %s, want %s", taskID, got.Status, want)
	}
}

// 请求树归组：非终态 __scheduler__ 根任务 + SchedulerBatch 子任务 + ParentTaskID
// 谱系后代整树取消，来源记 "user"；与请求树无关的任务不受波及。
func TestCancelLatestActiveRequest_RequestTree(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	publishCancelable(t, s, reg, &model.Task{
		ID: "req-legacy", Description: "帮我写一份非常详尽的月度经营分析报告并附上数据图表",
		EventType: "__scheduler__", SchedulerBatch: []string{"child-1", "child-2"},
	})
	publishCancelable(t, s, reg, &model.Task{ID: "child-1", Description: "子任务一", EventType: "work", ParentTaskID: "req-legacy"})
	publishCancelable(t, s, reg, &model.Task{ID: "child-2", Description: "子任务二", EventType: "work", ParentTaskID: "req-legacy"})
	// child-2 置为 processing，覆盖两段式的 processing→cancelled 支路
	if err := s.ClaimTask("worker-1", "child-2"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// 与请求树无关的任务不应被波及
	publishCancelable(t, s, reg, &model.Task{ID: "unrelated", Description: "无关任务", EventType: "work"})

	summary, err := cancelLatestActiveRequest(context.Background(), s)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if !strings.Contains(summary, "…」") {
		t.Fatalf("长描述应被截断并带省略号: %q", summary)
	}
	if !strings.Contains(summary, "共取消 3 个任务") {
		t.Fatalf("摘要应含取消任务数: %q", summary)
	}
	if strings.Contains(summary, "Plan") {
		t.Fatalf("C6b 后请求取消不应再提及 Plan: %q", summary)
	}
	for _, id := range []string{"req-legacy", "child-1", "child-2"} {
		mustStatus(t, s, id, model.TaskStatusCancelled)
		if src := reg.Source(id); src != "user" {
			t.Fatalf("任务 %s 取消来源 = %q, want user", id, src)
		}
	}
	mustStatus(t, s, "unrelated", model.TaskStatusPending)
}

// 谱系归组：SchedulerBatch 未显式跟踪、但经 ParentTaskID 多级挂到根任务的
// 后代（worker 自拆的子任务）同样被整树取消。
func TestCancelLatestActiveRequest_LineageDescendants(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	publishCancelable(t, s, reg, &model.Task{ID: "req-lineage", Description: "谱系测试", EventType: "__scheduler__"})
	publishCancelable(t, s, reg, &model.Task{ID: "mid", Description: "中间任务", EventType: "work", ParentTaskID: "req-lineage"})
	publishCancelable(t, s, reg, &model.Task{ID: "leaf", Description: "叶子任务", EventType: "work", ParentTaskID: "mid"})
	// 其他谱系的任务不应被波及
	publishCancelable(t, s, reg, &model.Task{ID: "other-root", Description: "其他根", EventType: "work"})
	publishCancelable(t, s, reg, &model.Task{ID: "other-child", Description: "其他子", EventType: "work", ParentTaskID: "other-root"})

	summary, err := cancelLatestActiveRequest(context.Background(), s)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if !strings.Contains(summary, "共取消 3 个任务") {
		t.Fatalf("摘要应含谱系归组后的取消总数: %q", summary)
	}
	for _, id := range []string{"req-lineage", "mid", "leaf"} {
		mustStatus(t, s, id, model.TaskStatusCancelled)
		if src := reg.Source(id); src != "user" {
			t.Fatalf("任务 %s 取消来源 = %q, want user", id, src)
		}
	}
	mustStatus(t, s, "other-root", model.TaskStatusPending)
	mustStatus(t, s, "other-child", model.TaskStatusPending)
}

// 栈语义：两棵请求树时先取消最新创建的一棵；重复调用落到下一棵，最终
// 返回 ErrNoActiveRequest（幂等）。
func TestCancelLatestActiveRequest_StackOrderAndIdempotent(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	// 先发布旧请求再发布新请求；CreatedAt 打平时由 ID 字典序兜底（req-bbb > req-aaa）。
	publishCancelable(t, s, reg, &model.Task{ID: "req-aaa", Description: "旧请求", EventType: "__scheduler__"})
	publishCancelable(t, s, reg, &model.Task{ID: "req-bbb", Description: "新请求", EventType: "__scheduler__"})

	summary, err := cancelLatestActiveRequest(context.Background(), s)
	if err != nil {
		t.Fatalf("第一次取消: %v", err)
	}
	if !strings.Contains(summary, "新请求") {
		t.Fatalf("应先取消最新请求: %q", summary)
	}
	mustStatus(t, s, "req-bbb", model.TaskStatusCancelled)
	mustStatus(t, s, "req-aaa", model.TaskStatusPending)

	summary, err = cancelLatestActiveRequest(context.Background(), s)
	if err != nil {
		t.Fatalf("第二次取消: %v", err)
	}
	if !strings.Contains(summary, "旧请求") {
		t.Fatalf("第二次应落到旧请求: %q", summary)
	}
	mustStatus(t, s, "req-aaa", model.TaskStatusCancelled)

	if _, err = cancelLatestActiveRequest(context.Background(), s); !errors.Is(err, ui.ErrNoActiveRequest) {
		t.Fatalf("第三次 err = %v, want ErrNoActiveRequest", err)
	}
}

// 级联：Dependencies 指向已取消任务的非终态任务一并取消（含多级链），
// 来源记 "dependency_failure"。
func TestCancelLatestActiveRequest_CascadeDependents(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	publishCancelable(t, s, reg, &model.Task{
		ID: "req-cascade", Description: "级联测试", EventType: "__scheduler__", SchedulerBatch: []string{"batch-1"},
	})
	publishCancelable(t, s, reg, &model.Task{ID: "batch-1", Description: "批内任务", EventType: "work", ParentTaskID: "req-cascade"})
	publishCancelable(t, s, reg, &model.Task{ID: "dep-1", Description: "下游一", EventType: "work", Dependencies: []string{"batch-1"}})
	publishCancelable(t, s, reg, &model.Task{ID: "dep-2", Description: "下游二", EventType: "work", Dependencies: []string{"dep-1"}})
	publishCancelable(t, s, reg, &model.Task{ID: "unrelated", Description: "无关任务", EventType: "work"})

	summary, err := cancelLatestActiveRequest(context.Background(), s)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if !strings.Contains(summary, "共取消 4 个任务") {
		t.Fatalf("摘要应含级联后的取消总数: %q", summary)
	}
	for _, id := range []string{"req-cascade", "batch-1", "dep-1", "dep-2"} {
		mustStatus(t, s, id, model.TaskStatusCancelled)
	}
	if src := reg.Source("dep-1"); src != "dependency_failure" {
		t.Fatalf("dep-1 取消来源 = %q, want dependency_failure", src)
	}
	if src := reg.Source("dep-2"); src != "dependency_failure" {
		t.Fatalf("dep-2 取消来源 = %q, want dependency_failure", src)
	}
	mustStatus(t, s, "unrelated", model.TaskStatusPending)
}

// 无活跃请求树（空公告板 / scheduler 任务已终态）返回 ErrNoActiveRequest。
func TestCancelLatestActiveRequest_NoActiveRequest(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	if _, err := cancelLatestActiveRequest(context.Background(), s); !errors.Is(err, ui.ErrNoActiveRequest) {
		t.Fatalf("空公告板 err = %v, want ErrNoActiveRequest", err)
	}

	publishCancelable(t, s, reg, &model.Task{ID: "req-done", Description: "已完成", EventType: "__scheduler__"})
	if err := s.ClaimTask("scheduler", "req-done"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if err := s.SubmitResult("scheduler", "req-done", "done"); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	publishCancelable(t, s, reg, &model.Task{ID: "worker-task", Description: "普通任务", EventType: "work"})
	if _, err := cancelLatestActiveRequest(context.Background(), s); !errors.Is(err, ui.ErrNoActiveRequest) {
		t.Fatalf("err = %v, want ErrNoActiveRequest", err)
	}
	if got := ui.ErrNoActiveRequest.Error(); !strings.Contains(got, "当前没有正在运行的请求") {
		t.Fatalf("ErrNoActiveRequest 消息应为中文提示: %q", got)
	}
	mustStatus(t, s, "worker-task", model.TaskStatusPending)
}

// blocked 是终态：只剩 blocked 根任务的公告板没有活跃请求树，返回
// ErrNoActiveRequest 且不触碰该任务（C6b 后归组只认非终态 __scheduler__ 根）。
func TestCancelLatestActiveRequest_BlockedRootNotCandidate(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	publishCancelable(t, s, reg, &model.Task{ID: "req-blocked", Description: "挂起的请求", EventType: "__scheduler__"})
	if err := s.ClaimTask("scheduler-1", "req-blocked"); err != nil {
		t.Fatal(err)
	}
	if err := store.TransitionStateWithCancelSource(s, "req-blocked", model.TaskStatusProcessing, model.TaskStatusBlocked, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := cancelLatestActiveRequest(context.Background(), s); !errors.Is(err, ui.ErrNoActiveRequest) {
		t.Fatalf("err = %v, want ErrNoActiveRequest（blocked 是终态，非活跃请求）", err)
	}
	mustStatus(t, s, "req-blocked", model.TaskStatusBlocked)
}
