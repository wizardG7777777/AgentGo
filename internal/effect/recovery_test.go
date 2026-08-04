package effect

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"agentgo/internal/trace"
)

// digestOf 计算内容的 sha256 hex 前 12（与 FileHashVerifier 同口径）。
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:argsDigestLen]
}

// traceCapture 截获 trace.Emit 事件（Dispatcher 接口），用于断言 effect_*
// 事件的发出与字段。
type traceCapture struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *traceCapture) Dispatch(ev trace.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *traceCapture) byKind(kind trace.EventKind) []trace.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []trace.Event
	for _, ev := range c.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// installTraceCapture 挂载事件截获并在测试结束时恢复原 dispatcher。
func installTraceCapture(t *testing.T) *traceCapture {
	t.Helper()
	c := &traceCapture{}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(c)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })
	return c
}

// prepareRaw 直接 Prepare 一条 Effect（测试快捷方式）。
func prepareRaw(t *testing.T, j *Journal, taskID string, kind Kind, target, digest string, policy ReplayPolicy) string {
	t.Helper()
	e := Effect{TaskID: taskID, Kind: kind, Target: target, ArgsDigest: digest, Policy: policy}
	if err := j.Prepare(&e); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return e.ID
}

// TestRecoverVerifyFirstMatchedSettles 覆盖：prepared 未 settled 的文件写在
// 重启后被标 unknown，verify_first 核验盘上 hash 与账载一致 → 转 settled
// 「已核验」，并发 effect_recovery_decided（decision=verified_settled）。
func TestRecoverVerifyFirstMatchedSettles(t *testing.T) {
	dir := t.TempDir()
	capture := installTraceCapture(t)

	content := []byte("已落盘的内容")
	target := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatalf("写目标文件: %v", err)
	}

	j := openTestJournal(t, dir)
	id := prepareRaw(t, j, "task-1", KindFileWrite, target, digestOf(content), PolicyVerifyFirst)

	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 1 {
		t.Fatalf("应有 1 条裁决，实际 %d", len(decisions))
	}
	d := decisions[0]
	if d.Decision != DecisionVerifiedSettled {
		t.Fatalf("裁决应为 verified_settled，实际 %s（%s）", d.Decision, d.Reason)
	}
	got := j.Query("task-1")
	if got[0].Status != StatusSettled {
		t.Fatalf("核验一致后应转 settled，实际 %s", got[0].Status)
	}
	if got[0].ResultSummary == "" {
		t.Fatal("settled 应载恢复核验摘要")
	}
	// 事件对：effect_unknown（标未知）+ effect_recovery_decided（裁决）。
	if n := len(capture.byKind(trace.KindEffectUnknown)); n != 1 {
		t.Fatalf("应发 1 条 effect_unknown，实际 %d", n)
	}
	decided := capture.byKind(trace.KindEffectRecoveryDecided)
	if len(decided) != 1 {
		t.Fatalf("应发 1 条 effect_recovery_decided，实际 %d", len(decided))
	}
	pl := decided[0].Effect
	if pl == nil || pl.EffectID != id || pl.Decision != DecisionVerifiedSettled ||
		pl.Kind != string(KindFileWrite) || pl.Policy != string(PolicyVerifyFirst) {
		t.Fatalf("recovery_decided payload 字段不正确: %+v", pl)
	}
}

// TestRecoverVerifyFirstMismatchKeepsUnknown 覆盖：盘上内容与账载 digest
// 不一致 → 保持 unknown（不转 settled），decision=kept_unknown_mismatch。
func TestRecoverVerifyFirstMismatchKeepsUnknown(t *testing.T) {
	dir := t.TempDir()
	installTraceCapture(t)

	target := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(target, []byte("盘上真实内容"), 0o644); err != nil {
		t.Fatalf("写目标文件: %v", err)
	}

	j := openTestJournal(t, dir)
	prepareRaw(t, j, "task-1", KindFileEdit, target, digestOf([]byte("账载的另一份内容")), PolicyVerifyFirst)

	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 1 || decisions[0].Decision != DecisionKeptUnknownMismatch {
		t.Fatalf("裁决应为 kept_unknown_mismatch: %+v", decisions)
	}
	got := j.Query("task-1")
	if got[0].Status != StatusUnknown {
		t.Fatalf("核验不一致应保持 unknown，实际 %s", got[0].Status)
	}
}

