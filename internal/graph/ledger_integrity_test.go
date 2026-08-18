package graph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJournalEntryDigestRejectsActivationResultPayloadTamper(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	record := ActivationResult{
		NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`{"ok":true}`),
	}
	if err := s.RecordActivationResult("g1", record); err != nil {
		t.Fatalf("RecordActivationResult: %v", err)
	}
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	entries := readJournalEntries(t, gdir)
	if len(entries) != 2 || entries[1].Kind != journalKindActivationResult || entries[1].EntryDigest == "" {
		t.Fatalf("新写 activation_result 必须携带 entry digest: %+v", entries)
	}
	var payload activationResultPayload
	if err := json.Unmarshal(entries[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Result = json.RawMessage(`{"ok":false}`) // 保持合法 object，只改账本事实。
	entries[1].Payload, _ = json.Marshal(payload)    // 故意不更新 EntryDigest。
	writeJournalEntries(t, gdir, entries)

	ns, recoverErr := recoverExpectWarn(t, dir)
	if !strings.Contains(recoverErr.Error(), "entry_digest") {
		t.Fatalf("恢复必须报告 payload 摘要不一致: %v", recoverErr)
	}
	if _, ok := ns.ResolveActivationResult("g1", activationResultRef("g1", "a@1")); ok {
		t.Fatal("被篡改的 activation_result 及其后缀必须截断，不得进入 ledger")
	}
	if got := readJournalEntries(t, gdir); len(got) != 1 || got[0].Kind != journalKindSubmit {
		t.Fatalf("坏记录起的物理后缀应截断，实际 %+v", got)
	}
}

func TestJournalEntryDigestRejectsTransitionPayloadTamper(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	result := ActivationResult{
		NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`{"ok":true}`),
	}
	if err := s.RecordActivationResult("g1", result); err != nil {
		t.Fatal(err)
	}
	rec := TransitionRecord{
		SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0,
		TargetNodeID: "b", TargetActivationID: "b@1",
		Input: &EdgeInput{
			ResultRef: activationResultRef("g1", "a@1"), Result: json.RawMessage(`{"ok":true}`),
		},
	}
	if err := s.RecordTransition("g1", rec, 0); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	entries := readJournalEntries(t, gdir)
	if len(entries) != 3 || entries[2].Kind != journalKindTransition {
		t.Fatalf("缺少 transition journal: %+v", entries)
	}
	var payload transitionPayload
	if err := json.Unmarshal(entries[2].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.TargetActivationID = "b@2"
	entries[2].Payload, _ = json.Marshal(payload) // 合法 owner，只有链式摘要能发现。
	writeJournalEntries(t, gdir, entries)

	ns, recoverErr := recoverExpectWarn(t, dir)
	if !strings.Contains(recoverErr.Error(), "entry_digest") {
		t.Fatalf("恢复必须拒绝 transition payload 篡改: %v", recoverErr)
	}
	if got := ns.Transitions("g1"); len(got) != 0 {
		t.Fatalf("被篡改 transition 不得进入 ledger: %+v", got)
	}
	if _, ok := ns.ResolveActivationResult("g1", activationResultRef("g1", "a@1")); !ok {
		t.Fatal("坏 transition 前已通过摘要的 activation_result 应保留")
	}
}

func TestSnapshotV2IntegrityCoversActivationLedger(t *testing.T) {
	setCompactThresholds(t, 1, 1<<20)
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	if err := s.RecordActivationResult("g1", ActivationResult{
		NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	snap := readSnapshotForTest(t, gdir)
	if snap.Version != snapshotVersion || snap.IntegrityDigest == "" || snap.ChainDigest == "" || len(snap.ActivationResults) != 1 {
		t.Fatalf("压缩必须写完整 v2 ledger snapshot: %+v", snap)
	}
	snap.ActivationResults[0].Result = json.RawMessage(`{"ok":false}`)
	writeSnapshotForTest(t, gdir, snap) // 故意保留旧 IntegrityDigest。

	ns, recoverErr := recoverExpectWarn(t, dir)
	if !strings.Contains(recoverErr.Error(), "integrity_digest") {
		t.Fatalf("snapshot ledger 篡改必须被完整性摘要发现: %v", recoverErr)
	}
	if _, ok := ns.Get("g1"); ok {
		t.Fatal("journal 已压缩时，损坏 snapshot 不得产生半恢复 Graph")
	}
}

func TestSnapshotV2RestoresStableResultRefAndEvidence(t *testing.T) {
	setCompactThresholds(t, 1, 1<<20)
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	success := true
	record := ActivationResult{
		Ref: activationResultRef("g1", "a@1"), NodeID: "a", ActivationID: "a@1",
		Result: json.RawMessage(`{"ok":true,"value":17}`),
		Evidence: []EvidenceEntry{{
			Ref: "ev:stable:snapshot", Kind: "shell", ToolName: "run_shell",
			Success: &success, Command: "go test ./internal/graph", Summary: "exit 0",
		}},
	}
	if err := s.RecordActivationResult("g1", record); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	closeStore(t, s)

	ns, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if err := ns.Recover(); err != nil {
		t.Fatalf("Recover snapshot v2: %v", err)
	}
	got, ok := ns.ResolveActivationResult("g1", activationResultRef("g1", "a@1"))
	if !ok || !sameActivationResult(got, record) {
		t.Fatalf("snapshot 压缩后稳定 Ref 必须精确恢复 Result+Evidence: ok=%v got=%+v", ok, got)
	}
}

func TestTransitionLedgerStrictForNewWritesAndCompatibleForLegacy(t *testing.T) {
	t.Run("live-v2-rejects-missing-ref", func(t *testing.T) {
		s := newTestStore(t)
		mustSubmit(t, s, tinyDocJSON)
		err := s.RecordTransition("g1", TransitionRecord{
			SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0,
			TargetNodeID: "b", TargetActivationID: "b@1",
			Input: newEdgeInput(map[string]any{"ok": true}, nil),
		}, 0)
		if err == nil || !strings.Contains(err.Error(), "result_ref") {
			t.Fatalf("live 新写不得借 legacy 口径省略 ResultRef: %v", err)
		}
	})

	t.Run("v2-journal-rejects-validly-digested-legacy-shape", func(t *testing.T) {
		dir := t.TempDir()
		gdir := filepath.Join(dir, "g1")
		if err := os.MkdirAll(gdir, 0o755); err != nil {
			t.Fatal(err)
		}
		doc := mustParse(t, tinyDocJSON)
		line1, _, chain1, err := buildJournalLine(1, journalKindSubmit, doc, submitPayload{Doc: doc}, "")
		if err != nil {
			t.Fatal(err)
		}
		next, _ := cloneDoc(doc)
		next.StateVersion++
		legacyShape := TransitionRecord{
			SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0,
			TargetNodeID: "b", TargetActivationID: "b@1",
			Input: newEdgeInput(map[string]any{"ok": true}, nil),
		}
		line2, _, _, err := buildJournalLine(2, journalKindTransition, next, transitionPayload(legacyShape), chain1)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gdir, journalFileName), append(append(line1, '\n'), append(line2, '\n')...), 0o600); err != nil {
			t.Fatal(err)
		}

		ns, recoverErr := recoverExpectWarn(t, dir)
		if !strings.Contains(recoverErr.Error(), "result_ref") {
			t.Fatalf("摘要合法的新格式记录仍须通过 ledger 语义校验: %v", recoverErr)
		}
		if got := ns.Transitions("g1"); len(got) != 0 {
			t.Fatalf("legacy 形状 v2 transition 不得生效: %+v", got)
		}
	})

	t.Run("legacy-journal-allows-historical-shape", func(t *testing.T) {
		dir := t.TempDir()
		gdir := filepath.Join(dir, "g1")
		if err := os.MkdirAll(gdir, 0o755); err != nil {
			t.Fatal(err)
		}
		doc := mustParse(t, tinyDocJSON)
		next, _ := cloneDoc(doc)
		next.StateVersion++
		rec := TransitionRecord{
			SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0, TargetNodeID: "b",
		}
		writeJournalEntries(t, gdir, []journalEntry{
			legacyJournalEntry(1, journalKindSubmit, doc, submitPayload{Doc: doc}),
			legacyJournalEntry(2, journalKindTransition, next, transitionPayload(rec)),
		})

		ns, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ns.Close() })
		if err := ns.Recover(); err != nil {
			t.Fatalf("历史无 ResultRef transition 应保持只读兼容: %v", err)
		}
		if got := ns.Transitions("g1"); len(got) != 1 || got[0].SourceActivationID != "a@1" {
			t.Fatalf("legacy transition 恢复错误: %+v", got)
		}
	})

	t.Run("legacy-transition-survives-v2-compaction", func(t *testing.T) {
		setCompactThresholds(t, 0, 1<<20)
		dir := t.TempDir()
		gdir := filepath.Join(dir, "g1")
		if err := os.MkdirAll(gdir, 0o755); err != nil {
			t.Fatal(err)
		}
		doc := mustParse(t, tinyDocJSON)
		next, _ := cloneDoc(doc)
		next.StateVersion++
		rec := TransitionRecord{
			SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0, TargetNodeID: "b",
		}
		writeJournalEntries(t, gdir, []journalEntry{
			legacyJournalEntry(1, journalKindSubmit, doc, submitPayload{Doc: doc}),
			legacyJournalEntry(2, journalKindTransition, next, transitionPayload(rec)),
		})
		s, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Recover(); err != nil {
			t.Fatalf("Recover legacy: %v", err)
		}
		current := mustGet(t, s, "g1")
		mustMutate(t, s.SetGraphStatus("g1", GraphRunning, current.StateVersion))
		closeStore(t, s)
		if snap := readSnapshotForTest(t, gdir); snap.Version != snapshotVersion || len(snap.Transitions) != 1 {
			t.Fatalf("legacy ledger 应被纳入 v2 snapshot: %+v", snap)
		}

		ns, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ns.Close() })
		if err := ns.Recover(); err != nil {
			t.Fatalf("v2 snapshot 必须保留迁移前 legacy transition: %v", err)
		}
		if got := ns.Transitions("g1"); len(got) != 1 || got[0].TargetNodeID != "b" {
			t.Fatalf("压缩恢复后 legacy transition 丢失: %+v", got)
		}
	})
}

