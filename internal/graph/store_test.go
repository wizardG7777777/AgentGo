package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================
// 测试辅助
// ============================================================

// newTestStore 创建以 t.TempDir() 为根的 Store；t.Cleanup 先 Close
// （Windows 文件句柄硬约束：先关闭再让 TempDir 清理）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustSubmit 提交 JSON 文档并断言成功。
func mustSubmit(t *testing.T, s *Store, data string) {
	t.Helper()
	if err := s.SubmitGraph(mustParse(t, data)); err != nil {
		t.Fatalf("SubmitGraph 应成功: %v", err)
	}
}

// mustGet 读取图并断言存在。
func mustGet(t *testing.T, s *Store, graphID string) *GraphDocument {
	t.Helper()
	doc, ok := s.Get(graphID)
	if !ok {
		t.Fatalf("图 %s 应存在", graphID)
	}
	return doc
}

// mustMutate 断言变更成功。
func mustMutate(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("变更应成功: %v", err)
	}
}

// closeStore 关闭 Store 并断言成功。
func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close 应成功: %v", err)
	}
}

// reopenStore 关闭并重建同目录 Store + Recover（模拟重启，断言恢复无告警）。
func reopenStore(t *testing.T, s *Store) *Store {
	t.Helper()
	dir := s.dir
	closeStore(t, s)
	ns, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if err := ns.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	return ns
}

// recoverExpectWarn 重建 Store 并断言 Recover 返回告警/错误（图可能仍已恢复）。
func recoverExpectWarn(t *testing.T, dir string) (*Store, error) {
	t.Helper()
	ns, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	rerr := ns.Recover()
	if rerr == nil {
		t.Fatalf("Recover 应返回告警/错误")
	}
	return ns, rerr
}

// assertDocEqual 以规范化 JSON 比较两份文档逐字段一致。
func assertDocEqual(t *testing.T, want, got *GraphDocument) {
	t.Helper()
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("序列化 want: %v", err)
	}
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("序列化 got: %v", err)
	}
	if !bytes.Equal(w, g) {
		t.Errorf("文档逐字段不一致:\nwant: %s\ngot:  %s", w, g)
	}
}

func strPtr(s string) *string { return &s }

// setCompactThresholds 调小压缩阈值（测试后恢复），快速触发压缩。
func setCompactThresholds(t *testing.T, maxEntries, maxBytes int64) {
	t.Helper()
	oldE, oldB := journalCompactMaxEntries, journalCompactMaxBytes
	journalCompactMaxEntries, journalCompactMaxBytes = maxEntries, maxBytes
	t.Cleanup(func() { journalCompactMaxEntries, journalCompactMaxBytes = oldE, oldB })
}

// readJournalEntries 读取图目录下的 journal 条目（边界处归一 CRLF）。
func readJournalEntries(t *testing.T, graphDir string) []journalEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(graphDir, journalFileName))
	if err != nil {
		t.Fatalf("读 journal: %v", err)
	}
	var out []journalEntry
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var je journalEntry
		if err := json.Unmarshal([]byte(line), &je); err != nil {
			t.Fatalf("journal 行应可解析: %v", err)
		}
		out = append(out, je)
	}
	return out
}

// fakeSink 是可编程失败的 journalSink，用于制造 persistence-degraded。
type fakeSink struct {
	appendErr error
	resetErr  error
	lines     [][]byte
}

func (f *fakeSink) append(line []byte) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.lines = append(f.lines, append([]byte(nil), line...))
	return nil
}

func (f *fakeSink) reset() error { return f.resetErr }
func (f *fakeSink) close() error { return nil }

// swapJournal 替换 entry 的 journal 实现（先关闭真实 writer，避免 Windows
// 句柄滞留导致 TempDir 清理失败）。
func swapJournal(t *testing.T, s *Store, graphID string, sink journalSink) {
	t.Helper()
	s.mu.RLock()
	e, ok := s.entries[graphID]
	s.mu.RUnlock()
	if !ok {
		t.Fatalf("图 %s 应存在", graphID)
	}
	e.mu.Lock()
	_ = e.journal.close()
	e.journal = sink
	e.mu.Unlock()
}

// ============================================================
// Submit / Get
// ============================================================

// TestStoreSubmitGetDeepCopy Submit 后 Get 返回深拷贝，改写返回值不影响 Store。
func TestStoreSubmitGetDeepCopy(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)

	doc := mustGet(t, s, "g1")
	if doc.Revision != 1 || doc.StateVersion != 0 {
		t.Errorf("初始版本应归一：revision=1 state_version=0，实际 revision=%d state_version=%d",
			doc.Revision, doc.StateVersion)
	}

	// 改写返回值：定义字段、运行字段、节点表、root 全面污染
	n := doc.Nodes["a"]
	n.Task = &NodeTask{Title: "被改写"}
	n.Status = NodeCompleted
	doc.Nodes["a"] = n
	doc.Nodes["zzz"] = Node{Kind: KindEnd}
	doc.Root = "zzz"
	doc.Status = GraphCompleted
	doc.StateVersion = 999

	fresh := mustGet(t, s, "g1")
	if fresh.Root != "a" || fresh.Status != GraphPending || fresh.StateVersion != 0 {
		t.Errorf("Store 内文档被返回值污染: root=%q status=%q state_version=%d", fresh.Root, fresh.Status, fresh.StateVersion)
	}
	if len(fresh.Nodes) != 2 {
		t.Errorf("节点表被返回值污染: 节点数=%d", len(fresh.Nodes))
	}
	a := fresh.Nodes["a"]
	if a.Task.Title != "做 A" || a.Status != NodeInactive {
		t.Errorf("节点 a 被返回值污染: title=%q status=%q", a.Task.Title, a.Status)
	}
	if d, ok := s.Digest("g1"); !ok || len(d) != 64 {
		t.Errorf("Digest 应返回 64 位 hex: %q ok=%v", d, ok)
	}
}

