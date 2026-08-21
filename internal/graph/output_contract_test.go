package graph

// 终态契约 v2 §5 输出契约机械派生、任务发布注入与钉入解析的测试。
// 设计权威：docs/design/graph-terminal-contract-v2.md §5。

import (
	"encoding/json"
	"strings"
	"testing"
)

// 多算子混合出边的派生形态：同一字段 eq/in 合并值域（eq 先于 in，与边
// 顺序无关）、exists 降级、ne 值域集合；系统事件边与无条件边不参与派生。
func TestRenderOutputContractMixedOperators(t *testing.T) {
	next := []Transition{
		// 刻意把 in 边放在 eq 边之前：合并仍须 eq 优先（{gap, ok} 而非 {ok, gap}）。
		{To: "review", When: &Condition{Path: "$.coverage", Operator: OpIn, Value: json.RawMessage(`["ok","gap"]`)}},
		{To: "gapfix", When: &Condition{Path: "$.coverage", Operator: OpEq, Value: json.RawMessage(`"gap"`)}},
		{To: "withdetail", When: &Condition{Path: "$.detail", Operator: OpExists}},
		{To: "polish", When: &Condition{Path: "$.note", Operator: OpNe, Value: json.RawMessage(`"draft"`)}},
		{To: "leveled", When: &Condition{Path: "$.level", Operator: OpEq, Value: json.RawMessage(`2`)}},
		// exists > ne：同字段两条件时按 exists 渲染。
		{To: "flagoff", When: &Condition{Path: "$.flag", Operator: OpNe, Value: json.RawMessage(`"off"`)}},
		{To: "flagon", When: &Condition{Path: "$.flag", Operator: OpExists}},
		// 纯系统事件边与无条件边不参与派生。
		{To: "retry", When: &Condition{Event: EventFailed}},
		{To: "always"},
	}
	block := RenderOutputContract(next)
	if block == "" {
		t.Fatal("有 path 条件出边应渲染契约块")
	}
	if !strings.HasPrefix(block, OutputContractBegin) || !strings.HasSuffix(block, OutputContractEnd) {
		t.Errorf("契约块应以定界标记首尾包裹: %q", block)
	}
	for _, want := range []string{
		"coverage ∈ {gap, ok}",
		"detail 必须存在",
		"flag 必须存在",
		"level ∈ {2}",
		"note 必须不等于 {draft}（或字段缺失）",
		"summary 由系统参数承载，不在 result 内重复；禁止提交 event。",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("契约块应包含 %q，实际：\n%s", want, block)
		}
	}
	// 字段按路径排序逐行列出（coverage < detail < flag < level < note）。
	order := []string{"coverage ∈", "detail 必须存在", "flag 必须存在", "level ∈", "note 必须不等于"}
	prev := -1
	for _, marker := range order {
		idx := strings.Index(block, marker)
		if idx < 0 || idx < prev {
			t.Fatalf("字段行应按路径排序（%q 位置 %d，前一位置 %d）:\n%s", marker, idx, prev, block)
		}
		prev = idx
	}
	// 路径的 $. 前缀在契约行中剥离；事件边/无条件边不产生任何字段行。
	for _, unwanted := range []string{"$.coverage", "$.flag", "failed", "always ∈", "retry"} {
		if strings.Contains(block, unwanted) {
			t.Errorf("契约块不应包含 %q:\n%s", unwanted, block)
		}
	}
}

// 无条件边/纯系统事件边 → 不注入（返回空串）。
func TestRenderOutputContractNoPathConditions(t *testing.T) {
	for name, next := range map[string][]Transition{
		"空出边":   nil,
		"仅无条件边": {{To: "done"}},
		"仅系统事件边": {
			{To: "done", When: &Condition{Event: EventCompleted}},
			{To: "fix", When: &Condition{Event: EventFailed}},
			{To: "any", When: &Condition{Event: EventAlways}},
		},
	} {
		if got := RenderOutputContract(next); got != "" {
			t.Errorf("%s：无 path 条件不应渲染契约块，实际 %q", name, got)
		}
	}
}

