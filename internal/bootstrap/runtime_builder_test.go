package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/config"
)

// TestBuildAgentRuntime_IdleThresholdFromGlobalConfig 验证 E3 接线收口：
// 静态 kind 路径的 buildAgentRuntime 把全局 agent_idle_threshold 填入
// AgentRuntimeConfig（此前未接线，零值 0 兜底）。runner.New 消费
// rt.IdleThreshold 的末端由 internal/runner 的测试守护。
func TestBuildAgentRuntime_IdleThresholdFromGlobalConfig(t *testing.T) {
	promptPath := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(promptPath, []byte("test worker prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kind := config.AgentKind{
		Kind:             "worker",
		Tools:            []string{"read_file"},
		SystemPromptFile: promptPath,
	}
	for _, tc := range []struct {
		name string
		idle int
	}{
		{name: "全局值透传", idle: 7},
		{name: "零值保持", idle: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := buildAgentRuntime(kind, config.LLMConfig{DefaultModel: "m"}, nil,
				[]config.AgentKind{kind}, 1, tc.idle)
			if err != nil {
				t.Fatalf("buildAgentRuntime: %v", err)
			}
			if rt.IdleThreshold != tc.idle {
				t.Errorf("IdleThreshold=%d, want %d（全局 agent_idle_threshold 应透传到静态 kind 运行时）",
					rt.IdleThreshold, tc.idle)
			}
		})
	}
}

// writeTempPromptFile 是测试辅助：写一个最小 system prompt 文件并返回路径。
func writeTempPromptFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "worker.md")
	if err := os.WriteFile(p, []byte("test worker prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBuildAgentRuntime_MissingProfileIsError 验证 D3 统一解析后的错误行为：
// kind.Profile 引用了不存在的 profile 时，buildAgentRuntime 必须报错（错误中
// 带 profile 名），而不是静默留 nil AllowedTools 继续启动。
func TestBuildAgentRuntime_MissingProfileIsError(t *testing.T) {
	kind := config.AgentKind{
		Kind:             "worker",
		Profile:          "不存在",
		SystemPromptFile: writeTempPromptFile(t),
	}
	_, err := buildAgentRuntime(kind, config.LLMConfig{DefaultModel: "m"},
		map[string][]string{"other": {"read_file"}},
		[]config.AgentKind{kind}, 1, 0)
	if err == nil {
		t.Fatal("期望缺失 profile 报错，实际 nil")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误应包含缺失的 profile 名，实际: %v", err)
	}
}

// TestBuildAgentRuntime_TeamAwarenessMissingProfileIsError 验证 D3：协作者
// kind（allKinds 中其他 kind）引用缺失 profile 时，team awareness 构建同样
// 报错——原先静默忽略会把该 kind 展示为"无工具"，误导派发决策。
func TestBuildAgentRuntime_TeamAwarenessMissingProfileIsError(t *testing.T) {
	self := config.AgentKind{
		Kind:             "worker",
		Tools:            []string{"read_file"},
		SystemPromptFile: writeTempPromptFile(t),
	}
	teammate := config.AgentKind{
		Kind:    "verifier",
		Profile: "不存在",
	}
	_, err := buildAgentRuntime(self, config.LLMConfig{DefaultModel: "m"},
		map[string][]string{"other": {"read_file"}},
		[]config.AgentKind{self, teammate}, 1, 0)
	if err == nil {
		t.Fatal("期望协作者缺失 profile 报错，实际 nil")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误应包含缺失的 profile 名，实际: %v", err)
	}
}

// TestBuildAgentRuntime_ToolsInlineBypassesProfileResolution 验证 D3：kind 走
// tools 直列字段（合法配置，与 profile 互斥——config 规则 5）时不触碰 profile
// 解析，即使 ToolProfiles 表为 nil 也能正常构建。
func TestBuildAgentRuntime_ToolsInlineBypassesProfileResolution(t *testing.T) {
	kind := config.AgentKind{
		Kind:             "worker",
		Tools:            []string{"read_file", "write_file"},
		SystemPromptFile: writeTempPromptFile(t),
	}
	rt, err := buildAgentRuntime(kind, config.LLMConfig{DefaultModel: "m"}, nil,
		[]config.AgentKind{kind}, 1, 0)
	if err != nil {
		t.Fatalf("buildAgentRuntime: %v", err)
	}
	if len(rt.AllowedTools) != 4 || rt.AllowedTools[0] != "read_file" || rt.AllowedTools[1] != "write_file" ||
		rt.AllowedTools[2] != "record_observation_delta" || rt.AllowedTools[3] != "submit_change_decision" {
		t.Errorf("AllowedTools=%v, want 用户工具 + framework control", rt.AllowedTools)
	}
}

// TestResolveRouteCapabilities_MissingProfileIsError 验证 D3：静态 route 注册
// 的 profile 解析缺失即报错（原先裸 map 查找静默留 nil，靠启动校验兜底）。
func TestResolveRouteCapabilities_MissingProfileIsError(t *testing.T) {
	cfg := &config.Config{ToolProfiles: map[string][]string{"other": {"read_file"}}}
	kind := config.AgentKind{Kind: "worker", Profile: "不存在"}
	_, err := resolveRouteCapabilities(cfg, kind)
	if err == nil {
		t.Fatal("期望缺失 profile 报错，实际 nil")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误应包含缺失的 profile 名，实际: %v", err)
	}
}

// TestResolveRouteCapabilities_ToolsTakePrecedenceOverProfile 验证 D3：tools
// 直列字段优先于 profile——tools 非空时 profile（哪怕是缺失的）根本不被解析。
func TestResolveRouteCapabilities_ToolsTakePrecedenceOverProfile(t *testing.T) {
	cfg := &config.Config{} // ToolProfiles 为 nil：一旦触碰解析必报错
	kind := config.AgentKind{Kind: "worker", Tools: []string{"read_file"}, Profile: "不存在"}
	caps, err := resolveRouteCapabilities(cfg, kind)
	if err != nil {
		t.Fatalf("tools 直列应短路 profile 解析，实际报错: %v", err)
	}
	if len(caps) != 1 || caps[0] != "read_file" {
		t.Errorf("caps=%v, want [read_file]", caps)
	}
}
