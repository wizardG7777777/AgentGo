package graph

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agentgo/internal/runbudget"
	"agentgo/internal/runcontract"
)

type rejectingRunBudgetGate struct{}

func (rejectingRunBudgetGate) CanReserve(runcontract.RunID, runcontract.BudgetUsage, time.Time) error {
	return fmt.Errorf("Run model_calls 预算已耗尽")
}

func recoveryDeltaFixture(t *testing.T) (*Runtime, *Store, map[string]any) {
	t.Helper()
	store, runtime, board := newTestRuntime(t)
	failure := map[string]any{
		"status": "blocked", "reason_code": "loop_intervention_required",
		"_checkpoint_ref": "checkpoint-source", "_observation_delta_ref": "observation:sha256:source",
		"_failure_fingerprint": "failure:sha256:source",
	}
	now := time.Now().UTC()
	run := &runcontract.RunContract{Schema: runcontract.SchemaV1, RunID: "run-recovery-delta",
		CreatedAt: now.Add(-time.Minute), DeadlineAt: now.Add(time.Hour),
		FinalizationReserve: time.Minute, RecoveryReserve: 5 * time.Minute, BudgetProfile: "test/v2"}
	doc := &GraphDocument{Schema: SchemaV2, GraphID: "g-recovery-delta", RunID: run.RunID,
		RunContract: run, Revision: 1,
		Root: "work", Status: GraphPending, Nodes: map[string]Node{
			"work": {Kind: KindAgent, Status: NodeInactive,
				Task: &NodeTask{Title: "工作", Description: "工作"},
				Next: []Transition{{To: "recovery", TargetInput: "failure_context", When: &Condition{Event: EventBlocked}}},
			},
			"recovery": {Kind: KindController, Status: NodeInactive,
				Task: &NodeTask{Title: "恢复", Description: "result.decision 必须为 retry 或 blocked", RequiredInputs: []string{"failure_context"}},
				Next: []Transition{{To: "work", ReplayInputs: true, When: recoveryDecision("retry")},
					{To: "blocked", When: recoveryDecision("blocked")}},
				Metadata: map[string]string{MetadataControllerRole: string(ControllerRoleLoopRecovery),
					MetadataRecoveryMaxRetries: "2", MetadataRecoveryDeltaSchema: RecoveryDeltaSchemaV1},
				OutputContract: &NodeOutputContract{SummaryRequired: true, Fields: []OutputFieldContract{
					{Path: "$.decision", Type: "string", Required: true},
					{Path: "$.recovery_delta", Type: "object"},
				}},
			},
			"blocked": {Kind: KindEnd, Status: NodeInactive, Task: &NodeTask{Title: "阻塞"}, EndOutcome: EndBlocked},
		}}
	if err := runtime.SubmitGraph(doc); err != nil {
		t.Fatal(err)
	}
	work := nodeOf(t, store, doc.GraphID, "work")
	taskID := work.Execution.TaskID
	board.snapshots[doc.GraphID+"\x00work@1"] = GraphTaskSnapshot{TaskID: taskID,
		NodeKind: KindAgent, TerminalStatus: NodeBlocked, Result: failure}
	mustTerminal(t, runtime, TerminalFact{GraphID: doc.GraphID, NodeID: "work", ActivationID: "work@1",
		TaskID: taskID, Status: NodeBlocked, Result: failure})
	recovery := nodeOf(t, store, doc.GraphID, "recovery")
	if recovery.Execution == nil || recovery.Execution.ActivationID != "recovery@1" {
		t.Fatalf("recovery activation 未创建: %+v", recovery.Execution)
	}
	result := map[string]any{"decision": "retry", "recovery_delta": map[string]any{
		"schema": RecoveryDeltaSchemaV1, "source_checkpoint_ref": "checkpoint-source",
		"source_observation_delta_ref": "observation:sha256:source",
		"failure_fingerprint":          "failure:sha256:source", "changed_dimensions": []any{"strategy"},
		"strategy": "先修改最小调用点", "first_required_action": "edit_file src/a.go",
		"expected_milestone": "目标测试通过",
	}}
	return runtime, store, result
}

