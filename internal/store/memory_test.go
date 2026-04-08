package store

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"agentgo/internal/model"
)

func newTestStore(bufSize int, fifoLimit int) (*MemoryTaskStore, chan model.Event) {
	ch := make(chan model.Event, bufSize)
	s := NewMemoryTaskStore(ch, fifoLimit, 2, 300)
	return s, ch
}

func publishTestTask(t *testing.T, s *MemoryTaskStore, desc string) *model.Task {
	t.Helper()
	task := &model.Task{Description: desc}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask failed: %v", err)
	}
	return task
}

// --- Basic CRUD ---

func TestPublishAndGetTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test task")

	if task.ID == "" {
		t.Fatal("task ID should be generated")
	}
	if task.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending", task.Status)
	}
	if task.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if task.MaxConcurrency != 2 {
		t.Errorf("MaxConcurrency = %d, want default 2", task.MaxConcurrency)
	}

	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if got.Description != "test task" {
		t.Errorf("Description = %s, want 'test task'", got.Description)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	s, _ := newTestStore(10, 100)
	_, err := s.GetTask("nonexistent")
	if err != ErrTaskNotFound {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

// --- ClaimTask ---

func TestClaimTask_Basic(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "claim test")

	err := s.ClaimTask("agent-1", task.ID)
	if err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing", got.Status)
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt should be set")
	}
	if len(got.Agents) != 1 || got.Agents[0] != "agent-1" {
		t.Errorf("Agents = %v, want [agent-1]", got.Agents)
	}
}

func TestClaimTask_ConcurrencyLimit(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "concurrency test")

	// Claim twice (MaxConcurrency=2)
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimTask("agent-2", task.ID); err != nil {
		t.Fatal(err)
	}

	// Third claim should fail
	err := s.ClaimTask("agent-3", task.ID)
	if err != ErrConcurrencyFull {
		t.Errorf("err = %v, want ErrConcurrencyFull", err)
	}
}

