package team

import (
	"os"
	"path/filepath"
	"testing"
)

// B2 回归：RebindDir 后持久化位置迁移到新路径，内存态连续不重置；
// 新路径从换绑时刻起即为完整副本，旧路径冻结在换绑时刻。
func TestStoreRebindDir_MovesPersistenceKeepsState(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "sess-a", "agent-teams.json")
	newPath := filepath.Join(t.TempDir(), "sess-b", "agent-teams.json")

	store, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	spec := testSpec("team-a", "controller-a", "investigate")
	if _, created, err := store.Ensure(spec); err != nil || !created {
		t.Fatalf("Ensure: created=%v err=%v", created, err)
	}

	// 换绑：新路径必须立即含有切换前的完整状态
	if err := store.RebindDir(newPath); err != nil {
		t.Fatalf("RebindDir: %v", err)
	}
	reopenedNew, err := OpenStore(newPath)
	if err != nil {
		t.Fatalf("reopen new path: %v", err)
	}
	got, err := reopenedNew.Get(spec.ID)
	if err != nil || got.Status != StatusReady {
		t.Fatalf("换绑后新路径缺少切换前的 team: got=%+v err=%v", got, err)
	}

	// 换绑后的变更只落新路径
	if _, err := store.SetStatus(spec.ID, StatusStopped, "controller_terminal:completed"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, err = store.Get(spec.ID)
	if err != nil || got.Status != StatusStopped {
		t.Fatalf("内存态应连续反映换绑后的变更: got=%+v err=%v", got, err)
	}
	reopenedNew, err = OpenStore(newPath)
	if err != nil {
		t.Fatalf("reopen new path again: %v", err)
	}
	got, err = reopenedNew.Get(spec.ID)
	if err != nil || got.Status != StatusStopped || got.StopReason != "controller_terminal:completed" {
		t.Fatalf("新路径缺少换绑后的变更: got=%+v err=%v", got, err)
	}

	// 旧路径冻结在换绑时刻：仍是 StatusReady
	reopenedOld, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("reopen old path: %v", err)
	}
	got, err = reopenedOld.Get(spec.ID)
	if err != nil || got.Status != StatusReady {
		t.Fatalf("旧路径未冻结在换绑时刻: got=%+v err=%v", got, err)
	}
}

// B2 回归：RebindDir 到新路径失败时保留旧路径，后续变更仍落旧路径
// （此前 team 侧缺这条失败路径覆盖）。
func TestStoreRebindDir_FailureKeepsOldPath(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "sess-a", "agent-teams.json")
	store, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	spec := testSpec("team-keep", "controller-a", "investigate")
	if _, created, err := store.Ensure(spec); err != nil || !created {
		t.Fatalf("Ensure: created=%v err=%v", created, err)
	}

	// 用一个已存在的【文件】作为父目录分量，使 MkdirAll 必然失败
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}
	badPath := filepath.Join(blocker, "agent-teams.json")
	if err := store.RebindDir(badPath); err == nil {
		t.Fatal("RebindDir 到不可写路径应返回错误")
	}

	// 后续变更仍落旧路径
	if _, err := store.SetStatus(spec.ID, StatusStopped, "after_failed_rebind"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	reopenedOld, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("reopen old path: %v", err)
	}
	got, err := reopenedOld.Get(spec.ID)
	if err != nil || got.Status != StatusStopped {
		t.Fatalf("换绑失败后旧路径未继续承担持久化: got=%+v err=%v", got, err)
	}
}
