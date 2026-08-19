package graph

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// exampleDocJSON 是根节点循环修复图的初始提交形态。root 的初始
// activation 不算入边，verify → root 是它唯一的静态生产边。
const exampleDocJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "graph-123",
  "revision": 1,
  "state_version": 0,
  "root": "root",
  "status": "pending",
  "nodes": {
    "root": {
      "kind": "controller",
      "task": { "title": "完成用户请求" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": [{ "to": "implement", "when": { "event": "ready" } }]
    },
    "implement": {
      "kind": "agent",
      "task": { "title": "实施修改" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": [{ "to": "verify", "when": { "event": "completed" } }]
    },
    "verify": {
      "kind": "agent",
      "task": { "title": "验证修改" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": [
        { "to": "finish", "when": { "path": "$.verdict", "operator": "eq", "value": "pass" } },
        { "to": "root", "activation": "new", "when": { "path": "$.verdict", "operator": "eq", "value": "fixable" } }
      ]
    },
    "finish": {
      "kind": "end",
      "task": { "title": "形成最终结果" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": []
    }
  }
}`

// tinyDocJSON 是最小合法图：a（agent）→ b（end）。
const tinyDocJSON = `{
  "schema": "agentgo.graph/v1",
  "graph_id": "g1",
  "revision": 0,
  "state_version": 0,
  "root": "a",
  "status": "pending",
  "nodes": {
    "a": {
      "kind": "agent",
      "task": { "title": "做 A" },
      "status": "inactive",
      "next": [{ "to": "b" }]
    },
    "b": {
      "kind": "end",
      "task": { "title": "结束" },
      "status": "inactive",
      "next": []
    }
  }
}`

// mustParse 解析并断言成功。
func mustParse(t *testing.T, data string) *GraphDocument {
	t.Helper()
	doc, err := ParseAndValidate([]byte(data))
	if err != nil {
		t.Fatalf("应解析成功: %v", err)
	}
	return doc
}

// mutate 在 src 上做一次唯一替换；old 必须恰好出现一次，否则测试资产本身有误。
func mutate(t *testing.T, src, old, new string) string {
	t.Helper()
	if n := strings.Count(src, old); n != 1 {
		t.Fatalf("测试资产错误：锚点 %q 在文档中出现 %d 次", old, n)
	}
	return strings.Replace(src, old, new, 1)
}

// assertInvalid 断言文档被拒绝，且错误为 *ValidationError、阶段与消息子串匹配。
func assertInvalid(t *testing.T, name, data, wantStage, wantSub string) {
	t.Helper()
	_, err := ParseAndValidate([]byte(data))
	if err == nil {
		t.Fatalf("%s: 应拒绝但通过了校验", name)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: 错误类型应为 *ValidationError，实际为 %T（%v）", name, err, err)
	}
	if ve.Stage != wantStage {
		t.Errorf("%s: 阶段应为 %q，实际为 %q（%v）", name, wantStage, ve.Stage, err)
	}
	if wantSub != "" && !strings.Contains(ve.Error(), wantSub) {
		t.Errorf("%s: 错误消息应包含 %q，实际为 %q", name, wantSub, ve.Error())
	}
}

// TestExampleDocumentParses §6 示例图（含回边）必须通过校验，且结构解码正确。
func TestExampleDocumentParses(t *testing.T) {
	doc := mustParse(t, exampleDocJSON)

	if doc.Schema != SchemaV1 {
		t.Errorf("schema 应为 %q，实际为 %q", SchemaV1, doc.Schema)
	}
	if doc.Root != "root" {
		t.Errorf("root 应为 %q，实际为 %q", "root", doc.Root)
	}
	if len(doc.Nodes) != 4 {
		t.Fatalf("节点数应为 4，实际为 %d", len(doc.Nodes))
	}

	verify, ok := doc.Nodes["verify"]
	if !ok {
		t.Fatalf("未找到 verify 节点")
	}
	if len(verify.Next) != 2 {
		t.Fatalf("verify 应有 2 条转移，实际为 %d", len(verify.Next))
	}
	back := verify.Next[1]
	if back.To != "root" || back.Activation != ActivationNew {
		t.Errorf("回边转移应为 root + activation=new，实际为 to=%q activation=%q", back.To, back.Activation)
	}
	if back.When == nil || back.When.Path != "$.verdict" || back.When.Operator != OpEq {
		t.Errorf("回边条件形态解码错误: %+v", back.When)
	}
	if string(back.When.Value) != `"fixable"` {
		t.Errorf("回边条件 value 应为 \"fixable\"，实际为 %s", back.When.Value)
	}

	root := doc.Nodes["root"]
	if root.Executor != nil {
		t.Errorf("初始提交的 root.executor 应为 nil: %+v", root.Executor)
	}
	if root.Execution != nil {
		t.Errorf("初始提交的 root.execution 应为 nil: %+v", root.Execution)
	}
	if doc.Nodes["implement"].Executor != nil || doc.Nodes["implement"].Execution != nil {
		t.Errorf("implement 的 executor/execution 应为 nil")
	}
}

// TestBackEdgeAccepted 明确断言含环（回边）的图不被拒绝：b → a 构成环。
func TestBackEdgeAccepted(t *testing.T) {
	cyclic := `{
      "schema": "agentgo.graph/v1",
      "graph_id": "g-cycle",
      "revision": 0,
      "state_version": 0,
      "root": "a",
      "status": "pending",
      "nodes": {
        "a": { "kind": "agent", "task": { "title": "节点A" }, "status": "inactive",
               "next": [{ "to": "b" }] },
        "b": { "kind": "agent", "task": { "title": "节点B" }, "status": "inactive",
               "next": [
                 { "to": "a", "activation": "new", "when": { "event": "fixable" } },
                 { "to": "c", "when": { "event": "pass" } }
               ] },
        "c": { "kind": "end", "task": { "title": "C" }, "status": "inactive", "next": [] }
      }
    }`
	doc := mustParse(t, cyclic)
	if len(doc.Nodes["b"].Next) != 2 || doc.Nodes["b"].Next[0].To != "a" {
		t.Errorf("回边 b → a 应保留在转移表中: %+v", doc.Nodes["b"].Next)
	}
}

// TestValidConditionForms 两种合法 when 形态与无条件转移都通过校验。
func TestValidConditionForms(t *testing.T) {
	cases := map[string]string{
		"事件形态_always":  `{ "to": "b", "when": { "event": "always" } }`,
		"条件形态_in字符串列表": `{ "to": "b", "when": { "path": "$.verdict", "operator": "in", "value": ["pass", "fixable"] } }`,
		"条件形态_in空列表":   `{ "to": "b", "when": { "path": "$.verdict", "operator": "in", "value": [] } }`,
		"条件形态_exists":  `{ "to": "b", "when": { "path": "$.verdict", "operator": "exists" } }`,
		"条件形态_eq数字":    `{ "to": "b", "when": { "path": "$.score", "operator": "eq", "value": 100 } }`,
		"条件形态_ne布尔":    `{ "to": "b", "when": { "path": "$.ok", "operator": "ne", "value": false } }`,
		"条件形态_eq_null": `{ "to": "b", "when": { "path": "$.err", "operator": "eq", "value": null } }`,
		"无条件":          `{ "to": "b" }`,
	}
	for name, transition := range cases {
		doc := mutate(t, tinyDocJSON, `{ "to": "b" }`, transition)
		if _, err := ParseAndValidate([]byte(doc)); err != nil {
			t.Errorf("%s: 应通过校验: %v", name, err)
		}
	}
}

// TestJoinOutgoingEventContract 锁定 barrier 的事件边界：上游 event 只负责
// 选中入边，join 自身不会透传它；join 成功出边只能依赖 completed/always，
// 或显式检查按目标输入端口归并的 Result 路径。
func TestJoinOutgoingEventContract(t *testing.T) {
	const template = `{
      "schema":"agentgo.graph/v1","graph_id":"g-join-authoring","revision":0,"state_version":0,
      "root":"work","status":"pending","nodes":{
        "work":{"kind":"agent","task":{"title":"调查"},"status":"inactive",
          "next":[{"to":"join","when":{"event":"ready"}}]},
        "join":{"kind":"join","task":{"title":"汇合"},"status":"inactive",
          "next":[{"to":"done","when":JOIN_WHEN}]},
        "done":{"kind":"end","task":{"title":"结束"},"status":"inactive","next":[]}
      }
    }`
	build := func(when string) string {
		return strings.Replace(template, "JOIN_WHEN", when, 1)
	}

	for name, when := range map[string]string{
		"completed": `{"event":"completed"}`,
		"always":    `{"event":"always"}`,
		"结果路径":      `{"path":"$.work.event","operator":"eq","value":"ready"}`,
	} {
		if _, err := ParseAndValidate([]byte(build(when))); err != nil {
			t.Errorf("%s: join 合法出边应通过校验: %v", name, err)
		}
	}

	for _, event := range []string{EventReady, EventPass, EventFailed, EventBlocked, EventTimeout} {
		_, err := ParseAndValidate([]byte(build(`{"event":"` + event + `"}`)))
		if err == nil {
			t.Errorf("join event=%q 永远不可匹配，应在提交前拒绝", event)
			continue
		}
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Stage != "转移" || !strings.Contains(err.Error(), "不透传上游 event") {
			t.Errorf("join event=%q 应返回明确的 authoring 诊断，实际: %v", event, err)
		}
	}
}

// TestAcceptanceAuthoringContract 验收判据和路由在建图时即 fail-closed：
// completed 业务结论只能是 $.verdict eq 三值，Runtime 失败另走
// failed/blocked 事件兜底。
func TestAcceptanceAuthoringContract(t *testing.T) {
	const template = `{
	  "schema":"agentgo.graph/v1","graph_id":"g-acceptance-authoring","revision":1,"state_version":0,
	  "root":"verify","status":"pending","nodes":{
	    "verify":{"kind":"acceptance","task":{"title":"验收","description":"必须通过指定检查"},"status":"inactive","next":[TRANSITION]},
	    "done":{"kind":"end","task":{"title":"收官"},"status":"inactive","next":[]}
	  }
	}`
	build := func(transition string) string {
		return strings.Replace(template, "TRANSITION", transition, 1)
	}
	for _, verdict := range []string{"pass", "fixable", "failed"} {
		transition := `{"to":"done","when":{"path":"$.verdict","operator":"eq","value":"` + verdict + `"}}`
		if _, err := ParseAndValidate([]byte(build(transition))); err != nil {
			t.Errorf("verdict=%s 应是合法业务路由: %v", verdict, err)
		}
	}
	for _, event := range []string{EventFailed, EventBlocked} {
		transition := `{"to":"done","when":{"event":"` + event + `"}}`
		if _, err := ParseAndValidate([]byte(build(transition))); err != nil {
			t.Errorf("Runtime event=%s 应是合法兜底路由: %v", event, err)
		}
	}
	invalid := map[string]string{
		"无条件":             `{"to":"done"}`,
		"event_pass":      `{"to":"done","when":{"event":"pass"}}`,
		"event_fixable":   `{"to":"done","when":{"event":"fixable"}}`,
		"event_completed": `{"to":"done","when":{"event":"completed"}}`,
		"event_always":    `{"to":"done","when":{"event":"always"}}`,
		"verdict_in":      `{"to":"done","when":{"path":"$.verdict","operator":"in","value":["pass","failed"]}}`,
		"legacy_fail":     `{"to":"done","when":{"path":"$.verdict","operator":"eq","value":"fail"}}`,
		"verdict_blocked": `{"to":"done","when":{"path":"$.verdict","operator":"eq","value":"blocked"}}`,
		"其它路径":            `{"to":"done","when":{"path":"$.ok","operator":"eq","value":true}}`,
	}
	for name, transition := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAndValidate([]byte(build(transition))); err == nil {
				t.Fatal("会绕过 verdict 权威的 acceptance 出边应被拒绝")
			}
		})
	}
}

func TestAcceptanceRequiresExplicitCriterion(t *testing.T) {
	const noTask = `{
	  "schema":"agentgo.graph/v1","graph_id":"g-acceptance-task","revision":1,"state_version":0,
	  "root":"verify","status":"pending","nodes":{
	    "verify":{"kind":"acceptance","status":"inactive","next":[{"to":"done","when":{"path":"$.verdict","operator":"eq","value":"pass"}}]},
	    "done":{"kind":"end","task":{"title":"收官"},"status":"inactive","next":[]}
	  }
	}`
	assertInvalid(t, "acceptance 无 task", noTask, "节点", "必须携带 task")
	blankDescription := strings.Replace(noTask, `"kind":"acceptance",`,
		`"kind":"acceptance","task":{"title":"验收","description":"   "},`, 1)
	assertInvalid(t, "acceptance 无判据", blankDescription, "节点", "验收判据")
}

// TestRequiredInputsRejectExtraPort 显式端口表是完整输入契约：所有入边都
// 必须写入已声明端口。额外端口不会参与 barrier，却会预留新的 activation，
// 因此在 authoring 阶段 fail-closed；同一端口也只允许一个生产者。
func TestRequiredInputsRejectExtraPort(t *testing.T) {
	const invalid = `{
	  "schema":"agentgo.graph/v1","graph_id":"g-extra-port","revision":1,"state_version":0,
	  "root":"root","status":"pending","nodes":{
	    "root":{"kind":"router","task":{"title":"扇出"},"status":"inactive","next":[{"to":"a"},{"to":"b"}]},
	    "a":{"kind":"agent","task":{"title":"主来源"},"status":"inactive","next":[{"to":"join","target_input":"selected"}]},
	    "b":{"kind":"agent","task":{"title":"额外来源"},"status":"inactive","next":[{"to":"join","target_input":"optional"}]},
	    "join":{"kind":"join","task":{"title":"汇合","required_inputs":["selected"]},"status":"inactive","next":[{"to":"done"}]},
	    "done":{"kind":"end","task":{"title":"结束"},"status":"inactive","next":[]}
	  }
	}`
	_, err := ParseAndValidate([]byte(invalid))
	if err == nil || !strings.Contains(err.Error(), "未列入") {
		t.Fatalf("额外 target_input 应被明确拒绝，实际 %v", err)
	}

	shared := strings.Replace(invalid, `"target_input":"optional"`, `"target_input":"selected"`, 1)
	if _, err := ParseAndValidate([]byte(shared)); err == nil || !strings.Contains(err.Error(), "单赋值") {
		t.Fatalf("两个来源共享 required port 应被拒绝，实际 %v", err)
	}
}

// TestRejectMalformedInputs 输入边界 / 语法 / 类型 / 未知字段 / 重复键。
func TestRejectMalformedInputs(t *testing.T) {
	t.Run("语法错误_截断", func(t *testing.T) {
		assertInvalid(t, "截断", tinyDocJSON[:len(tinyDocJSON)/2], "JSON语法", "")
	})
	t.Run("语法错误_多余内容", func(t *testing.T) {
		assertInvalid(t, "多余内容", tinyDocJSON+" {}", "JSON语法", "多余内容")
	})
	t.Run("超大", func(t *testing.T) {
		big := strings.Repeat(" ", MaxDocumentBytes+1)
		assertInvalid(t, "超大", big, "输入边界", "超过上限")
	})
	t.Run("超深", func(t *testing.T) {
		deep := strings.Repeat("[", 40) + strings.Repeat("]", 40)
		doc := mutate(t, tinyDocJSON, `"next": []`, `"next": [], "extensions": { "x": `+deep+` }`)
		assertInvalid(t, "超深", doc, "输入边界", "嵌套深度")
	})
	t.Run("未知核心字段_节点层", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"kind": "agent"`, `"kind": "agent", "statsu": "inactive"`)
		assertInvalid(t, "未知字段", doc, "未知字段", `"statsu"`)
	})
	t.Run("未知核心字段_顶层", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"root": "a"`, `"root": "a", "mystery": 1`)
		assertInvalid(t, "未知字段", doc, "未知字段", `"mystery"`)
	})
	t.Run("重复键_顶层", func(t *testing.T) {
		doc := strings.Replace(tinyDocJSON, `"schema": "agentgo.graph/v1",`,
			`"schema": "agentgo.graph/v1", "schema": "agentgo.graph/v1",`, 1)
		assertInvalid(t, "重复键", doc, "重复键", `"schema"`)
	})
	t.Run("重复键_嵌套转移层", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `{ "to": "b" }`, `{ "to": "b", "to": "b" }`)
		assertInvalid(t, "重复键", doc, "重复键", `nodes.a.next[0] 的 "to" 重复出现`)
	})
	t.Run("类型错误_when为字符串", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `{ "to": "b" }`, `{ "to": "b", "when": "verdict == 'pass'" }`)
		assertInvalid(t, "脚本式条件", doc, "类型", "")
	})
}

// TestRejectBasicFields 阶段 4：schema / graph_id / 版本号 / 数量上限。
func TestRejectBasicFields(t *testing.T) {
	t.Run("schema错误", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"agentgo.graph/v1"`, `"agentgo.graph/v2"`)
		assertInvalid(t, "schema 错误", doc, "基本字段", "agentgo.graph/v1")
	})
	t.Run("schema缺失", func(t *testing.T) {
		doc := `{"graph_id":"g1","revision":0,"state_version":0,"root":"a","status":"pending",
          "nodes":{"a":{"kind":"end","status":"inactive","next":[]}}}`
		assertInvalid(t, "schema 缺失", doc, "基本字段", "agentgo.graph/v1")
	})
	t.Run("graph_id为空", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": ""`)
		assertInvalid(t, "graph_id 为空", doc, "基本字段", "不能为空")
	})
	t.Run("graph_id非法字符", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "bad id!"`)
		assertInvalid(t, "graph_id 非法字符", doc, "基本字段", "非法字符")
	})
	t.Run("节点ID非法字符", func(t *testing.T) {
		doc := `{"schema":"agentgo.graph/v1","graph_id":"g1","revision":0,"state_version":0,
          "root":"bad id","status":"pending",
          "nodes":{"bad id":{"kind":"end","status":"inactive","next":[]}}}`
		assertInvalid(t, "节点 ID 非法字符", doc, "基本字段", "非法字符")
	})
	t.Run("revision为负", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"revision": 0`, `"revision": -1`)
		assertInvalid(t, "revision 为负", doc, "基本字段", "非负")
	})
	t.Run("state_version为负", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"state_version": 0`, `"state_version": -2`)
		assertInvalid(t, "state_version 为负", doc, "基本字段", "非负")
	})
	t.Run("nodes为空", func(t *testing.T) {
		doc := `{"schema":"agentgo.graph/v1","graph_id":"g1","revision":0,"state_version":0,
          "root":"a","status":"pending","nodes":{}}`
		assertInvalid(t, "nodes 为空", doc, "基本字段", "不能为空")
	})
	t.Run("节点数超限", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`{"schema":"agentgo.graph/v1","graph_id":"g1","revision":0,"state_version":0,"root":"n0","status":"pending","nodes":{`)
		for i := 0; i <= MaxNodes; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"n` + strconv.Itoa(i) + `":{"kind":"end","status":"inactive","next":[]}`)
		}
		b.WriteString("}}")
		assertInvalid(t, "节点数超限", b.String(), "基本字段", "超过上限")
	})
	t.Run("单节点next超限", func(t *testing.T) {
		edges := strings.TrimSuffix(strings.Repeat(`{"to":"b"},`, MaxNextPerNode+1), ",")
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`, `"next": [`+edges+`]`)
		assertInvalid(t, "next 超限", doc, "基本字段", "超过上限")
	})
}

// TestRejectRootAndReferences 阶段 5/6/7：root、悬空引用、可达性。
func TestRejectRootAndReferences(t *testing.T) {
	t.Run("root为空", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"root": "a"`, `"root": ""`)
		assertInvalid(t, "root 为空", doc, "root", "不能为空")
	})
	t.Run("root悬空", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"root": "a"`, `"root": "ghost"`)
		assertInvalid(t, "root 悬空", doc, "root", "ghost")
	})
	t.Run("next悬空引用", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `{ "to": "b" }`, `{ "to": "ghost" }`)
		assertInvalid(t, "next 悬空", doc, "转移", "ghost")
	})
	t.Run("不可达节点", func(t *testing.T) {
		doc := `{
          "schema": "agentgo.graph/v1", "graph_id": "g1", "revision": 0, "state_version": 0,
          "root": "a", "status": "pending",
          "nodes": {
            "a": { "kind": "agent", "task": { "title": "节点A" }, "status": "inactive", "next": [{ "to": "b" }] },
            "b": { "kind": "end", "status": "inactive", "next": [] },
            "c": { "kind": "end", "status": "inactive", "next": [] }
          }
        }`
		assertInvalid(t, "不可达节点", doc, "可达性", `"c"`)
	})
}

// TestRejectConditions 阶段 6：脚本式条件、非法 operator、非法 activation 等。
func TestRejectConditions(t *testing.T) {
	wrap := func(when string) string {
		return mutate(t, tinyDocJSON, `{ "to": "b" }`, `{ "to": "b", "when": `+when+` }`)
	}
	t.Run("脚本式条件_函数字段", func(t *testing.T) {
		assertInvalid(t, "函数字段", wrap(`{ "fn": "eval(x)" }`), "未知字段", `"fn"`)
	})
	t.Run("脚本式条件_事件名实为表达式", func(t *testing.T) {
		assertInvalid(t, "表达式事件名", wrap(`{ "event": "verdict == 'pass'" }`), "转移", "未知")
	})
	t.Run("event与path同时出现", func(t *testing.T) {
		assertInvalid(t, "混合形态", wrap(`{ "event": "completed", "path": "$.x", "operator": "eq", "value": 1 }`), "转移", "不得同时出现")
	})
	t.Run("事件形态携带value", func(t *testing.T) {
		assertInvalid(t, "事件形态携带 value", wrap(`{ "event": "completed", "value": 1 }`), "转移", "不得携带")
	})
	t.Run("非法operator", func(t *testing.T) {
		assertInvalid(t, "非法 operator", wrap(`{ "path": "$.verdict", "operator": "contains", "value": "x" }`), "转移", "contains")
	})
	t.Run("eq缺value", func(t *testing.T) {
		assertInvalid(t, "eq 缺 value", wrap(`{ "path": "$.x", "operator": "eq" }`), "转移", "需要 value")
	})
	t.Run("eq的value为数组", func(t *testing.T) {
		assertInvalid(t, "eq 数组 value", wrap(`{ "path": "$.x", "operator": "eq", "value": [1, 2] }`), "转移", "标量")
	})
	t.Run("in的value为标量", func(t *testing.T) {
		assertInvalid(t, "in 标量 value", wrap(`{ "path": "$.x", "operator": "in", "value": "x" }`), "转移", "字符串数组")
	})
	t.Run("in的value为数字列表", func(t *testing.T) {
		assertInvalid(t, "in 数字列表", wrap(`{ "path": "$.x", "operator": "in", "value": [1, 2] }`), "转移", "字符串数组")
	})
	t.Run("exists携带value", func(t *testing.T) {
		assertInvalid(t, "exists 携带 value", wrap(`{ "path": "$.x", "operator": "exists", "value": true }`), "转移", "不得携带")
	})
	t.Run("path未以$.开头", func(t *testing.T) {
		assertInvalid(t, "path 形态错误", wrap(`{ "path": "verdict", "operator": "eq", "value": "pass" }`), "转移", `"$."`)
	})
	t.Run("when为空对象", func(t *testing.T) {
		assertInvalid(t, "when 为空", wrap(`{}`), "转移", "为空")
	})
	t.Run("非法activation", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `{ "to": "b" }`, `{ "to": "b", "activation": "reuse" }`)
		assertInvalid(t, "非法 activation", doc, "转移", "reuse")
	})
}

// TestRejectNodeSemantics 阶段 8：节点类型与 next 规则。
func TestRejectNodeSemantics(t *testing.T) {
	t.Run("未知节点类型", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"kind": "agent"`, `"kind": "magic"`)
		assertInvalid(t, "未知节点类型", doc, "节点", "magic")
	})
	t.Run("end带next", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": []`, `"next": [{ "to": "a" }]`)
		assertInvalid(t, "end 带 next", doc, "节点", "必须为空")
	})
	t.Run("非end无next", func(t *testing.T) {
		doc := `{"schema":"agentgo.graph/v1","graph_id":"g1","revision":0,"state_version":0,
          "root":"a","status":"pending",
          "nodes":{"a":{"kind":"agent","task":{"title":"solo"},"status":"inactive","next":[]}}}`
		assertInvalid(t, "非 end 无 next", doc, "节点", "不能为空")
	})
	t.Run("task标题为空", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `{ "title": "做 A" }`, `{ "title": "  " }`)
		assertInvalid(t, "task.title 为空", doc, "节点", "task.title")
	})
	t.Run("output_schema已删除按未知字段拒绝", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `{ "title": "做 A" }`,
			`{ "title": "做 A", "output_schema": "agentgo.result/v1" }`)
		assertInvalid(t, "output_schema 未知字段", doc, "未知字段", "output_schema")
	})
}

