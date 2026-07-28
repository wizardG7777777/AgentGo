package plan

// acceptance_evidence_purity_test.go 覆盖类型化 criterion 的证据纯度规则：
// file_hash / command_exit / task_status criterion 的证据列表只接受同类证据。
// verifier 多次真实运行中把佐证性异类证据混挂到同一 criterion（2026-07-27/28），
// 错误消息必须指明违例证据 ID 并给出可执行的修正指引。

import (
	"strings"
	"testing"

	"agentgo/internal/model"
)

func evCommand(id string) model.Evidence {
	ec := 0
	return model.Evidence{ID: id, Kind: "command", Command: "go test ./...", ExitCode: &ec}
}

func TestCriterionEvidenceReason_FileHashRejectsCommandEvidenceWithGuidance(t *testing.T) {
	criterion := model.Criterion{ID: "conflict-file", Check: criterionCheckFileHash}
	reason := criterionEvidenceReason(criterion, evCommand("ev-certutil"))
	if reason == "" {
		t.Fatal("file_hash criterion 挂 command 证据必须被拒绝")
	}
	if !strings.Contains(reason, "ev-certutil") {
		t.Fatalf("消息应指明违例证据 ID，实际: %s", reason)
	}
	if !strings.Contains(reason, "accepts only file evidence") {
		t.Fatalf("消息应说明只接受 file 证据，实际: %s", reason)
	}
	if !strings.Contains(reason, "move command/task_status evidence") {
		t.Fatalf("消息应给出修正指引，实际: %s", reason)
	}
}

func TestCriterionEvidenceReason_CommandExitRejectsFileEvidenceWithGuidance(t *testing.T) {
	criterion := model.Criterion{ID: "build-ok", Check: criterionCheckCommandExit}
	reason := criterionEvidenceReason(criterion, model.Evidence{ID: "ev-file", FilePath: "a.txt", FileHash: "abc"})
	if reason == "" {
		t.Fatal("command_exit criterion 挂 file 证据必须被拒绝")
	}
	if !strings.Contains(reason, "ev-file") || !strings.Contains(reason, "accepts only command evidence") {
		t.Fatalf("消息应指明违例证据与规则，实际: %s", reason)
	}
}

func TestCriterionEvidenceReason_TaskStatusRejectsCommandEvidenceWithGuidance(t *testing.T) {
	criterion := model.Criterion{ID: "task-done", Check: criterionCheckTaskStatus}
	reason := criterionEvidenceReason(criterion, evCommand("ev-cmd"))
	if reason == "" {
		t.Fatal("task_status criterion 挂 command 证据必须被拒绝")
	}
	if !strings.Contains(reason, "ev-cmd") || !strings.Contains(reason, "accepts only task-status evidence") {
		t.Fatalf("消息应指明违例证据与规则，实际: %s", reason)
	}
}

// 同类证据不受影响：file 证据挂在 file_hash criterion 上通过（目标/hash 校验照旧）。
func TestCriterionEvidenceReason_TypedEvidenceStillAccepted(t *testing.T) {
	criterion := model.Criterion{ID: "c1", Check: criterionCheckFileHash, Target: "a.txt", Expected: "ABC"}
	if reason := criterionEvidenceReason(criterion, model.Evidence{ID: "ev", FilePath: "a.txt", FileHash: "abc"}); reason != "" {
		t.Fatalf("匹配的 file 证据应通过，实际: %s", reason)
	}
	criterion2 := model.Criterion{ID: "c2", Check: criterionCheckCommandExit, Expected: "0"}
	if reason := criterionEvidenceReason(criterion2, evCommand("ev")); reason != "" {
		t.Fatalf("command 证据应通过，实际: %s", reason)
	}
	criterion3 := model.Criterion{ID: "c3", Check: criterionCheckTaskStatus, Target: "t1", Expected: "completed"}
	if reason := criterionEvidenceReason(criterion3, model.Evidence{ID: "ev", TaskID: "t1", Output: "completed"}); reason != "" {
		t.Fatalf("task-status 证据应通过，实际: %s", reason)
	}
}
