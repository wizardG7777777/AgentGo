package tools

// meta_capability_test.go 覆盖 publish_task 的 per-node 节点能力（NodeCapability）
// 发布面：三道校验（写入权限 / 静态合法 / 伴生关系软警告）与 Task.Capability 落盘。

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/agent"
)

// schedulerCapableMeta 构造一个具备控制面权限（AllowNodeCapability=true）
// 的 MetaGroup，与 scheduler 装配时的注入一致。
func schedulerCapableMeta(s *fakeStore) MetaGroup {
	return MetaGroup{Store: s, AllowNodeCapability: true}
}

// a. 写入权限：非控制面（Worker/Reactor 装配下 AllowNodeCapability 为零值）
// 携带 tools 或 model 参数必须被拒绝。
func TestPublishTask_Capability_NonControlPlaneRejected(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"tools 参数", map[string]any{"description": "x", "tools": "read_file"}},
		{"model 参数", map[string]any{"description": "x", "model": "deepseek-r1"}},
		{"两者同时", map[string]any{"description": "x", "tools": "read_file", "model": "m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeStore()
			// Worker 语义：AllowNodeCapability 留零值（bootstrap 对非 scheduler 不注入）。
			g := MetaGroup{Store: s}
			reg := agent.NewToolRegistry()
			g.Register(reg)
			_, err := reg.Dispatch(context.Background(), mkCall("publish_task", tc.args))
			if err == nil || !strings.Contains(err.Error(), "只能由 Scheduler 计划控制面设置") {
				t.Fatalf("非控制面携带能力参数应被拒绝，got %v", err)
			}
			if len(s.createCalls) != 0 {
				t.Fatalf("拒绝前不得发布任务: %+v", s.createCalls)
			}
		})
	}
}

// b. 静态合法：tools 含未注册工具名时必须拒绝；model 只做去空白校验。
func TestPublishTask_Capability_UnknownToolRejected(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "bad tools", "tools": "read_file,not_a_tool",
	}))
	if err == nil || !strings.Contains(err.Error(), "未注册工具名") {
		t.Fatalf("未知工具名应被拒绝，got %v", err)
	}
	if len(s.createCalls) != 0 {
		t.Fatalf("校验失败前不得发布任务: %+v", s.createCalls)
	}
}

// 合法 tools + model 全量设置后，Task.Capability 必须完整落盘。
func TestPublishTask_Capability_PersistedOnTask(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "cap node", "tools": " read_file , web_fetch ", "model": " deepseek-r1 ",
	}))
	if err != nil {
		t.Fatalf("合法能力声明应发布成功: %v", err)
	}
	if len(s.createCalls) != 1 {
		t.Fatalf("expected 1 task, got %d", len(s.createCalls))
	}
	cap := s.createCalls[0].Capability
	if cap == nil {
		t.Fatal("Task.Capability 未写入")
	}
	if len(cap.Tools) != 2 || cap.Tools[0] != "read_file" || cap.Tools[1] != "web_fetch" {
		t.Errorf("Capability.Tools 解析错误（空白/逗号归一化）: %+v", cap.Tools)
	}
	if cap.Model != "deepseek-r1" {
		t.Errorf("Capability.Model 应去空白，got %q", cap.Model)
	}
}

// 仅 model 覆盖（不设 tools）同样合法，Tools 保持空。
func TestPublishTask_Capability_ModelOnly(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	if _, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "model only", "model": "deepseek-r1",
	})); err != nil {
		t.Fatalf("仅 model 覆盖应发布成功: %v", err)
	}
	cap := s.createCalls[0].Capability
	if cap == nil || cap.Model != "deepseek-r1" || len(cap.Tools) != 0 {
		t.Fatalf("仅 model 覆盖的 Capability 形态错误: %+v", cap)
	}
}

// 未携带任一参数时 Capability 必须保持 nil（零值兼容既有任务）。
func TestPublishTask_Capability_NilWhenAbsent(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	if _, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "plain",
	})); err != nil {
		t.Fatalf("普通发布应成功: %v", err)
	}
	if s.createCalls[0].Capability != nil {
		t.Fatalf("未声明能力参数时 Capability 必须为 nil，got %+v", s.createCalls[0].Capability)
	}
}

