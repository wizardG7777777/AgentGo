package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/plan"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/tools"
)

// newSoloTestBundle 构造一个 scheduler Bundle 用于 solo 单测：
// modeStore 由调用方给定（nil 时走 New 内部的 DefaultStore 回落，等价 team）。
// 返回的 scheduler task 已发布并被认领，holder 已通过 OnTaskStart 设置，
// 使 publish_task / report_done 等依赖当前任务上下文的工具可直接执行。
func newSoloTestBundle(t *testing.T, modeStore *modes.Store, mockLLM llm.Client) (*Bundle, *store.MemoryTaskStore, *model.Task) {
	t.Helper()
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}

	bundle := New(s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, modeStore)

	task := &model.Task{Description: "solo 测试任务", EventType: "__scheduler__"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("发布 scheduler 任务失败: %v", err)
	}
	if err := s.ClaimTask(bundle.Agent.ID, task.ID); err != nil {
		t.Fatalf("认领 scheduler 任务失败: %v", err)
	}
	bundle.Agent.OnTaskStart(task.ID)
	t.Cleanup(func() { bundle.Agent.OnTaskEnd(task.ID, true) })
	return bundle, s, task
}

// executeOneRound 用脚本化 LLM 跑一轮 Execute，返回唯一工具调用的 result content。
func executeOneRound(t *testing.T, bundle *Bundle, task *model.Task) string {
	t.Helper()
	result, err := bundle.SchedulerExec.Execute(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if len(result.ToolResults) != 1 {
		t.Fatalf("期望 1 个工具结果，实际 %d: %+v", len(result.ToolResults), result.ToolResults)
	}
	return result.ToolResults[0].Content
}

// TestSoloPublishTaskBlocked_Solo 验证 topo=solo 时 scheduler 的 publish_task
// 被硬拦截，且错误消息明确告知"solo 模式禁止派发子任务，请直接执行"。
func TestSoloPublishTaskBlocked_Solo(t *testing.T) {
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:   "call_1",
			Name: "publish_task",
			Arguments: map[string]any{
				"description": "应该被 solo 拦截的子任务",
			},
		}},
	}}}
	bundle, s, task := newSoloTestBundle(t, modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoSolo), mockLLM)

	content := executeOneRound(t, bundle, task)
	if !strings.Contains(content, "solo 编排模式禁止派发子任务") {
		t.Errorf("solo 下 publish_task 应被拦截并返回中文指引，实际: %s", content)
	}
	if !strings.Contains(content, "请直接使用") {
		t.Errorf("错误消息应给出直接执行的替代路径，实际: %s", content)
	}

	// 公告板上不应出现被拦截任务产生的子任务
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll 失败: %v", err)
	}
	for _, other := range tasks {
		if other.ID != task.ID {
			t.Errorf("solo 拦截后不应产生新任务，发现: id=%s desc=%s", other.ID, other.Description)
		}
	}
}

// TestSoloPublishTaskBlocked_TeamAllows 验证 topo=team 时 publish_task 正常放行。
func TestSoloPublishTaskBlocked_TeamAllows(t *testing.T) {
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:   "call_1",
			Name: "publish_task",
			Arguments: map[string]any{
				"description": "team 模式下允许派发的子任务",
			},
		}},
	}}}
	bundle, s, _ := newSoloTestBundle(t, modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoTeam), mockLLM)

	content := executeOneRound(t, bundle, mustTask(t, s))
	if !strings.Contains(content, "已创建任务") {
		t.Errorf("team 下 publish_task 应成功，实际: %s", content)
	}
	if strings.Contains(content, "solo 编排模式禁止") {
		t.Errorf("team 下不应出现 solo 拦截消息，实际: %s", content)
	}
}

func mustTask(t *testing.T, s *store.MemoryTaskStore) *model.Task {
	t.Helper()
	tasks, err := s.ScanAll()
	if err != nil || len(tasks) == 0 {
		t.Fatalf("读取 scheduler 任务失败: tasks=%d err=%v", len(tasks), err)
	}
	return tasks[0]
}