// TestRecoverVerifyFirstUnverifiableKeepsUnknown 覆盖：目标文件不可读
// （不存在）→ 无法核验，保持 unknown，decision=kept_unknown_unverifiable。
func TestRecoverVerifyFirstUnverifiableKeepsUnknown(t *testing.T) {
	dir := t.TempDir()
	installTraceCapture(t)

	j := openTestJournal(t, dir)
	prepareRaw(t, j, "task-1", KindFileWrite, filepath.Join(dir, "missing.txt"), digestOf([]byte("x")), PolicyVerifyFirst)

	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 1 || decisions[0].Decision != DecisionKeptUnknownUnverifiable {
		t.Fatalf("裁决应为 kept_unknown_unverifiable: %+v", decisions)
	}
	if got := j.Query("task-1"); got[0].Status != StatusUnknown {
		t.Fatalf("无法核验应保持 unknown，实际 %s", got[0].Status)
	}
}

// TestRecoverManualOnlyNoAutoAction 覆盖：manual_only / never_replay 的
// 未 settled 副作用在恢复时不产生任何自动动作——保持 unknown +
// decision=kept_unknown_manual，账本不出现 settle 行。
func TestRecoverManualOnlyNoAutoAction(t *testing.T) {
	dir := t.TempDir()
	capture := installTraceCapture(t)

	j := openTestJournal(t, dir)
	prepareRaw(t, j, "task-1", KindShell, "cmd:abc", digestOf([]byte("rm -rf x")), PolicyManualOnly)
	prepareRaw(t, j, "task-1", KindMessage, "worker-1", digestOf([]byte("hello")), PolicyManualOnly)
	prepareRaw(t, j, "task-2", KindWorkspaceMerge, "task-2", digestOf([]byte("task-2|agent")), PolicyNeverReplay)

	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 3 {
		t.Fatalf("应有 3 条裁决，实际 %d", len(decisions))
	}
	for _, d := range decisions {
		if d.Decision != DecisionKeptUnknownManual {
			t.Fatalf("manual_only/never_replay 裁决应为 kept_unknown_manual: %+v", d)
		}
	}
	// 没有任何条目被自动 settle——恢复禁止自动动作。
	if n := len(j.QueryByStatus(StatusSettled)); n != 0 {
		t.Fatalf("恢复不应自动 settle 任何条目，实际 %d", n)
	}
	if n := len(j.QueryByStatus(StatusUnknown)); n != 3 {
		t.Fatalf("3 条应保持 unknown，实际 %d", n)
	}
	// unknown 清单经 trace 事件可见：3 条 effect_unknown + 3 条 recovery_decided。
	if n := len(capture.byKind(trace.KindEffectUnknown)); n != 3 {
		t.Fatalf("应发 3 条 effect_unknown，实际 %d", n)
	}
	if n := len(capture.byKind(trace.KindEffectRecoveryDecided)); n != 3 {
		t.Fatalf("应发 3 条 effect_recovery_decided，实际 %d", n)
	}
}

// TestRecoverSafeReplayAnnotatedNotExecuted 覆盖：safe_replay（预留策略）
// 仍不自动重跑，只标注可重放（decision=replayable_hold），保持 unknown。
func TestRecoverSafeReplayAnnotatedNotExecuted(t *testing.T) {
	dir := t.TempDir()
	installTraceCapture(t)

	j := openTestJournal(t, dir)
	prepareRaw(t, j, "task-1", KindShell, "cmd:xyz", digestOf([]byte("echo x")), PolicySafeReplay)

	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 1 || decisions[0].Decision != DecisionReplayableHold {
		t.Fatalf("safe_replay 裁决应为 replayable_hold: %+v", decisions)
	}
	if got := j.Query("task-1"); got[0].Status != StatusUnknown {
		t.Fatalf("safe_replay 本轮也不自动重跑，应保持 unknown，实际 %s", got[0].Status)
	}
}

