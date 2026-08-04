package bootstrap

// 本文件是 G1b acceptance 服务端核验桥（graph_acceptance.go）的单测：
//   - acceptanceVerifier 三类证据各自的 valid / disputed 路径，无证据 /
//     类型未知 / journal 不可用的 unverifiable 路径；
//   - command 证据的逐字纪律（命令必须存在于该任务的 shell 账、exit code
//     相符、跨任务不可借用）；
//   - file_hash 篡改与越界场景；
//   - graphChangeWaker 的唤醒任务发布与幂等查重。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/store"
)

// newAcceptanceJournal 打开 TempDir 下的真实 Effect Journal（Windows 纪律：
// t.Cleanup 先 Close 再让 TempDir 清理）。
func newAcceptanceJournal(t *testing.T) *effect.Journal {
	t.Helper()
	j, err := effect.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatalf("打开 Effect Journal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// settleShellEffect 向账本落一条已 settled 的 shell 账（模拟 run_shell 的
// 真实埋点：Target=cmd:<digest>，ArgsDigest 覆盖命令+working_dir，
// ResultSummary 载 exit_code）。
func settleShellEffect(t *testing.T, j *effect.Journal, taskID, command, workingDir string, exitCode int) {
	t.Helper()
	e := &effect.Effect{
		TaskID: taskID, Kind: effect.KindShell,
		Target:     "cmd:" + acceptanceDigest12([]byte(command)),
		ArgsDigest: acceptanceDigest12([]byte(command + "\n" + workingDir)),
		Policy:     effect.PolicyManualOnly,
	}
	if err := j.Prepare(e); err != nil {
		t.Fatalf("Prepare shell 账: %v", err)
	}
	if err := j.Settle(e.ID, fmt.Sprintf("exit_code=%d outcome=success duration_ms=3 out_bytes=0 out_sha256=", exitCode)); err != nil {
		t.Fatalf("Settle shell 账: %v", err)
	}
}

func commandEvidence(criterion, command string) graph.EvidenceItem {
	return graph.EvidenceItem{Criterion: criterion, Type: graph.EvidenceTypeCommand, Value: command}
}

// TestAcceptanceVerifierCommand command 证据：同命令 + exit 相符 → valid；
// 命令不在账 / exit 不符 / 跨任务借用 → disputed；expect_exit 生效；
// 首尾空白规范化后逐字一致。
func TestAcceptanceVerifierCommand(t *testing.T) {
	j := newAcceptanceJournal(t)
	root := t.TempDir()
	v := &acceptanceVerifier{journal: j, projectRoot: root}
	settleShellEffect(t, j, "task-v", "go test ./...", root, 0)
	settleShellEffect(t, j, "task-v", "go vet ./...", root, 1)
	settleShellEffect(t, j, "task-other", "make build", root, 0)
	settleShellEffect(t, j, "task-v", "go test ./wrong/...", t.TempDir(), 0)

	cases := []struct {
		name       string
		taskID     string
		item       graph.EvidenceItem
		wantStatus string
	}{
		{"同命令 exit0 通过", "task-v", commandEvidence("测试通过", "go test ./..."), graph.VerifyValid},
		{"首尾空白规范化", "task-v", commandEvidence("测试通过", "  go test ./...  "), graph.VerifyValid},
		{"命令不在账 disputed", "task-v", commandEvidence("静态检查", "golangci-lint run"), graph.VerifyDisputed},
		{"exit 不符 disputed", "task-v", commandEvidence("vet 通过", "go vet ./..."), graph.VerifyDisputed},
		{"跨任务不可借用", "task-v", commandEvidence("构建通过", "make build"), graph.VerifyDisputed},
		{"其它工作目录不可借用", "task-v", commandEvidence("错误目录测试", "go test ./wrong/..."), graph.VerifyDisputed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := v.VerifyAcceptance(tc.taskID, "pass", []graph.EvidenceItem{tc.item})
			if err != nil {
				t.Fatalf("VerifyAcceptance 返回错误: %v", err)
			}
			if out.Status != tc.wantStatus {
				t.Fatalf("status = %q，期望 %q（reason=%q）", out.Status, tc.wantStatus, out.Reason)
			}
			if tc.wantStatus == graph.VerifyValid && out.Checked != 1 {
				t.Errorf("valid 时 Checked 应为 1，实际 %d", out.Checked)
			}
			if tc.wantStatus == graph.VerifyDisputed && !strings.Contains(out.Reason, tc.item.Criterion) {
				t.Errorf("disputed 的 Reason 应含失败判据名: %q", out.Reason)
			}
		})
	}

	// expect_exit：账载 exit=1 的命令，声明 expect_exit=1 通过，缺省（0）不通过。
	exit1 := 1
	out, err := v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{{
		Criterion: "vet 应失败", Type: graph.EvidenceTypeCommand, Value: "go vet ./...", ExpectExit: &exit1,
	}})
	if err != nil || out.Status != graph.VerifyValid {
		t.Fatalf("expect_exit=1 应核验通过: status=%q err=%v", out.Status, err)
	}
	out, _ = v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{commandEvidence("vet 通过", "go vet ./...")})
	if out.Status != graph.VerifyDisputed {
		t.Fatalf("缺省 expect_exit=0 与账载 exit=1 不符，应 disputed，实际 %q", out.Status)
	}
}