// TestStoreSubmitDuplicateRejected 重复 Submit 同 graph_id 拒绝。
func TestStoreSubmitDuplicateRejected(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	err := s.SubmitGraph(mustParse(t, tinyDocJSON))
	if !errors.Is(err, ErrGraphExists) {
		t.Fatalf("重复 Submit 应返回 ErrGraphExists，实际: %v", err)
	}
}

// TestStoreSubmitInvalidAndNormalizes 非法文档拒绝；版本字段归一。
func TestStoreSubmitInvalidAndNormalizes(t *testing.T) {
	s := newTestStore(t)
	if err := s.SubmitGraph(nil); err == nil {
		t.Errorf("nil 文档应拒绝")
	}
	bad := &GraphDocument{Schema: "wrong", GraphID: "x", Root: "a", Nodes: map[string]Node{}}
	err := s.SubmitGraph(bad)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Errorf("非法文档应返回 *ValidationError，实际 %T（%v）", err, err)
	}
	// exampleDocJSON 自带 revision=1 state_version=0，提交后保持 1 / 0。
	mustSubmit(t, s, exampleDocJSON)
	doc := mustGet(t, s, "graph-123")
	if doc.Revision != 1 || doc.StateVersion != 0 {
		t.Errorf("提交后版本应归一：revision=1 state_version=0，实际 %d / %d", doc.Revision, doc.StateVersion)
	}
	if doc.Status != GraphPending {
		t.Errorf("提交的初始 status 应为 pending: %q", doc.Status)
	}
}

// TestStoreSubmitPersistenceFailure journal 路径被预先破坏时 Submit 失败且图不入索引。
func TestStoreSubmitPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	// 预先破坏 journal 路径：graph 目录已存在且 journal.jsonl 是目录。
	if err := os.MkdirAll(filepath.Join(dir, "g1", journalFileName), 0o755); err != nil {
		t.Fatalf("造目录: %v", err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SubmitGraph(mustParse(t, tinyDocJSON)); err == nil {
		t.Fatalf("journal 路径被破坏时 Submit 应失败")
	}
	if _, ok := s.Get("g1"); ok {
		t.Errorf("Submit 失败时图不得入索引")
	}
	if n := len(s.List()); n != 0 {
		t.Errorf("Submit 失败后 List 应为空，实际 %d", n)
	}
}

// ============================================================
// PatchGraph（定义面）
// ============================================================

// TestStorePatchRevisionConflict baseRevision 不符 → ErrRevisionConflict 携带当前 revision。
func TestStorePatchRevisionConflict(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	patch := DefinitionPatch{UpsertNodes: []NodeDefUpsert{
		{ID: "a", Kind: KindAgent, Task: &NodeTask{Title: "做 A v2"}, Next: []Transition{{To: "b"}}},
	}}
	_, err := s.PatchGraph("g1", 99, patch)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("baseRevision 不符应返回 ErrRevisionConflict，实际: %v", err)
	}
	var rce *RevisionConflictError
	if !errors.As(err, &rce) {
		t.Fatalf("错误应可取 *RevisionConflictError，实际 %T", err)
	}
	if rce.Base != 99 || rce.Current != 1 {
		t.Errorf("冲突详情应为 Base=99 Current=1，实际 Base=%d Current=%d", rce.Base, rce.Current)
	}
	if _, err := s.PatchGraph("zzz", 1, patch); !errors.Is(err, ErrGraphNotFound) {
		t.Errorf("图不存在应返回 ErrGraphNotFound，实际: %v", err)
	}
	// 冲突不污染内存
	doc := mustGet(t, s, "g1")
	if doc.Revision != 1 || doc.Nodes["a"].Task.Title != "做 A" {
		t.Errorf("冲突后内存不得前进: revision=%d title=%q", doc.Revision, doc.Nodes["a"].Task.Title)
	}
}

