package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFormatEventDetailsAllBuiltInKinds(t *testing.T) {
	transition := &Transition{
		PrevStatus: "processing", NewStatus: "failed", Cause: "test-cause",
		CancelSource: "scheduler", RetryCount: 2,
	}
	cases := []struct {
		name string
		ev   Event
		want []string
	}{
		{"task_published", Event{Kind: KindTaskPublished, PublishedBy: "scheduler", Dependencies: []string{"dep"}, EventType: "worker", Priority: "high", Depth: 2, ToolsOverride: []string{"read_file", "web_fetch"}, ModelOverride: "deepseek-r1", IsolationOverride: "workspace", Description: "work"}, []string{"by=scheduler", "deps=[dep]", "type=worker", "priority=high", "depth=2", "tools_override=[read_file web_fetch]", "model_override=deepseek-r1", "isolation_override=workspace", `desc="work"`}},
		{"task_claimed", Event{Kind: KindTaskClaimed, Transition: transition}, []string{"prev=processing", "new=failed", "cause=test-cause"}},
		{"task_submitted", Event{Kind: KindTaskSubmitted, OutputLen: 10, LoopsUsed: 2}, []string{"output_len=10", "loops_used=2"}},
		{"task_completed", Event{Kind: KindTaskCompleted, OutputLen: 10, LoopsUsed: 2, Transition: transition}, []string{"cause=test-cause", "output_len=10", "loops_used=2"}},
		{"text_only_submission", Event{Kind: KindTextOnlySubmission, OutputLen: 11, LoopsUsed: 3}, []string{"output_len=11", "loops_used=3"}},
		{"task_retry", Event{Kind: KindTaskRetry, AttemptNo: 2, Reason: "retry", Transition: transition}, []string{"retry=2", "attempt=2", `reason="retry"`}},
		{"task_failed", Event{Kind: KindTaskFailed, Reason: "failed", Transition: transition}, []string{"retry=2", `reason="failed"`}},
		{"task_blocked", Event{Kind: KindTaskBlocked, Reason: "no compatible route", Transition: &Transition{PrevStatus: "pending", NewStatus: "blocked", Cause: "system_blocked"}}, []string{"prev=pending", "new=blocked", "cause=system_blocked", `reason="no compatible route"`}},
		{"task_cancelled", Event{Kind: KindTaskCancelled, Reason: "cancelled", Transition: transition}, []string{"source=scheduler", "cause=test-cause", `reason="cancelled"`}},
		{"llm_call_start", Event{Kind: KindLLMCallStart, HistoryEntries: 4, ToolCallsCount: 7}, []string{"history_entries=4", "tools=7"}},
		{"llm_call_end", Event{Kind: KindLLMCallEnd, DurationMS: 12, PromptTokens: 13, CompletionTokens: 14, ToolCallsCount: 1, FinishReason: "tool_calls"}, []string{"duration=12ms", "prompt_tokens=13", "completion_tokens=14", "tool_calls=1", "finish_reason=tool_calls"}},
		{"tool_call", Event{Kind: KindToolCall, Tool: "read_file", CallID: "call-1", Args: map[string]any{"path": "a.go"}}, []string{"tool=read_file", "call_id=call-1", `args={"path":"a.go"}`}},
		{"tool_result", Event{Kind: KindToolResult, Tool: "read_file", CallID: "call-1", Args: map[string]any{"path": "a.go"}, DurationMS: 8, ResultLen: 99}, []string{"tool=read_file", "duration=8ms", "call_id=call-1", `args={"path":"a.go"}`, "result_len=99"}},
		{"history_compaction", Event{Kind: KindHistoryCompaction, PromptTokensBefore: 100, PromptTokensAfter: 60, Strategy: "summary", KeptEntries: 4}, []string{"tokens_before=100", "tokens_after=60", "strategy=summary", "kept_entries=4"}},
		{"context_manifest_built", Event{Kind: KindContextManifestBuilt, PromptTokens: 4321, HistoryEntries: 6, PromptBuildID: "pb-abc123def456", Description: `[{"id":"system_prompt","source":"agent-prompt"}]`}, []string{"est_prompt_tokens=4321", "history_entries=6", "build=pb-abc123def456", `sections="[{`}},
		{"prompt_compiled", Event{Kind: KindPromptCompiled, TaskID: "t1", PromptBuildID: "pb-abc123def456", Description: `[{"id":"agent_role","digest":"abc123def456","in_message":true}]`}, []string{"build=pb-abc123def456", `components="[{`}},
		{"agent_audit_started", Event{Kind: KindAgentAuditStarted, TaskID: "t1", Description: `{"agents":3,"snapshot_digest":"abc123def456","warnings":0}`}, []string{`summary="{\"agents\":3`}},
		{"agent_audit_warning", Event{Kind: KindAgentAuditWarning, TaskID: "t1", Description: `{"agent":"explorer","type":"route_missing"}`}, []string{`summary="{\"agent\":\"explorer\"`}},
		{"agent_audit_completed", Event{Kind: KindAgentAuditCompleted, TaskID: "t1", Reason: "completed", Description: `{"agents":3,"warnings":1}`}, []string{`reason="completed"`, `summary="{\"agents\":3`}},
		{"file_written", Event{Kind: KindFileWritten, Tool: "write_file", Path: "a.go", Bytes: 42, Hash: "full-hash"}, []string{"path=a.go", "bytes=42", "hash=full-hash", "tool=write_file"}},
		{"file_write_queued", Event{Kind: KindFileWriteQueued, Path: "a.go", QueueLen: 2, WaitMS: 15, Description: "acquired"}, []string{"path=a.go", "queue_len=2", "wait_ms=15", `desc="acquired"`}},
		{"progress_notify", Event{Kind: KindProgressNotify, NotifyType: "halfway"}, []string{"notify_type=halfway"}},
		{"workspace_materialized", Event{Kind: KindWorkspaceMaterialized, Path: "/proj/.agentgo/workspaces/t1"}, []string{"path=/proj/.agentgo/workspaces/t1"}},
		{"workspace_merged", Event{Kind: KindWorkspaceMerged, Description: "fast_forward=2 auto_merged=1"}, []string{`desc="fast_forward=2 auto_merged=1"`}},
		{"workspace_merge_conflict", Event{Kind: KindWorkspaceMergeConflict, Path: "/proj/a.go", Description: "regions=2"}, []string{"path=/proj/a.go", `desc="regions=2"`}},
		{"workspace_cleaned", Event{Kind: KindWorkspaceCleaned, Path: "/proj/.agentgo/workspaces/t1"}, []string{"path=/proj/.agentgo/workspaces/t1"}},
		{"error", Event{Kind: KindError, Error: "boom", Reason: "reactor"}, []string{`error="boom"`, `reason="reactor"`}},
		{"agent_state_changed", Event{Kind: KindAgentStateChanged, Transition: &Transition{PrevState: "idle", NewState: "processing", Cause: "claim"}}, []string{"prev=idle", "new=processing", "cause=claim"}},
		{"shell_executed", Event{Kind: KindShellExecuted, Tool: "run_shell", Args: map[string]any{"command": "go test"}, ShellExec: &ShellExec{Command: "go test", ExitCode: 0, DurationMS: 9, Outcome: "success", StdoutExcerpt: "ok", StderrExcerpt: "warn"}}, []string{`cmd="go test"`, "exit=0", "outcome=success", `stdout="ok"`, `stderr="warn"`, "tool=run_shell"}},
		{"shell_timeout_pending", Event{Kind: KindShellTimeoutPending, ShellTimeout: &ShellTimeout{Command: "go test", ElapsedSec: 30, PreviousWaits: 1, StdoutExcerpt: "partial"}}, []string{"elapsed=30s", "waits=1", `stdout="partial"`}},
		{"shell_timeout_resolved", Event{Kind: KindShellTimeoutResolved, ShellTimeout: &ShellTimeout{Command: "go test", ElapsedSec: 60, PreviousWaits: 2, Decision: "wait", ExtraSeconds: 20}}, []string{"elapsed=60s", "waits=2", "decision=wait", "extra=20s"}},
		{"reactor_spawn_depth_exceeded", Event{Kind: KindReactorSpawnDepthExceeded, Depth: 6, Reason: "too deep"}, []string{"depth=6", `reason="too deep"`}},
		{"runtime_loop_fuse_triggered", Event{Kind: KindRuntimeLoopFuseTriggered, Loop: 10000, Reason: "fuse"}, []string{"loop=10000", `reason="fuse"`}},
		{"task_finalizing", Event{Kind: KindTaskFinalizing, TaskID: "t1", Transition: &Transition{PrevStatus: "processing", NewStatus: "blocked"}}, []string{"status=blocked"}},
		{"tool_call_skipped", Event{Kind: KindToolCallSkipped, TaskID: "t1", Tool: "write_file", CallID: "call-9", Reason: "task_finalizing"}, []string{"tool=write_file", "call_id=call-9", `reason="task_finalizing"`}},
		{"task_result_committed", Event{Kind: KindTaskResultCommitted, TaskID: "t1", Reason: "缺权限", Transition: &Transition{PrevStatus: "processing", NewStatus: "blocked", Cause: "agent_reported_blocked"}}, []string{"prev=processing", "new=blocked", "cause=agent_reported_blocked", `reason="缺权限"`}},
		{"execution_lease_frozen", Event{Kind: KindExecutionLeaseFrozen, TaskID: "t1", Lease: &LeasePayload{Digest: "abc123def456", BusinessTools: 3, ControlTools: 1, Model: "deepseek-r1", Workspace: "workspace", Synthetic: true, Attempt: 1}}, []string{"digest=abc123def456", "biz=3 ctl=1", "model=deepseek-r1", "workspace=workspace", "synthetic=true"}},
		{"execution_lease_rejected", Event{Kind: KindExecutionLeaseRejected, TaskID: "t1", Reason: "节点能力工具子集越界", Lease: &LeasePayload{Cause: "节点能力工具子集越界", Missing: []string{"web_fetch"}}}, []string{"missing=[web_fetch]", `cause="节点能力工具子集越界"`, `reason="节点能力工具子集越界"`}},
		{"execution_lease_reused", Event{Kind: KindExecutionLeaseReused, TaskID: "t1", Lease: &LeasePayload{Digest: "abc123def456", BusinessTools: 2, ControlTools: 2, Attempt: 1}}, []string{"digest=abc123def456", "biz=2 ctl=2"}},
		{"execution_lease_revoked", Event{Kind: KindExecutionLeaseRevoked, TaskID: "t1", Lease: &LeasePayload{Digest: "abc123def456", BusinessTools: 3, ControlTools: 1, Cause: "finalizing_accepted"}}, []string{"digest=abc123def456", "biz=3 ctl=1", `cause="finalizing_accepted"`}},
		{"graph_submitted", Event{Kind: KindGraphSubmitted, GraphID: "graph-1", Description: "revision=1"}, []string{"graph=graph-1", `desc="revision=1"`}},
		{"graph_submission_rejected", Event{Kind: KindGraphSubmissionRejected, GraphID: "graph-1", Error: "校验失败"}, []string{"graph=graph-1", `error="校验失败"`}},
		{"node_activation_created", Event{Kind: KindNodeActivationCreated, GraphID: "graph-1", NodeID: "implement", ActivationID: "implement@2"}, []string{"graph=graph-1", "node=implement", "activation=implement@2"}},
		{"graph_transition_selected", Event{Kind: KindGraphTransitionSelected, GraphID: "graph-1", NodeID: "verify", ActivationID: "verify@1", Description: "next[1] -> implement"}, []string{"graph=graph-1", "node=verify", "activation=verify@1", `desc="next[1] -> implement"`}},
		{"graph_ended", Event{Kind: KindGraphEnded, GraphID: "graph-1", Reason: "节点无出路"}, []string{"graph=graph-1", `reason="节点无出路"`}},
		{"graph_join_resolved", Event{Kind: KindGraphJoinResolved, GraphID: "graph-1", NodeID: "merge", ActivationID: "merge@1", Description: "生效入边 2/2"}, []string{"graph=graph-1", "node=merge", "activation=merge@1", `desc="生效入边 2/2"`}},
		{"graph_wait_started", Event{Kind: KindGraphWaitStarted, GraphID: "graph-1", NodeID: "wait", ActivationID: "wait@1", Description: "event=deploy.done"}, []string{"graph=graph-1", "node=wait", "activation=wait@1", `desc="event=deploy.done"`}},
		{"graph_wait_resumed", Event{Kind: KindGraphWaitResumed, GraphID: "graph-1", NodeID: "wait", ActivationID: "wait@1", Description: "event=deploy.done"}, []string{"graph=graph-1", "node=wait", "activation=wait@1", `desc="event=deploy.done"`}},
		{"graph_approval_decided", Event{Kind: KindGraphApprovalDecided, GraphID: "graph-1", NodeID: "approve", ActivationID: "approve@1", Description: "approved"}, []string{"graph=graph-1", "node=approve", "activation=approve@1", `desc="approved"`}},
		{"graph_change_requested", Event{Kind: KindGraphChangeRequested, GraphID: "graph-1", NodeID: "implement", ActivationID: "implement@2", TaskID: "task-1", Reason: "route_missing", Description: "verify 无可用路由"}, []string{"graph=graph-1", "node=implement", "activation=implement@2", `reason="route_missing"`, `desc="verify 无可用路由"`}},
		{"graph_revision_committed", Event{Kind: KindGraphRevisionCommitted, GraphID: "graph-1", Description: "new_revision=2 upsert=[implement]"}, []string{"graph=graph-1", `desc="new_revision=2 upsert=[implement]"`}},
		{"task_memory_created", Event{Kind: KindTaskMemoryCreated, Description: `{"version":1,"actions":0}`}, []string{`sections="{\"version\":1`}},
		{"task_memory_updated", Event{Kind: KindTaskMemoryUpdated, Loop: 2, Description: `{"version":3,"actions":2}`}, []string{`sections="{\"version\":3`}},
		{"task_memory_checkpointed", Event{Kind: KindTaskMemoryCheckpointed, Loop: -1, Reason: "terminal:completed", Description: `{"version":5,"sealed":true}`}, []string{`reason="terminal:completed"`, `sections="{\"version\":5`}},
		{"session_memory_promotion_proposed", Event{Kind: KindSessionMemoryPromotionProposed, TaskID: "t1", Reason: "completed", Description: `{"version":5,"sealed":true}`}, []string{`reason="completed"`, `summary="{\"version\":5`}},
		{"session_memory_promotion_decided", Event{Kind: KindSessionMemoryPromotionDecided, TaskID: "t1", Reason: "completed", Description: `{"decided":"promoted","entries":2}`}, []string{`reason="completed"`, `summary="{\"decided\":\"promoted\"`}},
		{"memory_recalled", Event{Kind: KindMemoryRecalled, TaskID: "t2", Description: `{"entries":2,"keys":["task_result:result:t1:confirmed"]}`}, []string{`summary="{\"entries\":2`}},
		{"memory_entry_state_changed", Event{Kind: KindMemoryEntryStateChanged, TaskID: "t1", Description: `{"key":"decision:ab12","new_state":"superseded"}`}, []string{`summary="{\"key\":\"decision:ab12\"`}},
		{"suggestions_returned", Event{Kind: KindSuggestionsReturned, TaskID: "t1", Suggestion: &SuggestionPayload{SuggestionID: "require-read-before-write:read_before_write:ab12cd34", ReasonCode: "read_before_write", Retryable: true, Offered: 1, Filtered: 1, FilterReason: "finalizing", RepeatCount: 2}}, []string{"reason_code=read_before_write", "retryable=true", "offered=1", "filtered=1", "filter=finalizing", "repeat=2"}},
		{"suggestion_disposition", Event{Kind: KindSuggestionDisposition, TaskID: "t1", Suggestion: &SuggestionPayload{SuggestionID: "require-read-before-write:read_before_write:ab12cd34", ReasonCode: "read_before_write", Disposition: "adopted"}}, []string{"id=require-read-before-write:read_before_write:ab12cd34", "disposition=adopted", "reason_code=read_before_write"}},
		{"effect_prepared", Event{Kind: KindEffectPrepared, TaskID: "t1", Effect: &EffectPayload{EffectID: "t1-1", Kind: "file_write", Policy: "verify_first", Status: "prepared", Target: "/proj/a.go", ArgsDigest: "ab12cd34ef56"}}, []string{"effect=t1-1", "kind=file_write", "policy=verify_first", "target=/proj/a.go"}},
		{"effect_settled", Event{Kind: KindEffectSettled, TaskID: "t1", Effect: &EffectPayload{EffectID: "t1-2", Kind: "shell", Policy: "manual_only", Status: "settled", Target: "cmd:ab12cd34ef56", ResultSummary: "exit_code=0 outcome=success"}}, []string{"effect=t1-2", "kind=shell", "policy=manual_only", `result="exit_code=0 outcome=success"`}},
		{"effect_unknown", Event{Kind: KindEffectUnknown, TaskID: "t1", Effect: &EffectPayload{EffectID: "t1-3", Kind: "message", Policy: "manual_only", Status: "unknown", Target: "worker-1", Reason: "进程在副作用执行窗口退出"}}, []string{"effect=t1-3", "kind=message", `reason="进程在副作用执行窗口退出"`}},
		{"effect_recovery_decided", Event{Kind: KindEffectRecoveryDecided, TaskID: "t1", Effect: &EffectPayload{EffectID: "t1-1", Kind: "file_write", Policy: "verify_first", Decision: "verified_settled", Reason: "文件 hash 与账载一致"}}, []string{"effect=t1-1", "decision=verified_settled", `reason="文件 hash 与账载一致"`}},
		{"acceptance_completed", Event{Kind: KindAcceptanceCompleted, GraphID: "graph-1", NodeID: "verify", ActivationID: "verify@1", TaskID: "task-9", Acceptance: &AcceptancePayload{Verdict: "pass", Status: "disputed", Checked: 2, Reason: "命令未在该任务的 shell 账中找到"}}, []string{"graph=graph-1", "node=verify", "activation=verify@1", "verdict=pass", "verify=disputed", "checked=2", `reason="命令未在该任务的 shell 账中找到"`}},
	}
	if len(cases) != 65 {
		t.Fatalf("test inventory has %d built-in EventKinds, want 65", len(cases))
	}
	seen := make(map[string]struct{}, len(cases))
	for _, tc := range cases {
		if tc.name != string(tc.ev.Kind) {
			t.Fatalf("formatter case name %q does not match Event.Kind %q", tc.name, tc.ev.Kind)
		}
		if _, exists := seen[tc.name]; exists {
			t.Fatalf("duplicate EventKind formatter case %q", tc.name)
		}
		seen[tc.name] = struct{}{}
		t.Run(tc.name, func(t *testing.T) {
			got := formatEventDetails(tc.ev)
			if got == "" {
				t.Fatal("formatter returned empty details")
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("details %q missing %q", got, want)
				}
			}
		})
	}
	actual := eventKindValuesFromSource(t)
	if len(actual) != len(seen) {
		t.Fatalf("event.go declares %d unique EventKinds, formatter corpus has %d", len(actual), len(seen))
	}
	for kind := range actual {
		if _, ok := seen[kind]; !ok {
			t.Errorf("EventKind %q exists in event.go but is missing from formatter corpus", kind)
		}
	}
	for kind := range seen {
		if _, ok := actual[kind]; !ok {
			t.Errorf("formatter corpus contains non-existent EventKind %q", kind)
		}
	}
	if !eventCarriesLoop(KindTaskCancelled) {
		t.Error("task_cancelled must expose its loop field in CLI timelines")
	}
}

