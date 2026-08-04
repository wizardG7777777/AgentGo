package bootstrap

// 本文件是 V6 Graph tool 桥（C5c，graph_tool.go）的测试：
// 临时目录真实跑只读四工具（read_file/list_dir/grep_search/glob_search），
// 写/Shell/Meta 类工具一律拒绝，另含图内 tool 节点端到端（tool→end）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/store"
)

// newGraphToolProject 搭一个临时项目根：main.go + docs/a.txt。
// 返回项目根路径（文件随写随关，无长生命周期句柄）。
func newGraphToolProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("写 main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("hello graph\n第二行\n"), 0o644); err != nil {
		t.Fatalf("写 a.txt: %v", err)
	}
	return root
}

// executeGraphTool 调执行器并断言成功，返回规整后的 Result。
func executeGraphTool(t *testing.T, ex *graphToolExecutor, name string, args map[string]any) map[string]any {
	t.Helper()
	result, err := ex.ExecuteNodeTool(context.Background(), name, args)
	if err != nil {
		t.Fatalf("执行 %s 应成功: %v", name, err)
	}
	if result["tool"] != name {
		t.Errorf("Result[tool] = %v，应为 %q", result["tool"], name)
	}
	if _, ok := result["content"].(string); !ok {
		t.Fatalf("Result[content] 应为字符串，实际 %T", result["content"])
	}
	return result
}

// TestGraphToolExecutorReadOnly 只读四工具在临时目录真实执行，
// 返回值规整为 {tool, content}。
func TestGraphToolExecutorReadOnly(t *testing.T) {
	root := newGraphToolProject(t)
	ex := newGraphToolExecutor(root)

	// read_file：内容 + 自描述头部。
	result := executeGraphTool(t, ex, "read_file", map[string]any{"path": "docs/a.txt"})
	content := result["content"].(string)
	if !strings.Contains(content, "hello graph") || !strings.Contains(content, "[file]") || !strings.Contains(content, "[hash]") {
		t.Errorf("read_file 输出应含内容与自描述头部:\n%s", content)
	}

	// list_dir：目录枚举。
	result = executeGraphTool(t, ex, "list_dir", map[string]any{"path": "."})
	content = result["content"].(string)
	if !strings.Contains(content, "main.go") || !strings.Contains(content, "docs") {
		t.Errorf("list_dir 输出应含 main.go 与 docs:\n%s", content)
	}

	// grep_search：字面子串命中。
	result = executeGraphTool(t, ex, "grep_search", map[string]any{"pattern": "hello", "path": "docs"})
	if !strings.Contains(result["content"].(string), "a.txt:1") {
		t.Errorf("grep_search 输出应含 a.txt:1 命中行:\n%s", result["content"])
	}

	// glob_search：** 递归通配。
	result = executeGraphTool(t, ex, "glob_search", map[string]any{"pattern": "**/*.txt", "root_dir": "."})
	if !strings.Contains(result["content"].(string), "docs/a.txt") {
		t.Errorf("glob_search 输出应含 docs/a.txt:\n%s", result["content"])
	}
}

// TestGraphToolExecutorRejectsNonReadOnly 写/Shell/Meta 类工具一律中文错误拒绝
// （tool 节点只做确定性只读执行；副作用是 agent 节点的职责）。
func TestGraphToolExecutorRejectsNonReadOnly(t *testing.T) {
	ex := newGraphToolExecutor(t.TempDir())
	for _, name := range []string{"write_file", "edit_file", "run_shell", "send_message", "publish_task", "web_search", "nonexistent"} {
		_, err := ex.ExecuteNodeTool(context.Background(), name, map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "不允许执行工具") || !strings.Contains(err.Error(), name) {
			t.Errorf("工具 %q 应被拒绝且错误载明工具名，实际: %v", name, err)
		}
	}
}

// TestGraphToolExecutorPathBoundary 项目根边界照常生效：逃逸路径被拒绝。
func TestGraphToolExecutorPathBoundary(t *testing.T) {
	root := newGraphToolProject(t)
	ex := newGraphToolExecutor(root)
	if _, err := ex.ExecuteNodeTool(context.Background(), "read_file", map[string]any{"path": "../outside.txt"}); err == nil {
		t.Error("项目根外路径应被 pathutil 边界拒绝")
	}
}

// bridgeToolGraphJSON tool 节点端到端图：root(agent) → t(tool: read_file) → finish(end)。
const bridgeToolGraphJSON = `{
  "schema": "agentgo.graph/v1", "graph_id": "g-bridge-tool", "revision": 1, "state_version": 0,
  "root": "root", "status": "pending",
  "nodes": {
    "root": {"kind":"agent","task":{"title":"准备材料"},"status":"inactive","executor":null,"execution":null,
      "next":[{"to":"t"}]},
    "t": {"kind":"tool","task":{"title":"读取材料"},"status":"inactive","executor":null,"execution":null,
      "tool":{"name":"read_file","args":{"path":"docs/a.txt"}},
      "next":[{"to":"finish"}]},
    "finish": {"kind":"end","task":{"title":"收尾"},"status":"inactive","executor":null,"execution":null,"next":[]}
  }
}`

// TestGraphToolNodeEndToEnd 图内 tool 节点端到端：root 终态 → tool 节点
// 同步执行真实 read_file → Result 落盘（载文件内容）→ 图 completed。
func TestGraphToolNodeEndToEnd(t *testing.T) {
	root := newGraphToolProject(t)
	tasks := store.NewMemoryTaskStore(nil, 100, 1, 300)
	gs, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 graph Store: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() }) // Windows 纪律：先 Close 再让 TempDir 清理
	rt := graph.NewRuntime(gs, newGraphBoard(tasks))
	wireGraphToolBridge(root, rt)

	doc, err := graph.ParseAndValidate([]byte(bridgeToolGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := rt.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
	rootTask := mustFindGraphTask(t, tasks, "g-bridge-tool", "root", "root@1")
	if err := rt.OnTaskTerminal(graph.TerminalFact{
		GraphID: "g-bridge-tool", NodeID: "root", ActivationID: "root@1", TaskID: rootTask.ID, Status: graph.NodeCompleted,
	}); err != nil {
		t.Fatalf("root 终态回填应成功: %v", err)
	}

	g, ok := gs.Get("g-bridge-tool")
	if !ok {
		t.Fatal("图应存在")
	}
	toolNode := g.Nodes["t"]
	if toolNode.Status != graph.NodeCompleted || !strings.Contains(toolNode.Execution.ResultRef, "hello graph") {
		t.Errorf("tool 节点应 completed 且 Result 载文件内容: status=%s result_ref=%s", toolNode.Status, toolNode.Execution.ResultRef)
	}
	if g.Status != graph.GraphCompleted {
		t.Errorf("图应为 completed，实际 %s", g.Status)
	}
}
