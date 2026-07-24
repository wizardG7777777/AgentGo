package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/tools"
)

// 装配验证：allowlist 含 submit_task_result 的 runner 能真实调通该工具——
// LLM 一轮调用后任务以结构化渲染文本收尾（SubmitResult=Format 输出，
// TransferNote=summary，LastResponse=权威结果文本）。
func TestRunnerSubmitTaskResultShortCircuitsWithStructuredPayload(t *testing.T) {
	root := t.TempDir()
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := &model.Task{Description: "write report", EventType: "code"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	client := &orderedToolClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID: "submit-1", Name: "submit_task_result", Arguments: map[string]any{
				"summary":          "报告已写入 report.md",
				"checks_performed": "go build, go test ./...",
				"evidence":         "report.md",
				"remaining_risks":  "覆盖率未测",
			},
		}},
		FinishReason: llm.FinishReasonToolCalls,
	}}}
	rn := New(config.AgentRuntimeConfig{
		InstanceID: "worker-submit", Kind: "worker", EventType: "code",
		AllowedTools: []string{"submit_task_result"}, AgentMaxLoops: 3, TaskMaxRetries: 1,
	}, RunnerDeps{
		Store: taskStore, Roster: roster.NewMemoryRoster(), LLMClient: client, ProjectRoot: root,
	})
	rn.Agent().TextOnlyReportsDir = filepath.Join(root, "reports")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rn.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("runner did not stop")
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, getErr := taskStore.GetTask(task.ID)
		if getErr == nil && got.Status == model.TaskStatusCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not complete via submit_task_result: task=%+v err=%v", got, getErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := got.Results["worker-submit"]
	for _, want := range []string{"## 任务结果摘要", "报告已写入 report.md", "- go build", "- report.md", "- 覆盖率未测"} {
		if !strings.Contains(result, want) {
			t.Errorf("SubmitResult 负载缺少 %q，实际：\n%s", want, result)
		}
	}
	if got.TransferNote != "报告已写入 report.md" {
		t.Errorf("TransferNote = %q，期望 summary 原文", got.TransferNote)
	}
	if got.LastResponse != result {
		t.Errorf("LastResponse 应等于结构化渲染文本")
	}
	if client.callCount() != 1 {
		t.Errorf("LLM calls = %d，期望 1（工具调用后下一轮 loop 顶部短路，不再调 LLM）", client.callCount())
	}
}

// 装配验证：resolveToolGroups 必须把提交通道注入 PlanControlGroup，
// 否则 submit_task_result 根本不会被注册（nil 任一字段即跳过注册）。
func TestResolveToolGroups_WiresSubmitChannelToPlanControlGroup(t *testing.T) {
	submitState := agent.NewSubmitState()
	groups := resolveToolGroups("w-1", RunnerDeps{}, &CurrentTaskHolder{},
		agent.NewFinalizationHolder(), submitState, agent.NewFileStateCache(1), &tools.DefaultWorkdir{}, nil)

	var planGroup *tools.PlanControlGroup
	for i := range groups {
		if pg, ok := groups[i].(tools.PlanControlGroup); ok {
			planGroup = &pg
		}
	}
	if planGroup == nil {
		t.Fatal("resolveToolGroups 应包含 PlanControlGroup")
	}
	if planGroup.FinalizationNotifier == nil {
		t.Fatal("PlanControlGroup.FinalizationNotifier 未接线")
	}
	if planGroup.SubmitState == nil || planGroup.SubmitState != submitState {
		t.Fatal("PlanControlGroup.SubmitState 未接线")
	}
}
