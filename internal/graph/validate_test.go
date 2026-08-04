package graph

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// exampleDocJSON 是 docs/nextUpgrade-V6.md §6 第 4 条拓扑的初始提交形态
// （含 verify → implement 回边；runtime-owned 字段保持初始值）。
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
      "task": { "title": "实施修改", "output_schema": "agentgo.change-set/v1" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": [{ "to": "verify", "when": { "event": "completed" } }]
    },
    "verify": {
      "kind": "agent",
      "task": { "title": "验证修改", "output_schema": "agentgo.verification/v1" },
      "status": "inactive",
      "executor": null,
      "execution": null,
      "next": [
        { "to": "finish", "when": { "path": "$.verdict", "operator": "eq", "value": "pass" } },
        { "to": "implement", "activation": "new", "when": { "path": "$.verdict", "operator": "eq", "value": "fixable" } }
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
	if back.To != "implement" || back.Activation != ActivationNew {
		t.Errorf("回边转移应为 implement + activation=new，实际为 to=%q activation=%q", back.To, back.Activation)
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
        "a": { "kind": "agent", "task": { "title": "A" }, "status": "inactive",
               "next": [{ "to": "b" }] },
        "b": { "kind": "agent", "task": { "title": "B" }, "status": "inactive",
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
            "a": { "kind": "agent", "task": { "title": "A" }, "status": "inactive", "next": [{ "to": "b" }] },
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
}

// TestRejectCapabilityShape 阶段 9：capability 与 executor 结构形状。
func TestRejectCapabilityShape(t *testing.T) {
	t.Run("isolation非法值", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`,
			`"capability": { "isolation": "docker" }, "next": [{ "to": "b" }]`)
		assertInvalid(t, "isolation 非法", doc, "能力", "docker")
	})
	t.Run("budget负值", func(t *testing.T) {
		doc := mutate(t, tinyDocJSON, `"next": [{ "to": "b" }]`,
			`"capability": { "budget": { "max_tokens": -5 } }, "next": [{ "to": "b" }]`)
		assertInvalid(t, "budget 负值", doc, "能力", "非负有限")
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