func TestRecoveryRetryRejectedBeforeOutcomeWhenExecutionWindowClosed(t *testing.T) {
	runtime, store, _ := recoveryDeltaFixture(t)
	doc, _ := store.Get("g-recovery-delta")
	err := runtime.ValidateRecoveryRetryStart("g-recovery-delta", "recovery", "recovery@1",
		doc.RunContract.PhaseStartDeadline(runcontract.PhaseExecution))
	if err == nil || !strings.Contains(err.Error(), RecoveryRetryUnstartableReasonCode) {
		t.Fatalf("execution window 关闭后 retry 必须在提交前拒绝: %v", err)
	}
}

func TestRecoveryRetryRejectedWhenExecutionWindowIsTooShort(t *testing.T) {
	runtime, store, _ := recoveryDeltaFixture(t)
	doc, _ := store.Get("g-recovery-delta")
	now := doc.RunContract.PhaseStartDeadline(runcontract.PhaseExecution).Add(-14 * time.Second)
	err := runtime.ValidateRecoveryRetryStart("g-recovery-delta", "recovery", "recovery@1", now)
	if err == nil || !strings.Contains(err.Error(), RecoveryRetryUnstartableReasonCode) ||
		!strings.Contains(err.Error(), "最小可执行窗口") {
		t.Fatalf("不足一次最小调用窗口的 retry 必须在提交前拒绝: %v", err)
	}
}

func TestRecoveryRetryRejectedWhenRunExecutionGrantUnavailable(t *testing.T) {
	runtime, _, _ := recoveryDeltaFixture(t)
	runtime.SetRunBudgetGate(rejectingRunBudgetGate{})
	err := runtime.ValidateRecoveryRetryStart("g-recovery-delta", "recovery", "recovery@1", time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), RecoveryRetryUnstartableReasonCode) ||
		!strings.Contains(err.Error(), "execution grant") {
		t.Fatalf("Run grant 耗尽必须在 retry 提交前拒绝: %v", err)
	}
}

