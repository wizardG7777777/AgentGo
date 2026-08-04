package agent

// prompt_build_test.go 覆盖 V6 §2（P1a）Prompt 有序编译的 agent 侧接入：
//   - Build.Text 与 buildMessages 的 system+user 首条逐字节一致（防回归：
//     编译产物只用于身份与观测，不改变任何消息字节）；
//   - 组件含当时工具清单（来自冻结租约/注册全集，非手写文本）；
//   - processTask 每个 attempt 恰好一条 prompt_compiled，重试复用同
//     Build.ID；context_manifest_built 事件并入同一 prompt_build_id；
//   - task.SystemPrompt 覆盖产生不同 Build.ID。

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/prompt"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// newPromptBuildAgent 构造带静态 prompt 身份与可换入 executor 的测试 Agent。
func newPromptBuildAgent(agentID, eventType string, s store.TaskStore, teamAwareness, sysPrompt string, toolNames ...string) (*Agent, *LLMExecutor, *capMockClient) {
	mock := &capMockClient{}
	exec := NewSwappableLLMExecutor(mock, newLeaseToolRegistry(toolNames...), nil, nil, nil, teamAwareness, sysPrompt)
	exec.SetPromptVersion("file:abc123def456")
	ag := NewAgent(agentID, eventType, s, nil, exec.Execute)
	ag.ToolSwapper = exec
	ag.PromptSource = exec
	return ag, exec, mock
}

// TestCompilePromptBuild_ByteIdenticalWithBuildMessages 钉住核心防回归不变量：
// Build.Text 与 buildMessages 的 system+user 首条逐字节一致（含
// teamAwareness、<task-context> 块与任务描述；depResults 为空——依赖结果段
// 与 buildMessages 同为 map 序，单元素以下才谈逐字节稳定）。
func TestCompilePromptBuild_ByteIdenticalWithBuildMessages(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newPromptBuildAgent("agent-pb", "code", s, "团队感知文本", "系统提示全文",
		"read_file", "write_file", "submit_task_result")

	task := &model.Task{ID: "t-pb-1", Description: "任务描述正文", EventType: "code"}
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" {
		t.Fatalf("租约计算被拒绝: %s", rejection)
	}
	build := ag.compilePromptBuild(task, nil, lease)

	msgs := buildMessages("系统提示全文", task, nil, nil, "团队感知文本")
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("buildMessages 形态不符预期: %+v", msgs)
	}
	want := msgs[0].Content + msgs[1].Content
	if build.Text != want {
		t.Fatalf("Build.Text 应与 buildMessages 的 system+user 首条逐字节一致\n got: %q\nwant: %q", build.Text, want)
	}

	// 组件序列：agent_role → base_contract → control_protocol →
	// task_objective → tool_guidance → output_contract。
	ids := make([]string, 0, len(build.Components))
	for _, c := range build.Components {
		ids = append(ids, c.ID)
	}
	wantIDs := []string{
		prompt.ComponentAgentRole, prompt.ComponentBaseContract,
		prompt.ComponentControlProtocol, prompt.ComponentTaskObjective,
		prompt.ComponentToolGuidance, prompt.ComponentOutputContract,
	}
	if strings.Join(ids, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("组件序列 = %v，want %v", ids, wantIDs)
	}
	// agent_role 身份：全文 + 文件版本。
	if build.Components[0].Text != "系统提示全文" || build.Components[0].Version != "file:abc123def456" {
		t.Fatalf("agent_role 组件身份不符: %+v", build.Components[0])
	}
}

