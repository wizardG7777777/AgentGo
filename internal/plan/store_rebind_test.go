package plan

import (
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/model"
)

// B2 回归：RebindDir 后持久化位置迁移到新路径，内存态连续不重置；
// 新路径从换绑时刻起即为完整副本，旧路径冻结在换绑时刻。
func TestStoreRebindDir_MovesPersistenceKeepsState(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "sess-a", "plan-state.json")
	newPath := filepath.Join(t.TempDir(), "sess-b", "plan-state.json")

	store, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := NewCoordinator(store, nil)
	createTestPlan(t, c, "p-before", model.PlanBudget{})

	// 换绑：新路径必须立即含有切换前的完整状态
	if err := store.RebindDir(newPath); err != nil {
		t.Fatalf("RebindDir: %v", err)
	}
	reopenedNew, err := OpenStore(newPath)
	if err != nil {
		t.Fatalf("reopen new path: %v", err)
	}
	if _, err := reopenedNew.GetPlan("p-before"); err != nil {
		t.Fatalf("换绑后新路径缺少切换前的 plan: %v", err)
	}

	// 换绑后的变更只落新路径
	createTestPlan(t, c, "p-after", model.PlanBudget{})

	plans, err := store.ListPlans()
	if err != nil || len(plans) != 2 {
		t.Fatalf("内存态应连续含 2 个 plan: plans=%v err=%v", plans, err)
	}
	reopenedNew, err = OpenStore(newPath)
	if err != nil {
		t.Fatalf("reopen new path again: %v", err)
	}
	if _, err := reopenedNew.GetPlan("p-after"); err != nil {
		t.Fatalf("新路径缺少换绑后的变更: %v", err)
	}

	// 旧路径冻结在换绑时刻：只有 p-before，没有 p-after
	reopenedOld, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("reopen old path: %v", err)
	}
	if _, err := reopenedOld.GetPlan("p-before"); err != nil {
		t.Fatalf("旧路径丢失切换前的状态: %v", err)
	}
	if _, err := reopenedOld.GetPlan("p-after"); err == nil {
		t.Fatal("旧路径混入了换绑后的变更——未冻结")
	}
}

// 换绑失败（目标目录不可建）时返回错误，且持久化目标保持旧路径不动。
func TestStoreRebindDir_FailureKeepsOldPath(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "sess-a", "plan-state.json")
	store, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	c := NewCoordinator(store, nil)
	createTestPlan(t, c, "p-keep", model.PlanBudget{})

	// 用一个已存在的【文件】作为父目录分量，使 MkdirAll 必然失败
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}
	badPath := filepath.Join(blocker, "plan-state.json")
	if err := store.RebindDir(badPath); err == nil {
		t.Fatal("RebindDir 到不可写路径应返回错误")
	}

	// 后续变更仍落旧路径
	createTestPlan(t, c, "p-after-failed-rebind", model.PlanBudget{})
	reopenedOld, err := OpenStore(oldPath)
	if err != nil {
		t.Fatalf("reopen old path: %v", err)
	}
	if _, err := reopenedOld.GetPlan("p-after-failed-rebind"); err != nil {
		t.Fatalf("换绑失败后旧路径未继续承担持久化: %v", err)
	}
}
