package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

type recordingTemplateProvisioner struct {
	calls int
	last  agenttemplate.ProvisionRequest
}

func (p *recordingTemplateProvisioner) Provision(_ context.Context, req agenttemplate.ProvisionRequest) (agenttemplate.ProvisionResult, error) {
	p.calls++
	p.last = req
	return agenttemplate.ProvisionResult{
		TeamID: "team-1", EventType: "team:team-1",
		TemplateRef: req.TemplateRef, TemplateDigest: "sha256:test",
		AgentIDs: []string{"generalist-team-1-1"}, Tools: []string{"read_file", "write_file"},
		Replicas: req.Replicas,
	}, nil
}

// newProcessingController 发布一个 __scheduler__ 任务并认领为 processing，
// 满足 provision_agent_team 的「正在执行的 Scheduler 任务」准入。
func newProcessingController(t *testing.T, taskStore store.TaskStore) *model.Task {
	t.Helper()
	controller := &model.Task{Description: "controller", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("scheduler", controller.ID); err != nil {
		t.Fatal(err)
	}
	return controller
}

func TestAgentTemplateGroupListsAndProvisionsWithoutCreatingDAGTask(t *testing.T) {
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model", ValidateTools: ValidateToolNames,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	controller := newProcessingController(t, taskStore)
	beforeTasks, err := taskStore.ScanAll()
	if err != nil {
		t.Fatal(err)
	}

	provisioner := &recordingTemplateProvisioner{}
	group := AgentTemplateGroup{
		Catalog: catalog, Provisioner: provisioner,
		Store: taskStore, Holder: &fakeHolder{id: controller.ID},
	}
	listed, err := group.list(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var summaries []agenttemplate.Summary
	if err := json.Unmarshal([]byte(listed), &summaries); err != nil {
		t.Fatalf("list output is not valid JSON: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("built-in template count=%d, want 3: %s", len(summaries), listed)
	}
	if provisioner.calls != 0 {
		t.Fatal("list_agent_templates provisioned a runtime Team")
	}

	out, err := group.provision(context.Background(), map[string]any{
		"template_ref": "builtin/generalist@1",
		"purpose":      "implementation",
		"replicas":     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provisioner.calls != 1 || provisioner.last.ControllerTaskID != controller.ID ||
		provisioner.last.TemplateRef != "builtin/generalist@1" ||
		provisioner.last.Purpose != "implementation" || provisioner.last.Replicas != 1 {
		t.Fatalf("provision authority was not injected correctly: calls=%d request=%+v", provisioner.calls, provisioner.last)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("provision output is not valid JSON: %v", err)
	}
	if result["team_id"] != "team-1" || result["event_type"] != "team:team-1" {
		t.Fatalf("provision output lost stable snake_case identities: %s", out)
	}
	if _, exists := result["EventType"]; exists {
		t.Fatalf("provision output leaked Go field names: %s", out)
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("provision output must expose runtime tools, got %s", out)
	}

	afterTasks, err := taskStore.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTasks) != len(beforeTasks) {
		t.Fatalf("provision created a DAG Task: before=%d after=%d", len(beforeTasks), len(afterTasks))
	}
}

// provision 准入（C6b）：当前任务必须是 processing 的 __scheduler__ 任务——
// pending 的 scheduler 任务与非 scheduler 任务都不得触达 runtime provisioner。
func TestAgentTemplateGroupProvisionRequiresRunningSchedulerTask(t *testing.T) {
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model", ValidateTools: ValidateToolNames,
	})
	if err != nil {
		t.Fatal(err)
	}

	// pending 的 __scheduler__ 任务（未认领执行）。
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	pending := &model.Task{Description: "pending controller", EventType: "__scheduler__"}
	if err := taskStore.PublishTask(pending); err != nil {
		t.Fatal(err)
	}
	provisioner := &recordingTemplateProvisioner{}
	group := AgentTemplateGroup{
		Catalog: catalog, Provisioner: provisioner,
		Store: taskStore, Holder: &fakeHolder{id: pending.ID},
	}
	_, err = group.provision(context.Background(), map[string]any{
		"template_ref": "builtin/explorer@1", "purpose": "investigation",
	})
	if err == nil || !strings.Contains(err.Error(), "requires a running Scheduler task") {
		t.Fatalf("pending scheduler task provision err=%v", err)
	}

	// processing 但非 __scheduler__ 的普通任务。
	worker := &model.Task{Description: "worker", EventType: "code"}
	if err := taskStore.PublishTask(worker); err != nil {
		t.Fatal(err)
	}
	if err := taskStore.ClaimTask("worker-1", worker.ID); err != nil {
		t.Fatal(err)
	}
	group.Holder = &fakeHolder{id: worker.ID}
	_, err = group.provision(context.Background(), map[string]any{
		"template_ref": "builtin/explorer@1", "purpose": "investigation",
	})
	if err == nil || !strings.Contains(err.Error(), "requires a running Scheduler task") {
		t.Fatalf("non-scheduler task provision err=%v", err)
	}
	if provisioner.calls != 0 {
		t.Fatal("非运行中 Scheduler 任务触达了 runtime provisioner")
	}
}

func TestAgentTemplateGroupRejectsMalformedReplicaCount(t *testing.T) {
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model", ValidateTools: ValidateToolNames,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	controller := newProcessingController(t, taskStore)
	provisioner := &recordingTemplateProvisioner{}
	group := AgentTemplateGroup{
		Catalog: catalog, Provisioner: provisioner,
		Store: taskStore, Holder: &fakeHolder{id: controller.ID},
	}
	for _, replicas := range []any{"2", 1.5, 0, 33} {
		if _, err := group.provision(context.Background(), map[string]any{
			"template_ref": "builtin/generalist@1", "purpose": "implementation", "replicas": replicas,
		}); err == nil || !strings.Contains(err.Error(), "replicas") {
			t.Fatalf("replicas=%v should fail strict integer validation, got %v", replicas, err)
		}
	}
	if provisioner.calls != 0 {
		t.Fatal("malformed replica count reached runtime provisioner")
	}
}
