package tools

// effect_journal_test.go 覆盖 V6 §4 H2b 工具层埋点：
//   - write_file / edit_file 产生 verify_first 账（ArgsDigest = 落盘内容 hash 前 12）；
//   - run_shell 产生 manual_only 账（Target 只载命令 digest，脱敏）；
//   - send_message 产生 manual_only 账；
//   - 账本失败（journal 已关闭）降级不阻断副作用本身。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/effect"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// openToolJournal 打开临时目录下的 Effect Journal 并登记 Windows 句柄清理。
func openToolJournal(t *testing.T) *effect.Journal {
	t.Helper()
	j, err := effect.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func taskCtx(agentID, taskID string) context.Context {
	return agent.WithAgentContext(context.Background(), agentID, taskID, 0)
}

func attachArtifactTask(t *testing.T, g *LocalWriteGroup, taskID string) {
	t.Helper()
	taskStore := store.NewMemoryTaskStore(nil, 8, 1, 60)
	if err := taskStore.PublishTask(&model.Task{ID: taskID, Description: "effect journal artifact"}); err != nil {
		t.Fatalf("发布 artifact 测试任务: %v", err)
	}
	g.ArtifactStore = taskStore
}

func TestWriteFile_EffectJournalVerifyFirst(t *testing.T) {
	j := openToolJournal(t)
	g, _, tmp := newWriteGroup(t, nil)
	g.EffectJournal = j
	attachArtifactTask(t, &g, "task-1")

	canonicalRoot, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(canonicalRoot, "out.txt")
	content := "effect journal 测试内容"
	if _, err := g.writeFile(taskCtx("agent-1", "task-1"), map[string]any{
		"path": target, "content": content,
	}); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	got := j.Query("task-1")
	if len(got) != 1 {
		t.Fatalf("应有 1 条副作用账，实际 %d", len(got))
	}
	e := got[0]
	if e.Kind != effect.KindFileWrite || e.Policy != effect.PolicyVerifyFirst {
		t.Fatalf("账目 kind/policy 不符: %+v", e)
	}
	if e.Status != effect.StatusSettled {
		t.Fatalf("成功写入应 settled，实际 %s", e.Status)
	}
	if e.Target != target {
		t.Fatalf("Target 应为逻辑路径 %s，实际 %s", target, e.Target)
	}
	if e.ArgsDigest != digest12([]byte(content)) {
		t.Fatalf("ArgsDigest 应为落盘内容 hash 前 12，实际 %s", e.ArgsDigest)
	}
	if !strings.Contains(e.ResultSummary, "bytes=") || !strings.Contains(e.ResultSummary, "sha256=") {
		t.Fatalf("ResultSummary 应载 bytes+hash: %q", e.ResultSummary)
	}
	// verify_first 恢复核验链路自洽：账载 digest 与盘上事实比对一致。
	res := (effect.FileHashVerifier{}).Verify(&e)
	if !res.Matched {
		t.Fatalf("账载 digest 与盘上事实应一致（恢复核验自洽）: %s", res.Detail)
	}
}

func TestEditFile_EffectJournalVerifyFirst(t *testing.T) {
	j := openToolJournal(t)
	g, _, tmp := newWriteGroup(t, nil)
	g.EffectJournal = j
	attachArtifactTask(t, &g, "task-2")

	target := filepath.Join(tmp, "edit.txt")
	if err := os.WriteFile(target, []byte("旧内容"), 0o644); err != nil {
		t.Fatalf("写初始文件: %v", err)
	}
	if _, err := g.editFile(taskCtx("agent-1", "task-2"), map[string]any{
		"path": target, "old_str": "旧内容", "new_str": "新内容",
	}); err != nil {
		t.Fatalf("editFile: %v", err)
	}

	got := j.Query("task-2")
	if len(got) != 1 {
		t.Fatalf("应有 1 条副作用账，实际 %d", len(got))
	}
	e := got[0]
	if e.Kind != effect.KindFileEdit || e.Policy != effect.PolicyVerifyFirst {
		t.Fatalf("账目 kind/policy 不符: %+v", e)
	}
	if e.Status != effect.StatusSettled {
		t.Fatalf("成功编辑应 settled，实际 %s", e.Status)
	}
	// ArgsDigest 是替换后全文的 hash——与盘上事实一致。
	if e.ArgsDigest != digest12([]byte("新内容")) {
		t.Fatalf("ArgsDigest 应为替换后全文 hash 前 12，实际 %s", e.ArgsDigest)
	}
	if res := (effect.FileHashVerifier{}).Verify(&e); !res.Matched {
		t.Fatalf("账载 digest 与盘上事实应一致: %s", res.Detail)
	}
}

func TestRunShell_EffectJournalManualOnly(t *testing.T) {
	j := openToolJournal(t)
	g, _ := newTestShellGroup(t, t.TempDir(), emptyFilter())
	g.EffectJournal = j

	registry := agent.NewToolRegistry()
	g.Register(registry)
	out, err := registry.Dispatch(taskCtx("test-agent", "task-3"), llm.ToolCall{
		Name:      "run_shell",
		Arguments: map[string]any{"command": "echo effect-journal-probe"},
	})
	if err != nil {
		t.Fatalf("run_shell: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") {
		t.Fatalf("命令应成功: %q", out)
	}

	got := j.Query("task-3")
	if len(got) != 1 {
		t.Fatalf("应有 1 条副作用账，实际 %d", len(got))
	}
	e := got[0]
	if e.Kind != effect.KindShell || e.Policy != effect.PolicyManualOnly {
		t.Fatalf("shell 账应为 manual_only: %+v", e)
	}
	if e.Status != effect.StatusSettled {
		t.Fatalf("执行完成应 settled，实际 %s", e.Status)
	}
	// 脱敏：Target 只载命令 digest，完整命令不进账本。
	if !strings.HasPrefix(e.Target, "cmd:") || strings.Contains(e.Target, "echo") {
		t.Fatalf("Target 应只载命令 digest: %q", e.Target)
	}
	if !strings.Contains(e.ResultSummary, "exit_code=0") {
		t.Fatalf("ResultSummary 应载 exit code: %q", e.ResultSummary)
	}
}

func TestSendMessage_EffectJournalManualOnly(t *testing.T) {
	j := openToolJournal(t)
	mbReg := mailbox.NewRegistry(8)
	mbReg.Register("sender", "")
	receiverBox := mbReg.Register("receiver", "")

	g := MetaGroup{MBRegistry: mbReg, AgentID: "sender", EffectJournal: j}
	reg := agent.NewToolRegistry()
	g.Register(reg)

	if _, err := reg.Dispatch(taskCtx("sender", "task-4"), llm.ToolCall{
		Name:      "send_message",
		Arguments: map[string]any{"to": "receiver", "content": "hi", "msg_type": "info"},
	}); err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if msgs := receiverBox.Drain(); len(msgs) != 1 {
		t.Fatalf("消息应送达，实际 %d 条", len(msgs))
	}

	got := j.Query("task-4")
	if len(got) != 1 {
		t.Fatalf("应有 1 条副作用账，实际 %d", len(got))
	}
	e := got[0]
	if e.Kind != effect.KindMessage || e.Policy != effect.PolicyManualOnly {
		t.Fatalf("消息账应为 manual_only: %+v", e)
	}
	if e.Status != effect.StatusSettled {
		t.Fatalf("受理后应 settled，实际 %s", e.Status)
	}
	if e.Target != "receiver" {
		t.Fatalf("Target 应载收件人，实际 %q", e.Target)
	}
	// 脱敏：正文不进账本任何字段。
	if strings.Contains(e.ResultSummary, "hi") || strings.Contains(e.ArgsDigest, "hi") {
		t.Fatalf("账本不应载消息正文: %+v", e)
	}
}

// TestEffectJournalFailureDegrades 覆盖降级纪律：journal 已关闭（落账必然
// 失败）时副作用本身照常完成——账本是观测设施，不是执行门槛。
func TestEffectJournalFailureDegrades(t *testing.T) {
	dir := t.TempDir()
	j, err := effect.OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	g, _, tmp := newWriteGroup(t, nil)
	g.EffectJournal = j
	attachArtifactTask(t, &g, "task-5")
	target := filepath.Join(tmp, "degrade.txt")
	if _, err := g.writeFile(taskCtx("agent-1", "task-5"), map[string]any{
		"path": target, "content": "账本挂了也要写",
	}); err != nil {
		t.Fatalf("账本失败不应阻断写入: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "账本挂了也要写" {
		t.Fatalf("文件应正常落盘: data=%q err=%v", data, err)
	}
}
