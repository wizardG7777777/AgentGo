package bootstrap

import (
	"testing"
	"time"

	"agentgo/internal/output"
)

// A1 修复（done 逃生口 + Shutdown drainer）+ A4 修复（chanWriter → eventWriter
// 类型化事件）的回归测试。
// 背景：TUI forwarder 退出后 outputCh 无人消费，eventWriter 阻塞写会让 agent
// 卡死、System.Shutdown 的 wg.Wait() 挂死进程（H5）。

// done 关闭后 Write 必须立即返回成功（len(p), nil）——逃逸路径不能阻塞、
// 不能报错（fmt.Fprintf 调用方拿到 error 会进入重试循环）。
func TestEventWriter_WriteAfterDoneClosed(t *testing.T) {
	ch := make(chan output.Event) // 无缓冲 + 无消费者：不看 done 必然永久阻塞
	done := make(chan struct{})
	close(done)

	onResultCalls := 0
	w := &eventWriter{ch: ch, done: done, kind: output.KindResult, agentID: "a1",
		onResult: func(string) { onResultCalls++ }}

	writeDone := make(chan struct{})
	var n int
	var err error
	go func() {
		defer close(writeDone)
		n, err = w.Write([]byte("hello"))
	}()

	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("done 关闭后 Write 仍阻塞")
	}
	if err != nil {
		t.Fatalf("逃逸路径应返回 nil error，got %v", err)
	}
	if n != len("hello") {
		t.Errorf("逃逸路径应返回 len(p)=%d，got %d", len("hello"), n)
	}
	if onResultCalls != 0 {
		t.Errorf("文本未入队时不应触发 onResult，got %d 次", onResultCalls)
	}
}

// 缓冲打满 + 无消费者时关闭 done，后续 Write 立即返回——H5 死锁场景的直接回归。
func TestEventWriter_FullChannelEscapeViaDone(t *testing.T) {
	const capacity = 4
	ch := make(chan output.Event, capacity)
	done := make(chan struct{})
	onResultCalls := 0
	w := &eventWriter{ch: ch, done: done, kind: output.KindResult,
		onResult: func(string) { onResultCalls++ }}

	// 打满缓冲（缓冲未满时 Write 正常入队，onResult 逐条记账）
	for i := 0; i < capacity; i++ {
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
	}
	if onResultCalls != capacity {
		t.Fatalf("onResult 调用 = %d，want %d", onResultCalls, capacity)
	}

	close(done)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = w.Write([]byte("overflow"))
	}()

	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("channel 已满且 done 已关闭，Write 仍阻塞（H5 死锁未修复）")
	}
	if onResultCalls != capacity {
		t.Errorf("逃逸路径不应触发 onResult，got %d 次，want %d", onResultCalls, capacity)
	}
}

// TUI 存活（done 未关闭）时必须保持阻塞背压语义——正常运行的行为不允许改变。
func TestEventWriter_BlocksWhileAlive(t *testing.T) {
	ch := make(chan output.Event, 1)
	done := make(chan struct{})
	defer close(done)

	var results []string
	w := &eventWriter{ch: ch, done: done, kind: output.KindResult, agentID: "sched-1",
		onResult: func(s string) { results = append(results, s) }}

	if _, err := w.Write([]byte("first")); err != nil { // 占满缓冲
		t.Fatalf("first Write: %v", err)
	}

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = w.Write([]byte("second"))
	}()

	// done 未关闭、无消费者：Write 应阻塞在背压上
	select {
	case <-writeDone:
		t.Fatal("done 未关闭时 Write 应保持阻塞背压，不允许丢弃输出")
	case <-time.After(100 * time.Millisecond):
	}

	// 消费者读走一条后，被阻塞的 Write 完成
	if ev := <-ch; ev.Text != "first" {
		t.Fatalf("ch[0].Text = %q, want first", ev.Text)
	}
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("消费者就绪后 Write 应完成")
	}
	if ev := <-ch; ev.Text != "second" {
		t.Fatalf("ch[1].Text = %q, want second", ev.Text)
	}
	if len(results) != 2 || results[0] != "first" || results[1] != "second" {
		t.Errorf("onResult 应逐条记录入队文本，got %v", results)
	}
}

