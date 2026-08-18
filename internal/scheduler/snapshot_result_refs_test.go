package scheduler

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func completeSnapshotResultTask(t *testing.T, s *store.MemoryTaskStore, task *model.Task, results map[string]string) {
	t.Helper()
	task.MaxConcurrency = len(results)
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("publish %s: %v", task.ID, err)
	}
	agentIDs := make([]string, 0, len(results))
	for agentID := range results {
		agentIDs = append(agentIDs, agentID)
	}
	sort.Strings(agentIDs)
	for _, agentID := range agentIDs {
		if err := s.ClaimTask(agentID, task.ID); err != nil {
			t.Fatalf("claim %s by %s: %v", task.ID, agentID, err)
		}
	}
	for _, agentID := range agentIDs {
		if err := s.SubmitResult(agentID, task.ID, results[agentID]); err != nil {
			t.Fatalf("submit %s by %s: %v", task.ID, agentID, err)
		}
	}
}

func snapshotTaskByID(t *testing.T, snapshot boardSnapshot, taskID string) taskSnapshot {
	t.Helper()
	for _, task := range snapshot.Tasks {
		if task.ID == taskID {
			return task
		}
	}
	t.Fatalf("task %s missing from snapshot", taskID)
	return taskSnapshot{}
}

func TestBuildBoardJSON_ResultRefsAreBoundedStableAndSorted(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 4, 60)
	short := "short result"
	long := "HEAD-SENTINEL-" + strings.Repeat("中🙂", maxTaskResultExcerptRunes) + "-TAIL-SENTINEL"
	task := &model.Task{ID: "result-task", Description: "large result"}
	completeSnapshotResultTask(t, s, task, map[string]string{
		"z-long":  long,
		"a-short": short,
	})

	raw := BuildBoardJSON(s, &config.Config{}, testModeSnap(), model.Event{}, SnapshotSources{})
	snap := snapshotTaskByID(t, parseSnapshot(t, raw), task.ID)
	if len(snap.ResultRefs) != 2 {
		t.Fatalf("result refs=%+v", snap.ResultRefs)
	}
	if snap.ResultRefs[0].AgentID != "a-short" || snap.ResultRefs[1].AgentID != "z-long" {
		t.Fatalf("result refs must be sorted by agent_id: %+v", snap.ResultRefs)
	}

	for _, ref := range snap.ResultRefs {
		value := map[string]string{"a-short": short, "z-long": long}[ref.AgentID]
		wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
		if ref.OriginalBytes != len(value) || ref.OriginalRunes != utf8.RuneCountInString(value) || ref.SHA256 != wantHash {
			t.Errorf("metadata mismatch for %s: %+v", ref.AgentID, ref)
		}
	}
	if got := snap.ResultRefs[0]; got.Excerpt != short || got.Truncated {
		t.Errorf("short result should remain complete: %+v", got)
	}
	if got := snap.ResultRefs[1]; !got.Truncated || utf8.RuneCountInString(got.Excerpt) > maxTaskResultExcerptRunes ||
		!strings.Contains(got.Excerpt, "HEAD-SENTINEL") || !strings.Contains(got.Excerpt, "TAIL-SENTINEL") {
		t.Errorf("long result excerpt should be bounded head+tail: %+v", got)
	}
	if strings.Contains(raw, `"results"`) || strings.Contains(raw, strings.Repeat("中🙂", maxTaskResultExcerptRunes)) {
		t.Fatal("hot board leaked the legacy full Results body")
	}
	stored, err := s.GetTask(task.ID)
	if err != nil || stored.Results["a-short"] != short || stored.Results["z-long"] != long {
		t.Fatalf("building the hot projection mutated cold Results: task=%+v err=%v", stored, err)
	}
}

