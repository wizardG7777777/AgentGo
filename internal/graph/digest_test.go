package graph

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDigestStableAcrossStateChanges 状态字段变化不得改变 digest。
func TestDigestStableAcrossStateChanges(t *testing.T) {
	doc1 := mustParse(t, exampleDocJSON)
	doc2 := mustParse(t, exampleDocJSON)

	// 图级运行状态
	doc2.Status = GraphCompleted
	doc2.StateVersion = 999

	// 节点级运行状态与扩展字段
	root := doc2.Nodes["root"]
	root.Status = NodeFailed
	root.Executor = &Executor{Type: ExecutorTypeAgent, AgentID: "another-agent"}
	root.Execution = &Execution{
		Phase:        "done",
		TaskID:       "task-9",
		ActivationID: "root@3",
		ResultRef:    "result/root@3",
		EvidenceRefs: []string{"ev-1", "ev-2"},
	}
	root.Metadata = map[string]string{"label": "展示标签"}
	root.Extensions = map[string]json.RawMessage{"ext": json.RawMessage(`{"a":1}`)}
	doc2.Nodes["root"] = root

	verify := doc2.Nodes["verify"]
	verify.Status = NodeCompleted
	doc2.Nodes["verify"] = verify

	if d1, d2 := ComputeDigest(doc1), ComputeDigest(doc2); d1 != d2 {
		t.Errorf("状态刷新不得改变 digest:\n  d1=%s\n  d2=%s", d1, d2)
	}
}

// TestDigestChangesOnDefinitionChange 定义变化必须改变 digest。
func TestDigestChangesOnDefinitionChange(t *testing.T) {
	base := mustParse(t, exampleDocJSON)
	baseDigest := ComputeDigest(base)

	cases := map[string]func(doc *GraphDocument){
		"revision变化": func(doc *GraphDocument) { doc.Revision++ },
		"graph_id变化": func(doc *GraphDocument) { doc.GraphID = "graph-456" },
		"task变化": func(doc *GraphDocument) {
			n := doc.Nodes["implement"]
			n.Task = &NodeTask{Title: "实施修改 v2", Description: "返回结构化覆盖度字段"}
			doc.Nodes["implement"] = n
		},
		"capability变化": func(doc *GraphDocument) {
			n := doc.Nodes["implement"]
			n.Capability = &Capability{Tools: []string{"read_file", "edit_file"}, Isolation: IsolationWorkspace}
			doc.Nodes["implement"] = n
		},
		"next变化": func(doc *GraphDocument) {
			n := doc.Nodes["implement"]
			n.Next = append(n.Next, Transition{To: "finish", When: &Condition{Event: EventFailed}})
			doc.Nodes["implement"] = n
		},
		"条件value变化": func(doc *GraphDocument) {
			n := doc.Nodes["verify"]
			n.Next[0].When.Value = json.RawMessage(`"fail"`)
			doc.Nodes["verify"] = n
		},
		"kind变化": func(doc *GraphDocument) {
			n := doc.Nodes["verify"]
			n.Kind = KindAcceptance
			doc.Nodes["verify"] = n
		},
	}
	for name, mutateFn := range cases {
		doc := mustParse(t, exampleDocJSON)
		mutateFn(doc)
		if d := ComputeDigest(doc); d == baseDigest {
			t.Errorf("%s 应改变 digest", name)
		}
	}
}

