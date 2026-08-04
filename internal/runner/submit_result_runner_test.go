package runner

import (
	"context"
	"os"
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
// LastResponse=权威结果文本）。
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
		AllowedTools: []string{"submit_task_result"}, TaskMaxRetries: 1,
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
	groups := resolveToolGroups("w-1", nil, RunnerDeps{}, &CurrentTaskHolder{},
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

// 装配验证（finalizing fence，V6 §5）：runner.New 把 finHolder 接入 executor
// 的 fence——同一响应中排在 submit_task_result 之前的 write_file 正常落盘，
// 排在其后的 write_file 被跳过（磁盘无产物、收到「已跳过」提示），任务仍以
// completed 收尾。该测试防「装配漏接」：fence 逻辑单测全绿但 runner 未接线
// 时，after.txt 会被真实写出。
func TestRunnerFinalizingFenceSkipsTrailingToolCalls(t *testing.T) {
	root := t.TempDir()
	taskStore := store.NewMemoryTaskStore(nil, 32, 1, 60)
	task := &model.Task{Description: "fence e2e", EventType: "code"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatal(err)
	}

	client := &orderedToolClient{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{
			{ID: "w1", Name: "write_file", Arguments: map[string]any{"path": "before.txt", "content": "提交前写入"}},
			{ID: "s1", Name: "submit_task_result", Arguments: map[string]any{"summary": "已完成 before.txt"}},
			{ID: "w2", Name: "write_file", Arguments: map[string]any{"path": "after.txt", "content": "提交后不应写入"}},
		},
		FinishReason: llm.FinishReasonToolCalls,
	}}}
	rn := New(config.AgentRuntimeConfig{
		InstanceID: "worker-fence", Kind: "worker", EventType: "code",
		AllowedTools: []string{"write_file", "submit_task_result"}, TaskMaxRetries: 1,
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
		if getErr == nil && model.IsTerminal(got.Status) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not reach terminal: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := taskStore.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusCompleted {
		t.Fatalf("任务应以 completed 收尾，实际 %s（error: %s）", got.Status, got.Error)
	}
	// 提交前的 write_file 真实落盘。
	if data, err := os.ReadFile(filepath.Join(root, "before.txt")); err != nil || string(data) != "提交前写入" {
		t.Errorf("submit 之前的 write_file 应落盘: data=%q err=%v", data, err)
	}
	// 提交后的 write_file 被 fence 跳过：磁盘无产物。
	if _, err := os.Stat(filepath.Join(root, "after.txt")); !os.IsNotExist(err) {
		t.Errorf("submit 之后的 write_file 应被 fence 跳过（after.txt 不应存在），stat err=%v", err)
	}
}