// ExtractOutputContract 严格按定界标记解析：有块逐行返回；无块、子串提及、
// 块不完整（有 Begin 无 End）一律返回 nil，不做子串猜测。
func TestExtractOutputContract(t *testing.T) {
	next := []Transition{
		{To: "done", When: &Condition{Path: "$.coverage", Operator: OpEq, Value: json.RawMessage(`"gap"`)}},
		{To: "withdetail", When: &Condition{Path: "$.detail", Operator: OpExists}},
	}
	block := RenderOutputContract(next)
	desc := "实现功能\n\n实现请求的功能\n\n## 上游输入（Graph 数据流权威绑定，随本任务发布冻结）\n\n### 来自节点 probe（activation probe@1）\n结果摘要: {}\n\n" + block

	lines := ExtractOutputContract(desc)
	if len(lines) != 4 { // 表头 + 2 字段行 + 固定尾行
		t.Fatalf("块内应解析出 4 行，实际 %d: %v", len(lines), lines)
	}
	if lines[1] != "coverage ∈ {gap}" || lines[2] != "detail 必须存在" {
		t.Errorf("字段行应逐行原样返回，实际 %v", lines)
	}
	if !strings.HasSuffix(lines[3], "禁止提交 event。") {
		t.Errorf("尾行应为系统参数/event 纪律，实际 %q", lines[3])
	}

	for name, bad := range map[string]string{
		"无定界块":   "实现功能\n\n实现请求的功能",
		"子串提及":   "本任务 output-contract 见上文，coverage ∈ {gap, ok}",
		"块不完整":   "实现\n\n" + OutputContractBegin + "\ncoverage ∈ {gap}\n（截断，无结束标记）",
		"结束标记在前": OutputContractEnd + "\n实现\n" + OutputContractBegin,
	} {
		if got := ExtractOutputContract(bad); got != nil {
			t.Errorf("%s：应返回 nil，实际 %v", name, got)
		}
	}
}

// 任务发布派生：v2 图 agent 节点从冻结出边派生契约；acceptance 节点的
// $.verdict 出边同样派生；v1 图零行为变化（OutputContract 恒空）。
func TestTaskSpecOutputContractDerivedOnPublish(t *testing.T) {
	t.Run("v2 agent 节点", func(t *testing.T) {
		_, rt, b := newTestRuntime(t)
		mustSubmitRuntime(t, rt, v2OutletGraphJSON)
		specs := b.specsFor("impl")
		if len(specs) != 1 {
			t.Fatalf("impl 应发布一次任务，实际 %d", len(specs))
		}
		contract := specs[0].OutputContract
		if !strings.Contains(contract, "coverage ∈ {ok, gap}") {
			t.Errorf("v2 agent 节点任务应携带派生契约，实际 %q", contract)
		}
		if !strings.Contains(contract, OutputContractBegin) || !strings.Contains(contract, "禁止提交 event") {
			t.Errorf("契约应为定界块并含 event 禁令，实际 %q", contract)
		}
	})

	t.Run("v2 acceptance 节点", func(t *testing.T) {
		_, rt, b := newTestRuntime(t)
		mustSubmitRuntime(t, rt, v2AcceptanceGraphJSON)
		specs := b.specsFor("check")
		if len(specs) != 1 {
			t.Fatalf("check 应发布一次任务，实际 %d", len(specs))
		}
		// 两条 $.verdict eq 出边合并为同一值域（与既有 verdict 契约语义一致）。
		if !strings.Contains(specs[0].OutputContract, "verdict ∈ {pass, fixable}") {
			t.Errorf("acceptance 的 $.verdict 出边应派生 verdict 值域，实际 %q", specs[0].OutputContract)
		}
	})

	t.Run("v1 图不注入", func(t *testing.T) {
		_, rt, b := newTestRuntime(t)
		mustSubmitRuntime(t, rt, v1OutletGraphJSON)
		specs := b.specsFor("impl")
		if len(specs) != 1 {
			t.Fatalf("impl 应发布一次任务，实际 %d", len(specs))
		}
		if specs[0].OutputContract != "" {
			t.Errorf("v1 图任务不得携带输出契约（零行为变化），实际 %q", specs[0].OutputContract)
		}
	})
}