func TestFormatEventDetailsFallbacks(t *testing.T) {
	user := formatEventDetails(Event{Kind: "user.audit", Description: "custom payload", Reason: "why"})
	for _, want := range []string{`desc="custom payload"`, `reason="why"`} {
		if !strings.Contains(user, want) {
			t.Fatalf("user event details %q missing %q", user, want)
		}
	}
	parseErr := formatEventDetails(Event{Kind: "<parse_error>", Error: "invalid JSON"})
	if !strings.Contains(parseErr, `error="invalid JSON"`) {
		t.Fatalf("parse error details hidden: %q", parseErr)
	}
	// V6 legacy 姿态：旧 JSONL 里的 plan_/replan_ 事件行按未知 kind 优雅渲染——
	// 不崩、原样展示 kind 字符串，通用字段（reason 等）仍可见，不做专用格式化。
	//（acceptance_completed 已于 G1b 重新启用为 Graph 验收核验事件，不再是
	// legacy kind，渲染语料见 TestFormatEventDetailsAllBuiltInKinds。）
	for _, legacyKind := range []string{"plan_terminal", "plan_paused", "replan_requested"} {
		got := formatEventDetails(Event{Kind: EventKind(legacyKind), Reason: "terminal fact"})
		if !strings.Contains(got, `reason="terminal fact"`) {
			t.Fatalf("legacy kind %q 的通用字段未展示: %q", legacyKind, got)
		}
		if strings.Contains(got, "plan=") || strings.Contains(got, "plan_revision=") {
			t.Fatalf("legacy kind %q 不应再有 Plan 专用格式化: %q", legacyKind, got)
		}
	}
}

