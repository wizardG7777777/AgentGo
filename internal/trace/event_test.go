package trace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// v5 Phase 2 引入的 schema 升级（TraceUpgrade.md §3）。
// 三个 sub-payload struct（Transition / ShellExec / ShellTimeout）需要保证：
//  1. 序列化/反序列化对称——marshal 后 unmarshal 不丢字段
//  2. omitempty 生效——所有零值字段不出现在 JSON 中
//  3. v4 兼容——旧 jsonl（没有任何新字段）unmarshal 后三个指针为 nil
//  4. nil 子字段不输出——保持 v4 jsonl 字节级兼容（除新事件外）

func TestTransitionRoundtrip(t *testing.T) {
	ev := Event{
		Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		Kind:      KindTaskCancelled,
		TaskID:    "task-abc",
		AgentID:   "agent-1",
		Reason:    "watchdog: stuck",
		Transition: &Transition{
			PrevStatus:   "processing",
			NewStatus:    "cancelled",
			CancelSource: "watchdog",
		},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Transition == nil {
		t.Fatalf("Transition lost")
	}
	if got.Transition.CancelSource != "watchdog" {
		t.Errorf("CancelSource=%q want watchdog", got.Transition.CancelSource)
	}
	if got.Transition.PrevStatus != "processing" || got.Transition.NewStatus != "cancelled" {
		t.Errorf("status pair lost: prev=%q new=%q", got.Transition.PrevStatus, got.Transition.NewStatus)
	}
}

func TestShellExecRoundtrip(t *testing.T) {
	ev := Event{
		Timestamp: time.Now(),
		Kind:      KindShellExecuted,
		TaskID:    "task-shell",
		ShellExec: &ShellExec{
			Command:       "ls -la /tmp",
			ExitCode:      0,
			DurationMS:    42,
			Outcome:       "success",
			StdoutExcerpt: "total 0",
		},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ShellExec == nil || got.ShellExec.Command != "ls -la /tmp" {
		t.Fatalf("ShellExec lost: %+v", got.ShellExec)
	}
	if got.ShellExec.DurationMS != 42 || got.ShellExec.Outcome != "success" {
		t.Errorf("ShellExec fields wrong: %+v", got.ShellExec)
	}
}

func TestShellTimeoutPendingVsResolved(t *testing.T) {
	pending := Event{
		Timestamp: time.Now(),
		Kind:      KindShellTimeoutPending,
		TaskID:    "task-1",
		ShellTimeout: &ShellTimeout{
			Command:    "sleep 100",
			ElapsedSec: 30,
		},
	}
	resolved := Event{
		Timestamp: time.Now(),
		Kind:      KindShellTimeoutResolved,
		TaskID:    "task-1",
		ShellTimeout: &ShellTimeout{
			Command:      "sleep 100",
			ElapsedSec:   60,
			Decision:     "wait",
			ExtraSeconds: 30,
		},
	}

	pData, _ := json.Marshal(pending)
	rData, _ := json.Marshal(resolved)

	// Pending 不含 decision 字段（omitempty）
	if strings.Contains(string(pData), `"decision"`) {
		t.Errorf("pending event should omit decision field, got: %s", string(pData))
	}
	// Resolved 含 decision 字段
	if !strings.Contains(string(rData), `"decision":"wait"`) {
		t.Errorf("resolved event missing decision, got: %s", string(rData))
	}
}

func TestNilSubpayloadsOmitted(t *testing.T) {
	// v4 风格事件：所有新字段为 nil
	ev := Event{
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Kind:      KindLLMCallStart,
		TaskID:    "task-1",
		AgentID:   "agent-1",
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, field := range []string{`"transition"`, `"shell_exec"`, `"shell_timeout"`} {
		if strings.Contains(s, field) {
			t.Errorf("nil sub-payload field %s should be omitted, got: %s", field, s)
		}
	}
}

func TestV4JsonlBackwardCompat(t *testing.T) {
	// 模拟一条 v4 时代写出的 jsonl：完全没有 transition/shell_exec/shell_timeout 字段
	v4Line := `{"ts":"2026-04-01T00:00:00Z","kind":"task_claimed","task_id":"task-old","agent_id":"agent-1","loop":0}`
	var ev Event
	if err := json.Unmarshal([]byte(v4Line), &ev); err != nil {
		t.Fatalf("v4 jsonl unmarshal failed: %v", err)
	}
	if ev.Transition != nil || ev.ShellExec != nil || ev.ShellTimeout != nil {
		t.Errorf("v4 jsonl should produce nil sub-payloads, got transition=%v shellExec=%v shellTimeout=%v",
			ev.Transition, ev.ShellExec, ev.ShellTimeout)
	}
	if ev.Kind != KindTaskClaimed || ev.TaskID != "task-old" {
		t.Errorf("v4 jsonl base fields lost: kind=%s taskID=%s", ev.Kind, ev.TaskID)
	}
}

func TestFormatEventDetailsTransitionRendering(t *testing.T) {
	cases := []struct {
		name     string
		ev       Event
		contains []string
	}{
		{
			name: "task_claimed with Transition",
			ev: Event{
				Kind: KindTaskClaimed,
				Transition: &Transition{
					PrevStatus: "pending", NewStatus: "processing",
				},
			},
			contains: []string{"prev=pending", "new=processing"},
		},
		{
			name: "task_completed with Transition + output_len",
			ev: Event{
				Kind:      KindTaskCompleted,
				OutputLen: 128,
				Transition: &Transition{
					PrevStatus: "processing", NewStatus: "completed",
					Cause: "react_loop_exit:natural",
				},
			},
			contains: []string{"prev=processing", "new=completed", "cause=react_loop_exit:natural", "output_len=128"},
		},
		{
			name: "task_failed with Transition + Reason",
			ev: Event{
				Kind:   KindTaskFailed,
				Reason: "max retries",
				Transition: &Transition{
					PrevStatus: "processing", NewStatus: "failed",
					Cause: "max_loops_exceeded", RetryCount: 3,
				},
			},
			contains: []string{"prev=processing", "new=failed", "retry=3", "cause=max_loops_exceeded", `reason="max retries"`},
		},
		{
			name: "task_cancelled with cancel_source",
			ev: Event{
				Kind: KindTaskCancelled,
				Transition: &Transition{
					PrevStatus: "processing", NewStatus: "cancelled",
					CancelSource: "watchdog",
				},
			},
			contains: []string{"prev=processing", "new=cancelled", "source=watchdog"},
		},
		{
			name: "agent_state_changed",
			ev: Event{
				Kind: KindAgentStateChanged,
				Transition: &Transition{
					PrevState: "processing", NewState: "waiting_approval",
					Cause: "approval_required:run_shell",
				},
			},
			contains: []string{"prev=processing", "new=waiting_approval", "cause=approval_required:run_shell"},
		},
		{
			name: "shell_executed",
			ev: Event{
				Kind: KindShellExecuted,
				ShellExec: &ShellExec{
					Command: "ls", ExitCode: 0, DurationMS: 12, Outcome: "success",
				},
			},
			contains: []string{`cmd="ls"`, "exit=0", "duration=12ms", "outcome=success"},
		},
		{
			name: "shell_timeout_resolved with wait+extra",
			ev: Event{
				Kind: KindShellTimeoutResolved,
				ShellTimeout: &ShellTimeout{
					Command: "make build", Decision: "wait", ExtraSeconds: 60,
				},
			},
			contains: []string{`cmd="make build"`, "decision=wait", "extra=60s"},
		},
		{
			name: "task_claimed without Transition (v4 backward compat)",
			ev: Event{
				Kind: KindTaskClaimed,
			},
			contains: nil, // 没有 Transition 时不应 panic、不输出 prev/new
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatEventDetails(tc.ev)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("output %q missing %q", got, want)
				}
			}
			if len(tc.contains) == 0 && got != "" && tc.ev.Kind == KindTaskClaimed {
				t.Errorf("v4 backward compat case should produce empty details, got %q", got)
			}
		})
	}
}

