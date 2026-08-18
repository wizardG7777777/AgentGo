package bootstrap

import (
	"fmt"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/ui"
)

// uiGraphDocJSON 生成最小合法图文档：a（agent）→ b（end），带指定 session 归属
// （空串表示尚未归并的历史图，JSON 中省略 session_id 字段）。
func uiGraphDocJSON(graphID, sessionID string) string {
	sessionLine := ""
	if sessionID != "" {
		sessionLine = fmt.Sprintf("\n  \"session_id\": %q,", sessionID)
	}
	return fmt.Sprintf(`{
  "schema": "agentgo.graph/v1",
  "graph_id": %q,%s
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
}`, graphID, sessionLine)
}

// newUIGraphStore 创建以 t.TempDir() 为根的 graph.Store；t.Cleanup 先 Close
// 再让 TempDir 清理（Windows 文件句柄硬约束）。
func newUIGraphStore(t *testing.T) *graph.Store {
	t.Helper()
	s, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustSubmitUIGraph 提交指定归属的图文档并断言成功。
func mustSubmitUIGraph(t *testing.T, s *graph.Store, graphID, sessionID string) {
	t.Helper()
	doc, err := graph.ParseAndValidate([]byte(uiGraphDocJSON(graphID, sessionID)))
	if err != nil {
		t.Fatalf("ParseAndValidate 应成功: %v", err)
	}
	if err := s.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
}

func graphViewIDs(views []ui.GraphView) []string {
	ids := make([]string, 0, len(views))
	for _, v := range views {
		ids = append(ids, v.GraphID)
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestGraphViewsForUI_FiltersBySession 图可见性按 session 隔离：当前 session
// 只能看到自己拥有的图与尚未归并的历史图，看不到其他 session 的图。
func TestGraphViewsForUI_FiltersBySession(t *testing.T) {
	s := newUIGraphStore(t)
	mustSubmitUIGraph(t, s, "g-own", "sess-A")
	mustSubmitUIGraph(t, s, "g-other", "sess-B")
	mustSubmitUIGraph(t, s, "g-legacy", "")

	views := graphViewsForUI(s, "sess-A")
	ids := graphViewIDs(views)
	if !containsID(ids, "g-own") {
		t.Errorf("当前 session 应看到自己的图 g-own，实际列表: %v", ids)
	}
	if !containsID(ids, "g-legacy") {
		t.Errorf("未归并的历史图 g-legacy 应对任何 session 可见，实际列表: %v", ids)
	}
	if containsID(ids, "g-other") {
		t.Errorf("其他 session 的图 g-other 不应出现在 sess-A 的视图中，实际列表: %v", ids)
	}

	views = graphViewsForUI(s, "sess-B")
	ids = graphViewIDs(views)
	if containsID(ids, "g-own") {
		t.Errorf("sess-A 的图 g-own 不应出现在 sess-B 的视图中，实际列表: %v", ids)
	}
	if !containsID(ids, "g-other") {
		t.Errorf("sess-B 应看到自己的图 g-other，实际列表: %v", ids)
	}
}

// TestGraphViewsForUI_EmptySessionShowsAll 无 session 上下文（空串）时不过滤，
// 与恢复面空串=全量的语义一致。
func TestGraphViewsForUI_EmptySessionShowsAll(t *testing.T) {
	s := newUIGraphStore(t)
	mustSubmitUIGraph(t, s, "g-a", "sess-A")
	mustSubmitUIGraph(t, s, "g-b", "sess-B")

	views := graphViewsForUI(s, "")
	if got := len(views); got != 2 {
		t.Errorf("空 sessionID 应看到全部 2 张图，实际 %d: %v", got, graphViewIDs(views))
	}
}

// TestGraphViewsForUI_ProjectsSessionID 投影应透出图的 session 归属，便于前端
// 与调试排查隔离问题。
func TestGraphViewsForUI_ProjectsSessionID(t *testing.T) {
	s := newUIGraphStore(t)
	mustSubmitUIGraph(t, s, "g-own", "sess-A")

	views := graphViewsForUI(s, "sess-A")
	if len(views) != 1 {
		t.Fatalf("应投影 1 张图，实际 %d", len(views))
	}
	if views[0].SessionID != "sess-A" {
		t.Errorf("投影的 SessionID 应为 %q，实际 %q", "sess-A", views[0].SessionID)
	}
}

// TestGraphViewsForUI_NilStore nil store 返回空视图（保持既有 nil-safe 行为）。
func TestGraphViewsForUI_NilStore(t *testing.T) {
	if views := graphViewsForUI(nil, "sess-A"); len(views) != 0 {
		t.Errorf("nil store 应返回空视图，实际 %d 张", len(views))
	}
}
