package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSWEWorkerPromptPinsDecisionProgressAndControlRetryContract(t *testing.T) {
	text := readSWEPrompt(t, "worker.md")
	ordered := []string{
		"# 角色", "# 可用工具", "# 环境事实", "# 工作方式",
		"调查假设 → mutation → typed check",
		"新 read/grep 只能更新知识状态",
		"不会自动成为 confirmed 语义事实",
		"触发节奏由冻结 ProgressContract 决定",
		"只按系统给出的有界重试",
		"不要自行输出 JSON/DSML 标记",
		"Observation 是行动承诺边界",
		"typed next_action",
		"下一步必须执行该 mutation",
		"RecoveryDelta v4 handoff",
		"focus page",
		"只有你主动选择 edit",
		"完成后用 submit_task_result 提交",
	}
	assertPromptOrder(t, text, ordered)
	toolSection := text[strings.Index(text, "# 可用工具"):strings.Index(text, "# 环境事实")]
	if strings.Contains(toolSection, "record_observation_delta") {
		t.Fatal("普通 Worker 工具清单不得暴露 framework Observation 控制工具")
	}
	if strings.Contains(text, "普通调查命令才使用 run_shell") || strings.Contains(toolSection, "run_shell") {
		t.Fatal("SWE Worker prompt 不得声明 Graph v3 mutating Lease 已移除的 raw run_shell")
	}
	if !strings.Contains(text, "不是普通业务工具") || !strings.Contains(text, "唯一 tool schema") {
		t.Fatal("Worker prompt 必须说明 Observation 只由独立 control phase 暴露")
	}
	if !strings.Contains(text, "uv run --no-sync python -m pytest -q") || strings.Contains(text, `command=".venv/bin/python`) {
		t.Fatal("Worker prompt 必须使用 SWE Test Runner 的跨平台 uv 命令，禁止 POSIX venv 路径")
	}
	if !strings.Contains(text, "tool schema enum 逐字复制") || !strings.Contains(text, "禁止发明") {
		t.Fatal("Worker prompt 必须要求 check_id 从当前合同 enum 复制")
	}
	assertNoModelSpecificPromptBranch(t, text)
}

func TestSWEExplorerPromptPinsBoundedStructuredHandoff(t *testing.T) {
	text := readSWEPrompt(t, "explorer.md")
	for _, fragment := range []string{
		"独立调查节点", "最小证据集", "hypothesis", "evidence_files",
		"反证检查", "公开 API / proxy", "内部访问路径", "状态所有权边界",
		"第一条具体 exception", "failure_observation", "failure_kind",
		"evidence_ranges", "boundary_evidence", "public_entry", "state_owner",
		"internal_consumer", "rejected_alternative", "recommended_change",
		"backing field", "alias", "verification_focus", "提交 blocked",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("SWE Explorer prompt 缺少结构化 handoff 约束 %q", fragment)
		}
	}
	assertNoModelSpecificPromptBranch(t, text)
}

func TestSWEVerifierPromptPinsVerificationReserveAndTerminalHandoff(t *testing.T) {
	text := readSWEPrompt(t, "verifier.md")
	ordered := []string{
		"# 角色", "# 可用工具", "# 判定契约",
		"verification reserve", "冻结的 patch", "不得重新进行无界源码调查",
		"submit_task_result(status=blocked", "完成核验后必须调用 submit_task_result",
		"不要自然文本退出",
	}
	assertPromptOrder(t, text, ordered)
	assertNoModelSpecificPromptBranch(t, text)
}

func readSWEPrompt(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "prompts", "swe", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertPromptOrder(t *testing.T, text string, ordered []string) {
	t.Helper()
	last := -1
	for _, fragment := range ordered {
		index := strings.Index(text, fragment)
		if index < 0 || index <= last {
			t.Fatalf("prompt component/order 缺失或漂移: fragment=%q index=%d last=%d", fragment, index, last)
		}
		last = index
	}
}

func assertNoModelSpecificPromptBranch(t *testing.T, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, forbidden := range []string{"qwen", "deepseek", "按 provider", "按模型名称"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("SWE prompt 含模型/provider 特判 %q", forbidden)
		}
	}
}