func TestRecoverLegacyV1SnapshotAndSealNextWrite(t *testing.T) {
	setCompactThresholds(t, 1, 1<<20)
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	mustMutate(t, s.SetGraphStatus("g1", GraphRunning, 0)) // seq2 触发 snapshot。
	dir := s.dir
	gdir := filepath.Join(dir, "g1")
	closeStore(t, s)

	snap := readSnapshotForTest(t, gdir)
	snap.Version = snapshotVersionLegacy
	snap.ChainDigest = ""
	snap.IntegrityDigest = ""
	writeSnapshotForTest(t, gdir, snap)

	ns, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if err := ns.Recover(); err != nil {
		t.Fatalf("合法 v1 snapshot 必须保持可读迁移: %v", err)
	}
	doc := mustGet(t, ns, "g1")
	mustMutate(t, ns.SetGraphStatus("g1", GraphPaused, doc.StateVersion))
	closeStore(t, ns)

	entries := readJournalEntries(t, gdir)
	if len(entries) != 1 || entries[0].IntegrityVersion != journalIntegrityVersion ||
		entries[0].PreviousDigest == "" || entries[0].EntryDigest == "" {
		t.Fatalf("v1 恢复后的首个新写必须以 legacy anchor 接入 v2 链: %+v", entries)
	}
	check, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = check.Close() })
	if err := check.Recover(); err != nil {
		t.Fatalf("v1→v2 混合恢复必须成功: %v", err)
	}
	if got := mustGet(t, check, "g1"); got.Status != GraphPaused {
		t.Fatalf("混合恢复状态错误: %s", got.Status)
	}
}

