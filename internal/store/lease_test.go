package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/session"
)

// lease_test.go 覆盖 V6 §4（H1）执行租约的 store 侧语义：
//   - FreezeTaskLease：首次冻结 / 重认领复用（原子 CAS）；
//   - RevokeTaskLease：撤销幂等；
//   - 终态方法（SubmitResult / FailTask / TransitionState）自动撤销租约；
//   - 快照往返：Lease 经 ExportSnapshot → JSON → ImportSnapshot 完整还原，
//     旧快照无 Lease 字段时按「尚未冻结」降级。

func newTestLease(taskID string) *model.ExecutionLease {
	return &model.ExecutionLease{
		TaskID:        taskID,
		Attempt:       1,
		FrozenAt:      time.Date(2026, 8, 3, 1, 0, 0, 0, time.UTC),
		BusinessTools: []string{"read_file", "write_file"},
		ControlTools:  []string{"submit_task_result"},
		Model:         "m-kind",
		Synthetic:     true,
	}
}

func publishLeaseTask(t *testing.T, s *MemoryTaskStore, agentID string) *model.Task {
	t.Helper()
	task := &model.Task{Description: "租约任务", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask(agentID, task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	return task
}

func TestFreezeTaskLease_FirstFreezeThenReuse(t *testing.T) {
	s := NewMemoryTaskStore(nil, 10, 1, 60)
	task := publishLeaseTask(t, s, "worker-1")

	candidate := newTestLease(task.ID)
	candidate.Digest = candidate.ComputeDigest()

	effective, frozen, err := s.FreezeTaskLease(task.ID, candidate)
	if err != nil || !frozen {
		t.Fatalf("首次冻结应 frozen=true: frozen=%t err=%v", frozen, err)
	}
	if effective.Digest != candidate.Digest {
		t.Fatalf("冻结租约 digest = %q，want %q", effective.Digest, candidate.Digest)
	}

	// 重认领（重试/恢复）复用既有租约：即使候选不同也返回既有那份。
	other := newTestLease(task.ID)
	other.BusinessTools = []string{"read_file"}
	other.Digest = other.ComputeDigest()
	effective, frozen, err = s.FreezeTaskLease(task.ID, other)
	if err != nil || frozen {
		t.Fatalf("已有租约应复用 frozen=false: frozen=%t err=%v", frozen, err)
	}
	if effective.Digest != candidate.Digest {
		t.Fatalf("复用租约 digest = %q，want 既有 %q", effective.Digest, candidate.Digest)
	}
	if len(effective.BusinessTools) != 2 {
		t.Fatalf("复用租约 BusinessTools = %v，want 既有全量", effective.BusinessTools)
	}

	// 不存在的任务报错。
	if _, _, err := s.FreezeTaskLease("ghost", candidate); err == nil {
		t.Fatal("不存在任务的冻结应报错")
	}
}

func TestRevokeTaskLease_Idempotent(t *testing.T) {
	s := NewMemoryTaskStore(nil, 10, 1, 60)
	task := publishLeaseTask(t, s, "worker-1")

	// 无租约时撤销为 no-op。
	if _, newly, err := s.RevokeTaskLease(task.ID); err != nil || newly {
		t.Fatalf("无租约撤销应 newly=false: newly=%t err=%v", newly, err)
	}

	candidate := newTestLease(task.ID)
	candidate.Digest = candidate.ComputeDigest()
	if _, _, err := s.FreezeTaskLease(task.ID, candidate); err != nil {
		t.Fatalf("FreezeTaskLease: %v", err)
	}
	revoked, newly, err := s.RevokeTaskLease(task.ID)
	if err != nil || !newly {
		t.Fatalf("首次撤销应 newly=true: newly=%t err=%v", newly, err)
	}
	if !revoked.Revoked {
		t.Fatal("撤销返回的租约应置 Revoked=true")
	}
	// 幂等：重复撤销不再报告 newly。
	if _, newly, err := s.RevokeTaskLease(task.ID); err != nil || newly {
		t.Fatalf("重复撤销应 newly=false: newly=%t err=%v", newly, err)
	}
}

// 终态方法自动撤销租约：SubmitResult（completed）/ FailTask（failed）/
// TransitionState（cancelled）三条路径都覆盖。
func TestLease_TerminalTransitionsRevoke(t *testing.T) {
	cases := []struct {
		name      string
		terminate func(t *testing.T, s *MemoryTaskStore, taskID string)
	}{
		{"completed", func(t *testing.T, s *MemoryTaskStore, taskID string) {
			if err := s.SubmitResult("worker-1", taskID, "done"); err != nil {
				t.Fatalf("SubmitResult: %v", err)
			}
		}},
		{"failed", func(t *testing.T, s *MemoryTaskStore, taskID string) {
			if err := s.FailTask("worker-1", taskID, "boom"); err != nil {
				t.Fatalf("FailTask: %v", err)
			}
		}},
		{"cancelled", func(t *testing.T, s *MemoryTaskStore, taskID string) {
			if err := s.TransitionState(taskID, model.TaskStatusProcessing, model.TaskStatusCancelled); err != nil {
				t.Fatalf("TransitionState: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewMemoryTaskStore(nil, 10, 1, 60)
			task := publishLeaseTask(t, s, "worker-1")
			candidate := newTestLease(task.ID)
			candidate.Digest = candidate.ComputeDigest()
			if _, _, err := s.FreezeTaskLease(task.ID, candidate); err != nil {
				t.Fatalf("FreezeTaskLease: %v", err)
			}
			tc.terminate(t, s, task.ID)
			got, err := s.GetTask(task.ID)
			if err != nil || got == nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.Lease == nil || !got.Lease.Revoked {
				t.Fatalf("终态 %s 后租约应被撤销: %+v", tc.name, got.Lease)
			}
		})
	}
}

// 重试回滚（RetryRollback）不是终态：租约保留且不撤销，供重认领复用。
func TestLease_RetryRollbackKeepsLease(t *testing.T) {
	s := NewMemoryTaskStore(nil, 10, 1, 60)
	task := publishLeaseTask(t, s, "worker-1")
	candidate := newTestLease(task.ID)
	candidate.Digest = candidate.ComputeDigest()
	if _, _, err := s.FreezeTaskLease(task.ID, candidate); err != nil {
		t.Fatalf("FreezeTaskLease: %v", err)
	}
	if err := s.RetryRollback("worker-1", task.ID, "recoverable"); err != nil {
		t.Fatalf("RetryRollback: %v", err)
	}
	got, err := s.GetTask(task.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Lease == nil || got.Lease.Revoked {
		t.Fatalf("重试回滚后租约应保留且未撤销: %+v", got.Lease)
	}
	if got.Lease.Digest != candidate.Digest {
		t.Fatalf("回滚后 digest = %q，want %q", got.Lease.Digest, candidate.Digest)
	}
}

// 快照往返：Lease 经 ExportSnapshot → JSON（模拟落盘）→ ImportSnapshot
// 完整还原（含 Revoked 位）；旧快照无 Lease 字段时还原为 nil（尚未冻结）。
func TestSnapshot_LeaseRoundTrip(t *testing.T) {
	s1 := NewMemoryTaskStore(nil, 10, 1, 60)
	task := publishLeaseTask(t, s1, "worker-1")
	lease := newTestLease(task.ID)
	lease.Workspace = model.IsolationModeWorkspace
	lease.ApprovalRequired = true
	lease.Digest = lease.ComputeDigest()
	if _, _, err := s1.FreezeTaskLease(task.ID, lease); err != nil {
		t.Fatalf("FreezeTaskLease: %v", err)
	}

	payload, err := json.Marshal(s1.ExportSnapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var snaps []session.TaskSnapshot
	if err := json.Unmarshal(payload, &snaps); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	s2 := NewMemoryTaskStore(nil, 10, 1, 60)
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, err := s2.GetTask(task.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Lease == nil {
		t.Fatal("快照恢复后租约丢失")
	}
	rl := got.Lease
	if rl.Digest != lease.Digest || rl.TaskID != task.ID || rl.Attempt != 1 ||
		!rl.Synthetic || !rl.ApprovalRequired || rl.Revoked ||
		rl.Workspace != model.IsolationModeWorkspace || rl.Model != "m-kind" {
		t.Fatalf("快照恢复租约字段不符: %+v", rl)
	}
	if len(rl.BusinessTools) != 2 || len(rl.ControlTools) != 1 {
		t.Fatalf("快照恢复工具清单不符: %+v", rl)
	}
	if rl.FrozenAt.IsZero() {
		t.Fatal("快照恢复 FrozenAt 丢失")
	}
	// 恢复后重认领走复用语义：FreezeTaskLease 返回既有租约。
	other := newTestLease(task.ID)
	other.Digest = other.ComputeDigest()
	effective, frozen, err := s2.FreezeTaskLease(task.ID, other)
	if err != nil || frozen {
		t.Fatalf("恢复任务的重认领应复用既有租约: frozen=%t err=%v", frozen, err)
	}
	if effective.Digest != lease.Digest {
		t.Fatalf("复用 digest = %q，want 快照还原的 %q", effective.Digest, lease.Digest)
	}
}

func TestSnapshot_ProcessingRevokedLeaseIsQuarantined(t *testing.T) {
	s := NewMemoryTaskStore(nil, 10, 1, 60)
	snaps := []session.TaskSnapshot{{
		ID: "task-finalizing-crash", Description: "finalizing crash",
		Status: string(model.TaskStatusProcessing), Agents: []string{"worker-1"},
		MaxConcurrency: 1,
		Lease: &session.LeaseSnapshot{
			Attempt: 1, BusinessTools: []string{"write_file"},
			ControlTools: []string{"submit_task_result"}, Revoked: true, Digest: "deadbeef1234",
		},
	}}
	if err := s.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, err := s.GetTask("task-finalizing-crash")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskStatusBlocked ||
		!strings.Contains(got.Error, "execution_lease_recovery_quarantine") ||
		got.Lease == nil || !got.Lease.Revoked || got.CompletedAt.IsZero() {
		t.Fatalf("processing+revoked 快照应 fail-closed 为可见 blocked: %+v", got)
	}
	if err := s.ClaimTask("worker-2", got.ID); err == nil {
		t.Fatal("已撤销租约的 finalizing 恢复任务不得被重新认领")
	}
}

// 旧版快照（无 lease 字段）导入后租约为 nil——下次认领即时冻结。
func TestSnapshot_LegacyNoLeaseDecodesNil(t *testing.T) {
	s1 := NewMemoryTaskStore(nil, 10, 1, 60)
	task := publishLeaseTask(t, s1, "worker-1")

	payload, err := json.Marshal(s1.ExportSnapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var snaps []session.TaskSnapshot
	if err := json.Unmarshal(payload, &snaps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s2 := NewMemoryTaskStore(nil, 10, 1, 60)
	if err := s2.ImportSnapshot(snaps); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, err := s2.GetTask(task.ID)
	if err != nil || got == nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Lease != nil {
		t.Fatalf("未冻结任务快照恢复后租约应为 nil，实际 %+v", got.Lease)
	}
	// 即时冻结仍可用。
	candidate := newTestLease(task.ID)
	candidate.Digest = candidate.ComputeDigest()
	if _, frozen, err := s2.FreezeTaskLease(task.ID, candidate); err != nil || !frozen {
		t.Fatalf("旧快照任务应可即时冻结: frozen=%t err=%v", frozen, err)
	}
}
