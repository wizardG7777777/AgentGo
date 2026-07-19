package bootstrap

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/session"
	"agentgo/internal/trace"
)

// B5/B7 修复的端到端回归测试：SessionManager 注册 onSessionSwitched 钩子后，
// CreateNew 触发 trace writer 与 system.log 重绑——切换后的 trace 事件与
// 日志行落在新 Session 的 logs/ 目录；旧 writer 被永久关闭（不复活旧目录）；
// 旧 Session logs 目录无句柄泄漏（Windows 上可 rename 为证）。

// readJSONLContents 读取目录下所有 .jsonl 文件的拼接内容。
func readJSONLContents(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		sb.Write(data)
	}
	return sb.String()
}

func countFilesWithSuffix(t *testing.T, dir, suffix string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			n++
		}
	}
	return n
}

func TestRebindSessionLogs_SwitchMovesTraceAndSystemLog(t *testing.T) {
	root := t.TempDir()
	sessRoot := filepath.Join(root, ".agentgo", "sessions")
	sm, err := session.NewSessionManager(sessRoot, session.SessionConfig{})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = sm.Close() })

	oldSess := sm.Current()
	if oldSess == nil {
		t.Fatal("无当前 Session")
	}
	oldLogs := filepath.Join(oldSess.Dir, "logs")

	// 模拟 bootstrap 初装：trace writer + system.log 绑定到启动 Session
	oldTraceWriter, err := trace.NewWriter(oldLogs, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	origW, origD := trace.Default(), trace.DefaultDispatcher()
	trace.SetDefault(oldTraceWriter)
	trace.SetDefaultDispatcher(nil)
	t.Cleanup(func() {
		if w := trace.Default(); w != nil {
			_ = w.Close() // 关闭钩子换绑后的当前 writer，避免 Windows 句柄泄漏
		}
		trace.SetDefault(origW)
		trace.SetDefaultDispatcher(origD)
	})

	oldLogFile, err := os.OpenFile(filepath.Join(oldLogs, "system.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("OpenFile system.log: %v", err)
	}
	holder := &switchableLogFile{file: oldLogFile}
	origLogWriter := log.Writer()
	log.SetOutput(holder)
	t.Cleanup(func() {
		log.SetOutput(origLogWriter)
		_ = holder.Close()
	})

	// 注册重绑钩子（同 bootstrap Step 10）
	sys := &System{LogFile: holder}
	sm.SetOnSwitch(sys.onSessionSwitched)

	// 切换前：trace 事件 + 日志行落在旧 Session
	trace.Emit(trace.Event{Kind: trace.KindTaskClaimed, TaskID: "task-old1", AgentID: "a1"})
	log.Printf("marker-before-switch")

	// 切换（CreateNew 提交后锁外触发钩子）
	newSess, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("CreateNew: %v", err)
	}
	newLogs := filepath.Join(newSess.Dir, "logs")

	// 切换后：trace 事件 + 日志行落在新 Session
	trace.Emit(trace.Event{Kind: trace.KindTaskClaimed, TaskID: "task-new1", AgentID: "a1"})
	log.Printf("marker-after-switch")

	// 模拟 stale holder：旧 writer 已被钩子 Close，写入必须被永久丢弃
	oldTraceWriter.Emit(trace.Event{Kind: trace.KindTaskClaimed, TaskID: "task-stale", AgentID: "a1"})

	// --- 断言：trace 重绑 ---
	if trace.Default() == oldTraceWriter {
		t.Fatal("切换后默认 trace writer 仍是旧实例——未重绑")
	}
	if n := countFilesWithSuffix(t, oldLogs, ".jsonl"); n != 1 {
		t.Fatalf("旧 Session logs 应有 1 个 trace 文件（stale 写入未复活目录），实际 %d", n)
	}
	oldTraceContent := readJSONLContents(t, oldLogs)
	if !strings.Contains(oldTraceContent, "task-old1") {
		t.Fatal("旧 Session trace 文件缺少切换前的事件 task-old1")
	}
	if strings.Contains(oldTraceContent, "task-new1") || strings.Contains(oldTraceContent, "task-stale") {
		t.Fatal("旧 Session trace 文件混入了切换后/stale 事件——旧 writer 未被永久关闭")
	}
	newTraceContent := readJSONLContents(t, newLogs)
	if !strings.Contains(newTraceContent, "task-new1") {
		t.Fatal("新 Session logs 缺少切换后的 trace 事件 task-new1——重绑失败")
	}

	// --- 断言：system.log 重绑 ---
	oldSysLog, err := os.ReadFile(filepath.Join(oldLogs, "system.log"))
	if err != nil {
		t.Fatalf("读取旧 system.log: %v", err)
	}
	if !strings.Contains(string(oldSysLog), "marker-before-switch") {
		t.Fatal("旧 system.log 缺少切换前的日志行")
	}
	if strings.Contains(string(oldSysLog), "marker-after-switch") {
		t.Fatal("旧 system.log 混入了切换后的日志行——旧句柄仍在被写")
	}
	newSysLog, err := os.ReadFile(filepath.Join(newLogs, "system.log"))
	if err != nil {
		t.Fatalf("读取新 system.log: %v", err)
	}
	if !strings.Contains(string(newSysLog), "marker-after-switch") {
		t.Fatal("新 system.log 缺少切换后的日志行——日志未重绑")
	}

	// --- 断言：旧 logs 目录无句柄泄漏 ---
	// Windows 上只要有任何句柄（trace 分片 / system.log / history）还开着，
	// rename 就会失败。此断言在所有平台执行，Windows 上才是真正证明。
	moved := oldLogs + ".moved"
	if err := os.Rename(oldLogs, moved); err != nil {
		t.Fatalf("旧 Session logs 目录 rename 失败（疑似句柄泄漏）: %v", err)
	}
}

// TestSwitchableLogFile_ConcurrentWriteAndSwap：log.Printf 并发写与 Swap
// 换绑互不 panic/死锁（30s 超时兜底）；Close 后写入被丢弃不报错。
func TestSwitchableLogFile_ConcurrentWriteAndSwap(t *testing.T) {
	open := func(dir string) *os.File {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(dir, "system.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		return f
	}

	holder := &switchableLogFile{file: open(t.TempDir())}

	var wg sync.WaitGroup
	// 直接并发写 holder（log 包内部已串行，这里额外压测 holder 自身的锁）
	const writers = 8
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if _, err := holder.Write([]byte("concurrent log line\n")); err != nil {
					return
				}
			}
		}()
	}
	// swapper：反复换绑到新文件（Swap 内部关闭旧文件）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			holder.Swap(open(t.TempDir()))
		}
	}()

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("并发 Write + Swap 疑似死锁（30s 超时）")
	}

	// Close 后写丢弃、返回成功；重复 Close 安全
	if err := holder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n, err := holder.Write([]byte("dropped\n")); err != nil || n != len("dropped\n") {
		t.Fatalf("Close 后 Write = (%d, %v), want (%d, nil)", n, err, len("dropped\n"))
	}
	if err := holder.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
}
