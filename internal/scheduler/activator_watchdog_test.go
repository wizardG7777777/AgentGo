package scheduler

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

// 带 Payload 的 watchdog 告警上送 replan 时保留真实 reason_code 与原文 Detail，
// 让 Scheduler 能区分"暂时排队"与真正的异常。
func TestActivator_EventWatchdogAlert_ForwardsPayloadToReplan(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	a := NewActivator(s, ch, make(chan struct{}, 1), nil)

	pc := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := pc.Create(context.Background(), plan.CreateInput{PlanID: "plan-1", RootTaskID: "plan-1-root", Budget: model.PlanBudget{}}); err != nil {
		t.Fatal(err)
	}
	a.PlanCoordinator = pc

	task := &model.Task{Description: "queued explore task", EventType: "explore", PlanID: "plan-1"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	a.handleEvent(model.Event{
		Type:   model.EventWatchdogAlert,
		TaskID: task.ID,
		Payload: map[string]string{
			"reason_code": "claim_starvation",
			"reason":      "claim_starvation: compatible route exists for event_type=\"explore\"; task remains pending",
		},
	})

	p, err := pc.Store().GetPlan("plan-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.PendingReplanRequests) != 1 {
		t.Fatalf("pending replan requests = %d, want 1", len(p.PendingReplanRequests))
	}
	for _, req := range p.PendingReplanRequests {
		if req.ReasonCode != "claim_starvation" {
			t.Errorf("ReasonCode = %q, want claim_starvation", req.ReasonCode)
		}
		if !strings.Contains(req.Detail, "task remains pending") {
			t.Errorf("Detail = %q, want watchdog alert reason text", req.Detail)
		}
		if req.Urgency != model.ReplanUrgencyHigh {
			t.Errorf("Urgency = %q, want high", req.Urgency)
		}
	}
}

// 无 Payload 的告警（如 sendAlert 裸告警）回退到通用 reason_code，行为与旧版一致。
func TestActivator_EventWatchdogAlert_NoPayloadFallback(t *testing.T) {
	ch := make(chan model.Event, 4)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	a := NewActivator(s, ch, make(chan struct{}, 1), nil)

	pc := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := pc.Create(context.Background(), plan.CreateInput{PlanID: "plan-2", RootTaskID: "plan-2-root", Budget: model.PlanBudget{}}); err != nil {
		t.Fatal(err)
	}
	a.PlanCoordinator = pc

	task := &model.Task{Description: "timed out task", PlanID: "plan-2"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	a.handleEvent(model.Event{Type: model.EventWatchdogAlert, TaskID: task.ID})

	p, err := pc.Store().GetPlan("plan-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.PendingReplanRequests) != 1 {
		t.Fatalf("pending replan requests = %d, want 1", len(p.PendingReplanRequests))
	}
	for _, req := range p.PendingReplanRequests {
		if req.ReasonCode != "watchdog_alert" {
			t.Errorf("ReasonCode = %q, want watchdog_alert fallback", req.ReasonCode)
		}
	}
}