// TestRejectCapabilityShape 阶段 9：capability 与 executor 结构形状。
func TestRejectCapabilityShape(t *testing.T) {
	t.Run("isolation非法值", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`,
			`"capability": { "isolation": "docker" }, "next": [{ "to": "b" }]`)
		assertInvalid(t, "isolation 非法", doc, "能力", "docker")
	})
	t.Run("budget已删除按未知字段拒绝", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`,
			`"capability": { "budget": { "max_tokens": 5 } }, "next": [{ "to": "b" }]`)
		// capability.budget 占位字段已删除（从无 Runtime 消费者），
		// DisallowUnknownFields 应按未知字段拒绝，杜绝「预算已生效」虚假契约。
		assertInvalid(t, "budget 未知字段", doc, "未知字段", "budget")
	})
	t.Run("tools含空串", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`,
			`"capability": { "tools": ["read_file", ""] }, "next": [{ "to": "b" }]`)
		assertInvalid(t, "tools 含空串", doc, "能力", "空字符串")
	})
	t.Run("executor类型非法", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`,
			`"executor": { "type": "robot", "agent_id": "x" }, "next": [{ "to": "b" }]`)
		assertInvalid(t, "executor 类型非法", doc, "能力", "robot")
	})
	t.Run("executor缺agent_id", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`,
			`"executor": { "type": "agent" }, "next": [{ "to": "b" }]`)
		assertInvalid(t, "executor 缺 agent_id", doc, "能力", "agent_id")
	})
}

// TestRejectInitialStatus 阶段 10：初始状态合法性。
func TestRejectInitialStatus(t *testing.T) {
	t.Run("图状态为completed", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"status": "pending"`, `"status": "completed"`)
		assertInvalid(t, "图状态 completed", doc, "字段所有权", "completed")
	})
	t.Run("图状态非法", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"status": "pending"`, `"status": "bogus"`)
		assertInvalid(t, "图状态 bogus", doc, "初始状态", "bogus")
	})
	t.Run("节点状态非法", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"status": "inactive",
      "next": [{ "to": "b" }]`, `"status": "bogus",
      "next": [{ "to": "b" }]`)
		assertInvalid(t, "节点状态 bogus", doc, "初始状态", "bogus")
	})
}