// kind/agentID 标记：文本写产出 KindText 事件且不触发 onResult；
// 结果写产出 KindResult 事件且触发 onResult——分类在产生处完成（A4）。
func TestEventWriter_TagsKindAndAgentID(t *testing.T) {
	ch := make(chan output.Event, 4)
	done := make(chan struct{})
	defer close(done)

	var results []string
	textW := &eventWriter{ch: ch, done: done, kind: output.KindText, agentID: "worker-1",
		onResult: func(s string) { results = append(results, s) }}
	resultW := &eventWriter{ch: ch, done: done, kind: output.KindResult, agentID: "scheduler",
		onResult: func(s string) { results = append(results, s) }}
	// nil onResult 的结果写不应 panic（向后兼容单 Writer 装配）
	bareResultW := &eventWriter{ch: ch, done: done, kind: output.KindResult, agentID: "bare"}

	if _, err := textW.Write([]byte("progress note")); err != nil {
		t.Fatalf("text Write: %v", err)
	}
	ev := <-ch
	if ev.Kind != output.KindText || ev.AgentID != "worker-1" || ev.Text != "progress note" {
		t.Errorf("文本事件 = %+v，want {KindText worker-1 progress note}", ev)
	}
	if len(results) != 0 {
		t.Errorf("KindText 写不应触发 onResult，got %v", results)
	}

	if _, err := resultW.Write([]byte("final block")); err != nil {
		t.Fatalf("result Write: %v", err)
	}
	ev = <-ch
	if ev.Kind != output.KindResult || ev.AgentID != "scheduler" || ev.Text != "final block" {
		t.Errorf("结果事件 = %+v，want {KindResult scheduler final block}", ev)
	}
	if len(results) != 1 || results[0] != "final block" {
		t.Errorf("KindResult 写应恰好触发一次 onResult，got %v", results)
	}

	if _, err := bareResultW.Write([]byte("bare result")); err != nil {
		t.Fatalf("bare result Write: %v", err)
	}
	ev = <-ch
	if ev.Kind != output.KindResult || ev.AgentID != "bare" {
		t.Errorf("bare 结果事件 = %+v，want Kind=KindResult AgentID=bare", ev)
	}
	if len(results) != 1 {
		t.Errorf("nil onResult 的写不应触发回调，got %v", results)
	}
}

// drainer：队列打满后启动 drain 循环，已排队条目被消费、被阻塞的写者被放行；
// stop 关闭后 drainer 自身退出（不泄漏 goroutine）。
func TestDrainOutputChannels_DrainsAndUnblocksWriter(t *testing.T) {
	outputCh := make(chan output.Event, 2)
	statusCh := make(chan string, 2)
	outputCh <- output.Event{Kind: output.KindText, Text: "o1"}
	outputCh <- output.Event{Kind: output.KindText, Text: "o2"} // 打满
	statusCh <- "s1"

	stop := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		drainOutputChannels(outputCh, statusCh, stop)
	}()

	// 缓冲已满时被阻塞的写者应在 drainer 消费后被放行（超时守护）
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		outputCh <- output.Event{Kind: output.KindResult, Text: "o3"}
	}()
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("drainer 运行中，阻塞写者未被放行")
	}

	// drainer 持续丢弃，两个队列最终都应清空
	deadline := time.After(2 * time.Second)
	for len(outputCh) > 0 || len(statusCh) > 0 {
		select {
		case <-deadline:
			t.Fatalf("drainer 未消费完队列: output=%d status=%d", len(outputCh), len(statusCh))
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	close(stop)
	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stop 关闭后 drainer 未退出（goroutine 泄漏）")
	}
}

// Shutdown 端到端：无消费者 + outputCh 打满 + 一个被阻塞的 eventWriter 写者，
// Shutdown 必须能返回（关闭 outputDone 解除写阻塞 + drainer 兜底）。
func TestSystemShutdown_NoConsumerDoesNotHang(t *testing.T) {
	outputCh := make(chan output.Event, 2)
	statusCh := make(chan string, 2)
	outputDone := make(chan struct{})
	outputCh <- output.Event{Kind: output.KindText, Text: "queued-1"}
	outputCh <- output.Event{Kind: output.KindText, Text: "queued-2"} // 打满，模拟 TUI 已消失后的积压

	w := &eventWriter{ch: outputCh, done: outputDone, kind: output.KindText}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = w.Write([]byte("blocked-write")) // 在 Shutdown 前阻塞在满缓冲上
	}()

	// 给写者一点进入阻塞的时间
	time.Sleep(20 * time.Millisecond)

	sys := &System{OutputCh: outputCh, StatusCh: statusCh, outputDone: outputDone}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		sys.Shutdown()
	}()

	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("无消费者时 Shutdown 挂死（H5 未修复）")
	}
	select {
	case <-writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown 后被阻塞的写者未解除阻塞")
	}

	// 重复 Shutdown 不应 panic（closeOnce 保护）
	sys.Shutdown()
}
