package builtin

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/hook"
	"agentgo/internal/interaction"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

type fakeGraphLister struct{ summaries []graph.GraphSummary }

func (f *fakeGraphLister) List() []graph.GraphSummary { return f.summaries }

type fakeInteractionPeek struct{ pending int }

func (f *fakeInteractionPeek) ListPending(_ context.Context, _ string) ([]interaction.Request, error) {
	return make([]interaction.Request, f.pending), nil
}

func newClosureFixture(t *testing.T) (*SchedulerClosureHook, store.TaskStore) {
	t.Helper()
	ch := make(chan model.Event, 16)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	return NewSchedulerClosureHook(s), s
}

func publishSchedulerTask(t *testing.T, s store.TaskStore, id string) {
	t.Helper()
	if err := s.PublishTask(&model.Task{ID: id, Description: "用户请求", EventType: "__scheduler__"}); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
}

func closureCtx(taskID, summary string) hook.ToolHookContext {
	return hook.ToolHookContext{
		Ctx: context.Background(), Phase: hook.PhasePreCall,
		AgentID: "scheduler-1", TaskID: taskID, ToolName: "report_done",
		Args: map[string]any{"summary": summary},
	}
}

func TestSchedulerClosure_IgnoresNonSchedulerTasks(t *testing.T) {
	h, s := newClosureFixture(t)
	if err := s.PublishTask(&model.Task{ID: "t-w", Description: "worker 任务", EventType: "work"}); err != nil {
		t.Fatal(err)
	}
	// worker 任务即使出现在 report_done 调用点也不审查（它根本不该有这个工具）
	if d := h.Run(closureCtx("t-w", "")); d.Action != hook.Continue {
		t.Fatalf("非 scheduler 任务应放行, got %+v", d)
	}
	// 图内任务不审查（graph controller 用 submit_task_result 收口）
	if err := s.PublishTask(&model.Task{ID: "t-g", Description: "图节点", EventType: "__scheduler__", GraphID: "g1"}); err != nil {
		t.Fatal(err)
	}
	if d := h.Run(closureCtx("t-g", "")); d.Action != hook.Continue {
		t.Fatalf("图内任务应放行, got %+v", d)
	}
}

func TestSchedulerClosure_ZeroEvidenceEmptySummaryAborts(t *testing.T) {
	h, s := newClosureFixture(t)
	publishSchedulerTask(t, s, "t-sched")

	d := h.Run(closureCtx("t-sched", ""))
	if d.Action != hook.Abort {
		t.Fatalf("零证据+空 summary 应 Abort, got %+v", d)
	}
	if d.ReasonCode != "report_done_empty_summary" {
		t.Fatalf("ReasonCode = %q, want report_done_empty_summary", d.ReasonCode)
	}
	// 空 summary 没有确认豁免：第二次仍 Abort
	if d2 := h.Run(closureCtx("t-sched", "")); d2.Action != hook.Abort {
		t.Fatalf("空 summary 第二次调用仍应 Abort, got %+v", d2)
	}
}

func TestSchedulerClosure_ZeroEvidenceDirectAnswerNeedsConfirm(t *testing.T) {
	h, s := newClosureFixture(t)
	publishSchedulerTask(t, s, "t-sched")

	d1 := h.Run(closureCtx("t-sched", "现在是下午三点。"))
	if d1.Action != hook.Abort || d1.ReasonCode != "scheduler_zero_evidence_closure" {
		t.Fatalf("零证据直答第一次应 Abort 要求确认, got %+v", d1)
	}
	d2 := h.Run(closureCtx("t-sched", "现在是下午三点。"))
	if d2.Action != hook.Continue {
		t.Fatalf("确认后的直答应放行, got %+v", d2)
	}
}