// TestRejectSchedulerOwnedRuntimeFields 强制初始 JSON 的字段所有权：合法枚举
// 也不能被 Scheduler 用来伪造运行版本、认领者或 activation。
func TestRejectSchedulerOwnedRuntimeFields(t *testing.T) {
	t.Run("state_version", func(t *testing.T) {
		doc := strings.Replace(tinyDocJSON, `"state_version": 0`, `"state_version": 7`, 1)
		assertInvalid(t, "state_version", doc, "字段所有权", "state_version")
	})
	t.Run("node status", func(t *testing.T) {
		doc := strings.Replace(tinyDocJSON, `"status": "inactive"`, `"status": "completed"`, 1)
		assertInvalid(t, "node status", doc, "字段所有权", "completed")
	})
	t.Run("executor", func(t *testing.T) {
		doc := strings.Replace(tinyDocJSON, `"status": "inactive",`,
			`"status": "inactive", "executor": {"type":"agent","agent_id":"scheduler"},`, 1)
		assertInvalid(t, "executor", doc, "字段所有权", "executor")
	})
	t.Run("execution", func(t *testing.T) {
		doc := strings.Replace(tinyDocJSON, `"status": "inactive",`,
			`"status": "inactive", "execution": {"phase":"done","activation_id":"a@9"},`, 1)
		assertInvalid(t, "execution", doc, "字段所有权", "execution")
	})
}

