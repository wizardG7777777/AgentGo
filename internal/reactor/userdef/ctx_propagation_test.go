package userdef

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"agentgo/internal/reactor"
	"agentgo/internal/spawn"
	"agentgo/internal/trace"
)

// E4：用户 reactor 动作必须把 Registry 派生的可取消 ctx 一路传到 LLM 调用与
// Spawn 调用——Quiesce 取消该 ctx 时，在途动作随之中断，不会在 Shutdown 关闭
// store/trace 后继续写已关闭资源。

type ctxMarkerKey struct{}

// ctxCaptureLLM 记录 Complete 收到的 ctx；block=true 时阻塞到 ctx 取消
// （模拟一次在途 LLM 调用）。
type ctxCaptureLLM struct {
	mu     sync.Mutex
	gotCtx context.Context
	block  bool
}

func (l *ctxCaptureLLM) Complete(ctx context.Context, _ string) (string, error) {
	l.mu.Lock()
	l.gotCtx = ctx
	l.mu.Unlock()
	if l.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "ok-output", nil
}

func (l *ctxCaptureLLM) captured() context.Context {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gotCtx
}

// ctxCaptureHost 记录 Spawn 收到的 ctx；block=true 时阻塞到 ctx 取消。
type ctxCaptureHost struct {
	mu     sync.Mutex
	gotCtx context.Context
	block  bool
}

func (h *ctxCaptureHost) Spawn(ctx context.Context, _ spawn.SpawnRequest) (string, string, error) {
	h.mu.Lock()
	h.gotCtx = ctx
	h.mu.Unlock()
	if h.block {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	return "spawn-id", "task-id", nil
}

func (h *ctxCaptureHost) captured() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gotCtx
}

func loadInvokeLLMForCtxTest(t *testing.T, llm LLMCompleter) []reactor.Reactor {
	t.Helper()
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "Summarize ${event.task.id}")
	yamlData := []byte(`
reactors:
  - name: ctx-probe
    on: task_failed
    invoke_llm:
      prompt:
        file: ./p.md
      output:
        write_file:
          path: ./out.txt
`)
	rs, err := Load(yamlData, dir, dir, Deps{LLM: llm})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return rs
}

// invoke_llm：RunWithContext 传入的 ctx 必须（经 llmTimeout 派生后）到达
// LLMCompleter——父 ctx 的 value 保留即证明是同一条派生链。
func TestInvokeLLM_RunWithContext_PropagatesCtxToLLM(t *testing.T) {
	llm := &ctxCaptureLLM{}
	rs := loadInvokeLLMForCtxTest(t, llm)

	cr, ok := rs[0].(reactor.CtxReactor)
	if !ok {
		t.Fatalf("%T 未实现 reactor.CtxReactor（E4 要求用户动作接收派生 ctx）", rs[0])
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxMarkerKey{}, "marker"))
	defer cancel()
	if err := cr.RunWithContext(ctx, trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1"}); err != nil {
		t.Fatalf("RunWithContext: %v", err)
	}
	got := llm.captured()
	if got == nil {
		t.Fatal("LLM 未被调用")
	}
	if got.Value(ctxMarkerKey{}) != "marker" {
		t.Fatal("LLM 收到的不是调用方 ctx 的派生 ctx（value 丢失，派生链断裂）")
	}
}

