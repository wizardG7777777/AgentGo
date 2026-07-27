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
	planCtx := &PlanTraceContext{
		PlanID: "plan-1", PlanRevision: 3, ExecutionStateVersion: 5,
		AcceptanceSpecRevision: 2, GraphDigest: "digest-3",
	}
	transition := &Transition{
		PrevStatus: "processing", NewStatus: "failed", Cause: "test-cause",
		CancelSource: "scheduler", RetryCount: 2,
	}
	planEvent := func(kind EventKind, reason string) Event {
		return Event{Kind: kind, Reason: reason, Plan: planCtx}
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
		{"history_truncated", Event{Kind: KindHistoryTruncated, PromptTokensBefore: 200, PromptTokensAfter: 80, Strategy: "drop_middle", KeptEntries: 5}, []string{"tokens_before=200", "tokens_after=80", "strategy=drop_middle", "kept_entries=5"}},
		{"token_stats", Event{Kind: KindTokenStats, CallCount: 3, PromptTokens: 10, CompletionTokens: 5, TotalPromptTokens: 30, TotalCompletionTokens: 15}, []string{"call=3", "prompt_tokens=10", "completion_tokens=5", "total_prompt_tokens=30", "total_completion_tokens=15"}},
		{"file_written", Event{Kind: KindFileWritten, Tool: "write_file", Path: "a.go", Bytes: 42, Hash: "full-hash"}, []string{"path=a.go", "bytes=42", "hash=full-hash", "tool=write_file"}},
		{"file_write_queued", Event{Kind: KindFileWriteQueued, Path: "a.go", QueueLen: 2, WaitMS: 15, Description: "acquired"}, []string{"path=a.go", "queue_len=2", "wait_ms=15", `desc="acquired"`}},
		{"progress_notify", Event{Kind: KindProgressNotify, NotifyType: "halfway"}, []string{"notify_type=halfway"}},
		{"memory_context_inject", Event{Kind: KindMemoryContextInject, NotifyType: "team_snapshot", Path: "team_snapshot:worker-1", OutputLen: 128}, []string{"source=team_snapshot", "key=team_snapshot:worker-1", "runes=128"}},
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
		{"replan_requested", planEvent(KindReplanRequested, "terminal fact"), []string{`reason="terminal fact"`, "plan=plan-1"}},
		{"replan_coalesced", planEvent(KindReplanCoalesced, "two requests"), []string{`reason="two requests"`, "plan_revision=3"}},
		{"replan_decided", planEvent(KindReplanDecided, "apply_plan_patch"), []string{`reason="apply_plan_patch"`, "state_version=5"}},
		{"acceptance_completed", Event{Kind: KindAcceptanceCompleted, Plan: planCtx, Acceptance: &AcceptanceTraceContext{AcceptanceRunID: "run-1", ResultID: "result-1", SpecID: "spec-1", SpecRevision: 2, TargetRevision: 3, TargetGraphDigest: "digest-3", RunnerTaskID: "runner-task", RunnerKind: "verifier", Verdict: "pass", Status: "valid", Reason: "all green"}}, []string{"plan=plan-1", "acceptance_run=run-1", "result=result-1", "spec=spec-1", "spec_revision=2", "target_revision=3", "target_digest=digest-3", "runner_task=runner-task", "runner_kind=verifier", "verdict=pass", "status=valid", `acceptance_reason="all green"`}},
		{"plan_revision_changed", planEvent(KindPlanRevisionChanged, "replacement"), []string{`reason="replacement"`, "graph_digest=digest-3"}},
		{"plan_paused", planEvent(KindPlanPaused, "budget"), []string{`reason="budget"`, "acceptance_revision=2"}},
		{"plan_terminal", planEvent(KindPlanTerminal, "pass"), []string{`reason="pass"`, "plan=plan-1"}},
	}
	if len(cases) != 37 {
		t.Fatalf("test inventory has %d built-in EventKinds, want 37", len(cases))
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
	if got := formatEventDetails(Event{Kind: KindPlanPaused}); got != "" {
		t.Fatalf("nil optional Plan payload should stay empty, got %q", got)
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

func TestTaskAggregationAcrossRetryFilesAndPlan(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 18, 2, 0, 0, 0, time.UTC)
	planID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	taskID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	plan := &PlanTraceContext{PlanID: planID, PlanRevision: 1, ExecutionStateVersion: 2}

	writeTraceFixture(t, dir, base, taskID, []Event{
		{Timestamp: base, Kind: KindTaskPublished, TaskID: taskID, Plan: plan},
		{Timestamp: base, Kind: KindLLMCallStart, TaskID: taskID, Loop: 0},
		{Timestamp: base, Kind: KindToolCall, TaskID: taskID, Tool: "read_file", CallID: "first-fragment"},
		{Timestamp: base, Kind: KindTaskRetry, TaskID: taskID, AttemptNo: 1},
	})
	// Retry 分片没有 Plan payload；Plan 聚合仍须因完整 TaskID 成员关系纳入。
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
	if err := CLI([]string{"list"}, dir, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list.String(), "共 1 个任务") || strings.Count(list.String(), shortIdentifier(taskID)) != 1 {
		t.Fatalf("retry shards were listed as multiple tasks:\n%s", list.String())
	}

	var show bytes.Buffer
	if err := CLI([]string{"show", taskID}, dir, &show); err != nil {
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

	var planOut bytes.Buffer
	if err := CLI([]string{"plan", planID}, dir, &planOut); err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, want := range []string{"Tasks: 1", "Trace Files: 2", "Events: 9", "first-fragment", "second-fragment"} {
		if !strings.Contains(planOut.String(), want) {
			t.Errorf("plan missing retry fragment evidence %q:\n%s", want, planOut.String())
		}
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
		if err := CLI([]string{"show", tc.id}, dir, &out); err != nil {
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
		planID := "dddddddd-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		writeRawTraceFixture(t, dir, base, "dddddddd", []string{
			marshalLine(Event{Timestamp: base, Kind: KindToolCall, TaskID: taskID, CallID: "VALID", Plan: &PlanTraceContext{PlanID: planID}}),
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
		if err := CLI([]string{"show", taskID}, dir, &out); err != nil {
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
		var planOut bytes.Buffer
		if err := CLI([]string{"plan", planID}, dir, &planOut); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(planOut.String(), "WARNING: timeline incomplete") || !strings.Contains(planOut.String(), "<parse_error>") {
			t.Fatalf("relevant Plan issue was not reported:\n%s", planOut.String())
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
			if err := CLI([]string{"show", group.taskID}, dir, &out); err != nil {
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

func TestLatestPlanContextMergesMonotonicVersionsWithoutStaleDigest(t *testing.T) {
	planID := "plan-monotonic"
	events := []Event{
		{Plan: &PlanTraceContext{PlanID: planID, PlanRevision: 3, ExecutionStateVersion: 8, AcceptanceSpecRevision: 2, GraphDigest: "digest-3"}},
		// 后发的 partial context 只推进 execution state，不能清空图信息。
		{Plan: &PlanTraceContext{PlanID: planID, ExecutionStateVersion: 9}},
		// 更晚到达的旧 snapshot 不能让 header 版本回退。
		{Plan: &PlanTraceContext{PlanID: planID, PlanRevision: 2, ExecutionStateVersion: 7, AcceptanceSpecRevision: 1, GraphDigest: "digest-2"}},
	}
	got := latestPlanContext(events, planID)
	if got == nil || got.PlanRevision != 3 || got.ExecutionStateVersion != 9 ||
		got.AcceptanceSpecRevision != 2 || got.GraphDigest != "digest-3" {
		t.Fatalf("merged Plan context=%+v", got)
	}

	// 若确实观察到更高图 revision、但该 revision 尚无 digest，就不能把旧图
	// digest 错配到新 revision。
	events = append(events, Event{Plan: &PlanTraceContext{PlanID: planID, PlanRevision: 4, ExecutionStateVersion: 9}})
	got = latestPlanContext(events, planID)
	if got == nil || got.PlanRevision != 4 || got.GraphDigest != "" {
		t.Fatalf("higher partial revision reused stale digest: %+v", got)
	}
}

func TestCmdPlanAggregatesAcrossTaskFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 18, 3, 0, 0, 0, time.UTC)
	planID := "11111111-1111-1111-1111-111111111111"
	planCtx := func(rev, state int64, digest string) *PlanTraceContext {
		return &PlanTraceContext{PlanID: planID, PlanRevision: rev, ExecutionStateVersion: state, AcceptanceSpecRevision: 1, GraphDigest: digest}
	}
	writeTraceFixture(t, dir, base, planID, []Event{
		{Timestamp: base, Kind: KindToolCall, TaskID: planID, Tool: "provision_agent_team", CallID: "controller-first"},
		// 根 Task 本身没有 Plan payload，仍须因 TaskID==PlanID 纳入。
		{Timestamp: base.Add(3 * time.Second), Kind: KindReplanDecided, TaskID: planID, Reason: "start_acceptance"},
	})
	workerID := "22222222-2222-2222-2222-222222222222"
	writeTraceFixture(t, dir, base.Add(time.Second), workerID, []Event{
		{Timestamp: base.Add(time.Second), Kind: KindPlanRevisionChanged, TaskID: workerID, Reason: "publish worker", Plan: planCtx(1, 1, "digest-1")},
		{Timestamp: base.Add(2 * time.Second), Kind: KindToolCall, TaskID: workerID, Tool: "read_file", CallID: "worker-middle"},
	})
	acceptanceID := "33333333-3333-3333-3333-333333333333"
	writeTraceFixture(t, dir, base.Add(4*time.Second), acceptanceID, []Event{
		{Timestamp: base.Add(4 * time.Second), Kind: KindAcceptanceCompleted, TaskID: acceptanceID, Plan: planCtx(2, 5, "digest-2"), Acceptance: &AcceptanceTraceContext{AcceptanceRunID: "run-1", ResultID: "result-1", Verdict: "pass", Status: "valid"}},
	})
	otherID := "44444444-4444-4444-4444-444444444444"
	writeTraceFixture(t, dir, base.Add(5*time.Second), otherID, []Event{
		{Timestamp: base.Add(5 * time.Second), Kind: KindPlanPaused, TaskID: otherID, Reason: "SHOULD_NOT_APPEAR", Plan: &PlanTraceContext{PlanID: "99999999-9999-9999-9999-999999999999"}},
	})
	// 完全无关的坏文件不能阻断或污染目标 Plan。
	writeRawTraceFixture(t, dir, base.Add(6*time.Second), "deadbeef", []string{`{"broken":`})

	var out bytes.Buffer
	if err := CLI([]string{"plan", "11111111"}, dir, &out); err != nil {
		t.Fatalf("CLI plan: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Plan: " + planID, "Tasks: 3", "Events: 5", "Revision: 2", "State Version: 5",
		"Latest Acceptance: status=valid verdict=pass run=run-1 result=result-1",
		"task=11111111", "task=22222222", "task=33333333",
		"controller-first", "worker-middle", "acceptance_run=run-1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SHOULD_NOT_APPEAR") || strings.Contains(got, "task=44444444") {
		t.Fatalf("other Plan leaked into output:\n%s", got)
	}
	if strings.Contains(got, "timeline incomplete") || strings.Contains(got, "deadbeef") {
		t.Fatalf("unrelated malformed file polluted target Plan:\n%s", got)
	}
	controllerPos := strings.Index(got, "controller-first")
	workerPos := strings.Index(got, "worker-middle")
	acceptancePos := strings.Index(got, "acceptance_run=run-1")
	if controllerPos < 0 || workerPos <= controllerPos || acceptancePos <= workerPos {
		t.Fatalf("events not globally time-sorted:\n%s", got)
	}
}

func TestCmdPlanIDResolutionAmbiguousExactAndMissing(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 18, 3, 30, 0, 0, time.UTC)
	firstPlanID := "aaaaaaaa-1111-1111-1111-111111111111"
	secondPlanID := "aaaaaaaa-2222-2222-2222-222222222222"
	writeTraceFixture(t, dir, base, "bbbbbbbb-1111-1111-1111-111111111111", []Event{{
		Timestamp: base, Kind: KindPlanPaused, TaskID: "bbbbbbbb-1111-1111-1111-111111111111",
		Reason: "first-plan", Plan: &PlanTraceContext{PlanID: firstPlanID},
	}})
	writeTraceFixture(t, dir, base.Add(time.Second), "cccccccc-2222-2222-2222-222222222222", []Event{{
		Timestamp: base.Add(time.Second), Kind: KindPlanPaused, TaskID: "cccccccc-2222-2222-2222-222222222222",
		Reason: "second-plan", Plan: &PlanTraceContext{PlanID: secondPlanID},
	}})

	var ambiguous bytes.Buffer
	if err := CLI([]string{"plan", "aaaaaaaa"}, dir, &ambiguous); err != nil {
		t.Fatalf("ambiguous plan prefix: %v", err)
	}
	for _, want := range []string{"找到 2 个匹配的 Plan", firstPlanID, secondPlanID} {
		if !strings.Contains(ambiguous.String(), want) {
			t.Errorf("ambiguous output missing %q:\n%s", want, ambiguous.String())
		}
	}

	var exact bytes.Buffer
	if err := CLI([]string{"plan", firstPlanID}, dir, &exact); err != nil {
		t.Fatalf("exact plan ID: %v", err)
	}
	if !strings.Contains(exact.String(), "Plan: "+firstPlanID) || strings.Contains(exact.String(), "second-plan") {
		t.Fatalf("exact Plan selection leaked another Plan:\n%s", exact.String())
	}

	var missing bytes.Buffer
	err := CLI([]string{"plan", "deadbeef"}, dir, &missing)
	if err == nil || !strings.Contains(err.Error(), "未找到匹配 plan_id=deadbeef") {
		t.Fatalf("missing Plan error=%v", err)
	}
}

func TestCmdShowUsesFullTaskIDAndShowsPlanHeader(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 18, 4, 0, 0, 0, time.UTC)
	firstID := "abcdef12-1111-1111-1111-111111111111"
	secondID := "abcdef12-2222-2222-2222-222222222222"
	writeTraceFixture(t, dir, base, firstID, []Event{{Timestamp: base, Kind: KindError, TaskID: firstID, Error: "first-only"}})
	writeTraceFixture(t, dir, base.Add(time.Second), secondID, []Event{{
		Timestamp: base.Add(time.Second), Kind: KindHistoryTruncated, TaskID: secondID, AgentID: "worker", Loop: 0,
		PromptTokensBefore: 100, PromptTokensAfter: 50,
		Plan: &PlanTraceContext{PlanID: "plan-full", PlanRevision: 2, ExecutionStateVersion: 3, AcceptanceSpecRevision: 1, GraphDigest: "digest-full"},
	}})
	var ambiguous bytes.Buffer
	if err := CLI([]string{"show", "abcdef12"}, dir, &ambiguous); err != nil {
		t.Fatalf("CLI show ambiguous prefix: %v", err)
	}
	for _, want := range []string{"找到 2 个匹配的任务", firstID, secondID} {
		if !strings.Contains(ambiguous.String(), want) {
			t.Errorf("ambiguous task output missing %q:\n%s", want, ambiguous.String())
		}
	}
	var out bytes.Buffer
	if err := CLI([]string{"show", secondID}, dir, &out); err != nil {
		t.Fatalf("CLI show: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Plan: plan-full", "Graph Digest: digest-full", "history_truncated", "loop=0", "tokens_after=50"} {
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

func TestTraceCLIUsageIncludesPlan(t *testing.T) {
	var out bytes.Buffer
	if err := CLI([]string{"help"}, t.TempDir(), &out); err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out.String(), "plan <plan_id>") {
		t.Fatalf("help missing plan command:\n%s", out.String())
	}
	if err := CLI([]string{"plan"}, t.TempDir(), &out); err == nil || !strings.Contains(err.Error(), "plan <plan_id>") {
		t.Fatalf("missing plan ID error=%v", err)
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
// 逐次调用事件（不读累计型 token_stats，避免重复计数），并支持
// task / agent / plan 三种分组维度。
func TestCmdStatsAggregatesTokens(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	planID := "11111111-1111-1111-1111-111111111111"
	planCtx := &PlanTraceContext{PlanID: planID, PlanRevision: 1, ExecutionStateVersion: 1}

	taskA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	writeTraceFixture(t, dir, base, taskA, []Event{
		{Timestamp: base, Kind: KindTaskPublished, TaskID: taskA, Description: "任务A", Plan: planCtx},
		{Timestamp: base.Add(time.Second), Kind: KindTaskClaimed, TaskID: taskA, AgentID: "worker-1"},
		{Timestamp: base.Add(2 * time.Second), Kind: KindLLMCallEnd, TaskID: taskA, AgentID: "worker-1", Loop: 0, PromptTokens: 1000, CompletionTokens: 150},
		{Timestamp: base.Add(3 * time.Second), Kind: KindTaskRetry, TaskID: taskA, AgentID: "worker-1", AttemptNo: 1},
		{Timestamp: base.Add(4 * time.Second), Kind: KindLLMCallEnd, TaskID: taskA, AgentID: "worker-1", Loop: 0, PromptTokens: 2000, CompletionTokens: 200},
		// token_stats 是累计值，stats 不得纳入（否则 prompt 会虚高 6000）。
		{Timestamp: base.Add(5 * time.Second), Kind: KindTokenStats, TaskID: taskA, AgentID: "worker-1", PromptTokens: 2000, CompletionTokens: 200, TotalPromptTokens: 6000, TotalCompletionTokens: 700, CallCount: 2},
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
	if err := CLI([]string{"stats"}, dir, &taskOut); err != nil {
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
	if strings.Contains(got, "6.0k") || strings.Contains(got, "9.5k") {
		t.Errorf("token_stats 累计值被误计入 stats:\n%s", got)
	}

	// agent 视图：worker-1 聚合 2 次调用 / prompt 3.0k。
	var agentOut bytes.Buffer
	if err := CLI([]string{"stats", "agent"}, dir, &agentOut); err != nil {
		t.Fatalf("CLI stats agent: %v", err)
	}
	got = agentOut.String()
	if !strings.Contains(got, "worker-1") || !strings.Contains(got, "worker-2") {
		t.Errorf("stats agent output missing agents:\n%s", got)
	}
	if !strings.Contains(got, "3.0k") {
		t.Errorf("stats agent output missing worker-1 prompt 3.0k:\n%s", got)
	}

	// plan 视图：任务A 归入 plan 短 ID，任务B 归入 (no-plan)。
	var planOut bytes.Buffer
	if err := CLI([]string{"stats", "plan"}, dir, &planOut); err != nil {
		t.Fatalf("CLI stats plan: %v", err)
	}
	got = planOut.String()
	if !strings.Contains(got, "11111111") || !strings.Contains(got, "(no-plan)") {
		t.Errorf("stats plan output missing plan buckets:\n%s", got)
	}

	// 非法分组维度必须报错。
	if err := CLI([]string{"stats", "bogus"}, dir, &bytes.Buffer{}); err == nil {
		t.Fatal("stats bogus groupBy should fail")
	}
}

func TestCmdStatsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := CLI([]string{"stats"}, dir, &out); err != nil {
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
	if err := CLI([]string{"stats"}, dir, &out); err != nil {
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
	if err := CLI([]string{"stats"}, dir, &out); err != nil {
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
	if err := CLI([]string{"stats"}, dir, &out); err != nil {
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
