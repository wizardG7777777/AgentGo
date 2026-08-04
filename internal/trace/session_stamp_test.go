package trace

import (
	"path/filepath"
	"testing"
)

// V6 §7.2：SessionID 由 Writer 集中盖戳——事件未携带时补上 writer 绑定的
// session id；发射方显式填写的值不被覆盖；换绑（SetSessionID）后新事件
// 用新值。
func TestWriter_SessionIDStamp(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	w.SetSessionID("sess-1")
	if got := w.SessionID(); got != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", got)
	}

	// 空 SessionID → 盖戳
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-aaa"})
	// 显式携带 → 不覆盖
	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-bbb", SessionID: "explicit-sess"})

	files := listTraceFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("期望 2 个分片，实际 %d", len(files))
	}
	seen := map[string]string{} // taskID → sessionID
	for _, name := range files {
		for _, ev := range readEvents(t, filepath.Join(dir, name)) {
			seen[ev.TaskID] = ev.SessionID
		}
	}
	if seen["task-aaa"] != "sess-1" {
		t.Errorf("空 SessionID 事件盖戳 = %q, want sess-1", seen["task-aaa"])
	}
	if seen["task-bbb"] != "explicit-sess" {
		t.Errorf("显式 SessionID 被覆盖为 %q, want explicit-sess", seen["task-bbb"])
	}

	// 换绑（session 切换同步）：新事件用新 session id，旧分片内容不变
	w.SetSessionID("sess-2")
	w.Emit(Event{Kind: KindTaskCompleted, TaskID: "task-ccc"})
	for _, name := range listTraceFiles(t, dir) {
		for _, ev := range readEvents(t, filepath.Join(dir, name)) {
			if ev.TaskID == "task-ccc" && ev.SessionID != "sess-2" {
				t.Errorf("换绑后事件盖戳 = %q, want sess-2", ev.SessionID)
			}
		}
	}
}

// 未设置 session id（无活跃 Session，trace 落 .agentgo/traces/）时事件不带
// session_id。
func TestWriter_SessionIDEmptyNoStamp(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	w.Emit(Event{Kind: KindTaskClaimed, TaskID: "task-ddd"})
	files := listTraceFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("期望 1 个分片，实际 %d", len(files))
	}
	for _, ev := range readEvents(t, filepath.Join(dir, files[0])) {
		if ev.SessionID != "" {
			t.Errorf("无 Session 时 SessionID = %q, want 空", ev.SessionID)
		}
	}
}