func TestClaimTask_DependencyNotMet(t *testing.T) {
	s, _ := newTestStore(10, 100)
	dep := publishTestTask(t, s, "dependency")
	task := &model.Task{
		Description:  "depends on dep",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	err := s.ClaimTask("agent-1", task.ID)
	if err != ErrDependencyNotMet {
		t.Errorf("err = %v, want ErrDependencyNotMet", err)
	}
}

func TestClaimTask_DependencyMet(t *testing.T) {
	s, _ := newTestStore(10, 100)
	dep := publishTestTask(t, s, "dependency")

	// Complete the dependency
	s.ClaimTask("agent-1", dep.ID)
	s.SubmitResult("agent-1", dep.ID, "done")

	task := &model.Task{
		Description:  "depends on dep",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	err := s.ClaimTask("agent-2", task.ID)
	if err != nil {
		t.Fatalf("ClaimTask should succeed when dep is completed: %v", err)
	}
}

func TestClaimTask_CompletedTaskFails(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")
	s.ClaimTask("agent-1", task.ID)
	s.SubmitResult("agent-1", task.ID, "done")

	err := s.ClaimTask("agent-2", task.ID)
	if err == nil {
		t.Error("should not be able to claim a completed task")
	}
}

// --- SubmitResult ---

func TestSubmitResult_SingleAgent(t *testing.T) {
	s, ch := newTestStore(10, 100)
	task := publishTestTask(t, s, "submit test")
	s.ClaimTask("agent-1", task.ID)

	err := s.SubmitResult("agent-1", task.ID, "result data")
	if err != nil {
		t.Fatalf("SubmitResult failed: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	if got.Results["agent-1"] != "result data" {
		t.Errorf("result = %s, want 'result data'", got.Results["agent-1"])
	}
	if got.CompletedAt.IsZero() {
		t.Error("CompletedAt should be set")
	}

	// Check event
	select {
	case event := <-ch:
		if event.Type != model.EventTaskCompleted {
			t.Errorf("event type = %s, want task_completed", event.Type)
		}
		if event.TaskID != task.ID {
			t.Errorf("event taskID = %s, want %s", event.TaskID, task.ID)
		}
	default:
		t.Error("expected event on channel")
	}
}

func TestSubmitResult_CooperativeMode(t *testing.T) {
	s, ch := newTestStore(10, 100)
	task := publishTestTask(t, s, "cooperative test")
	s.ClaimTask("agent-1", task.ID)
	s.ClaimTask("agent-2", task.ID)

	// First submit: task should stay processing
	s.SubmitResult("agent-1", task.ID, "part 1")
	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing (still one agent left)", got.Status)
	}

	// Drain any events (there shouldn't be one yet)
	select {
	case <-ch:
		t.Error("should not have completed event yet")
	default:
	}

	// Second submit: task should complete
	s.SubmitResult("agent-2", task.ID, "part 2")
	got, _ = s.GetTask(task.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
	if got.Results["agent-1"] != "part 1" || got.Results["agent-2"] != "part 2" {
		t.Errorf("results = %v, want both parts", got.Results)
	}

	select {
	case event := <-ch:
		if event.Type != model.EventTaskCompleted {
			t.Errorf("event type = %s, want task_completed", event.Type)
		}
	default:
		t.Error("expected completed event")
	}
}

func TestSubmitResult_AgentNotInTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")
	s.ClaimTask("agent-1", task.ID)

	err := s.SubmitResult("agent-99", task.ID, "result")
	if err != ErrAgentNotInTask {
		t.Errorf("err = %v, want ErrAgentNotInTask", err)
	}
}

// --- TransitionState ---

func TestTransitionState_AllValidTransitions(t *testing.T) {
	transitions := []struct {
		from model.TaskStatus
		to   model.TaskStatus
	}{
		{model.TaskStatusPending, model.TaskStatusProcessing},
		{model.TaskStatusPending, model.TaskStatusCancelled},
		{model.TaskStatusPending, model.TaskStatusFailed},
		{model.TaskStatusProcessing, model.TaskStatusCompleted},
		{model.TaskStatusProcessing, model.TaskStatusFailed},
		{model.TaskStatusProcessing, model.TaskStatusCancelled},
		{model.TaskStatusProcessing, model.TaskStatusPending},
	}

	for _, tt := range transitions {
		t.Run(fmt.Sprintf("%s->%s", tt.from, tt.to), func(t *testing.T) {
			s, _ := newTestStore(10, 100)
			task := publishTestTask(t, s, "transition test")

			// Get task to desired from-state
			if tt.from == model.TaskStatusProcessing {
				s.ClaimTask("agent-1", task.ID)
			}

			err := s.TransitionState(task.ID, tt.from, tt.to)
			if err != nil {
				t.Fatalf("valid transition %s->%s failed: %v", tt.from, tt.to, err)
			}

			got, _ := s.GetTask(task.ID)
			if got.Status != tt.to {
				t.Errorf("status = %s, want %s", got.Status, tt.to)
			}
		})
	}
}

func TestTransitionState_InvalidTransition(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")

	// pending -> completed is not valid
	err := s.TransitionState(task.ID, model.TaskStatusPending, model.TaskStatusCompleted)
	if err != ErrInvalidTransition {
		t.Errorf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestTransitionState_WrongFromState(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "test")

	// Task is pending, but we say from=processing
	err := s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCompleted)
	if err == nil {
		t.Error("should fail when from-state doesn't match")
	}
}

func TestTransitionState_Events(t *testing.T) {
	tests := []struct {
		to       model.TaskStatus
		expected model.EventType
	}{
		{model.TaskStatusFailed, model.EventTaskFailed},
		{model.TaskStatusCancelled, model.EventTaskCancelled},
	}

	for _, tt := range tests {
		t.Run(string(tt.to), func(t *testing.T) {
			s, ch := newTestStore(10, 100)
			task := publishTestTask(t, s, "event test")

			s.TransitionState(task.ID, model.TaskStatusPending, tt.to)

			select {
			case event := <-ch:
				if event.Type != tt.expected {
					t.Errorf("event type = %s, want %s", event.Type, tt.expected)
				}
			default:
				t.Error("expected event")
			}
		})
	}
}

// --- RetryRollback ---

func TestRetryRollback_Basic(t *testing.T) {
	s, ch := newTestStore(10, 100)
	task := publishTestTask(t, s, "retry test")
	s.ClaimTask("agent-1", task.ID)

	err := s.RetryRollback("agent-1", task.ID, "temporary error")
	if err != nil {
		t.Fatalf("RetryRollback failed: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusPending {
		t.Errorf("status = %s, want pending", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", got.RetryCount)
	}
	if len(got.RetryReasons) != 1 || got.RetryReasons[0] != "temporary error" {
		t.Errorf("RetryReasons = %v, want ['temporary error']", got.RetryReasons)
	}

	select {
	case event := <-ch:
		if event.Type != model.EventTaskRetry {
			t.Errorf("event type = %s, want task_retry", event.Type)
		}
	default:
		t.Error("expected retry event")
	}
}

func TestRetryRollback_CooperativeStaysProcessing(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "cooperative retry")
	s.ClaimTask("agent-1", task.ID)
	s.ClaimTask("agent-2", task.ID)

	// Agent-1 retries, but agent-2 still working
	s.RetryRollback("agent-1", task.ID, "error")

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusProcessing {
		t.Errorf("status = %s, want processing (agent-2 still working)", got.Status)
	}
}

// --- QueryAvailable ---

func TestQueryAvailable_FilterAndSort(t *testing.T) {
	s, _ := newTestStore(10, 100)

	t1 := &model.Task{Description: "low", Priority: 1, EventType: "code"}
	t2 := &model.Task{Description: "high", Priority: 10, EventType: "code"}
	t3 := &model.Task{Description: "other", Priority: 5, EventType: "search"}
	t4 := &model.Task{Description: "untyped", Priority: 3, EventType: ""}
	s.PublishTask(t1)
	s.PublishTask(t2)
	s.PublishTask(t3)
	s.PublishTask(t4)

	// Filter by event type
	tasks, _ := s.QueryAvailable("code")
	if len(tasks) != 2 {
		t.Fatalf("got %d code tasks, want 2", len(tasks))
	}
	if tasks[0].Priority != 10 {
		t.Error("should be sorted by priority descending")
	}

	// 严格匹配：空 EventType 过滤器只返回 EventType="" 的任务（Worker 语义），
	// 不再像旧实现那样作为通配符返回所有任务。这个修复防止 Worker 顺手抢走
	// explore 类型任务，避免跨代理类型迁移引发的契约违约。
	untyped, _ := s.QueryAvailable("")
	if len(untyped) != 1 {
		t.Fatalf("empty filter should match only EventType=\"\" tasks, got %d, want 1", len(untyped))
	}
	if untyped[0].Description != "untyped" {
		t.Errorf("got %q, want \"untyped\"", untyped[0].Description)
	}

	// 严格匹配：search 过滤器只返回 search 任务
	search, _ := s.QueryAvailable("search")
	if len(search) != 1 {
		t.Fatalf("got %d search tasks, want 1", len(search))
	}
}

// --- FIFO Eviction ---

func TestFIFOEviction(t *testing.T) {
	s, _ := newTestStore(10, 3) // fifoLimit=3

	var taskIDs []string
	for i := 0; i < 5; i++ {
		task := publishTestTask(t, s, fmt.Sprintf("task-%d", i))
		taskIDs = append(taskIDs, task.ID)
		s.ClaimTask("agent", task.ID)
		s.SubmitResult("agent", task.ID, "done")
	}

	// First two should be evicted
	for _, id := range taskIDs[:2] {
		_, err := s.GetTask(id)
		if err != ErrTaskNotFound {
			t.Errorf("task %s should be evicted", id)
		}
	}
	// Last three should still exist
	for _, id := range taskIDs[2:] {
		_, err := s.GetTask(id)
		if err != nil {
			t.Errorf("task %s should still exist: %v", id, err)
		}
	}
}

// --- GetDependencyResults ---

func TestGetDependencyResults(t *testing.T) {
	s, _ := newTestStore(10, 100)
	dep := publishTestTask(t, s, "dep task")
	s.ClaimTask("agent-1", dep.ID)
	s.SubmitResult("agent-1", dep.ID, "dep result")

	task := &model.Task{
		Description:  "main task",
		Dependencies: []string{dep.ID},
	}
	s.PublishTask(task)

	results, err := s.GetDependencyResults(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if results[dep.ID] != "dep result" {
		t.Errorf("dep result = %s, want 'dep result'", results[dep.ID])
	}
}

// --- Concurrency ---

func TestConcurrentClaim(t *testing.T) {
	s, _ := newTestStore(100, 100)
	task := &model.Task{Description: "race test", MaxConcurrency: 3}
	s.PublishTask(task)

	var wg sync.WaitGroup
	successes := make(chan string, 10)
	start := make(chan struct{})

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			agentID := fmt.Sprintf("agent-%d", id)
			if err := s.ClaimTask(agentID, task.ID); err == nil {
				successes <- agentID
			}
		}(i)
	}

	close(start) // start all goroutines simultaneously
	wg.Wait()
	close(successes)

	count := 0
	for range successes {
		count++
	}
	if count != 3 {
		t.Errorf("expected 3 successful claims, got %d", count)
	}
}

func TestConcurrentPublish(t *testing.T) {
	s, _ := newTestStore(100, 100)
	var wg sync.WaitGroup
	n := 50

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			task := &model.Task{Description: fmt.Sprintf("task-%d", id)}
			s.PublishTask(task)
		}(i)
	}
	wg.Wait()

	all, _ := s.ScanAll()
	if len(all) != n {
		t.Errorf("expected %d tasks, got %d", n, len(all))
	}
}

