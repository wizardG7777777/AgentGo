package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
)

type simpleGraphAcceptancePass struct{}

func (simpleGraphAcceptancePass) EvaluateProposal(_ context.Context, _ graph.ProposalAcceptanceInput) (graph.ProposalAcceptanceDecision, error) {
	return graph.ProposalAcceptanceDecision{
		Verdict: graph.ProposalAcceptancePass, Ref: "proposal-acceptance:simple-test",
	}, nil
}

func newGraphAuthoringToolEnv(t *testing.T) (GraphAuthoringGroup, *graph.AuthoringStore) {
	t.Helper()
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	if err := tasks.PublishTask(&model.Task{ID: "scheduler-root", EventType: "__scheduler__", Description: "实现用户请求"}); err != nil {
		t.Fatal(err)
	}
	authoring, err := graph.NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	return GraphAuthoringGroup{
		Store: authoring, TaskStore: tasks, Holder: &fakeHolder{id: "scheduler-root"},
		SessionID: func() string { return "session-1" },
	}, authoring
}

func TestGraphAuthoringSchemasUseNativeObjectsAndArrays(t *testing.T) {
	registry := agent.NewToolRegistry()
	GraphAuthoringGroup{}.Register(registry)
	want := []string{
		"create_graph_draft", "configure_simple_graph_draft", "patch_graph_draft", "read_graph_draft",
		"validate_graph_draft", "validate_current_graph_draft", "commit_graph_draft", "commit_current_graph_draft",
		"start_graph", "start_current_graph",
		"propose_graph_change", "read_graph_change", "validate_graph_change", "commit_graph_change",
	}
	for _, name := range want {
		if !slicesContains(registry.Names(), name) {
			t.Fatalf("缺少 authoring tool %s，实际=%v", name, registry.Names())
		}
	}
	for _, def := range registry.Defs() {
		if def.Name != "create_graph_draft" && def.Name != "configure_simple_graph_draft" && def.Name != "patch_graph_draft" && def.Name != "propose_graph_change" {
			continue
		}
		properties, _ := def.Parameters["properties"].(map[string]any)
		if graphField, exists := properties["graph"]; exists || graphField != nil {
			t.Fatalf("%s 不得暴露完整 Graph JSON string: %#v", def.Name, properties)
		}
		if def.Name == "create_graph_draft" {
			if len(properties) != 0 {
				t.Fatalf("create_graph_draft 必须保持最小空 Draft schema: %#v", properties)
			}
			continue
		}
		if def.Name == "configure_simple_graph_draft" {
			if len(properties) != 1 || properties["execution_class"] == nil {
				t.Fatalf("configure_simple_graph_draft 只应暴露 execution_class: %#v", properties)
			}
			continue
		}
		if nodes, ok := properties["upsert_nodes"].(map[string]any); !ok || nodes["type"] != "array" {
			t.Fatalf("%s 节点参数必须是原生 array: %#v", def.Name, properties)
		}
	}
}