// TestAcceptanceVerifierFileHash file_hash 证据：路径形态（记录实际 hash）与
// 路径=hash 形态（重算比对，完整 hex 与前 12 digest 均接受）；篡改、越界、
// 文件不存在 → disputed。
func TestAcceptanceVerifierFileHash(t *testing.T) {
	j := newAcceptanceJournal(t)
	root := t.TempDir()
	v := &acceptanceVerifier{journal: j, projectRoot: root}
	content := []byte("hello acceptance\n")
	if err := os.WriteFile(filepath.Join(root, "out.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	fullHash := hex.EncodeToString(sum[:])

	cases := []struct {
		name       string
		value      string
		wantStatus string
		wantNote   bool // valid 且 Reason 载实际 hash（路径形态）
	}{
		{"路径形态记录 hash", "out.txt", graph.VerifyValid, true},
		{"路径=完整 hash 一致", "out.txt=" + fullHash, graph.VerifyValid, false},
		{"路径=hash 前 12 一致", "out.txt=" + fullHash[:12], graph.VerifyValid, false},
		{"篡改的 hash disputed", "out.txt=" + strings.Repeat("0", 64), graph.VerifyDisputed, false},
		{"越界路径 disputed", "../outside.txt", graph.VerifyDisputed, false},
		{"文件不存在 disputed", "missing.txt", graph.VerifyDisputed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{{
				Criterion: "产物正确", Type: graph.EvidenceTypeFileHash, Value: tc.value,
			}})
			if err != nil {
				t.Fatalf("VerifyAcceptance 返回错误: %v", err)
			}
			if out.Status != tc.wantStatus {
				t.Fatalf("status = %q，期望 %q（reason=%q）", out.Status, tc.wantStatus, out.Reason)
			}
			if tc.wantNote && !strings.Contains(out.Reason, fullHash[:12]) {
				t.Errorf("路径形态应在 Reason 载实际 hash 供记录: %q", out.Reason)
			}
		})
	}
}

// TestAcceptanceVerifierTaskStatus task_status 证据：合法词逐字通过；非法词
// （含近义词 success/passed）disputed。
func TestAcceptanceVerifierTaskStatus(t *testing.T) {
	j := newAcceptanceJournal(t)
	v := &acceptanceVerifier{journal: j, projectRoot: t.TempDir()}
	for _, word := range []string{"completed", "failed", "blocked", "cancelled", "pass", "fail"} {
		out, err := v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{{
			Criterion: "状态核对", Type: graph.EvidenceTypeTaskStatus, Value: word,
		}})
		if err != nil || out.Status != graph.VerifyValid {
			t.Errorf("状态词 %q 应核验通过: status=%q err=%v", word, out.Status, err)
		}
	}
	for _, word := range []string{"success", "passed", "ok", ""} {
		out, _ := v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{{
			Criterion: "状态核对", Type: graph.EvidenceTypeTaskStatus, Value: word,
		}})
		if out.Status != graph.VerifyDisputed {
			t.Errorf("非法状态词 %q 应 disputed，实际 %q", word, out.Status)
		}
	}
}