// TestRejectPlaceholderNodeText 占位符防线（2026-08-19 SWE 占位图事故：
// title="t"/description="d" 的图通过校验并被真实派发执行）：
// controller/agent/acceptance 节点的 title/description 不得为单个 ASCII
// 字母/数字；单字中文（「源」）与正常文本不受影响。
func TestRejectPlaceholderNodeText(t *testing.T) {
	mkGraph := func(kind, title, desc string) string {
		task := `"task":{"title":"` + title + `"}`
		if desc != "" {
			task = `"task":{"title":"` + title + `","description":"` + desc + `"}`
		}
		return `{"schema":"agentgo.graph/v1","graph_id":"g-ph","revision":0,"state_version":0,
          "root":"a","status":"pending",
          "nodes":{"a":{"kind":"` + kind + `",` + task + `,"status":"inactive","next":[{"to":"z"}]},
                   "z":{"kind":"end","task":{"title":"收官"},"status":"inactive","next":[]}}}`
	}
	t.Run("title单ASCII占位符", func(t *testing.T) {
		assertInvalid(t, "title 占位符", mkGraph("agent", "t", ""), "节点", "疑似占位符")
	})
	t.Run("description单ASCII占位符", func(t *testing.T) {
		assertInvalid(t, "description 占位符", mkGraph("agent", "实现登录", "d"), "节点", "疑似占位符")
	})
	t.Run("controller的title占位符", func(t *testing.T) {
		assertInvalid(t, "controller title 占位符", mkGraph("controller", "x", ""), "节点", "疑似占位符")
	})
	t.Run("单字中文title合法", func(t *testing.T) {
		mustParse(t, mkGraph("agent", "源", ""))
	})
	t.Run("正常title与desc合法", func(t *testing.T) {
		mustParse(t, mkGraph("agent", "修复 IPv6 解析", "在 app.py 中拆分 host 与 port"))
	})
	t.Run("end节点title不受影响", func(t *testing.T) {
		mustParse(t, `{"schema":"agentgo.graph/v1","graph_id":"g-ph-end","revision":0,"state_version":0,
          "root":"a","status":"pending",
          "nodes":{"a":{"kind":"agent","task":{"title":"做事"},"status":"inactive","next":[{"to":"z"}]},
                   "z":{"kind":"end","task":{"title":"z"},"status":"inactive","next":[]}}}`)
	})
}

// TestRejectControllerCapabilityTools controller 是纯控制面，不得声明
// capability.tools（租约层强制置空，图提交期 fail-closed）。
func TestRejectControllerCapabilityTools(t *testing.T) {
	doc := `{"schema":"agentgo.graph/v1","graph_id":"g-ctl-cap","revision":0,"state_version":0,
      "root":"c","status":"pending",
      "nodes":{"c":{"kind":"controller","task":{"title":"汇总裁决"},"status":"inactive",
        "capability":{"tools":["read_file"]},"next":[{"to":"z"}]},
        "z":{"kind":"end","task":{"title":"收官"},"status":"inactive","next":[]}}}`
	assertInvalid(t, "controller capability.tools", doc, "能力", "不得声明 capability.tools")
	// agent 节点声明 capability.tools 不受影响
	agentDoc := strings.Replace(doc, `"kind":"controller"`, `"kind":"agent"`, 1)
	mustParse(t, agentDoc)
}
