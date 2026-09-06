package agent

import (
	"agentgo/internal/trace"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureTraceToDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	w, err := trace.NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	orig := trace.Default()
	trace.SetDefault(w)
	t.Cleanup(func() { trace.SetDefault(orig) })
	return dir
}

// readTraceEventsFromDir 读取目录下全部 .jsonl trace 分片并逐行解析为 Event。
// 读取发生在断言阶段（Writer.Emit 每次 append 直接落盘），无需先 Close。
func readTraceEventsFromDir(t *testing.T, dir string) []trace.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir trace dir: %v", err)
	}
	var evs []trace.Event
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev trace.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("unmarshal trace event: %v (line=%q)", err, line)
			}
			evs = append(evs, ev)
		}
	}
	return evs
}

// TestInjectMemoryContext_NoAuditTraceEvent 钉住 V6 删除不变量：
// 注入发生后，trace 目录中不得存在 kind 为 "memory_context_inject" 的事件。
