package effect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestJournalMarkUnknownIdempotent(t *testing.T) {
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
	// 重复标记幂等——不产生重复账本行。
	if err := j.MarkUnknown(e.ID, "命令超时"); err != nil {
		t.Fatalf("重复 MarkUnknown 应幂等: %v", err)
	}
	data, err := os.ReadFile(j.Path())
	if err != nil {
		t.Fatalf("读取账本: %v", err)
	}
	if n := strings.Count(string(data), `"op":"unknown"`); n != 1 {
		t.Fatalf("unknown 账本行应只有 1 行，实际 %d", n)
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

func TestJournalReplayToleratesCorruptLines(t *testing.T) {
	dir := t.TempDir()
	func() {
		j, err := OpenJournal(dir)
		if err != nil {
			t.Fatalf("OpenJournal: %v", err)
		}
		defer j.Close()
		e := Effect{TaskID: "task-1", Kind: KindFileWrite, Target: "/p/a.go", ArgsDigest: "11", Policy: PolicyVerifyFirst}
		if err := j.Prepare(&e); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
	}()
	// 追加损坏行与孤儿 settle 行。
	f, err := os.OpenFile(filepath.Join(dir, journalFileName), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开账本追加: %v", err)
	}
	if _, err := f.WriteString("这不是 JSON\n{\"op\":\"settle\",\"id\":\"ghost-1\"}\n{\"op\":\"bogus\"}\n"); err != nil {
		t.Fatalf("写损坏行: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭账本: %v", err)
	}

	j := openTestJournal(t, dir)
	got := j.Query("task-1")
	if len(got) != 1 || got[0].Status != StatusPrepared {
		t.Fatalf("损坏行容错后应只重建 1 条 prepared: %+v", got)
	}
	// 孤儿 settle 不影响续号。
	e := Effect{TaskID: "task-1", Kind: KindFileEdit, Target: "/p/a.go", ArgsDigest: "22", Policy: PolicyVerifyFirst}
	if err := j.Prepare(&e); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if e.ID != "task-1-2" {
		t.Fatalf("续号应为 task-1-2，实际 %s", e.ID)
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
