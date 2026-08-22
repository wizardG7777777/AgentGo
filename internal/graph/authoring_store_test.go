package graph

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func authoringTestBody(title string) GraphDefinitionBody {
	return GraphDefinitionBody{
		Schema: SchemaV2,
		Root:   "work",
		Nodes: map[string]GraphDefinitionNode{
			"work": {
				Kind:                KindAgent,
				Task:                &NodeTask{Title: title, Description: "执行并提交 result"},
				Next:                []Transition{{To: "done", When: &Condition{Event: EventCompleted}}},
				Metadata:            map[string]string{"route": "team:impl", "label": "展示标签"},
				OutputContract:      &NodeOutputContract{SummaryRequired: true},
				ProgressContractRef: "progress:code-change/v1", ContextPolicyRef: "context:default/v1",
			},
			"done": {Kind: KindEnd, Task: &NodeTask{Title: "收官"}, Next: []Transition{}, EndOutcome: DefinitionEndSuccess},
		},
	}
}

func authoringTestContract() GraphContract {
	return GraphContract{
		RequestRef: "request-1", RequestDigest: "request-digest-1",
		ExecutionClass:     ExecutionMutating,
		Deliverables:       []ContractRequirement{{ID: "source", Kind: "artifact", Description: "源码修改"}},
		RequiredEffects:    []string{"file_write"},
		RequiredChecks:     []ContractRequirement{{ID: "tests", Kind: "test", Description: "运行测试"}},
		RequiresAcceptance: true,
	}
}