// TestCompilePromptBuild_ToolGuidanceFromLease 验证 tool_guidance 组件载当时
// 真实工具清单：合成租约取 ToolUnion（ceiling ∪ 控制通道），显式声明租约
// 取声明子集 ∪ 控制通道；Version 锚定租约 digest。
func TestCompilePromptBuild_ToolGuidanceFromLease(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newPromptBuildAgent("agent-pb", "code", s, "", "系统提示",
		"read_file", "write_file", "run_shell")

	// 合成租约：ceiling 全量 ∪ submit_task_result。
	task := &model.Task{ID: "t-pb-tools", Description: "x", EventType: "code"}
	lease, _ := ag.computeExecutionLease(task)
	build := ag.compilePromptBuild(task, nil, lease)
	tg := findComponent(t, build, prompt.ComponentToolGuidance)
	want := "read_file,run_shell,submit_task_result,write_file"
	if tg.Text != want {
		t.Fatalf("合成租约 tool_guidance = %q，want %q", tg.Text, want)
	}
	if tg.Version != "lease:"+lease.Digest {
		t.Fatalf("tool_guidance Version = %q，want lease:%s", tg.Version, lease.Digest)
	}
	if tg.InMessage {
		t.Fatal("tool_guidance 是带外身份组件，不应进入消息字节")
	}

	// 显式声明租约：声明子集 ∪ 控制通道。
	explicit := &model.Task{ID: "t-pb-exp", Description: "x", EventType: "code",
		Capability: &model.NodeCapability{Tools: []string{"read_file"}}}
	expLease, rejection := ag.computeExecutionLease(explicit)
	if rejection != "" {
		t.Fatalf("显式声明租约被拒绝: %s", rejection)
	}
	expBuild := ag.compilePromptBuild(explicit, nil, expLease)
	expTg := findComponent(t, expBuild, prompt.ComponentToolGuidance)
	if expTg.Text != "read_file,submit_task_result" {
		t.Fatalf("显式租约 tool_guidance = %q，want read_file,submit_task_result", expTg.Text)
	}
	// 工具面不同 → Build.ID 不同。
	if expBuild.ID == build.ID {
		t.Fatal("工具面不同应产生不同 Build.ID")
	}
}