// TestStorePatchDefinitionChanges 定义变化 → revision+1 / digest 变；覆盖
// upsert 修改、upsert 新增、删除节点、改 root、空 patch 拒绝。
func TestStorePatchDefinitionChanges(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	d0, _ := s.Digest("g1")

	// 1) upsert 修改 a + 新增 c（end），a.next 增加 to c
	rev, err := s.PatchGraph("g1", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{
		{ID: "a", Kind: KindAgent, Task: &NodeTask{Title: "做 A v2"}, Next: []Transition{{To: "b"}, {To: "c"}}},
		{ID: "c", Kind: KindEnd, Task: &NodeTask{Title: "结束 C"}},
	}})
	mustMutate(t, err)
	if rev != 2 {
		t.Fatalf("新 revision 应为 2，实际 %d", rev)
	}
	doc := mustGet(t, s, "g1")
	if doc.StateVersion != 1 {
		t.Errorf("patch 后 state_version 应为 1，实际 %d", doc.StateVersion)
	}
	if doc.Nodes["a"].Task.Title != "做 A v2" || len(doc.Nodes["a"].Next) != 2 {
		t.Errorf("节点 a 定义未更新: %+v", doc.Nodes["a"])
	}
	if c := doc.Nodes["c"]; c.Kind != KindEnd || c.Status != NodeInactive || c.Executor != nil || c.Execution != nil {
		t.Errorf("新节点 c 运行字段应从零开始: %+v", c)
	}
	d1, _ := s.Digest("g1")
	if d1 == d0 {
		t.Errorf("定义变化必须改变 digest")
	}

	// 2) 删除 b，同时把 a.next 收窄到 c
	rev, err = s.PatchGraph("g1", 2, DefinitionPatch{
		RemoveNodes: []string{"b"},
		UpsertNodes: []NodeDefUpsert{
			{ID: "a", Kind: KindAgent, Task: &NodeTask{Title: "做 A v2"}, Next: []Transition{{To: "c"}}},
		},
	})
	mustMutate(t, err)
	if rev != 3 {
		t.Fatalf("新 revision 应为 3，实际 %d", rev)
	}
	doc = mustGet(t, s, "g1")
	if _, ok := doc.Nodes["b"]; ok {
		t.Errorf("节点 b 应已删除")
	}

	// 3) 改 root：新增 top（→a）并切 root
	rev, err = s.PatchGraph("g1", 3, DefinitionPatch{
		UpsertNodes: []NodeDefUpsert{
			{ID: "top", Kind: KindController, Task: &NodeTask{Title: "顶层"}, Next: []Transition{{To: "a"}}},
		},
		Root: strPtr("top"),
	})
	mustMutate(t, err)
	if rev != 4 {
		t.Fatalf("新 revision 应为 4，实际 %d", rev)
	}
	if doc := mustGet(t, s, "g1"); doc.Root != "top" {
		t.Errorf("root 应改为 top，实际 %q", doc.Root)
	}

	// 4) 空 patch 拒绝
	if _, err := s.PatchGraph("g1", 4, DefinitionPatch{}); err == nil {
		t.Errorf("空 patch 应拒绝")
	}
}

// TestStorePatchRevalidatesSemantics patch 应用后重跑语义校验链：
// root/引用/可达性/转移形态违规一律拒绝且内存不前进。
func TestStorePatchRevalidatesSemantics(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	d0, _ := s.Digest("g1")

	cases := map[string]DefinitionPatch{
		"root指向不存在节点": {Root: strPtr("zzz")},
		"next悬空引用": {UpsertNodes: []NodeDefUpsert{
			{ID: "a", Kind: KindAgent, Task: &NodeTask{Title: "做 A"}, Next: []Transition{{To: "zzz"}}},
		}},
		"新节点不可达": {UpsertNodes: []NodeDefUpsert{
			{ID: "c", Kind: KindEnd, Task: &NodeTask{Title: "C"}},
		}},
		"删除后next悬空": {RemoveNodes: []string{"b"}},
		"非end节点无next": {UpsertNodes: []NodeDefUpsert{
			{ID: "a", Kind: KindAgent, Task: &NodeTask{Title: "做 A"}},
		}},
		"join不透传ready事件": {UpsertNodes: []NodeDefUpsert{
			{ID: "a", Kind: KindJoin, Task: &NodeTask{Title: "汇合"}, Next: []Transition{{
				To: "b", When: &Condition{Event: EventReady},
			}}},
		}},
		"删除不存在节点": {RemoveNodes: []string{"zzz"}},
		"upsert空节点ID": {UpsertNodes: []NodeDefUpsert{
			{ID: "", Kind: KindEnd, Task: &NodeTask{Title: "X"}},
		}},
	}
	for name, patch := range cases {
		if _, err := s.PatchGraph("g1", 1, patch); err == nil {
			t.Errorf("%s: 应被拒绝", name)
		}
	}
	doc := mustGet(t, s, "g1")
	if doc.Revision != 1 || doc.StateVersion != 0 || len(doc.Nodes) != 2 {
		t.Errorf("全部拒绝后内存不得前进: revision=%d state_version=%d 节点数=%d",
			doc.Revision, doc.StateVersion, len(doc.Nodes))
	}
	if d, _ := s.Digest("g1"); d != d0 {
		t.Errorf("全部拒绝后 digest 不得变化")
	}
}