func TestRecoveryDeltaRequiredBeforeRetryAndInjectedIntoNextActivation(t *testing.T) {
	runtime, _, result := recoveryDeltaFixture(t)
	if err := runtime.CheckActivationOutlet("g-recovery-delta", "recovery", "recovery@1", "completed",
		map[string]any{"decision": "retry"}); err == nil || !strings.Contains(err.Error(), "缺少 recovery_delta") {
		t.Fatalf("无 RecoveryDelta 的 retry 必须可修正地拒绝: %v", err)
	}
	if err := runtime.CheckActivationOutlet("g-recovery-delta", "recovery", "recovery@1", "completed", result); err != nil {
		t.Fatalf("合法 RecoveryDelta 应通过预检: %v", err)
	}
	replayed, err := runtime.recoveryReplayInputs("g-recovery-delta", TransitionRecord{
		SourceNodeID: "recovery", SourceActivationID: "recovery@1", TargetNodeID: "work", ReplayInputs: true,
	}, result)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, input := range replayed {
		if input.TargetInput == "recovery_directive" && strings.Contains(input.Summary, "edit_file src/a.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("RecoveryDelta 未作为 recovery_directive 注入: %+v", replayed)
	}
}

func TestBindRecoveryDeltaAuthorityFillsFrozenSourceFields(t *testing.T) {
	runtime, _, _ := recoveryDeltaFixture(t)
	bound, err := runtime.BindRecoveryDeltaAuthority("g-recovery-delta", "recovery", "recovery@1", RecoveryDelta{
		ChangedDimensions: []string{"strategy"}, Strategy: "换用最小修改",
		FirstRequiredAction: "edit_file src/a.go", ExpectedMilestone: "目标检查通过",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Schema != RecoveryDeltaSchemaV1 || bound.SourceCheckpointRef != "checkpoint-source" ||
		bound.SourceObservationDeltaRef != "observation:sha256:source" || bound.FailureFingerprint != "failure:sha256:source" {
		t.Fatalf("framework 未自动绑定 recovery authority: %+v", bound)
	}
}

func TestRecoveryDeltaRetryCreatesNextActivationWithFrozenDirective(t *testing.T) {
	runtime, store, result := recoveryDeltaFixture(t)
	recovery := nodeOf(t, store, "g-recovery-delta", "recovery")
	mustTerminal(t, runtime, TerminalFact{
		GraphID: "g-recovery-delta", NodeID: "recovery", ActivationID: "recovery@1",
		TaskID: recovery.Execution.TaskID, Status: NodeCompleted, Result: result,
	})
	work := nodeOf(t, store, "g-recovery-delta", "work")
	if work.Execution == nil || work.Execution.ActivationID != "work@2" {
		t.Fatalf("合法 RecoveryDelta retry 必须创建 work@2: %+v", work.Execution)
	}
	found := false
	for _, input := range work.Execution.Input {
		if input.TargetInput == "recovery_directive" && strings.Contains(input.Summary, "edit_file src/a.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("work@2 缺少冻结 recovery_directive: %+v", work.Execution.Input)
	}
}

func TestRecoveryStartPermitIsDurableInputForNextActivation(t *testing.T) {
	runtime, store, result := recoveryDeltaFixture(t)
	doc, _ := store.Get("g-recovery-delta")
	authority, err := runbudget.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if err := authority.InitializeRun(*doc.RunContract, runcontract.BudgetLimit{ModelCalls: 1}); err != nil {
		t.Fatal(err)
	}
	runtime.SetRunBudgetGate(authority)
	partial := RecoveryDelta{ChangedDimensions: []string{"strategy"},
		Strategy: "聚焦续做", FirstRequiredAction: "读取现有差异", ExpectedMilestone: "目标检查通过"}
	bound, err := runtime.BindRecoveryDeltaAuthority(doc.GraphID, "recovery", "recovery@1", partial)
	if err != nil {
		t.Fatal(err)
	}
	if bound.StartPermitRef == "" {
		t.Fatal("RecoveryDelta 缺少 framework start permit")
	}
	raw, _ := json.Marshal(bound)
	var encoded map[string]any
	_ = json.Unmarshal(raw, &encoded)
	result["recovery_delta"] = encoded
	recovery := nodeOf(t, store, doc.GraphID, "recovery")
	mustTerminal(t, runtime, TerminalFact{GraphID: doc.GraphID, NodeID: "recovery",
		ActivationID: "recovery@1", TaskID: recovery.Execution.TaskID,
		Status: NodeCompleted, Result: result})
	work := nodeOf(t, store, doc.GraphID, "work")
	if work.Execution == nil || work.Execution.ActivationID != "work@2" {
		t.Fatalf("retry 未创建 work@2: %+v", work.Execution)
	}
	spec := runtime.taskSpecFor(doc.GraphID, "work", work, *work.Execution)
	if spec.RunBudgetPermitRef != bound.StartPermitRef {
		t.Fatalf("RecoveryStartPermit 未冻结进目标 TaskSpec: got=%q want=%q",
			spec.RunBudgetPermitRef, bound.StartPermitRef)
	}
}

func TestRecoveryTerminalWithoutRetryCancelsPreparedStartPermit(t *testing.T) {
	runtime, store, _ := recoveryDeltaFixture(t)
	doc, _ := store.Get("g-recovery-delta")
	authority, err := runbudget.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if err := authority.InitializeRun(*doc.RunContract, runcontract.BudgetLimit{ModelCalls: 1}); err != nil {
		t.Fatal(err)
	}
	runtime.SetRunBudgetGate(authority)
	partial := RecoveryDelta{ChangedDimensions: []string{"strategy"},
		Strategy: "准备后改交 blocked", FirstRequiredAction: "none", ExpectedMilestone: "blocked"}
	if _, err := runtime.BindRecoveryDeltaAuthority(doc.GraphID, "recovery", "recovery@1", partial); err != nil {
		t.Fatal(err)
	}
	recovery := nodeOf(t, store, doc.GraphID, "recovery")
	mustTerminal(t, runtime, TerminalFact{GraphID: doc.GraphID, NodeID: "recovery",
		ActivationID: "recovery@1", TaskID: recovery.Execution.TaskID,
		Status: NodeBlocked, Result: map[string]any{"decision": "blocked", "blocked_reason": "无可验证变化"}})
	snapshot, ok, err := authority.Snapshot(doc.RunID)
	if err != nil || !ok || snapshot.Reserved != (runcontract.BudgetUsage{}) {
		t.Fatalf("recovery 未 retry 后 permit 未回收: snapshot=%+v ok=%v err=%v", snapshot, ok, err)
	}
}

func TestRecoveryBindValidationFailureDoesNotReserveStartPermit(t *testing.T) {
	runtime, store, _ := recoveryDeltaFixture(t)
	doc, _ := store.Get("g-recovery-delta")
	authority, err := runbudget.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if err = authority.InitializeRun(*doc.RunContract, runcontract.BudgetLimit{ModelCalls: 1}); err != nil {
		t.Fatal(err)
	}
	runtime.SetRunBudgetGate(authority)
	partial := RecoveryDelta{ChangedDimensions: []string{"strategy"},
		Strategy: strings.Repeat("长", 601), FirstRequiredAction: "读取现有差异", ExpectedMilestone: "目标检查通过"}
	if _, err = runtime.BindRecoveryDeltaAuthority(doc.GraphID, "recovery", "recovery@1", partial); err == nil ||
		!strings.Contains(err.Error(), "strategy 超过 600 rune") {
		t.Fatalf("超长 RecoveryDelta 必须被拒: %v", err)
	}
	snapshot, ok, err := authority.Snapshot(doc.RunID)
	if err != nil || !ok || snapshot.Reserved != (runcontract.BudgetUsage{}) || snapshot.Settled.ModelCalls != 0 {
		t.Fatalf("bind 校验失败前不得预留/伪消费 permit: snapshot=%+v ok=%v err=%v", snapshot, ok, err)
	}
	valid := RecoveryDelta{ChangedDimensions: []string{"strategy"},
		Strategy: "收敛重试", FirstRequiredAction: "run_check verification", ExpectedMilestone: "验证通过"}
	bound, err := runtime.BindRecoveryDeltaAuthority(doc.GraphID, "recovery", "recovery@1", valid)
	if err != nil || bound.StartPermitRef == "" {
		t.Fatalf("前一次参数校验失败不得烧掉修正后的 permit: bound=%+v err=%v", bound, err)
	}
	if err := authority.ValidateExecutionPermit(doc.RunID, bound.StartPermitRef, time.Now().UTC()); err != nil {
		t.Fatalf("修正后 permit 应保持 active: %v", err)
	}
}

func TestRecoveryDeltaRejectsAuthorityMismatch(t *testing.T) {
	runtime, _, result := recoveryDeltaFixture(t)
	delta := result["recovery_delta"].(map[string]any)
	delta["failure_fingerprint"] = "failure:sha256:other"
	if err := runtime.CheckActivationOutlet("g-recovery-delta", "recovery", "recovery@1", "completed", result); err == nil ||
		!strings.Contains(err.Error(), "failure_context 不一致") {
		t.Fatalf("RecoveryDelta authority mismatch 必须拒绝: %v", err)
	}
}

func TestRecoveryDeltaDefinitionChangeRequiresCurrentRevisionAdvance(t *testing.T) {
	runtime, store, result := recoveryDeltaFixture(t)
	delta := result["recovery_delta"].(map[string]any)
	delta["changed_dimensions"] = []any{"definition"}
	if err := runtime.CheckActivationOutlet("g-recovery-delta", "recovery", "recovery@1", "completed", result); err == nil ||
		!strings.Contains(err.Error(), "Definition revision 未前进") {
		t.Fatalf("没有 revision 前进的 definition delta 必须拒绝: %v", err)
	}
	doc, _ := store.Get("g-recovery-delta")
	blocked := doc.Nodes["blocked"]
	if _, err := store.PatchGraph(doc.GraphID, doc.Revision, DefinitionPatch{UpsertNodes: []NodeDefUpsert{{
		ID: "blocked", Kind: blocked.Kind, Task: &NodeTask{Title: "阻塞终态（revision 2）"},
		Next: blocked.Next, EndOutcome: blocked.EndOutcome,
	}}}); err != nil {
		t.Fatalf("推进 Definition revision: %v", err)
	}
	if err := runtime.CheckActivationOutlet("g-recovery-delta", "recovery", "recovery@1", "completed", result); err != nil {
		t.Fatalf("当前 Graph revision 已前进后 definition delta 应通过: %v", err)
	}
	replayed, err := runtime.recoveryReplayInputs("g-recovery-delta", TransitionRecord{
		SourceNodeID: "recovery", SourceActivationID: "recovery@1", TargetNodeID: "work", ReplayInputs: true,
	}, result)
	if err != nil {
		t.Fatal(err)
	}
	foundDirective := false
	for _, input := range replayed {
		if input.TargetInput == "recovery_directive" {
			foundDirective = true
		}
	}
	if !foundDirective {
		t.Fatalf("Definition 已变化时仍须注入 permit/first-action recovery_directive: %+v", replayed)
	}
}

func TestReplaceRecoveryDirectiveKeepsOnlyCurrentGeneration(t *testing.T) {
	inputs := []InputBinding{
		{TargetInput: "original", Summary: "业务输入"},
		{SourceActivationID: "recovery@1", TargetInput: "recovery_directive", Summary: "旧指令"},
		{TargetInput: "other", Summary: "其它输入"},
	}
	current := InputBinding{SourceActivationID: "recovery@2", TargetInput: "recovery_directive", Summary: "新指令"}
	got := replaceRecoveryDirective(inputs, current)
	count := 0
	for _, input := range got {
		if input.TargetInput != "recovery_directive" {
			continue
		}
		count++
		if input.SourceActivationID != "recovery@2" || input.Summary != "新指令" {
			t.Fatalf("保留了非当前代 recovery_directive: %+v", input)
		}
	}
	if count != 1 || len(got) != 3 {
		t.Fatalf("recovery_directive 必须保持单值，实际 inputs=%+v", got)
	}
}

func TestRecoveryDeltaV3RequiresReadFileFirstAction(t *testing.T) {
	base := map[string]any{
		"schema": RecoveryDeltaSchemaV3, "source_checkpoint_ref": "checkpoint",
		"failure_fingerprint": "failure", "changed_dimensions": []any{"strategy"},
		"strategy": "从目标读集直接进入 mutation", "expected_milestone": "形成补丁并检查",
	}
	base["first_action"] = map[string]any{"tool": "edit_file", "path": "src/a.py"}
	if _, err := decodeRecoveryDelta(map[string]any{"recovery_delta": base}); err == nil ||
		!strings.Contains(err.Error(), "必须是带 path 的 read_file") {
		t.Fatalf("v3 直接复用旧 Activation edit authority 必须拒绝: %v", err)
	}
	base["first_action"] = map[string]any{"tool": "read_file", "path": "src/a.py"}
	if _, err := decodeRecoveryDelta(map[string]any{"recovery_delta": base}); err != nil {
		t.Fatalf("v3 合法首读应通过: %v", err)
	}
}

func TestRecoveryDeltaV4RequiresCanonicalEvidenceContract(t *testing.T) {
	base := map[string]any{
		"schema": RecoveryDeltaSchemaV4, "source_checkpoint_ref": "checkpoint",
		"failure_fingerprint": "failure", "changed_dimensions": []any{"strategy"},
		"strategy": "完整覆盖后再决定是否编辑", "expected_milestone": "形成 typed change decision",
		"first_action": map[string]any{"tool": "read_file", "path": "src/a.py"},
	}
	base["evidence_contract"] = map[string]any{"files": []any{"src/a.py", "src/b.py"}}
	delta, err := decodeRecoveryDelta(map[string]any{"recovery_delta": base})
	if err != nil || delta.EvidenceContract == nil || len(delta.EvidenceContract.Files) != 2 {
		t.Fatalf("v4 合法 EvidenceContract 被拒绝: delta=%+v err=%v", delta, err)
	}
	base["evidence_contract"] = map[string]any{"files": []any{"src/b.py", "src/a.py"}}
	if _, err := decodeRecoveryDelta(map[string]any{"recovery_delta": base}); err == nil ||
		!strings.Contains(err.Error(), "files[0]") {
		t.Fatalf("v4 首动作必须绑定首个 evidence 文件: %v", err)
	}
	base["evidence_contract"] = map[string]any{"files": []any{"../escape.py"}}
	base["first_action"] = map[string]any{"tool": "read_file", "path": "../escape.py"}
	if _, err := decodeRecoveryDelta(map[string]any{"recovery_delta": base}); err == nil ||
		!strings.Contains(err.Error(), "逃逸") {
		t.Fatalf("v4 evidence path 必须限制在项目相对根: %v", err)
	}
}