func findComponent(t *testing.T, build prompt.Build, id string) prompt.Component {
	t.Helper()
	for _, c := range build.Components {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("组件 %s 不存在于 Build %+v", id, build.Components)
	return prompt.Component{}
}

// TestCompilePromptBuild_StableAndOverride 验证：同输入重编译 Build.ID 稳定
//（重试复用的基础）；task.SystemPrompt 覆盖时是另一个 Build。
func TestCompilePromptBuild_StableAndOverride(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newPromptBuildAgent("agent-pb", "code", s, "", "系统提示", "read_file")

	task := &model.Task{ID: "t-pb-stable", Description: "x", EventType: "code"}
	lease, _ := ag.computeExecutionLease(task)
	b1 := ag.compilePromptBuild(task, nil, lease)
	b2 := ag.compilePromptBuild(task, nil, lease)
	if b1.ID != b2.ID {
		t.Fatalf("同输入重编译 Build.ID 应稳定：%q vs %q", b1.ID, b2.ID)
	}

	overridden := &model.Task{ID: "t-pb-stable", Description: "x", EventType: "code",
		SystemPrompt: "任务级 system prompt 覆盖"}
	b3 := ag.compilePromptBuild(overridden, nil, lease)
	if b3.ID == b1.ID {
		t.Fatal("task.SystemPrompt 覆盖应产生不同 Build.ID")
	}
	role := findComponent(t, b3, prompt.ComponentAgentRole)
	if role.Version != promptVersionTaskOverride || role.Text != "任务级 system prompt 覆盖" {
		t.Fatalf("覆盖路径 agent_role 身份不符: %+v", role)
	}
	// 覆盖路径的 Build.Text 仍与 buildMessages 一致。
	msgs := buildMessages("任务级 system prompt 覆盖", overridden, nil, nil, "")
	if b3.Text != msgs[0].Content+msgs[1].Content {
		t.Fatal("覆盖路径 Build.Text 应与 buildMessages 逐字节一致")
	}
}

// TestProcessTask_PromptCompiledOnceAndBound 验证接入面：processTask 每个
// attempt 恰好 emit 一条 prompt_compiled（组件含实际工具清单），且每轮
// context_manifest_built 事件并入同一 prompt_build_id。
func TestProcessTask_PromptCompiledOnceAndBound(t *testing.T) {
	dir := captureTraceToDir(t)
	s, _, _ := setup()
	ag, _, mock := newPromptBuildAgent("agent-pb", "code", s, "", "系统提示全文",
		"read_file", "write_file")

	task := &model.Task{Description: "编译冻结任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	ag.processTask(context.Background(), task.ID)
	if mock.calls != 1 {
		t.Fatalf("LLM 调用次数 = %d，want 1", mock.calls)
	}

	compiled := leaseEventsFromDir(t, dir, trace.KindPromptCompiled)
	if len(compiled) != 1 {
		t.Fatalf("prompt_compiled 事件数 = %d，want 1", len(compiled))
	}
	ev := compiled[0]
	if ev.PromptBuildID == "" || !strings.HasPrefix(ev.PromptBuildID, "pb-") {
		t.Fatalf("prompt_compiled 缺少 pb- 前缀的 Build.ID: %+v", ev)
	}
	// 事件只载身份摘要：组件清单含 tool_guidance 且不含正文。
	for _, want := range []string{`"id":"agent_role"`, `"id":"tool_guidance"`, `"id":"output_contract"`} {
		if !strings.Contains(ev.Description, want) {
			t.Fatalf("prompt_compiled 摘要缺少 %s: %s", want, ev.Description)
		}
	}
	if strings.Contains(ev.Description, "系统提示全文") {
		t.Fatal("prompt_compiled 摘要不得包含 prompt 正文")
	}

	// 每轮 context_manifest_built 并入同一 prompt_build_id（prompt_bound
	// 不独立成事件——manifest 每轮恰好一条，避免同频双账本）。
	manifests := leaseEventsFromDir(t, dir, trace.KindContextManifestBuilt)
	if len(manifests) != 1 {
		t.Fatalf("context_manifest_built 事件数 = %d，want 1", len(manifests))
	}
	if manifests[0].PromptBuildID != ev.PromptBuildID {
		t.Fatalf("manifest prompt_build_id = %q，want 与 prompt_compiled 一致 %q",
			manifests[0].PromptBuildID, ev.PromptBuildID)
	}
}

// TestProcessTask_PromptBuildRetryReusesSameID 验证重试新 attempt 重新编译
// 但 Build.ID 稳定（输入不变）：两次 attempt 各一条 prompt_compiled，ID 相同。
func TestProcessTask_PromptBuildRetryReusesSameID(t *testing.T) {
	dir := captureTraceToDir(t)
	s, _, _ := setup()
	ag, _, _ := newPromptBuildAgent("agent-pb", "code", s, "", "系统提示全文", "read_file")

	task := &model.Task{Description: "重试编译任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// 第一次 attempt：可恢复错误 → 重试回滚。
	ag.Execute = func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{}, &ErrRecoverable{Err: context.DeadlineExceeded}
	}
	ag.processTask(context.Background(), task.ID)

	// 第二次 attempt：正常完成。
	if err := s.ClaimTask(ag.ID, task.ID); err != nil {
		t.Fatalf("重认领: %v", err)
	}
	ag.Execute = func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		return ExecuteResult{Output: "done"}, nil
	}
	ag.processTask(context.Background(), task.ID)

	compiled := leaseEventsFromDir(t, dir, trace.KindPromptCompiled)
	if len(compiled) != 2 {
		t.Fatalf("两个 attempt 应各有一条 prompt_compiled，实际 %d", len(compiled))
	}
	if compiled[0].PromptBuildID != compiled[1].PromptBuildID {
		t.Fatalf("重试应复用同一 Build.ID：%q vs %q",
			compiled[0].PromptBuildID, compiled[1].PromptBuildID)
	}
}

// TestProcessTask_PromptBuildSchedulerControlPlane 验证 scheduler 控制面路径：
// 带 swapper（观测用）的 __scheduler__ 任务仍保持记录型租约，tool_guidance
// 回退到注册全集。
func TestProcessTask_PromptBuildSchedulerControlPlane(t *testing.T) {
	s, _, _ := setup()
	ag, _, _ := newPromptBuildAgent("scheduler", "__scheduler__", s, "", "调度器提示",
		"read_file", "publish_task", "report_done")

	task := &model.Task{ID: "t-pb-sched", Description: "x", EventType: "__scheduler__"}
	lease, rejection := ag.computeExecutionLease(task)
	if rejection != "" {
		t.Fatalf("控制面租约不应被拒绝: %s", rejection)
	}
	if !lease.Synthetic || lease.BusinessTools != nil {
		t.Fatalf("控制面租约应保持记录型（Synthetic 且无裁剪面）: %+v", lease)
	}
	build := ag.compilePromptBuild(task, nil, lease)
	tg := findComponent(t, build, prompt.ComponentToolGuidance)
	if tg.Text != "publish_task,read_file,report_done" {
		t.Fatalf("控制面 tool_guidance 应为注册全集，got %q", tg.Text)
	}
	oc := findComponent(t, build, prompt.ComponentOutputContract)
	if !strings.Contains(oc.Text, "report_done") {
		t.Fatalf("控制面 output_contract 应描述 report_done 协议，got %q", oc.Text)
	}
}