// TestRecoverLeavesSettledUntouched 覆盖：已 settled 的账目不受恢复影响
// （不标 unknown、不产生裁决）。
func TestRecoverLeavesSettledUntouched(t *testing.T) {
	dir := t.TempDir()
	installTraceCapture(t)

	j := openTestJournal(t, dir)
	id := prepareRaw(t, j, "task-1", KindShell, "cmd:ok", digestOf([]byte("echo ok")), PolicyManualOnly)
	if err := j.Settle(id, "exit_code=0"); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 0 {
		t.Fatalf("已 settled 的账目不应有裁决: %+v", decisions)
	}
	if got := j.Query("task-1"); got[0].Status != StatusSettled {
		t.Fatalf("已 settled 的账目不应被动，实际 %s", got[0].Status)
	}
}

func TestRecoverMarkUnknownFailureStillReturnsQuarantineDecision(t *testing.T) {
	dir := t.TempDir()
	capture := installTraceCapture(t)

	j := openTestJournal(t, dir)
	id := prepareRaw(t, j, "task-closed", KindShell, "cmd:closed", digestOf([]byte("echo closed")), PolicyManualOnly)
	// 模拟 recovery 期间 journal 已不可写。prepared Effect 的外部结果
	// 仍然未知，不能因为 MarkUnknown 落账失败就从任务 quarantine 清单漏掉。
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 1 || decisions[0].EffectID != id ||
		decisions[0].Decision != DecisionKeptUnknownUnverifiable {
		t.Fatalf("MarkUnknown 失败也应返回 fail-closed 裁决: %+v", decisions)
	}
	if decisions[0].TaskID != "task-closed" {
		t.Fatalf("quarantine 裁决必须保留 TaskID: %+v", decisions[0])
	}
	if n := len(capture.byKind(trace.KindEffectRecoveryDecided)); n != 1 {
		t.Fatalf("MarkUnknown 失败仍应发 recovery_decided 审计事件，实际 %d", n)
	}
}

// TestRecoverAcrossProcessRestart 覆盖：模拟进程崩溃（journals 只写 prepare
// 后关闭），重启（重新 OpenJournal）后恢复裁决生效。再次 Recover
// 不重复写 unknown 状态，但必须继续返回未解决条目供任务 quarantine。
func TestRecoverAcrossProcessRestart(t *testing.T) {
	dir := t.TempDir()
	installTraceCapture(t)

	content := []byte("崩溃前已落盘")
	target := filepath.Join(dir, "committed.txt")
	if err := os.WriteFile(target, content, 0o644); err != nil {
		t.Fatalf("写目标文件: %v", err)
	}

	func() {
		j, err := OpenJournal(dir)
		if err != nil {
			t.Fatalf("OpenJournal: %v", err)
		}
		defer j.Close() // Windows 纪律：先 Close 再 reopen
		prepareRaw(t, j, "task-1", KindFileWrite, target, digestOf(content), PolicyVerifyFirst)
		prepareRaw(t, j, "task-1", KindShell, "cmd:abc", digestOf([]byte("some cmd")), PolicyManualOnly)
		// 崩溃：没有 Settle 就关闭。
	}()

	j := openTestJournal(t, dir)
	decisions := j.Recover(FileHashVerifier{})
	if len(decisions) != 2 {
		t.Fatalf("重启恢复应有 2 条裁决，实际 %d", len(decisions))
	}
	byKind := map[Kind]RecoveryDecision{}
	for _, d := range decisions {
		byKind[d.Kind] = d
	}
	if byKind[KindFileWrite].Decision != DecisionVerifiedSettled {
		t.Fatalf("文件写应核验一致转 settled: %+v", byKind[KindFileWrite])
	}
	if byKind[KindShell].Decision != DecisionKeptUnknownManual {
		t.Fatalf("shell 应保持 unknown 交人工: %+v", byKind[KindShell])
	}
	// 已 settle 的文件不再返回；未解决 shell 每次启动仍需可见并
	// 影响任务恢复，但 MarkUnknown 自身幂等，不追加第二条 unknown 记录。
	again := j.Recover(FileHashVerifier{})
	if len(again) != 1 || again[0].Kind != KindShell || again[0].Decision != DecisionKeptUnknownManual {
		t.Fatalf("重复 Recover 应持续返回未解决 shell: %+v", again)
	}
}
