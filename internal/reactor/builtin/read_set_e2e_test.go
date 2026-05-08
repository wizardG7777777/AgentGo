package builtin

import (
	"path/filepath"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// TestReadSetWrite_E2E_TraceEmitToGateContinue 是 v5 Phase 6 的端到端测试：
// trace.Emit(KindToolResult tool=read_file) → Dispatcher → ReadSetWriteReactor →
// store.UpsertReadSet → 后续 require-read-before-write Gate 查询 ReadSet 通过。
//
// 关键路径：本测试不导入 hook/builtin 包（避免循环），改为直接断言 store 中
// 出现 ReadSet 条目，效果等价（Gate 实现就是查 store.GetReadSet）。
//
// 这是"反查日志反模式"治理的最强证据：写入侧（Reactor 异步）与查询侧
// （Gate 同步）通过 store.ReadSet 显式状态完全解耦。
func TestReadSetWrite_E2E_TraceEmitToGateContinue(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "e2e read-set-write"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	// 注册 Reactor + 设置全局 dispatcher
	reg := reactor.NewRegistry()
	if err := reg.Register(NewReadSetWriteReactor(taskStore)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	// 模拟主流程：发了一次 read_file 成功的 KindToolResult 事件
	target, _ := filepath.Abs("/proj/foo.go")
	trace.Emit(trace.Event{
		Kind:    trace.KindToolResult,
		TaskID:  task.ID,
		AgentID: "agent-1",
		Tool:    "read_file",
		Args:    map[string]any{"path": target},
	})

	// Async Reactor 等待写入落地
	deadline := time.After(500 * time.Millisecond)
	for {
		readSet, err := taskStore.GetReadSet(task.ID)
		if err != nil {
			t.Fatalf("GetReadSet: %v", err)
		}
		if _, ok := readSet[target]; ok {
			return // 通过：Reactor 已写入 ReadSet
		}
		select {
		case <-deadline:
			t.Fatalf("ReadSet not written within 500ms (have keys=%v)", keysOf(readSet))
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestReadSetWrite_E2E_FailedReadDoesNotWrite 验证 Reactor filter 链路：
// 失败的 read_file 事件不写入 ReadSet —— 这是 v4 决议（Success=false 不计入）
// 在 v5 Phase 6 架构下的等价表达（Reactor 在 Run 内 filter ev.Error）。
func TestReadSetWrite_E2E_FailedReadDoesNotWrite(t *testing.T) {
	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 100, 2, 300)
	task := &model.Task{Description: "e2e failed read"}
	if err := taskStore.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	reg := reactor.NewRegistry()
	if err := reg.Register(NewReadSetWriteReactor(taskStore)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	originalDispatcher := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(reg)
	t.Cleanup(func() { trace.SetDefaultDispatcher(originalDispatcher) })

	target, _ := filepath.Abs("/proj/oops.go")
	trace.Emit(trace.Event{
		Kind:    trace.KindToolResult,
		TaskID:  task.ID,
		AgentID: "agent-1",
		Tool:    "read_file",
		Args:    map[string]any{"path": target},
		Error:   "permission denied",
	})

	// 给 Reactor goroutine 时间落地（即使 filter 后什么也没写）
	time.Sleep(100 * time.Millisecond)

	readSet, err := taskStore.GetReadSet(task.ID)
	if err != nil {
		t.Fatalf("GetReadSet: %v", err)
	}
	if len(readSet) != 0 {
		t.Errorf("failed read should not write ReadSet, got %d entries: %v", len(readSet), keysOf(readSet))
	}
}

func keysOf(m map[string]model.ReadInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