func TestBuildBoardJSON_HidesStructuredResultCarrier(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 8), 8, 1, 60)
	task := &model.Task{ID: "structured-result-task", Description: "structured result"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker-a", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordResultField(s, task.ID, agent.StructuredResultStorageKey, `{"coverage":"gap"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult("worker-a", task.ID, "权威结果正文"); err != nil {
		t.Fatal(err)
	}

	raw := BuildBoardJSON(s, &config.Config{}, testModeSnap(), model.Event{}, SnapshotSources{})
	snap := snapshotTaskByID(t, parseSnapshot(t, raw), task.ID)
	if len(snap.ResultRefs) != 1 || snap.ResultRefs[0].AgentID != "worker-a" {
		t.Fatalf("内部 carrier 不得伪装成第二份 Agent 结果: %+v", snap.ResultRefs)
	}
	if strings.Contains(raw, agent.StructuredResultStorageKey) {
		t.Fatal("热快照不得泄漏内部结构化结果 carrier")
	}
}

func TestBuildBoardJSON_ResultExcerptBudgetPrioritizesTriggerSources(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 32), 64, 1, 60)
	var tasks []*model.Task
	for i := 0; i < 9; i++ {
		task := &model.Task{ID: fmt.Sprintf("result-%02d", i), Description: "result"}
		completeSnapshotResultTask(t, s, task, map[string]string{
			"worker": fmt.Sprintf("task-%02d-", i) + strings.Repeat("x", 1800),
		})
		tasks = append(tasks, task)
	}
	trigger := model.Event{TaskID: tasks[0].ID, Payload: map[string]string{"source_task_ids": tasks[1].ID}}
	raw := BuildBoardJSON(s, &config.Config{}, testModeSnap(), trigger, SnapshotSources{})
	if again := BuildBoardJSON(s, &config.Config{}, testModeSnap(), trigger, SnapshotSources{}); again != raw {
		t.Fatal("result-ref allocation should be deterministic")
	}
	snapshot := parseSnapshot(t, raw)
	totalRunes := 0
	emptyExcerpts := 0
	for _, task := range snapshot.Tasks {
		if len(task.ResultRefs) != 1 {
			t.Fatalf("task %s refs=%+v", task.ID, task.ResultRefs)
		}
		totalRunes += utf8.RuneCountInString(task.ResultRefs[0].Excerpt)
		if task.ResultRefs[0].Excerpt == "" {
			emptyExcerpts++
		}
	}
	if totalRunes != maxBoardResultExcerptTotalRunes {
		t.Fatalf("excerpt allocator used %d runes, want exact fixed-input budget %d", totalRunes, maxBoardResultExcerptTotalRunes)
	}
	if emptyExcerpts == 0 {
		t.Fatal("all refs received excerpts despite an exhausted board budget")
	}
	for _, id := range []string{tasks[0].ID, tasks[1].ID} {
		if ref := snapshotTaskByID(t, snapshot, id).ResultRefs[0]; utf8.RuneCountInString(ref.Excerpt) != maxTaskResultExcerptRunes {
			t.Fatalf("trigger/source task %s did not receive a full priority excerpt: %+v", id, ref)
		}
	}
	for _, task := range snapshot.Tasks {
		if ref := task.ResultRefs[0]; ref.Excerpt == "" && !ref.Truncated {
			t.Fatalf("non-empty result without excerpt must be marked truncated: %+v", ref)
		}
	}
}