// --- Event non-blocking ---

func TestEventNonBlocking(t *testing.T) {
	// Channel buffer size 1, publish 3 completing tasks — should not block
	s, _ := newTestStore(1, 100)

	for i := 0; i < 3; i++ {
		task := publishTestTask(t, s, "nonblock test")
		s.ClaimTask("agent", task.ID)

		done := make(chan struct{})
		go func() {
			s.SubmitResult("agent", task.ID, "done")
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("SubmitResult blocked on full event channel")
		}
	}
}

func TestTransitionState_ClearsAgentsOnTerminal(t *testing.T) {
	s, _ := newTestStore(64, 100)

	task := publishTestTask(t, s, "multi-agent task")
	s.ClaimTask("agent-1", task.ID)
	s.ClaimTask("agent-2", task.ID)

	// 确认有 2 个代理
	got, _ := s.GetTask(task.ID)
	if len(got.Agents) != 2 {
		t.Fatalf("precondition: agents = %d, want 2", len(got.Agents))
	}

	// 外部取消任务（如看门狗超时）
	err := s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled)
	if err != nil {
		t.Fatalf("TransitionState error: %v", err)
	}

	got, _ = s.GetTask(task.ID)
	if len(got.Agents) != 0 {
		t.Errorf("agents after cancel = %d, want 0 (should be cleared)", len(got.Agents))
	}
}

