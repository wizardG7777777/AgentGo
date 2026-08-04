package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestSessionStore 在临时目录建一个 SessionStore；返回的 cleanup 负责
// Close（Windows 约束：先关句柄再让 TempDir 清理——SessionStore 无常驻句柄，
// Close 仅为生命周期标记，仍按纪律显式关闭）。
func newTestSessionStore(t *testing.T) *SessionStore {
	t.Helper()
	s, err := NewSessionStore(filepath.Join(t.TempDir(), "memory.jsonl"))
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sessionEntry(kind Kind, key, content string) Entry {
	return Entry{Scope: ScopeSession, Kind: kind, Key: key, Content: content, Source: "task-x"}
}

// TestSupersede_ReplacesOldAndKeepsAudit 验证同 Key 取代链：新条目接管检索键，
// 旧条目置 superseded + SupersededBy + 审计键改写，且不再被召回。
func TestSupersede_ReplacesOldAndKeepsAudit(t *testing.T) {
	ctx := context.Background()
	s := newTestSessionStore(t)

	if err := s.Put(ctx, sessionEntry(KindDecision, "decision:ab12", "旧结论")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	newEntry := sessionEntry(KindDecision, "decision:ab12", "新结论")
	newEntry.Evidence = []string{"user:request_user_input"}
	supersededID, err := s.Supersede(ctx, newEntry)
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if supersededID == "" {
		t.Fatal("存在同 Key 旧条目时 Supersede 应返回旧条目 ID")
	}
	after, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Count(string(after), "\n"), strings.Count(string(before), "\n")+1; got != want {
		t.Fatalf("Supersede 必须只 append 一条可恢复事务记录: lines=%d want=%d", got, want)
	}
	if got := strings.Count(string(after), `"op":"supersede"`); got != 1 {
		t.Fatalf("Supersede 日志应含一条 op=supersede 事务，got=%d: %s", got, after)
	}

	// 定点查询命中新条目。
	got, err := s.Query(ctx, ScopeSession, KindDecision, "decision:ab12", 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("Query by key = %+v, %v", got, err)
	}
	if got[0].Content != "新结论" || got[0].EffectiveState() != StateConfirmed {
		t.Errorf("新条目应接管检索键且为 confirmed: %+v", got[0])
	}

	// 范围查询两条都在（旧条目保留审计）；旧条目已置 superseded。
	all, err := s.Query(ctx, ScopeSession, KindDecision, "", 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("范围查询应有 2 条（新旧）: %+v, %v", all, err)
	}
	var old *Entry
	for i := range all {
		if all[i].ID == supersededID {
			old = &all[i]
		}
	}
	if old == nil {
		t.Fatalf("旧条目 %s 应保留: %+v", supersededID, all)
	}
	if old.EffectiveState() != StateSuperseded || old.SupersededBy != got[0].ID {
		t.Errorf("旧条目应置 superseded 并记 SupersededBy: %+v", old)
	}
	if old.Key == "decision:ab12" {
		t.Errorf("旧条目检索键应已改写为审计键: %q", old.Key)
	}
	if old.Recalled() {
		t.Error("superseded 条目不得被召回")
	}

	// 无旧条目时 Supersede 返回空 ID。
	id2, err := s.Supersede(ctx, sessionEntry(KindResult, "result:t1", "结果"))
	if err != nil || id2 != "" {
		t.Errorf("无旧条目时 Supersede 应返回空 ID: id=%q err=%v", id2, err)
	}
}

// TestMarkStale_FlagsWithoutDeleting 验证 stale 语义：标记失效但保留审计，
// 召回过滤，幂等。
func TestMarkStale_FlagsWithoutDeleting(t *testing.T) {
	ctx := context.Background()
	s := newTestSessionStore(t)

	if err := s.Put(ctx, sessionEntry(KindLearning, "failure:t1", "失败教训")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	all, _ := s.Query(ctx, ScopeSession, KindLearning, "", 1)
	if len(all) != 1 {
		t.Fatalf("条目应存在: %+v", all)
	}
	id := all[0].ID

	if err := s.MarkStale(ctx, id); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	got, _ := s.Query(ctx, ScopeSession, KindLearning, "failure:t1", 1)
	if len(got) != 1 || got[0].EffectiveState() != StateStale {
		t.Fatalf("条目应置 stale: %+v", got)
	}
	if got[0].Recalled() {
		t.Error("stale 条目不得被召回")
	}
	// 幂等：重复 MarkStale / 不存在 ID 均不报错。
	if err := s.MarkStale(ctx, id); err != nil {
		t.Errorf("重复 MarkStale 应幂等: %v", err)
	}
	if err := s.MarkStale(ctx, "no-such-id"); err != nil {
		t.Errorf("不存在 ID 的 MarkStale 应幂等成功: %v", err)
	}
}

// TestForget_DeletesEntry 验证 forget（Delete）语义。
func TestForget_DeletesEntry(t *testing.T) {
	ctx := context.Background()
	s := newTestSessionStore(t)

	if err := s.Put(ctx, sessionEntry(KindResult, "result:t1", "结果")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	all, _ := s.Query(ctx, ScopeSession, KindResult, "", 1)
	if len(all) != 1 {
		t.Fatalf("条目应存在: %+v", all)
	}
	if err := s.Delete(ctx, all[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, _ := s.Query(ctx, ScopeSession, KindResult, "", 10)
	if len(got) != 0 {
		t.Errorf("forget 后条目应消失: %+v", got)
	}
}

// TestStateMachine_ReplayAcrossReopen 验证状态机经 JSONL 重放跨重启保持：
// 新条目活跃、旧条目 superseded 保留、stale 保留。
func TestStateMachine_ReplayAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.jsonl")

	s1, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	if err := s1.Put(ctx, sessionEntry(KindDecision, "decision:ab12", "旧结论")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	newID, err := func() (string, error) {
		e := sessionEntry(KindDecision, "decision:ab12", "新结论")
		if _, err := s1.Supersede(ctx, e); err != nil {
			return "", err
		}
		got, _ := s1.Query(ctx, ScopeSession, KindDecision, "decision:ab12", 1)
		if len(got) != 1 {
			t.Fatalf("新条目应存在: %+v", got)
		}
		return got[0].ID, nil
	}()
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if err := s1.Put(ctx, Entry{Scope: ScopeSession, Kind: KindLearning, Key: "failure:t1", Content: "将失效"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	staleList, _ := s1.Query(ctx, ScopeSession, KindLearning, "failure:t1", 1)
	if err := s1.MarkStale(ctx, staleList[0].ID); err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 重开（模拟进程重启 resume 路径）。
	s2, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("重开 SessionStore: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, _ := s2.Query(ctx, ScopeSession, KindDecision, "decision:ab12", 1)
	if len(got) != 1 || got[0].Content != "新结论" || got[0].ID != newID {
		t.Errorf("重放后新条目应活跃且 ID 稳定: %+v", got)
	}
	all, _ := s2.Query(ctx, ScopeSession, KindDecision, "", 10)
	if len(all) != 2 {
		t.Fatalf("重放后应保留新旧两条: %+v", all)
	}
	var oldOK bool
	for _, e := range all {
		if e.EffectiveState() == StateSuperseded && e.SupersededBy == newID {
			oldOK = true
		}
	}
	if !oldOK {
		t.Errorf("重放后旧条目应保持 superseded 且 SupersededBy 指向新条目: %+v", all)
	}
	staleGot, _ := s2.Query(ctx, ScopeSession, KindLearning, "failure:t1", 1)
	if len(staleGot) != 1 || staleGot[0].EffectiveState() != StateStale {
		t.Errorf("重放后 stale 状态应保持: %+v", staleGot)
	}
}

// TestPut_MergesLifecycleFields 验证 Put upsert 合并路径携带生命周期字段
// （State/Evidence/SupersededBy 与 Content 同权覆盖）。
func TestPut_MergesLifecycleFields(t *testing.T) {
	ctx := context.Background()
	s := newTestSessionStore(t)

	if err := s.Put(ctx, Entry{
		Scope: ScopeSession, Kind: KindResult, Key: "result:t1",
		Content: "v1", State: StateInferred, Evidence: []string{"tool_result:read_file a"},
	}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := s.Put(ctx, Entry{
		Scope: ScopeSession, Kind: KindResult, Key: "result:t1",
		Content: "v2", State: StateConfirmed, Evidence: []string{"file_effect:b.go@abcd1234"},
	}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	got, _ := s.Query(ctx, ScopeSession, KindResult, "result:t1", 1)
	if len(got) != 1 {
		t.Fatalf("条目应存在: %+v", got)
	}
	if got[0].Content != "v2" || got[0].EffectiveState() != StateConfirmed {
		t.Errorf("Put 合并应覆盖 Content/State: %+v", got[0])
	}
	if len(got[0].Evidence) != 1 || !strings.HasPrefix(got[0].Evidence[0], "file_effect:") {
		t.Errorf("Put 合并应覆盖 Evidence: %+v", got[0].Evidence)
	}
}

// TestEntryStateVocabulary 钉住状态词表与默认归一。
func TestEntryStateVocabulary(t *testing.T) {
	if (Entry{}).EffectiveState() != StateConfirmed {
		t.Error("空 State 应归一为 confirmed（旧 JSONL 行兼容）")
	}
	if (Entry{State: StateInferred}).Recalled() != true {
		t.Error("inferred 条目可召回（注入时须标注未验证）")
	}
	for _, st := range []string{StateStale, StateSuperseded} {
		if (Entry{State: st}).Recalled() {
			t.Errorf("%s 条目不得召回", st)
		}
	}
}