// TestRecoverLegacyJoinEventContract 保证 authoring 加固不改写 durable 历史：
// 老版本已经接受的 join→ready 图仍可恢复并供 trace/read_graph 审计；只有新建
// 或 patch 定义时才要求修正为 join→completed。
func TestRecoverLegacyJoinEventContract(t *testing.T) {
	root := t.TempDir()
	doc := &GraphDocument{
		Schema: SchemaV1, GraphID: "g-legacy-join-ready", Revision: 1,
		Root: "join", Status: GraphPending,
		Nodes: map[string]Node{
			"join": {
				Kind: KindJoin, Task: &NodeTask{Title: "旧汇合"}, Status: NodeInactive,
				Next: []Transition{{To: "done", When: &Condition{Event: EventReady}}},
			},
			"done": {Kind: KindEnd, Task: &NodeTask{Title: "结束"}, Status: NodeInactive},
		},
	}
	graphDir := filepath.Join(root, doc.GraphID)
	if err := os.MkdirAll(graphDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line, _, _, err := buildJournalLine(1, journalKindSubmit, doc, submitPayload{Doc: doc}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(graphDir, journalFileName), append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Recover(); err != nil {
		t.Fatalf("旧 join→ready durable 图应保持可恢复: %v", err)
	}
	got, ok := s.Get(doc.GraphID)
	if !ok || got.Nodes["join"].Next[0].When.Event != EventReady {
		t.Fatalf("恢复应原样保留旧定义用于审计: ok=%v graph=%+v", ok, got)
	}
}

// TestStorePatchPreservesRuntimeFields patch 后既有 status/executor/execution
// 原样保留（字段所有权的行为断言；编译期已由 DefinitionPatch 类型保证）。
func TestStorePatchPreservesRuntimeFields(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeReady, 0))
	mustMutate(t, s.SetExecutor("g1", "a", Executor{Type: ExecutorTypeAgent, AgentID: "runner-1"}, 1))
	mustMutate(t, s.SetExecution("g1", "a", Execution{
		Phase: "executing", TaskID: "task-1", ActivationID: "a@1",
		ResultRef: "result/a@1", EvidenceRefs: []string{"ev-1"},
	}, 2))

	rev, err := s.PatchGraph("g1", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{
		{ID: "a", Kind: KindAgent, Task: &NodeTask{Title: "做 A v2"}, Next: []Transition{{To: "b"}}},
	}})
	mustMutate(t, err)
	if rev != 2 {
		t.Fatalf("新 revision 应为 2，实际 %d", rev)
	}
	a := mustGet(t, s, "g1").Nodes["a"]
	if a.Task.Title != "做 A v2" {
		t.Errorf("task 应被 patch 更新: %q", a.Task.Title)
	}
	if a.Status != NodeReady {
		t.Errorf("status 不得被 patch 触碰: %q", a.Status)
	}
	if a.Executor == nil || a.Executor.AgentID != "runner-1" {
		t.Errorf("executor 不得被 patch 触碰: %+v", a.Executor)
	}
	if a.Execution == nil || a.Execution.TaskID != "task-1" || a.Execution.ResultRef != "result/a@1" {
		t.Errorf("execution 不得被 patch 触碰: %+v", a.Execution)
	}
}

// ============================================================
// 运行面写入（状态机 + CAS）
// ============================================================

// TestStoreNodeStatusMachine 非法节点迁移被拒；blocked→ready 合法；CAS 生效。
func TestStoreNodeStatusMachine(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)

	// 非法：inactive → running（必须先经 ready）
	if err := s.SetNodeStatus("g1", "a", NodeRunning, 0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("inactive→running 应返回 ErrInvalidTransition，实际: %v", err)
	}
	if doc := mustGet(t, s, "g1"); doc.StateVersion != 0 {
		t.Fatalf("非法迁移不得推进 state_version，实际 %d", doc.StateVersion)
	}

	// 合法链：inactive→ready→running→blocked→ready（replan 修回）
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeReady, 0))
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeRunning, 1))
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeBlocked, 2))
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeReady, 3))
	doc := mustGet(t, s, "g1")
	if doc.Nodes["a"].Status != NodeReady || doc.StateVersion != 4 {
		t.Errorf("合法链后 a 应为 ready、state_version=4，实际 %q / %d", doc.Nodes["a"].Status, doc.StateVersion)
	}

	// CAS：过期 state_version 冲突，携带当前值
	err := s.SetNodeStatus("g1", "a", NodeCancelled, 2)
	if !errors.Is(err, ErrStateVersionConflict) {
		t.Fatalf("过期 state_version 应返回 ErrStateVersionConflict，实际: %v", err)
	}
	var sve *StateVersionConflictError
	if !errors.As(err, &sve) || sve.Current != 4 || sve.Base != 2 {
		t.Errorf("冲突详情应为 Base=2 Current=4，实际 %+v", sve)
	}

	// 节点不存在
	if err := s.SetNodeStatus("g1", "zzz", NodeReady, 4); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("节点不存在应返回 ErrNodeNotFound，实际: %v", err)
	}
}

// TestStoreGraphStatusMachine 非法图迁移被拒；合法全链通过；终态无出边。
func TestStoreGraphStatusMachine(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)

	// 非法：pending → completed（不得跳过 running）
	if err := s.SetGraphStatus("g1", GraphCompleted, 0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pending→completed 应返回 ErrInvalidTransition，实际: %v", err)
	}
	// 合法全链：pending→running→paused→running→completed
	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 0))
	mustMutate(t, s.SetGraphStatus("g1", GraphPaused, 1))
	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 2))
	mustMutate(t, s.SetGraphStatus("g1", GraphCompleted, 3))
	doc := mustGet(t, s, "g1")
	if doc.Status != GraphCompleted || doc.StateVersion != 4 {
		t.Errorf("合法链后应为 completed、state_version=4，实际 %q / %d", doc.Status, doc.StateVersion)
	}
	// 终态无出边
	if err := s.SetGraphStatus("g1", GraphFailed, 4); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("终态出边应返回 ErrInvalidTransition，实际: %v", err)
	}
	// 图级 CAS 冲突
	if err := s.SetGraphStatus("g1", GraphCancelled, 0); !errors.Is(err, ErrStateVersionConflict) {
		t.Errorf("过期 state_version 应返回 ErrStateVersionConflict，实际: %v", err)
	}
}