// c. 伴生关系软警告：写无读必须警告但不拒绝；纯 web_fetch 极简节点不应有警告。
func TestPublishTask_Capability_CompanionWarnings(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	out, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "write without read", "tools": "write_file",
	}))
	if err != nil {
		t.Fatalf("软警告不得拒绝发布: %v", err)
	}
	if !strings.Contains(out, "节点能力警告") || !strings.Contains(out, "require-read-before-write") {
		t.Errorf("写无读应在返回文本中警告，got %q", out)
	}

	out, err = reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "minimal investigation", "tools": "web_fetch",
	}))
	if err != nil {
		t.Fatalf("极简调查节点应发布成功: %v", err)
	}
	if strings.Contains(out, "节点能力警告") {
		t.Errorf("纯 web_fetch 节点不应触发警告，got %q", out)
	}

	// 无任何执行类工具的子集 → 提示只能纯文字收尾。
	out, err = reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "meta only", "tools": "send_message",
	}))
	if err != nil {
		t.Fatalf("meta 工具子集应发布成功: %v", err)
	}
	if !strings.Contains(out, "纯文字响应收尾") {
		t.Errorf("无执行类工具时应提示收尾通道，got %q", out)
	}
}

// ===== isolation 参数（写时复制执行隔离声明）=====
// 与 tools/model 同一守卫模式：仅 Scheduler 计划控制面可写；非空值只接受
// "workspace"；合法值落到 Task.Capability.Isolation。

// a. 写入权限：非控制面携带 isolation 参数必须被拒绝（与 tools/model 同款）。
func TestPublishTask_Isolation_NonControlPlaneRejected(t *testing.T) {
	s := newFakeStore()
	// Worker 语义：AllowNodeCapability 留零值（bootstrap 对非 scheduler 不注入）。
	g := MetaGroup{Store: s}
	reg := agent.NewToolRegistry()
	g.Register(reg)
	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "x", "isolation": "workspace",
	}))
	if err == nil || !strings.Contains(err.Error(), "只能由 Scheduler 计划控制面设置") {
		t.Fatalf("非控制面携带 isolation 应被拒绝，got %v", err)
	}
	if len(s.createCalls) != 0 {
		t.Fatalf("拒绝前不得发布任务: %+v", s.createCalls)
	}
}

// b. 静态合法：非 "workspace" 的隔离值必须报错（拼错不得静默降级为不隔离）。
func TestPublishTask_Isolation_UnknownModeRejected(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "bad isolation", "isolation": "container",
	}))
	if err == nil || !strings.Contains(err.Error(), "isolation 参数只接受") {
		t.Fatalf("非法 isolation 值应被拒绝，got %v", err)
	}
	if len(s.createCalls) != 0 {
		t.Fatalf("校验失败前不得发布任务: %+v", s.createCalls)
	}
}

// 合法 isolation="workspace" 必须落到 Task.Capability.Isolation（去空白）。
func TestPublishTask_Isolation_PersistedOnTask(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "isolated node", "isolation": " workspace ",
	}))
	if err != nil {
		t.Fatalf("合法 isolation 声明应发布成功: %v", err)
	}
	if len(s.createCalls) != 1 {
		t.Fatalf("expected 1 task, got %d", len(s.createCalls))
	}
	cap := s.createCalls[0].Capability
	if cap == nil || cap.Isolation == nil {
		t.Fatalf("Task.Capability.Isolation 未写入: %+v", cap)
	}
	if cap.Isolation.Mode != "workspace" {
		t.Errorf("Isolation.Mode 应去空白后为 workspace，got %q", cap.Isolation.Mode)
	}
	// 仅 isolation 时 Tools/Model 保持零值。
	if len(cap.Tools) != 0 || cap.Model != "" {
		t.Errorf("仅 isolation 的 Capability 形态错误: %+v", cap)
	}
}

// isolation 与 tools/model 同设时三者完整落盘。
func TestPublishTask_Isolation_WithToolsAndModel(t *testing.T) {
	s := newFakeStore()
	g := schedulerCapableMeta(s)
	reg := agent.NewToolRegistry()
	g.Register(reg)

	_, err := reg.Dispatch(context.Background(), mkCall("publish_task", map[string]any{
		"description": "full cap", "tools": "read_file,write_file", "model": "deepseek-r1", "isolation": "workspace",
	}))
	if err != nil {
		t.Fatalf("全量能力声明应发布成功: %v", err)
	}
	cap := s.createCalls[0].Capability
	if cap == nil || cap.Isolation == nil || cap.Isolation.Mode != "workspace" {
		t.Fatalf("Capability.Isolation 未随全量声明落盘: %+v", cap)
	}
	if len(cap.Tools) != 2 || cap.Model != "deepseek-r1" {
		t.Errorf("Capability.Tools/Model 形态错误: %+v", cap)
	}
}