// 循环/重进（终态契约 v2 §5）：activation:"new" 重进发布新任务时按当时冻结
// 定义重新派生——patch_graph 改了出边，新 activation 的契约随之更新；已发布
// 的旧 activation 任务契约不变（定义随 activation 冻结）。
func TestOutputContractRederivedOnReentryAfterPatch(t *testing.T) {
	s, rt, b := newTestRuntime(t)
	mustSubmitRuntime(t, rt, v2OutletLoopGraphJSON)

	first := b.specsFor("impl")
	if len(first) != 1 || !strings.Contains(first[0].OutputContract, "coverage ∈ {ok, gap}") {
		t.Fatalf("impl@1 应按初始出边派生契约，实际 %+v", first)
	}

	// patch 改 impl 出边：coverage eq ok -> stagedone（新 end）、stage eq built
	// -> done、failed -> fix；新字段已在 task.description 声明（切片 1 校验）。
	rev, err := rt.PatchGraph("g-v2-loop", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{
		{
			ID: "impl", Kind: KindAgent,
			Task: &NodeTask{Title: "实现功能", Description: "实现请求的功能；输出契约：result 必须包含 coverage 与 stage"},
			Next: []Transition{
				{To: "stagedone", When: &Condition{Path: "$.coverage", Operator: OpEq, Value: json.RawMessage(`"ok"`)}},
				{To: "done", When: &Condition{Path: "$.stage", Operator: OpEq, Value: json.RawMessage(`"built"`)}},
				{To: "fix", When: &Condition{Event: EventFailed}},
			},
		},
		{ID: "stagedone", Kind: KindEnd, Next: []Transition{}},
	}})
	if err != nil || rev != 2 {
		t.Fatalf("PatchGraph: revision=%d err=%v", rev, err)
	}

	// impl@1 failed → fix@1；fix@1 failed → 回边以新 activation 重进 impl@2。
	mustTerminal(t, rt, TerminalFact{GraphID: "g-v2-loop", NodeID: "impl", ActivationID: "impl@1", TaskID: "task-1", Status: NodeFailed})
	mustTerminal(t, rt, TerminalFact{GraphID: "g-v2-loop", NodeID: "fix", ActivationID: "fix@1", TaskID: "task-2", Status: NodeFailed})
	if got := activationOf(nodeOf(t, s, "g-v2-loop", "impl")); got != "impl@2" {
		t.Fatalf("回边应以新 activation impl@2 重进，实际 %q", got)
	}

	specs := b.specsFor("impl")
	if len(specs) != 2 {
		t.Fatalf("impl 应发布两次任务（impl@1/impl@2），实际 %d", len(specs))
	}
	if !strings.Contains(specs[0].OutputContract, "coverage ∈ {ok, gap}") {
		t.Errorf("已发布的 impl@1 任务契约应保持初始派生，实际 %q", specs[0].OutputContract)
	}
	renewed := specs[1].OutputContract
	for _, want := range []string{"coverage ∈ {ok}", "stage ∈ {built}"} {
		if !strings.Contains(renewed, want) {
			t.Errorf("impl@2 应按 patch 后冻结定义重新派生（缺 %q），实际 %q", want, renewed)
		}
	}
	if strings.Contains(renewed, "{ok, gap}") {
		t.Errorf("impl@2 不得残留 patch 前的值域，实际 %q", renewed)
	}
}

// 嵌套 path（$.stats.coverage）的派生：契约行剥离 "$." 前缀但保留完整
// 嵌套段，与根字段契约并存时按完整路径排序（覆盖切片 5 缺口：多 path
// 嵌套字段的派生形态）。
func TestRenderOutputContractNestedPath(t *testing.T) {
	next := []Transition{
		{To: "done", When: &Condition{Path: "$.stats.coverage", Operator: OpEq, Value: json.RawMessage(`"ok"`)}},
		{To: "leveled", When: &Condition{Path: "$.stats.level", Operator: OpExists}},
		{To: "gapfix", When: &Condition{Path: "$.coverage", Operator: OpIn, Value: json.RawMessage(`["gap"]`)}},
	}
	block := RenderOutputContract(next)
	if block == "" {
		t.Fatal("嵌套 path 条件应渲染契约块")
	}
	for _, want := range []string{"coverage ∈ {gap}", "stats.coverage ∈ {ok}", "stats.level 必须存在"} {
		if !strings.Contains(block, want) {
			t.Errorf("契约块应包含 %q，实际：\n%s", want, block)
		}
	}
	// 嵌套段保留、$. 前缀剥离；按完整路径排序（coverage < stats.coverage < stats.level）。
	if strings.Contains(block, "$.") {
		t.Errorf("契约行不得残留 $. 前缀:\n%s", block)
	}
	order := []string{"coverage ∈", "stats.coverage ∈", "stats.level 必须存在"}
	prev := -1
	for _, marker := range order {
		idx := strings.Index(block, marker)
		if idx < 0 || idx < prev {
			t.Fatalf("字段行应按完整路径排序（%q 位置 %d，前一位置 %d）:\n%s", marker, idx, prev, block)
		}
		prev = idx
	}
}