// TestStoreExecutorExecutionValidation executor/execution 的形状校验与节点存在性。
func TestStoreExecutorExecutionValidation(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)

	if err := s.SetExecutor("g1", "a", Executor{Type: "human", AgentID: "x"}, 0); err == nil {
		t.Errorf("executor.type 非 agent 应拒绝")
	}
	if err := s.SetExecutor("g1", "a", Executor{Type: ExecutorTypeAgent, AgentID: " "}, 0); err == nil {
		t.Errorf("executor.agent_id 为空应拒绝")
	}
	if err := s.SetExecution("g1", "a", Execution{TaskID: "task-1"}, 0); err == nil {
		t.Errorf("execution.phase 为空应拒绝")
	}
	if err := s.SetExecutor("g1", "zzz", Executor{Type: ExecutorTypeAgent, AgentID: "x"}, 0); !errors.Is(err, ErrNodeNotFound) {
		t.Errorf("节点不存在应返回 ErrNodeNotFound，实际: %v", err)
	}
	// 形状校验失败不得推进 state_version
	if doc := mustGet(t, s, "g1"); doc.StateVersion != 0 {
		t.Errorf("形状校验失败不得推进 state_version，实际 %d", doc.StateVersion)
	}
}

// TestStoreStateOpsKeepRevisionAndDigest 非定义变化 API：revision 不变、
// state_version+1、digest 不变（V6 §6-14）。
func TestStoreStateOpsKeepRevisionAndDigest(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	d0, _ := s.Digest("g1")

	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 0))
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeReady, 1))
	mustMutate(t, s.SetExecutor("g1", "a", Executor{Type: ExecutorTypeAgent, AgentID: "runner-1"}, 2))
	mustMutate(t, s.SetExecution("g1", "a", Execution{Phase: "executing", TaskID: "task-1"}, 3))

	doc := mustGet(t, s, "g1")
	if doc.Revision != 1 || doc.StateVersion != 4 {
		t.Errorf("运行面写入后应为 revision=1 state_version=4，实际 %d / %d", doc.Revision, doc.StateVersion)
	}
	if d, _ := s.Digest("g1"); d != d0 {
		t.Errorf("状态刷新不得改变 digest:\n  d0=%s\n  d1=%s", d0, d)
	}
	if got := ComputeDigest(doc); got != d0 {
		t.Errorf("Get 文档重算 digest 应不变: %s", got)
	}
	if a := doc.Nodes["a"]; a.Executor == nil || a.Execution == nil || a.Status != NodeReady {
		t.Errorf("运行字段应已写入: %+v", a)
	}
}

// ============================================================
// 持久化与恢复
// ============================================================

// TestStorePersistenceRoundTrip Submit+多次变更后 Close，重启 Recover →
// doc 逐字段一致、digest 一致，且恢复后可立即继续读写。
func TestStorePersistenceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, exampleDocJSON) // graph-123，4 节点含 verify→implement 回边
	mustMutate(t, s.SetGraphStatus("graph-123", GraphRunning, 0))

	rev, err := s.PatchGraph("graph-123", 1, DefinitionPatch{UpsertNodes: []NodeDefUpsert{
		{ID: "implement", Kind: KindAgent,
			Task: &NodeTask{Title: "实施修改 v2", Description: "提交 result object"},
			Next: []Transition{{To: "verify", When: &Condition{Event: EventCompleted}}}},
	}})
	mustMutate(t, err)
	if rev != 2 {
		t.Fatalf("新 revision 应为 2，实际 %d", rev)
	}
	mustMutate(t, s.SetNodeStatus("graph-123", "implement", NodeReady, 2))
	mustMutate(t, s.SetExecutor("graph-123", "implement", Executor{Type: ExecutorTypeAgent, AgentID: "runner-7"}, 3))
	mustMutate(t, s.SetExecution("graph-123", "implement", Execution{
		Phase: "executing", TaskID: "task-9", ActivationID: "implement@1",
		ResultRef: "result/implement@1", EvidenceRefs: []string{"ev-1", "ev-2"},
	}, 4))
	mustMutate(t, s.SetGraphStatus("graph-123", GraphPaused, 5))
	mustMutate(t, s.SetGraphStatus("graph-123", GraphRunning, 6))

	want := mustGet(t, s, "graph-123")
	d0, _ := s.Digest("graph-123")

	ns := reopenStore(t, s)
	got := mustGet(t, ns, "graph-123")
	assertDocEqual(t, want, got)
	if d, _ := ns.Digest("graph-123"); d != d0 {
		t.Errorf("恢复后 digest 应一致:\n  d0=%s\n  d1=%s", d0, d)
	}
	// 恢复后可立即正常读写
	rev, err = ns.PatchGraph("graph-123", got.Revision, DefinitionPatch{UpsertNodes: []NodeDefUpsert{
		{ID: "implement", Kind: KindAgent,
			Task: &NodeTask{Title: "实施修改 v3", Description: "提交 result object"},
			Next: []Transition{{To: "verify", When: &Condition{Event: EventCompleted}}}},
	}})
	mustMutate(t, err)
	if rev != 3 {
		t.Errorf("恢复后 patch 应得 revision=3，实际 %d", rev)
	}
	mustMutate(t, ns.SetNodeStatus("graph-123", "implement", NodeRunning, mustGet(t, ns, "graph-123").StateVersion))
}

