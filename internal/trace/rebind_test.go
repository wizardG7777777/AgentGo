package trace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// B5 修复的回归测试：包级默认 Writer/Dumper 换为同步 holder（RWMutex）+
// SwapDefaultWriter/SwapDefaultDumper 运行时重绑 + Writer/PromptDumper Close
// 后永久停用（绝不重开旧 session 目录的文件）。

// saveAndRestoreDefaults 保存包级默认 Writer/Dispatcher/Dumper，测试结束后恢复。
func saveAndRestoreDefaults(t *testing.T) {
	t.Helper()
	oldW, oldD, oldDumper := Default(), DefaultDispatcher(), DefaultDumper()
	t.Cleanup(func() {
		SetDefault(oldW)
		SetDefaultDispatcher(oldD)
		SetDefaultDumper(oldDumper)
	})
}

// countJSONLLines 统计目录下所有 .jsonl 文件的总行数。
func countJSONLLines(t *testing.T, dir string) (files, lines int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		files++
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				lines++
			}
		}
	}
	return files, lines
}

// TestSwapDefaultWriter_ConcurrentEmitAndSwap：N 个 goroutine 持续 Emit，
// 另一个 goroutine 反复 Swap+Close 旧 writer——事件落在某个存活 writer 的
// 目录里，无 panic、无死锁、无事件重复（30s 超时兜底）。
func TestSwapDefaultWriter_ConcurrentEmitAndSwap(t *testing.T) {
	saveAndRestoreDefaults(t)
	SetDefaultDispatcher(nil)

	const swaps = 50
	writers := make([]*Writer, 0, swaps)
	for i := 0; i < swaps; i++ {
		w, err := NewWriter(filepath.Join(t.TempDir(), "logs"), 0) // maxTasks=0：不做 GC，避免干扰计数
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		writers = append(writers, w)
	}
	SetDefault(writers[0])

	const emitters = 8
	const perEmitter = 200
	var wg sync.WaitGroup
	for g := 0; g < emitters; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perEmitter; i++ {
				Emit(Event{Kind: KindTaskClaimed, TaskID: fmt.Sprintf("task-%d-%d", g, i), AgentID: "agent-x"})
			}
		}(g)
	}
	// swapper：反复换绑并关闭被换下的旧 writer。每次换上的都是新 writer，
	// 保证任意时刻总有一个存活 writer 接收事件（最后一次换上的活到测试结束）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i < swaps; i++ {
			old := SwapDefaultWriter(writers[i])
			if old != nil {
				_ = old.Close()
			}
		}
	}()

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("并发 Emit + Swap 疑似死锁（30s 超时）")
	}

	// 停机后统计：事件总数不超过发射数（被 Close 瞬间命中的事件允许丢弃），
	// 且必须大于 0（证明事件确实落到了某个存活 writer 的目录）。
	total := 0
	for _, w := range writers {
		_, lines := countJSONLLines(t, w.Dir())
		total += lines
	}
	if total == 0 {
		t.Fatal("所有事件都丢失了——没有任何事件落到存活 writer 的目录")
	}
	if total > emitters*perEmitter {
		t.Fatalf("事件重复: 落盘 %d 行 > 发射 %d 条", total, emitters*perEmitter)
	}

	// 清理当前默认 writer（测试自建，不影响恢复的默认值）
	if w := Default(); w != nil {
		_ = w.Close()
	}
}

// TestSwapDefaultWriter_OldWriterDropsPermanently：Swap 返回旧 writer 并 Close 后，
// 旧 writer 上的 Emit/CloseTask 永久 no-op（旧目录文件数与内容冻结），
// 新 writer 正常记录后续事件。
func TestSwapDefaultWriter_OldWriterDropsPermanently(t *testing.T) {
	saveAndRestoreDefaults(t)
	SetDefaultDispatcher(nil)

	dirA := t.TempDir()
	dirB := t.TempDir()
	wa, err := NewWriter(dirA, 0)
	if err != nil {
		t.Fatalf("NewWriter A: %v", err)
	}
	SetDefault(wa)

	Emit(Event{Kind: KindTaskClaimed, TaskID: "task-before", AgentID: "a1"})

	wb, err := NewWriter(dirB, 0)
	if err != nil {
		t.Fatalf("NewWriter B: %v", err)
	}
	old := SwapDefaultWriter(wb)
	if old != wa {
		t.Fatalf("SwapDefaultWriter 返回的旧 writer 不是原默认实例")
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close old writer: %v", err)
	}

	// 切换后的事件只能落 B
	Emit(Event{Kind: KindTaskClaimed, TaskID: "task-after", AgentID: "a1"})

	// 冻结 A 的状态
	filesA, _ := countJSONLLines(t, dirA)
	if filesA != 1 {
		t.Fatalf("切换前 A 目录应有 1 个 trace 文件，实际 %d", filesA)
	}
	snapshotA := readDirBytes(t, dirA)

	// 模拟滞后持有旧 writer 的调用方（stale holder）继续写——必须被丢弃
	wa.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-stale", AgentID: "a1"})
	wa.CloseTask("task-before")
	Emit(Event{Kind: KindTaskClaimed, TaskID: "task-after-2", AgentID: "a1"})

	if got := readDirBytes(t, dirA); got != snapshotA {
		t.Fatal("旧 writer Close 后旧目录内容发生变化——文件被复活")
	}
	if files, _ := countJSONLLines(t, dirA); files != 1 {
		t.Fatalf("旧目录文件数变化: %d, want 1", files)
	}

	// 新 writer 记录了切换后的两条事件
	_, linesB := countJSONLLines(t, dirB)
	if linesB != 2 {
		t.Fatalf("新 writer 目录应有 2 行事件，实际 %d", linesB)
	}
	_ = wb.Close()
}