func TestSchedulerClosure_EvidencePassesImmediately(t *testing.T) {
	t.Run("有图", func(t *testing.T) {
		h, s := newClosureFixture(t)
		publishSchedulerTask(t, s, "t-sched")
		h.SessionID = func() string { return "sess-1" }
		h.Graphs = &fakeGraphLister{summaries: []graph.GraphSummary{{GraphID: "g1", SessionID: "sess-1"}}}
		if d := h.Run(closureCtx("t-sched", "已建图执行")); d.Action != hook.Continue {
			t.Fatalf("有图应放行, got %+v", d)
		}
	})
	t.Run("图属于别的session不算", func(t *testing.T) {
		h, s := newClosureFixture(t)
		publishSchedulerTask(t, s, "t-sched")
		h.SessionID = func() string { return "sess-1" }
		h.Graphs = &fakeGraphLister{summaries: []graph.GraphSummary{{GraphID: "g1", SessionID: "sess-other"}}}
		if d := h.Run(closureCtx("t-sched", "答复")); d.Action != hook.Abort {
			t.Fatalf("其他 session 的图不算证据, got %+v", d)
		}
	})
	t.Run("有delegated任务", func(t *testing.T) {
		h, s := newClosureFixture(t)
		publishSchedulerTask(t, s, "t-sched")
		if err := s.PublishTask(&model.Task{ID: "t-worker", Description: "委派任务", EventType: "work"}); err != nil {
			t.Fatal(err)
		}
		if d := h.Run(closureCtx("t-sched", "已委派")); d.Action != hook.Continue {
			t.Fatalf("有 delegated 任务应放行, got %+v", d)
		}
	})
	t.Run("有pending交互", func(t *testing.T) {
		h, s := newClosureFixture(t)
		publishSchedulerTask(t, s, "t-sched")
		h.Interactions = &fakeInteractionPeek{pending: 1}
		if d := h.Run(closureCtx("t-sched", "等待用户回答")); d.Action != hook.Continue {
			t.Fatalf("有 pending 交互应放行, got %+v", d)
		}
	})
}

// ReviewNaturalExit（2026-08-20 SWE-001 兜底 1）：纯文本自然退出路径的零证据
// 审查——与 report_done PreCall 共享同一 confirmed 状态与证据判定。

func TestSchedulerClosure_ReviewNaturalExitZeroEvidenceConfirmOnce(t *testing.T) {
	h, s := newClosureFixture(t)
	publishSchedulerTask(t, s, "t-sched-text")
	task, err := s.GetTask("t-sched-text")
	if err != nil {
		t.Fatal(err)
	}
	// 零证据：第一次拒绝并给出出路说明
	d1 := h.ReviewNaturalExit(context.Background(), task, "直接答复", false)
	if d1.Allow || !strings.Contains(d1.Nudge, "纯问答") || !strings.Contains(d1.Nudge, "submit_graph") {
		t.Fatalf("零证据第一次应拒绝且 Nudge 含出路: %+v", d1)
	}
	// 第二次确认放行
	if d2 := h.ReviewNaturalExit(context.Background(), task, "直接答复", false); !d2.Allow {
		t.Fatal("同一任务第二次应确认放行")
	}
}

func TestSchedulerClosure_ReviewNaturalExitSharesConfirmedWithReportDone(t *testing.T) {
	h, s := newClosureFixture(t)
	publishSchedulerTask(t, s, "t-sched-shared")
	task, err := s.GetTask("t-sched-shared")
	if err != nil {
		t.Fatal(err)
	}
	// 经纯文本路径确认过一次后，report_done 路径同样视为已确认（两路径同一状态）
	if d := h.ReviewNaturalExit(context.Background(), task, "答复", false); d.Allow {
		t.Fatal("第一次应拒绝")
	}
	if d := h.Run(closureCtx("t-sched-shared", "答复")); d.Action != hook.Continue {
		t.Fatalf("纯文本路径已确认，report_done 应直接放行: %+v", d)
	}
}

func TestSchedulerClosure_ReviewNaturalExitSkipsOutOfScope(t *testing.T) {
	h, s := newClosureFixture(t)
	// 非 scheduler 任务不审查
	if err := s.PublishTask(&model.Task{ID: "t-w2", Description: "worker", EventType: "work"}); err != nil {
		t.Fatal(err)
	}
	wtask, _ := s.GetTask("t-w2")
	if d := h.ReviewNaturalExit(context.Background(), wtask, "x", false); !d.Allow {
		t.Fatal("非 scheduler 任务应放行")
	}
	// 图内 scheduler 任务不审查
	if err := s.PublishTask(&model.Task{ID: "t-g2", Description: "图节点", EventType: "__scheduler__", GraphID: "g1"}); err != nil {
		t.Fatal(err)
	}
	gtask, _ := s.GetTask("t-g2")
	if d := h.ReviewNaturalExit(context.Background(), gtask, "x", false); !d.Allow {
		t.Fatal("图内任务应放行")
	}
}