func TestLegacyLedgerRejectsConflictingResultAndEvidenceIdentities(t *testing.T) {
	cases := []struct {
		name    string
		records []ActivationResult
		want    string
	}{
		{
			name: "same-result-ref-different-content",
			records: []ActivationResult{
				{Ref: activationResultRef("g1", "a@1"), NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`{"v":1}`)},
				{Ref: activationResultRef("g1", "a@1"), NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`{"v":2}`)},
			},
			want: "内容不一致",
		},
		{
			name: "same-evidence-ref-different-fact",
			records: []ActivationResult{
				{Ref: activationResultRef("g1", "a@1"), NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`{"v":1}`), Evidence: []EvidenceEntry{{Ref: "ev:stable", Kind: "shell", Summary: "exit 0"}}},
				{Ref: activationResultRef("g1", "a@2"), NodeID: "a", ActivationID: "a@2", Result: json.RawMessage(`{"v":2}`), Evidence: []EvidenceEntry{{Ref: "ev:stable", Kind: "shell", Summary: "exit 1"}}},
			},
			want: "EvidenceRef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			doc := mustParse(t, tinyDocJSON)
			entries := []journalEntry{legacyJournalEntry(1, journalKindSubmit, doc, submitPayload{Doc: doc})}
			for i, rec := range tc.records {
				entries = append(entries, legacyJournalEntry(int64(i+2), journalKindActivationResult, doc, activationResultPayload(rec)))
			}
			gdir := filepath.Join(dir, "g1")
			if err := os.MkdirAll(gdir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeJournalEntries(t, gdir, entries)

			ns, recoverErr := recoverExpectWarn(t, dir)
			if !strings.Contains(recoverErr.Error(), tc.want) {
				t.Fatalf("冲突 identity 应 fail-closed，want %q: %v", tc.want, recoverErr)
			}
			got, ok := ns.ResolveActivationResult("g1", tc.records[0].Ref)
			if !ok || !sameActivationResult(got, tc.records[0]) {
				t.Fatalf("应保留冲突前第一份不可变记录: ok=%v got=%+v", ok, got)
			}
		})
	}
}

