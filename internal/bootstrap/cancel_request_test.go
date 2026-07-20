package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/plan"
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

// legacy 路径：有 Coordinator 但无 Plan 记录（immediate 模式的真实形态）——
// 不终止任何 Plan，scheduler 任务 + SchedulerBatch 子任务整树取消，来源记
// "user"。注意子任务沿 ParentTaskID 谱系继承根任务的 PlanID（PublishTask
// 会把 __scheduler__ 任务的 PlanID 自动指派为自身 ID），归组取消同样覆盖。
func TestCancelLatestActiveRequest_LegacyTree(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil) // 无 Plan 记录
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

	summary, err := cancelLatestActiveRequest(context.Background(), s, coord)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if !strings.Contains(summary, "…」") {
		t.Fatalf("长描述应被截断并带省略号: %q", summary)
	}
	if !strings.Contains(summary, "共取消 3 个任务") {
		t.Fatalf("摘要应含取消任务数: %q", summary)
	}
	if strings.Contains(summary, "终止 Plan") {
		t.Fatalf("legacy 路径不应提及 Plan: %q", summary)
	}
	for _, id := range []string{"req-legacy", "child-1", "child-2"} {
		mustStatus(t, s, id, model.TaskStatusCancelled)
		if src := reg.Source(id); src != "user" {
			t.Fatalf("任务 %s 取消来源 = %q, want user", id, src)
		}
	}
	mustStatus(t, s, "unrelated", model.TaskStatusPending)
}

// 动态 DAG 路径：TerminatePlan 终止 Plan（审计含 esc-cancel），controller 与
// 节点任务全部终态。
func TestCancelLatestActiveRequest_PlanTree(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coord.Create(context.Background(), plan.CreateInput{PlanID: "plan-abc", RootTaskID: "req-plan"}); err != nil {
		t.Fatalf("Create Plan: %v", err)
	}
	publishCancelable(t, s, reg, &model.Task{
		ID: "req-plan", Description: "新功能开发", EventType: "__scheduler__",
		PlanID: "plan-abc", NodeRole: model.PlanNodeRoleController,
	})
	publishCancelable(t, s, reg, &model.Task{ID: "node-1", Description: "节点一", EventType: "work", PlanID: "plan-abc"})
	publishCancelable(t, s, reg, &model.Task{ID: "node-2", Description: "节点二", EventType: "work", PlanID: "plan-abc"})
	// controller 置为 processing，覆盖两段式的 processing→cancelled 支路
	if err := s.ClaimTask("scheduler", "req-plan"); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// 其他 Plan 的任务不应被波及
	publishCancelable(t, s, reg, &model.Task{ID: "other-node", Description: "其他 Plan", EventType: "work", PlanID: "plan-xyz"})

	summary, err := cancelLatestActiveRequest(context.Background(), s, coord)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if !strings.Contains(summary, "终止 Plan plan-abc") || !strings.Contains(summary, "共取消 3 个任务") {
		t.Fatalf("摘要不符合预期: %q", summary)
	}
	p, err := coord.Store().GetPlan("plan-abc")
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if p.Status != model.PlanStatusCancelledByUser {
		t.Fatalf("Plan 状态 = %s, want cancelled_by_user", p.Status)
	}
	if len(p.Overrides) != 1 {
		t.Fatalf("Overrides = %d 条, want 1", len(p.Overrides))
	}
	ov := p.Overrides[0]
	if ov.Resolution != plan.PauseResolutionTerminate || ov.AuthorizedBy != "user" || ov.Reason != "esc-cancel" {
		t.Fatalf("终止审计字段不符合预期: %+v", ov)
	}
	if ov.AddedTasks != 0 || ov.AddedTokens != 0 || ov.AddedTime != 0 {
		t.Fatalf("终止不应施加预算增量: %+v", ov)
	}
	for _, id := range []string{"req-plan", "node-1", "node-2"} {
		mustStatus(t, s, id, model.TaskStatusCancelled)
		if src := reg.Source(id); src != "user" {
			t.Fatalf("任务 %s 取消来源 = %q, want user", id, src)
		}
	}
	mustStatus(t, s, "other-node", model.TaskStatusPending)
}