// TestSoloPublishTaskBlocked_NilStoreAllows 验证 modeStore 为 nil 时
// （New 内部回落 DefaultStore=team，包装器本身也 nil 安全）publish_task 不受拦截。
func TestSoloPublishTaskBlocked_NilStoreAllows(t *testing.T) {
	// 包装器自身 nil 安全：nil store 等价 team，直接透传 inner
	innerCalled := false
	inner := agent.ToolFunc(func(context.Context, map[string]any) (string, error) {
		innerCalled = true
		return "ok", nil
	})
	wrapped := wrapPublishTaskForSolo(nil)(inner)
	if _, err := wrapped(context.Background(), nil); err != nil || !innerCalled {
		t.Errorf("nil modeStore 应透传 inner: called=%v err=%v", innerCalled, err)
	}

	// Bundle 级：nil modeStore 走 DefaultStore 回落（team），publish_task 正常成功
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "publish_task",
			Arguments: map[string]any{"description": "nil store 下允许派发的子任务"},
		}},
	}}}
	bundle, s, _ := newSoloTestBundle(t, nil, mockLLM)
	content := executeOneRound(t, bundle, mustTask(t, s))
	if !strings.Contains(content, "已创建任务") {
		t.Errorf("nil modeStore（回落 team）下 publish_task 应成功，实际: %s", content)
	}
}

// TestSoloPublishTaskBlocked_RunnerUnaffected 验证拦截只作用于 scheduler 自己的
// ToolRegistry：按 runner 方式装配的 registry（不经 scheduler.New 的包装）即使在
// solo 模式存在的环境下，publish_task 也不受影响。
func TestSoloPublishTaskBlocked_RunnerUnaffected(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)

	parent := &model.Task{Description: "runner 父任务", EventType: ""}
	if err := s.PublishTask(parent); err != nil {
		t.Fatalf("发布父任务失败: %v", err)
	}
	holder := agent.NewFinalizationHolder()
	holder.Set(parent.ID)

	// runner 装配路径：MetaGroup 直接注册，没有任何 solo 包装
	reg := agent.NewToolRegistry()
	tools.RegisterGroups(reg, tools.MetaGroup{
		Store:   s,
		Holder:  holder,
		AgentID: "worker-1",
	})

	out, err := reg.Dispatch(context.Background(), llm.ToolCall{
		ID:        "call_1",
		Name:      "publish_task",
		Arguments: map[string]any{"description": "runner 派发的子任务"},
	})
	if err != nil {
		t.Fatalf("runner 的 publish_task 不应被 solo 拦截: %v", err)
	}
	if !strings.Contains(out, "已创建任务") {
		t.Errorf("runner 的 publish_task 应成功，实际: %s", out)
	}
}

// TestSoloPublishTaskBlocked_SendMessageAllowed 验证 solo 下 send_message 不被误伤。
func TestSoloPublishTaskBlocked_SendMessageAllowed(t *testing.T) {
	mockLLM := &scriptedLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{{
			ID:   "call_1",
			Name: "send_message",
			Arguments: map[string]any{
				"to":      "scheduler", // New 注册的稳定别名，保证路由存在
				"content": "solo 下的普通邮件",
			},
		}},
	}}}
	bundle, _, task := newSoloTestBundle(t, modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoSolo), mockLLM)

	content := executeOneRound(t, bundle, task)
	if strings.Contains(content, "solo 编排模式禁止") {
		t.Errorf("send_message 不应被 solo 拦截，实际: %s", content)
	}
	if strings.Contains(content, "错误") {
		t.Errorf("send_message 应成功送达 scheduler 别名，实际: %s", content)
	}
}

// TestSchedulerSystemPrompt_ContainsSoloGuidance 验证 system prompt 含 solo 指引。
//
// prompt 是构建期静态常量，solo/team 两种模式的指引都写在其中；运行期模式切换
// 的生效路径 = 每轮注入的 board 快照 topo_mode 字段（由 Modes 实时读取，
// 已有 TestSchedulerExecutor_ModesStoreLiveSwitch 覆盖）+ 本静态条件指引。
func TestSchedulerSystemPrompt_ContainsSoloGuidance(t *testing.T) {
	for _, want := range []string{
		"topo_mode（编排拓扑轴）",
		"solo",
		"唯一的执行者",
		"禁止调用 publish_task",
	} {
		if !strings.Contains(schedulerSystemPrompt, want) {
			t.Errorf("scheduler system prompt 缺少 solo 指引要素 %q", want)
		}
	}
}

