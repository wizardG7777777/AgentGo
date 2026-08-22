package graph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func outcomeGraphJSON(graphID string, outcome EndOutcome, legacy bool) string {
	endOutcome := fmt.Sprintf(`,"end_outcome":%q`, outcome)
	if legacy {
		endOutcome = ""
	}
	return fmt.Sprintf(`{
  "schema":"agentgo.graph/v1","graph_id":%q,"revision":1,"state_version":0,
  "root":"work","status":"pending","nodes":{
    "work":{"kind":"agent","task":{"title":"执行"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish"}]},
    "finish":{"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,
      "next":[]%s}
  }
}`, graphID, endOutcome)
}

func TestRuntimeEndOutcomeDurableRecovery(t *testing.T) {
	tests := []struct {
		name       string
		outcome    EndOutcome
		wantStatus GraphStatus
		legacy     bool
	}{
		{name: "success", outcome: EndSuccess, wantStatus: GraphCompleted},
		{name: "failed", outcome: EndFailed, wantStatus: GraphFailed},
		{name: "blocked", outcome: EndBlocked, wantStatus: GraphBlocked},
		{name: "cancelled", outcome: EndCancelled, wantStatus: GraphCancelled},
		{name: "legacy-empty-is-success", outcome: EndSuccess, wantStatus: GraphCompleted, legacy: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewStore(dir)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			graphID := "g-outcome-" + strings.ReplaceAll(test.name, "-is-", "-")
			runtime := NewRuntime(store, newFakeBoard())
			mustSubmitRuntime(t, runtime, outcomeGraphJSON(graphID, test.outcome, test.legacy))
			mustTerminal(t, runtime, TerminalFact{
				GraphID: graphID, NodeID: "work", ActivationID: "work@1",
				TaskID: "task-1", Status: NodeCompleted,
			})

			assertGraphOutcome(t, store, graphID, test.outcome, test.wantStatus)
			closeStore(t, store)

			recovered, err := NewStore(dir)
			if err != nil {
				t.Fatalf("NewStore(restart): %v", err)
			}
			t.Cleanup(func() { _ = recovered.Close() })
			if err := recovered.Recover(); err != nil {
				t.Fatalf("Recover 应恢复 typed outcome: %v", err)
			}
			assertGraphOutcome(t, recovered, graphID, test.outcome, test.wantStatus)
		})
	}
}

func assertGraphOutcome(t *testing.T, store *Store, graphID string, wantOutcome EndOutcome, wantStatus GraphStatus) {
	t.Helper()
	doc := mustGet(t, store, graphID)
	if doc.Status != wantStatus {
		t.Fatalf("图 %s status=%s，want %s", graphID, doc.Status, wantStatus)
	}
	if doc.Outcome == nil || doc.Outcome.Outcome != wantOutcome {
		t.Fatalf("图 %s outcome=%+v，want %s", graphID, doc.Outcome, wantOutcome)
	}
	if doc.Outcome.Source != "end" || doc.Outcome.EndNodeID == "" ||
		doc.Outcome.EndActivationID != doc.Outcome.EndNodeID+"@1" || doc.Outcome.ResultRef == "" ||
		doc.Outcome.DefinitionRevision != 1 || doc.Outcome.CommittedAt.IsZero() {
		t.Fatalf("图 %s 的 end outcome 谱系不完整: %+v", graphID, doc.Outcome)
	}
}

func TestValidateRuntimeStateOutcomeStatusCoherence(t *testing.T) {
	doc := mustParse(t, outcomeGraphJSON("g-outcome-coherence", EndSuccess, false))
	doc.Status = GraphCompleted
	doc.Outcome = &GraphOutcomeRecord{
		Outcome: EndFailed, Source: "runtime_failure", Reason: "故障",
		DefinitionRevision: 1, CommittedAt: time.Now().UTC(),
	}
	if err := validateRuntimeState(doc); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("status/outcome 不一致必须拒绝，实际 err=%v", err)
	}

	// 历史快照没有 GraphOutcomeRecord；既有 completed/failed/cancelled 仍须可读。
	doc.Outcome = nil
	doc.Status = GraphCompleted
	if err := validateRuntimeState(doc); err != nil {
		t.Fatalf("legacy empty outcome 应兼容: %v", err)
	}
}

const blockedSubgraphOutcomeJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-subgraph-blocked","revision":1,"state_version":0,
  "root":"nested","status":"pending","nodes":{
    "nested":{"kind":"subgraph","task":{"title":"执行子图"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{"root":"child_end","nodes":{
        "child_end":{"kind":"end","task":{"title":"子图阻塞收官"},"end_outcome":"blocked",
          "status":"inactive","executor":null,"execution":null,"next":[]}
      }},
      "next":[{"to":"parent_end","when":{"event":"blocked"}}]},
    "parent_end":{"kind":"end","task":{"title":"父图阻塞收官"},"end_outcome":"blocked",
      "status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func TestRuntimeSubgraphBlockedOutcomePropagation(t *testing.T) {
	store, runtime, _ := newTestRuntime(t)
	mustSubmitRuntime(t, runtime, blockedSubgraphOutcomeJSON)

	childID := "g-subgraph-blocked/nested@1"
	assertGraphOutcome(t, store, childID, EndBlocked, GraphBlocked)
	assertGraphOutcome(t, store, "g-subgraph-blocked", EndBlocked, GraphBlocked)

	parentNode := nodeOf(t, store, "g-subgraph-blocked", "nested")
	if parentNode.Status != NodeBlocked || parentNode.Execution == nil {
		t.Fatalf("blocked 子图必须结算为父节点 blocked: %+v", parentNode)
	}
	result := activationResultOf(t, store, "g-subgraph-blocked", parentNode.Execution.ResultRef)
	var value map[string]any
	if err := json.Unmarshal(result.Result, &value); err != nil {
		t.Fatalf("解析父节点 result: %v", err)
	}
	if value["child_status"] != "blocked" || value["child_outcome"] != "blocked" || value["event"] != EventBlocked {
		t.Fatalf("子图 typed outcome 未完整传播: %+v", value)
	}
}

const cancelledSubgraphOutcomeJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-subgraph-cancelled","revision":1,"state_version":0,
  "root":"nested","status":"pending","nodes":{
    "nested":{"kind":"subgraph","task":{"title":"执行子图"},"status":"inactive","executor":null,"execution":null,
      "subgraph":{"root":"child_work","nodes":{
        "child_work":{"kind":"agent","task":{"title":"子图工作"},"status":"inactive","executor":null,"execution":null,
          "next":[{"to":"child_end"}]},
        "child_end":{"kind":"end","task":{"title":"子图成功收官"},"end_outcome":"success",
          "status":"inactive","executor":null,"execution":null,"next":[]}
      }},
      "next":[{"to":"parent_end","when":{"path":"$.child_outcome","operator":"eq","value":"cancelled"}}]},
    "parent_end":{"kind":"end","task":{"title":"父图取消收官"},"end_outcome":"cancelled",
      "status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func TestRuntimeSubgraphCancelledOutcomeCanRouteParent(t *testing.T) {
	store, runtime, _ := newTestRuntime(t)
	mustSubmitRuntime(t, runtime, cancelledSubgraphOutcomeJSON)
	childID := "g-subgraph-cancelled/nested@1"
	if err := runtime.CancelGraphTree(childID, "外部取消子图"); err != nil {
		t.Fatalf("取消子图: %v", err)
	}
	assertNonEndOutcome(t, store, childID, EndCancelled, GraphCancelled, "control_plane")
	assertGraphOutcome(t, store, "g-subgraph-cancelled", EndCancelled, GraphCancelled)

	parentNode := nodeOf(t, store, "g-subgraph-cancelled", "nested")
	if parentNode.Status != NodeFailed || parentNode.Execution == nil {
		t.Fatalf("cancelled 子图必须以非 completed 父节点事实传播: %+v", parentNode)
	}
	result := activationResultOf(t, store, "g-subgraph-cancelled", parentNode.Execution.ResultRef)
	var value map[string]any
	if err := json.Unmarshal(result.Result, &value); err != nil {
		t.Fatalf("解析父节点 result: %v", err)
	}
	if value["child_status"] != "cancelled" || value["child_outcome"] != "cancelled" {
		t.Fatalf("cancelled 子图 outcome 未进入父图路由数据: %+v", value)
	}
}

func TestChildOutcomeFromStatusNeverCollapsesTerminalState(t *testing.T) {
	for status, want := range map[GraphStatus]EndOutcome{
		GraphCompleted: EndSuccess,
		GraphFailed:    EndFailed,
		GraphBlocked:   EndBlocked,
		GraphCancelled: EndCancelled,
	} {
		if got := childOutcomeFromStatus(status); got != want {
			t.Errorf("child status=%s 映射 outcome=%s，want %s", status, got, want)
		}
	}
}

const runtimeFailureOutcomeJSON = `{
  "schema":"agentgo.graph/v1","graph_id":"g-runtime-outcome","revision":1,"state_version":0,
  "root":"work","status":"pending","nodes":{
    "work":{"kind":"agent","task":{"title":"执行"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"finish","when":{"event":"completed"}}]},
    "finish":{"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

func TestRuntimeFailureAndControlCancellationWriteDurableOutcome(t *testing.T) {
	t.Run("runtime failure", func(t *testing.T) {
		store := newTestStore(t)
		runtime := NewRuntime(store, newFakeBoard())
		mustSubmitRuntime(t, runtime, runtimeFailureOutcomeJSON)
		err := runtime.OnTaskTerminal(TerminalFact{
			GraphID: "g-runtime-outcome", NodeID: "work", ActivationID: "work@1",
			TaskID: "task-1", Status: NodeFailed,
		})
		if err == nil || !strings.Contains(err.Error(), "无任何匹配的出路") {
			t.Fatalf("runtime failure 应保留无出路诊断，实际 %v", err)
		}
		assertNonEndOutcome(t, store, "g-runtime-outcome", EndFailed, GraphFailed, "runtime_failure")
		recovered := reopenStore(t, store)
		assertNonEndOutcome(t, recovered, "g-runtime-outcome", EndFailed, GraphFailed, "runtime_failure")
	})

	t.Run("control cancellation", func(t *testing.T) {
		store := newTestStore(t)
		runtime := NewRuntime(store, newFakeBoard())
		raw := strings.Replace(runtimeFailureOutcomeJSON, "g-runtime-outcome", "g-control-outcome", 1)
		mustSubmitRuntime(t, runtime, raw)
		if err := runtime.CancelGraphTree("g-control-outcome", "用户取消"); err != nil {
			t.Fatalf("CancelGraphTree: %v", err)
		}
		assertNonEndOutcome(t, store, "g-control-outcome", EndCancelled, GraphCancelled, "control_plane")
		recovered := reopenStore(t, store)
		assertNonEndOutcome(t, recovered, "g-control-outcome", EndCancelled, GraphCancelled, "control_plane")
	})
}

func TestStoreRejectsStatusOnlyBlockedTerminal(t *testing.T) {
	store := newTestStore(t)
	mustSubmit(t, store, runtimeFailureOutcomeJSON)
	doc := mustGet(t, store, "g-runtime-outcome")
	mustMutate(t, store.SetGraphStatus(doc.GraphID, GraphRunning, doc.StateVersion))
	doc = mustGet(t, store, doc.GraphID)
	if err := store.SetGraphStatus(doc.GraphID, GraphBlocked, doc.StateVersion); err == nil ||
		!strings.Contains(err.Error(), "CommitGraphOutcome") {
		t.Fatalf("status-only blocked 必须 fail-closed，实际 err=%v", err)
	}
}

func assertNonEndOutcome(t *testing.T, store *Store, graphID string, wantOutcome EndOutcome, wantStatus GraphStatus, wantSource string) {
	t.Helper()
	doc := mustGet(t, store, graphID)
	if doc.Status != wantStatus || doc.Outcome == nil || doc.Outcome.Outcome != wantOutcome ||
		doc.Outcome.Source != wantSource || strings.TrimSpace(doc.Outcome.Reason) == "" || doc.Outcome.CommittedAt.IsZero() {
		t.Fatalf("图 %s 的非 end outcome 不完整: status=%s outcome=%+v", graphID, doc.Status, doc.Outcome)
	}
}
