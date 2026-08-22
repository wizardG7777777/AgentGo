package trace

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// trace graph / trace node（V6 §7.5）测试
// ============================================================

// writeGraphShard 以给定文件名写一份 JSONL 分片（graph_ 分片无时间戳前缀，
// 与任务分片命名不同，因此独立的 fixture helper）。
func writeGraphShard(t *testing.T, dir, name string, events []Event) string {
	t.Helper()
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode fixture: %v", err)
		}
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatalf("write graph shard: %v", err)
	}
	return path
}

// writeGraphRawShard 写原始行（可含坏行），文件名同上分片规则。
func writeGraphRawShard(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write raw shard: %v", err)
	}
	return path
}

// writeGraphSnapshot 在 state 目录下写 <graph_id>/snapshot.json（"/" 段映射
// 为嵌套目录，与 store.graphDir 一致）。snapshotJSON 是原始 JSON 文本。
func writeGraphSnapshot(t *testing.T, stateDir, graphID, snapshotJSON string) string {
	t.Helper()
	dir := filepath.Join(stateDir, filepath.FromSlash(graphID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir snapshot dir: %v", err)
	}
	path := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(path, []byte(snapshotJSON), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return path
}

// graphFixtureEvents 构造 deploy-pipeline 的一组完整生命周期事件。
func graphFixtureEvents(base time.Time, graphID string) []Event {
	return []Event{
		{Timestamp: base, Kind: KindGraphSubmitted, GraphID: graphID, Description: "revision=1 digest=aaaabbbbcccc"},
		{Timestamp: base.Add(time.Second), Kind: KindNodeActivationCreated, GraphID: graphID, NodeID: "implement", ActivationID: "implement@1", Description: "phase=ready"},
		{Timestamp: base.Add(2 * time.Second), Kind: KindGraphTransitionSelected, GraphID: graphID, NodeID: "implement", ActivationID: "implement@1", Description: "next[0] -> verify"},
		{Timestamp: base.Add(3 * time.Second), Kind: KindGraphWaitStarted, GraphID: graphID, NodeID: "verify", ActivationID: "verify@1", Description: "event=deploy.done"},
		{Timestamp: base.Add(4 * time.Second), Kind: KindGraphEnded, GraphID: graphID},
	}
}

// TestCmdGraphCompleteTimeline 完整时间线：snapshot 可读 + 分片存在 +
// 无坏行 → complete；头部字段来自 snapshot，终态由 graph_ended 事件校准。
func TestCmdGraphCompleteTimeline(t *testing.T) {
	traceDir := t.TempDir()
	stateDir := t.TempDir()
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	graphID := "deploy-pipeline"

	writeGraphShard(t, traceDir, "graph_deploy-p.jsonl", graphFixtureEvents(base, graphID))
	// graph_change_requested 携 TaskID 落任务分片，应被并入图时间线。
	writeTraceFixture(t, traceDir, base.Add(5*time.Second), "task-1234567890", []Event{
		{Timestamp: base.Add(5 * time.Second), Kind: KindGraphChangeRequested, GraphID: graphID, NodeID: "implement", ActivationID: "implement@1", TaskID: "task-1234567890", Reason: "route_missing", Description: "verify 无可用路由"},
	})
	writeGraphSnapshot(t, stateDir, graphID, `{
  "version": 1, "graph_id": "deploy-pipeline", "seq": 9,
  "revision": 3, "state_version": 41, "digest": "abcdef1234567890deadbeef",
  "doc": {"status": "running"}
}`)

	var out bytes.Buffer
	if err := CLI([]string{"graph", "deploy"}, traceDir, stateDir, &out); err != nil {
		t.Fatalf("trace graph: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Graph: deploy-pipeline",
		"Status: completed", // graph_ended（无 Reason）校准 snapshot 的 running
		"Outcome: legacy",
		"Revision: 3", "StateVersion: 41", "Digest: abcdef123456",
		"Events: 6", "Shards: 2", "Coverage: complete",
		"graph_submitted", "node_activation_created", "graph_transition_selected",
		"graph_wait_started", "graph_ended", "graph_change_requested",
		"graph=deploy-pipeline", "node=implement", "activation=implement@1",
		"task=task-123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("trace graph 输出缺少 %q:\n%s", want, got)
		}
	}
	// 时间线按时间排序：submitted 在 ended 之前，change 最后。
	if !(strings.Index(got, "graph_submitted") < strings.Index(got, "graph_ended") &&
		strings.Index(got, "graph_ended") < strings.Index(got, "graph_change_requested")) {
		t.Errorf("图时间线未按时间排序:\n%s", got)
	}
	if strings.Contains(got, "WARNING") {
		t.Errorf("complete 场景不应出现 WARNING:\n%s", got)
	}
}

// TestCmdGraphPrefixMatch 前缀碰撞列候选；更长前缀消歧。两张前缀共享
// 8 位的图会同落一个分片，事件仍须按完整 GraphID 精确拆分。
func TestCmdGraphPrefixMatch(t *testing.T) {
	traceDir := t.TempDir()
	base := time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)
	writeGraphShard(t, traceDir, "graph_graph-aa.jsonl", []Event{
		{Timestamp: base, Kind: KindGraphSubmitted, GraphID: "graph-aaa", Description: "revision=1 digest=aaaa"},
		{Timestamp: base.Add(time.Second), Kind: KindGraphSubmitted, GraphID: "graph-aab", Description: "revision=1 digest=bbbb"},
		{Timestamp: base.Add(2 * time.Second), Kind: KindGraphEnded, GraphID: "graph-aab"},
	})

	var amb bytes.Buffer
	if err := CLI([]string{"graph", "graph-aa"}, traceDir, "", &amb); err != nil {
		t.Fatalf("trace graph 前缀碰撞: %v", err)
	}
	for _, want := range []string{"找到 2 个匹配的图", "graph-aaa", "graph-aab"} {
		if !strings.Contains(amb.String(), want) {
			t.Errorf("碰撞输出缺少 %q:\n%s", want, amb.String())
		}
	}

	var out bytes.Buffer
	if err := CLI([]string{"graph", "graph-aab"}, traceDir, "", &out); err != nil {
		t.Fatalf("trace graph 消歧: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Graph: graph-aab") || !strings.Contains(got, "Status: completed") {
		t.Errorf("消歧后头部不符:\n%s", got)
	}
	// 事件必须精确归属：graph-aab 的时间线不得混入 graph-aaa 的事件行。
	if strings.Count(got, "graph_submitted") != 1 {
		t.Errorf("共享分片的事件泄漏:\n%s", got)
	}
}

// TestCmdGraphListAll 无参列表：trace 事件与 state 目录并集去重，
// 含状态与最近事件时间。
func TestCmdGraphListAll(t *testing.T) {
	traceDir := t.TempDir()
	stateDir := t.TempDir()
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	writeGraphShard(t, traceDir, "graph_deploy-p.jsonl", graphFixtureEvents(base, "deploy-pipeline"))
	writeGraphShard(t, traceDir, "graph_graph-aa.jsonl", []Event{
		{Timestamp: base.Add(time.Minute), Kind: KindGraphSubmitted, GraphID: "graph-aaa", Description: "revision=1 digest=aaaa"},
	})
	// 仅在 state 目录存在的图（trace 分片已 GC）。
	orphanDir := filepath.Join(stateDir, "orphan-graph")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "journal.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := CLI([]string{"graph"}, traceDir, stateDir, &out); err != nil {
		t.Fatalf("trace graph 无参列表: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"deploy-pipeline", "graph-aaa", "orphan-graph",
		"completed", "running", "unknown",
		"共 3 张图",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("列表输出缺少 %q:\n%s", want, got)
		}
	}
	// 最近事件时间倒序：graph-aaa（最新）应排在 deploy-pipeline 之前。
	if strings.Index(got, "graph-aaa") > strings.Index(got, "deploy-pipeline") {
		t.Errorf("列表未按最近事件倒序:\n%s", got)
	}

	// 空目录提示。
	var empty bytes.Buffer
	if err := CLI([]string{"graph"}, t.TempDir(), t.TempDir(), &empty); err != nil {
		t.Fatalf("空目录列表: %v", err)
	}
	if !strings.Contains(empty.String(), "没有已知的图") {
		t.Errorf("空目录应提示没有已知的图:\n%s", empty.String())
	}
}

// TestCmdGraphNodeGroupsActivations 单节点视图：只展示该节点事件，
// 按 activation 分组（回边重进 = 新 activation）。
func TestCmdGraphNodeGroupsActivations(t *testing.T) {
	traceDir := t.TempDir()
	base := time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC)
	writeGraphShard(t, traceDir, "graph_g-node.jsonl", []Event{
		{Timestamp: base, Kind: KindNodeActivationCreated, GraphID: "g-node", NodeID: "implement", ActivationID: "implement@1"},
		{Timestamp: base.Add(time.Second), Kind: KindGraphTransitionSelected, GraphID: "g-node", NodeID: "implement", ActivationID: "implement@1", Description: "next[1] -> verify"},
		{Timestamp: base.Add(2 * time.Second), Kind: KindNodeActivationCreated, GraphID: "g-node", NodeID: "verify", ActivationID: "verify@1"},
		{Timestamp: base.Add(3 * time.Second), Kind: KindGraphTransitionSelected, GraphID: "g-node", NodeID: "verify", ActivationID: "verify@1", Description: "next[0] -> implement"},
		{Timestamp: base.Add(4 * time.Second), Kind: KindNodeActivationCreated, GraphID: "g-node", NodeID: "implement", ActivationID: "implement@2"},
		{Timestamp: base.Add(5 * time.Second), Kind: KindGraphTransitionSelected, GraphID: "g-node", NodeID: "implement", ActivationID: "implement@2", Description: "next[0] -> end"},
	})

	var out bytes.Buffer
	if err := CLI([]string{"node", "g-node/implement"}, traceDir, "", &out); err != nil {
		t.Fatalf("trace node: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Graph: g-node", "Node: implement",
		"activation implement@1", "activation implement@2",
		"activations=2", "events=4",
		"next[1] -> verify", "next[0] -> end",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("node 输出缺少 %q:\n%s", want, got)
		}
	}
	// verify 节点的事件不得混入。
	if strings.Contains(got, "node=verify") || strings.Contains(got, "verify@1") {
		t.Errorf("其他节点的事件混入单节点视图:\n%s", got)
	}
	// 分组顺序保持首次出现：implement@1 在 implement@2 之前。
	if strings.Index(got, "implement@1") > strings.Index(got, "implement@2") {
		t.Errorf("activation 分组顺序不符:\n%s", got)
	}

	// 节点无事件：头部照常 + 中文提示，不报错。
	var none bytes.Buffer
	if err := CLI([]string{"node", "g-node/ghost"}, traceDir, "", &none); err != nil {
		t.Fatalf("trace node 未知节点: %v", err)
	}
	if !strings.Contains(none.String(), "没有节点 ghost 的事件") {
		t.Errorf("未知节点应输出中文提示:\n%s", none.String())
	}
}