// TestDigestMapOrderIndependent 同一语义文档，JSON key 顺序不同则 digest 相同。
func TestDigestMapOrderIndependent(t *testing.T) {
	// 与 tinyDocJSON 语义相同，但顶层 key、nodes 顺序、节点内字段顺序全部打乱
	reordered := `{
      "nodes": {
        "b": { "next": [], "status": "inactive", "task": { "title": "结束" }, "kind": "end" },
        "a": { "next": [{ "to": "b" }], "status": "inactive", "task": { "title": "做 A" }, "kind": "agent" }
      },
      "status": "pending",
      "root": "a",
      "state_version": 0,
      "revision": 0,
      "graph_id": "g1",
      "schema": "agentgo.graph/v1"
    }`
	doc1 := mustParse(t, tinyDocJSON)
	doc2 := mustParse(t, reordered)
	if d1, d2 := ComputeDigest(doc1), ComputeDigest(doc2); d1 != d2 {
		t.Errorf("map 序无关性失败:\n  d1=%s\n  d2=%s", d1, d2)
	}

	// 同一文档重复计算必须稳定
	if ComputeDigest(doc1) != ComputeDigest(doc1) {
		t.Errorf("同一文档的 digest 必须稳定")
	}
}

// TestDigestNormalization 空值与缺省归一：capability 空对象不改变 digest。
func TestDigestNormalization(t *testing.T) {
	doc1 := mustParse(t, tinyDocJSON)
	doc2 := mustParse(t, tinyDocJSON)
	a := doc2.Nodes["a"]
	a.Capability = &Capability{} // 全空 capability，与缺省等价
	doc2.Nodes["a"] = a
	if d1, d2 := ComputeDigest(doc1), ComputeDigest(doc2); d1 != d2 {
		t.Errorf("全空 capability 应与缺省同 digest:\n  d1=%s\n  d2=%s", d1, d2)
	}

	// 条件 value 的字面量格式差异（1e2 vs 100）不改变 digest
	doc3 := mustParse(t, tinyDocJSON)
	n := doc3.Nodes["a"]
	n.Next[0].When = &Condition{Path: "$.score", Operator: OpEq, Value: json.RawMessage(`100`)}
	doc3.Nodes["a"] = n
	doc4 := mustParse(t, tinyDocJSON)
	n4 := doc4.Nodes["a"]
	n4.Next[0].When = &Condition{Path: "$.score", Operator: OpEq, Value: json.RawMessage(` 1e2 `)}
	doc4.Nodes["a"] = n4
	if d3, d4 := ComputeDigest(doc3), ComputeDigest(doc4); d3 != d4 {
		t.Errorf("value 字面量格式差异不得改变 digest:\n  d3=%s\n  d4=%s", d3, d4)
	}
}

// TestDigestShape digest 是 64 位 hex；nil 文档返回空串。
func TestDigestShape(t *testing.T) {
	d := ComputeDigest(mustParse(t, exampleDocJSON))
	if len(d) != 64 {
		t.Errorf("digest 应为 64 位 hex，实际长度 %d", len(d))
	}
	for _, r := range d {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("digest 含非 hex 字符 %q", r)
		}
	}
	if got := ComputeDigest(nil); got != "" {
		t.Errorf("nil 文档的 digest 应为空串，实际为 %q", got)
	}
}

// TestDefinitionDigestCoversRootAndRoute 锁定新 GraphDefinition 摘要相对 legacy
// execution 摘要补齐的两个现存语义字段：root 与 metadata.route。
func TestDefinitionDigestCoversRootAndRoute(t *testing.T) {
	base := mustParse(t, exampleDocJSON)
	baseDigest := ComputeDefinitionDigest(base)

	rootChanged := mustParse(t, exampleDocJSON)
	rootChanged.Root = "implement"
	if got := ComputeDefinitionDigest(rootChanged); got == baseDigest {
		t.Fatal("GraphDefinition root 变化必须改变 definition digest")
	}

	routeChanged := mustParse(t, exampleDocJSON)
	root := routeChanged.Nodes["root"]
	root.Metadata = map[string]string{"route": "team:controller"}
	routeChanged.Nodes["root"] = root
	if got := ComputeDefinitionDigest(routeChanged); got == baseDigest {
		t.Fatal("metadata.route 变化必须改变 definition digest")
	}
}

