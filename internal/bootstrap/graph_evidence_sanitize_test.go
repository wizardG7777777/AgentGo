package bootstrap

import (
	"agentgo/internal/store"
	"strings"
	"testing"
)

func dsmlGarbageToolName() string {
	return "run_shell>\n<｜DSML｜parameter name=\"command\" string=\"true\">" + strings.Repeat("x", 200)
}
func TestEvidenceKindOfNormalization(t *testing.T) {
	cases := []struct // SWE-002 第一层防线（evidence 装配归一）的单测与事故形状端到端回归：
	//   - evidenceKindOf：垃圾名归一 unknown、合法自定义名保留、超长合法名截断；
	//   - evidenceCallEntry：DSML 垃圾名 → kind=unknown + tool_name=malformed 占位
	//     （确定性），装配产物过 store 的 validateEvidenceEntryBounds 权威校验；
	//   - 集成回归：200+ 字符 DSML 垃圾名进账本后，终态回填不再整条拒写——
	//     节点正常 completed、图继续推进（SWE-002 事故形状不再复现）。
	// dsmlGarbageToolName 复现事故形状：模型把 DSML 标记泄进 tool_call 名字段。
	// 超长但形状合法的名字：截断到 64 rune（末位省略号），不归一 unknown、
	// 更不会原样透传撞 MaxIDLength=128。
	// TestEvidenceCallEntrySanitizesGarbage 验证单条调用证据的归一：DSML 垃圾名
	// → kind=unknown、tool_name=确定性 malformed 占位；原始垃圾名不进证据。
	// 确定性：同一垃圾名永远同一占位；不同垃圾名占位不同。
	// 合法名逐字节保留（已知工具名 kind 仍归并）。
	{
		name     string
		toolName string
		want     string
	}{{"已知归并 shell", "run_shell", "shell"}, {"已知归并 file_write", "apply_change", "file_change"}, {"已知归并 web", "web_fetch", "web"}, {"合法自定义名保留", "submit_task_result", "submit_task_result"}, {"合法自定义名含冒号点线", "mcp__x.y:z-w", "mcp__x.y:z-w"}, {"DSML 垃圾名归一 unknown", dsmlGarbageToolName(), "unknown"}, {"含换行归一 unknown", "run_shell\nxx", "unknown"}, {"数字开头归一 unknown", "3foo", "unknown"}, {"空串归一 unknown", "", "unknown"}, {"含 CJK 归一 unknown", "工具", "unknown"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := evidenceKindOf(tc.toolName); got != tc.want {
				t.Errorf("evidenceKindOf(%q) = %q，应为 %q", tc.toolName, got, tc.want)
			}
		})
	}
	long := strings.Repeat("a", 200)
	got := evidenceKindOf(long)
	if len([]rune(got)) != evidenceKindMaxRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("超长合法名应截断到 %d rune 带省略号，实际 %q（%d rune）", evidenceKindMaxRunes, got, len([]rune(got)))
	}
}

func TestEvidenceCallEntrySanitizesGarbage(t *testing.T) {
	raw := dsmlGarbageToolName()
	call := store.ToolCallRecord{CallID: "c-1", AgentID: "a-1", ToolName: raw, Success: true}
	entry := evidenceCallEntry("ev:t:call:1", call)
	if entry.Kind != "unknown" {
		t.Errorf("垃圾名 kind 应归一 unknown，实际 %q", entry.Kind)
	}
	wantPlaceholder := store.MalformedToolNamePlaceholder(raw)
	if entry.ToolName != wantPlaceholder {
		t.Errorf("tool_name 应为确定性占位 %q，实际 %q", wantPlaceholder, entry.ToolName)
	}
	if strings.Contains(entry.ToolName, raw) || len([]rune(entry.ToolName)) > 64 {
		t.Errorf("原始垃圾名不得进入证据层: %q", entry.ToolName)
	}
	again := evidenceCallEntry("ev:t:call:2", call)
	if again.ToolName != entry.ToolName {
		t.Errorf("同一垃圾名应得同一占位: %q vs %q", again.ToolName, entry.ToolName)
	}
	other := evidenceCallEntry("ev:t:call:3", store.ToolCallRecord{ToolName: "apply_change>|<" + strings.Repeat("y", 150)})
	if other.ToolName == entry.ToolName {
		t.Errorf("不同垃圾名占位应可区分，均为 %q", entry.ToolName)
	}
	legal := evidenceCallEntry("ev:t:call:4", store.ToolCallRecord{CallID: "c-4", ToolName: "run_shell", Args: map[string]any{"command": "pytest -q | tail"}, Success: true, ExitCodeScope: store.ShellExitCodeScopeLastPipelineCommand})
	if legal.ToolName != "run_shell" || legal.Kind != "shell" || legal.ExitCodeScope != string(store.ShellExitCodeScopeLastPipelineCommand) {
		t.Errorf("合法名应原样保留且 kind 归并: %+v", legal)
	}
	rejected := evidenceCallEntry("ev:t:call:5", store.ToolCallRecord{CallID: "c-5", ToolName: "run_shell", Args: map[string]any{"command": "pytest | tail"}, Success: false})
	if !strings.Contains(rejected.Summary, "scope=?") || rejected.ExitCodeScope != "" {
		t.Errorf("执行前拒绝的 pipeline 不得伪装 whole-command scope: %+v", rejected)
	}
}
