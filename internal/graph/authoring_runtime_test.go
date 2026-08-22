package graph

import (
	"context"
	"testing"
	"time"

	"agentgo/internal/runcontract"
)

func commitRuntimeDefinition(t *testing.T, authoring *AuthoringStore, graphID, proposalID string, baseRevision int64) *GraphDefinition {
	t.Helper()
	draftInput := validCompilerDraft()
	draftInput.ProposalID = proposalID
	draftInput.GraphID = graphID
	draftInput.SessionID = "session-runtime"
	draftInput.BaseDefinitionRevision = baseRevision
	draftInput.Candidate.RunID = runcontract.RunID("run-runtime")
	draftInput.Candidate.RunContract = &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: runcontract.RunID("run-runtime"),
		DeadlineAt: time.Now().UTC().Add(time.Hour), FinalizationReserve: time.Minute,
		RecoveryReserve: time.Minute, BudgetProfile: "test", CreatedAt: time.Now().UTC(),
	}
	if baseRevision > 0 {
		base, ok := authoring.GetDefinition(graphID, baseRevision)
		if !ok {
			t.Fatalf("缺少 amendment base Definition %s@%d", graphID, baseRevision)
		}
		draftInput.Candidate.RunID = base.Body.RunID
		draftInput.Candidate.RunContract = base.Body.RunContract
		work := draftInput.Candidate.Nodes["work"]
		work.Task.Title = "实施修改 revision 2"
		draftInput.Candidate.Nodes["work"] = work
	}
	draft, err := authoring.CreateDraft(draftInput)
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	definitionRevision := baseRevision + 1
	compiled, err := (DefinitionCompiler{
		Policies: validCompilerPolicies(), Acceptance: validProposalAcceptance(),
	}).Compile(context.Background(), DefinitionCompileRequest{
		ReportID: "report-" + proposalID, Draft: *draft, DefinitionRevision: definitionRevision,
	})
	if err != nil || !compiled.Report.Accepted {
		t.Fatalf("Compile: report=%+v err=%v", compiled.Report, err)
	}
	report, err := authoring.RecordValidation(compiled.Report)
	if err != nil {
		t.Fatalf("RecordValidation: %v", err)
	}
	definition, err := authoring.CommitDraft(proposalID, draft.DraftRevision, report.ReportID, compiled.Definition)
	if err != nil {
		t.Fatalf("CommitDraft: %v", err)
	}
	return definition
}

func startRequest(definition *GraphDefinition, startID string) StartDefinitionRequest {
	return StartDefinitionRequest{
		StartID: startID, GraphID: definition.GraphID,
		ExpectedDefinitionRevision: definition.Revision,
		ExpectedDefinitionDigest:   definition.DefinitionDigest,
		ExpectedContractDigest:     definition.ContractDigest,
		SessionID:                  definition.SessionID, OwnerTaskID: definition.OwnerTaskID,
	}
}