func TestActivationResultRequiresJSONObjectAndUnambiguousEvidence(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, tinyDocJSON)
	if err := s.RecordActivationResult("g1", ActivationResult{
		NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`[1,2]`),
	}); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("Result 数组必须拒绝: %v", err)
	}
	if err := s.RecordActivationResult("g1", ActivationResult{
		NodeID: "a", ActivationID: "a@1", Result: json.RawMessage(`{"ok":true}`),
		Evidence: []EvidenceEntry{
			{Ref: "ev:dup", Kind: "shell", Summary: "exit 0"},
			{Ref: "ev:dup", Kind: "shell", Summary: "exit 1"},
		},
	}); err == nil || !strings.Contains(err.Error(), "EvidenceRef") {
		t.Fatalf("同一 Result 内 EvidenceRef 歧义必须拒绝: %v", err)
	}
}

func TestRecoverRejectsDanglingCurrentExecutionLedgerBindings(t *testing.T) {
	cases := []struct {
		name string
		edit func(*GraphDocument)
		want string
	}{
		{
			name: "settlement-result-ref",
			edit: func(doc *GraphDocument) {
				node := doc.Nodes["a"]
				node.Status = NodeCompleted
				node.Execution = &Execution{
					Phase: "done", ActivationID: "a@1",
					Settlement: &TerminalSettlement{
						Status: NodeCompleted, ResultRef: activationResultRef("g1", "a@1"),
						Continuation: SettlementContinueTransitions,
					},
				}
				doc.Nodes["a"] = node
			},
			want: "settlement.result_ref",
		},
		{
			name: "execution-input",
			edit: func(doc *GraphDocument) {
				node := doc.Nodes["b"]
				node.Status = NodeRunning
				node.Execution = &Execution{
					Phase: "executing", ActivationID: "b@1",
					Input: []InputBinding{{
						SourceNodeID: "a", SourceActivationID: "a@1",
						ResultRef: activationResultRef("g1", "a@1"),
					}},
				}
				doc.Nodes["b"] = node
			},
			want: "durable transition",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gdir := filepath.Join(dir, "g1")
			if err := os.MkdirAll(gdir, 0o755); err != nil {
				t.Fatal(err)
			}
			doc := mustParse(t, tinyDocJSON)
			tc.edit(doc)
			snap := &snapshotFile{
				Version: snapshotVersionLegacy, GraphID: "g1", Seq: 1,
				Revision: doc.Revision, StateVersion: doc.StateVersion,
				Digest: ComputeDigest(doc), Doc: doc,
				ActivationSeq: map[string]int{"a": 1, "b": 1},
			}
			writeSnapshotForTest(t, gdir, snap)

			ns, recoverErr := recoverExpectWarn(t, dir)
			if !strings.Contains(recoverErr.Error(), tc.want) {
				t.Fatalf("dangling 当前 execution 必须拒绝，want %q: %v", tc.want, recoverErr)
			}
			if _, ok := ns.Get("g1"); ok {
				t.Fatal("仅有损坏 snapshot 时不得恢复 Graph")
			}
		})
	}
}