func TestDetectAnomaliesNewHeuristics(t *testing.T) {
	t.Run("panic cause", func(t *testing.T) {
		events := []Event{
			{
				Kind:   KindTaskFailed,
				Reason: "agent panic: nil pointer",
				Transition: &Transition{
					PrevStatus: "processing", NewStatus: "failed",
					Cause: "react_loop_exit:panic",
				},
			},
		}
		anom := detectAnomalies(events)
		found := false
		for _, a := range anom {
			if strings.Contains(a, "ERROR") && strings.Contains(a, "panic") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected panic anomaly, got %v", anom)
		}
	})

	t.Run("watchdog cancel", func(t *testing.T) {
		events := []Event{
			{
				Kind: KindTaskCancelled,
				Transition: &Transition{
					PrevStatus: "processing", NewStatus: "cancelled",
					CancelSource: "watchdog",
				},
			},
		}
		anom := detectAnomalies(events)
		found := false
		for _, a := range anom {
			if strings.Contains(a, "watchdog") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected watchdog anomaly, got %v", anom)
		}
	})

	t.Run("shell timeout flood", func(t *testing.T) {
		var events []Event
		for i := 0; i < 4; i++ {
			events = append(events, Event{
				Kind:         KindShellTimeoutPending,
				ShellTimeout: &ShellTimeout{Command: "x", ElapsedSec: 10},
			})
		}
		anom := detectAnomalies(events)
		found := false
		for _, a := range anom {
			if strings.Contains(a, "shell timeout") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected shell timeout anomaly, got %v", anom)
		}
	})

	t.Run("waiting approval too long", func(t *testing.T) {
		base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
		events := []Event{
			{
				Timestamp: base,
				Kind:      KindAgentStateChanged,
				Transition: &Transition{
					PrevState: "processing", NewState: "waiting_approval",
				},
			},
			{
				Timestamp: base.Add(7 * time.Minute),
				Kind:      KindAgentStateChanged,
				Transition: &Transition{
					PrevState: "waiting_approval", NewState: "processing",
				},
			},
		}
		anom := detectAnomalies(events)
		found := false
		for _, a := range anom {
			if strings.Contains(a, "waiting_approval") {
				found = true
			}
		}
		if !found {
			t.Errorf("expected waiting_approval anomaly, got %v", anom)
		}
	})
}