func TestAuthoringRuntimeStartsCommittedDefinitionAndIsIdempotent(t *testing.T) {
	authoring, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	executions, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Close() })
	board := newFakeBoard()
	adapter := &AuthoringRuntime{Authoring: authoring, Runtime: NewRuntime(executions, board)}
	definition := commitRuntimeDefinition(t, authoring, "g-start-runtime", "proposal-runtime", 0)
	req := startRequest(definition, "start-runtime")

	first, err := adapter.StartDefinition(context.Background(), req)
	if err != nil {
		t.Fatalf("StartDefinition: %v", err)
	}
	if first.Intent.Status != StartStarted || first.RootActivationID != "work@1" || first.ExecutionRef != "graph:g-start-runtime" {
		t.Fatalf("start result 非预期: %+v", first)
	}
	doc, ok := executions.Get(definition.GraphID)
	if !ok || doc.Status != GraphRunning || doc.Revision != definition.Revision {
		t.Fatalf("Execution 未 running 或 revision 漂移: %+v ok=%v", doc, ok)
	}
	if doc.DefinitionDigest != definition.DefinitionDigest || doc.ContractDigest != definition.ContractDigest ||
		doc.SourceProposalID != definition.SourceProposalID {
		t.Fatalf("Execution 未 durable 绑定 Definition: %+v", doc)
	}
	if doc.RunID != definition.Body.RunID || doc.RunContract == nil || doc.RunContract.RunID != definition.Body.RunID {
		t.Fatalf("RunContract 未显式投影: %+v", doc)
	}
	if doc.Nodes["success"].EndOutcome != EndSuccess || doc.Nodes["failed"].EndOutcome != EndFailed || doc.Nodes["blocked"].EndOutcome != EndBlocked {
		t.Fatalf("DefinitionEndOutcome 未显式转换: success=%q failed=%q blocked=%q",
			doc.Nodes["success"].EndOutcome, doc.Nodes["failed"].EndOutcome, doc.Nodes["blocked"].EndOutcome)
	}
	if board.count() != 1 || board.specAt(0).RunID != definition.Body.RunID {
		t.Fatalf("root Task 应发布一次并继承 RunID: count=%d spec=%+v", board.count(), board.specAt(0))
	}
	if board.specAt(0).DefinitionDigestVersion != GraphDefinitionDigestVersionV1 {
		t.Fatalf("authoring marker 未进入 TaskSpec: %+v", board.specAt(0))
	}
	if got, want := board.specAt(0).ProgressContractRef,
		definition.Body.Nodes[definition.Body.Root].ProgressContractRef; got != want || got == "" {
		t.Fatalf("root Task 未继承冻结 ProgressContractRef: got=%q want=%q", got, want)
	}
	if got, want := doc.Nodes["work"].OutputContract, definition.Body.Nodes["work"].OutputContract; got == nil || want == nil ||
		!got.SummaryRequired || len(got.Fields) != len(want.Fields) || board.specAt(0).TypedOutputContract == nil {
		t.Fatalf("NodeOutputContract 未强类型投影到 Runtime/TaskSpec: node=%+v spec=%+v", got, board.specAt(0).TypedOutputContract)
	}

	second, err := adapter.StartDefinition(context.Background(), req)
	if err != nil || second.Intent.StartID != first.Intent.StartID || board.count() != 1 {
		t.Fatalf("重复 start 必须返回同一结果且不重复 Task: second=%+v count=%d err=%v", second, board.count(), err)
	}
	// CompleteStart 本身也必须支持同一旧 CAS 请求的幂等重放。
	replayed, err := authoring.CompleteStart(first.Intent.StartID, 1, first.ExecutionRef)
	if err != nil || replayed.Status != StartStarted || replayed.IntentRevision != first.Intent.IntentRevision {
		t.Fatalf("CompleteStart 幂等重放失败: replayed=%+v err=%v", replayed, err)
	}
}

func TestAuthoringRuntimePreservesAmendedDefinitionRevision(t *testing.T) {
	authoring, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	executions, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Close() })
	_ = commitRuntimeDefinition(t, authoring, "g-amended", "proposal-v1", 0)
	v2 := commitRuntimeDefinition(t, authoring, "g-amended", "proposal-v2", 1)
	adapter := &AuthoringRuntime{Authoring: authoring, Runtime: NewRuntime(executions, newFakeBoard())}
	if _, err := adapter.StartDefinition(context.Background(), startRequest(v2, "start-v2")); err != nil {
		t.Fatal(err)
	}
	doc, _ := executions.Get(v2.GraphID)
	if doc.Revision != 2 || doc.DefinitionDigest != v2.DefinitionDigest {
		t.Fatalf("CreateExecution 不得把 Definition revision 2 归一回 1: %+v", doc)
	}
}

func TestStoreCreateExecutionIsParkedAndLegacyCannotForgeBinding(t *testing.T) {
	authoring, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	_ = commitRuntimeDefinition(t, authoring, "g-parked", "proposal-parked-v1", 0)
	definition := commitRuntimeDefinition(t, authoring, "g-parked", "proposal-parked-v2", 1)
	doc, err := graphExecutionDocument(*definition)
	if err != nil {
		t.Fatal(err)
	}

	executions, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Close() })
	if err := executions.createExecution(doc); err != nil {
		t.Fatalf("createExecution: %v", err)
	}
	parked, ok := executions.Get(definition.GraphID)
	if !ok || parked.Status != GraphPending || parked.Revision != 2 ||
		parked.Nodes[parked.Root].Status != NodeInactive || parked.Nodes[parked.Root].Execution != nil {
		t.Fatalf("createExecution 必须只 durable parked Execution，不得激活 root: %+v ok=%v", parked, ok)
	}

	legacyStore, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacyStore.Close() })
	legacy := mustParse(t, tinyDocJSON)
	legacy.DefinitionDigestVersion = GraphDefinitionDigestVersionV1
	legacy.DefinitionDigest = "forged-definition"
	legacy.ContractDigest = "forged-contract"
	legacy.SourceProposalID = "forged-proposal"
	if err := legacyStore.SubmitGraph(legacy); err == nil {
		t.Fatal("legacy SubmitGraph 不得伪造 Authoring Definition 绑定")
	}
	if _, exists := legacyStore.Get(legacy.GraphID); exists {
		t.Fatal("伪造绑定被拒后不得留下 Execution")
	}
}

