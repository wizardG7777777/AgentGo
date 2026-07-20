package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
)

func TestBuildBoardJSONExposesPersistedReactorAndAgentReplanDetails(t *testing.T) {
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: "plan"}); err != nil {
		t.Fatal(err)
	}
	reactorRequest, err := coordinator.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: "plan", SourceTaskID: "worker-task", SourceEvent: "task_retry",
		ReasonCode: "worker_retry_pressure", Detail: "third retry still fails in package scheduler",
		ObservedRevision: 4, Urgency: model.ReplanUrgencyHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	agentRequest, err := coordinator.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: "plan", SourceTaskID: "acceptance-task", SourceEvent: "request_replan_tool",
		ReasonCode: "acceptance_fix_needed", Detail: "go test failed in TestCurrentGraph",
		ObservedRevision: 4, Urgency: model.ReplanUrgencyNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := coordinator.Store().GetPlan("plan")
	if err != nil {
		t.Fatal(err)
	}
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 1), 16, 1, 60)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	raw := BuildBoardJSON(taskStore, cfg, testModeSnap("plan"), model.Event{Type: model.EventPlanSignal}, SnapshotSources{Plan: p})
	board := parseSnapshot(t, raw)

	if board.Plan == nil || board.Plan.PendingReplanCount != 2 || board.Plan.PendingReplanOmitted != 0 {
		t.Fatalf("pending replan envelope = %+v", board.Plan)
	}
	byID := make(map[string]replanRequestSnapshot)
	for _, request := range board.Plan.PendingReplanRequests {
		byID[request.RequestID] = request
	}
	reactorSummary := byID[reactorRequest.ID]
	if reactorSummary.Reason != "worker_retry_pressure" ||
		reactorSummary.Detail != "third retry still fails in package scheduler" ||
		reactorSummary.SourceTaskID != "worker-task" || reactorSummary.SourceEvent != "task_retry" ||
		reactorSummary.ObservedRevision != 4 ||
		reactorSummary.ObservedStateVersion != reactorRequest.ObservedStateVersion ||
		reactorSummary.Urgency != string(model.ReplanUrgencyHigh) {
		t.Fatalf("reactor request did not reach Scheduler snapshot: %+v", reactorSummary)
	}
	agentSummary := byID[agentRequest.ID]
	if agentSummary.Reason != "acceptance_fix_needed" ||
		agentSummary.Detail != "go test failed in TestCurrentGraph" ||
		agentSummary.SourceTaskID != "acceptance-task" ||
		agentSummary.SourceEvent != "request_replan_tool" ||
		agentSummary.ObservedRevision != 4 ||
		agentSummary.ObservedStateVersion != agentRequest.ObservedStateVersion ||
		agentSummary.Urgency != string(model.ReplanUrgencyNormal) {
		t.Fatalf("agent request did not reach Scheduler snapshot: %+v", agentSummary)
	}
}

func TestBuildBoardJSONCapsPendingReplanSummaries(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	p := &model.Plan{
		ID: "storm", Status: model.PlanStatusRunning,
		PendingReplanRequests: make(map[string]model.ReplanRequest),
	}
	for i := 0; i < 24; i++ {
		id := fmt.Sprintf("request-%02d", i)
		urgency := model.ReplanUrgencyNormal
		if i >= 18 {
			urgency = model.ReplanUrgencyHigh
		}
		detail := fmt.Sprintf("detail-%02d-%s", i, strings.Repeat("界", 1000))
		if i == 0 {
			detail = "OMITTED_SENTINEL_" + detail
		}
		p.PendingReplanRequests[id] = model.ReplanRequest{
			ID: id, PlanID: p.ID, SourceTaskID: fmt.Sprintf("task-%02d", i),
			SourceEvent: "user.event", ReasonCode: fmt.Sprintf("reason-%02d", i), Detail: detail,
			ObservedRevision: 3, ObservedStateVersion: int64(i), Urgency: urgency,
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		}
	}

	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 1), 16, 1, 60)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}
	raw := BuildBoardJSON(taskStore, cfg, testModeSnap("plan"), model.Event{Type: model.EventPlanSignal}, SnapshotSources{Plan: p})
	board := parseSnapshot(t, raw)

	requests := board.Plan.PendingReplanRequests
	if board.Plan.PendingReplanCount != 24 || len(requests) != maxReplanRequestSnapshots ||
		board.Plan.PendingReplanOmitted != 24-maxReplanRequestSnapshots {
		t.Fatalf("request cap envelope: count=%d visible=%d omitted=%d", board.Plan.PendingReplanCount,
			len(requests), board.Plan.PendingReplanOmitted)
	}
	for i := 0; i < 6; i++ {
		if requests[i].Urgency != string(model.ReplanUrgencyHigh) {
			t.Fatalf("high urgency request was displaced at index %d: %+v", i, requests[i])
		}
	}
	totalDetailRunes := 0
	for _, request := range requests {
		detailRunes := len([]rune(request.Detail))
		if detailRunes > maxReplanRequestDetailRunes {
			t.Fatalf("detail cap exceeded for %s: %d", request.RequestID, detailRunes)
		}
		totalDetailRunes += detailRunes
		if !request.DetailTruncated {
			t.Fatalf("long detail was not marked truncated: %+v", request)
		}
	}
	if totalDetailRunes > maxReplanRequestTotalDetailRunes {
		t.Fatalf("total detail cap exceeded: %d", totalDetailRunes)
	}
	if strings.Contains(raw, "OMITTED_SENTINEL") {
		t.Fatalf("omitted request detail leaked into Scheduler prompt: %s", raw)
	}
}