// invoke_llm：取消父 ctx 必须打断阻塞中的 LLM 调用，错误沿动作层透出。
func TestInvokeLLM_RunWithContext_CancelInterruptsLLM(t *testing.T) {
	llm := &ctxCaptureLLM{block: true}
	rs := loadInvokeLLMForCtxTest(t, llm)

	cr, ok := rs[0].(reactor.CtxReactor)
	if !ok {
		t.Fatalf("%T 未实现 reactor.CtxReactor（E4 要求用户动作接收派生 ctx）", rs[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- cr.RunWithContext(ctx, trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1"})
	}()
	// 等 LLM 进入阻塞
	deadline := time.Now().Add(2 * time.Second)
	for llm.captured() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if llm.captured() == nil {
		t.Fatal("LLM 未在界内被调用")
	}
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望 context.Canceled 透出，got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消未打断阻塞中的 LLM 调用")
	}
}

// spawn_agent：SpawnHost.Spawn 必须收到派生 ctx（静态 description 路径）。
func TestSpawnAgent_RunWithContext_PropagatesCtxToSpawn(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "desc.md", "Investigate ${event.task.id}")
	yamlData := []byte(`
reactors:
  - name: spawn-ctx-probe
    on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          file: ./desc.md
      lifecycle: one_shot
`)
	host := &ctxCaptureHost{}
	rs, err := Load(yamlData, dir, dir, Deps{
		SpawnHost:      host,
		KindEventTypes: map[string]string{"explorer": "explore"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cr, ok := rs[0].(reactor.CtxReactor)
	if !ok {
		t.Fatalf("%T 未实现 reactor.CtxReactor（E4 要求用户动作接收派生 ctx）", rs[0])
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxMarkerKey{}, "marker"))
	defer cancel()
	if err := cr.RunWithContext(ctx, trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1"}); err != nil {
		t.Fatalf("RunWithContext: %v", err)
	}
	got := host.captured()
	if got == nil {
		t.Fatal("Spawn 未被调用")
	}
	if got.Value(ctxMarkerKey{}) != "marker" {
		t.Fatal("Spawn 收到的不是调用方 ctx 的派生 ctx（value 丢失，派生链断裂）")
	}
}

// spawn_agent：via_translator 的 reactor 自带 LLM 调用同样走派生 ctx。
func TestSpawnAgent_RunWithContext_TranslatorReceivesDerivedCtx(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "tx.md", "Reword: task=${event.task.id}")
	yamlData := []byte(`
reactors:
  - name: spawn-tx-ctx-probe
    on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          via_translator:
            translator_prompt: { file: ./tx.md }
      lifecycle: one_shot
`)
	llm := &ctxCaptureLLM{}
	host := &ctxCaptureHost{}
	rs, err := Load(yamlData, dir, dir, Deps{
		SpawnHost:      host,
		LLM:            llm,
		KindEventTypes: map[string]string{"explorer": "explore"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cr, ok := rs[0].(reactor.CtxReactor)
	if !ok {
		t.Fatalf("%T 未实现 reactor.CtxReactor（E4 要求用户动作接收派生 ctx）", rs[0])
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxMarkerKey{}, "marker"))
	defer cancel()
	if err := cr.RunWithContext(ctx, trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1"}); err != nil {
		t.Fatalf("RunWithContext: %v", err)
	}
	if got := llm.captured(); got == nil || got.Value(ctxMarkerKey{}) != "marker" {
		t.Fatal("translator LLM 未收到派生 ctx")
	}
	if got := host.captured(); got == nil || got.Value(ctxMarkerKey{}) != "marker" {
		t.Fatal("Spawn 未收到派生 ctx")
	}
}

// spawn_agent：取消父 ctx 必须打断阻塞中的 Spawn 调用。
func TestSpawnAgent_RunWithContext_CancelInterruptsSpawn(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "desc.md", "Investigate ${event.task.id}")
	yamlData := []byte(`
reactors:
  - name: spawn-cancel-probe
    on: task_failed
    spawn_agent:
      base_kind: explorer
      initial_task:
        description:
          file: ./desc.md
      lifecycle: one_shot
`)
	host := &ctxCaptureHost{block: true}
	rs, err := Load(yamlData, dir, dir, Deps{
		SpawnHost:      host,
		KindEventTypes: map[string]string{"explorer": "explore"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cr, ok := rs[0].(reactor.CtxReactor)
	if !ok {
		t.Fatalf("%T 未实现 reactor.CtxReactor（E4 要求用户动作接收派生 ctx）", rs[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- cr.RunWithContext(ctx, trace.Event{Kind: trace.KindTaskFailed, TaskID: "T-1"})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for host.captured() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if host.captured() == nil {
		t.Fatal("Spawn 未在界内被调用")
	}
	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望 context.Canceled 透出，got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消未打断阻塞中的 Spawn 调用")
	}
}