// TestWriter_EmitAfterClose_NoReopen：Emit-after-Close 不再重开文件——
// 旧目录文件数与字节数保持不变（B5 重绑地雷：stale holder 复活旧目录）。
func TestWriter_EmitAfterClose_NoReopen(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-1", AgentID: "a1"})
	w.CloseTask("task-1") // 先验证 CloseTask 本身语义不受影响
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-1", AgentID: "a1"})

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	filesBefore, _ := countJSONLLines(t, dir)
	snapshot := readDirBytes(t, dir)

	// Close 后所有写操作都是 no-op
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-1", AgentID: "a1"})
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-2", AgentID: "a1"})
	w.CloseTask("task-1")

	filesAfter, linesAfter := countJSONLLines(t, dir)
	if filesAfter != filesBefore {
		t.Fatalf("Close 后目录文件数变化: %d → %d（文件被重开）", filesBefore, filesAfter)
	}
	if got := readDirBytes(t, dir); got != snapshot {
		t.Fatal("Close 后目录内容变化（文件被追加）")
	}
	if linesAfter != 2 {
		t.Fatalf("事件行数应为 2，实际 %d", linesAfter)
	}
}

// TestSwapDefaultDumper_OldClosedNoReopen：prompt dumper 与 writer 同语义——
// Swap 换绑 + Close 旧实例后，旧实例上的 Dump 永久丢弃，新实例正常记录。
func TestSwapDefaultDumper_OldClosedNoReopen(t *testing.T) {
	saveAndRestoreDefaults(t)

	dirA := t.TempDir()
	dirB := t.TempDir()
	da, err := NewPromptDumper(dirA, true)
	if err != nil {
		t.Fatalf("NewPromptDumper A: %v", err)
	}
	SetDefaultDumper(da)
	DumpRequest("task-aaa", 0, "msgs", 1)

	db, err := NewPromptDumper(dirB, true)
	if err != nil {
		t.Fatalf("NewPromptDumper B: %v", err)
	}
	old := SwapDefaultDumper(db)
	if old != da {
		t.Fatal("SwapDefaultDumper 返回的旧实例不是原默认 dumper")
	}
	old.Close()

	// 注意：prompts 文件名只取 taskID 前 8 字符，这里用前 8 字符即不同的 ID
	DumpRequest("task-bbb", 0, "msgs", 1)
	snapshotA := readDirBytesFiltered(t, dirA, ".prompts.jsonl")

	// stale holder 继续写旧 dumper——必须被丢弃
	da.DumpRequest("task-ccc", 0, time.Now(), "msgs", 1)
	DumpResponse("task-ddd", 0, "content", nil, 1, 1)

	if got := readDirBytesFiltered(t, dirA, ".prompts.jsonl"); got != snapshotA {
		t.Fatal("旧 dumper Close 后旧目录 prompts 内容变化——文件被复活")
	}
	entries, err := os.ReadDir(dirB)
	if err != nil {
		t.Fatalf("ReadDir B: %v", err)
	}
	promptFiles := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".prompts.jsonl") {
			promptFiles++
		}
	}
	if promptFiles != 2 {
		t.Fatalf("新 dumper 目录应有 2 个 prompts 文件，实际 %d", promptFiles)
	}
	db.Close()
}

// readDirBytes 拼接目录下所有 .jsonl 文件内容，用于快照比较。
func readDirBytes(t *testing.T, dir string) string {
	t.Helper()
	return readDirBytesFiltered(t, dir, ".jsonl")
}

// readDirBytesFiltered 拼接目录下指定后缀文件的内容（按文件名序，结果确定）。
func readDirBytesFiltered(t *testing.T, dir, suffix string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		sb.WriteString(e.Name())
		sb.WriteString("\n")
		sb.Write(data)
	}
	return sb.String()
}