// TestAcceptanceVerifierUnverifiablePaths unverifiable 三条路径：无证据 /
// 证据类型未知 / journal 不可用（不误判 valid）。
func TestAcceptanceVerifierUnverifiablePaths(t *testing.T) {
	j := newAcceptanceJournal(t)
	v := &acceptanceVerifier{journal: j, projectRoot: t.TempDir()}

	out, err := v.VerifyAcceptance("task-v", "pass", nil)
	if err != nil || out.Status != graph.VerifyUnverifiable {
		t.Errorf("无证据应 unverifiable: status=%q err=%v", out.Status, err)
	}
	out, _ = v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{{
		Criterion: "c", Type: "screenshot", Value: "x",
	}})
	if out.Status != graph.VerifyUnverifiable || !strings.Contains(out.Reason, "未知") {
		t.Errorf("类型未知应 unverifiable 且说明: %+v", out)
	}

	vNoJournal := &acceptanceVerifier{journal: nil, projectRoot: t.TempDir()}
	out, err = vNoJournal.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{{
		Criterion: "状态核对", Type: graph.EvidenceTypeTaskStatus, Value: "pass",
	}})
	if err != nil || out.Status != graph.VerifyUnverifiable {
		t.Errorf("journal 不可用应 unverifiable（不误判 valid）: status=%q err=%v", out.Status, err)
	}
}

// TestAcceptanceVerifierMixedEvidence 混合证据：全部通过时 Checked 为总条数；
// 首条失败即短路（Checked 记录已核验条数，Reason 指认失败判据）。
func TestAcceptanceVerifierMixedEvidence(t *testing.T) {
	j := newAcceptanceJournal(t)
	root := t.TempDir()
	v := &acceptanceVerifier{journal: j, projectRoot: root}
	settleShellEffect(t, j, "task-v", "go test ./...", root, 0)

	out, err := v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{
		commandEvidence("测试通过", "go test ./..."),
		{Criterion: "状态核对", Type: graph.EvidenceTypeTaskStatus, Value: "pass"},
	})
	if err != nil || out.Status != graph.VerifyValid || out.Checked != 2 {
		t.Fatalf("两条证据全部通过应 valid 且 Checked=2: %+v err=%v", out, err)
	}

	out, _ = v.VerifyAcceptance("task-v", "pass", []graph.EvidenceItem{
		commandEvidence("测试通过", "go test ./..."),
		{Criterion: "状态核对", Type: graph.EvidenceTypeTaskStatus, Value: "success"},
	})
	if out.Status != graph.VerifyDisputed || out.Checked != 1 || !strings.Contains(out.Reason, "状态核对") {
		t.Fatalf("第二条失败应 disputed、Checked=1、Reason 指认判据: %+v", out)
	}
}

// TestGraphChangeWaker 唤醒任务：发布到 __scheduler__ 队列（含幂等标记、
// 不带图身份、ParentTaskID 挂来源任务）；同 marker 重复唤醒幂等查重。
func TestGraphChangeWaker(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	w := graphChangeWaker{store: s}
	spec := graph.GraphChangeWakeSpec{
		GraphID: "g-1", NodeID: "verify", ActivationID: "verify@1",
		TaskID: "task-v", Reason: "acceptance_disputed", Detail: "证据未通过",
	}
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("WakeGraphChange: %v", err)
	}
	tasks, err := s.ScanAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("应发布 1 个唤醒任务，实际 %d", len(tasks))
	}
	wake := tasks[0]
	if wake.EventType != "__scheduler__" || wake.EventSource != "graph-change-request" {
		t.Errorf("唤醒任务路由不符: EventType=%q EventSource=%q", wake.EventType, wake.EventSource)
	}
	if !strings.Contains(wake.Description, "[graph-change-request: g-1/verify@1/change]") {
		t.Errorf("唤醒任务描述应含幂等标记: %q", wake.Description)
	}
	if wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
		t.Errorf("唤醒任务不得携带图身份（防 feed 误回填）: %+v", wake)
	}
	if wake.ParentTaskID != "task-v" || wake.MaxConcurrency != 1 {
		t.Errorf("唤醒任务应挂来源任务且 MaxConcurrency=1: %+v", wake)
	}

	// 同一 activation 重复唤醒：幂等查重，不重复发布。
	if err := w.WakeGraphChange(spec); err != nil {
		t.Fatalf("重复 WakeGraphChange: %v", err)
	}
	tasks, _ = s.ScanAll()
	if len(tasks) != 1 {
		t.Fatalf("重复唤醒应幂等查重（仍 1 个任务），实际 %d", len(tasks))
	}
}