func TestLiveExecutionWriteRejectsDanglingLedgerBindings(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		s := newTestStore(t)
		mustSubmit(t, s, tinyDocJSON)
		err := s.SetExecutionAndStatus("g1", "b", Execution{
			Phase: "executing", ActivationID: "b@1",
			Input: []InputBinding{{
				SourceNodeID: "a", SourceActivationID: "a@1",
				ResultRef: activationResultRef("g1", "a@1"),
			}},
		}, NodeReady, 0)
		if err == nil || !strings.Contains(err.Error(), "durable transition") {
			t.Fatalf("live dangling Input 必须在 append 前拒绝: %v", err)
		}
		if got := mustGet(t, s, "g1"); got.StateVersion != 0 || got.Nodes["b"].Execution != nil {
			t.Fatalf("拒绝后内存不得前进: %+v", got.Nodes["b"])
		}
	})

	t.Run("settlement", func(t *testing.T) {
		s := newTestStore(t)
		mustSubmit(t, s, tinyDocJSON)
		exec := Execution{Phase: "executing", ActivationID: "a@1"}
		mustMutate(t, s.SetExecutionAndStatus("g1", "a", exec, NodeReady, 0))
		mustMutate(t, s.SetExecutionAndStatus("g1", "a", exec, NodeRunning, 1))
		exec.Settlement = &TerminalSettlement{
			Status: NodeCompleted, ResultRef: activationResultRef("g1", "a@1"),
			Continuation: SettlementContinueTransitions,
		}
		err := s.SetExecutionAndStatus("g1", "a", exec, NodeCompleted, 2)
		if err == nil || !strings.Contains(err.Error(), "settlement.result_ref") {
			t.Fatalf("live dangling Settlement.ResultRef 必须拒绝: %v", err)
		}
		if got := mustGet(t, s, "g1"); got.StateVersion != 2 || got.Nodes["a"].Status != NodeRunning {
			t.Fatalf("拒绝后应保持 running@sv2: %+v", got.Nodes["a"])
		}
	})
}

func legacyJournalEntry(seq int64, kind string, doc *GraphDocument, payload any) journalEntry {
	raw, _ := json.Marshal(payload)
	return journalEntry{
		Seq: seq, Kind: kind, Revision: doc.Revision, StateVersion: doc.StateVersion,
		Digest: ComputeDigest(doc), At: time.Unix(seq, 0).UTC(), Payload: raw,
	}
}

func writeJournalEntries(t *testing.T, graphDir string, entries []journalEntry) {
	t.Helper()
	var lines []string
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	if err := os.WriteFile(filepath.Join(graphDir, journalFileName), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSnapshotForTest(t *testing.T, graphDir string) *snapshotFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(graphDir, snapshotFileName))
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshotFile
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	return &snap
}

func writeSnapshotForTest(t *testing.T, graphDir string, snap *snapshotFile) {
	t.Helper()
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(graphDir, snapshotFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
