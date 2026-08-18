package graph

import (
	"reflect"
	"testing"
)

// TestParseSessionIDCompat session_id 字段的单向兼容（已批准）：
// 旧 JSON（无 session_id）仍可解析、新 JSON 带 session_id 可解析、
// 未知字段仍被 DisallowUnknownFields 拒绝。
func TestParseSessionIDCompat(t *testing.T) {
	// 旧 JSON：无 session_id，解析通过且归属为空串。
	doc := mustParse(t, tinyDocJSON)
	if doc.SessionID != "" {
		t.Errorf("旧 JSON 的 SessionID 应为空串，实际为 %q", doc.SessionID)
	}

	// 新 JSON：带 session_id，解析通过且归属正确。
	withSess := mutate(t, tinyDocJSON, `"graph_id": "g1",`, `"graph_id": "g1",
  "session_id": "sess-x",`)
	doc2 := mustParse(t, withSess)
	if doc2.SessionID != "sess-x" {
		t.Errorf("带 session_id 的 JSON 应解析出归属 %q，实际为 %q", "sess-x", doc2.SessionID)
	}

	// 未知字段仍然拒绝。
	unknown := mutate(t, tinyDocJSON, `"graph_id": "g1",`, `"graph_id": "g1",
  "owner": "sess-x",`)
	assertInvalid(t, "未知字段", unknown, "未知字段", `"owner"`)
}

// TestDigestIgnoresSessionID digest 只覆盖执行语义：两张仅 session_id
// 不同的图 digest 必须相同。
func TestDigestIgnoresSessionID(t *testing.T) {
	d1 := mustParse(t, exampleDocJSON)
	d2 := mustParse(t, exampleDocJSON)
	d2.SessionID = "sess-abc"
	if got1, got2 := ComputeDigest(d1), ComputeDigest(d2); got1 != got2 {
		t.Errorf("session_id 不得影响 digest:\n  d1=%s\n  d2=%s", got1, got2)
	}
}

// TestSubmitStampsSessionID 提交落图时经 sessionIDProvider 盖章：
// 空归属才填充（不覆盖显式声明）；provider 为 nil 时行为同今（空串）。
func TestSubmitStampsSessionID(t *testing.T) {
	s, rt, _ := newTestRuntime(t)
	rt.SetSessionIDProvider(func() string { return "sess-cur" })

	// 未声明归属：盖章为当前 session。
	mustSubmitRuntime(t, rt, tinyDocJSON)
	if got := mustGet(t, s, "g1").SessionID; got != "sess-cur" {
		t.Errorf("提交应盖章当前 session %q，实际为 %q", "sess-cur", got)
	}

	// 已显式声明归属：不覆盖。
	declared := mutate(t, tinyDocJSON, `"graph_id": "g1",`, `"graph_id": "g2",
  "session_id": "sess-declared",`)
	mustSubmitRuntime(t, rt, declared)
	if got := mustGet(t, s, "g2").SessionID; got != "sess-declared" {
		t.Errorf("显式声明的归属不应被覆盖，期望 %q，实际为 %q", "sess-declared", got)
	}

	// provider 为 nil：行为同今，归属保持空串。
	s2, rt2, _ := newTestRuntime(t)
	mustSubmitRuntime(t, rt2, tinyDocJSON)
	if got := mustGet(t, s2, "g1").SessionID; got != "" {
		t.Errorf("无 provider 时归属应为空串，实际为 %q", got)
	}
}

// TestSessionlessGraphsSuspendedOnStartup 2026-08 二期：无归属历史图不再归并
// 给当前 session（AdoptSessionlessGraphs 已删除）——恢复后归属保持空串，
// 会话模式下 SuspendGraphsExceptSession 把它们一并停驻（僵尸图）。
func TestSessionlessGraphsSuspendedOnStartup(t *testing.T) {
	s := newTestStore(t)
	// 直接经 Store 提交（不经过 Runtime），模拟升级前落盘的无归属历史图。
	mustSubmit(t, s, tinyDocJSON)

	// 模拟重启：关闭并同目录重建 + Recover。
	ns := reopenStore(t, s)
	if got := mustGet(t, ns, "g1").SessionID; got != "" {
		t.Fatalf("恢复出的历史图归属应为空串，实际为 %q", got)
	}

	rt := NewRuntime(ns, newFakeBoard())
	rt.SetSessionIDProvider(func() string { return "sess-cur" })
	suspended := rt.SuspendGraphsExceptSession("sess-cur")
	if !reflect.DeepEqual(suspended, []string{"g1"}) {
		t.Fatalf("无归属历史图应被停驻，实际停驻列表为 %v", suspended)
	}
	if got := mustGet(t, ns, "g1").SessionID; got != "" {
		t.Errorf("停驻不得改写归属，实际为 %q", got)
	}
}

// TestGraphsForSession 按 session 归属过滤图 ID（查询面）。
func TestGraphsForSession(t *testing.T) {
	_, rt, _ := newTestRuntime(t)
	cur := "sess-A"
	rt.SetSessionIDProvider(func() string { return cur })

	mustSubmitRuntime(t, rt, tinyDocJSON) // g1 → sess-A
	cur = "sess-B"
	docB := mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "g2"`)
	mustSubmitRuntime(t, rt, docB) // g2 → sess-B

	if got := rt.GraphsForSession("sess-A"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("sess-A 的图应为 [g1]，实际为 %v", got)
	}
	if got := rt.GraphsForSession("sess-B"); !reflect.DeepEqual(got, []string{"g2"}) {
		t.Errorf("sess-B 的图应为 [g2]，实际为 %v", got)
	}
	if got := rt.GraphsForSession(""); len(got) != 0 {
		t.Errorf("无归属过滤应为空，实际为 %v", got)
	}
	if got := rt.GraphsForSession("sess-不存在"); len(got) != 0 {
		t.Errorf("未知 session 过滤应为空，实际为 %v", got)
	}
}
