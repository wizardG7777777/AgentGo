package effect

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// openTestJournal 打开临时目录下的 journal 并登记 Windows 句柄清理
// （先 Close 再让 TempDir 清理——Windows 不给 FILE_SHARE_DELETE）。
func openTestJournal(t *testing.T, dir string) *Journal {
	t.Helper()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

func TestJournalPrepareAssignsSequentialIDs(t *testing.T) {
	j := openTestJournal(t, t.TempDir())

	e1 := Effect{TaskID: "task-a", Kind: KindFileWrite, Target: "/p/a.go", ArgsDigest: "aa", Policy: PolicyVerifyFirst}
	if err := j.Prepare(&e1); err != nil {
		t.Fatalf("Prepare #1: %v", err)
	}
	e2 := Effect{TaskID: "task-a", Kind: KindShell, Target: "cmd:bb", ArgsDigest: "bb", Policy: PolicyManualOnly}
	if err := j.Prepare(&e2); err != nil {
		t.Fatalf("Prepare #2: %v", err)
	}
	e3 := Effect{TaskID: "task-b", Kind: KindMessage, Target: "worker-1", ArgsDigest: "cc", Policy: PolicyManualOnly}
	if err := j.Prepare(&e3); err != nil {
		t.Fatalf("Prepare #3: %v", err)
	}

	// 幂等身份：<taskID>-<seq>，per-task 单调、任务间互不影响。
	if e1.ID != "task-a-1" || e2.ID != "task-a-2" || e3.ID != "task-b-1" {
		t.Fatalf("ID 分配不符幂等身份约定: %s %s %s", e1.ID, e2.ID, e3.ID)
	}
	for _, e := range []*Effect{&e1, &e2, &e3} {
		if e.Status != StatusPrepared {
			t.Fatalf("Prepare 后状态应为 prepared，实际 %s", e.Status)
		}
		if e.PreparedAt.IsZero() {
			t.Fatal("Prepare 应填 PreparedAt")
		}
	}

	got := j.Query("task-a")
	if len(got) != 2 || got[0].ID != "task-a-1" || got[1].ID != "task-a-2" {
		t.Fatalf("Query(task-a) 应按 prepare 顺序返回 2 条: %+v", got)
	}
	if n := len(j.QueryByStatus(StatusPrepared)); n != 3 {
		t.Fatalf("QueryByStatus(prepared) 应为 3 条，实际 %d", n)
	}
}

func TestJournalSettleLifecycle(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	e := Effect{TaskID: "task-1", Kind: KindFileWrite, Target: "/p/a.go", ArgsDigest: "dd", Policy: PolicyVerifyFirst}
	if err := j.Prepare(&e); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := j.Settle(e.ID, "bytes=5 sha256=abc"); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	got := j.Query("task-1")
	if len(got) != 1 {
		t.Fatalf("Query 应返回 1 条，实际 %d", len(got))
	}
	if got[0].Status != StatusSettled {
		t.Fatalf("状态应为 settled，实际 %s", got[0].Status)
	}
	if got[0].ResultSummary != "bytes=5 sha256=abc" {
		t.Fatalf("ResultSummary 未记录: %q", got[0].ResultSummary)
	}
	if got[0].SettledAt.IsZero() {
		t.Fatal("Settle 应填 SettledAt")
	}
	// 重复 settle 拒绝（唯一结果记录者）。
	if err := j.Settle(e.ID, "again"); err == nil {
		t.Fatal("重复 Settle 应报错")
	}
	// settled 后拒绝改标 unknown。
	if err := j.MarkUnknown(e.ID, "late"); err == nil {
		t.Fatal("settled 后 MarkUnknown 应报错")
	}
	// 未落账的 ID 报错。
	if err := j.Settle("task-x-9", "nope"); err == nil {
		t.Fatal("对未 Prepare 的 ID Settle 应报错")
	}
}

func TestJournalMarkUnknownRejectsDuplicateTransition(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	e := Effect{TaskID: "task-1", Kind: KindShell, Target: "cmd:ee", ArgsDigest: "ee", Policy: PolicyManualOnly}
	if err := j.Prepare(&e); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := j.MarkUnknown(e.ID, "命令超时"); err != nil {
		t.Fatalf("MarkUnknown: %v", err)
	}
	got := j.Query("task-1")
	if got[0].Status != StatusUnknown || got[0].UnknownReason != "命令超时" {
		t.Fatalf("unknown 状态/原因未记录: %+v", got[0])
	}
	// 重复 transition 会隐藏上层重复结算缺陷，必须拒绝。
	if err := j.MarkUnknown(e.ID, "命令再次超时"); err == nil {
		t.Fatal("重复 MarkUnknown 应拒绝")
	}
}

func TestJournalReplayRebuildsIndexAndContinuesSeq(t *testing.T) {
	dir := t.TempDir()
	func() {
		j, err := OpenJournal(dir)
		if err != nil {
			t.Fatalf("OpenJournal: %v", err)
		}
		defer j.Close() // Windows 纪律：先 Close 再 reopen
		for _, k := range []Kind{KindFileWrite, KindShell, KindMessage} {
			e := Effect{TaskID: "task-1", Kind: k, Target: string(k), ArgsDigest: "ff", Policy: PolicyVerifyFirst}
			if err := j.Prepare(&e); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
		}
		if err := j.Settle("task-1-1", "bytes=1 sha256=x"); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if err := j.MarkUnknown("task-1-2", "超时"); err != nil {
			t.Fatalf("MarkUnknown: %v", err)
		}
	}()

	j := openTestJournal(t, dir)
	got := j.Query("task-1")
	if len(got) != 3 {
		t.Fatalf("重放后应有 3 条，实际 %d", len(got))
	}
	wantStatus := []Status{StatusSettled, StatusUnknown, StatusPrepared}
	for i, want := range wantStatus {
		if got[i].Status != want {
			t.Fatalf("第 %d 条重放状态应为 %s，实际 %s", i, want, got[i].Status)
		}
	}
	if got[0].ResultSummary != "bytes=1 sha256=x" {
		t.Fatalf("重放后 ResultSummary 丢失: %q", got[0].ResultSummary)
	}
	if got[1].UnknownReason != "超时" {
		t.Fatalf("重放后 UnknownReason 丢失: %q", got[1].UnknownReason)
	}
	// 幂等：进程重启后 effect seq 按账本最大值续号，绝不重号。
	e := Effect{TaskID: "task-1", Kind: KindFileWrite, Target: "/p/b.go", ArgsDigest: "00", Policy: PolicyVerifyFirst}
	if err := j.Prepare(&e); err != nil {
		t.Fatalf("重开后 Prepare: %v", err)
	}
	if e.ID != "task-1-4" {
		t.Fatalf("重启后续号应为 task-1-4，实际 %s", e.ID)
	}
}

func TestJournalReplayFailsClosedOnBrokenAuthorityChain(t *testing.T) {
	now := time.Now().UTC()
	prepared := Effect{
		ID: "task-1-1", TaskID: "task-1", Kind: KindFileWrite,
		Target: "/p/a.go", ArgsDigest: "11", Policy: PolicyVerifyFirst,
		Status: StatusPrepared, PreparedAt: now,
	}
	line := func(rec record) string {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("序列化测试账本行: %v", err)
		}
		return string(data) + "\n"
	}
	prepareLine := line(record{Op: opPrepare, Effect: &prepared})
	tests := map[string]string{
		"JSON 损坏":    "这不是 JSON\n",
		"孤儿 settle":  line(record{Op: opSettle, ID: "ghost-1", At: now}),
		"孤儿 unknown": line(record{Op: opUnknown, ID: "ghost-1", Reason: "unknown", At: now}),
		"未知操作":       line(record{Op: "bogus"}),
		"重复 prepare": prepareLine + prepareLine,
		"重复 settle":  prepareLine + line(record{Op: opSettle, ID: prepared.ID, At: now}) + line(record{Op: opSettle, ID: prepared.ID, At: now}),
		"重复 unknown": prepareLine + line(record{Op: opUnknown, ID: prepared.ID, Reason: "first", At: now}) + line(record{Op: opUnknown, ID: prepared.ID, Reason: "second", At: now}),
		"不连续 seq": func() string {
			gap := prepared
			gap.ID = "task-1-2"
			return line(record{Op: opPrepare, Effect: &gap})
		}(),
		"未知字段": `{"op":"settle","id":"ghost-1","unexpected":true}` + "\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, journalFileName)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("写入测试账本: %v", err)
			}
			if j, err := OpenJournal(dir); err == nil {
				_ = j.Close()
				t.Fatal("损坏权威链应使 OpenJournal fail-closed")
			}
		})
	}
}

