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