func TestBuildBoardJSON_ProgressKeepsBoundedUTF8Tail(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	long := "HEAD-SHOULD-DROP-" + strings.Repeat("进🚀", maxTaskProgressTailRunes+200) + "-TAIL"
	task := &model.Task{ID: "running-long", Description: "running"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOutput("worker", task.ID, long); err != nil {
		t.Fatal(err)
	}

	raw := BuildBoardJSON(s, &config.Config{}, testModeSnap(), model.Event{}, SnapshotSources{})
	got := snapshotTaskByID(t, parseSnapshot(t, raw), task.ID).Progress
	if got == nil || !got.Truncated {
		t.Fatalf("expected truncated progress: %+v", got)
	}
	runes := []rune(long)
	wantTail := string(runes[len(runes)-maxTaskProgressTailRunes:])
	if got.RetainedTail != wantTail || got.OriginalBytes != len(long) || got.OriginalRunes != len(runes) {
		t.Fatalf("progress mismatch: %+v", got)
	}
	if strings.Contains(raw, `"partial_output"`) || strings.Contains(raw, "HEAD-SHOULD-DROP") {
		t.Fatal("hot board leaked the unbounded PartialOutput field")
	}
}

func TestBuildBoardJSON_LastResponseUsesBoundedPreview(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	response := "HEAD-" + strings.Repeat("甲", maxTaskLastResponseRunes) + "MIDDLE-SECRET" + strings.Repeat("乙", maxTaskLastResponseRunes) + "-TAIL"
	task := &model.Task{ID: "failed-response", Description: "failed"}
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("worker", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLastResponse(task.ID, response); err != nil {
		t.Fatal(err)
	}
	if err := s.FailTask("worker", task.ID, "failed after response"); err != nil {
		t.Fatal(err)
	}

	raw := BuildBoardJSON(s, &config.Config{}, testModeSnap(), model.Event{}, SnapshotSources{})
	preview := snapshotTaskByID(t, parseSnapshot(t, raw), task.ID).LastResponsePreview
	if preview == nil || !preview.Truncated || utf8.RuneCountInString(preview.Text) > maxTaskLastResponseRunes {
		t.Fatalf("expected bounded LastResponse preview: %+v", preview)
	}
	if preview.OriginalBytes != len(response) || preview.OriginalRunes != utf8.RuneCountInString(response) ||
		!strings.Contains(preview.Text, "HEAD-") || !strings.Contains(preview.Text, "-TAIL") {
		t.Fatalf("LastResponse preview metadata/head/tail mismatch: %+v", preview)
	}
	if strings.Contains(raw, `"last_response":`) || strings.Contains(raw, "MIDDLE-SECRET") {
		t.Fatal("hot board leaked the unbounded LastResponse field")
	}
}

func TestBuildBoardJSON_LegacyVisibilityMatchesRequestTree(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 32), 64, 1, 60)
	controller := &model.Task{ID: "legacy-root", Description: "root", EventType: "__scheduler__"}
	if err := s.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler", controller.ID); err != nil {
		t.Fatal(err)
	}
	child := &model.Task{ID: "legacy-child", ParentTaskID: controller.ID, Description: "child"}
	completeSnapshotResultTask(t, s, child, map[string]string{"worker": "child result"})
	grandchild := &model.Task{ID: "legacy-grandchild", ParentTaskID: child.ID, Description: "grandchild"}
	completeSnapshotResultTask(t, s, grandchild, map[string]string{"worker": "grandchild result"})
	unrelated := &model.Task{ID: "other-root", Description: "other", EventType: "__scheduler__"}
	completeSnapshotResultTask(t, s, unrelated, map[string]string{"worker": "UNRELATED-SECRET"})
	unrelatedBusy := &model.Task{ID: "other-busy-worker", Description: "other busy"}
	if err := s.PublishTask(unrelatedBusy); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("other-worker", unrelatedBusy.ID); err != nil {
		t.Fatal(err)
	}

	raw := BuildBoardJSON(s, &config.Config{}, testModeSnap(), model.Event{}, SnapshotSources{
		CurrentControllerTaskID: controller.ID,
	})
	snapshot := parseSnapshot(t, raw)
	for _, id := range []string{controller.ID, child.ID, grandchild.ID} {
		_ = snapshotTaskByID(t, snapshot, id)
	}
	if strings.Contains(raw, unrelated.ID) || strings.Contains(raw, unrelatedBusy.ID) || strings.Contains(raw, "UNRELATED-SECRET") {
		t.Fatal("legacy board exposed an unrelated request tree")
	}
	if snapshot.Resources.BusyWorkers != 1 {
		t.Fatalf("task filtering corrupted global resource accounting: %+v", snapshot.Resources)
	}
}

