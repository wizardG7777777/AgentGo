package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func publishCompletedResultTask(t *testing.T, s *store.MemoryTaskStore, task *model.Task, results map[string]string) {
	t.Helper()
	task.MaxConcurrency = len(results)
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	agentIDs := make([]string, 0, len(results))
	for agentID := range results {
		agentIDs = append(agentIDs, agentID)
	}
	sort.Strings(agentIDs)
	for _, agentID := range agentIDs {
		if err := s.ClaimTask(agentID, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, agentID := range agentIDs {
		if err := s.SubmitResult(agentID, task.ID, results[agentID]); err != nil {
			t.Fatal(err)
		}
	}
}

func newResultToolRegistry(g SchedulerGroup) *agent.ToolRegistry {
	registry := agent.NewToolRegistry()
	g.Register(registry)
	return registry
}

func dispatchResultPage(t *testing.T, registry *agent.ToolRegistry, args map[string]any) (taskResultPage, error) {
	t.Helper()
	out, err := registry.Dispatch(context.Background(), mkCall("get_task_result", args))
	if err != nil {
		return taskResultPage{}, err
	}
	var page taskResultPage
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("decode task result page: %v\n%s", err, out)
	}
	return page, nil
}

func newLegacyResultFixture(t *testing.T, fifoLimit int) (*store.MemoryTaskStore, *model.Task) {
	t.Helper()
	s := store.NewMemoryTaskStore(make(chan model.Event, 32), fifoLimit, 4, 60)
	controller := &model.Task{ID: "legacy-controller", Description: "request", EventType: "__scheduler__"}
	if err := s.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler", controller.ID); err != nil {
		t.Fatal(err)
	}
	return s, controller
}

func TestSchedulerGroup_GetTaskResultUnicodePaginationAndDigest(t *testing.T) {
	s, controller := newLegacyResultFixture(t, 32)
	result := "甲🙂乙🚀丙-" + strings.Repeat("界🌍", 15)
	target := &model.Task{ID: "unicode-result", Description: "unicode", ParentTaskID: controller.ID}
	publishCompletedResultTask(t, s, target, map[string]string{"worker-a": result})
	registry := newResultToolRegistry(SchedulerGroup{Store: s, Holder: &fakeHolder{id: controller.ID}})

	var rebuilt strings.Builder
	offset := 0
	for {
		page, err := dispatchResultPage(t, registry, map[string]any{
			"task_id": target.ID,
			"offset":  offset,
			"limit":   5,
		})
		if err != nil {
			t.Fatal(err)
		}
		if page.TaskID != target.ID || page.AgentID != "worker-a" || page.Offset != offset ||
			page.OriginalBytes != len(result) || page.OriginalRunes != utf8.RuneCountInString(result) ||
			page.SHA256 != computeSHA256([]byte(result)) {
			t.Fatalf("unstable page metadata: %+v", page)
		}
		if utf8.RuneCountInString(page.Content) > 5 || page.NextOffset <= offset {
			t.Fatalf("invalid page boundary: %+v", page)
		}
		rebuilt.WriteString(page.Content)
		offset = page.NextOffset
		if page.Complete {
			break
		}
	}
	if rebuilt.String() != result {
		t.Fatalf("rune pages did not reconstruct result\n got: %q\nwant: %q", rebuilt.String(), result)
	}

	end, err := dispatchResultPage(t, registry, map[string]any{
		"task_id": target.ID,
		"offset":  utf8.RuneCountInString(result),
		"limit":   3,
	})
	if err != nil || !end.Complete || end.Content != "" {
		t.Fatalf("offset at EOF should return an empty complete page: page=%+v err=%v", end, err)
	}
}