func TestJournalClosedErrors(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close 应幂等: %v", err)
	}
	e := Effect{TaskID: "task-1", Kind: KindFileWrite, Target: "/p", ArgsDigest: "x", Policy: PolicyVerifyFirst}
	if err := j.Prepare(&e); err != ErrJournalClosed {
		t.Fatalf("关闭后 Prepare 应返回 ErrJournalClosed，实际 %v", err)
	}
	if err := j.Settle("task-1-1", "s"); err != ErrJournalClosed {
		t.Fatalf("关闭后 Settle 应返回 ErrJournalClosed，实际 %v", err)
	}
	if err := j.MarkUnknown("task-1-1", "r"); err != ErrJournalClosed {
		t.Fatalf("关闭后 MarkUnknown 应返回 ErrJournalClosed，实际 %v", err)
	}
}

func TestJournalPrepareRequiresTaskID(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	e := Effect{Kind: KindFileWrite, Target: "/p", ArgsDigest: "x", Policy: PolicyVerifyFirst}
	if err := j.Prepare(&e); err == nil {
		t.Fatal("缺 TaskID 的 Prepare 应报错（无法分配幂等身份）")
	}
}

func TestJournalFilePermissionIsPrivate(t *testing.T) {
	j := openTestJournal(t, t.TempDir())
	if runtime.GOOS == "windows" {
		return // Windows 的 os.FileMode 不投影 ACL，Chmod 仍在生产路径执行。
	}
	info, err := os.Stat(j.Path())
	if err != nil {
		t.Fatalf("Stat journal: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("effect journal 权限 = %04o，want 0600", got)
	}
}

type faultJournalFile struct {
	shortWrite    bool
	writeErr      error
	syncErr       error
	failWriteCall int
	failSyncCall  int
	writeCalls    int
	syncCalls     int
	closed        bool
}

func (f *faultJournalFile) Write(p []byte) (int, error) {
	f.writeCalls++
	failing := f.failWriteCall == 0 || f.writeCalls == f.failWriteCall
	if failing && f.shortWrite && len(p) > 0 {
		return len(p) - 1, f.writeErr
	}
	if failing {
		return len(p), f.writeErr
	}
	return len(p), nil
}

func (f *faultJournalFile) Sync() error {
	f.syncCalls++
	if f.failSyncCall == 0 || f.syncCalls == f.failSyncCall {
		return f.syncErr
	}
	return nil
}
func (f *faultJournalFile) Close() error {
	f.closed = true
	return nil
}

func TestJournalSettleFsyncFailurePoisonsPreparedAuthority(t *testing.T) {
	file := &faultJournalFile{syncErr: io.ErrUnexpectedEOF, failSyncCall: 2}
	j := newFaultJournal(file)
	t.Cleanup(func() { _ = j.Close() })
	e := Effect{
		TaskID: "task-settle-poison", Kind: KindFileWrite, Target: "/p/a.go",
		ArgsDigest: "abcdef123456", Policy: PolicyVerifyFirst,
	}
	if err := j.Prepare(&e); err != nil {
		t.Fatalf("Prepare 应在首次 fsync 成功: %v", err)
	}
	if err := j.Settle(e.ID, "bytes=1"); !errors.Is(err, ErrJournalPoisoned) {
		t.Fatalf("第二次 fsync 失败应 poison: %v", err)
	}
	if _, err := j.QueryStrict(e.TaskID); !errors.Is(err, ErrJournalPoisoned) {
		t.Fatalf("Settle poison 后严格读应失败: %v", err)
	}
	if _, err := j.RecoverStrict(nil); !errors.Is(err, ErrAuthorityUnavailable) || !errors.Is(err, ErrJournalPoisoned) {
		t.Fatalf("Settle poison 后严格恢复应阻断: %v", err)
	}
}

func newFaultJournal(file journalFile) *Journal {
	return &Journal{
		file: file, path: "fault://effects.jsonl",
		index: make(map[string]*Effect), maxSeq: make(map[string]int),
	}
}

func TestJournalWriteOrSyncFailurePoisonsAllAuthorityAccess(t *testing.T) {
	tests := map[string]*faultJournalFile{
		"短写":       {shortWrite: true},
		"write 错误": {writeErr: io.ErrUnexpectedEOF},
		"fsync 错误": {syncErr: io.ErrUnexpectedEOF},
	}
	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			j := newFaultJournal(file)
			t.Cleanup(func() { _ = j.Close() })
			e := Effect{
				TaskID: "task-poison", Kind: KindFileWrite, Target: "/p/a.go",
				ArgsDigest: "abcdef123456", Policy: PolicyVerifyFirst,
			}
			err := j.Prepare(&e)
			if !errors.Is(err, ErrJournalPoisoned) {
				t.Fatalf("Prepare 应 poison journal: %v", err)
			}
			if e.ID != "" || e.Status != "" || !e.PreparedAt.IsZero() {
				t.Fatalf("落账失败不得向调用方暴露未 durable 身份: %+v", e)
			}
			if err := j.Health(); !errors.Is(err, ErrJournalPoisoned) {
				t.Fatalf("Health 应返回 poison: %v", err)
			}
			if _, err := j.QueryStrict("task-poison"); !errors.Is(err, ErrJournalPoisoned) {
				t.Fatalf("QueryStrict 应在 poison 后失败: %v", err)
			}
			if got := j.Query("task-poison"); got != nil {
				t.Fatalf("legacy Query 不得把 poison 后的局部索引当权威值: %+v", got)
			}
			if err := j.Settle("task-poison-1", "done"); !errors.Is(err, ErrJournalPoisoned) {
				t.Fatalf("poison 后 Settle 应稳定失败: %v", err)
			}
			if err := j.MarkUnknown("task-poison-1", "unknown"); !errors.Is(err, ErrJournalPoisoned) {
				t.Fatalf("poison 后 MarkUnknown 应稳定失败: %v", err)
			}
		})
	}
}

func TestRequireJournalIsExplicitProductionAssemblyGate(t *testing.T) {
	if err := RequireJournal(nil); !errors.Is(err, ErrAuthorityUnavailable) || !errors.Is(err, ErrJournalRequired) {
		t.Fatalf("nil Journal 应返回 typed 装配错误: %v", err)
	}
	j := openTestJournal(t, t.TempDir())
	if err := RequireJournal(j); err != nil {
		t.Fatalf("健康 Journal 应通过装配校验: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := RequireJournal(j); !errors.Is(err, ErrAuthorityUnavailable) || !errors.Is(err, ErrJournalClosed) {
		t.Fatalf("已关闭 Journal 不得通过生产装配: %v", err)
	}
}
