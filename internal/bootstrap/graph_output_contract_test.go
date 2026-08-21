package bootstrap

// 终态契约 v2 §5：输出契约定界块随任务描述冻结的注入测试。
// 设计权威：docs/design/graph-terminal-contract-v2.md §5。

import (
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/store"
)

func testOutputContract(t *testing.T) string {
	t.Helper()
	contract := graph.RenderOutputContract([]graph.Transition{
		{To: "done", When: &graph.Condition{Path: "$.coverage", Operator: graph.OpIn, Value: json.RawMessage(`["ok","gap"]`)}},
		{To: "fix", When: &graph.Condition{Event: graph.EventFailed}},
	})
	if contract == "" {
		t.Fatal("测试前置：有 path 条件出边应渲染出契约块")
	}
	return contract
}

// 契约块追加在任务描述尾部：「## 上游输入」段保持完整且契约块在其后出现；
// 空契约（v1 图/无 path 条件节点）与无契约逐字节一致（零行为变化）。
func TestGraphTaskDescriptionAppendsOutputContract(t *testing.T) {
	contract := testOutputContract(t)
	spec := graph.TaskSpec{
		Title: "实现功能", Description: "实现请求的功能",
		OutputContract: contract,
		Inputs: []graph.InputBinding{{
			SourceNodeID: "probe", SourceActivationID: "probe@1",
			Summary: `{"note":"第一版"}`,
		}},
	}
	desc := graphTaskDescription(spec)
	if !strings.HasPrefix(desc, "实现功能\n\n实现请求的功能") {
		t.Errorf("描述应以原标题+描述开头: %q", desc)
	}
	upstream := strings.Index(desc, "## 上游输入")
	block := strings.Index(desc, graph.OutputContractBegin)
	if upstream < 0 {
		t.Fatalf("「## 上游输入」段应保留: %q", desc)
	}
	if block < 0 || block < upstream {
		t.Errorf("契约块应出现在「## 上游输入」段之后（upstream=%d block=%d）: %q", upstream, block, desc)
	}
	if !strings.HasSuffix(desc, graph.OutputContractEnd) {
		t.Errorf("契约块应为描述的最后一段: %q", desc)
	}
	if !strings.Contains(desc, "来自节点 probe（activation probe@1）") || !strings.Contains(desc, "coverage ∈ {ok, gap}") {
		t.Errorf("既有段落与契约内容应并存: %q", desc)
	}

	plain := spec
	plain.OutputContract = ""
	if got, want := graphTaskDescription(plain), graphTaskDescriptionWithInputs(spec); got != want {
		t.Errorf("空契约应与无契约逐字节一致（零行为变化）:\n got=%q\nwant=%q", got, want)
	}
}

// 发布链路：契约随任务描述冻结落盘，并能被 TaskMemory 钉入侧
// （graph.ExtractOutputContract）严格按定界标记逐行取回。
func TestGraphBoardPublishFreezesOutputContract(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	board := newGraphBoard(s)
	id, err := board.PublishGraphTask(graph.TaskSpec{
		GraphID: "g-contract", NodeID: "impl", ActivationID: "impl@1",
		NodeKind: graph.KindAgent, Title: "实现功能", Description: "实现请求的功能",
		OutputContract: testOutputContract(t),
	})
	if err != nil {
		t.Fatalf("PublishGraphTask: %v", err)
	}
	task, err := s.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if !strings.Contains(task.Description, graph.OutputContractBegin) {
		t.Fatalf("落盘任务描述应含契约定界块: %q", task.Description)
	}
	lines := graph.ExtractOutputContract(task.Description)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "coverage ∈ {ok, gap}") || !strings.Contains(joined, "禁止提交 event") {
		t.Errorf("钉入侧应能逐行取回契约内容，实际 %v", lines)
	}
}
