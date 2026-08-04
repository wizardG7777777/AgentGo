package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// V6 §7.1 trace_degraded：制造写失败（预建同名目录让 shard open 失败）→
// marker 落盘 + OnDegraded 回调触发一次；写恢复后 marker 清除。
func TestWriter_DegradedMarkerAndRecovery(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// 失败注入：预建与目标分片同名的目录，os.OpenFile 必失败（跨平台确定性）
	failTS := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	taskID := "degraded-task-1"
	blockName := fmt.Sprintf("%s_%s.jsonl", failTS.Format("2006-01-02T15-04-05"), taskID[:8])
	blockPath := filepath.Join(dir, blockName)
	if err := os.Mkdir(blockPath, 0755); err != nil {
		t.Fatalf("预建阻塞目录: %v", err)
	}

	cbCount := 0
	w.SetOnDegraded(func(err error) { cbCount++ })

	failEv := Event{Kind: KindTaskClaimed, TaskID: taskID, Timestamp: failTS}
	w.Emit(failEv)

	m := ReadDegradedMarker(dir)
	if m == nil {
		t.Fatalf("首次写失败后 marker 未落盘")
	}
	if m.Count != 1 || m.Error == "" || m.FirstFailureTime == "" {
		t.Errorf("marker 内容不对: %+v", m)
	}
	if cbCount != 1 {
		t.Errorf("OnDegraded 回调次数 = %d, want 1", cbCount)
	}

	// 第二次失败：计数刷新，回调不重复触发
	w.Emit(failEv)
	m = ReadDegradedMarker(dir)
	if m == nil || m.Count != 2 {
		t.Errorf("第二次失败后 marker Count = %+v, want 2", m)
	}
	if cbCount != 1 {
		t.Errorf("降级期内回调重复触发：%d 次", cbCount)
	}

	// 恢复：移除阻塞目录，换时间戳写新分片 → 成功 → marker 清除
	if err := os.Remove(blockPath); err != nil {
		t.Fatalf("移除阻塞目录: %v", err)
	}
	okTS := failTS.Add(time.Second)
	w.Emit(Event{Kind: KindTaskCompleted, TaskID: taskID, Timestamp: okTS})

	if m := ReadDegradedMarker(dir); m != nil {
		t.Errorf("写恢复后 marker 应被清除，仍读到 %+v", m)
	}
	okName := fmt.Sprintf("%s_%s.jsonl", okTS.Format("2006-01-02T15-04-05"), taskID[:8])
	evs := readEvents(t, filepath.Join(dir, okName))
	if len(evs) != 1 || evs[0].Kind != KindTaskCompleted {
		t.Errorf("恢复后分片内容不对: %+v", evs)
	}

	// 恢复后再次失败：重新进入降级态，回调再触发一次。先 CloseTask 模拟
	// 重试边界（句柄表按 taskID 归键，不关会复用上一分片的句柄而不再
	// 触发 open 失败）。
	w.CloseTask(taskID)
	if err := os.Mkdir(blockPath, 0755); err != nil {
		t.Fatalf("重建阻塞目录: %v", err)
	}
	w.Emit(failEv)
	if cbCount != 2 {
		t.Errorf("恢复后再失败应重新触发回调，累计 = %d, want 2", cbCount)
	}
}

// ReadDegradedMarker：无 marker / 坏内容均返回 nil。
func TestReadDegradedMarker(t *testing.T) {
	dir := t.TempDir()
	if m := ReadDegradedMarker(dir); m != nil {
		t.Errorf("无 marker 应返回 nil，实际 %+v", m)
	}
	if err := os.WriteFile(filepath.Join(dir, DegradedMarkerFileName), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if m := ReadDegradedMarker(dir); m != nil {
		t.Errorf("坏内容应返回 nil，实际 %+v", m)
	}
	good, _ := json.Marshal(DegradedMarker{FirstFailureTime: "2026-08-01T00:00:00Z", Error: "disk full", Count: 3})
	if err := os.WriteFile(filepath.Join(dir, DegradedMarkerFileName), good, 0644); err != nil {
		t.Fatal(err)
	}
	m := ReadDegradedMarker(dir)
	if m == nil || m.Count != 3 || m.Error != "disk full" {
		t.Errorf("正常 marker 解析不对: %+v", m)
	}
}

// CLI：trace list / show 检测到 marker 时在 header 打 trace_degraded 提示。
func TestCLI_DegradedHint(t *testing.T) {
	dir := t.TempDir()
	taskID := "abcd1234-5678"
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var lines []string
	for _, ev := range []Event{
		{Timestamp: base, Kind: KindTaskPublished, TaskID: taskID},
		{Timestamp: base.Add(time.Second), Kind: KindTaskCompleted, TaskID: taskID},
	} {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	shard := filepath.Join(dir, "2026-08-01T00-00-00_abcd1234.jsonl")
	if err := os.WriteFile(shard, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 无 marker：无提示
	var buf bytes.Buffer
	if err := cmdList(dir, &buf); err != nil {
		t.Fatalf("cmdList: %v", err)
	}
	if strings.Contains(buf.String(), "trace_degraded") {
		t.Errorf("无 marker 时不应出现 trace_degraded 提示")
	}

	marker, _ := json.Marshal(DegradedMarker{FirstFailureTime: "2026-08-01T00:00:00Z", Error: "disk full", Count: 2})
	if err := os.WriteFile(filepath.Join(dir, DegradedMarkerFileName), marker, 0644); err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	if err := cmdList(dir, &buf); err != nil {
		t.Fatalf("cmdList: %v", err)
	}
	if !strings.Contains(buf.String(), "trace_degraded") {
		t.Errorf("trace list 应在 header 前打 trace_degraded 提示，输出:\n%s", buf.String())
	}

	buf.Reset()
	if err := cmdShow(dir, "abcd1234", &buf); err != nil {
		t.Fatalf("cmdShow: %v", err)
	}
	if !strings.Contains(buf.String(), "trace_degraded") {
		t.Errorf("trace show 应在 header 打 trace_degraded 提示，输出:\n%s", buf.String())
	}
}
