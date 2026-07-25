package userdef

import (
	"strings"
	"testing"

	"agentgo/internal/trace"
)

func TestParseWhen_Empty(t *testing.T) {
	w, err := parseWhen("")
	if err != nil {
		t.Fatalf("empty when should not error, got %v", err)
	}
	if w != nil {
		t.Errorf("empty when should return nil cond, got %+v", w)
	}
}

func TestParseWhen_NoOperator(t *testing.T) {
	if _, err := parseWhen("some plain text"); err == nil {
		t.Error("expected error for missing operator")
	}
}

func TestParseWhen_UnknownVarRef(t *testing.T) {
	if _, err := parseWhen(`${event.bogus.field} == "x"`); err == nil {
		t.Error("expected error for unknown var")
	}
}

func TestEval_Equals_StringLiteral(t *testing.T) {
	w, err := parseWhen(`${event.task.id} == "abc"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !w.eval(trace.Event{TaskID: "abc"}) {
		t.Error("abc == \"abc\" should be true")
	}
	if w.eval(trace.Event{TaskID: "xyz"}) {
		t.Error("xyz == \"abc\" should be false")
	}
}

func TestEval_NotEquals(t *testing.T) {
	w, _ := parseWhen(`${event.task.event_type} != "internal"`)
	if !w.eval(trace.Event{EventType: "user"}) {
		t.Error("user != internal should be true")
	}
	if w.eval(trace.Event{EventType: "internal"}) {
		t.Error("internal != internal should be false")
	}
}

func TestEval_OrderedNumeric(t *testing.T) {
	w, err := parseWhen(`${event.task.retry_count} >= 3`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !w.eval(trace.Event{AttemptNo: 5}) {
		t.Error("5 >= 3 should be true")
	}
	if w.eval(trace.Event{AttemptNo: 2}) {
		t.Error("2 >= 3 should be false")
	}
	if !w.eval(trace.Event{AttemptNo: 3}) {
		t.Error("3 >= 3 should be true (boundary)")
	}
}

func TestEval_OrderedStringFallback(t *testing.T) {
	// 非数字字符串走字典序
	w, _ := parseWhen(`${event.task.id} < "m"`)
	if !w.eval(trace.Event{TaskID: "abc"}) {
		t.Error("abc < m lexically true")
	}
	if w.eval(trace.Event{TaskID: "zzz"}) {
		t.Error("zzz < m lexically false")
	}
}

func TestEval_LongOpsBeforeShort(t *testing.T) {
	// "<=" 不能被切成 "<"
	w, _ := parseWhen(`${event.loop} <= 10`)
	if !w.eval(trace.Event{Loop: 10}) {
		t.Error("10 <= 10 should be true")
	}
	if w.eval(trace.Event{Loop: 11}) {
		t.Error("11 <= 10 should be false")
	}
}

func TestEval_In_Literal(t *testing.T) {
	w, err := parseWhen(`${event.task.event_type} in [task_completed, task_failed]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !w.eval(trace.Event{EventType: "task_failed"}) {
		t.Error("task_failed should be in list")
	}
	if w.eval(trace.Event{EventType: "task_retry"}) {
		t.Error("task_retry should not be in list")
	}
}

func TestEval_In_WithVarRefs(t *testing.T) {
	w, err := parseWhen(`${event.task.id} in [${event.agent.id}, "fallback"]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !w.eval(trace.Event{TaskID: "x", AgentID: "x"}) {
		t.Error("task.id matches agent.id should be true")
	}
	if !w.eval(trace.Event{TaskID: "fallback"}) {
		t.Error("fallback literal should match")
	}
	if w.eval(trace.Event{TaskID: "other"}) {
		t.Error("other not in list")
	}
}

func TestEval_QuotedOperatorInString(t *testing.T) {
	// 字符串字面量中的 == 不应被识别为 operator
	w, err := parseWhen(`${event.task.error} == "x == y"`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !w.eval(trace.Event{Error: "x == y"}) {
		t.Errorf("string with literal == should match")
	}
}

func TestEval_NilCond_AlwaysTrue(t *testing.T) {
	var w *whenCond
	if !w.eval(trace.Event{}) {
		t.Error("nil whenCond should be always true")
	}
}

func TestParseWhen_RejectLogicalCompose(t *testing.T) {
	cases := []string{
		`${event.task.retry_count} >= 3 && ${event.kind} == "task_failed"`,
		`${event.task.retry_count} >= 3 || ${event.kind} == "task_failed"`,
		`${event.task.retry_count} >= 3 and ${event.kind} == "task_failed"`,
		`not ${event.task.retry_count} >= 3`,
	}
	for _, expr := range cases {
		if _, err := parseWhen(expr); err == nil {
			t.Fatalf("expected logical composition to be rejected: %s", expr)
		}
	}
}

func TestParseWhen_AllowsLogicalWordsInsideQuotesAndVars(t *testing.T) {
	if _, err := parseWhen(`${event.task.reason} == "and/or/not"`); err != nil {
		t.Fatalf("logical words inside quotes should be allowed: %v", err)
	}
	if _, err := parseWhen(`${event.task.reason} == "${event.task.id}"`); err != nil {
		t.Fatalf("variable-shaped text inside quotes should be allowed: %v", err)
	}
}

func TestSplitListComma_RespectsQuotesAndBraces(t *testing.T) {
	got := splitListComma(`a, "b, c", ${event.task.id}, d`)
	want := []string{"a", ` "b, c"`, ` ${event.task.id}`, " d"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (%+v)", len(got), len(want), got)
	}
	for i := range got {
		if strings.TrimSpace(got[i]) != strings.TrimSpace(want[i]) {
			t.Errorf("[%d] got=%q want=%q", i, got[i], want[i])
		}
	}
}

// ===== and / or / not 逻辑组合 =====

// 两个叶子条件：A = retry_count >= 3，B = kind == task_failed
var (
	whenLeafA = `${event.task.retry_count} >= 3`
	whenLeafB = `${event.kind} == "task_failed"`
)

func whenTruthEvents() map[string]trace.Event {
	return map[string]trace.Event{
		"TT": {Kind: trace.KindTaskFailed, AttemptNo: 5},
		"TF": {Kind: trace.KindTaskCompleted, AttemptNo: 5},
		"FT": {Kind: trace.KindTaskFailed, AttemptNo: 1},
		"FF": {Kind: trace.KindTaskCompleted, AttemptNo: 1},
	}
}

func mustParseWhen(t *testing.T, raw any) *whenCond {
	t.Helper()
	w, err := parseWhen(raw)
	if err != nil {
		t.Fatalf("parseWhen(%v): %v", raw, err)
	}
	return w
}

func TestEval_And_TruthTable(t *testing.T) {
	w := mustParseWhen(t, map[string]any{"and": []any{whenLeafA, whenLeafB}})
	want := map[string]bool{"TT": true, "TF": false, "FT": false, "FF": false}
	for name, ev := range whenTruthEvents() {
		if got := w.eval(ev); got != want[name] {
			t.Errorf("and[%s]: got %v want %v", name, got, want[name])
		}
	}
}

func TestEval_Or_TruthTable(t *testing.T) {
	w := mustParseWhen(t, map[string]any{"or": []any{whenLeafA, whenLeafB}})
	want := map[string]bool{"TT": true, "TF": true, "FT": true, "FF": false}
	for name, ev := range whenTruthEvents() {
		if got := w.eval(ev); got != want[name] {
			t.Errorf("or[%s]: got %v want %v", name, got, want[name])
		}
	}
}

func TestEval_Not_TruthTable(t *testing.T) {
	w := mustParseWhen(t, map[string]any{"not": whenLeafA})
	if w.eval(trace.Event{AttemptNo: 5}) {
		t.Error("not(5 >= 3) should be false")
	}
	if !w.eval(trace.Event{AttemptNo: 1}) {
		t.Error("not(1 >= 3) should be true")
	}
}

func TestEval_SingleCondAnd_Or_EquivalentToLeaf(t *testing.T) {
	// 单子条件的 and/or 与叶子本身语义一致
	for _, key := range []string{"and", "or"} {
		w := mustParseWhen(t, map[string]any{key: []any{whenLeafA}})
		if !w.eval(trace.Event{AttemptNo: 5}) || w.eval(trace.Event{AttemptNo: 1}) {
			t.Errorf("%s[single] should behave like the leaf itself", key)
		}
	}
}

func TestEval_NestedCombination(t *testing.T) {
	// and(retry>=2, or(agent==worker-1, agent==worker-2), not(task.id==T-skip))
	w := mustParseWhen(t, map[string]any{"and": []any{
		`${event.task.retry_count} >= 2`,
		map[string]any{"or": []any{
			`${event.agent.id} == worker-1`,
			`${event.agent.id} == worker-2`,
		}},
		map[string]any{"not": `${event.task.id} == T-skip`},
	}})

	cases := []struct {
		name string
		ev   trace.Event
		want bool
	}{
		{"全部命中", trace.Event{TaskID: "T-1", AgentID: "worker-1", AttemptNo: 3}, true},
		{"or 另一分支命中", trace.Event{TaskID: "T-1", AgentID: "worker-2", AttemptNo: 3}, true},
		{"retry 不足", trace.Event{TaskID: "T-1", AgentID: "worker-1", AttemptNo: 1}, false},
		{"agent 不在 or 列表", trace.Event{TaskID: "T-1", AgentID: "worker-9", AttemptNo: 3}, false},
		{"not 排除的任务", trace.Event{TaskID: "T-skip", AgentID: "worker-1", AttemptNo: 3}, false},
	}
	for _, tc := range cases {
		if got := w.eval(tc.ev); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestEval_Not_NestedCombination(t *testing.T) {
	// not 的值也可以是组合节点：not(or(A, B)) ≡ 两者都不成立
	w := mustParseWhen(t, map[string]any{"not": map[string]any{"or": []any{whenLeafA, whenLeafB}}})
	if w.eval(trace.Event{Kind: trace.KindTaskFailed, AttemptNo: 5}) {
		t.Error("not(or(T,T)) should be false")
	}
	if !w.eval(trace.Event{Kind: trace.KindTaskCompleted, AttemptNo: 1}) {
		t.Error("not(or(F,F)) should be true")
	}
}

// nestNot 把 leaf 包进 n 层 not 组合节点，用于深度边界测试。
func nestNot(raw any, n int) any {
	for i := 0; i < n; i++ {
		raw = map[string]any{"not": raw}
	}
	return raw
}

func TestParseWhen_NestingDepthLimit(t *testing.T) {
	// 8 层组合节点合法；第 9 层报深度超限
	if _, err := parseWhen(nestNot(whenLeafA, maxWhenNesting)); err != nil {
		t.Fatalf("%d 层嵌套应合法, got %v", maxWhenNesting, err)
	}
	_, err := parseWhen(nestNot(whenLeafA, maxWhenNesting+1))
	if err == nil || !strings.Contains(err.Error(), "最大深度") {
		t.Fatalf("%d 层嵌套应报深度超限, got %v", maxWhenNesting+1, err)
	}
}

func TestParseWhen_InvalidStructures(t *testing.T) {
	cases := []struct {
		name    string
		raw     any
		wantErr string // 报错必须包含的片段
	}{
		{"组合map多键", map[string]any{"and": []any{whenLeafA}, "or": []any{whenLeafB}}, "恰好包含一个键"},
		{"未知组合键", map[string]any{"xor": []any{whenLeafA}}, `未知的组合键 "xor"`},
		{"and值非列表", map[string]any{"and": whenLeafA}, "需要非空条件列表"},
		{"or空列表", map[string]any{"or": []any{}}, "不能为空"},
		{"not值为列表", map[string]any{"not": []any{whenLeafA}}, "not 只接受单个条件"},
		{"嵌套空字符串", map[string]any{"and": []any{"  "}}, "条件表达式不能为空"},
		{"顶层非字符串非map", 5, "不支持的条件类型"},
		{"嵌套非字符串非map", map[string]any{"and": []any{true}}, "不支持的条件类型"},
	}
	for _, tc := range cases {
		if _, err := parseWhen(tc.raw); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: 期望报错含 %q, got %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestParseWhen_ErrorCarriesPath(t *testing.T) {
	// 嵌套内叶子语法错误必须带路径定位 when.or[0]
	_, err := parseWhen(map[string]any{"or": []any{"plain text", whenLeafA}})
	if err == nil || !strings.Contains(err.Error(), "when.or[0]") {
		t.Fatalf("期望报错路径 when.or[0], got %v", err)
	}
	// 更深一层：when.and[1].not
	_, err = parseWhen(map[string]any{"and": []any{
		whenLeafA,
		map[string]any{"not": "plain text"},
	}})
	if err == nil || !strings.Contains(err.Error(), "when.and[1].not") {
		t.Fatalf("期望报错路径 when.and[1].not, got %v", err)
	}
}

// ===== YAML 端到端：嵌套 when 经 Load 全链路 =====

func TestLoad_When_NestedLogicYAML(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub ${event.task.id}")

	yamlData := []byte(`
reactors:
  - on: task_failed
    when:
      and:
        - "${event.task.retry_count} >= 3"
        - or:
            - "${event.agent.id} == worker-1"
            - "${event.agent.id} == worker-2"
        - not: "${event.task.id} == T-skip"
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
	store := &fakeStore{}
	rs, err := Load(yamlData, dir, dir, Deps{Store: store})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	run := func(ev trace.Event) int {
		if err := rs[0].Run(ev); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return len(store.snapshot())
	}

	base := trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1", AgentID: "worker-1",
		Transition: &trace.Transition{RetryCount: 5}}

	// 不命中 or：不投递
	ev := base
	ev.AgentID = "worker-9"
	if got := run(ev); got != 0 {
		t.Errorf("agent 不在 or 列表应跳过, got %d tasks", got)
	}
	// 不命中 retry：不投递
	ev = base
	ev.Transition = &trace.Transition{RetryCount: 1}
	if got := run(ev); got != 0 {
		t.Errorf("retry 不足应跳过, got %d tasks", got)
	}
	// 命中 not 排除：不投递
	ev = base
	ev.TaskID = "T-skip"
	if got := run(ev); got != 0 {
		t.Errorf("not 排除的任务应跳过, got %d tasks", got)
	}
	// 全部命中：投递
	if got := run(base); got != 1 {
		t.Errorf("全部条件命中应投递, got %d tasks", got)
	}
}

func TestLoad_When_InvalidStructureYAML(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")

	yamlData := []byte(`
reactors:
  - on: task_failed
    when:
      and: []
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}})
	if err == nil || !strings.Contains(err.Error(), "when.and") || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("期望 when.and 空列表报错, got %v", err)
	}
}

func TestLoad_When_InlineLogicRejectedWithHint(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub")

	yamlData := []byte(`
reactors:
  - on: task_failed
    when: "${event.task.retry_count} >= 3 and ${event.kind} == task_failed"
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
	_, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}})
	if err == nil || !strings.Contains(err.Error(), "嵌套 and:/or:/not: 结构") {
		t.Fatalf("期望行内逻辑组合报错并提示嵌套写法, got %v", err)
	}
}
