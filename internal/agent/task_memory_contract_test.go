// task_memory_contract_test.go 覆盖 2026-08-20 SWE-001 预防 2：收口契约由
// 系统从持久化任务事实派生写入 TaskMemory.Constraints（渲染时每轮注入，
// 历史压缩碰不到），模型不可改写。
package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/model"
)

func TestTaskMemInitialConstraintsGraphNodeClosingContract(t *testing.T) {
	// 图节点任务：钉入 submit_task_result 收口契约
	graphTask := &model.Task{
		Description: "实现", GraphID: "g-1", NodeID: "impl", ActivationID: "impl@1", GraphNodeKind: "agent",
	}
	joined := strings.Join(taskMemInitialConstraints(graphTask), "\n")
	if !strings.Contains(joined, "submit_task_result") || !strings.Contains(joined, "纯文本回复不会被接受") {
		t.Errorf("图节点任务应钉入收口契约: %q", joined)
	}
	if strings.Contains(joined, "pass/fixable/failed") {
		t.Errorf("非 acceptance 节点不应有验收契约: %q", joined)
	}

	// acceptance 节点：追加 verdict 契约
	accTask := &model.Task{
		Description: "验收", GraphID: "g-1", NodeID: "verify", ActivationID: "verify@1", GraphNodeKind: "acceptance",
	}
	joined = strings.Join(taskMemInitialConstraints(accTask), "\n")
	if !strings.Contains(joined, "pass/fixable/failed") || !strings.Contains(joined, "省略 event") {
		t.Errorf("acceptance 节点应钉入 verdict 契约: %q", joined)
	}

	// 非图任务：无收口契约（直答/legacy 路径不受影响）
	plain := taskMemInitialConstraints(&model.Task{Description: "普通任务"})
	for _, c := range plain {
		if strings.Contains(c, "收口契约") {
			t.Errorf("非图任务不应有收口契约: %v", plain)
		}
	}

	// 既有约束（capability/预期产物）保持不丢
	capTask := &model.Task{
		Description: "x", GraphID: "g-1",
		Capability:        &model.NodeCapability{Tools: []string{"read_file"}},
		ExpectedArtifacts: []string{"out.md"},
	}
	joined = strings.Join(taskMemInitialConstraints(capTask), "\n")
	if !strings.Contains(joined, "工具子集") || !strings.Contains(joined, "预期产物") || !strings.Contains(joined, "收口契约") {
		t.Errorf("三类约束应共存: %q", joined)
	}
}

// 终态契约 v2 §5：图节点任务描述尾部的 <output-contract> 定界块逐行钉入
// Constraints（与既有收口/验收契约并存）；严格按定界标记解析，v1 图与非
// 图节点任务无此块自然跳过。
func TestTaskMemInitialConstraintsOutputContract(t *testing.T) {
	// 用真实派生渲染构造契约块，与发布侧（graph.RenderOutputContract）同形。
	block := graph.RenderOutputContract([]graph.Transition{
		{To: "review", When: &graph.Condition{Path: "$.coverage", Operator: graph.OpIn, Value: json.RawMessage(`["ok","gap"]`)}},
		{To: "gapfix", When: &graph.Condition{Path: "$.coverage", Operator: graph.OpEq, Value: json.RawMessage(`"gap"`)}},
		{To: "fix", When: &graph.Condition{Event: graph.EventFailed}},
	})

	// 图节点任务：定界块逐行钉入，与收口契约并存。
	graphTask := &model.Task{
		Description: "实现功能\n\n实现请求的功能\n\n" + block,
		GraphID:     "g-1", NodeID: "impl", ActivationID: "impl@1", GraphNodeKind: "agent",
	}
	constraints := taskMemInitialConstraints(graphTask)
	joined := strings.Join(constraints, "\n")
	for _, want := range []string{
		"收口契约",
		"输出契约: coverage ∈ {gap, ok}",
		"输出契约: summary 由系统参数承载，不在 result 内重复；禁止提交 event。",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("图节点任务应钉入 %q，实际：\n%s", want, joined)
		}
	}
	// 定界标记本身不得进入约束行。
	for _, c := range constraints {
		if strings.Contains(c, "output-contract>") {
			t.Errorf("约束行不得携带定界标记: %q", c)
		}
	}

	// acceptance 节点：verdict 契约与 $.verdict 出边派生的输出契约并存不冲突。
	verdictBlock := graph.RenderOutputContract([]graph.Transition{
		{To: "done", When: &graph.Condition{Path: "$.verdict", Operator: graph.OpEq, Value: json.RawMessage(`"pass"`)}},
		{To: "rework", When: &graph.Condition{Path: "$.verdict", Operator: graph.OpEq, Value: json.RawMessage(`"fixable"`)}},
		{To: "fixfail", When: &graph.Condition{Event: graph.EventFailed}},
	})
	accTask := &model.Task{
		Description: "验收实现\n\n" + verdictBlock,
		GraphID:     "g-1", NodeID: "verify", ActivationID: "verify@1", GraphNodeKind: "acceptance",
	}
	joined = strings.Join(taskMemInitialConstraints(accTask), "\n")
	if !strings.Contains(joined, "验收契约: verdict 只填 pass/fixable/failed") {
		t.Errorf("acceptance 应保留 verdict 契约: %s", joined)
	}
	if !strings.Contains(joined, "输出契约: verdict ∈ {pass, fixable}") {
		t.Errorf("acceptance 的 $.verdict 出边应派生并钉入输出契约: %s", joined)
	}

	// 无定界块（v1 图/普通图节点任务）：无输出契约约束，既有行为不变。
	plainGraph := &model.Task{Description: "实现", GraphID: "g-1", GraphNodeKind: "agent"}
	for _, c := range taskMemInitialConstraints(plainGraph) {
		if strings.Contains(c, "输出契约") {
			t.Errorf("无定界块的图任务不得有输出契约约束: %q", c)
		}
	}

	// 非图任务即使描述含块也不提取（钉入只发生在图节点任务上）。
	nonGraph := &model.Task{Description: "普通任务\n\n" + block}
	for _, c := range taskMemInitialConstraints(nonGraph) {
		if strings.Contains(c, "输出契约") {
			t.Errorf("非图任务不得提取输出契约: %q", c)
		}
	}

	// 不完整块（有 Begin 无 End）：严格解析返回空，不子串猜测。
	broken := &model.Task{
		Description: "实现\n\n" + graph.OutputContractBegin + "\ncoverage ∈ {gap}\n（截断）",
		GraphID:     "g-1", GraphNodeKind: "agent",
	}
	for _, c := range taskMemInitialConstraints(broken) {
		if strings.Contains(c, "输出契约") {
			t.Errorf("不完整块不得提取输出契约: %q", c)
		}
	}
}