// TestStoreCompactionAndRecover journal 超阈值触发压缩（snapshot+journal 混合
// 重放）；journal 截断后 seq 连续；恢复后文档一致且续写 seq 连续。
func TestStoreCompactionAndRecover(t *testing.T) {
	setCompactThresholds(t, 5, 1<<20)
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)

	// 8 次图状态往返：seq 2..9；第 5 次后（seq6，条目数 6>5）触发压缩。
	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 0))
	to := GraphPaused
	for i := 1; i < 8; i++ {
		doc := mustGet(t, s, "g1")
		mustMutate(t, s.SetGraphStatus("g1", to, doc.StateVersion))
		if to == GraphPaused {
			to = GraphRunning
		} else {
			to = GraphPaused
		}
	}
	want := mustGet(t, s, "g1") // status=paused, state_version=8, seq=9
	d0, _ := s.Digest("g1")
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	// snapshot 存在且 seq=6；journal 截断后只剩 seq 7..9 且连续。
	snapData, err := os.ReadFile(filepath.Join(gdir, snapshotFileName))
	if err != nil {
		t.Fatalf("压缩后 snapshot.json 应存在: %v", err)
	}
	var snap snapshotFile
	if err := json.Unmarshal(snapData, &snap); err != nil {
		t.Fatalf("snapshot 应可解析: %v", err)
	}
	if snap.Seq != 6 {
		t.Fatalf("snapshot seq 应为 6，实际 %d", snap.Seq)
	}
	entries := readJournalEntries(t, gdir)
	if len(entries) != 3 {
		t.Fatalf("压缩后 journal 应剩 3 条，实际 %d", len(entries))
	}
	for i, je := range entries {
		wantSeq := snap.Seq + 1 + int64(i)
		if je.Seq != wantSeq {
			t.Fatalf("journal 截断后 seq 应连续：第 %d 条期望 seq=%d，实际 %d", i, wantSeq, je.Seq)
		}
	}

	// snapshot+journal 混合重放：文档一致、digest 一致。
	ns, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if err := ns.Recover(); err != nil {
		t.Fatalf("Recover 应无告警: %v", err)
	}
	got := mustGet(t, ns, "g1")
	assertDocEqual(t, want, got)
	if d, _ := ns.Digest("g1"); d != d0 {
		t.Errorf("恢复后 digest 应一致")
	}

	// 恢复后续写：seq 在截断后的 journal 上连续推进。
	mustMutate(t, ns.SetGraphStatus("g1", GraphRunning, got.StateVersion))
	entries = readJournalEntries(t, gdir)
	if last := entries[len(entries)-1]; last.Seq != 10 {
		t.Errorf("恢复后续写 seq 应为 10，实际 %d", last.Seq)
	}
}

