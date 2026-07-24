package watchdog

import (
	"testing"
	"time"

	"agentgo/internal/model"
)

// acceptance 角色的 dependent 不被级联取消：watchdog 只按租约告警一次，
// 由控制面（PlanCoordinator/Scheduler）决定修图；普通任务行为不变。
func TestWatchdog_AcceptanceTaskExemptFromCascadeCancel(t *testing.T) {
	w, s, ch := newTestWatchdog()

	dep := &model.Task{Description: "cancelled dependency"}
	if err := s.PublishTask(dep); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionState(dep.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatal(err)
	}

	acceptance := &model.Task{
		Description:  "formal acceptance runner",
		PlanID:       "plan-1",
		NodeRole:     model.PlanNodeRoleAcceptance,
		Dependencies: []string{dep.ID},
	}
	if err := s.PublishTask(acceptance); err != nil {
		t.Fatal(err)
	}
	normal := &model.Task{
		Description:  "normal dependent",
		Dependencies: []string{dep.ID},
	}
	if err := s.PublishTask(normal); err != nil {
		t.Fatal(err)
	}

	inspectAll(w)
	inspectAll(w)

	got, _ := s.GetTask(acceptance.ID)
	if got.Status != model.TaskStatusPending {
		t.Fatalf("acceptance task status = %s, want pending (exempt from cascade cancel)", got.Status)
	}
	gotNormal, _ := s.GetTask(normal.ID)
	if gotNormal.Status != model.TaskStatusCancelled {
		t.Fatalf("normal task status = %s, want cancelled (cascade still applies)", gotNormal.Status)
	}

	acceptanceAlerts := 0
	for _, evt := range watchdogAlerts(drainEvents(ch)) {
		if evt.TaskID != acceptance.ID {
			continue
		}
		acceptanceAlerts++
		if evt.Payload["reason_code"] != "acceptance_dependency_lost" {
			t.Errorf("reason_code = %q, want acceptance_dependency_lost", evt.Payload["reason_code"])
		}
		if evt.Payload["reason"] == "" {
			t.Error("alert payload missing human-readable reason")
		}
	}
	if acceptanceAlerts != 1 {
		t.Fatalf("acceptance alerts = %d, want exactly one per lease", acceptanceAlerts)
	}
}

// processing 路径同样豁免：验收 runner 运行中依赖被取消时保持 processing。
func TestWatchdog_ProcessingAcceptanceTaskExemptFromCascadeCancel(t *testing.T) {
	w, s, ch := newTestWatchdog()

	dep := &model.Task{Description: "dependency"}
	if err := s.PublishTask(dep); err != nil {
		t.Fatal(err)
	}
	acceptance := &model.Task{
		Description:  "acceptance runner in flight",
		PlanID:       "plan-1",
		NodeRole:     model.PlanNodeRoleAcceptance,
		Dependencies: []string{dep.ID},
	}
	if err := s.PublishTask(acceptance); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionState(acceptance.ID, model.TaskStatusPending, model.TaskStatusProcessing); err != nil {
		t.Fatal(err)
	}
	setTaskTiming(t, s, acceptance.ID, time.Now(), time.Now())

	if err := s.TransitionState(dep.ID, model.TaskStatusPending, model.TaskStatusCancelled); err != nil {
		t.Fatal(err)
	}

	inspectAll(w)
	inspectAll(w)

	got, _ := s.GetTask(acceptance.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Fatalf("status = %s, want processing (exempt from cascade cancel)", got.Status)
	}

	acceptanceAlerts := 0
	for _, evt := range watchdogAlerts(drainEvents(ch)) {
		if evt.TaskID == acceptance.ID && evt.Payload["reason_code"] == "acceptance_dependency_lost" {
			acceptanceAlerts++
		}
	}
	if acceptanceAlerts != 1 {
		t.Fatalf("acceptance alerts = %d, want exactly one per lease", acceptanceAlerts)
	}
}