func TestTransitionState_ClearsAgentsOnFailed(t *testing.T) {
	s, _ := newTestStore(64, 100)

	task := publishTestTask(t, s, "failing task")
	s.ClaimTask("agent-1", task.ID)

	err := s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusFailed)
	if err != nil {
		t.Fatalf("TransitionState error: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if len(got.Agents) != 0 {
		t.Errorf("agents after failed = %d, want 0", len(got.Agents))
	}
}

// --- AppendOutput ---

func TestAppendOutput_Basic(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "streaming task")
	s.ClaimTask("agent-1", task.ID)

	if err := s.AppendOutput("agent-1", task.ID, "chunk1"); err != nil {
		t.Fatalf("first AppendOutput failed: %v", err)
	}
	if err := s.AppendOutput("agent-1", task.ID, "chunk2"); err != nil {
		t.Fatalf("second AppendOutput failed: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if got.PartialOutput != "chunk1chunk2" {
		t.Errorf("PartialOutput = %q, want %q", got.PartialOutput, "chunk1chunk2")
	}
}

func TestAppendOutput_WrongAgent(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "wrong agent test")
	s.ClaimTask("agent-1", task.ID)

	err := s.AppendOutput("agent-99", task.ID, "data")
	if err != ErrAgentNotInTask {
		t.Errorf("err = %v, want ErrAgentNotInTask", err)
	}
}