func TestAuthoringRuntimeRecoversStartIntentExecutionCrashWindow(t *testing.T) {
	authoringDir, executionDir := t.TempDir(), t.TempDir()
	authoring, err := NewAuthoringStore(authoringDir)
	if err != nil {
		t.Fatal(err)
	}
	executions, err := NewStore(executionDir)
	if err != nil {
		_ = authoring.Close()
		t.Fatal(err)
	}
	board := newFakeBoard()
	definition := commitRuntimeDefinition(t, authoring, "g-start-crash", "proposal-crash", 0)
	req := startRequest(definition, "start-crash")
	intent, err := authoring.BeginStart(StartIntent{
		StartID: req.StartID, GraphID: req.GraphID,
		DefinitionRevision: req.ExpectedDefinitionRevision, DefinitionDigest: req.ExpectedDefinitionDigest,
		ContractDigest: req.ExpectedContractDigest, SessionID: req.SessionID, OwnerTaskID: req.OwnerTaskID,
		RootActivationID: definition.Body.Root + "@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := graphExecutionDocument(*definition)
	if err != nil {
		t.Fatal(err)
	}
	rootID, err := NewRuntime(executions, board).startCommittedExecution(doc)
	if err != nil || rootID != "work@1" || board.count() != 1 {
		t.Fatalf("构造 crash window 失败: root=%s count=%d err=%v", rootID, board.count(), err)
	}
	if current, _ := authoring.GetStartIntent(intent.StartID); current.Status != StartRequested {
		t.Fatalf("crash window 应仍 requested: %+v", current)
	}
	// Windows：两个长生命周期 journal 句柄先关闭，再复用目录恢复。
	if err := executions.Close(); err != nil {
		t.Fatal(err)
	}
	if err := authoring.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredAuthoring, err := NewAuthoringStore(authoringDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recoveredAuthoring.Close() })
	recoveredExecutions, err := NewStore(executionDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recoveredExecutions.Close() })
	if err := recoveredExecutions.Recover(); err != nil {
		t.Fatalf("recover Graph Store: %v", err)
	}
	adapter := &AuthoringRuntime{Authoring: recoveredAuthoring, Runtime: NewRuntime(recoveredExecutions, board)}
	result, err := adapter.StartDefinition(context.Background(), req)
	if err != nil || result.Intent.Status != StartStarted || board.count() != 1 {
		t.Fatalf("重试应补 CompleteStart 且不重复 Task: result=%+v count=%d err=%v", result, board.count(), err)
	}
}

func TestAuthoringRuntimeFailsIntentBeforeExecutionAndAllowsRetry(t *testing.T) {
	authoring, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	definition := commitRuntimeDefinition(t, authoring, "g-start-fail", "proposal-fail", 0)
	req := startRequest(definition, "start-fail")
	broken := &AuthoringRuntime{Authoring: authoring, Runtime: NewRuntime(nil, newFakeBoard())}
	result, err := broken.StartDefinition(context.Background(), req)
	if err == nil || result.Intent.Status != StartFailed || result.Intent.FailureCode != "execution_start_failed" {
		t.Fatalf("Runtime 缺 Store 应 durable FailStart: result=%+v err=%v", result, err)
	}
	replayedFailure, replayErr := authoring.FailStart(result.Intent.StartID, 1, "execution_start_failed", "重复回放")
	if replayErr != nil || replayedFailure.Status != StartFailed || replayedFailure.FailureReason != result.Intent.FailureReason {
		t.Fatalf("FailStart 幂等重放不得改写首个失败: replayed=%+v err=%v", replayedFailure, replayErr)
	}

	executions, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Close() })
	req.StartID = "start-retry"
	retry := &AuthoringRuntime{Authoring: authoring, Runtime: NewRuntime(executions, newFakeBoard())}
	result, err = retry.StartDefinition(context.Background(), req)
	if err != nil || result.Intent.Status != StartStarted || result.Intent.StartID != "start-retry" {
		t.Fatalf("failed intent 后新 start_id 应可重试: result=%+v err=%v", result, err)
	}
}

