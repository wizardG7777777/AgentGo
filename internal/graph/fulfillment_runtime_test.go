package graph

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestCodeChangeCurrentVersionsMissingFulfillmentRouteBlocked(t *testing.T) {
	for _, progressRef := range []string{"progress:code-change/v5", "progress:code-change/v6"} {
		t.Run(progressRef, func(t *testing.T) {
			const template = `{
  "schema":"agentgo.graph/v2","graph_id":"g-fulfillment","revision":1,"state_version":0,
  "root":"work","status":"pending","nodes":{
    "work":{"kind":"agent","task":{"title":"实现","description":"修改并验证"},"status":"inactive",
      "progress_contract_ref":"%s","context_policy_ref":"context:default/v9",
      "output_contract":{"summary_required":true},
      "next":[{"to":"ok","when":{"event":"completed"}},{"to":"blocked","when":{"event":"blocked"}},{"to":"failed","when":{"event":"failed"}}]},
    "ok":{"kind":"end","task":{"title":"成功"},"end_outcome":"success","status":"inactive","next":[]},
    "blocked":{"kind":"end","task":{"title":"阻塞"},"end_outcome":"blocked","status":"inactive","next":[]},
    "failed":{"kind":"end","task":{"title":"失败"},"end_outcome":"failed","status":"inactive","next":[]}
  }} `
			doc := fmt.Sprintf(template, progressRef)
			s, rt, board := newTestRuntime(t)
			mustSubmitRuntime(t, rt, doc)
			taskID := board.byActivation["g-fulfillment\x00work@1"]
			mustTerminal(t, rt, TerminalFact{GraphID: "g-fulfillment", NodeID: "work", ActivationID: "work@1", TaskID: taskID, Status: NodeCompleted, Result: map[string]any{"claimed": true}})
			work := nodeOf(t, s, "g-fulfillment", "work")
			if work.Status != NodeBlocked {
				t.Fatalf("缺 fulfillment 的 completed 必须被 L5 降为 blocked: %+v", work)
			}
			result := activationResultOf(t, s, "g-fulfillment", work.Execution.ResultRef)
			var value map[string]any
			if json.Unmarshal(result.Result, &value) != nil || value["reason_code"] != "contract_fulfillment_missing" {
				t.Fatalf("缺少 reason_code: %+v", result)
			}
			assertGraphOutcome(t, s, "g-fulfillment", EndBlocked, GraphBlocked)
		})
	}
}