// TestCmdGraphPartialOnBadLine 分片有坏行 → partial，并列明文件与原因。
func TestCmdGraphPartialOnBadLine(t *testing.T) {
	traceDir := t.TempDir()
	base := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
	valid, err := json.Marshal(Event{Timestamp: base, Kind: KindGraphSubmitted, GraphID: "g-bad", Description: "revision=1 digest=aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	writeGraphRawShard(t, traceDir, "graph_g-bad.jsonl", []string{string(valid), `{"broken":`})

	var out bytes.Buffer
	if err := CLI([]string{"graph", "g-bad"}, traceDir, "", &out); err != nil {
		t.Fatalf("trace graph: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Coverage: partial", "graph_g-bad.jsonl", "1 行无法解析"} {
		if !strings.Contains(got, want) {
			t.Errorf("partial 输出缺少 %q:\n%s", want, got)
		}
	}
	// 坏行不影响已读事件的时间线展示。
	if !strings.Contains(got, "graph_submitted") {
		t.Errorf("partial 场景时间线应照常展示:\n%s", got)
	}
}

// TestCmdGraphPartialOnMissingShard 图仅在 state 目录存在（trace 分片
// 缺失，可能被 GC）→ partial，头部字段标 unknown。
func TestCmdGraphPartialOnMissingShard(t *testing.T) {
	traceDir := t.TempDir()
	stateDir := t.TempDir()
	orphanDir := filepath.Join(stateDir, "orphan-graph")
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "journal.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := CLI([]string{"graph", "orphan"}, traceDir, stateDir, &out); err != nil {
		t.Fatalf("trace graph: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Graph: orphan-graph", "Coverage: partial",
		"预期分片 graph_orphan-g.jsonl 不存在", "没有可追溯的事件",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺失分片输出缺少 %q:\n%s", want, got)
		}
	}
}

// TestCmdGraphDegradedOnCorruptSnapshot snapshot 损坏 → degraded；
// 头部字段由事件重建，时间线照常展示。
func TestCmdGraphDegradedOnCorruptSnapshot(t *testing.T) {
	traceDir := t.TempDir()
	stateDir := t.TempDir()
	base := time.Date(2026, 8, 3, 15, 0, 0, 0, time.UTC)
	writeGraphShard(t, traceDir, "graph_deploy-p.jsonl", graphFixtureEvents(base, "deploy-pipeline"))
	writeGraphSnapshot(t, stateDir, "deploy-pipeline", `{"version": 1, "graph_id": "deploy-pip`)

	var out bytes.Buffer
	if err := CLI([]string{"graph", "deploy-pipeline"}, traceDir, stateDir, &out); err != nil {
		t.Fatalf("trace graph: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Coverage: degraded", "snapshot 不可读",
		"Revision: 1", "Digest: aaaabbbbcccc", // 由 graph_submitted 事件重建
		"Status: completed",
		"graph_submitted", "graph_ended", // 时间线照常
	} {
		if !strings.Contains(got, want) {
			t.Errorf("degraded 输出缺少 %q:\n%s", want, got)
		}
	}
}

// TestCmdGraphSubgraphWithSlash 子图（graph_id 含 "/"）定位：writer 消毒
// 后的分片名可查；父图查询给出候选；node 命令在最后一个 "/" 处切分。
func TestCmdGraphSubgraphWithSlash(t *testing.T) {
	traceDir := t.TempDir()
	base := time.Date(2026, 8, 3, 16, 0, 0, 0, time.UTC)
	// 父图 g1 与子图 g1/check@1 各落自己的分片（子图分片名经消毒）。
	writeGraphShard(t, traceDir, graphShardFileName("g1"), []Event{
		{Timestamp: base, Kind: KindGraphSubmitted, GraphID: "g1", Description: "revision=1 digest=pppp"},
	})
	writeGraphShard(t, traceDir, graphShardFileName("g1/check@1"), []Event{
		{Timestamp: base.Add(time.Second), Kind: KindNodeActivationCreated, GraphID: "g1/check@1", NodeID: "v", ActivationID: "v@1"},
		{Timestamp: base.Add(2 * time.Second), Kind: KindGraphEnded, GraphID: "g1/check@1"},
	})

	// 完整子图 ID 精确定位。
	var out bytes.Buffer
	if err := CLI([]string{"graph", "g1/check@1"}, traceDir, "", &out); err != nil {
		t.Fatalf("trace graph 子图: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Graph: g1/check@1", "Status: completed", "node=v", "Coverage: complete"} {
		if !strings.Contains(got, want) {
			t.Errorf("子图输出缺少 %q:\n%s", want, got)
		}
	}
	// 短前缀命中父子两图 → 候选列表。
	var amb bytes.Buffer
	if err := CLI([]string{"graph", "g1"}, traceDir, "", &amb); err != nil {
		t.Fatalf("trace graph 父子前缀: %v", err)
	}
	if !strings.Contains(amb.String(), "找到 2 个匹配的图") {
		t.Errorf("父子图前缀应列候选:\n%s", amb.String())
	}
	// node 命令：graph_id 含 /，在最后一个 / 处切分。
	var node bytes.Buffer
	if err := CLI([]string{"node", "g1/check@1/v"}, traceDir, "", &node); err != nil {
		t.Fatalf("trace node 子图: %v", err)
	}
	if !strings.Contains(node.String(), "Node: v") || !strings.Contains(node.String(), "activation v@1") {
		t.Errorf("子图 node 视图不符:\n%s", node.String())
	}
}

// TestCmdGraphUnknownGraph 未知图中文报错；node 缺参数报用法。
func TestCmdGraphUnknownGraph(t *testing.T) {
	traceDir := t.TempDir()
	if err := CLI([]string{"graph", "nope"}, traceDir, "", &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "未找到匹配 graph_id=nope 的图") {
		t.Fatalf("未知图应中文报错，err=%v", err)
	}
	if err := CLI([]string{"node"}, traceDir, "", &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "trace node <graph_id>/<node_id>") {
		t.Fatalf("node 缺参数应报用法，err=%v", err)
	}
	if err := CLI([]string{"node", "noslash"}, traceDir, "", &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "trace node <graph_id>/<node_id>") {
		t.Fatalf("node 无 / 应报用法，err=%v", err)
	}
}

// TestGraphShardFileNameSanitizes 分片命名：与 writer 实现对齐，
// 路径敌对字符（/、\、:）替换为 "~"。
func TestGraphShardFileNameSanitizes(t *testing.T) {
	for _, tc := range []struct {
		graphID, want string
	}{
		{"graph-1234abcd", "graph_graph-12.jsonl"}, // 无前敌对字符：沿用旧名
		{"g1", "graph_g1.jsonl"},
		{"g1/check@1", "graph_g1~check.jsonl"},              // 子图：/ → ~
		{"a:bcdefg", "graph_a~bcdefg.jsonl"},                // Windows 非法字符 : → ~
		{"deploy-pipeline/check@1", "graph_deploy-p.jsonl"}, // 前 8 位无 /：父子同片
	} {
		if got := graphShardFileName(tc.graphID); got != tc.want {
			t.Errorf("graphShardFileName(%q)=%q, want %q", tc.graphID, got, tc.want)
		}
	}
}

// TestWriterSubgraphShardPersisted writer 端到端：子图事件（ID 含 /）
// 真实落盘到消毒后的分片文件（此前这类事件因目录不存在被静默丢弃）。
func TestWriterSubgraphShardPersisted(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.Emit(Event{Kind: KindNodeActivationCreated, GraphID: "g1/check@1", NodeID: "v", ActivationID: "v@1"})

	data, err := os.ReadFile(filepath.Join(dir, "graph_g1~check.jsonl"))
	if err != nil {
		t.Fatalf("子图分片应已落盘: %v", err)
	}
	if !strings.Contains(string(data), `"graph_id":"g1/check@1"`) {
		t.Fatalf("子图分片内容不符: %s", data)
	}
}

// TestScanGraphStateDirNested 子图的 state 目录嵌套在父图目录下，
// 发现结果为斜杠形式的完整 graph_id。
func TestScanGraphStateDirNested(t *testing.T) {
	stateDir := t.TempDir()
	writeGraphSnapshot(t, stateDir, "g1/check@1", `{"version":1,"graph_id":"g1/check@1","revision":1,"state_version":2,"digest":"ddddddeeeeee","doc":{"status":"running"}}`)

	found := scanGraphStateDir(stateDir)
	entry, ok := found["g1/check@1"]
	if !ok || entry.snapshotPath == "" {
		t.Fatalf("嵌套子图未发现: %+v", found)
	}
	head, snapOK, snapErr := readGraphSnapshotHead(entry.snapshotPath, "g1/check@1")
	if snapErr != nil || !snapOK {
		t.Fatalf("snapshot 读取失败: ok=%v err=%v", snapOK, snapErr)
	}
	if head.Revision != 1 || head.StateVersion != 2 || head.Doc == nil || head.Doc.Status != "running" {
		t.Fatalf("snapshot 头不符: %+v", head)
	}
	// graph_id 不符的 snapshot 视为不可读（degraded）。
	if _, ok, err := readGraphSnapshotHead(entry.snapshotPath, "g1/other@1"); err == nil || ok {
		t.Fatalf("graph_id 不符应报错: ok=%v err=%v", ok, err)
	}
}

func TestGraphFactsTypedTerminalOutcomeNeverCollapsesToCompleted(t *testing.T) {
	for _, test := range []struct {
		outcome string
		want    string
	}{
		{outcome: "success", want: "completed"},
		{outcome: "failed", want: "failed"},
		{outcome: "blocked", want: "blocked"},
		{outcome: "cancelled", want: "cancelled"},
		{outcome: "corrupt", want: "invalid_outcome"},
	} {
		records := []traceEventRecord{{event: Event{
			Kind: KindGraphEnded, GraphID: "g-typed", GraphOutcome: test.outcome,
		}}}
		status, _, _, _ := graphFactsFromEvents(records)
		if status != test.want {
			t.Errorf("graph_outcome=%s 投影 status=%s，want %s", test.outcome, status, test.want)
		}
		// trace 的新终态晚于旧 snapshot；blocked/cancelled 不得被旧 completed 覆盖。
		if got := resolveGraphStatus(status, "completed"); got != test.want {
			t.Errorf("event=%s snapshot=completed 合并为 %s", test.want, got)
		}
	}
}

func TestCmdGraphDisplaysTypedOutcome(t *testing.T) {
	traceDir := t.TempDir()
	base := time.Date(2026, 8, 22, 1, 2, 3, 0, time.UTC)
	writeGraphShard(t, traceDir, "graph_g-typed.jsonl", []Event{
		{Timestamp: base, Kind: KindGraphSubmitted, GraphID: "g-typed", Description: "revision=1 digest=aaaa"},
		{Timestamp: base.Add(time.Second), Kind: KindGraphEnded, GraphID: "g-typed", GraphOutcome: "blocked"},
	})
	var out bytes.Buffer
	if err := CLI([]string{"graph", "g-typed"}, traceDir, "", &out); err != nil {
		t.Fatalf("trace graph: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Status: blocked") || !strings.Contains(got, "Outcome: blocked") ||
		strings.Contains(got, "Status: completed") {
		t.Fatalf("typed blocked outcome 显示错误:\n%s", got)
	}
}
