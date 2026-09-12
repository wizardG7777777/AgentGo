package graph

import (
	"encoding/json"
	"strings"
	"testing"
)

func testAgentTask(id string) AgentTaskNode {
	return AgentTaskNode{NodeID: id, Kind: AgentTaskKind, Title: id, Objective: "执行确定任务并提交结果", Execution: AgentTaskExecutionSpec{RouteRef: "default", Tools: []string{"read_file", "submit_task_result"}}, ResultSchema: map[string]any{"type": "object", "properties": map[string]any{"summary": map[string]any{"type": "string"}}, "required": []any{"summary"}}}
}

func testDataflowDefinition() DataflowDefinition {
	return DataflowDefinition{Schema: DataflowSchema, GraphID: "graph-dataflow", SessionID: "session-current", RunID: "run-current", Revision: 1, Objective: "先调查，再按结果扩展", Nodes: []AgentTaskNode{testAgentTask("investigate")}}
}

func TestDataflowAllowsInvestigationWithoutControlNodes(t *testing.T) {
	def := testDataflowDefinition()
	raw, _ := json.Marshal(def)
	decoded, err := DecodeDataflowDefinition(raw)
	if err != nil {
		t.Fatalf("单一调查节点应能定义，无需结束或验收: %v", err)
	}
	if len(decoded.Nodes) != 1 {
		t.Fatal("不应自动插入控制节点")
	}
	for _, legacy := range []string{"agent", "controller", "router", "tool", "approval", "acceptance", "subgraph", "join", "wait_event", "end"} {
		t.Run(legacy, func(t *testing.T) {
			d := testDataflowDefinition()
			d.Nodes[0].Kind = legacy
			if err := ValidateDataflowDefinition(d); err == nil {
				t.Fatal("不得兼容旧节点")
			}
		})
	}
	for _, field := range []string{`"root":"investigate"`, `"next":[]`, `"requires_acceptance":true`} {
		bad := append([]byte(`{`+field+`,`), raw[1:]...)
		if _, err := DecodeDataflowDefinition(bad); err == nil {
			t.Fatalf("旧控制字段应拒绝: %s", field)
		}
	}
}

func TestDataflowMultiInputAndNewIteration(t *testing.T) {
	def := testDataflowDefinition()
	b := testAgentTask("investigate-b")
	fix := testAgentTask("fix")
	fix.Inputs = map[string]DataflowInputSource{"first": {Kind: "node_result", NodeID: "investigate"}, "second": {Kind: "node_result", NodeID: "investigate-b"}}
	def.Nodes = append(def.Nodes, b, fix)
	if err := ValidateDataflowDefinition(def); err != nil {
		t.Fatalf("普通任务可直接消费两个输入: %v", err)
	}
	results := map[string]AgentTaskResult{}
	for _, id := range []string{"investigate", "investigate-b"} {
		results[id] = AgentTaskResult{Schema: AgentTaskResultSchema, GraphID: def.GraphID, RunID: def.RunID, NodeID: id, Ref: "result:" + id, Value: map[string]any{"summary": id}}
	}
	partial := map[string]AgentTaskResult{"investigate": results["investigate"]}
	if _, wait, err := ResolveDataflowInputs(def, "fix", partial, nil, nil, nil); err != nil || wait != "waiting_inputs:second" {
		t.Fatalf("缺少第二输入应等待: %s %v", wait, err)
	}
	frozen, wait, err := ResolveDataflowInputs(def, "fix", results, nil, nil, nil)
	if err != nil || wait != "" {
		t.Fatalf("齐备输入应就绪: %s %v", wait, err)
	}
	results["investigate"].Value["summary"] = "改写调用方数据"
	if frozen.Values["first"].Value.(map[string]any)["summary"] != "investigate" {
		t.Fatal("冻结输入被外部 map 改写")
	}
	check := testAgentTask("check")
	check.Inputs = map[string]DataflowInputSource{"candidate": {Kind: "node_result", NodeID: "fix"}}
	next := testAgentTask("fix-next")
	next.Inputs = map[string]DataflowInputSource{"feedback": {Kind: "node_result", NodeID: "check"}, "previous": {Kind: "node_result", NodeID: "fix"}}
	def.Nodes = append(def.Nodes, check, next)
	if err := ValidateDataflowDefinition(def); err != nil {
		t.Fatal(err)
	}
	def.Nodes[0].Inputs = map[string]DataflowInputSource{"loop": {Kind: "node_result", NodeID: "fix-next"}}
	if err := ValidateDataflowDefinition(def); err == nil {
		t.Fatal("不得恢复同节点控制回边")
	}
}

