package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/memory"
	"agentgo/internal/scheduler"
	"agentgo/internal/session"
)

// newSessionMemoryFixture 构造 wireSessionMemory 所需的最小 System：
// 真实 SessionManager（TempDir）+ 仅携带共享 Memory 的 scheduler Bundle。
func newSessionMemoryFixture(t *testing.T) (*System, *memory.ProcessStore, *session.SessionManager) {
	t.Helper()
	sm, err := session.NewSessionManager(filepath.Join(t.TempDir(), "sessions"), session.SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	if sm.Current() == nil {
		t.Fatalf("SessionManager 应有当前 Session")
	}
	proc := memory.NewProcessStore()
	sys := &System{
		SessionMgr: sm,
		Scheduler: &scheduler.Bundle{
			Agent: &agent.Agent{Memory: proc},
		},
	}
	return sys, proc, sm
}

// TestWireSessionMemory_AttachesBackend 验证 resume 挂点把 SessionStore 挂到
// 共享 ProcessStore 上，且文件落在当前 Session 目录（与 snapshot.json 同位置）。
func TestWireSessionMemory_AttachesBackend(t *testing.T) {
	sys, proc, sm := newSessionMemoryFixture(t)

	wireSessionMemory(sys)

	backend := proc.SessionBackend()
	if backend == nil {
		t.Fatalf("挂接后 ProcessStore 应有 Session 后端")
	}
	t.Cleanup(func() { _ = backend.Close() })
	wantPath := filepath.Join(sm.Current().Dir, "memory.jsonl")
	if backend.Path() != wantPath {
		t.Errorf("后端路径=%q, want %q", backend.Path(), wantPath)
	}

	// 经路由写入一条 Session 记忆，验证真实落盘到 sess-<id>/memory.jsonl。
	ctx := context.Background()
	if err := proc.Put(ctx, memory.Entry{
		Scope: memory.ScopeSession, Kind: memory.KindLearning,
		Key: "lesson", Content: "会话级经验", Source: "worker-1",
	}); err != nil {
		t.Fatalf("Put session entry: %v", err)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("memory.jsonl 应已创建在 Session 目录: %v", err)
	}
	entries, err := proc.Query(ctx, memory.ScopeSession, memory.KindLearning, "lesson", 1)
	if err != nil || len(entries) != 1 || entries[0].Source != "worker-1" {
		t.Errorf("Query 经路由返回不符: entries=%+v err=%v", entries, err)
	}
}

// TestWireSessionMemory_RestoresAcrossReopen 验证同一 Session 目录重开时
// memory.jsonl 内容被重放（模拟进程重启 resume 路径）。
func TestWireSessionMemory_RestoresAcrossReopen(t *testing.T) {
	sys, proc, _ := newSessionMemoryFixture(t)
	ctx := context.Background()

	wireSessionMemory(sys)
	backend := proc.SessionBackend()
	if backend == nil {
		t.Fatalf("挂接失败")
	}
	if err := proc.Put(ctx, memory.Entry{
		Scope: memory.ScopeSession, Kind: memory.KindPattern,
		Key: "pattern", Content: "重启前的模式", Source: "scheduler",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close backend: %v", err)
	}

	// 模拟重启后的第二次挂接：同一 Session 目录。
	proc2 := memory.NewProcessStore()
	sys.Scheduler.Agent.Memory = proc2
	wireSessionMemory(sys)
	backend2 := proc2.SessionBackend()
	if backend2 == nil {
		t.Fatalf("第二次挂接失败")
	}
	t.Cleanup(func() { _ = backend2.Close() })

	entries, err := proc2.Query(ctx, memory.ScopeSession, memory.KindPattern, "pattern", 1)
	if err != nil || len(entries) != 1 || entries[0].Content != "重启前的模式" {
		t.Errorf("重开后应恢复 memory.jsonl 内容: entries=%+v err=%v", entries, err)
	}
}

// TestWireSessionMemory_DegradesWithoutSession 覆盖全部降级路径：
// 无 SessionMgr / 无当前 Session / 无 Scheduler / Memory 非 *ProcessStore。
// 降级时不得 panic、不得改变 Process 作用域现状行为。
func TestWireSessionMemory_DegradesWithoutSession(t *testing.T) {
	ctx := context.Background()

	// nil System / 无 SessionMgr
	wireSessionMemory(nil)
	wireSessionMemory(&System{})

	// 有 SessionMgr 但无 Scheduler
	sm, err := session.NewSessionManager(filepath.Join(t.TempDir(), "sessions"), session.SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	wireSessionMemory(&System{SessionMgr: sm})

	// 有 Scheduler 但 Memory 为 nil（断言失败 → 降级）
	sys := &System{SessionMgr: sm, Scheduler: &scheduler.Bundle{Agent: &agent.Agent{}}}
	wireSessionMemory(sys)

	// 降级后 Process 作用域行为不变：可写，Session 作用域仍拒绝。
	proc := memory.NewProcessStore()
	if err := proc.Put(ctx, memory.Entry{Scope: memory.ScopeProcess, Kind: memory.KindContext, Key: "k", Content: "v"}); err != nil {
		t.Errorf("降级后 Process Put 不应受影响: %v", err)
	}
	if err := proc.Put(ctx, memory.Entry{Scope: memory.ScopeSession, Kind: memory.KindLearning, Key: "k", Content: "v"}); err == nil {
		t.Errorf("未挂接时 Session Put 应仍被拒绝")
	}
}