func TestBuildBoardJSON_GraphVisibilityIncludesSameGraphResultRefsOnly(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 32), 64, 4, 60)
	controller := &model.Task{
		ID:           "graph-controller",
		Description:  "summarize",
		EventType:    "__scheduler__",
		GraphID:      "g-current",
		NodeID:       "summarize",
		ActivationID: "summarize@1",
	}
	if err := s.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("scheduler", controller.ID); err != nil {
		t.Fatal(err)
	}

	sameGraph := &model.Task{
		ID: "same-graph-result", GraphID: controller.GraphID,
		NodeID: "investigate", ActivationID: "investigate@1",
	}
	completeSnapshotResultTask(t, s, sameGraph, map[string]string{"worker": "same graph evidence"})
	crossGraph := &model.Task{ID: "cross-graph-result", GraphID: "g-other"}
	completeSnapshotResultTask(t, s, crossGraph, map[string]string{"worker": "CROSS-GRAPH-SECRET"})
	legacyDescendant := &model.Task{ID: "legacy-descendant", ParentTaskID: controller.ID}
	completeSnapshotResultTask(t, s, legacyDescendant, map[string]string{"worker": "LEGACY-SECRET"})

	raw := BuildBoardJSON(s, &config.Config{}, testModeSnap(), model.Event{}, SnapshotSources{
		CurrentControllerTaskID: controller.ID,
		CurrentGraphID:          controller.GraphID,
	})
	snapshot := parseSnapshot(t, raw)
	if len(snapshot.Tasks) != 2 {
		t.Fatalf("Graph snapshot tasks=%+v, want controller + same-Graph result only", snapshot.Tasks)
	}
	_ = snapshotTaskByID(t, snapshot, controller.ID)
	resultTask := snapshotTaskByID(t, snapshot, sameGraph.ID)
	if len(resultTask.ResultRefs) != 1 || resultTask.ResultRefs[0].AgentID != "worker" ||
		resultTask.ResultRefs[0].Excerpt != "same graph evidence" {
		t.Fatalf("same-Graph result_refs missing or malformed: %+v", resultTask.ResultRefs)
	}
	for _, leaked := range []string{crossGraph.ID, legacyDescendant.ID, "CROSS-GRAPH-SECRET", "LEGACY-SECRET"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("Graph snapshot leaked cross-scope value %q: %s", leaked, raw)
		}
	}
}

func TestBuildBoardJSON_GraphAgentSnapshotDoesNotLeakCrossGraphCurrentTask(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	controller := &model.Task{ID: "controller", EventType: "__scheduler__", GraphID: "g-current"}
	foreign := &model.Task{ID: "foreign-running", Description: "CROSS-GRAPH-SECRET", GraphID: "g-other"}
	for _, task := range []*model.Task{controller, foreign} {
		if err := s.PublishTask(task); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClaimTask("worker-1", foreign.ID); err != nil {
		t.Fatal(err)
	}
	mb := mailbox.NewRegistry(4)
	mb.Register("worker-1", "")

	raw := BuildBoardJSON(s, &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}, testModeSnap(), model.Event{}, SnapshotSources{
		CurrentGraphID: controller.GraphID,
		MBRegistry:     mb,
	})
	if strings.Contains(raw, foreign.ID) || strings.Contains(raw, foreign.Description) {
		t.Fatalf("Graph agent snapshot leaked cross-Graph current task: %s", raw)
	}
	snap := parseSnapshot(t, raw)
	if len(snap.Resources.Agents) != 1 || snap.Resources.Agents[0].ID != "worker-1" || snap.Resources.Agents[0].CurrentTaskID != "" {
		t.Fatalf("global worker should remain visible without foreign task details: %+v", snap.Resources.Agents)
	}
}

func TestCompactSnapshotExcerpt_RuneBoundaries(t *testing.T) {
	value := "开头🙂-middle-结尾🚀"
	for _, tt := range []struct {
		limit     int
		truncated bool
	}{
		{limit: 0, truncated: true},
		{limit: 1, truncated: true},
		{limit: 6, truncated: true},
		{limit: utf8.RuneCountInString(value), truncated: false},
	} {
		got, truncated := compactSnapshotExcerpt(value, tt.limit)
		if truncated != tt.truncated {
			t.Errorf("limit=%d truncated=%v want %v", tt.limit, truncated, tt.truncated)
		}
		if utf8.RuneCountInString(got) > tt.limit {
			t.Errorf("limit=%d returned %d runes: %q", tt.limit, utf8.RuneCountInString(got), got)
		}
	}
}
