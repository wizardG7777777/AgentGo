package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 检查角色边界与外部判题权威，不能用旧观察/决策流程限制正常调查。
func TestSWEPromptsUseExecutionFactsAndRoleBoundaries(t *testing.T) {
	for _, role := range []string{"worker", "explorer", "verifier"} {
		t.Run(role, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "prompts", "swe", role+".md"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for _, old := range []string{"record_observation_delta", "submit_change_decision", "run_check", "CheckContract", "verification reserve", "RecoveryDelta", "唯一 tool schema"} {
				if strings.Contains(text, old) {
					t.Errorf("提示词仍承诺退役机制 %s", old)
				}
			}
			for _, required := range []string{"submit_task_result", "SWE Test Runner", "inspect_node", "read_evidence"} {
				if !strings.Contains(text, required) {
					t.Errorf("提示词缺少权责说明 %s", required)
				}
			}
			if role == "worker" {
				for _, name := range []string{"apply_change", "run_shell", "uv run --no-sync python -m pytest -q"} {
					if !strings.Contains(text, name) {
						t.Errorf("执行角色缺少 %s", name)
					}
				}
			} else if strings.Contains(text, "完成与缺陷相关的最小修复") {
				t.Error("非写入角色不应承诺实施源码修复")
			}
			if role == "verifier" && !strings.Contains(text, "不执行 Shell") {
				t.Error("验收角色必须说明 Shell 权限边界")
			}
		})
	}
}