func TestAppendOutput_WrongState(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "wrong state test")

	err := s.AppendOutput("agent-1", task.ID, "data")
	if err != ErrTaskNotProcessing {
		t.Errorf("err = %v, want ErrTaskNotProcessing", err)
	}
}

// --- Dependency-Aware FIFO Eviction ---

func TestFIFOEviction_DependencyProtection(t *testing.T) {
	s, _ := newTestStore(10, 3) // fifoLimit=3

	var taskIDs []string
	for i := 0; i < 5; i++ {
		task := &model.Task{Description: fmt.Sprintf("task-%d", i)}
		if err := s.PublishTask(task); err != nil {
			t.Fatalf("PublishTask failed: %v", err)
		}
		taskIDs = append(taskIDs, task.ID)
	}

	// 任务4依赖任务0
	s.mu.Lock()
	s.tasks[taskIDs[4]].Dependencies = []string{taskIDs[0]}
	s.mu.Unlock()

	// 完成任务0-3
	for i := 0; i < 4; i++ {
		s.ClaimTask("agent", taskIDs[i])
		s.SubmitResult("agent", taskIDs[i], "done")
	}

	// 任务0应受保护（被任务4依赖）
	_, err := s.GetTask(taskIDs[0])
	if err != nil {
		t.Errorf("task 0 should be protected by dependency, got err: %v", err)
	}

	// 任务1应被驱逐
	_, err = s.GetTask(taskIDs[1])
	if err != ErrTaskNotFound {
		t.Errorf("task 1 should be evicted, got err: %v", err)
	}
}

func TestFIFOEviction_ProtectedTaskEventuallyEvicted(t *testing.T) {
	s, _ := newTestStore(10, 2) // fifoLimit=2

	var taskIDs []string
	for i := 0; i < 3; i++ {
		task := &model.Task{Description: fmt.Sprintf("task-%d", i)}
		if err := s.PublishTask(task); err != nil {
			t.Fatalf("PublishTask failed: %v", err)
		}
		taskIDs = append(taskIDs, task.ID)
	}

	s.mu.Lock()
	s.tasks[taskIDs[2]].Dependencies = []string{taskIDs[0]}
	s.mu.Unlock()

	// 完成任务0和1
	for i := 0; i < 2; i++ {
		s.ClaimTask("agent", taskIDs[i])
		s.SubmitResult("agent", taskIDs[i], "done")
	}

	// 任务0应受保护
	_, err := s.GetTask(taskIDs[0])
	if err != nil {
		t.Errorf("task 0 should be protected: %v", err)
	}

	// 完成任务2，解除依赖
	s.ClaimTask("agent", taskIDs[2])
	s.SubmitResult("agent", taskIDs[2], "done")

	// 任务0不再受保护，应被驱逐
	_, err = s.GetTask(taskIDs[0])
	if err != ErrTaskNotFound {
		t.Errorf("task 0 should be evicted after dependent task completed, got err: %v", err)
	}
}

func TestAppendArtifact_Basic(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "test"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if err := s.AppendArtifact(task.ID, "docs/foo.md"); err != nil {
		t.Fatalf("AppendArtifact: %v", err)
	}
	if err := s.AppendArtifact(task.ID, "docs/bar.md"); err != nil {
		t.Fatalf("AppendArtifact: %v", err)
	}

	got, _ := s.GetTask(task.ID)
	if len(got.Artifacts) != 2 {
		t.Errorf("expected 2 artifacts, got %d: %v", len(got.Artifacts), got.Artifacts)
	}
	if got.Artifacts[0] != "docs/foo.md" || got.Artifacts[1] != "docs/bar.md" {
		t.Errorf("artifacts order or content wrong: %v", got.Artifacts)
	}
}

