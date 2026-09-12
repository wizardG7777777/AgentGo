package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestWaitingFactsDoNotRepeatAfterAcknowledgementOrRestart(t *testing.T) {
	r, board, dir := newDataflowTestRuntime(t)
	ctx := context.Background()
	d := testDataflowDefinition()
	if _, err := r.Create(ctx, "create", d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(ctx, d.GraphID, "start", 1); err != nil {
		t.Fatal(err)
	}
	board.checksErr = fmt.Errorf("暂缺执行者")
	if _, err := r.Apply(ctx, d.GraphID, DataflowChange{RequestID: "waiting-nodes", ExpectedRevision: 1, Add: []AgentTaskNode{testAgentTask("later-a"), testAgentTask("later-b")}}); err != nil {
		t.Fatal(err)
	}
	s, _, _ := r.Store.Get(d.GraphID)
	if len(s.PlanningEvents) != 2 {
		t.Fatalf("每个等待节点只应发布一条事实: %+v", s.PlanningEvents)
	}
	if err := r.AcknowledgePlanning(d.GraphID, s.PlanningEvents[len(s.PlanningEvents)-1].Sequence); err != nil {
		t.Fatal(err)
	}
	s, _, _ = r.Store.Get(d.GraphID)
	before, err := os.ReadFile(r.Store.file(d.GraphID))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := r.Step(ctx, d.GraphID); err != nil {
			t.Fatal(err)
		}
	}
	got, _, _ := r.Store.Get(d.GraphID)
	after, _ := os.ReadFile(r.Store.file(d.GraphID))
	if got.StateVersion != s.StateVersion || string(before) != string(after) {
		t.Fatal("重复 Step 制造了新事件或日志写入")
	}
	reopened, err := NewDataflowStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	r = NewDataflowRuntime(reopened, board)
	if err := r.Step(ctx, d.GraphID); err != nil {
		t.Fatal(err)
	}
	got, _, _ = r.Store.Get(d.GraphID)
	if got.StateVersion != s.StateVersion {
		t.Fatal("重启后丢失等待事实去重状态")
	}
	board.checksErr = fmt.Errorf("执行权限已变化")
	if err := r.Step(ctx, d.GraphID); err != nil {
		t.Fatal(err)
	}
	got, _, _ = r.Store.Get(d.GraphID)
	if len(got.PlanningEvents) != len(s.PlanningEvents)+2 {
		t.Fatal("实际等待原因变化必须发布新事实")
	}
	board.checksErr = nil
	if err := r.Step(ctx, d.GraphID); err != nil {
		t.Fatal(err)
	}
	got, _, _ = r.Store.Get(d.GraphID)
	for _, id := range []string{"later-a", "later-b"} {
		if got.Executions[id].Status != "running" {
			t.Fatalf("执行者恢复后未正常派发 %s", id)
		}
	}
}

func TestRetiredWorkspaceInputRejected(t *testing.T) {
	d := testDataflowDefinition()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`""`, `"investigate"`} {
		bad := strings.Replace(string(raw), `"node_id":"investigate"`, `"node_id":"investigate","workspace_input":`+value, 1)
		if _, err := DecodeDataflowDefinition([]byte(bad)); err == nil {
			t.Fatal("退役 workspace_input 不得被接受或静默丢弃")
		}
	}
	d.Schema = "agentgo.graph/v6"
	if err := ValidateDataflowDefinition(d); err == nil {
		t.Fatal("旧图契约不能执行")
	}
}

type candidateResolverTestPort struct{ calls int }

func (p *candidateResolverTestPort) ResolveCandidateBaseline(_ context.Context, graphID, runID string, refs []string) (string, error) {
	p.calls++
	if graphID != "graph-dataflow" || runID != "run-current" || len(refs) != 2 {
		return "", fmt.Errorf("候选解析身份丢失")
	}
	return "candidate:b", nil
}

func TestRuntimeResolvesInputCandidateBeforeDispatch(t *testing.T) {
	r, board, _ := newDataflowTestRuntime(t)
	port := &candidateResolverTestPort{}
	r.Candidates = port
	d := testDataflowDefinition()
	d.Nodes = append(d.Nodes, testAgentTask("other"))
	ctx := context.Background()
	if _, err := r.Create(ctx, "create", d); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(ctx, d.GraphID, "start", 1); err != nil {
		t.Fatal(err)
	}
	settleDataflowTestNode(t, r, d.GraphID, "investigate", "candidate:a")
	settleDataflowTestNode(t, r, d.GraphID, "other", "candidate:b")
	node := testAgentTask("follow-up")
	node.Inputs = map[string]DataflowInputSource{"previous": {Kind: "node_result", NodeID: "investigate"}, "current": {Kind: "node_result", NodeID: "other"}}
	if _, err := r.Apply(ctx, d.GraphID, DataflowChange{RequestID: "next", ExpectedRevision: 1, Add: []AgentTaskNode{node}}); err != nil {
		t.Fatal(err)
	}
	state, _, _ := r.Store.Get(d.GraphID)
	dispatch := board.published[state.Executions[node.NodeID].TaskID]
	if port.calls != 1 || dispatch.Inputs.WorkspaceCandidateRef != "candidate:b" || dispatch.Inputs.Values["previous"].CandidateRef != "candidate:a" {
		t.Fatalf("没有冻结运行时选择的基线或丢失输入事实: %+v", dispatch)
	}
}
