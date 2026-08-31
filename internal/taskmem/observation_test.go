package taskmem

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordObservationV2ChainsStateAndReplacesProjection(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	evidence := EvidenceRef{Kind: EvidenceToolResult, Ref: "tool-call:call-1", Digest: "abc"}
	firstCandidate := NewObservationCandidate("task-1", "task-1/attempt-1", "编辑 ctx.py 后运行目标测试")
	first := ObservationDelta{
		Schema: ObservationDeltaSchemaV2, TaskID: "task-1", AttemptID: "task-1/attempt-1",
		Phase: ObservationPhaseInvestigate, CreatedAt: time.Now().UTC(),
		Facts:          []ObservationFact{{Text: "失败来自 ctx.session 字段", Evidence: []EvidenceRef{evidence}}},
		NextCandidates: []ObservationCandidate{firstCandidate}, WorkspaceRevisionRef: "workspace:empty",
	}
	firstRef, err := store.RecordObservation(first)
	if err != nil {
		t.Fatal(err)
	}
	resolvedFirst, err := store.ResolveObservation(first.TaskID, firstRef)
	if err != nil || !resolvedFirst.SemanticAdvance {
		t.Fatalf("首个状态必须建立 baseline: %+v err=%v", resolvedFirst, err)
	}

	secondCandidate := NewObservationCandidate(first.TaskID, first.AttemptID, "运行目标测试")
	second := ObservationDelta{
		Schema: ObservationDeltaSchemaV2, TaskID: first.TaskID, AttemptID: first.AttemptID,
		PreviousRef: firstRef, Phase: ObservationPhaseImplement, CreatedAt: first.CreatedAt.Add(time.Minute),
		Facts:              []ObservationFact{{Text: "ctx.py 已修改", Evidence: []EvidenceRef{evidence}}},
		ResolvedCandidates: []ResolvedObservationCandidate{{Ref: firstCandidate.Ref, Evidence: []EvidenceRef{evidence}}},
		NextCandidates:     []ObservationCandidate{secondCandidate}, WorkspaceRevisionRef: "workspace:sha256:changed",
	}
	secondRef, err := store.RecordObservation(second)
	if err != nil || secondRef == firstRef {
		t.Fatalf("状态前进必须形成 chained ref: first=%s second=%s err=%v", firstRef, secondRef, err)
	}
	mem, err := store.Load(first.TaskID)
	if err != nil || mem == nil || mem.LatestObservationDeltaRef != secondRef ||
		len(mem.Facts) != 1 || mem.Facts[0].Text != "ctx.py 已修改" ||
		len(mem.NextCandidates) != 1 || mem.NextCandidates[0] != "运行目标测试" {
		t.Fatalf("TaskMemory 必须只投影最新 Observation 状态: %+v err=%v", mem, err)
	}

	stale := second
	stale.PreviousRef = secondRef
	stale.ResolvedCandidates = nil
	stale.CreatedAt = stale.CreatedAt.Add(time.Minute)
	staleRef, err := store.RecordObservation(stale)
	if err != nil {
		t.Fatal(err)
	}
	staleStored, err := store.ResolveObservation(first.TaskID, staleRef)
	if err != nil || staleStored.SemanticAdvance {
		t.Fatalf("同 phase/revision/check 且未关闭候选不得算语义进展: %+v err=%v", staleStored, err)
	}
	if matches, _ := filepath.Glob(filepath.Join(store.dir, "observations", "task-1", "*.json")); len(matches) != 3 {
		t.Fatalf("Observation append-only object 数量=%d", len(matches))
	}
}

func TestObservationV2RejectsMissingEvidenceAndBrokenPredecessor(t *testing.T) {
	delta := ObservationDelta{Schema: ObservationDeltaSchemaV2, TaskID: "t", AttemptID: "a",
		Phase: ObservationPhaseInvestigate, WorkspaceRevisionRef: "workspace:empty",
		CreatedAt: time.Now().UTC(), Facts: []ObservationFact{{Text: "无证据声称"}}}
	if err := delta.Validate(); err == nil {
		t.Fatal("无 evidence 的事实必须拒绝")
	}
	store := NewStore(t.TempDir())
	delta.Facts = nil
	delta.PreviousRef = "observation:sha256:missing"
	if _, err := store.RecordObservation(delta); err == nil {
		t.Fatal("previous_ref 不是当前 TaskMemory 状态时必须拒绝")
	}
}

func TestObservationV3ProjectsModelClaimsAsInferred(t *testing.T) {
	store := NewStore(t.TempDir())
	evidence := EvidenceRef{Kind: EvidenceToolResult, Ref: "tool-call:edit-1", Digest: "abc"}
	delta := ObservationDelta{
		Schema: ObservationDeltaSchemaV3, TaskID: "task-v3", AttemptID: "task-v3/attempt-1",
		Phase: ObservationPhaseImplement, WorkspaceRevisionRef: "workspace:sha256:changed",
		CreatedAt: time.Now().UTC(), Facts: []ObservationFact{{
			Text: "AppContext 首参重构已经完成", Evidence: []EvidenceRef{evidence},
			Authority: ObservationFactAuthorityInferred,
		}},
	}
	if _, err := store.RecordObservation(delta); err != nil {
		t.Fatalf("记录 Observation v3: %v", err)
	}
	mem, err := store.Load(delta.TaskID)
	if err != nil || mem == nil || len(mem.Facts) != 1 {
		t.Fatalf("读取 Observation v3 投影: mem=%+v err=%v", mem, err)
	}
	if mem.Facts[0].Confirmed {
		t.Fatalf("模型 claim 只有 edit receipt 时不得成为 confirmed: %+v", mem.Facts[0])
	}
	rendered := Render(mem, 1500)
	if !strings.Contains(rendered, "待验证观察:") || !strings.Contains(rendered, delta.Facts[0].Text) ||
		strings.Contains(rendered, "已确认事实:\n- "+delta.Facts[0].Text) {
		t.Fatalf("inferred claim 必须带低权威标签渲染: %q", rendered)
	}
}

func TestObservationV3RejectsModelClaimWithoutInferredAuthority(t *testing.T) {
	delta := ObservationDelta{
		Schema: ObservationDeltaSchemaV3, TaskID: "task-v3", AttemptID: "attempt-1",
		Phase: ObservationPhaseInvestigate, WorkspaceRevisionRef: "workspace:empty",
		CreatedAt: time.Now().UTC(), Facts: []ObservationFact{{
			Text: "未经标记的 claim", Evidence: []EvidenceRef{{Kind: EvidenceToolResult, Ref: "tool-call:read-1"}},
		}},
	}
	if err := delta.Validate(); err == nil || !strings.Contains(err.Error(), "authority") {
		t.Fatalf("Observation v3 必须由 framework 标记 inferred: %v", err)
	}
}