// TestStoreRecoverCorruptJournalTruncates journal 坏行即停：该行及其后丢弃
// （物理截断），以最后一个完整一致状态为准，Recover 返回告警说明。
func TestStoreRecoverCorruptJournalTruncates(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeReady, 0))
	good := mustGet(t, s, "g1")
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	// 追加坏行 + 一条 seq 连续但 digest 篡改的记录（两条都应被丢弃）。
	fake := journalEntry{
		Seq: 3, Kind: journalKindGraphStatus, Revision: 1, StateVersion: 2,
		Digest: "deadbeef", At: time.Now().UTC(),
		Payload: json.RawMessage(`{"to":"running"}`),
	}
	fakeLine, err := json.Marshal(fake)
	if err != nil {
		t.Fatalf("编码伪造记录: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(gdir, journalFileName), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("打开 journal: %v", err)
	}
	if _, err := f.WriteString("{这行是坏行\n" + string(fakeLine) + "\n"); err != nil {
		t.Fatalf("写坏行: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭 journal: %v", err)
	}

	ns, rerr := recoverExpectWarn(t, dir)
	if !strings.Contains(rerr.Error(), "坏行") {
		t.Errorf("告警应说明坏行，实际: %v", rerr)
	}
	got := mustGet(t, ns, "g1")
	assertDocEqual(t, good, got)

	// 坏行及其后已被物理截断：journal 只剩 2 条完整记录。
	entries := readJournalEntries(t, gdir)
	if len(entries) != 2 {
		t.Fatalf("截断后 journal 应剩 2 条，实际 %d", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(gdir, journalFileName))
	if err != nil {
		t.Fatalf("读 journal: %v", err)
	}
	if strings.Contains(string(raw), "deadbeef") {
		t.Errorf("篡改记录应被截断丢弃")
	}

	// 恢复后可继续读写，seq 从最后一个完整状态连续推进。
	mustMutate(t, ns.SetNodeStatus("g1", "a", NodeCancelled, got.StateVersion))
	entries = readJournalEntries(t, gdir)
	if last := entries[len(entries)-1]; last.Seq != 3 {
		t.Errorf("续写 seq 应为 3，实际 %d", last.Seq)
	}
}

// TestStoreRecoverCorruptSnapshotFallsBackToJournal snapshot 损坏时尝试纯
// journal 重放（journal 自 submit 起完整时恢复成功，附告警）。
func TestStoreRecoverCorruptSnapshotFallsBackToJournal(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	mustMutate(t, s.SetNodeStatus("g1", "a", NodeReady, 0))
	want := mustGet(t, s, "g1")
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	// 写入损坏的 snapshot（正常流程在未压缩时本无 snapshot）。
	if err := os.WriteFile(filepath.Join(gdir, snapshotFileName), []byte("{损坏"), 0o600); err != nil {
		t.Fatalf("写坏 snapshot: %v", err)
	}
	ns, rerr := recoverExpectWarn(t, dir)
	if !strings.Contains(rerr.Error(), "snapshot") {
		t.Errorf("告警应说明 snapshot 损坏，实际: %v", rerr)
	}
	got := mustGet(t, ns, "g1")
	assertDocEqual(t, want, got)
}

// TestStoreRecoverCorruptSnapshotAfterCompaction snapshot 损坏且 journal 已被
// 压缩截断（无法从 submit 重放）时，该图不可恢复：Recover 报错且图不入索引。
func TestStoreRecoverCorruptSnapshotAfterCompaction(t *testing.T) {
	setCompactThresholds(t, 3, 1<<20)
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 0))
	mustMutate(t, s.SetGraphStatus("g1", GraphPaused, 1))
	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 2)) // seq4，条目数 4>3 → 压缩
	mustMutate(t, s.SetGraphStatus("g1", GraphPaused, 3))  // seq5
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	if err := os.WriteFile(filepath.Join(gdir, snapshotFileName), []byte("{损坏"), 0o600); err != nil {
		t.Fatalf("写坏 snapshot: %v", err)
	}
	_, rerr := recoverExpectWarn(t, dir)
	if !strings.Contains(rerr.Error(), "snapshot") {
		t.Errorf("告警应说明 snapshot 损坏，实际: %v", rerr)
	}
	// 同一目录再开 Store 验证：图不入索引。
	ns := mustStore(t, dir)
	if _, ok := ns.Get("g1"); ok {
		t.Errorf("snapshot 损坏且 journal 已截断时图不可恢复，不得入索引")
	}
}

// mustStore 打开目录并尝试 Recover（告警在本断言路径中忽略），供断言
// 「图不可恢复」使用；与 recoverExpectWarn 的 Store 均无残留句柄。
func mustStore(t *testing.T, dir string) *Store {
	t.Helper()
	ns, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore 应成功: %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	_ = ns.Recover()
	return ns
}

// ============================================================
// persistence-degraded
// ============================================================

// TestStoreDegradedFailClosed 落盘失败 → 变更返回 ErrDegraded、Degraded 可查、
// 内存不被污染、后续变更 fail-closed、OnDegraded 恰好触发一次、读取仍可用。
func TestStoreDegradedFailClosed(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	var notified []string
	s.OnDegraded = func(graphID string, err error) { notified = append(notified, graphID) }
	swapJournal(t, s, "g1", &fakeSink{appendErr: errors.New("磁盘已拔出")})

	before := mustGet(t, s, "g1")
	err := s.SetNodeStatus("g1", "a", NodeReady, before.StateVersion)
	if !errors.Is(err, ErrDegraded) {
		t.Fatalf("落盘失败应返回 ErrDegraded，实际: %v", err)
	}
	var de *DegradedError
	if !errors.As(err, &de) || de.GraphID != "g1" {
		t.Errorf("应可取 *DegradedError 且 GraphID=g1，实际 %+v", de)
	}
	if derr, ok := s.Degraded("g1"); !ok || derr == nil {
		t.Errorf("Degraded(g1) 应返回首个落盘错误")
	}
	if _, ok := s.Degraded("zzz"); ok {
		t.Errorf("Degraded(zzz) 图不存在应返回 false")
	}

	// 内存未被污染
	after := mustGet(t, s, "g1")
	assertDocEqual(t, before, after)

	// 后续变更 fail-closed（不再触碰底层 sink）
	if err := s.SetGraphStatus("g1", GraphRunning, after.StateVersion); !errors.Is(err, ErrDegraded) {
		t.Errorf("degraded 后变更应 fail-closed 返回 ErrDegraded，实际: %v", err)
	}
	if len(notified) != 1 || notified[0] != "g1" {
		t.Errorf("OnDegraded 应恰好触发一次且为 g1，实际 %v", notified)
	}

	// 读取仍可用，List 反映 degraded
	if _, ok := s.Get("g1"); !ok {
		t.Errorf("degraded 后读取应可用")
	}
	for _, sum := range s.List() {
		if sum.GraphID == "g1" && !sum.Degraded {
			t.Errorf("List 摘要应标记 Degraded")
		}
	}
}