func TestDataflowCandidateSelectionAndScope(t *testing.T) {
	def := testDataflowDefinition()
	b := testAgentTask("other")
	verify := testAgentTask("verify")
	verify.Inputs = map[string]DataflowInputSource{"a": {Kind: "node_result", NodeID: "investigate"}, "b": {Kind: "node_result", NodeID: "other"}}
	def.Nodes = append(def.Nodes, b, verify)
	results := map[string]AgentTaskResult{}
	for _, id := range []string{"investigate", "other"} {
		results[id] = AgentTaskResult{Schema: AgentTaskResultSchema, GraphID: def.GraphID, RunID: def.RunID, NodeID: id, Ref: "result:" + id, CandidateRef: "candidate:" + id, Value: map[string]any{"summary": id}}
	}
	if _, _, err := ResolveDataflowInputs(def, "verify", results, nil, nil, nil); err == nil {
		t.Fatal("不可任取第一个候选")
	}
	other := results["other"]
	other.CandidateRef = results["investigate"].CandidateRef
	results["other"] = other
	inputs, _, err := ResolveDataflowInputs(def, "verify", results, nil, nil, nil)
	if err != nil || inputs.WorkspaceCandidateRef != "candidate:investigate" {
		t.Fatalf("普通检查任务应绑定明确候选: %+v %v", inputs, err)
	}
	foreign := results["other"]
	foreign.GraphID = "other-graph"
	results["other"] = foreign
	if _, _, err := ResolveDataflowInputs(def, "verify", results, nil, nil, nil); err == nil {
		t.Fatal("不得读其它图的结果")
	}
}

func TestDataflowExternalInputAndStrictResultSchema(t *testing.T) {
	def := testDataflowDefinition()
	def.Inputs = map[string]DataflowInputSpec{"question": {Schema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}}}}
	def.Nodes[0].Inputs = map[string]DataflowInputSource{"question": {Kind: "graph_input", Port: "question", Version: 1, Pointer: "/text"}}
	if err := ValidateDataflowDefinition(def); err != nil {
		t.Fatal(err)
	}
	if _, wait, err := ResolveDataflowInputs(def, "investigate", nil, nil, nil, nil); err != nil || wait == "" {
		t.Fatal("已声明未到达外部输入应等待")
	}
	ext := map[string]map[int64]DataflowInputValue{"question": {1: {Ref: "input:question/1", Value: map[string]any{"text": "调查多行\n输入"}}}}
	input, _, err := ResolveDataflowInputs(def, "investigate", nil, ext, nil, nil)
	if err != nil || !strings.Contains(input.Values["question"].Value.(string), "\n") {
		t.Fatalf("内容可含换行，身份与内容分开: %v", err)
	}
	ext["question"][1] = DataflowInputValue{Ref: "input:question/1", Value: map[string]any{"text": 42}}
	if _, _, err := ResolveDataflowInputs(def, "investigate", nil, ext, nil, nil); err == nil {
		t.Fatal("坏数据类型应拒绝")
	}
	def.Nodes[0].ResultSchema["magicIgnoreValidation"] = true
	if err := ValidateDataflowDefinition(def); err == nil {
		t.Fatal("不支持的 schema 不得静默放行")
	}
}

func TestDataflowDigestCoversExecutionAndResult(t *testing.T) {
	def := testDataflowDefinition()
	before, err := def.Digest()
	if err != nil {
		t.Fatal(err)
	}
	def.Nodes[0].Execution.Model = "explicit-model"
	after, err := def.Digest()
	if err != nil || before == after {
		t.Fatal("模型选择必须进入定义摘要")
	}
	before = after
	def.Nodes[0].ResultSchema["additionalProperties"] = false
	after, err = def.Digest()
	if err != nil || before == after {
		t.Fatal("结果契约必须进入定义摘要")
	}
}
