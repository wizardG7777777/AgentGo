package graph

// 终态契约 v2 §7（D2）：patch_graph 的版本不交叉测试——patch 在变更后的
// 候选副本上重跑 authoring 校验链，校验规则按图自身冻结的 schema 版本分流：
// v1 存量图只能按 v1 规则 patch（业务事件边照旧合法，schema 不变）；
// v2 图按 v2 规则 patch（业务事件名与未声明字段的 path 条件一律拒绝，
// 且拒绝后图定义与 revision 保持不变）。
// 设计权威：docs/design/graph-terminal-contract-v2.md §7。

import (
	"encoding/json"
	"strings"
	"testing"
)

// v1 存量图 patch：upsert 业务事件边（approved/ready 属 v1 合法词表）按 v1
// 规则通过；patch 不改变图的 schema 版本（版本不交叉，v1 跑完即自然消亡）。
func TestPatchGraphV1KeepsV1Rules(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v1OutletGraphJSON) // revision=1，impl --ready--> done

	rev, err := rt.PatchGraph("g-v1-outlet", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{{
		ID: "impl", Kind: KindAgent,
		Task: &NodeTask{Title: "实现功能", Description: "实现请求的功能"},
		Next: []Transition{
			{To: "done", When: &Condition{Event: EventApproved}},
		},
	}}})
	if err != nil {
		t.Fatalf("v1 图 patch 业务事件边应按 v1 规则通过: %v", err)
	}
	if rev != 2 {
		t.Fatalf("patch 成功后 revision 应推进为 2，实际 %d", rev)
	}
	doc := mustGet(t, s, "g-v1-outlet")
	if doc.Schema != SchemaV1 {
		t.Fatalf("patch 不得改变图 schema 版本，实际 %q", doc.Schema)
	}
	impl := doc.Nodes["impl"]
	if len(impl.Next) != 1 || impl.Next[0].When == nil || impl.Next[0].When.Event != EventApproved {
		t.Fatalf("v1 patch 应整体替换出边定义: %+v", impl.Next)
	}
}

// v2 图 patch：业务事件名（ready）与未在 task.description 声明的 path
// 字段一律按 v2 规则拒绝；两次拒绝后图定义与 revision 保持原样（校验在
// 候选副本上进行，失败不落地）。
func TestPatchGraphV2EnforcesV2Rules(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletGraphJSON) // revision=1

	// 业务事件边：v2 词表收窄为 completed/failed/blocked/always。
	_, err := rt.PatchGraph("g-v2-outlet", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{{
		ID: "impl", Kind: KindAgent,
		Task: &NodeTask{Title: "实现功能", Description: "实现请求的功能；输出契约：result 必须包含 coverage"},
		Next: []Transition{
			{To: "done", When: &Condition{Event: EventReady}},
		},
	}}})
	if err == nil || !strings.Contains(err.Error(), "事件词表") {
		t.Fatalf("v2 图 patch 业务事件边应按事件词表拒绝，实际: %v", err)
	}

	// 未声明字段的 path 条件：description 只声明 coverage，边引用 stage。
	_, err = rt.PatchGraph("g-v2-outlet", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{{
		ID: "impl", Kind: KindAgent,
		Task: &NodeTask{Title: "实现功能", Description: "实现请求的功能；输出契约：result 必须包含 coverage"},
		Next: []Transition{
			{To: "done", When: &Condition{Path: "$.stage", Operator: OpEq, Value: json.RawMessage(`"built"`)}},
		},
	}}})
	if err == nil || !strings.Contains(err.Error(), "输出契约") {
		t.Fatalf("v2 图 patch 未声明字段的 path 边应按输出契约拒绝，实际: %v", err)
	}

	// 两次拒绝后图定义与 revision 保持原样。
	doc := mustGet(t, s, "g-v2-outlet")
	if doc.Revision != 1 {
		t.Fatalf("被拒绝的 patch 不得推进 revision，实际 %d", doc.Revision)
	}
	if doc.Schema != SchemaV2 {
		t.Fatalf("图 schema 应保持 v2，实际 %q", doc.Schema)
	}
	impl := doc.Nodes["impl"]
	if len(impl.Next) != 2 || impl.Next[0].When == nil || impl.Next[0].When.Path != "$.coverage" {
		t.Fatalf("被拒绝的 patch 不得改变既有出边定义: %+v", impl.Next)
	}
}
