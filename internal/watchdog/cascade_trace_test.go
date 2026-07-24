package watchdog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// 2026-07-22 浪费可观测化：pending→cancelled 的级联取消必须补发
// trace.KindTaskCancelled 事件——pending 任务没有执行 agent，若 watchdog
// 不补发，排队期间被级联取消的任务在 trace 中完全不可见（2026-07-21
// 验收空转事故里多个排队验收任务即是如此）。processing 路径由 agent 在
// ctx.Done() 分支 emit，不在此重复。
func TestWatchdog_PendingCascadeCancelEmitsTrace(t *testing.T) {
	tw, err := trace.NewWriter(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	old := trace.SwapDefaultWriter(tw)
	t.Cleanup(func() {
		trace.SetDefault(old)
		_ = tw.Close()
	})

	w, s, _, _ := newWatchdogWithMailbox(t)

	// A 先失败，B 依赖 A 且处于 pending——下一次 inspect 触发级联取消。
	taskA := &model.Task{Description: "A 先失败", EventSource: "scheduler"}
	if err := s.PublishTask(taskA); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionState(taskA.ID, model.TaskStatusPending, model.TaskStatusFailed); err != nil {
		t.Fatal(err)
	}
	taskB := &model.Task{Description: "B 等 A，会被级联取消", Dependencies: []string{taskA.ID}, EventSource: "scheduler"}
	if err := s.PublishTask(taskB); err != nil {
		t.Fatal(err)
	}

	inspectAll(w)

	if got, _ := s.GetTask(taskB.ID); got.Status != model.TaskStatusCancelled {
		t.Fatalf("B.status = %s, want cancelled", got.Status)
	}

	// B 从未被 claim，trace 文件应由 watchdog 的补发事件创建。
	shortID := taskB.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	matches, err := filepath.Glob(filepath.Join(tw.Dir(), "*_"+shortID+".jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("未找到 B 的 trace 文件（级联取消事件未落盘）: matches=%v err=%v", matches, err)
	}

	var found *trace.Event
	for _, path := range matches {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev trace.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			if ev.Kind == trace.KindTaskCancelled && ev.TaskID == taskB.ID {
				evCopy := ev
				found = &evCopy
			}
		}
		_ = f.Close()
	}
	if found == nil {
		t.Fatal("B 的 trace 中没有 task_cancelled 事件")
	}
	if found.Transition == nil {
		t.Fatal("task_cancelled 事件缺少 Transition 载荷")
	}
	if found.Transition.CancelSource != "dependency_failure" {
		t.Errorf("CancelSource = %q, want dependency_failure", found.Transition.CancelSource)
	}
	if found.Transition.PrevStatus != string(model.TaskStatusPending) {
		t.Errorf("PrevStatus = %q, want pending", found.Transition.PrevStatus)
	}
	if !strings.Contains(found.Reason, "级联取消") {
		t.Errorf("Reason 应含 '级联取消': %q", found.Reason)
	}
}