// TestDefinitionDigestIgnoresRuntimeAndDisplayState 确认新摘要只覆盖 Definition：
// 运行状态、展示标签和未知 extension 均不改变 definition identity。
func TestDefinitionDigestIgnoresRuntimeAndDisplayState(t *testing.T) {
	doc1 := mustParse(t, exampleDocJSON)
	doc2 := mustParse(t, exampleDocJSON)
	doc2.Status = GraphCompleted
	doc2.StateVersion = 99

	root := doc2.Nodes["root"]
	root.Status = NodeFailed
	root.Executor = &Executor{Type: ExecutorTypeAgent, AgentID: "agent-x"}
	root.Execution = &Execution{Phase: "done", ActivationID: "root@9"}
	root.Metadata = map[string]string{"label": "仅展示"}
	root.Extensions = map[string]json.RawMessage{"future": json.RawMessage(`{"x":1}`)}
	doc2.Nodes["root"] = root

	if d1, d2 := ComputeDefinitionDigest(doc1), ComputeDefinitionDigest(doc2); d1 != d2 {
		t.Fatalf("运行/展示状态不得改变 definition digest:\n  d1=%s\n  d2=%s", d1, d2)
	}
}

// TestDefinitionDigestNormalizesRouteWhitespace route 的 Runtime 语义会先
// TrimSpace；摘要采用相同边界归一，避免仅首尾空白制造不同 Definition 身份。
func TestDefinitionDigestNormalizesRouteWhitespace(t *testing.T) {
	doc1 := mustParse(t, exampleDocJSON)
	doc2 := mustParse(t, exampleDocJSON)

	n1 := doc1.Nodes["implement"]
	n1.Metadata = map[string]string{"route": "team:impl"}
	doc1.Nodes["implement"] = n1
	n2 := doc2.Nodes["implement"]
	n2.Metadata = map[string]string{"route": "  team:impl\t"}
	doc2.Nodes["implement"] = n2

	if d1, d2 := ComputeDefinitionDigest(doc1), ComputeDefinitionDigest(doc2); d1 != d2 {
		t.Fatalf("route 首尾空白归一后应为同一 definition digest:\n  d1=%s\n  d2=%s", d1, d2)
	}
}

// TestLegacyDigestRemainsDefinitionBlind 把 legacy 行为锁住，防止后续为
// GraphDefinition 补字段时误改历史 journal/snapshot 摘要算法。
func TestLegacyDigestRemainsDefinitionBlind(t *testing.T) {
	base := mustParse(t, exampleDocJSON)
	changed := mustParse(t, exampleDocJSON)
	changed.Root = "implement"
	root := changed.Nodes["root"]
	root.Metadata = map[string]string{"route": "team:controller"}
	changed.Nodes["root"] = root

	if before, after := ComputeDigest(base), ComputeDigest(changed); before != after {
		t.Fatalf("legacy ComputeDigest 不得因新增 Definition 字段覆盖而改变:\n  before=%s\n  after=%s", before, after)
	}
	if ComputeDefinitionDigest(base) == ComputeDefinitionDigest(changed) {
		t.Fatal("新 definition digest 必须检测到 legacy 摘要刻意忽略的 root/route 变化")
	}
}

// TestDefinitionDigestShapeAndDomain 验证新摘要形状、nil 行为及版本化 domain
// 与 legacy 摘要隔离。
func TestDefinitionDigestShapeAndDomain(t *testing.T) {
	doc := mustParse(t, exampleDocJSON)
	got := ComputeDefinitionDigest(doc)
	if len(got) != 64 {
		t.Fatalf("definition digest 应为 64 位 hex，实际长度 %d", len(got))
	}
	for _, r := range got {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("definition digest 含非 hex 字符 %q", r)
		}
	}
	if got == ComputeDigest(doc) {
		t.Fatal("版本化 definition digest 必须与 legacy execution digest 使用不同 domain")
	}
	if nilDigest := ComputeDefinitionDigest(nil); nilDigest != "" {
		t.Fatalf("nil GraphDocument 的 definition digest 应为空，实际 %q", nilDigest)
	}
}
