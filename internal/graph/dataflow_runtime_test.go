package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type dataflowTestBoard struct {
	mu        sync.Mutex
	published map[string]AgentTaskDispatch
	checksErr error
}

func (b *dataflowTestBoard) CheckAgentTask(context.Context, AgentTaskDispatch) error {
	return b.checksErr
}
func (b *dataflowTestBoard) PublishAgentTask(_ context.Context, d AgentTaskDispatch) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.published == nil {
		b.published = map[string]AgentTaskDispatch{}
	}
	b.published[d.TaskID] = d
	return nil
}
func (b *dataflowTestBoard) CancelAgentTask(context.Context, string, string) error { return nil }

type dataflowCommitProbe struct {
	count int
	fail  bool
}

func (c *dataflowCommitProbe) ValidateCandidate(context.Context, string, string, string) error {
	return nil
}

func (c *dataflowCommitProbe) CommitCandidate(_ context.Context, _, _, ref string) (string, error) {
	c.count++
	if c.fail {
		return "", fmt.Errorf("交付不可确认")
	}
	return "delivery:" + ref, nil
}

func newDataflowTestRuntime(t *testing.T) (*DataflowRuntime, *dataflowTestBoard, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewDataflowStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	b := &dataflowTestBoard{}
	return NewDataflowRuntime(s, b), b, dir
}

