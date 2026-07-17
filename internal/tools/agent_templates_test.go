package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/agenttemplate"
	"agentgo/internal/model"
	"agentgo/internal/plan"
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

func TestAgentTemplateGroupListsAndProvisionsWithoutCreatingDAGTask(t *testing.T) {
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model", ValidateTools: ValidateToolNames,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := &model.Task{
		Description: "controller", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController,
	}
	if err := taskStore.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	controller.PlanID = controller.ID
	p, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: controller.ID, RootTaskID: controller.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeTasks, err := taskStore.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision := p.CurrentRevision

	provisioner := &recordingTemplateProvisioner{}
	group := AgentTemplateGroup{
		Catalog: catalog, Provisioner: provisioner, Coordinator: coordinator,
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
	if provisioner.calls != 1 || provisioner.last.PlanID != p.ID ||
		provisioner.last.ControllerTaskID != controller.ID {
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
	afterPlan, err := coordinator.Store().GetPlan(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterPlan.CurrentRevision != beforeRevision {
		t.Fatalf("provision changed PlanRevision: before=%d after=%d", beforeRevision, afterPlan.CurrentRevision)
	}
}

func TestAgentTemplateGroupRejectsSupersededController(t *testing.T) {
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model", ValidateTools: ValidateToolNames,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	oldController := &model.Task{
		Description: "old controller", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController,
	}
	if err := taskStore.PublishTask(oldController); err != nil {
		t.Fatal(err)
	}
	oldController.PlanID = oldController.ID
	p, err := coordinator.Create(context.Background(), plan.CreateInput{
		PlanID: oldController.ID, RootTaskID: oldController.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	newController := &model.Task{
		PlanID: p.ID, Description: "new controller", EventType: "__scheduler__",
		NodeRole: model.PlanNodeRoleController,
	}
	if err := taskStore.PublishTask(newController); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ActivateController(context.Background(), p.ID, newController.ID); err != nil {
		t.Fatal(err)
	}

	provisioner := &recordingTemplateProvisioner{}
	group := AgentTemplateGroup{
		Catalog: catalog, Provisioner: provisioner, Coordinator: coordinator,
		Store: taskStore, Holder: &fakeHolder{id: oldController.ID},
	}
	_, err = group.provision(context.Background(), map[string]any{
		"template_ref": "builtin/explorer@1", "purpose": "investigation",
	})
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("superseded controller provision err=%v", err)
	}
	if provisioner.calls != 0 {
		t.Fatal("superseded controller reached runtime provisioner")
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
	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	controller := &model.Task{Description: "controller", EventType: "__scheduler__", NodeRole: model.PlanNodeRoleController}
	if err := taskStore.PublishTask(controller); err != nil {
		t.Fatal(err)
	}
	controller.PlanID = controller.ID
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: controller.ID, RootTaskID: controller.ID}); err != nil {
		t.Fatal(err)
	}
	provisioner := &recordingTemplateProvisioner{}
	group := AgentTemplateGroup{
		Catalog: catalog, Provisioner: provisioner, Coordinator: coordinator,
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