func TestSummarizeLifecycleAndCounters(t *testing.T) {
	base := time.Date(2026, 7, 18, 1, 2, 3, 0, time.UTC)
	cases := []struct {
		name       string
		events     []Event
		wantStatus string
		wantLoops  int
		wantErrors int
		wantAgent  string
	}{
		{
			name: "failed loop and nonterminal error",
			events: []Event{
				{Timestamp: base, Kind: KindTaskClaimed, TaskID: "failed-task", AgentID: "worker-1"},
				{Timestamp: base.Add(time.Second), Kind: KindLLMCallStart, TaskID: "failed-task", AgentID: "worker-1", Loop: 0},
				{Timestamp: base.Add(2 * time.Second), Kind: KindError, TaskID: "failed-task", Error: "diagnostic"},
				{Timestamp: base.Add(3 * time.Second), Kind: KindTaskFailed, TaskID: "failed-task"},
			},
			wantStatus: "failed", wantLoops: 1, wantErrors: 1,
		},
		{
			name: "cancelled third loop",
			events: []Event{
				{Timestamp: base, Kind: KindTaskClaimed, TaskID: "cancel-task", AgentID: "worker-2"},
				{Timestamp: base.Add(time.Second), Kind: KindLLMCallStart, TaskID: "cancel-task", Loop: 2},
				{Timestamp: base.Add(2 * time.Second), Kind: KindTaskCancelled, TaskID: "cancel-task"},
			},
			wantStatus: "cancelled", wantLoops: 1,
		},
		{
			name: "system blocked is terminal",
			events: []Event{
				{Timestamp: base, Kind: KindTaskPublished, TaskID: "blocked-task"},
				{Timestamp: base.Add(time.Second), Kind: KindTaskBlocked, TaskID: "blocked-task"},
			},
			wantStatus: "blocked",
		},
		{
			name: "retry then reclaimed",
			events: []Event{
				{Timestamp: base, Kind: KindTaskPublished, TaskID: "retry-task"},
				{Timestamp: base.Add(time.Second), Kind: KindTaskClaimed, TaskID: "retry-task", AgentID: "worker-1"},
				{Timestamp: base.Add(2 * time.Second), Kind: KindTaskRetry, TaskID: "retry-task"},
				{Timestamp: base.Add(3 * time.Second), Kind: KindTaskClaimed, TaskID: "retry-task", AgentID: "worker-2"},
			},
			wantStatus: "processing", wantAgent: "worker-2",
		},
		{
			name: "retry waiting",
			events: []Event{
				{Timestamp: base, Kind: KindTaskClaimed, TaskID: "retry-wait"},
				{Timestamp: base.Add(time.Second), Kind: KindTaskRetry, TaskID: "retry-wait"},
			},
			wantStatus: "pending(retry)",
		},
		{
			name: "legacy trace falls back to loops used",
			events: []Event{
				{Timestamp: base, Kind: KindTaskSubmitted, TaskID: "legacy-loops", LoopsUsed: 4},
			},
			wantStatus: "unknown", wantLoops: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTraceFixture(t, dir, base, tc.events[0].TaskID, tc.events)
			row := summarize(taskFile{path: path, filename: filepath.Base(path), taskShortID: shortIdentifier(tc.events[0].TaskID), publishedAt: base})
			if row.status != tc.wantStatus || row.loops != tc.wantLoops || row.errors != tc.wantErrors {
				t.Fatalf("summary=%+v, want status=%s loops=%d errors=%d", row, tc.wantStatus, tc.wantLoops, tc.wantErrors)
			}
			if tc.wantAgent != "" && row.agentID != tc.wantAgent {
				t.Fatalf("summary agent=%q, want latest claimant %q", row.agentID, tc.wantAgent)
			}
		})
	}
}

func TestTaskAggregationAcrossRetryFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 18, 2, 0, 0, 0, time.UTC)
	taskID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	writeTraceFixture(t, dir, base, taskID, []Event{
		{Timestamp: base, Kind: KindTaskPublished, TaskID: taskID},
		{Timestamp: base, Kind: KindLLMCallStart, TaskID: taskID, Loop: 0},
		{Timestamp: base, Kind: KindToolCall, TaskID: taskID, Tool: "read_file", CallID: "first-fragment"},
		{Timestamp: base, Kind: KindTaskRetry, TaskID: taskID, AttemptNo: 1},
	})
	// Retry 分片与首片按完整 TaskID 归并为同一任务组。
	writeTraceFixture(t, dir, base.Add(time.Second), taskID, []Event{
		{Timestamp: base, Kind: KindTaskClaimed, TaskID: taskID},
		{Timestamp: base, Kind: KindLLMCallStart, TaskID: taskID, Loop: 0},
		{Timestamp: base, Kind: KindToolCall, TaskID: taskID, Tool: "read_file", CallID: "second-fragment"},
		{Timestamp: base, Kind: KindFileWritten, TaskID: taskID, Tool: "write_file", Path: "out.txt"},
		{Timestamp: base, Kind: KindTaskCompleted, TaskID: taskID},
	})

	files, err := listTaskFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	groups := groupTraceFiles(loadTraceFiles(files))
	if len(groups) != 1 {
		t.Fatalf("groups=%d, want one complete TaskID group", len(groups))
	}
	if len(groups[0].files) != 2 {
		t.Fatalf("trace files=%d, want 2", len(groups[0].files))
	}
	summary := summarizeTask(groups[0])
	if summary.status != "completed" || summary.loops != 2 || summary.filesWritten != 1 {
		t.Fatalf("summary=%+v, want completed loops=2 files=1", summary)
	}

	var list bytes.Buffer
	if err := CLI([]string{"list"}, dir, "", &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list.String(), "共 1 个任务") || strings.Count(list.String(), shortIdentifier(taskID)) != 1 {
		t.Fatalf("retry shards were listed as multiple tasks:\n%s", list.String())
	}

	var show bytes.Buffer
	if err := CLI([]string{"show", taskID}, dir, "", &show); err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"Trace Files: 2", "Events: 9", "first-fragment", "second-fragment", "loops=2", "tool=write_file"} {
		if !strings.Contains(show.String(), want) {
			t.Errorf("show missing %q:\n%s", want, show.String())
		}
	}
	if strings.Index(show.String(), "first-fragment") > strings.Index(show.String(), "second-fragment") {
		t.Fatalf("equal timestamps were not sorted by filename then line:\n%s", show.String())
	}
}