func settleDataflowTestNode(t *testing.T, r *DataflowRuntime, id, node, candidate string) DataflowSnapshot {
	t.Helper()
	s, _, err := r.Store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions[node]
	s, err = r.RecordTerminal(context.Background(), id, node, AgentTaskTerminal{TaskID: e.TaskID, AttemptID: e.TaskID + "/attempt-1", OutcomeRef: "outcome:" + node, Status: "completed", Value: map[string]any{"summary": node}, CandidateRef: candidate})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDataflowIncrementalExecutionAndCompletion(t *testing.T) {
	r, b, dir := newDataflowTestRuntime(t)
	ctx := context.Background()
	d := testDataflowDefinition()
	if _, err := r.Create(ctx, "create", d); err != nil {
		t.Fatal(err)
	}
	// 调用方修改定义不能污染已持久化图。
	d.Nodes[0].Title = "外部改写"
	s, _, _ := r.Store.Get(d.GraphID)
	if s.Definition.Nodes[0].Title == "外部改写" {
		t.Fatal("定义未深拷贝")
	}
	if _, err := r.Start(ctx, d.GraphID, "start", 1); err != nil {
		t.Fatal(err)
	}
	settleDataflowTestNode(t, r, d.GraphID, "investigate", "")
	if err := r.Step(ctx, d.GraphID); err != nil {
		t.Fatal(err)
	}
	s, _, _ = r.Store.Get(d.GraphID)
	if s.Status != "open" || len(s.PlanningEvents) == 0 {
		t.Fatal("调查完成应等待扩图，不自动结束")
	}
	fix := testAgentTask("fix")
	fix.Inputs = map[string]DataflowInputSource{"research": {Kind: "node_result", NodeID: "investigate"}}
	change := DataflowChange{RequestID: "add-fix", ExpectedRevision: 1, Add: []AgentTaskNode{fix}}
	if _, err := r.Apply(ctx, d.GraphID, change); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(ctx, d.GraphID, change); err != nil {
		t.Fatalf("重复更新应返回原结果: %v", err)
	}
	if len(b.published) != 2 {
		t.Fatal("更新不应重复派发")
	}
	s = settleDataflowTestNode(t, r, d.GraphID, "fix", "candidate:fixed")
	commit := &dataflowCommitProbe{}
	r.Delivery = commit
	req := CompleteDataflowRequest{RequestID: "finish", ExpectedRevision: 2, Outcome: "success", Summary: "交付确定结果", ResultRefs: []string{s.Results["fix"].Ref}}
	final, err := r.Complete(ctx, d.GraphID, req)
	if err != nil || final.Status != "completed" || commit.count != 1 {
		t.Fatalf("图级完成应实际提交候选: %+v %v", final, err)
	}
	if _, err := r.Complete(ctx, d.GraphID, req); err != nil || commit.count != 1 {
		t.Fatal("重复 complete 不得重放交付")
	}
	if _, err := r.Apply(ctx, d.GraphID, DataflowChange{RequestID: "resurrect", ExpectedRevision: 2, Add: []AgentTaskNode{testAgentTask("new")}}); err == nil {
		t.Fatal("终态图不能复活")
	}
	recovered, err := NewDataflowStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	saved, ok, err := recovered.Get(d.GraphID)
	if err != nil || !ok || saved.Completion.DeliveryRef != final.Completion.DeliveryRef {
		t.Fatalf("恢复丢失交付权威: %v", err)
	}
}

func TestDataflowCompletionFencesAndUnknown(t *testing.T) {
	r, _, _ := newDataflowTestRuntime(t)
	d := testDataflowDefinition()
	ctx := context.Background()
	_, _ = r.Create(ctx, "create", d)
	_, _ = r.Start(ctx, d.GraphID, "start", 1)
	if _, err := r.Complete(ctx, d.GraphID, CompleteDataflowRequest{RequestID: "early", ExpectedRevision: 1, Outcome: "success", Summary: "不能吞掉在途工作"}); err == nil {
		t.Fatal("在途工作必须阻止完成")
	}
	s := settleDataflowTestNode(t, r, d.GraphID, "investigate", "candidate:x")
	c := &dataflowCommitProbe{fail: true}
	r.Delivery = c
	req := CompleteDataflowRequest{RequestID: "done", ExpectedRevision: 1, Outcome: "success", Summary: "结果", ResultRefs: []string{s.Results["investigate"].Ref}}
	if _, err := r.Complete(ctx, d.GraphID, req); err == nil {
		t.Fatal("交付失败不能完成")
	}
	if _, err := r.Complete(ctx, d.GraphID, req); err == nil || c.count != 1 {
		t.Fatal("unknown 不得自动重放")
	}
	if _, err := r.Apply(ctx, d.GraphID, DataflowChange{RequestID: "new", ExpectedRevision: 1, Add: []AgentTaskNode{testAgentTask("later")}}); err == nil {
		t.Fatal("finalizing 不得扩图")
	}
}

func TestDataflowAppendCASAndJournalIntegrity(t *testing.T) {
	r, _, dir := newDataflowTestRuntime(t)
	d := testDataflowDefinition()
	ctx := context.Background()
	_, _ = r.Create(ctx, "create", d)
	var wg sync.WaitGroup
	wins := make(chan bool, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := r.Apply(ctx, d.GraphID, DataflowChange{RequestID: id, ExpectedRevision: 1, Add: []AgentTaskNode{testAgentTask(id)}})
			wins <- err == nil
		}(id)
	}
	wg.Wait()
	close(wins)
	count := 0
	for win := range wins {
		if win {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("CAS 只能一个成功，实际 %d", count)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatal("缺少图日志")
	}
	f, err := os.OpenFile(files[0], os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("{broken\n")
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if _, err := NewDataflowStore(dir); err == nil {
		t.Fatal("坏历史不能跳过后继续执行")
	}
}

func TestDataflowFanInAndIdleDoesNotRecallPlanner(t *testing.T) {
	r, b, _ := newDataflowTestRuntime(t)
	ctx := context.Background()
	d := testDataflowDefinition()
	d.Nodes = append(d.Nodes, testAgentTask("second"))
	join := testAgentTask("combine")
	join.Inputs = map[string]DataflowInputSource{"a": {Kind: "node_result", NodeID: "investigate"}, "b": {Kind: "node_result", NodeID: "second"}}
	d.Nodes = append(d.Nodes, join)
	if _, err := r.Create(ctx, "create", d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(ctx, d.GraphID, "start", 1); err != nil {
		t.Fatal(err)
	}
	if len(b.published) != 2 {
		t.Fatal("只应派发两个输入就绪任务")
	}
	settleDataflowTestNode(t, r, d.GraphID, "investigate", "")
	if err := r.Step(ctx, d.GraphID); err != nil {
		t.Fatal(err)
	}
	if len(b.published) != 2 {
		t.Fatal("缺少第二输入时不能执行汇总")
	}
	settleDataflowTestNode(t, r, d.GraphID, "second", "")
	if err := r.Step(ctx, d.GraphID); err != nil {
		t.Fatal(err)
	}
	if len(b.published) != 3 {
		t.Fatal("两输入齐备必须派发普通任务，无需 join 类型")
	}
	settleDataflowTestNode(t, r, d.GraphID, "combine", "")
	if err := r.Step(ctx, d.GraphID); err != nil {
		t.Fatal(err)
	}
	s, _, _ := r.Store.Get(d.GraphID)
	last := s.PlanningEvents[len(s.PlanningEvents)-1].Sequence
	if err := r.AcknowledgePlanning(d.GraphID, last); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := r.Step(ctx, d.GraphID); err != nil {
			t.Fatal(err)
		}
	}
	s, _, _ = r.Store.Get(d.GraphID)
	if len(s.PlanningEvents) != 0 {
		t.Fatal("没有新事实时不得不断重新调用 Scheduler")
	}
}

func TestDataflowCancelBeforeClaimWaitsForTerminalReceipt(t *testing.T) {
	r, _, _ := newDataflowTestRuntime(t)
	ctx := context.Background()
	d := testDataflowDefinition()
	_, _ = r.Create(ctx, "create", d)
	s, err := r.Start(ctx, d.GraphID, "start", 1)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions["investigate"]
	s, err = r.Cancel(ctx, d.GraphID, "cancel", "用户取消")
	if err != nil || s.Status != "cancelling" {
		t.Fatalf("接受取消不等于任务已结算: %v", err)
	}
	s, err = r.RecordTerminal(ctx, d.GraphID, "investigate", AgentTaskTerminal{TaskID: e.TaskID, OutcomeRef: "outcome:cancelled-before-claim", Status: "cancelled", Error: "未开始执行"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != "cancelled" || s.Completion.Status != "committed" {
		t.Fatal("收到真实取消回执后应结束图")
	}
	if _, err = r.Cancel(ctx, d.GraphID, "cancel", "用户取消"); err != nil {
		t.Fatal("重复取消必须幂等", err)
	}
}

func TestDataflowFailureCanFeedNewDiagnosticTaskWithoutPretendingSuccess(t *testing.T) {
	r, b, _ := newDataflowTestRuntime(t)
	ctx := context.Background()
	d := testDataflowDefinition()
	_, _ = r.Create(ctx, "create", d)
	s, err := r.Start(ctx, d.GraphID, "start", 1)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Executions["investigate"]
	s, err = r.RecordTerminal(ctx, d.GraphID, "investigate", AgentTaskTerminal{TaskID: e.TaskID, AttemptID: e.TaskID + "/attempt-1", OutcomeRef: "outcome:failed", Status: "failed", Error: "调查命令失败"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Results) != 0 {
		t.Fatal("失败不可伪装成成功结果")
	}
	next := testAgentTask("diagnose")
	next.Inputs = map[string]DataflowInputSource{"failure": {Kind: "node_outcome", NodeID: "investigate"}}
	if _, err := r.Apply(ctx, d.GraphID, DataflowChange{RequestID: "diagnose", ExpectedRevision: 1, Add: []AgentTaskNode{next}}); err != nil {
		t.Fatal(err)
	}
	if len(b.published) != 2 {
		t.Fatal("诊断任务应消费明确失败事实后执行")
	}
	s, _, _ = r.Store.Get(d.GraphID)
	fact := s.Executions["diagnose"].Inputs.Values["failure"]
	if fact.Ref != "outcome:failed" || fact.Value.(map[string]any)["status"] != "failed" {
		t.Fatal("诊断输入丢失权威身份")
	}
}