func TestAuthoringRuntimeRejectsWrongDigestBeforeIntent(t *testing.T) {
	authoring, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	executions, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Close() })
	definition := commitRuntimeDefinition(t, authoring, "g-start-wrong", "proposal-wrong", 0)
	req := startRequest(definition, "start-wrong")
	req.ExpectedDefinitionDigest = "wrong"
	adapter := &AuthoringRuntime{Authoring: authoring, Runtime: NewRuntime(executions, newFakeBoard())}
	if _, err := adapter.StartDefinition(context.Background(), req); err == nil {
		t.Fatal("错误 digest 必须在 BeginStart 前拒绝")
	}
	if _, found := authoring.GetStartIntent(req.StartID); found {
		t.Fatal("错误 digest 不得留下 StartIntent")
	}
	if _, found := executions.Get(req.GraphID); found {
		t.Fatal("错误 digest 不得创建 Execution")
	}
}

func TestRuntimeEndOutcomeProjection(t *testing.T) {
	tests := []struct {
		in   DefinitionEndOutcome
		want EndOutcome
	}{
		{DefinitionEndSuccess, EndSuccess}, {DefinitionEndFailed, EndFailed},
		{DefinitionEndBlocked, EndBlocked}, {DefinitionEndCancelled, EndCancelled},
	}
	for _, test := range tests {
		got, err := runtimeEndOutcome(test.in)
		if err != nil || got != test.want {
			t.Fatalf("runtimeEndOutcome(%q)=%q err=%v，want %q", test.in, got, err, test.want)
		}
	}
	if _, err := runtimeEndOutcome("unknown"); err == nil {
		t.Fatal("未知 DefinitionEndOutcome 必须拒绝")
	}
}