func TestPhysicalFileWithMultipleTaskIDsIsSplit(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 18, 2, 30, 0, 0, time.UTC)
	firstID := "cccccccc-1111-1111-1111-111111111111"
	secondID := "cccccccc-2222-2222-2222-222222222222"
	writeTraceFixture(t, dir, base, firstID, []Event{
		{Timestamp: base, Kind: KindToolCall, TaskID: firstID, Tool: "read_file", CallID: "ONLY-FIRST"},
		{Timestamp: base.Add(time.Millisecond), Kind: KindToolCall, TaskID: secondID, Tool: "read_file", CallID: "ONLY-SECOND"},
	})

	files, err := listTaskFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	groups := groupTraceFiles(loadTraceFiles(files))
	if len(groups) != 2 {
		t.Fatalf("one colliding physical file produced %d task groups, want 2", len(groups))
	}
	for _, tc := range []struct {
		id, own, other string
	}{{firstID, "ONLY-FIRST", "ONLY-SECOND"}, {secondID, "ONLY-SECOND", "ONLY-FIRST"}} {
		var out bytes.Buffer
		if err := CLI([]string{"show", tc.id}, dir, "", &out); err != nil {
			t.Fatalf("show %s: %v", tc.id, err)
		}
		if !strings.Contains(out.String(), tc.own) || strings.Contains(out.String(), tc.other) {
			t.Fatalf("Task timelines leaked across a shared physical file:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "Trace Files: 1") {
			t.Fatalf("show missing physical file count:\n%s", out.String())
		}
	}
}

func TestParseErrorAttributionAndSyntheticFileGroups(t *testing.T) {
	base := time.Date(2026, 7, 18, 2, 45, 0, 0, time.UTC)
	marshalLine := func(ev Event) string {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("single Task file attributes parse error", func(t *testing.T) {
		dir := t.TempDir()
		taskID := "dddddddd-1111-1111-1111-111111111111"
		writeRawTraceFixture(t, dir, base, "dddddddd", []string{
			marshalLine(Event{Timestamp: base, Kind: KindToolCall, TaskID: taskID, CallID: "VALID"}),
			`{"broken":`,
			marshalLine(Event{Timestamp: base.Add(time.Second), Kind: KindToolCall, TaskID: taskID, CallID: "AFTER"}),
		})
		groups := mustTaskGroups(t, dir)
		if len(groups) != 1 || len(groups[0].records) != 3 {
			t.Fatalf("groups=%d records=%d, parse error should belong to unique TaskID", len(groups), len(groups[0].records))
		}
		summary := summarizeTask(groups[0])
		if summary.errors != 1 || summary.status != "malformed" {
			t.Fatalf("summary=%+v, want malformed with one error", summary)
		}
		var out bytes.Buffer
		if err := CLI([]string{"show", taskID}, dir, "", &out); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"WARNING: timeline incomplete", "<parse_error>", `error="invalid JSON:`} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("show missing %q:\n%s", want, out.String())
			}
		}
		validPos := strings.Index(out.String(), "call_id=VALID")
		parsePos := strings.Index(out.String(), "<parse_error>")
		afterPos := strings.Index(out.String(), "call_id=AFTER")
		if validPos < 0 || parsePos <= validPos || afterPos <= parsePos {
			t.Fatalf("parse error lost its physical line position:\n%s", out.String())
		}
	})

	t.Run("multi Task file does not leak unowned parse error", func(t *testing.T) {
		dir := t.TempDir()
		firstID := "eeeeeeee-1111-1111-1111-111111111111"
		secondID := "eeeeeeee-2222-2222-2222-222222222222"
		writeRawTraceFixture(t, dir, base, "eeeeeeee", []string{
			marshalLine(Event{Timestamp: base, Kind: KindToolCall, TaskID: firstID, CallID: "FIRST"}),
			`{"broken":`,
			marshalLine(Event{Timestamp: base, Kind: KindToolCall, TaskID: secondID, CallID: "SECOND"}),
		})
		groups := mustTaskGroups(t, dir)
		if len(groups) != 2 {
			t.Fatalf("groups=%d, want 2", len(groups))
		}
		for _, group := range groups {
			if len(group.records) != 1 || summarizeTask(group).errors != 1 {
				t.Fatalf("group %s records=%d summary=%+v", group.displayID(), len(group.records), summarizeTask(group))
			}
			var out bytes.Buffer
			if err := CLI([]string{"show", group.taskID}, dir, "", &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "WARNING: timeline incomplete") || strings.Contains(out.String(), "<parse_error>") {
				t.Fatalf("unowned parse event leaked into Task timeline:\n%s", out.String())
			}
		}
	})

	t.Run("no-task KindError is not double counted", func(t *testing.T) {
		dir := t.TempDir()
		taskID := "abababab-1111-1111-1111-111111111111"
		writeRawTraceFixture(t, dir, base, "abababab", []string{
			marshalLine(Event{Timestamp: base, Kind: KindTaskPublished, TaskID: taskID}),
			marshalLine(Event{Timestamp: base.Add(time.Second), Kind: KindError, Error: "missing task id"}),
		})
		groups := mustTaskGroups(t, dir)
		if len(groups) != 1 {
			t.Fatalf("groups=%d, want 1", len(groups))
		}
		summary := summarizeTask(groups[0])
		if summary.errors != 1 || summary.status != "malformed" {
			t.Fatalf("summary=%+v, no-task KindError should count exactly once", summary)
		}
	})

	t.Run("synthetic terminal cannot claim a trusted status", func(t *testing.T) {
		dir := t.TempDir()
		writeRawTraceFixture(t, dir, base, "12121212", []string{
			marshalLine(Event{Timestamp: base, Kind: KindTaskCompleted}),
		})
		groups := mustTaskGroups(t, dir)
		if len(groups) != 1 {
			t.Fatalf("groups=%d, want 1", len(groups))
		}
		summary := summarizeTask(groups[0])
		if summary.status != "malformed" || summary.errors != 1 {
			t.Fatalf("synthetic terminal summary=%+v, want malformed/errors=1", summary)
		}
	})

	t.Run("empty trace file is malformed", func(t *testing.T) {
		dir := t.TempDir()
		writeRawTraceFixture(t, dir, base, "34343434", nil)
		groups := mustTaskGroups(t, dir)
		if len(groups) != 1 {
			t.Fatalf("groups=%d, want 1", len(groups))
		}
		summary := summarizeTask(groups[0])
		if summary.status != "malformed" || summary.errors != 1 || len(groups[0].issues) != 1 {
			t.Fatalf("empty trace summary=%+v issues=%+v", summary, groups[0].issues)
		}
	})

	t.Run("files with no TaskID stay separate", func(t *testing.T) {
		dir := t.TempDir()
		writeRawTraceFixture(t, dir, base, "ffffffff", []string{`{"broken":`})
		writeRawTraceFixture(t, dir, base.Add(time.Second), "ffffffff", []string{`{"also_broken":`})
		groups := mustTaskGroups(t, dir)
		if len(groups) != 2 || groups[0].taskID != "" || groups[1].taskID != "" || groups[0].key == groups[1].key {
			t.Fatalf("no-Task files were merged: %#v", groups)
		}
		if groups[0].displayID() == groups[1].displayID() ||
			!strings.HasPrefix(groups[0].displayID(), "file-") || !strings.HasPrefix(groups[1].displayID(), "file-") {
			t.Fatalf("synthetic IDs are not unique namespaced tokens: %q %q", groups[0].displayID(), groups[1].displayID())
		}
	})
}

