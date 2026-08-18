package store

import (
	"fmt"
	"testing"

	"agentgo/internal/model"
)

type terminalRaceOutcome struct {
	operation string
	err       error
}

// TestSubmitResultWithFieldsRacesCancellation 验证成功结构化提交与取消共享
// 同一状态锁：无论谁先获得锁，终态都只能看到完整成功快照或零结构字段的
// cancelled 快照，不能出现 cancelled + event/custom path 的混合事实。
func TestSubmitResultWithFieldsRacesCancellation(t *testing.T) {
	for i := 0; i < 100; i++ {
		s, _ := newTestStore(4, 100)
		task := publishTestTask(t, s, fmt.Sprintf("原子成功提交竞争-%d", i))
		const agentID = "agent-atomic-complete"
		if err := s.ClaimTask(agentID, task.ID); err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}

		start := make(chan struct{})
		outcomes := make(chan terminalRaceOutcome, 2)
		go func() {
			<-start
			outcomes <- terminalRaceOutcome{
				operation: "submit",
				err: s.SubmitResultWithFields(agentID, task.ID, "done", map[string]string{
					"event":  "ready",
					"custom": `{"coverage":"gap"}`,
				}),
			}
		}()
		go func() {
			<-start
			outcomes <- terminalRaceOutcome{
				operation: "cancel",
				err:       s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled),
			}
		}()
		close(start)

		results := map[string]error{}
		for n := 0; n < 2; n++ {
			outcome := <-outcomes
			results[outcome.operation] = outcome.err
		}
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch got.Status {
		case model.TaskStatusCompleted:
			if results["submit"] != nil || results["cancel"] == nil {
				t.Fatalf("completed 竞争结果不一致: submit=%v cancel=%v", results["submit"], results["cancel"])
			}
			if got.Results["event"] != "ready" || got.Results["custom"] == "" || got.Results[agentID] != "done" {
				t.Fatalf("completed 必须包含完整原子快照: %#v", got.Results)
			}
		case model.TaskStatusCancelled:
			if results["cancel"] != nil || results["submit"] == nil {
				t.Fatalf("cancelled 竞争结果不一致: submit=%v cancel=%v", results["submit"], results["cancel"])
			}
			for _, key := range []string{"event", "custom", agentID} {
				if _, exists := got.Results[key]; exists {
					t.Fatalf("cancelled 不得遗留原子提交字段 %q: %#v", key, got.Results)
				}
			}
		default:
			t.Fatalf("竞争后状态 = %s，期望 completed/cancelled", got.Status)
		}
	}
}

// TestCommitBlockedResultRacesCancellation 对 blocked 路径施加同一竞争：
// carrier、agent 正文、blocked 原因和终态必须一起出现或一起不出现。
func TestCommitBlockedResultRacesCancellation(t *testing.T) {
	for i := 0; i < 100; i++ {
		s, _ := newTestStore(4, 100)
		task := publishTestTask(t, s, fmt.Sprintf("原子 blocked 提交竞争-%d", i))
		const agentID = "agent-atomic-blocked"
		if err := s.ClaimTask(agentID, task.ID); err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}

		start := make(chan struct{})
		outcomes := make(chan terminalRaceOutcome, 2)
		go func() {
			<-start
			outcomes <- terminalRaceOutcome{
				operation: "blocked",
				err: s.CommitBlockedResult(agentID, task.ID, "blocked result", map[string]string{
					"custom": `{"missing":"catalog"}`,
				}, "缺少上游输入", "agent_reported_blocked"),
			}
		}()
		go func() {
			<-start
			outcomes <- terminalRaceOutcome{
				operation: "cancel",
				err:       s.TransitionState(task.ID, model.TaskStatusProcessing, model.TaskStatusCancelled),
			}
		}()
		close(start)

		results := map[string]error{}
		for n := 0; n < 2; n++ {
			outcome := <-outcomes
			results[outcome.operation] = outcome.err
		}
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch got.Status {
		case model.TaskStatusBlocked:
			if results["blocked"] != nil || results["cancel"] == nil {
				t.Fatalf("blocked 竞争结果不一致: blocked=%v cancel=%v", results["blocked"], results["cancel"])
			}
			if got.Results["custom"] == "" || got.Results[agentID] != "blocked result" || got.Error != "缺少上游输入" {
				t.Fatalf("blocked 必须包含完整原子快照: results=%#v error=%q", got.Results, got.Error)
			}
		case model.TaskStatusCancelled:
			if results["cancel"] != nil || results["blocked"] == nil {
				t.Fatalf("cancelled 竞争结果不一致: blocked=%v cancel=%v", results["blocked"], results["cancel"])
			}
			for _, key := range []string{"custom", agentID} {
				if _, exists := got.Results[key]; exists {
					t.Fatalf("cancelled 不得遗留 blocked 提交字段 %q: %#v", key, got.Results)
				}
			}
			if got.Error != "" {
				t.Fatalf("cancelled 不得遗留 blocked 原因: %q", got.Error)
			}
		default:
			t.Fatalf("竞争后状态 = %s，期望 blocked/cancelled", got.Status)
		}
	}
}
