package builtin

import (
	"context"
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