func createValidatedDefinition(t *testing.T, store *AuthoringStore, graphID, proposalID string) (*GraphDraft, *GraphDefinition) {
	t.Helper()
	body := authoringTestBody("实施修改")
	contract := authoringTestContract()
	draft, err := store.CreateDraft(GraphDraft{
		ProposalID: proposalID, GraphID: graphID, SessionID: "session-1", OwnerTaskID: "task-owner",
		RequestRef: contract.RequestRef, RequestDigest: contract.RequestDigest,
		Contract: contract, Candidate: body,
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	digest := ComputeGraphDefinitionDigest(graphID, 1, body)
	report, err := store.RecordValidation(ValidationReport{
		ReportID: "report-" + proposalID, SubjectKind: "draft", SubjectID: proposalID,
		SubjectRevision: draft.DraftRevision, DefinitionRevision: 1,
		Accepted: true, NormalizedDigest: digest, NormalizedDefinition: &body, ContractDigest: ComputeGraphContractDigest(contract),
		ProposalAcceptance: ProposalAcceptancePass, ProposalAcceptanceRef: "acceptance-1",
	})
	if err != nil {
		t.Fatalf("RecordValidation: %v", err)
	}
	definition, err := store.CommitDraft(proposalID, draft.DraftRevision, report.ReportID, body)
	if err != nil {
		t.Fatalf("CommitDraft: %v", err)
	}
	return draft, definition
}

// TestAuthoringStoreLifecycleAndRecovery 覆盖 Draft→Validation→Definition、
// StartIntent 和 ChangeProposal 同一 journal 的 durable 重放。
func TestAuthoringStoreLifecycleAndRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := NewAuthoringStore(dir)
	if err != nil {
		t.Fatalf("NewAuthoringStore: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = store.Close()
		}
	})

	body := authoringTestBody("初始标题")
	contract := authoringTestContract()
	draft, err := store.CreateDraft(GraphDraft{
		ProposalID: "proposal-1", GraphID: "g-authoring", SessionID: "session-1", OwnerTaskID: "task-owner",
		RequestRef: contract.RequestRef, RequestDigest: contract.RequestDigest,
		Contract: contract, Candidate: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	body = authoringTestBody("规范化标题")
	draft, err = store.PatchDraft(draft.ProposalID, draft.DraftRevision, GraphDraftPatch{Candidate: &body})
	if err != nil || draft.DraftRevision != 2 {
		t.Fatalf("PatchDraft: draft=%+v err=%v", draft, err)
	}
	digest := ComputeGraphDefinitionDigest(draft.GraphID, 1, body)
	report, err := store.RecordValidation(ValidationReport{
		ReportID: "report-1", SubjectKind: "draft", SubjectID: draft.ProposalID,
		SubjectRevision: 2, DefinitionRevision: 1, Accepted: true,
		NormalizedDigest: digest, NormalizedDefinition: &body, ContractDigest: ComputeGraphContractDigest(contract),
		ProposalAcceptance: ProposalAcceptancePass, ProposalAcceptanceRef: "acceptance-1",
	})
	if err != nil {
		t.Fatalf("RecordValidation: %v", err)
	}
	definition, err := store.CommitDraft(draft.ProposalID, 2, report.ReportID, body)
	if err != nil {
		t.Fatalf("CommitDraft: %v", err)
	}
	if definition.Revision != 1 || definition.DefinitionDigest != digest || definition.Status != DefinitionPending {
		t.Fatalf("Definition 非预期: %+v", definition)
	}
	committedDraft, ok := store.GetDraft(draft.ProposalID)
	if !ok || committedDraft.Status != DraftCommitted || committedDraft.CommittedDefinitionRevision != 1 {
		t.Fatalf("Draft 未与 Definition 原子 commit: %+v ok=%v", committedDraft, ok)
	}

	intent, err := store.BeginStart(StartIntent{
		StartID: "start-1", GraphID: definition.GraphID,
		DefinitionRevision: definition.Revision, DefinitionDigest: definition.DefinitionDigest,
		ContractDigest: definition.ContractDigest, SessionID: definition.SessionID, OwnerTaskID: definition.OwnerTaskID,
	})
	if err != nil || intent.Status != StartRequested || intent.RootActivationID != "work@1" {
		t.Fatalf("BeginStart: intent=%+v err=%v", intent, err)
	}
	intent, err = store.CompleteStart(intent.StartID, intent.IntentRevision, "execution:g-authoring")
	if err != nil || intent.Status != StartStarted || intent.ExecutionRef == "" {
		t.Fatalf("CompleteStart: intent=%+v err=%v", intent, err)
	}

	change, err := store.CreateGraphChangeProposal(GraphChangeProposal{
		ChangeID: "change-1", GraphID: definition.GraphID,
		BaseDefinitionRevision: definition.Revision, BaseDefinitionDigest: definition.DefinitionDigest,
		SessionID: definition.SessionID, OwnerTaskID: definition.OwnerTaskID, Reason: "增加验证节点",
		Patch: GraphDefinitionPatch{UpsertNodes: []GraphDefinitionNodeUpsert{{
			ID: "verify", GraphDefinitionNode: GraphDefinitionNode{Kind: KindAgent, Task: &NodeTask{Title: "验证"}, Next: []Transition{{To: "done"}}},
		}}},
	})
	if err != nil || change.ProposalRevision != 1 || change.Status != GraphChangeProposed {
		t.Fatalf("CreateGraphChangeProposal: change=%+v err=%v", change, err)
	}
	if _, err := store.RecordValidation(ValidationReport{
		ReportID: "change-report-1", SubjectKind: "change", SubjectID: change.ChangeID,
		SubjectRevision: change.ProposalRevision, DefinitionRevision: 2,
		Accepted: false, ProposalAcceptance: ProposalAcceptanceFixable,
		Errors: []ValidationIssue{{Code: "MISSING_OUTLET", Path: "nodes.verify.next", Retryable: true, Message: "缺少失败出口"}},
	}); err != nil {
		t.Fatalf("Record change validation: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	recovered, err := NewAuthoringStore(dir)
	if err != nil {
		t.Fatalf("recover AuthoringStore: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })

	if got, ok := recovered.GetDraft("proposal-1"); !ok || got.Status != DraftCommitted {
		t.Fatalf("恢复 Draft 失败: %+v ok=%v", got, ok)
	}
	if got, ok := recovered.GetDefinition("g-authoring", 1); !ok || got.DefinitionDigest != digest {
		t.Fatalf("恢复 Definition 失败: %+v ok=%v", got, ok)
	}
	if got, ok := recovered.GetStartIntent("start-1"); !ok || got.Status != StartStarted {
		t.Fatalf("恢复 StartIntent 失败: %+v ok=%v", got, ok)
	}
	if got, ok := recovered.GetGraphChangeProposal("change-1"); !ok || got.Status != GraphChangeRejected || got.LastValidationReportRef != "change-report-1" {
		t.Fatalf("恢复 Change 失败: %+v ok=%v", got, ok)
	}
}

// TestAuthoringStoreCASAndCommitGuards 覆盖迟到 patch/report、未通过报告与
// normalized digest 错绑均 fail-closed。
func TestAuthoringStoreCASAndCommitGuards(t *testing.T) {
	store, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	body := authoringTestBody("实施修改")
	contract := authoringTestContract()
	draft, err := store.CreateDraft(GraphDraft{
		ProposalID: "proposal-cas", GraphID: "g-cas", OwnerTaskID: "task-owner",
		Contract: contract, Candidate: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PatchDraft(draft.ProposalID, 99, GraphDraftPatch{Candidate: &body}); !errors.Is(err, ErrAuthoringRevisionConflict) {
		t.Fatalf("stale Draft patch 应冲突: %v", err)
	}
	if _, err := store.RecordValidation(ValidationReport{
		ReportID: "stale-report", SubjectKind: "draft", SubjectID: draft.ProposalID,
		SubjectRevision: 99, ProposalAcceptance: ProposalAcceptancePass,
	}); !errors.Is(err, ErrAuthoringRevisionConflict) {
		t.Fatalf("迟到 ValidationReport 应冲突: %v", err)
	}

	rejected, err := store.RecordValidation(ValidationReport{
		ReportID: "rejected-report", SubjectKind: "draft", SubjectID: draft.ProposalID,
		SubjectRevision: 1, DefinitionRevision: 1, Accepted: false,
		ProposalAcceptance: ProposalAcceptanceFixable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitDraft(draft.ProposalID, 1, rejected.ReportID, body); err == nil {
		t.Fatal("未通过报告不得 commit")
	}

	good, err := store.RecordValidation(ValidationReport{
		ReportID: "good-report", SubjectKind: "draft", SubjectID: draft.ProposalID,
		SubjectRevision: 1, DefinitionRevision: 1, Accepted: true,
		NormalizedDigest:     ComputeGraphDefinitionDigest(draft.GraphID, 1, body),
		NormalizedDefinition: &body, ContractDigest: ComputeGraphContractDigest(contract), ProposalAcceptance: ProposalAcceptancePass,
	})
	if err != nil {
		t.Fatal(err)
	}
	badDigest, err := store.RecordValidation(ValidationReport{
		ReportID: "bad-digest-report", SubjectKind: "draft", SubjectID: draft.ProposalID,
		SubjectRevision: 1, DefinitionRevision: 1, Accepted: true,
		NormalizedDigest: "wrong", NormalizedDefinition: &body, ContractDigest: ComputeGraphContractDigest(contract),
		ProposalAcceptance: ProposalAcceptancePass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitDraft(draft.ProposalID, 1, good.ReportID, body); err == nil {
		t.Fatal("非最新 ValidationReport 不得 commit")
	}
	if _, err := store.CommitDraft(draft.ProposalID, 1, badDigest.ReportID, body); err == nil {
		t.Fatal("normalized digest 错绑不得 commit")
	}
}

// TestAuthoringStoreStartIdentityAndChangeCAS 验证 start 全身份核对、幂等返回和
// ChangeProposal 自身 revision 与 Definition revision 不混用。
func TestAuthoringStoreStartIdentityAndChangeCAS(t *testing.T) {
	store, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, definition := createValidatedDefinition(t, store, "g-start", "proposal-start")

	wrong := StartIntent{
		StartID: "start-wrong", GraphID: definition.GraphID,
		DefinitionRevision: definition.Revision, DefinitionDigest: "wrong",
		ContractDigest: definition.ContractDigest, SessionID: definition.SessionID, OwnerTaskID: definition.OwnerTaskID,
	}
	if _, err := store.BeginStart(wrong); err == nil {
		t.Fatal("Definition digest 不一致的 start 必须拒绝")
	}
	wrong.DefinitionDigest = definition.DefinitionDigest
	wrong.OwnerTaskID = "other-task"
	if _, err := store.BeginStart(wrong); err == nil {
		t.Fatal("owner 不一致的 start 必须拒绝")
	}

	valid := wrong
	valid.StartID = "start-valid"
	valid.OwnerTaskID = definition.OwnerTaskID
	first, err := store.BeginStart(valid)
	if err != nil {
		t.Fatal(err)
	}
	retry := valid
	retry.StartID = "start-retry-new-id"
	second, err := store.BeginStart(retry)
	if err != nil || second.StartID != first.StartID {
		t.Fatalf("相同 Definition 重复 start 应返回同一 Intent: first=%+v second=%+v err=%v", first, second, err)
	}
	failed, err := store.FailStart(first.StartID, first.IntentRevision, "route_unavailable", "route 暂不可用")
	if err != nil || failed.Status != StartFailed {
		t.Fatalf("FailStart: %+v err=%v", failed, err)
	}
	retried, err := store.BeginStart(retry)
	if err != nil || retried.StartID != retry.StartID || retried.Status != StartRequested {
		t.Fatalf("failed start 后应允许新 start_id 重试: %+v err=%v", retried, err)
	}

	change, err := store.CreateGraphChangeProposal(GraphChangeProposal{
		ChangeID: "change-cas", GraphID: definition.GraphID,
		BaseDefinitionRevision: definition.Revision, BaseDefinitionDigest: definition.DefinitionDigest,
		SessionID: definition.SessionID, OwnerTaskID: "graph-controller-task", Reason: "修订",
		Patch: GraphDefinitionPatch{Root: stringPointer("work")},
	})
	if err != nil {
		t.Fatal(err)
	}
	reason := "修订二版"
	changed, err := store.PatchGraphChangeProposal(change.ChangeID, 1, GraphChangeProposalPatch{Reason: &reason})
	if err != nil || changed.ProposalRevision != 2 {
		t.Fatalf("PatchGraphChangeProposal: %+v err=%v", changed, err)
	}
	if _, err := store.PatchGraphChangeProposal(change.ChangeID, 1, GraphChangeProposalPatch{Reason: &reason}); !errors.Is(err, ErrAuthoringRevisionConflict) {
		t.Fatalf("stale Change patch 应冲突: %v", err)
	}
}

// TestAuthoringStoreReadCopiesAndContractNormalization 防止读取返回值污染权威，
// 并锁定 nil/empty Contract slice 的同语义摘要。
func TestAuthoringStoreReadCopiesAndContractNormalization(t *testing.T) {
	store, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	draft, err := store.CreateDraft(GraphDraft{
		ProposalID: "proposal-copy", GraphID: "g-copy", OwnerTaskID: "task-owner",
		Contract:  GraphContract{RequestDigest: "r", ExecutionClass: ExecutionAnswer},
		Candidate: authoringTestBody("原始"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.GetDraft(draft.ProposalID)
	if !ok {
		t.Fatal("GetDraft miss")
	}
	n := got.Candidate.Nodes["work"]
	n.Task.Title = "污染"
	got.Candidate.Nodes["work"] = n
	fresh, _ := store.GetDraft(draft.ProposalID)
	if fresh.Candidate.Nodes["work"].Task.Title != "原始" {
		t.Fatal("读取副本污染了 Store 权威")
	}

	a := GraphContract{RequestDigest: "r", ExecutionClass: ExecutionAnswer}
	b := a
	b.Deliverables = []ContractRequirement{}
	b.Constraints = []string{}
	b.RequiredEffects = []string{}
	b.RequiredArtifacts = []ContractRequirement{}
	b.RequiredChecks = []ContractRequirement{}
	b.SuccessEvidence = []ContractRequirement{}
	if ComputeGraphContractDigest(a) != ComputeGraphContractDigest(b) {
		t.Fatal("Contract nil/empty slice 应归一为同一 digest")
	}
}

// TestAuthoringStoreRejectsCorruptJournal 验证 authoring 事务日志坏行 fail-closed，
// 不像观测日志一样跳过后继续。
func TestAuthoringStoreRejectsCorruptJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := NewAuthoringStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDraft(GraphDraft{
		ProposalID: "proposal-corrupt", GraphID: "g-corrupt", OwnerTaskID: "task-owner",
		Contract:  GraphContract{RequestDigest: "r", ExecutionClass: ExecutionAnswer},
		Candidate: authoringTestBody("原始"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, authoringJournalName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"seq\":999}\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if recovered, err := NewAuthoringStore(dir); err == nil {
		_ = recovered.Close()
		t.Fatal("损坏 authoring journal 必须拒绝恢复")
	}
}

func stringPointer(value string) *string { return &value }