func TestReadEventStreamSupportsLongLinesAndReturnsPartialEvents(t *testing.T) {
	longDescription := strings.Repeat("x", 5<<20)
	line, err := json.Marshal(Event{Kind: KindTaskPublished, TaskID: "long-task", Description: longDescription})
	if err != nil {
		t.Fatal(err)
	}
	events, err := readEventStream(bytes.NewReader(append(line, '\n')))
	if err != nil || len(events) != 1 || len(events[0].Description) != len(longDescription) {
		t.Fatalf("long-line read events=%d desc=%d err=%v", len(events), len(events[0].Description), err)
	}

	sentinel := errors.New("injected read failure")
	partialLine, err := json.Marshal(Event{Kind: KindError, TaskID: "partial-task", Error: "kept"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &errorAfterDataReader{data: append(partialLine, '\n'), err: sentinel}
	partial, err := readEventStream(reader)
	if !errors.Is(err, sentinel) || len(partial) != 1 || partial[0].TaskID != "partial-task" {
		t.Fatalf("partial read events=%+v err=%v", partial, err)
	}
}

func TestCmdShowUsesFullTaskID(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 18, 4, 0, 0, 0, time.UTC)
	firstID := "abcdef12-1111-1111-1111-111111111111"
	secondID := "abcdef12-2222-2222-2222-222222222222"
	writeTraceFixture(t, dir, base, firstID, []Event{{Timestamp: base, Kind: KindError, TaskID: firstID, Error: "first-only"}})
	writeTraceFixture(t, dir, base.Add(time.Second), secondID, []Event{{
		Timestamp: base.Add(time.Second), Kind: KindHistoryCompaction, TaskID: secondID, AgentID: "worker", Loop: 0,
		PromptTokensBefore: 100, PromptTokensAfter: 50,
	}})
	var ambiguous bytes.Buffer
	if err := CLI([]string{"show", "abcdef12"}, dir, "", &ambiguous); err != nil {
		t.Fatalf("CLI show ambiguous prefix: %v", err)
	}
	for _, want := range []string{"找到 2 个匹配的任务", firstID, secondID} {
		if !strings.Contains(ambiguous.String(), want) {
			t.Errorf("ambiguous task output missing %q:\n%s", want, ambiguous.String())
		}
	}
	var out bytes.Buffer
	if err := CLI([]string{"show", secondID}, dir, "", &out); err != nil {
		t.Fatalf("CLI show: %v", err)
	}
	got := out.String()
	for _, want := range []string{"history_compaction", "loop=0", "tokens_after=50"} {
		if !strings.Contains(got, want) {
			t.Errorf("show output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "first-only") {
		t.Fatalf("full TaskID did not disambiguate colliding short IDs:\n%s", got)
	}
}

func TestMatchTaskGroupsSyntheticIDNeverSilentlyShadowsPrefix(t *testing.T) {
	synthetic := &taskTrace{syntheticID: "file-abcdef12"}
	real := &taskTrace{taskID: "file-abcdef12-real-task"}
	matches, err := matchTaskGroups([]*taskTrace{real, synthetic}, synthetic.syntheticID)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("synthetic ID collision was silently resolved: %#v", matches)
	}
}

func TestTraceCLIUsage(t *testing.T) {
	var out bytes.Buffer
	if err := CLI([]string{"help"}, t.TempDir(), "", &out); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"show <task_id>", "stats [task|agent]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q:\n%s", want, out.String())
		}
	}
	// V6：plan 子命令已随 Plan 控制面删除，按未知子命令报错。
	if err := CLI([]string{"plan", "anything"}, t.TempDir(), "", &out); err == nil ||
		!strings.Contains(err.Error(), "unknown trace subcommand") {
		t.Fatalf("plan 子命令应报未知子命令错误，err=%v", err)
	}
	// stats 分组维度只余 task / agent；plan 维度已删除。
	if err := CLI([]string{"stats", "plan"}, t.TempDir(), "", &out); err == nil ||
		!strings.Contains(err.Error(), "stats [task|agent]") {
		t.Fatalf("stats plan 应报用法错误，err=%v", err)
	}
}

func TestFitColumnKeepsFixedWidth(t *testing.T) {
	for _, tc := range []struct {
		input string
		width int
		want  string
	}{
		{"worker-1", 8, "worker-1"},
		{"explorer-1", 8, "explo..."},
		{"abcdef", 3, "abc"},
		{"anything", 0, ""},
	} {
		if got := fitColumn(tc.input, tc.width); got != tc.want {
			t.Errorf("fitColumn(%q, %d)=%q, want %q", tc.input, tc.width, got, tc.want)
		}
	}
}

func writeTraceFixture(t *testing.T, dir string, fileTime time.Time, taskID string, events []Event) string {
	t.Helper()
	name := fmt.Sprintf("%s_%s.jsonl", fileTime.Format("2006-01-02T15-04-05"), shortIdentifier(taskID))
	path := filepath.Join(dir, name)
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode fixture: %v", err)
		}
	}
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func writeRawTraceFixture(t *testing.T, dir string, fileTime time.Time, taskShortID string, lines []string) string {
	t.Helper()
	name := fmt.Sprintf("%s_%s.jsonl", fileTime.Format("2006-01-02T15-04-05"), taskShortID)
	path := filepath.Join(dir, name)
	data := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write raw fixture: %v", err)
	}
	return path
}

func mustTaskGroups(t *testing.T, dir string) []*taskTrace {
	t.Helper()
	files, err := listTaskFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	return groupTraceFiles(loadTraceFiles(files))
}

type errorAfterDataReader struct {
	data []byte
	err  error
}

var _ io.Reader = (*errorAfterDataReader)(nil)