func TestSchedulerGroup_GetTaskResultClampsPageAndValidatesBounds(t *testing.T) {
	s, controller := newLegacyResultFixture(t, 32)
	result := strings.Repeat("界", maxTaskResultPageRunes+50)
	target := &model.Task{ID: "large-result", ParentTaskID: controller.ID}
	publishCompletedResultTask(t, s, target, map[string]string{"worker": result})
	registry := newResultToolRegistry(SchedulerGroup{Store: s, Holder: &fakeHolder{id: controller.ID}})

	page, err := dispatchResultPage(t, registry, map[string]any{
		"task_id": target.ID,
		"limit":   maxTaskResultPageRunes + 999,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.LimitApplied != maxTaskResultPageRunes || utf8.RuneCountInString(page.Content) != maxTaskResultPageRunes || page.Complete {
		t.Fatalf("page limit was not clamped: %+v", page)
	}
	defaultPage, err := dispatchResultPage(t, registry, map[string]any{"task_id": target.ID})
	if err != nil || defaultPage.LimitApplied != defaultTaskResultPageRunes || utf8.RuneCountInString(defaultPage.Content) != defaultTaskResultPageRunes {
		t.Fatalf("default page limit mismatch: page=%+v err=%v", defaultPage, err)
	}

	for _, tt := range []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "negative offset", args: map[string]any{"task_id": target.ID, "offset": -1}, want: "offset 不能为负数"},
		{name: "zero limit", args: map[string]any{"task_id": target.ID, "limit": 0}, want: "limit 必须 > 0"},
		{name: "past end", args: map[string]any{"task_id": target.ID, "offset": len([]rune(result)) + 1}, want: "超出结果总 rune 数"},
		{name: "unknown agent", args: map[string]any{"task_id": target.ID, "agent_id": "missing"}, want: "没有 agent_id=missing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.Dispatch(context.Background(), mkCall("get_task_result", tt.args))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want substring %q", err, tt.want)
			}
		})
	}
}

func TestSchedulerGroup_GetTaskResultRequiresAgentForMultipleResults(t *testing.T) {
	s, controller := newLegacyResultFixture(t, 32)
	target := &model.Task{ID: "multi-result", ParentTaskID: controller.ID}
	publishCompletedResultTask(t, s, target, map[string]string{"z-worker": "z", "a-worker": "a"})
	registry := newResultToolRegistry(SchedulerGroup{Store: s, Holder: &fakeHolder{id: controller.ID}})

	_, err := registry.Dispatch(context.Background(), mkCall("get_task_result", map[string]any{"task_id": target.ID}))
	if err == nil || !strings.Contains(err.Error(), "a-worker, z-worker") {
		t.Fatalf("ambiguous result should list sorted agent IDs: %v", err)
	}
	page, err := dispatchResultPage(t, registry, map[string]any{"task_id": target.ID, "agent_id": "z-worker"})
	if err != nil || page.Content != "z" {
		t.Fatalf("explicit agent result: page=%+v err=%v", page, err)
	}
}