// TestSchedulerBundle_SoloMode_DirectExecutionCompletes 是 solo 不卡死集成测试。
//
// 场景：topo=solo + 已装配 PlanCoordinator（与生产一致，scheduler 任务发布会
// 自动建立 root-only Plan）。LLM 全程不注册任何 DAG 节点，只调用普通工具
// （read_file）后 report_done 收尾。断言：
//   - scheduler 任务到达 completed（不会在 waitForPlanSignal 等处挂死——
//     root-only Plan 无节点，waitForPlanSignal 立即返回）；
//   - Plan 到达终态（只读路径走 CompleteWithoutExecution）；
//   - 公告板没有产生任何子任务。
func TestSchedulerBundle_SoloMode_DirectExecutionCompletes(t *testing.T) {
	// 独立项目根，read_file 读取其中的真实文件
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "note.txt"), []byte("solo 集成测试内容"), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	cfg.ProjectRoot = projectRoot
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}

	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoSolo)

	mockLLM := &scriptedLLM{responses: []llm.Response{
		// 第一轮：普通只读工具
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "read_file",
			Arguments: map[string]any{"path": "note.txt"},
		}}},
		// 第二轮：report_done 收尾
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_2",
			Name:      "report_done",
			Arguments: map[string]any{"summary": "已读取 note.txt 并总结"},
		}}},
		// 之后的回合（若有）返回纯文本
	}}

	bundle := New(s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, modeStore, coordinator)

	// 模拟生产发布路径：__scheduler__ 任务由 store 自动赋 PlanID + controller 角色，
	// 随后 plan hook 建立 root-only Plan（此处手动调 Create，与 bootstrap 的 hook 等价）。
	root := &model.Task{Description: "读取 note.txt 并总结", EventType: "__scheduler__"}
	if err := s.PublishTask(root); err != nil {
		t.Fatalf("发布 scheduler 任务失败: %v", err)
	}
	if root.PlanID != root.ID || root.NodeRole != model.PlanNodeRoleController {
		t.Fatalf("scheduler 任务应自动成为 Plan controller: PlanID=%s NodeRole=%s", root.PlanID, root.NodeRole)
	}
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatalf("创建 root-only Plan 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); bundle.Agent.Run(ctx) }()

	// 等待 scheduler 任务到达 completed；若在任何等待处挂死，此处超时失败
	deadline := time.Now().Add(10 * time.Second)
	var finalTask *model.Task
	for time.Now().Before(deadline) {
		if task, err := s.GetTask(root.ID); err == nil && task.Status == model.TaskStatusCompleted {
			finalTask = task
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	if finalTask == nil {
		task, _ := s.GetTask(root.ID)
		t.Fatalf("solo 下 scheduler 任务未在 10s 内 completed（疑似挂死），当前状态: %+v", task)
	}

	// root-only Plan 应随 report_done 走只读完成路径到达终态
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatalf("读取 Plan 失败: %v", err)
	}
	if !model.IsPlanTerminal(p.Status) {
		t.Errorf("Plan 应到达终态，实际 status=%s", p.Status)
	}

	// 全程不应产生任何子任务（LLM 未注册 DAG 节点，也没有 publish_task 漏网）
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll 失败: %v", err)
	}
	for _, other := range tasks {
		if other.ID != root.ID {
			t.Errorf("solo 执行不应产生额外任务，发现: id=%s desc=%s status=%s", other.ID, other.Description, other.Status)
		}
	}
}