// TestStoreCompactionFailureDegrades 压缩本身失败（snapshot 已写、journal
// 截断失败）→ 触发压缩的当次变更已 durable 生效，图进入 degraded，后续
// 变更 fail-closed。
func TestStoreCompactionFailureDegrades(t *testing.T) {
	setCompactThresholds(t, 1, 1<<20)
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	var notified int
	s.OnDegraded = func(string, error) { notified++ }
	swapJournal(t, s, "g1", &fakeSink{resetErr: errors.New("截断被拒")})

	// 第 2 条 journal 使条目数 2>1 触发压缩；append 成功（fake 接受），
	// reset 失败 → degraded。当次变更已 durable，不得报错。
	if err := s.SetNodeStatus("g1", "a", NodeReady, 0); err != nil {
		t.Fatalf("触发压缩的当次变更已成功落盘，不应报错: %v", err)
	}
	if doc := mustGet(t, s, "g1"); doc.Nodes["a"].Status != NodeReady {
		t.Errorf("触发压缩的当次变更应已生效")
	}
	if _, ok := s.Degraded("g1"); !ok {
		t.Errorf("压缩失败应进入 degraded")
	}
	if notified != 1 {
		t.Errorf("OnDegraded 应触发一次，实际 %d", notified)
	}
	doc := mustGet(t, s, "g1")
	if err := s.SetNodeStatus("g1", "a", NodeCancelled, doc.StateVersion); !errors.Is(err, ErrDegraded) {
		t.Errorf("degraded 后变更应 fail-closed，实际: %v", err)
	}
}

// ============================================================
// 其它行为
// ============================================================

// TestStoreListAndDigest List 摘要按 graph_id 排序且字段完整。
func TestStoreListAndDigest(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)                                                    // g1
	mustSubmit(t, s, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "a2"`)) // a2

	list := s.List()
	if len(list) != 2 || list[0].GraphID != "a2" || list[1].GraphID != "g1" {
		t.Fatalf("List 应按 graph_id 排序返回两图: %+v", list)
	}
	for _, sum := range list {
		if sum.Revision != 1 || sum.StateVersion != 0 || sum.Status != GraphPending ||
			sum.Root != "a" || sum.NodeCount != 2 || sum.Seq != 1 || sum.Degraded {
			t.Errorf("摘要字段不符: %+v", sum)
		}
		doc := mustGet(t, s, sum.GraphID)
		if sum.Digest != ComputeDigest(doc) {
			t.Errorf("摘要 digest 应与文档一致: %s", sum.GraphID)
		}
	}
	if _, ok := s.Digest("zzz"); ok {
		t.Errorf("Digest(zzz) 图不存在应返回 false")
	}
}

// TestStoreCloseRejectsMutations Close 后变更拒绝（ErrStoreClosed），读取仍可用，Close 幂等。
func TestStoreCloseRejectsMutations(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	closeStore(t, s)

	if err := s.SetNodeStatus("g1", "a", NodeReady, 0); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("Close 后变更应返回 ErrStoreClosed，实际: %v", err)
	}
	if err := s.SubmitGraph(mustParse(t, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "a2"`))); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("Close 后 Submit 应返回 ErrStoreClosed，实际: %v", err)
	}
	if _, ok := s.Get("g1"); !ok {
		t.Errorf("Close 后内存读取应可用")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close 应幂等: %v", err)
	}
}

// TestStoreConcurrentMutations 同一图变更经 entry 锁串行（CAS 重试最终一致），
// 不同图并行互不阻塞。
func TestStoreConcurrentMutations(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)                                                    // g1
	mustSubmit(t, s, mutate(t, tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "a2"`)) // a2

	// pending→running→paused→running… 的合法往返迁移目标
	next := map[GraphStatus]GraphStatus{
		GraphPending: GraphRunning,
		GraphRunning: GraphPaused,
		GraphPaused:  GraphRunning,
	}
	var wg sync.WaitGroup
	hammer := func(graphID string, n int) {
		defer wg.Done()
		for i := 0; i < n; i++ {
			for {
				doc, ok := s.Get(graphID)
				if !ok {
					t.Errorf("图 %s 应存在", graphID)
					return
				}
				err := s.SetGraphStatus(graphID, next[doc.Status], doc.StateVersion)
				if errors.Is(err, ErrStateVersionConflict) {
					continue // CAS 冲突：重读重试
				}
				if err != nil {
					t.Errorf("图 %s 变更应成功: %v", graphID, err)
					return
				}
				break
			}
		}
	}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go hammer("g1", 25) // 同图 8×25=200 次
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go hammer("a2", 25) // 另一图并行 2×25=50 次
	}
	wg.Wait()

	if doc := mustGet(t, s, "g1"); doc.StateVersion != 200 {
		t.Errorf("g1 最终 state_version 应为 200，实际 %d", doc.StateVersion)
	}
	if doc := mustGet(t, s, "a2"); doc.StateVersion != 50 {
		t.Errorf("a2 最终 state_version 应为 50，实际 %d", doc.StateVersion)
	}
	// 重启恢复后版本一致（journal 链完整性的端到端佐证）
	ns := reopenStore(t, s)
	if doc := mustGet(t, ns, "g1"); doc.StateVersion != 200 {
		t.Errorf("恢复后 g1 state_version 应为 200，实际 %d", doc.StateVersion)
	}
	if doc := mustGet(t, ns, "a2"); doc.StateVersion != 50 {
		t.Errorf("恢复后 a2 state_version 应为 50，实际 %d", doc.StateVersion)
	}
}