func (r *errorAfterDataReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func eventKindValuesFromSource(t *testing.T) map[string]struct{} {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(thisFile), "event.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse event.go: %v", err)
	}
	values := make(map[string]struct{})
	declaredBy := make(map[string]string)
	ast.Inspect(parsed, func(node ast.Node) bool {
		decl, ok := node.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if !strings.HasPrefix(name.Name, "Kind") || len(valueSpec.Values) == 0 {
					continue
				}
				valueIndex := i
				if len(valueSpec.Values) == 1 {
					valueIndex = 0
				}
				if valueIndex >= len(valueSpec.Values) {
					continue
				}
				literal, ok := valueSpec.Values[valueIndex].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				if previous, duplicate := declaredBy[value]; duplicate {
					t.Fatalf("EventKind value %q is duplicated by %s and %s", value, previous, name.Name)
				}
				declaredBy[value] = name.Name
				values[value] = struct{}{}
			}
		}
		return false
	})
	return values
}

// TestCmdStatsAggregatesTokens 验证 stats 子命令：token 取自 llm_call_end
// 逐次调用事件（唯一的 token 账本），并支持 task / agent 两种分组维度。
func TestCmdStatsAggregatesTokens(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)

	taskA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	writeTraceFixture(t, dir, base, taskA, []Event{
		{Timestamp: base, Kind: KindTaskPublished, TaskID: taskA, Description: "任务A"},
		{Timestamp: base.Add(time.Second), Kind: KindTaskClaimed, TaskID: taskA, AgentID: "worker-1"},
		{Timestamp: base.Add(2 * time.Second), Kind: KindLLMCallEnd, TaskID: taskA, AgentID: "worker-1", Loop: 0, PromptTokens: 1000, CompletionTokens: 150},
		{Timestamp: base.Add(3 * time.Second), Kind: KindTaskRetry, TaskID: taskA, AgentID: "worker-1", AttemptNo: 1},
		{Timestamp: base.Add(4 * time.Second), Kind: KindLLMCallEnd, TaskID: taskA, AgentID: "worker-1", Loop: 0, PromptTokens: 2000, CompletionTokens: 200},
		{Timestamp: base.Add(6 * time.Second), Kind: KindTaskCompleted, TaskID: taskA, AgentID: "worker-1"},
	})
	taskB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	writeTraceFixture(t, dir, base.Add(10*time.Second), taskB, []Event{
		{Timestamp: base.Add(10 * time.Second), Kind: KindTaskPublished, TaskID: taskB, Description: "任务B"},
		{Timestamp: base.Add(11 * time.Second), Kind: KindTaskClaimed, TaskID: taskB, AgentID: "worker-2"},
		{Timestamp: base.Add(12 * time.Second), Kind: KindLLMCallEnd, TaskID: taskB, AgentID: "worker-2", Loop: 0, PromptTokens: 500, CompletionTokens: 50},
		{Timestamp: base.Add(13 * time.Second), Kind: KindTaskCompleted, TaskID: taskB, AgentID: "worker-2"},
	})
	// 任务C 被级联取消：其全部 token 应计入浪费口径。
	taskC := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	writeTraceFixture(t, dir, base.Add(20*time.Second), taskC, []Event{
		{Timestamp: base.Add(20 * time.Second), Kind: KindTaskPublished, TaskID: taskC, Description: "任务C"},
		{Timestamp: base.Add(21 * time.Second), Kind: KindTaskClaimed, TaskID: taskC, AgentID: "worker-3"},
		{Timestamp: base.Add(22 * time.Second), Kind: KindLLMCallEnd, TaskID: taskC, AgentID: "worker-3", Loop: 0, PromptTokens: 300, CompletionTokens: 30},
		{Timestamp: base.Add(23 * time.Second), Kind: KindTaskCancelled, TaskID: taskC, AgentID: "worker-3",
			Transition: &Transition{PrevStatus: "processing", NewStatus: "cancelled", CancelSource: "dependency_failure"}},
	})

	// 默认 task 视图：session 总计 + 每任务一行。
	var taskOut bytes.Buffer
	if err := CLI([]string{"stats"}, dir, "", &taskOut); err != nil {
		t.Fatalf("CLI stats: %v", err)
	}
	got := taskOut.String()
	for _, want := range []string{
		"session 总计: 3 个任务, 4 次 LLM 调用, prompt=3.8k, completion=430, 合计=4.2k tokens, 重试=1 次, 浪费=330 tokens (8%)",
		"aaaaaaaa", "worker-1", "completed",
		"bbbbbbbb", "worker-2",
		"cccccccc", "worker-3", "cancelled", "330",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stats task output missing %q:\n%s", want, got)
		}
	}

	// agent 视图：worker-1 聚合 2 次调用 / prompt 3.0k。
	var agentOut bytes.Buffer
	if err := CLI([]string{"stats", "agent"}, dir, "", &agentOut); err != nil {
		t.Fatalf("CLI stats agent: %v", err)
	}
	got = agentOut.String()
	if !strings.Contains(got, "worker-1") || !strings.Contains(got, "worker-2") {
		t.Errorf("stats agent output missing agents:\n%s", got)
	}
	if !strings.Contains(got, "3.0k") {
		t.Errorf("stats agent output missing worker-1 prompt 3.0k:\n%s", got)
	}

	// 非法分组维度必须报错。
	if err := CLI([]string{"stats", "bogus"}, dir, "", &bytes.Buffer{}); err == nil {
		t.Fatal("stats bogus groupBy should fail")
	}
}

func TestCmdStatsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := CLI([]string{"stats"}, dir, "", &out); err != nil {
		t.Fatalf("CLI stats on empty dir: %v", err)
	}
	if !strings.Contains(out.String(), "没有 LLM 调用记录") {
		t.Errorf("empty dir should note no LLM calls:\n%s", out.String())
	}
}

