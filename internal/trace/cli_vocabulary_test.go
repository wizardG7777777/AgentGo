package trace

import (
	"testing"
	"time"
)

// D5：trace CLI 汇总状态词表对齐 model.TaskStatus（processing/pending 等），
// 不再使用自造的 running / retrying。retry 事件对应 processing→pending
// 回滚，等待重新认领期间标注为 pending(retry)。
func TestSummarizeTask_ModelVocabulary(t *testing.T) {
	base := time.Date(2026, 7, 18, 3, 0, 0, 0, time.UTC)
	recs := func(events ...Event) []traceEventRecord {
		out := make([]traceEventRecord, 0, len(events))
		for i, ev := range events {
			ev.Timestamp = base.Add(time.Duration(i) * time.Second)
			out = append(out, traceEventRecord{event: ev})
		}
		return out
	}

	cases := []struct {
		name   string
		events []Event
		want   string
	}{
		{"published 未认领", []Event{{Kind: KindTaskPublished, TaskID: "t1"}}, "pending"},
		{"claimed 处理中", []Event{{Kind: KindTaskClaimed, TaskID: "t1"}}, "processing"},
		{"retry 等待重认领", []Event{
			{Kind: KindTaskClaimed, TaskID: "t1"},
			{Kind: KindTaskRetry, TaskID: "t1"},
		}, "pending(retry)"},
		{"completed", []Event{
			{Kind: KindTaskClaimed, TaskID: "t1"},
			{Kind: KindTaskCompleted, TaskID: "t1"},
		}, "completed"},
		{"failed", []Event{
			{Kind: KindTaskClaimed, TaskID: "t1"},
			{Kind: KindTaskFailed, TaskID: "t1"},
		}, "failed"},
		{"blocked", []Event{
			{Kind: KindTaskPublished, TaskID: "t1"},
			{Kind: KindTaskBlocked, TaskID: "t1"},
		}, "blocked"},
		{"cancelled", []Event{
			{Kind: KindTaskClaimed, TaskID: "t1"},
			{Kind: KindTaskCancelled, TaskID: "t1"},
		}, "cancelled"},
		{"无生命周期事件", []Event{{Kind: KindLLMCallStart, TaskID: "t1", Loop: 0}}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := &taskTrace{taskID: "t1", records: recs(tc.events...)}
			row := summarizeTask(group)
			if row.status != tc.want {
				t.Fatalf("status = %q, want %q", row.status, tc.want)
			}
			if row.status == "running" || row.status == "retrying" {
				t.Fatalf("出现自造状态词（D5 回归）: %q", row.status)
			}
		})
	}
}

// D5：终态判定只认 model 终态词。
func TestIsTerminalSummaryStatus_ModelTerms(t *testing.T) {
	for _, s := range []string{"completed", "failed", "blocked", "cancelled"} {
		if !isTerminalSummaryStatus(s) {
			t.Errorf("%q 应为终态", s)
		}
	}
	for _, s := range []string{"pending", "processing", "pending(retry)", "unknown"} {
		if isTerminalSummaryStatus(s) {
			t.Errorf("%q 不应为终态", s)
		}
	}
}