// TestSchedulerBundle_SoloMode_DirectWriteCompletes 是 solo 写操作收尾集成测试。
//
// 场景：topo=solo + 已装配 PlanCoordinator，recordToolCall 按生产方式接线
// （bootstrap 同款闭包），因此 controller 亲自 write_file 成功后，收尾守卫能
// 看到这次写操作事实。LLM 第二轮调用 report_done 收尾。断言：
//   - scheduler 任务到达 completed（放宽前：report_done 被"尚未依据最新正式验收
//     进入终态"拒绝，随后自然文本回合又被强制继续，任务在 30 轮空转后 failed）；
//   - Plan 以 completed_no_execution 终态化（无验收运行收尾路径）；
//   - 文件真实落盘，公告板没有产生任何子任务。
func TestSchedulerBundle_SoloMode_DirectWriteCompletes(t *testing.T) {
	projectRoot := t.TempDir()

	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	mb := mailbox.NewRegistry(8)
	cfg := config.DefaultConfig()
	cfg.ProjectRoot = projectRoot
	cfg.Agents = []config.AgentKind{{Kind: "worker", Replicas: 1}}

	coordinator := plan.NewCoordinator(plan.NewMemoryStore(), nil)
	modeStore := modes.NewStore(modes.GateImmediate, modes.ExecNormal, modes.TopoSolo)

	// 与 bootstrap 一致的工具调用记录接线：write_file 成功后收尾守卫才能看到事实
	recordToolCall := func(taskID string, rec store.ToolCallRecord) {
		_ = s.AppendToolCall(taskID, rec)
	}

	mockLLM := &scriptedLLM{responses: []llm.Response{
		// 第一轮：controller 亲自写文件
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_1",
			Name:      "write_file",
			Arguments: map[string]any{"path": "solo_write.txt", "content": "solo 写入内容"},
		}}},
		// 第二轮：report_done 收尾
		{ToolCalls: []llm.ToolCall{{
			ID:        "call_2",
			Name:      "report_done",
			Arguments: map[string]any{"summary": "已写入 solo_write.txt"},
		}}},
		// 之后的回合（若有）返回纯文本
	}}

	bundle := New(s, r, mockLLM, ch, cfg, nil, mb, nil, nil, nil, recordToolCall,
		nil, nil, nil, nil, nil, nil, modeStore, coordinator)

	root := &model.Task{Description: "写入 solo_write.txt", EventType: "__scheduler__"}
	if err := s.PublishTask(root); err != nil {
		t.Fatalf("发布 scheduler 任务失败: %v", err)
	}
	if root.PlanID != root.ID || root.NodeRole != model.PlanNodeRoleController {
		t.Fatalf("scheduler 任务应自动成为 Plan controller: PlanID=%s NodeRole=%s", root.PlanID, root.NodeRole)
	}
	if _, err := coordinator.Create(context.Background(), plan.CreateInput{PlanID: root.PlanID, RootTaskID: root.ID}); err != nil {
		t.Fatalf("创建 root-only Plan 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); bundle.Agent.Run(ctx) }()

	// 等待 scheduler 任务到达 completed；若收尾被正式验收要求卡死，此处超时失败
	deadline := time.Now().Add(10 * time.Second)
	var finalTask *model.Task
	for time.Now().Before(deadline) {
		if task, err := s.GetTask(root.ID); err == nil && task.Status == model.TaskStatusCompleted {
			finalTask = task
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	if finalTask == nil {
		task, _ := s.GetTask(root.ID)
		t.Fatalf("solo 写操作任务未在 10s 内 completed（收尾被正式验收要求卡死），当前状态: %+v", task)
	}

	// 写操作事实确实被记录（否则本测试没有真正走到放宽分支）
	records, err := s.QueryToolCalls(root.ID, "write_file")
	if err != nil || len(records) == 0 || !records[0].Success {
		t.Fatalf("write_file 成功记录缺失，测试未覆盖放宽分支: records=%+v err=%v", records, err)
	}

	// Plan 应走"无验收运行"收尾到达终态
	p, err := coordinator.Store().GetPlan(root.PlanID)
	if err != nil {
		t.Fatalf("读取 Plan 失败: %v", err)
	}
	if p.Status != model.PlanStatusCompletedNoExecution {
		t.Errorf("Plan 应以 completed_no_execution 终态化，实际 status=%s", p.Status)
	}

	// 文件真实落盘
	written, err := os.ReadFile(filepath.Join(projectRoot, "solo_write.txt"))
	if err != nil || string(written) != "solo 写入内容" {
		t.Errorf("solo_write.txt 落盘内容不符: content=%q err=%v", written, err)
	}

	// 全程不应产生任何子任务
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatalf("ScanAll 失败: %v", err)
	}
	for _, other := range tasks {
		if other.ID != root.ID {
			t.Errorf("solo 执行不应产生额外任务，发现: id=%s desc=%s status=%s", other.ID, other.Description, other.Status)
		}
	}
}
