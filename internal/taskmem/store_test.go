package taskmem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStore_SaveLoadRoundtrip：Save/LoadOrCreate 往返保持内容；进程重启
// （新建 Store 实例、索引为空）后从磁盘恢复。
func TestStore_SaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	m, created, err := s.LoadOrCreate("task-abc")
	if err != nil || !created {
		t.Fatalf("首次 LoadOrCreate = (created=%v, err=%v), want (true, nil)", created, err)
	}
	m.Goal = "写一份报告"
	ApplyTurn(m, TurnFacts{FilesWritten: []FileWrittenFact{{Path: "a.go", Hash: "h1"}}})
	if err := s.Save(m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-abc.json")); err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}

	// 模拟进程重启：新 Store（索引为空）→ 从磁盘恢复，created=false。
	s2 := NewStore(dir)
	m2, created2, err := s2.LoadOrCreate("task-abc")
	if err != nil || created2 {
		t.Fatalf("恢复 LoadOrCreate = (created=%v, err=%v), want (false, nil)", created2, err)
	}
	if m2.Goal != "写一份报告" || m2.Version != m.Version || len(m2.Files) != 1 || m2.Files[0].Hash != "h1" {
		t.Errorf("恢复内容不符: %+v", m2)
	}

	// 删除后再次 LoadOrCreate 回到新建语义。
	if err := s2.Delete("task-abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, created3, err := s2.LoadOrCreate("task-abc"); err != nil || !created3 {
		t.Errorf("删除后 LoadOrCreate = (created=%v, err=%v), want (true, nil)", created3, err)
	}
}

// TestStore_CorruptedFileDegrades：损坏文件降级新建（不返回错误），
// 下一次 Save 覆盖坏文件。
func TestStore_CorruptedFileDegrades(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "task-bad.json")
	if err := os.WriteFile(bad, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	m, created, err := s.LoadOrCreate("task-bad")
	if err != nil {
		t.Fatalf("损坏文件不应返回 IO 错误, got %v", err)
	}
	if !created {
		t.Error("损坏文件应降级新建（created=true）")
	}
	m.Goal = "重建"
	if err := s.Save(m); err != nil {
		t.Fatalf("Save 覆盖坏文件: %v", err)
	}
	// 再恢复应为正常内容。
	s2 := NewStore(dir)
	m2, created2, err := s2.LoadOrCreate("task-bad")
	if err != nil || created2 || m2.Goal != "重建" {
		t.Errorf("覆盖后恢复 = (%+v, created=%v, err=%v), want Goal=重建/false/nil", m2, created2, err)
	}
}

// TestStore_UnwritableDirReturnsError：目录不可写（路径被文件占用）时
// Save 返回错误——调用方据此降级，不 panic。
func TestStore_UnwritableDirReturnsError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(filepath.Join(blocker, "taskmem")) // 父路径是文件 → MkdirAll 失败
	m := New("task-x")
	m.Goal = "g"
	if err := s.Save(m); err == nil {
		t.Fatal("目录不可写时 Save 应返回错误")
	}
}

// TestStore_PathSanitize：异常任务 ID 被文件名安全化，不产生路径穿越。
func TestStore_PathSanitize(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	m, _, err := s.LoadOrCreate("../evil/../../etc")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := s.Save(m); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("安全化落盘文件不符: %+v", entries)
	}
	if strings.Contains(entries[0].Name(), "..") || strings.ContainsRune(entries[0].Name(), '/') {
		t.Errorf("文件名含穿越字符: %s", entries[0].Name())
	}
}

func TestStore_LoadReturnsDeepSnapshot(t *testing.T) {
	s := NewStore(t.TempDir())
	m, _, err := s.LoadOrCreate("task-copy")
	if err != nil {
		t.Fatal(err)
	}
	m.Goal = "stored"
	m.Constraints = []string{"c1"}
	m.Facts = []Fact{{Text: "fact", Evidence: []EvidenceRef{{Kind: EvidenceStatus, Ref: "completed"}}}}
	if err := s.Save(m); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Load("task-copy")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Goal = "caller-mutated"
	loaded.Constraints[0] = "changed"
	loaded.Facts[0].Evidence[0].Ref = "changed"

	again, err := s.Load("task-copy")
	if err != nil {
		t.Fatal(err)
	}
	if again.Goal != "stored" || again.Constraints[0] != "c1" || again.Facts[0].Evidence[0].Ref != "completed" {
		t.Fatalf("Load 不得泄露 Store 内部可变指针: %+v", again)
	}
}

func TestStore_WaitSealedReturnsCheckpointedSnapshot(t *testing.T) {
	s := NewStore(t.TempDir())
	m, _, err := s.LoadOrCreate("task-seal")
	if err != nil {
		t.Fatal(err)
	}
	m.Goal = "before"
	if err := s.Save(m); err != nil {
		t.Fatal(err)
	}

	resultCh := make(chan *TaskMemory, 1)
	errCh := make(chan error, 1)
	go func() {
		sealed, waitErr := s.WaitSealed("task-seal", time.Second)
		resultCh <- sealed
		errCh <- waitErr
	}()

	select {
	case <-resultCh:
		t.Fatal("WaitSealed 不得在终态 checkpoint 前返回")
	case <-time.After(20 * time.Millisecond):
	}
	m.Goal = "terminal"
	m.Sealed = true
	if err := s.Save(m); err != nil {
		t.Fatal(err)
	}
	sealed := <-resultCh
	if waitErr := <-errCh; waitErr != nil {
		t.Fatal(waitErr)
	}
	if sealed == nil || !sealed.Sealed || sealed.Goal != "terminal" {
		t.Fatalf("WaitSealed 应返回已落盘终态快照: %+v", sealed)
	}
}
