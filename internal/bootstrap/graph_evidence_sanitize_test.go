package bootstrap

// SWE-002 第一层防线（evidence 装配归一）的单测与事故形状端到端回归：
//   - evidenceKindOf：垃圾名归一 unknown、合法自定义名保留、超长合法名截断；
//   - evidenceCallEntry：DSML 垃圾名 → kind=unknown + tool_name=malformed 占位
//     （确定性），装配产物过 store 的 validateEvidenceEntryBounds 权威校验；
//   - 集成回归：200+ 字符 DSML 垃圾名进账本后，终态回填不再整条拒写——
//     节点正常 completed、图继续推进（SWE-002 事故形状不再复现）。

import (
	"strings"
	"testing"

	"agentgo/internal/graph"
	"agentgo/internal/store"
)

// dsmlGarbageToolName 复现事故形状：模型把 DSML 标记泄进 tool_call 名字段。
func dsmlGarbageToolName() string {
	return "run_shell>\n<｜DSML｜parameter name=\"command\" string=\"true\">" + strings.Repeat("x", 200)
}

func TestEvidenceKindOfNormalization(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		want     string
	}{
		{"已知归并 shell", "run_shell", "shell"},
		{"已知归并 file_write", "write_file", "file_write"},
		{"已知归并 web", "web_fetch", "web"},
		{"合法自定义名保留", "submit_task_result", "submit_task_result"},
		{"合法自定义名含冒号点线", "mcp__x.y:z-w", "mcp__x.y:z-w"},
		{"DSML 垃圾名归一 unknown", dsmlGarbageToolName(), "unknown"},
		{"含换行归一 unknown", "run_shell\nxx", "unknown"},
		{"数字开头归一 unknown", "3foo", "unknown"},
		{"空串归一 unknown", "", "unknown"},
		{"含 CJK 归一 unknown", "工具", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceKindOf(tc.toolName); got != tc.want {
				t.Errorf("evidenceKindOf(%q) = %q，应为 %q", tc.toolName, got, tc.want)
			}
		})
	}
	// 超长但形状合法的名字：截断到 64 rune（末位省略号），不归一 unknown、
	// 更不会原样透传撞 MaxIDLength=128。
	long := strings.Repeat("a", 200)
	got := evidenceKindOf(long)
	if len([]rune(got)) != evidenceKindMaxRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("超长合法名应截断到 %d rune 带省略号，实际 %q（%d rune）",
			evidenceKindMaxRunes, got, len([]rune(got)))
	}
}

// TestEvidenceCallEntrySanitizesGarbage 验证单条调用证据的归一：DSML 垃圾名
// → kind=unknown、tool_name=确定性 malformed 占位；原始垃圾名不进证据。
func TestEvidenceCallEntrySanitizesGarbage(t *testing.T) {
	raw := dsmlGarbageToolName()
	call := store.ToolCallRecord{CallID: "c-1", AgentID: "a-1", ToolName: raw, Success: true}
	entry := evidenceCallEntry("ev:t:call:1", call)
	if entry.Kind != "unknown" {
		t.Errorf("垃圾名 kind 应归一 unknown，实际 %q", entry.Kind)
	}
	wantPlaceholder := store.MalformedToolNamePlaceholder(raw)
	if entry.ToolName != wantPlaceholder {
		t.Errorf("tool_name 应为确定性占位 %q，实际 %q", wantPlaceholder, entry.ToolName)
	}
	if strings.Contains(entry.ToolName, raw) || len([]rune(entry.ToolName)) > 64 {
		t.Errorf("原始垃圾名不得进入证据层: %q", entry.ToolName)
	}
	// 确定性：同一垃圾名永远同一占位；不同垃圾名占位不同。
	again := evidenceCallEntry("ev:t:call:2", call)
	if again.ToolName != entry.ToolName {
		t.Errorf("同一垃圾名应得同一占位: %q vs %q", again.ToolName, entry.ToolName)
	}
	other := evidenceCallEntry("ev:t:call:3", store.ToolCallRecord{ToolName: "edit_file>|<" + strings.Repeat("y", 150)})
	if other.ToolName == entry.ToolName {
		t.Errorf("不同垃圾名占位应可区分，均为 %q", entry.ToolName)
	}

	// 合法名逐字节保留（已知工具名 kind 仍归并）。
	legal := evidenceCallEntry("ev:t:call:4", store.ToolCallRecord{
		CallID: "c-4", ToolName: "run_shell", Args: map[string]any{"command": "pytest -q | tail"},
		Success: true, ExitCodeScope: store.ShellExitCodeScopeLastPipelineCommand,
	})
	if legal.ToolName != "run_shell" || legal.Kind != "shell" ||
		legal.ExitCodeScope != string(store.ShellExitCodeScopeLastPipelineCommand) {
		t.Errorf("合法名应原样保留且 kind 归并: %+v", legal)
	}
	rejected := evidenceCallEntry("ev:t:call:5", store.ToolCallRecord{
		CallID: "c-5", ToolName: "run_shell", Args: map[string]any{"command": "pytest | tail"}, Success: false,
	})
	if !strings.Contains(rejected.Summary, "scope=?") || rejected.ExitCodeScope != "" {
		t.Errorf("执行前拒绝的 pipeline 不得伪装 whole-command scope: %+v", rejected)
	}
}