func TestConfigureSimpleGraphDraftBuildsFrameworkOwnedAcceptedShape(t *testing.T) {
	group, authoring := newGraphAuthoringToolEnv(t)
	if _, err := group.createDraft(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	raw, err := group.configureSimpleDraft(context.Background(), map[string]any{"execution_class": " mutating "})
	if err != nil {
		t.Fatal(err)
	}
	var draft graph.GraphDraft
	if err := json.Unmarshal([]byte(raw), &draft); err != nil {
		t.Fatal(err)
	}
	work := draft.Candidate.Nodes["work"]
	acceptance := draft.Candidate.Nodes["acceptance"]
	if draft.DraftRevision != 2 || draft.Candidate.Root != "work" || draft.Contract.ExecutionClass != graph.ExecutionMutating ||
		!draft.Contract.RequiresAcceptance || len(draft.Contract.RequiredEffects) != 1 ||
		work.Kind != graph.KindAgent || work.Metadata["authoring_template"] != "simple-task/v1" ||
		work.ProgressContractRef != policycatalog.ProgressCodeChangeV1 ||
		acceptance.Kind != graph.KindAcceptance || acceptance.ProgressContractRef != policycatalog.ProgressVerificationV1 ||
		len(draft.Candidate.Nodes) != 9 {
		t.Fatalf("simple task graph 未由 framework 完整生成: %+v", draft)
	}
	if _, err := group.configureSimpleDraft(context.Background(), map[string]any{"execution_class": "mutating"}); err != nil {
		t.Fatalf("同 execution_class 重放应幂等: %v", err)
	}
	persisted, ok := authoring.GetDraft(draft.ProposalID)
	if !ok || persisted.DraftRevision != 2 {
		t.Fatalf("幂等 configure 不得重复增加 revision: %+v", persisted)
	}
	reconfiguredRaw, err := group.configureSimpleDraft(context.Background(), map[string]any{"execution_class": "read_only"})
	if err != nil {
		t.Fatalf("被 Proposal Acceptance 拒绝后应能用高层契约修正 execution_class: %v", err)
	}
	var reconfigured graph.GraphDraft
	if err := json.Unmarshal([]byte(reconfiguredRaw), &reconfigured); err != nil ||
		reconfigured.DraftRevision != 3 || reconfigured.Contract.ExecutionClass != graph.ExecutionReadOnly {
		t.Fatalf("simple graph reconfigure 未形成新 revision: err=%v draft=%+v", err, reconfigured)
	}
	if _, err := group.configureSimpleDraft(context.Background(), map[string]any{"execution_class": "mutating"}); err != nil {
		t.Fatalf("恢复 mutating simple graph: %v", err)
	}
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := (graph.DefinitionCompiler{Policies: catalog, Acceptance: simpleGraphAcceptancePass{}}).Compile(
		context.Background(), graph.DefinitionCompileRequest{
			ReportID: "simple-graph-report", Draft: draft, DefinitionRevision: 1,
		})
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.Report.Accepted || len(compiled.Report.Errors) != 0 {
		t.Fatalf("framework-owned simple graph 应直接通过确定性 compiler: %+v", compiled.Report.Errors)
	}
	group.Compiler = graph.DefinitionCompiler{Policies: catalog, Acceptance: simpleGraphAcceptancePass{}}
	group.RouteValidator = fakeRouteValidator{routes: map[string][]string{
		"": {
			"read_file", "list_dir", "grep_search", "glob_search", "read_content_ref",
			"write_file", "edit_file", "run_shell", "submit_task_result",
		},
		graph.RouteAcceptance: {
			"read_file", "list_dir", "grep_search", "glob_search", "read_content_ref", "submit_task_result",
		},
	}}
	validatedRaw, err := group.validateCurrentDraft(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var report graph.ValidationReport
	if err := json.Unmarshal([]byte(validatedRaw), &report); err != nil || !report.Accepted {
		t.Fatalf("current transaction validate 未通过: err=%v report=%+v", err, report)
	}
	committedRaw, err := group.commitCurrentDraft(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var receipt graphDefinitionReceipt
	if err := json.Unmarshal([]byte(committedRaw), &receipt); err != nil || receipt.Revision != 1 || receipt.GraphID != draft.GraphID {
		t.Fatalf("current transaction commit receipt 错误: err=%v receipt=%+v", err, receipt)
	}
}

func TestGraphAuthoringCreateAndPatchUseNativeArguments(t *testing.T) {
	group, authoring := newGraphAuthoringToolEnv(t)
	createArgs := map[string]any{}
	createdRaw, err := group.createDraft(context.Background(), createArgs)
	if err != nil {
		t.Fatalf("createDraft: %v", err)
	}
	var created graph.GraphDraft
	if err := json.Unmarshal([]byte(createdRaw), &created); err != nil {
		t.Fatal(err)
	}
	if created.DraftRevision != 1 || created.RequestDigest == "" || created.Contract.RequestDigest != created.RequestDigest ||
		created.Contract.ExecutionClass != "" || len(created.Candidate.Nodes) != 0 ||
		created.ProposalID != "graph-proposal-scheduler-root" || created.GraphID != "graph-scheduler-root" {
		t.Fatalf("create 未由框架绑定 request: %+v", created)
	}

	patchArgs := map[string]any{
		"proposal_id": created.ProposalID, "base_draft_revision": float64(1), "root": "work",
		"contract": map[string]any{
			"execution_class":  "mutating",
			"deliverables":     []any{map[string]any{"id": "source", "kind": "artifact"}},
			"required_effects": []any{"file_write"},
		},
		"upsert_nodes": []any{
			map[string]any{
				"id": "work", "kind": "agent",
				"task": map[string]any{"title": "实施", "description": "实施并提交结果"},
				"next": []any{
					map[string]any{"to": "done", "when": map[string]any{"event": "completed"}},
					map[string]any{"to": "failed", "when": map[string]any{"event": "failed"}},
					map[string]any{"to": "blocked", "when": map[string]any{"event": "blocked"}},
				},
				"output_contract":       map[string]any{"summary_required": true},
				"progress_contract_ref": "progress:code-change/v1", "context_policy_ref": "context:default/v1",
				"contract_bindings": map[string]any{"deliverables": []any{"source"}, "effects": []any{"file_write"}},
			},
			map[string]any{"id": "done", "kind": "end", "end_outcome": "success", "next": []any{}},
			map[string]any{"id": "failed", "kind": "end", "end_outcome": "failed", "next": []any{}},
			map[string]any{"id": "blocked", "kind": "end", "end_outcome": "blocked", "next": []any{}},
		},
	}
	patchedRaw, err := group.patchDraft(context.Background(), patchArgs)
	if err != nil {
		t.Fatalf("patchDraft: %v", err)
	}
	var patched graph.GraphDraft
	if err := json.Unmarshal([]byte(patchedRaw), &patched); err != nil {
		t.Fatal(err)
	}
	if patched.DraftRevision != 2 || patched.Candidate.Root != "work" || len(patched.Candidate.Nodes) != 4 {
		t.Fatalf("native patch 未生效: %+v", patched)
	}
	stored, _ := authoring.GetDraft(created.ProposalID)
	if stored.DraftRevision != 2 {
		t.Fatalf("Store revision=%d", stored.DraftRevision)
	}

	if _, err := group.createDraft(context.Background(), map[string]any{"graph": `{"root":"done"}`}); err == nil {
		t.Fatal("完整 Graph JSON string 参数必须被 strict native decoder 拒绝")
	}
}

func TestGraphAuthoringCreateIsIdempotentAndRejectsArguments(t *testing.T) {
	group, _ := newGraphAuthoringToolEnv(t)
	raw, err := group.createDraft(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var draft graph.GraphDraft
	if err := json.Unmarshal([]byte(raw), &draft); err != nil {
		t.Fatal(err)
	}
	repeated, err := group.createDraft(context.Background(), map[string]any{})
	if err != nil || repeated != raw {
		t.Fatalf("deterministic create 重试不幂等: err=%v\nfirst=%s\nsecond=%s", err, raw, repeated)
	}
	if _, err := group.createDraft(context.Background(), map[string]any{"graph_id": "forbidden"}); err == nil {
		t.Fatal("零参数 create 不得接受模型自造 identity")
	}
}

func TestInterventionAuthoringBindsOriginalRequestWithoutTransferringDraftOwnership(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 8), 32, 1, 60)
	now := time.Now().UTC()
	run := &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: "run-intervention-authoring", CreatedAt: now,
		DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
		RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
	}
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	progress, ok := catalog.ProgressContract(policycatalog.ProgressCoordinationV1)
	if !ok {
		t.Fatal("缺少 coordination ProgressContract")
	}
	source := &model.Task{
		ID: "source-root", EventType: "__scheduler__", EventSource: "user",
		Description: "原始用户请求", RunID: run.RunID, RunContract: run,
		ContextPolicyRef: policycatalog.ContextDefaultCurrent, ProgressContract: &progress.Contract,
	}
	if err := tasks.PublishTask(source); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("scheduler", source.ID); err != nil {
		t.Fatal(err)
	}
	if err := tasks.BlockProcessingTaskBySystem(source.ID, "需要介入", "loop_intervention_required"); err != nil {
		t.Fatal(err)
	}
	wake := &model.Task{
		ID: "wake-root", EventType: "__scheduler__", EventSource: model.TaskEventSourceLoopIntervention,
		ParentTaskID: source.ID, Description: "介入恢复", RunID: run.RunID, RunContract: run,
		ContextPolicyRef: policycatalog.ContextDefaultCurrent, ProgressContract: &progress.Contract,
		RunPhase: runcontract.PhaseRecovery,
	}
	if err := tasks.PublishTask(wake); err != nil {
		t.Fatal(err)
	}
	authoring, err := graph.NewAuthoringStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authoring.Close() })
	group := GraphAuthoringGroup{
		Store: authoring, TaskStore: tasks, Holder: &fakeHolder{id: wake.ID},
		SessionID: func() string { return "session-1" },
	}
	raw, err := group.createDraft(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var draft graph.GraphDraft
	if err := json.Unmarshal([]byte(raw), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.OwnerTaskID != wake.ID || draft.RequestRef != source.ID ||
		draft.Contract.RequestRef != source.ID || draft.RequestDigest != schedulerRequestDigest(source) {
		t.Fatalf("intervention authoring provenance/ownership 错误: %+v", draft)
	}
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