func TestSchedulerGroup_GetTaskResultRejectsNonTerminalResults(t *testing.T) {
	s, controller := newLegacyResultFixture(t, 32)
	target := &model.Task{ID: "still-running", ParentTaskID: controller.ID, MaxConcurrency: 2}
	if err := s.PublishTask(target); err != nil {
		t.Fatal(err)
	}
	for _, agentID := range []string{"worker-a", "worker-b"} {
		if err := s.ClaimTask(agentID, target.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SubmitResult("worker-a", target.ID, "partial but not stable"); err != nil {
		t.Fatal(err)
	}
	registry := newResultToolRegistry(SchedulerGroup{Store: s, Holder: &fakeHolder{id: controller.ID}})
	_, err := registry.Dispatch(context.Background(), mkCall("get_task_result", map[string]any{"task_id": target.ID}))
	if err == nil || !strings.Contains(err.Error(), "Results 仅在终态后") {
		t.Fatalf("processing Results should be rejected: %v", err)
	}
}

// 可见性（C6b）：Scheduler 只能读取自己 SchedulerBatch / ParentTaskID 谱系
// 内的任务结果（store.LegacyRequestTaskIDs）；谱系外的任务一律拒绝。
func TestSchedulerGroup_GetTaskResultLegacyScope(t *testing.T) {
	s, controller := newLegacyResultFixture(t, 64)
	samePlan := &model.Task{ID: "same-plan", ParentTaskID: controller.ID}
	publishCompletedResultTask(t, s, samePlan, map[string]string{"worker": "same"})
	batchOnly := &model.Task{ID: "batch-only"}
	publishCompletedResultTask(t, s, batchOnly, map[string]string{"worker": "batch"})
	if err := s.AppendSchedulerBatch(controller.ID, batchOnly.ID); err != nil {
		t.Fatal(err)
	}
	descendant := &model.Task{ID: "batch-descendant", ParentTaskID: batchOnly.ID}
	publishCompletedResultTask(t, s, descendant, map[string]string{"worker": "descendant"})
	unrelated := &model.Task{ID: "unrelated", EventType: "__scheduler__"}
	publishCompletedResultTask(t, s, unrelated, map[string]string{"worker": "secret"})
	labelOnly := &model.Task{ID: "label-only", BatchID: controller.ID, Dependencies: []string{samePlan.ID}}
	publishCompletedResultTask(t, s, labelOnly, map[string]string{"worker": "label"})

	registry := newResultToolRegistry(SchedulerGroup{Store: s, Holder: &fakeHolder{id: controller.ID}})
	for _, taskID := range []string{samePlan.ID, batchOnly.ID, descendant.ID} {
		if _, err := dispatchResultPage(t, registry, map[string]any{"task_id": taskID}); err != nil {
			t.Fatalf("legacy task %s should be readable: %v", taskID, err)
		}
	}
	for _, taskID := range []string{unrelated.ID, labelOnly.ID} {
		if _, err := registry.Dispatch(context.Background(), mkCall("get_task_result", map[string]any{"task_id": taskID})); err == nil || !strings.Contains(err.Error(), "batch/lineage") {
			t.Fatalf("unrelated legacy task %s should be rejected: %v", taskID, err)
		}
	}
}

func TestSchedulerGroup_GetTaskResultRejectsInactiveCallerAndEvictedResult(t *testing.T) {
	s, controller := newLegacyResultFixture(t, 1)
	first := &model.Task{ID: "evicted", ParentTaskID: controller.ID}
	publishCompletedResultTask(t, s, first, map[string]string{"worker": "first"})
	second := &model.Task{ID: "newer", ParentTaskID: controller.ID}
	publishCompletedResultTask(t, s, second, map[string]string{"worker": "second"})
	registry := newResultToolRegistry(SchedulerGroup{Store: s, Holder: &fakeHolder{id: controller.ID}})
	if _, err := registry.Dispatch(context.Background(), mkCall("get_task_result", map[string]any{"task_id": first.ID})); err == nil || !strings.Contains(err.Error(), "读取任务结果失败") {
		t.Fatalf("evicted result should return a clear unavailable error: %v", err)
	}

	activeStore, activeController := newLegacyResultFixture(t, 8)
	activeTarget := &model.Task{ID: "available", ParentTaskID: activeController.ID}
	publishCompletedResultTask(t, activeStore, activeTarget, map[string]string{"worker": "available"})
	inactiveRegistry := newResultToolRegistry(SchedulerGroup{Store: activeStore, Holder: &fakeHolder{id: activeController.ID}})
	if err := activeStore.TransitionState(activeController.ID, model.TaskStatusProcessing, model.TaskStatusCancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := inactiveRegistry.Dispatch(context.Background(), mkCall("get_task_result", map[string]any{"task_id": activeTarget.ID})); err == nil || !strings.Contains(err.Error(), "不是正在执行的 Scheduler 任务") {
		t.Fatalf("inactive caller should be rejected: %v", err)
	}
}