func TestGraphChangeCommitAffectsOnlyFutureActivation(t *testing.T) {
	authoring, err := NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	executions, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Close() })
	board := newFakeBoard()
	runtime := NewRuntime(executions, board)
	definition := commitRuntimeDefinition(t, authoring, "g-change-runtime", "proposal-change-base", 0)
	adapter := &AuthoringRuntime{Authoring: authoring, Runtime: runtime}
	if _, err := adapter.StartDefinition(context.Background(), startRequest(definition, "start-change-base")); err != nil {
		t.Fatal(err)
	}
	rootBefore := nodeOf(t, executions, definition.GraphID, "work")
	if rootBefore.Execution == nil || rootBefore.Execution.DefinitionRevision != 1 {
		t.Fatalf("root activation 应冻结 revision 1: %+v", rootBefore.Execution)
	}

	updatedEnd := definition.Body.Nodes["success"]
	updatedEnd.Task = &NodeTask{Title: "revision 2 成功收官"}
	change, err := authoring.CreateGraphChangeProposal(GraphChangeProposal{
		ChangeID: "change-runtime", GraphID: definition.GraphID,
		BaseDefinitionRevision: definition.Revision, BaseDefinitionDigest: definition.DefinitionDigest,
		SessionID: definition.SessionID, OwnerTaskID: "change-task", Reason: "更新未来 end 定义",
		Patch: GraphDefinitionPatch{UpsertNodes: []GraphDefinitionNodeUpsert{{ID: "success", GraphDefinitionNode: updatedEnd}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := ApplyGraphDefinitionPatch(definition.Body, change.Patch)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := (DefinitionCompiler{Policies: validCompilerPolicies(), Acceptance: validProposalAcceptance()}).Compile(
		context.Background(), DefinitionCompileRequest{
			ReportID: "change-runtime-report",
			Draft: GraphDraft{
				ProposalID: change.ChangeID, GraphID: change.GraphID, SessionID: change.SessionID,
				OwnerTaskID: definition.OwnerTaskID, BaseDefinitionRevision: 1,
				DraftRevision: change.ProposalRevision, Status: DraftEditing,
				RequestRef: definition.Contract.RequestRef, RequestDigest: definition.Contract.RequestDigest,
				Contract: definition.Contract, Candidate: candidate,
			},
			DefinitionRevision: 2,
		})
	if err != nil || !compiled.Report.Accepted {
		t.Fatalf("compile change: report=%+v err=%v", compiled.Report, err)
	}
	compiled.Report.SubjectKind = "change"
	compiled.Report.SubjectID = change.ChangeID
	compiled.Report.SubjectRevision = change.ProposalRevision
	report, err := authoring.RecordValidation(compiled.Report)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := authoring.CommitGraphChange(change.ChangeID, change.ProposalRevision, report.ReportID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.AdoptCommittedDefinition(*v2); err != nil {
		t.Fatal(err)
	}
	doc, _ := executions.Get(definition.GraphID)
	if doc.Revision != 2 || doc.DefinitionDigest != v2.DefinitionDigest || doc.Nodes["success"].Task.Title != "revision 2 成功收官" {
		t.Fatalf("Execution 未 adopt revision 2: %+v", doc)
	}
	rootAfter := doc.Nodes["work"]
	if rootAfter.Execution == nil || rootAfter.Execution.DefinitionRevision != 1 ||
		rootAfter.Execution.Definition.Task.Title != rootBefore.Execution.Definition.Task.Title {
		t.Fatalf("在途 root definition 被改写: before=%+v after=%+v", rootBefore.Execution, rootAfter.Execution)
	}

	mustTerminal(t, runtime, TerminalFact{
		GraphID: definition.GraphID, NodeID: "work", ActivationID: "work@1",
		TaskID: rootAfter.Execution.TaskID, Status: NodeCompleted, Result: map[string]any{"changed": true},
	})
	end := nodeOf(t, executions, definition.GraphID, "success")
	if end.Execution == nil || end.Execution.DefinitionRevision != 2 || end.Execution.Definition.Task.Title != "revision 2 成功收官" {
		t.Fatalf("未来 end activation 未使用 revision 2: %+v", end.Execution)
	}
	if got := graphStatusOf(t, executions, definition.GraphID); got != GraphCompleted {
		t.Fatalf("Graph 应按新 success end 收官: %s", got)
	}
}

func TestReconcileCommittedDefinitionsRepairsCrossJournalCrash(t *testing.T) {
	authoringDir, executionDir := t.TempDir(), t.TempDir()
	authoring, err := NewAuthoringStore(authoringDir)
	if err != nil {
		t.Fatal(err)
	}
	executions, err := NewStore(executionDir)
	if err != nil {
		_ = authoring.Close()
		t.Fatal(err)
	}
	board := newFakeBoard()
	v1 := commitRuntimeDefinition(t, authoring, "g-reconcile-change", "proposal-reconcile-v1", 0)
	adapter := &AuthoringRuntime{Authoring: authoring, Runtime: NewRuntime(executions, board)}
	if _, err := adapter.StartDefinition(context.Background(), startRequest(v1, "start-reconcile")); err != nil {
		t.Fatal(err)
	}
	v2 := commitRuntimeDefinition(t, authoring, v1.GraphID, "proposal-reconcile-v2", 1)
	if current, _ := executions.Get(v1.GraphID); current.Revision != 1 {
		t.Fatalf("crash 前 Authoring 已到 v2、Execution 应仍为 v1: %+v", current)
	}
	if err := executions.Close(); err != nil {
		t.Fatal(err)
	}
	if err := authoring.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredAuthoring, err := NewAuthoringStore(authoringDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recoveredAuthoring.Close() })
	recoveredExecutions, err := NewStore(executionDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recoveredExecutions.Close() })
	if err := recoveredExecutions.Recover(); err != nil {
		t.Fatal(err)
	}
	reconciler := &AuthoringRuntime{Authoring: recoveredAuthoring, Runtime: NewRuntime(recoveredExecutions, board)}
	if err := reconciler.ReconcileCommittedDefinitions(); err != nil {
		t.Fatalf("ReconcileCommittedDefinitions: %v", err)
	}
	current, _ := recoveredExecutions.Get(v1.GraphID)
	if current.Revision != 2 || current.DefinitionDigest != v2.DefinitionDigest {
		t.Fatalf("启动 reconciliation 未补 adoption: %+v", current)
	}
	if root := current.Nodes[current.Root]; root.Execution == nil || root.Execution.DefinitionRevision != 1 {
		t.Fatalf("reconciliation 不得改写在途 activation: %+v", root.Execution)
	}
}