func TestSchedulerClosure_ReviewNaturalExitEvidencePasses(t *testing.T) {
	h, s := newClosureFixture(t)
	h.Graphs = &fakeGraphLister{summaries: []graph.GraphSummary{{GraphID: "g-exists"}}}
	h.SessionID = func() string { return "" }
	publishSchedulerTask(t, s, "t-sched-evid")
	task, err := s.GetTask("t-sched-evid")
	if err != nil {
		t.Fatal(err)
	}
	if d := h.ReviewNaturalExit(context.Background(), task, "答复", false); !d.Allow {
		t.Fatal("session 已有图证据，应放行")
	}
}

// ReviewNaturalExit 三态状态机（2026-08-21 SWE-008）：放行与否看 toolFailed
// 状态事实，不看正文。矩阵：
//   - 无失败记录：首拒 → 次放（纯问答出口保留）；
//   - 有失败记录：首拒 → 格式提醒 → Retry（换上下文），计数清零重获梯度。
func TestSchedulerClosure_ReviewNaturalExitThreeStates(t *testing.T) {
	ctx := context.Background()

	t.Run("有失败记录_格式提醒后转重试并清零", func(t *testing.T) {
		h, s := newClosureFixture(t)
		publishSchedulerTask(t, s, "t-crash")
		task, err := s.GetTask("t-crash")
		if err != nil {
			t.Fatal(err)
		}
		// 第 1 次：通用拒绝（与无失败记录同）
		d1 := h.ReviewNaturalExit(ctx, task, "残片", true)
		if d1.Allow || d1.Retry || !strings.Contains(d1.Nudge, "submit_graph") {
			t.Fatalf("第 1 次应通用拒绝: %+v", d1)
		}
		// 第 2 次：格式提醒（不放行、不重试）
		d2 := h.ReviewNaturalExit(ctx, task, "残片", true)
		if d2.Allow || d2.Retry {
			t.Fatalf("第 2 次应拒绝+格式提醒而非放行/重试: %+v", d2)
		}
		if !strings.Contains(d2.Nudge, "工具调用失败") || !strings.Contains(d2.Nudge, "标记文本") {
			t.Fatalf("格式提醒应点明工具失败与标记文本: %q", d2.Nudge)
		}
		// 第 3 次：Retry，且计数清零
		d3 := h.ReviewNaturalExit(ctx, task, "残片", true)
		if !d3.Retry || d3.Allow {
			t.Fatalf("第 3 次应转重试: %+v", d3)
		}
		// 清零后第 4 次回到首拒梯度（新 attempt 语义）
		d4 := h.ReviewNaturalExit(ctx, task, "残片", true)
		if d4.Allow || d4.Retry || !strings.Contains(d4.Nudge, "submit_graph") {
			t.Fatalf("计数清零后应回到首拒: %+v", d4)
		}
	})

	t.Run("无失败记录_出口保留且不受后续梯度影响", func(t *testing.T) {
		h, s := newClosureFixture(t)
		publishSchedulerTask(t, s, "t-qa")
		task, err := s.GetTask("t-qa")
		if err != nil {
			t.Fatal(err)
		}
		if d := h.ReviewNaturalExit(ctx, task, "答复", false); d.Allow {
			t.Fatal("第 1 次应拒绝")
		}
		if d := h.ReviewNaturalExit(ctx, task, "答复", false); !d.Allow || d.Retry {
			t.Fatalf("无失败记录第 2 次应放行: %+v", d)
		}
	})

	t.Run("失败记录中途出现_按当次状态判定", func(t *testing.T) {
		h, s := newClosureFixture(t)
		publishSchedulerTask(t, s, "t-mid")
		task, err := s.GetTask("t-mid")
		if err != nil {
			t.Fatal(err)
		}
		if d := h.ReviewNaturalExit(ctx, task, "答复", false); d.Allow {
			t.Fatal("第 1 次应拒绝")
		}
		// 第 2 次时工具失败已发生（如 submit_graph 刚被拒）→ 不放行，格式提醒
		d2 := h.ReviewNaturalExit(ctx, task, "答复", true)
		if d2.Allow || d2.Retry {
			t.Fatalf("失败记录出现后第 2 次应转格式提醒: %+v", d2)
		}
	})
}