// TestCmdStatsAnomalies 验证 stats 末尾的 task 粒度异常提示：浪费占比超阈、
// 同任务多次重试、单任务消耗异常集中三类信号。
func TestCmdStatsAnomalies(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC)

	// A：大头任务（占 session 总量 > 40%）。
	taskA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	writeTraceFixture(t, dir, base, taskA, []Event{
		{Timestamp: base, Kind: KindTaskClaimed, TaskID: taskA, AgentID: "worker-1"},
		{Timestamp: base.Add(time.Second), Kind: KindLLMCallEnd, TaskID: taskA, AgentID: "worker-1", PromptTokens: 1000, CompletionTokens: 100},
		{Timestamp: base.Add(2 * time.Second), Kind: KindTaskCompleted, TaskID: taskA, AgentID: "worker-1"},
	})
	// B：被取消（贡献浪费占比 > 20%）。
	taskB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	writeTraceFixture(t, dir, base.Add(10*time.Second), taskB, []Event{
		{Timestamp: base.Add(10 * time.Second), Kind: KindTaskClaimed, TaskID: taskB, AgentID: "worker-2"},
		{Timestamp: base.Add(11 * time.Second), Kind: KindLLMCallEnd, TaskID: taskB, AgentID: "worker-2", PromptTokens: 500, CompletionTokens: 50},
		{Timestamp: base.Add(12 * time.Second), Kind: KindTaskCancelled, TaskID: taskB, AgentID: "worker-2"},
	})
	// C：同任务重试 3 次。
	taskC := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	writeTraceFixture(t, dir, base.Add(20*time.Second), taskC, []Event{
		{Timestamp: base.Add(20 * time.Second), Kind: KindTaskClaimed, TaskID: taskC, AgentID: "worker-3"},
		{Timestamp: base.Add(21 * time.Second), Kind: KindTaskRetry, TaskID: taskC, AgentID: "worker-3", AttemptNo: 1},
		{Timestamp: base.Add(22 * time.Second), Kind: KindTaskRetry, TaskID: taskC, AgentID: "worker-3", AttemptNo: 2},
		{Timestamp: base.Add(23 * time.Second), Kind: KindTaskRetry, TaskID: taskC, AgentID: "worker-3", AttemptNo: 3},
		{Timestamp: base.Add(24 * time.Second), Kind: KindLLMCallEnd, TaskID: taskC, AgentID: "worker-3", PromptTokens: 100, CompletionTokens: 10},
		{Timestamp: base.Add(25 * time.Second), Kind: KindTaskCompleted, TaskID: taskC, AgentID: "worker-3"},
	})

	var out bytes.Buffer
	if err := CLI([]string{"stats"}, dir, "", &out); err != nil {
		t.Fatalf("CLI stats: %v", err)
	}
	got := out.String()
	for _, want := range []string{"异常提示:", "浪费占比", "重试 3 次", "异常集中"} {
		if !strings.Contains(got, want) {
			t.Errorf("stats anomalies output missing %q:\n%s", want, got)
		}
	}
}

// TestCmdStatsNoAnomaliesWhenHealthy 验证健康 session 不输出异常提示区块。
func TestCmdStatsNoAnomaliesWhenHealthy(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 22, 3, 0, 0, 0, time.UTC)
	for i, id := range []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"cccccccc-cccc-cccc-cccc-cccccccccccc",
	} {
		ts := base.Add(time.Duration(i*10) * time.Second)
		writeTraceFixture(t, dir, ts, id, []Event{
			{Timestamp: ts, Kind: KindTaskClaimed, TaskID: id, AgentID: "worker-1"},
			{Timestamp: ts.Add(time.Second), Kind: KindLLMCallEnd, TaskID: id, AgentID: "worker-1", PromptTokens: 100, CompletionTokens: 10},
			{Timestamp: ts.Add(2 * time.Second), Kind: KindTaskCompleted, TaskID: id, AgentID: "worker-1"},
		})
	}
	var out bytes.Buffer
	if err := CLI([]string{"stats"}, dir, "", &out); err != nil {
		t.Fatalf("CLI stats: %v", err)
	}
	if strings.Contains(out.String(), "异常提示:") {
		t.Errorf("healthy session should not print anomalies:\n%s", out.String())
	}
}

// TestCmdStatsReReadMetric 验证修正后的重读判定（2026-07-22）：
// 只有"重复读同一内容"算浪费——重复全文读、相同 offset 重复分页；
// 大文件不同 offset 的顺序分页是合法阅读，不计入。
func TestCmdStatsReReadMetric(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	read := func(ts time.Time, taskID, path string, args map[string]any) Event {
		if args == nil {
			args = map[string]any{}
		}
		args["path"] = path
		return Event{Timestamp: ts, Kind: KindToolCall, TaskID: taskID, Tool: "read_file", Args: args}
	}

	// 任务A：a.go 全文读 2 次（1 次浪费）+ big.go 分页 1/300/600（合法）
	// + big.go offset=300 重复（1 次浪费）→ 5 次读取，2 次重读 = 40%。
	taskA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	writeTraceFixture(t, dir, base, taskA, []Event{
		{Timestamp: base, Kind: KindTaskClaimed, TaskID: taskA, AgentID: "explorer-1"},
		read(base.Add(time.Second), taskA, "a.go", nil),
		read(base.Add(2*time.Second), taskA, "a.go", nil),
		read(base.Add(3*time.Second), taskA, "big.go", map[string]any{"offset": float64(1), "limit": float64(300)}),
		read(base.Add(4*time.Second), taskA, "big.go", map[string]any{"offset": float64(300), "limit": float64(300)}),
		read(base.Add(5*time.Second), taskA, "big.go", map[string]any{"offset": float64(300), "limit": float64(400)}),
		read(base.Add(6*time.Second), taskA, "big.go", map[string]any{"offset": float64(600), "limit": float64(300)}),
		{Timestamp: base.Add(6500 * time.Millisecond), Kind: KindLLMCallEnd, TaskID: taskA, AgentID: "explorer-1", PromptTokens: 100, CompletionTokens: 10},
		{Timestamp: base.Add(7 * time.Second), Kind: KindTaskCompleted, TaskID: taskA, AgentID: "explorer-1"},
	})
	// 任务B：纯顺序分页 + 一次全文读 → 0 重读，不应出现 WARNING。
	taskB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	writeTraceFixture(t, dir, base.Add(10*time.Second), taskB, []Event{
		{Timestamp: base.Add(10 * time.Second), Kind: KindTaskClaimed, TaskID: taskB, AgentID: "explorer-1"},
		read(base.Add(11*time.Second), taskB, "x.go", nil),
		read(base.Add(12*time.Second), taskB, "big.go", map[string]any{"offset": float64(1), "limit": float64(200)}),
		read(base.Add(13*time.Second), taskB, "big.go", map[string]any{"offset": float64(200), "limit": float64(200)}),
		read(base.Add(14*time.Second), taskB, "big.go", map[string]any{"offset": float64(400), "limit": float64(200)}),
		read(base.Add(15*time.Second), taskB, "big.go", map[string]any{"offset": float64(600), "limit": float64(200)}),
		{Timestamp: base.Add(16 * time.Second), Kind: KindTaskCompleted, TaskID: taskB, AgentID: "explorer-1"},
	})

	var out bytes.Buffer
	if err := CLI([]string{"stats"}, dir, "", &out); err != nil {
		t.Fatalf("CLI stats: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "任务 aaaaaaaa read_file 重读率 33%（2/6 次为重复读取") {
		t.Errorf("任务A 重读判定不符（应为 2/6=33%%）:\n%s", got)
	}
	// 任务B 纯顺序分页 + 一次全文读 → 0 重读，不应出现在异常提示区块。
	anomalySection := ""
	if idx := strings.Index(got, "异常提示:"); idx >= 0 {
		anomalySection = got[idx:]
	}
	if strings.Contains(anomalySection, "bbbbbbbb") {
		t.Errorf("任务B 纯顺序分页不应被判为重读:\n%s", anomalySection)
	}
}