// TestAssembledEvidencePassesStoreBounds 直接断言装配产物过 store 的
// validateEvidenceEntryBounds 权威校验：把含垃圾名的调用证据经真实
// graph.Store.RecordActivationResult 落盘，必须被接受（事故前在此被拒写）。
func TestAssembledEvidencePassesStoreBounds(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(nil, 100, 1, 300)
	graphStore, err := graph.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("创建 graph Store: %v", err)
	}
	t.Cleanup(func() { _ = graphStore.Close() })
	rt := graph.NewRuntime(graphStore, newGraphBoard(taskStore))
	doc, err := graph.ParseAndValidate([]byte(`{
	  "schema": "agentgo.graph/v1",
	  "graph_id": "g-ev-bounds",
	  "revision": 1, "state_version": 0,
	  "root": "root", "status": "pending",
	  "nodes": {
	    "root": {"kind":"agent","task":{"title":"任务"},"status":"inactive","executor":null,"execution":null,
	      "next":[{"to":"finish"}]},
	    "finish": {"kind":"end","task":{"title":"收官"},"status":"inactive","executor":null,"execution":null,"next":[]}
	  }
	}`))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := rt.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph: %v", err)
	}

	call := store.ToolCallRecord{CallID: "c-1", AgentID: "a-1", ToolName: dsmlGarbageToolName(), Success: true}
	entry := evidenceCallEntry(evidenceCallRef("task-x", call), call)
	err = graphStore.RecordActivationResult("g-ev-bounds", graph.ActivationResult{
		NodeID: "root", ActivationID: "root@1",
		Result:   []byte(`{"status":"completed"}`),
		Evidence: []graph.EvidenceEntry{entry},
	})
	if err != nil {
		t.Fatalf("装配产物必须过 store 证据边界校验（事故前整条拒写）: %v", err)
	}
}

// TestGraphBridgeMalformedToolNameWritebackRegression 是 SWE-002 事故形状的
// 端到端回归：200+ 字符 DSML 垃圾名经 AppendToolCall 落账（第二层清洗）、
// evidence 装配（第一层归一）后，终态回填不再被整条拒写——root 正常
// completed、下游 implement 按边发布、唤醒裁决不需要发生。
func TestGraphBridgeMalformedToolNameWritebackRegression(t *testing.T) {
	env := newGraphBridgeEnv(t)
	doc, err := graph.ParseAndValidate([]byte(bridgeLinearGraphJSON))
	if err != nil {
		t.Fatalf("解析图: %v", err)
	}
	if err := env.runtime.SubmitGraph(doc); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
	rootTask := mustFindGraphTask(t, env.tasks, "g-bridge-linear", "root", "root@1")

	raw := dsmlGarbageToolName()
	if err := env.tasks.AppendToolCall(rootTask.ID, store.ToolCallRecord{
		CallID: "garbage-1", AgentID: "runner-1", ToolName: raw, Success: true,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	runTaskToCompleted(t, env.tasks, "runner-1", rootTask.ID, "需求已理解")
	eventually(t, "垃圾名入账后 root 终态回填应成功并推进到 implement", func() bool {
		return findGraphTask(env.tasks, "g-bridge-linear", "implement", "implement@1") != nil
	})
	g, ok := env.graphs.Get("g-bridge-linear")
	if !ok {
		t.Fatal("图应存在")
	}
	if g.Nodes["root"].Status != graph.NodeCompleted {
		t.Fatalf("root 应为 completed（终态事实未丢失），实际 %s", g.Nodes["root"].Status)
	}
	// 落盘证据中的 tool_name 是确定性占位，原始垃圾名不进证据层。
	rec, ok := env.graphs.ResolveActivationResult("g-bridge-linear", g.Nodes["root"].Execution.ResultRef)
	if !ok {
		t.Fatal("root 的 activation result 应可解引用")
	}
	found := false
	for _, ev := range rec.Evidence {
		if ev.CallID == "garbage-1" {
			found = true
			if !strings.HasPrefix(ev.ToolName, "malformed:") {
				t.Errorf("垃圾名在证据层应为 malformed: 占位，实际 %q", ev.ToolName)
			}
			if strings.Contains(ev.ToolName, raw) || strings.Contains(ev.Kind, "DSML") {
				t.Errorf("原始垃圾名不得进证据层: kind=%q tool_name=%q", ev.Kind, ev.ToolName)
			}
		}
	}
	if !found {
		t.Error("证据中应包含垃圾名调用的清洗后条目")
	}
}