func TestAppendArtifact_Dedup(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "test"}
	s.PublishTask(task)

	// 同一文件追加 5 次
	for i := 0; i < 5; i++ {
		if err := s.AppendArtifact(task.ID, "docs/same.md"); err != nil {
			t.Fatalf("AppendArtifact: %v", err)
		}
	}

	got, _ := s.GetTask(task.ID)
	if len(got.Artifacts) != 1 {
		t.Errorf("expected dedup to 1 entry, got %d: %v", len(got.Artifacts), got.Artifacts)
	}
}

func TestAppendArtifact_TaskNotFound(t *testing.T) {
	s, _ := newTestStore(10, 100)
	err := s.AppendArtifact("nonexistent-id", "docs/foo.md")
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestGetDependencyArtifacts_Basic(t *testing.T) {
	s, _ := newTestStore(10, 100)

	// 创建上游任务 A，写入两个 artifact
	taskA := &model.Task{Description: "upstream A"}
	s.PublishTask(taskA)
	s.AppendArtifact(taskA.ID, "docs/a1.md")
	s.AppendArtifact(taskA.ID, "docs/a2.md")

	// 创建上游任务 B，写入一个 artifact
	taskB := &model.Task{Description: "upstream B"}
	s.PublishTask(taskB)
	s.AppendArtifact(taskB.ID, "docs/b1.md")

	// 创建下游任务 C，依赖 A 和 B
	taskC := &model.Task{
		Description:  "downstream C",
		Dependencies: []string{taskA.ID, taskB.ID},
	}
	s.PublishTask(taskC)

	// 查询 C 的依赖 artifacts
	deps, err := s.GetDependencyArtifacts(taskC.ID)
	if err != nil {
		t.Fatalf("GetDependencyArtifacts: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 dep entries, got %d", len(deps))
	}
	if len(deps[taskA.ID]) != 2 || deps[taskA.ID][0] != "docs/a1.md" {
		t.Errorf("taskA artifacts wrong: %v", deps[taskA.ID])
	}
	if len(deps[taskB.ID]) != 1 || deps[taskB.ID][0] != "docs/b1.md" {
		t.Errorf("taskB artifacts wrong: %v", deps[taskB.ID])
	}
}

func TestGetDependencyArtifacts_EmptyArtifacts(t *testing.T) {
	s, _ := newTestStore(10, 100)
	// 上游任务没有任何 artifacts（report-only 失败模式）
	taskA := &model.Task{Description: "upstream A"}
	s.PublishTask(taskA)

	taskB := &model.Task{
		Description:  "downstream B",
		Dependencies: []string{taskA.ID},
	}
	s.PublishTask(taskB)

	deps, err := s.GetDependencyArtifacts(taskB.ID)
	if err != nil {
		t.Fatalf("GetDependencyArtifacts: %v", err)
	}
	// 上游应当出现在 map 中，值为空 slice，让下游能识别"依赖存在但产出为空"
	if _, ok := deps[taskA.ID]; !ok {
		t.Errorf("expected taskA in deps map even with empty artifacts")
	}
	if len(deps[taskA.ID]) != 0 {
		t.Errorf("expected empty artifacts for taskA, got: %v", deps[taskA.ID])
	}
}

func TestGetDependencyArtifacts_NoDependencies(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "independent"}
	s.PublishTask(task)

	deps, err := s.GetDependencyArtifacts(task.ID)
	if err != nil {
		t.Fatalf("GetDependencyArtifacts: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected empty deps map, got: %v", deps)
	}
}

func TestGetDependencyArtifacts_TaskNotFound(t *testing.T) {
	s, _ := newTestStore(10, 100)
	_, err := s.GetDependencyArtifacts("nonexistent-id")
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestAppendArtifact_ConcurrentSameTask(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := &model.Task{Description: "concurrent test"}
	s.PublishTask(task)

	// 100 goroutine 并发追加 100 个不同 artifact
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s.AppendArtifact(task.ID, fmt.Sprintf("docs/file_%03d.md", idx))
		}(i)
	}
	wg.Wait()

	got, _ := s.GetTask(task.ID)
	if len(got.Artifacts) != 100 {
		t.Errorf("expected 100 unique artifacts, got %d", len(got.Artifacts))
	}
}