// coord 缺失（nil）时 Plan 路径降级：按 PlanID 归属直接取消任务，无 Plan 审计。
func TestCancelLatestActiveRequest_PlanTreeWithoutCoordinator(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	publishCancelable(t, s, reg, &model.Task{
		ID: "req-plan", Description: "新功能开发", EventType: "__scheduler__", PlanID: "plan-abc",
	})
	publishCancelable(t, s, reg, &model.Task{ID: "node-1", Description: "节点一", EventType: "work", PlanID: "plan-abc"})

	summary, err := cancelLatestActiveRequest(context.Background(), s, nil)
	if err != nil {
		t.Fatalf("cancelLatestActiveRequest: %v", err)
	}
	if strings.Contains(summary, "终止 Plan") || !strings.Contains(summary, "共取消 2 个任务") {
		t.Fatalf("摘要不符合预期: %q", summary)
	}
	mustStatus(t, s, "req-plan", model.TaskStatusCancelled)
	mustStatus(t, s, "node-1", model.TaskStatusCancelled)
}

// 栈语义：两棵请求树时先取消最新创建的一棵；重复调用落到下一棵，最终
// 返回 ErrNoActiveRequest（幂等）。
func TestCancelLatestActiveRequest_StackOrderAndIdempotent(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	// 先发布旧请求再发布新请求；CreatedAt 打平时由 ID 字典序兜底（req-bbb > req-aaa）。
	publishCancelable(t, s, reg, &model.Task{ID: "req-aaa", Description: "旧请求", EventType: "__scheduler__"})
	publishCancelable(t, s, reg, &model.Task{ID: "req-bbb", Description: "新请求", EventType: "__scheduler__"})

	summary, err := cancelLatestActiveRequest(context.Background(), s, nil)
	if err != nil {
		t.Fatalf("第一次取消: %v", err)
	}
	if !strings.Contains(summary, "新请求") {
		t.Fatalf("应先取消最新请求: %q", summary)
	}
	mustStatus(t, s, "req-bbb", model.TaskStatusCancelled)
	mustStatus(t, s, "req-aaa", model.TaskStatusPending)

	summary, err = cancelLatestActiveRequest(context.Background(), s, nil)
	if err != nil {
		t.Fatalf("第二次取消: %v", err)
	}
	if !strings.Contains(summary, "旧请求") {
		t.Fatalf("第二次应落到旧请求: %q", summary)
	}
	mustStatus(t, s, "req-aaa", model.TaskStatusCancelled)

	if _, err = cancelLatestActiveRequest(context.Background(), s, nil); !errors.Is(err, ui.ErrNoActiveRequest) {
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

	summary, err := cancelLatestActiveRequest(context.Background(), s, nil)
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
	if _, err := cancelLatestActiveRequest(context.Background(), s, nil); !errors.Is(err, ui.ErrNoActiveRequest) {
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
	if _, err := cancelLatestActiveRequest(context.Background(), s, nil); !errors.Is(err, ui.ErrNoActiveRequest) {
		t.Fatalf("err = %v, want ErrNoActiveRequest", err)
	}
	if got := ui.ErrNoActiveRequest.Error(); !strings.Contains(got, "当前没有正在运行的请求") {
		t.Fatalf("ErrNoActiveRequest 消息应为中文提示: %q", got)
	}
	mustStatus(t, s, "worker-task", model.TaskStatusPending)
}

// 栈序合并：等待批准的 Plan 与非终态根任务并存时，按创建时间取最新的一棵
// （CreatedAt 打平由 ID 字典序兜底）；重复调用按栈序落到下一棵，最终返回
// ErrNoActiveRequest。
func TestCancelLatestActiveRequest_StackOrderRootVsReviewPlan(t *testing.T) {
	// 子用例一：Plan 先创建（较旧）、根任务后发布（较新）——先取消根任务。
	// 时间打平时 "req-zzz" > "aaaa…" 的字典序兜底同样选中根任务，结果确定。
	t.Run("根任务更新时先取消根任务", func(t *testing.T) {
		s, reg := newCancelRequestFixture(t)
		coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
		p := publishReviewPlan(t, s, coord, "aaaa0000-0000-0000-0000-0000000000a1", "计划A")
		if err := s.ClaimTask("scheduler-1", p.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.SuspendTaskExecution("scheduler-1", p.ID, "plan suspended", nil); err != nil {
			t.Fatal(err)
		}
		publishCancelable(t, s, reg, &model.Task{ID: "req-zzz", Description: "新请求", EventType: "__scheduler__"})

		summary, err := cancelLatestActiveRequest(context.Background(), s, coord)
		if err != nil {
			t.Fatalf("第一次取消: %v", err)
		}
		if !strings.Contains(summary, "已取消请求「新请求」") {
			t.Fatalf("应先取消更新的根任务: %q", summary)
		}
		mustStatus(t, s, "req-zzz", model.TaskStatusCancelled)
		if got, _ := coord.Store().GetPlan(p.ID); got.Status != model.PlanStatusPausedAwaitingDecision {
			t.Fatalf("第一次取消不应触碰较旧的 Plan: %s", got.Status)
		}

		summary, err = cancelLatestActiveRequest(context.Background(), s, coord)
		if err != nil {
			t.Fatalf("第二次取消: %v", err)
		}
		if !strings.Contains(summary, "已取消等待批准的计划") {
			t.Fatalf("第二次应落到等待批准的 Plan: %q", summary)
		}
		if got, _ := coord.Store().GetPlan(p.ID); got.Status != model.PlanStatusCancelledByUser {
			t.Fatalf("Plan 状态 = %s, want cancelled_by_user", got.Status)
		}

		if _, err := cancelLatestActiveRequest(context.Background(), s, coord); !errors.Is(err, ui.ErrNoActiveRequest) {
			t.Fatalf("第三次 err = %v, want ErrNoActiveRequest", err)
		}
	})

	// 子用例二：根任务先发布（较旧）、Plan 后挂起（较新）——先取消 Plan。
	// 时间打平时 "zzzz…" > "req-aaa" 的字典序兜底同样选中 Plan，结果确定。
	t.Run("等待批准的Plan更新时先取消Plan", func(t *testing.T) {
		s, reg := newCancelRequestFixture(t)
		coord := plan.NewCoordinator(plan.NewMemoryStore(), nil)
		publishCancelable(t, s, reg, &model.Task{ID: "req-aaa", Description: "旧请求", EventType: "__scheduler__"})
		p := publishReviewPlan(t, s, coord, "zzzz0000-0000-0000-0000-0000000000b1", "计划B")
		if err := s.ClaimTask("scheduler-1", p.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.SuspendTaskExecution("scheduler-1", p.ID, "plan suspended", nil); err != nil {
			t.Fatal(err)
		}

		summary, err := cancelLatestActiveRequest(context.Background(), s, coord)
		if err != nil {
			t.Fatalf("第一次取消: %v", err)
		}
		if !strings.Contains(summary, "已取消等待批准的计划") {
			t.Fatalf("应先取消更新的待批准 Plan: %q", summary)
		}
		if got, _ := coord.Store().GetPlan(p.ID); got.Status != model.PlanStatusCancelledByUser {
			t.Fatalf("Plan 状态 = %s, want cancelled_by_user", got.Status)
		}
		mustStatus(t, s, "req-aaa", model.TaskStatusPending)

		summary, err = cancelLatestActiveRequest(context.Background(), s, coord)
		if err != nil {
			t.Fatalf("第二次取消: %v", err)
		}
		if !strings.Contains(summary, "已取消请求「旧请求」") {
			t.Fatalf("第二次应落到较旧的根任务: %q", summary)
		}
		mustStatus(t, s, "req-aaa", model.TaskStatusCancelled)

		if _, err := cancelLatestActiveRequest(context.Background(), s, coord); !errors.Is(err, ui.ErrNoActiveRequest) {
			t.Fatalf("第三次 err = %v, want ErrNoActiveRequest", err)
		}
	})
}

// coord 缺失（nil）时等待批准 Plan 候选源整体跳过：只有 blocked（终态）
// controller 的公告板返回 ErrNoActiveRequest，不 panic。
func TestCancelLatestActiveRequest_ReviewPlanNilCoordinator(t *testing.T) {
	s, reg := newCancelRequestFixture(t)
	publishCancelable(t, s, reg, &model.Task{ID: "req-blocked", Description: "挂起的请求", EventType: "__scheduler__"})
	if err := s.ClaimTask("scheduler-1", "req-blocked"); err != nil {
		t.Fatal(err)
	}
	if err := s.SuspendTaskExecution("scheduler-1", "req-blocked", "plan suspended", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := cancelLatestActiveRequest(context.Background(), s, nil); !errors.Is(err, ui.ErrNoActiveRequest) {
		t.Fatalf("err = %v, want ErrNoActiveRequest（coord=nil 无候选源）", err)
	}
	mustStatus(t, s, "req-blocked", model.TaskStatusBlocked)
}
