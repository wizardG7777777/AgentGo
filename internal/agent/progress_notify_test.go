package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/trace"
)

// toolNames is the set of tool names used for random generation.
var toolNames = []string{"write_file", "edit_file", "read_file", "publish_subtask", "run_shell"}

// randomExecuteResult generates a random ExecuteResult with 0-5 ToolCalls.
// Each ToolCall has a random Name from toolNames, and the corresponding
// ToolResult.Content is either normal content or prefixed with "错误:".
func randomExecuteResult(rng *rand.Rand) ExecuteResult {
	n := rng.Intn(6) // 0-5 tool calls
	calls := make([]llm.ToolCall, n)
	results := make([]ToolResult, n)
	for i := 0; i < n; i++ {
		name := toolNames[rng.Intn(len(toolNames))]
		args := map[string]any{}
		// Give write/edit tools a random path argument
		if name == "write_file" || name == "edit_file" {
			args["path"] = randomPath(rng)
		}
		calls[i] = llm.ToolCall{
			ID:        randomString(rng, 8),
			Name:      name,
			Arguments: args,
		}
		// Randomly choose normal content or error prefix
		if rng.Intn(3) == 0 {
			results[i] = ToolResult{
				ToolCallID: calls[i].ID,
				Content:    "错误:" + randomString(rng, 10),
			}
		} else {
			results[i] = ToolResult{
				ToolCallID: calls[i].ID,
				Content:    "success: " + randomString(rng, 10),
			}
		}
	}
	return ExecuteResult{
		ToolCalled:  n > 0,
		ToolCalls:   calls,
		ToolResults: results,
	}
}

func randomPath(rng *rand.Rand) string {
	segments := []string{"internal", "src", "pkg", "cmd", "agent", "config"}
	depth := rng.Intn(3) + 1
	parts := make([]string, depth)
	for i := 0; i < depth; i++ {
		parts[i] = segments[rng.Intn(len(segments))]
	}
	return strings.Join(parts, "/") + "/" + randomString(rng, 5) + ".go"
}

func randomString(rng *rand.Rand, length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rng.Intn(len(charset))]
	}
	return string(b)
}

// expectedFileWriteDetected is the oracle: returns true iff there exists at least
// one ToolCall with Name "write_file" or "edit_file" whose corresponding
// ToolResult.Content does NOT start with "错误:".
func expectedFileWriteDetected(result ExecuteResult) bool {
	for i, tc := range result.ToolCalls {
		if tc.Name != "write_file" && tc.Name != "edit_file" {
			continue
		}
		if i >= len(result.ToolResults) {
			continue
		}
		if strings.HasPrefix(result.ToolResults[i].Content, "错误:") {
			continue
		}
		return true
	}
	return false
}

// TestProperty1_FileWriteDetection verifies that detectFileWrite returns a
// non-empty list if and only if there exists a successful write_file/edit_file
// call in the ExecuteResult.
//
// **Validates: Requirements 1.1**
func TestProperty1_FileWriteDetection(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 200,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			values[0] = reflect.ValueOf(randomExecuteResult(rng))
		},
	}

	prop := func(result ExecuteResult) bool {
		paths := detectFileWrite(result)
		gotNonEmpty := len(paths) > 0
		wantNonEmpty := expectedFileWriteDetected(result)
		return gotNonEmpty == wantNonEmpty
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 1 failed: %v", err)
	}
}

// expectedSubtaskPublishDetected is the oracle: returns true iff there exists
// at least one ToolCall with Name "publish_subtask".
func expectedSubtaskPublishDetected(result ExecuteResult) bool {
	for _, tc := range result.ToolCalls {
		if tc.Name == "publish_subtask" {
			return true
		}
	}
	return false
}

// TestProperty2_SubtaskPublishDetection verifies that detectSubtaskPublish
// returns true if and only if ToolCalls contains a "publish_subtask" entry.
//
// **Validates: Requirements 2.1**
func TestProperty2_SubtaskPublishDetection(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 200,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			values[0] = reflect.ValueOf(randomExecuteResult(rng))
		},
	}

	prop := func(result ExecuteResult) bool {
		got := detectSubtaskPublish(result)
		want := expectedSubtaskPublishDetected(result)
		return got == want
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 2 failed: %v", err)
	}
}

// TestProperty3_HalfwayDetection verifies that detectHalfway returns true
// if and only if loopIndex > maxLoops/2, for random pairs of (loopIndex, maxLoops)
// where maxLoops in [1, 200] and loopIndex in [0, maxLoops-1].
//
// **Validates: Requirements 3.1**
func TestProperty3_HalfwayDetection(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 200,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			maxLoops := rng.Intn(200) + 1   // [1, 200]
			loopIndex := rng.Intn(maxLoops) // [0, maxLoops-1]
			values[0] = reflect.ValueOf(loopIndex)
			values[1] = reflect.ValueOf(maxLoops)
		},
	}

	prop := func(loopIndex, maxLoops int) bool {
		got := detectHalfway(loopIndex, maxLoops)
		want := loopIndex > maxLoops/2
		return got == want
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 3 failed: %v", err)
	}
}

// TestProperty5_MessageFixedFieldInvariants verifies that all messages built by
// buildFileWriteMsg, buildSubtaskMsg, and buildHalfwayMsg have fixed fields:
// Type=="info", Priority=="low", ChainDepth==0.
// Additionally: file-write and halfway messages have To=="*",
// subtask messages have To=="scheduler".
//
// **Validates: Requirements 4.1, 4.2, 5.1, 5.2**
func TestProperty5_MessageFixedFieldInvariants(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 200,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			agentID := "agent-" + randomString(rng, 6)
			// Generate 1-5 random file paths
			nFiles := rng.Intn(5) + 1
			files := make([]string, nFiles)
			for i := range files {
				files[i] = randomPath(rng)
			}
			maxLoops := rng.Intn(200) + 1   // [1, 200]
			loopIndex := rng.Intn(maxLoops) // [0, maxLoops-1]

			values[0] = reflect.ValueOf(agentID)
			values[1] = reflect.ValueOf(files)
			values[2] = reflect.ValueOf(loopIndex)
			values[3] = reflect.ValueOf(maxLoops)
		},
	}

	prop := func(agentID string, files []string, loopIndex, maxLoops int) bool {
		msgs := []struct {
			msg    mailbox.Message
			wantTo string
			label  string
		}{
			{buildFileWriteMsg(agentID, files, loopIndex, maxLoops), "*", "buildFileWriteMsg"},
			{buildSubtaskMsg(agentID, loopIndex, maxLoops), "scheduler", "buildSubtaskMsg"},
			{buildHalfwayMsg(agentID, loopIndex, maxLoops), "*", "buildHalfwayMsg"},
		}

		for _, tc := range msgs {
			if tc.msg.Type != mailbox.MsgTypeInfo {
				t.Logf("%s: Type=%q, want %q", tc.label, tc.msg.Type, mailbox.MsgTypeInfo)
				return false
			}
			if tc.msg.Priority != mailbox.PriorityLow {
				t.Logf("%s: Priority=%q, want %q", tc.label, tc.msg.Priority, mailbox.PriorityLow)
				return false
			}
			if tc.msg.ChainDepth != 0 {
				t.Logf("%s: ChainDepth=%d, want 0", tc.label, tc.msg.ChainDepth)
				return false
			}
			if tc.msg.To != tc.wantTo {
				t.Logf("%s: To=%q, want %q", tc.label, tc.msg.To, tc.wantTo)
				return false
			}
		}
		return true
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 5 failed: %v", err)
	}
}

// TestProperty6_FileWriteMsgContentCompleteness verifies that buildFileWriteMsg
// produces a Content string containing agentID, filepath.Base(path),
// the string representation of loopIndex+1, and the string representation of maxLoops.
//
// **Validates: Requirements 1.2, 4.4**
func TestProperty6_FileWriteMsgContentCompleteness(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 200,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			agentID := "agent-" + randomString(rng, 6)
			// Generate 1-3 random file paths
			nFiles := rng.Intn(3) + 1
			files := make([]string, nFiles)
			for i := range files {
				files[i] = randomPath(rng)
			}
			maxLoops := rng.Intn(200) + 1   // [1, 200]
			loopIndex := rng.Intn(maxLoops) // [0, maxLoops-1]

			values[0] = reflect.ValueOf(agentID)
			values[1] = reflect.ValueOf(files)
			values[2] = reflect.ValueOf(loopIndex)
			values[3] = reflect.ValueOf(maxLoops)
		},
	}

	prop := func(agentID string, files []string, loopIndex, maxLoops int) bool {
		msg := buildFileWriteMsg(agentID, files, loopIndex, maxLoops)

		// Content must contain agentID
		if !strings.Contains(msg.Content, agentID) {
			t.Logf("Content missing agentID %q: %q", agentID, msg.Content)
			return false
		}
		// Content must contain filepath.Base of the first file
		if len(files) > 0 {
			baseName := filepath.Base(files[0])
			if !strings.Contains(msg.Content, baseName) {
				t.Logf("Content missing filepath.Base %q: %q", baseName, msg.Content)
				return false
			}
		}
		// Content must contain loopIndex+1 as string
		loopStr := fmt.Sprintf("%d", loopIndex+1)
		if !strings.Contains(msg.Content, loopStr) {
			t.Logf("Content missing loopIndex+1 %q: %q", loopStr, msg.Content)
			return false
		}
		// Content must contain maxLoops as string
		maxStr := fmt.Sprintf("%d", maxLoops)
		if !strings.Contains(msg.Content, maxStr) {
			t.Logf("Content missing maxLoops %q: %q", maxStr, msg.Content)
			return false
		}
		return true
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 6 failed: %v", err)
	}
}

// TestProperty7_HalfwayMsgContentCompleteness verifies that buildHalfwayMsg
// produces a Content string containing agentID, the string representation of
// loopIndex+1, and the string representation of maxLoops.
//
// **Validates: Requirements 4.5**
func TestProperty7_HalfwayMsgContentCompleteness(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 200,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			agentID := "agent-" + randomString(rng, 6)
			maxLoops := rng.Intn(200) + 1   // [1, 200]
			loopIndex := rng.Intn(maxLoops) // [0, maxLoops-1]

			values[0] = reflect.ValueOf(agentID)
			values[1] = reflect.ValueOf(loopIndex)
			values[2] = reflect.ValueOf(maxLoops)
		},
	}

	prop := func(agentID string, loopIndex, maxLoops int) bool {
		msg := buildHalfwayMsg(agentID, loopIndex, maxLoops)

		// Content must contain agentID
		if !strings.Contains(msg.Content, agentID) {
			t.Logf("Content missing agentID %q: %q", agentID, msg.Content)
			return false
		}
		// Content must contain loopIndex+1 as string
		loopStr := fmt.Sprintf("%d", loopIndex+1)
		if !strings.Contains(msg.Content, loopStr) {
			t.Logf("Content missing loopIndex+1 %q: %q", loopStr, msg.Content)
			return false
		}
		// Content must contain maxLoops as string
		maxStr := fmt.Sprintf("%d", maxLoops)
		if !strings.Contains(msg.Content, maxStr) {
			t.Logf("Content missing maxLoops %q: %q", maxStr, msg.Content)
			return false
		}
		return true
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 7 failed: %v", err)
	}
}

// TestProperty8_ConfigDisabledSkipsAll verifies that when ProgressNotifyEnabled
// is false, progressNotify does not call MailRegistry.Send (no messages delivered)
// and does not modify any field of progressFlags.
//
// The test creates a real mailbox.Registry with a sibling and a scheduler alias,
// sets ProgressNotifyEnabled=false, then calls progressNotify with an
// ExecuteResult that would trigger all three conditions. After the call it
// drains all mailboxes and asserts they are empty, and checks that
// progressFlags remains all-false.
//
// **Validates: Requirements 10.2**
func TestProperty8_ConfigDisabledSkipsAll(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 100,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			values[0] = reflect.ValueOf("worker-" + randomString(rng, 6))
			// maxLoops in [4, 200] so halfway can trigger
			values[1] = reflect.ValueOf(rng.Intn(197) + 4)
		},
	}

	prop := func(agentID string, maxLoops int) bool {
		// Set up a real mailbox.Registry with a sibling and a scheduler
		reg := mailbox.NewRegistry(64)
		siblingMB := reg.Register("sibling-001", "")
		schedMB := reg.Register("sched-001", "__scheduler__")
		reg.RegisterAlias("scheduler", "sched-001")
		reg.Register(agentID, "")

		ag := &Agent{
			ID:                    agentID,
			MaxLoops:              maxLoops,
			ProgressNotifyEnabled: false, // disabled!
			MailRegistry:          reg,
		}

		// Build an ExecuteResult that triggers ALL three conditions
		triggeringResult := ExecuteResult{
			ToolCalled: true,
			ToolCalls: []llm.ToolCall{
				{ID: "tc1", Name: "write_file", Arguments: map[string]any{"path": "src/foo.go"}},
				{ID: "tc2", Name: "publish_subtask", Arguments: map[string]any{}},
			},
			ToolResults: []ToolResult{
				{ToolCallID: "tc1", Content: "success: wrote file"},
				{ToolCallID: "tc2", Content: "subtask published"},
			},
		}

		// loopIndex that guarantees halfway: maxLoops-1 > maxLoops/2 for maxLoops >= 4
		loopIndex := maxLoops - 1

		flags := &progressFlags{}

		ctx := context.Background()
		ag.progressNotify(ctx, "task-001", loopIndex, triggeringResult, flags)

		// Verify: no messages were sent to sibling
		if msgs := siblingMB.Drain(); len(msgs) != 0 {
			t.Logf("sibling got %d messages, want 0 (agentID=%s, maxLoops=%d)",
				len(msgs), agentID, maxLoops)
			return false
		}

		// Verify: no messages were sent to scheduler
		if msgs := schedMB.Drain(); len(msgs) != 0 {
			t.Logf("scheduler got %d messages, want 0 (agentID=%s, maxLoops=%d)",
				len(msgs), agentID, maxLoops)
			return false
		}

		// Verify: progressFlags remains all false (unchanged)
		if flags.notifiedFileWrite || flags.notifiedSubtask || flags.notifiedHalfway {
			t.Logf("flags modified: fw=%v sub=%v hw=%v (want all false)",
				flags.notifiedFileWrite, flags.notifiedSubtask, flags.notifiedHalfway)
			return false
		}

		return true
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 8 failed: %v", err)
	}
}

// TestProperty4_AtMostOncePerTriggerType verifies that for any sequence of
// progressNotify calls on the same progressFlags instance, each trigger type
// (file_write, subtask, halfway) produces at most one Send call, even when
// every call in the sequence satisfies all three trigger conditions.
//
// The test creates a real mailbox.Registry with a sibling agent and a scheduler
// alias, then calls progressNotify N times (N in [2,10]) with results that
// always trigger all three conditions. After all calls, it drains the mailboxes
// and verifies exactly 3 total messages were sent (one per trigger type).
//
// **Validates: Requirements 1.3, 2.2, 3.2**
func TestProperty4_AtMostOncePerTriggerType(t *testing.T) {
	cfg := &quick.Config{
		MaxCount: 100,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			// N: number of repeated progressNotify calls [2, 10]
			n := rng.Intn(9) + 2
			values[0] = reflect.ValueOf(n)
			// Random agent ID
			values[1] = reflect.ValueOf("worker-" + randomString(rng, 6))
			// maxLoops in [4, 200] (need at least 4 so halfway can trigger)
			values[2] = reflect.ValueOf(rng.Intn(197) + 4)
		},
	}

	prop := func(n int, agentID string, maxLoops int) bool {
		// Set up a real mailbox.Registry with a sibling and a scheduler
		reg := mailbox.NewRegistry(64)
		siblingMB := reg.Register("sibling-001", "")
		schedMB := reg.Register("sched-001", "__scheduler__")
		reg.RegisterAlias("scheduler", "sched-001")
		// Register the agent itself (Send with To="*" skips self)
		reg.Register(agentID, "")

		ag := &Agent{
			ID:                    agentID,
			MaxLoops:              maxLoops,
			ProgressNotifyEnabled: true,
			MailRegistry:          reg,
		}

		// Build an ExecuteResult that triggers ALL three conditions:
		// - write_file with successful result
		// - publish_subtask
		// - loopIndex > maxLoops/2 (use maxLoops-1 to guarantee halfway)
		triggeringResult := ExecuteResult{
			ToolCalled: true,
			ToolCalls: []llm.ToolCall{
				{ID: "tc1", Name: "write_file", Arguments: map[string]any{"path": "src/foo.go"}},
				{ID: "tc2", Name: "publish_subtask", Arguments: map[string]any{}},
			},
			ToolResults: []ToolResult{
				{ToolCallID: "tc1", Content: "success: wrote file"},
				{ToolCallID: "tc2", Content: "subtask published"},
			},
		}

		// loopIndex that guarantees halfway: maxLoops-1 > maxLoops/2 for maxLoops >= 4
		loopIndex := maxLoops - 1

		// Single flags instance shared across all calls
		flags := &progressFlags{}

		// Call progressNotify N times with the same flags
		ctx := context.Background()
		for i := 0; i < n; i++ {
			ag.progressNotify(ctx, "task-001", loopIndex, triggeringResult, flags)
		}

		// Drain sibling mailbox: should have file_write (To="*") + halfway (To="*")
		siblingMsgs := siblingMB.Drain()
		// Drain scheduler mailbox: should have subtask (To="scheduler") + broadcasts (To="*")
		schedMsgs := schedMB.Drain()

		// Count by notify type from sibling perspective:
		// Sibling receives broadcasts (To="*"): file_write + halfway = 2
		// Scheduler receives: subtask (To="scheduler") + broadcasts (To="*") = 3
		// But we need to count total unique sends. Let's count all messages.

		// file_write broadcast: sibling gets 1, sched gets 1 = 2 deliveries from 1 Send
		// subtask point-to-point: sched gets 1 = 1 delivery from 1 Send
		// halfway broadcast: sibling gets 1, sched gets 1 = 2 deliveries from 1 Send
		// Total: sibling should have 2 msgs, sched should have 3 msgs

		// Verify sibling got exactly 2 messages (file_write + halfway broadcasts)
		if len(siblingMsgs) != 2 {
			t.Logf("sibling got %d messages, want 2 (n=%d, agentID=%s, maxLoops=%d)",
				len(siblingMsgs), n, agentID, maxLoops)
			return false
		}

		// Verify scheduler got exactly 3 messages (subtask + file_write broadcast + halfway broadcast)
		if len(schedMsgs) != 3 {
			t.Logf("scheduler got %d messages, want 3 (n=%d, agentID=%s, maxLoops=%d)",
				len(schedMsgs), n, agentID, maxLoops)
			return false
		}

		// Verify flags are all set (each trigger fired exactly once)
		if !flags.notifiedFileWrite || !flags.notifiedSubtask || !flags.notifiedHalfway {
			t.Logf("flags not all set: fw=%v sub=%v hw=%v",
				flags.notifiedFileWrite, flags.notifiedSubtask, flags.notifiedHalfway)
			return false
		}

		return true
	}

	if err := quick.Check(prop, cfg); err != nil {
		t.Errorf("Property 4 failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Task 4.5: 故障隔离场景单元测试
// ---------------------------------------------------------------------------

// TestProgressNotify_NilMailRegistry_NoPanic verifies that calling
// progressNotify with MailRegistry==nil and ProgressNotifyEnabled==true
// does not panic and leaves progressFlags unchanged.
//
// Validates: Requirements 5.3
func TestProgressNotify_NilMailRegistry_NoPanic(t *testing.T) {
	ag := &Agent{
		ID:                    "worker-nil-reg",
		MaxLoops:              10,
		ProgressNotifyEnabled: true,
		MailRegistry:          nil, // explicitly nil
	}

	// Build a result that would trigger all three conditions
	result := ExecuteResult{
		ToolCalled: true,
		ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "write_file", Arguments: map[string]any{"path": "src/foo.go"}},
			{ID: "tc2", Name: "publish_subtask", Arguments: map[string]any{}},
		},
		ToolResults: []ToolResult{
			{ToolCallID: "tc1", Content: "success"},
			{ToolCallID: "tc2", Content: "ok"},
		},
	}

	flags := &progressFlags{}
	ctx := context.Background()

	// Must not panic
	ag.progressNotify(ctx, "task-nil-reg", 8, result, flags)

	// Flags should remain untouched (early return before any detection)
	if flags.notifiedFileWrite || flags.notifiedSubtask || flags.notifiedHalfway {
		t.Errorf("flags should be all false when MailRegistry is nil, got fw=%v sub=%v hw=%v",
			flags.notifiedFileWrite, flags.notifiedSubtask, flags.notifiedHalfway)
	}
}

// TestProgressNotify_SendError_NoInterrupt verifies that when
// mailbox.Registry.Send returns an error (e.g., unknown recipient),
// progressNotify does not panic, continues processing subsequent triggers,
// and still sets the corresponding flags.
//
// Validates: Requirements 6.1
func TestProgressNotify_SendError_NoInterrupt(t *testing.T) {
	// Create a registry but do NOT register "scheduler" alias or any agent
	// except the agent itself. This means:
	// - Broadcast (To="*") will succeed but deliver to nobody (only self, which is skipped)
	// - Point-to-point (To="scheduler") will fail with "未知收件人" error
	reg := mailbox.NewRegistry(64)
	reg.Register("worker-send-err", "")
	// Intentionally NOT registering scheduler alias

	ag := &Agent{
		ID:                    "worker-send-err",
		MaxLoops:              10,
		ProgressNotifyEnabled: true,
		MailRegistry:          reg,
	}

	// Result that triggers file_write and subtask (subtask Send will fail)
	result := ExecuteResult{
		ToolCalled: true,
		ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "write_file", Arguments: map[string]any{"path": "src/bar.go"}},
			{ID: "tc2", Name: "publish_subtask", Arguments: map[string]any{}},
		},
		ToolResults: []ToolResult{
			{ToolCallID: "tc1", Content: "success"},
			{ToolCallID: "tc2", Content: "ok"},
		},
	}

	flags := &progressFlags{}
	ctx := context.Background()

	// loopIndex=8, maxLoops=10 → 8 > 10/2=5 → halfway triggers too
	ag.progressNotify(ctx, "task-send-err", 8, result, flags)

	// All three flags should be set despite the subtask Send error
	if !flags.notifiedFileWrite {
		t.Error("notifiedFileWrite should be true (broadcast succeeds even with no recipients)")
	}
	if !flags.notifiedSubtask {
		t.Error("notifiedSubtask should be true (flag set even when Send returns error)")
	}
	if !flags.notifiedHalfway {
		t.Error("notifiedHalfway should be true (broadcast succeeds)")
	}
}

// TestProgressNotify_PanicRecovery verifies that a panic inside the
// progressNotify method body is caught by defer/recover and does not
// propagate to the caller.
//
// We trigger a panic by passing a nil progressFlags pointer. The method
// body accesses flags.notifiedFileWrite after the nil/config guard, so
// a nil flags will cause a nil pointer dereference panic inside the
// defer/recover scope.
//
// Validates: Requirements 6.2
func TestProgressNotify_PanicRecovery(t *testing.T) {
	reg := mailbox.NewRegistry(64)
	reg.Register("worker-panic", "")

	ag := &Agent{
		ID:                    "worker-panic",
		MaxLoops:              10,
		ProgressNotifyEnabled: true,
		MailRegistry:          reg,
	}

	// Result that triggers file_write detection, which will access flags.notifiedFileWrite
	result := ExecuteResult{
		ToolCalled: true,
		ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "write_file", Arguments: map[string]any{"path": "src/panic.go"}},
		},
		ToolResults: []ToolResult{
			{ToolCallID: "tc1", Content: "success"},
		},
	}

	ctx := context.Background()

	// Passing nil flags will cause a nil pointer dereference inside progressNotify
	// after the MailRegistry/config guard. The defer/recover should catch it.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panic escaped progressNotify: %v", r)
		}
	}()

	ag.progressNotify(ctx, "task-panic", 8, result, nil)
	// If we reach here, the panic was successfully recovered
}

// TestProgressNotify_TraceEventKind verifies that when a progress notification
// is successfully sent, a trace.Event with Kind==trace.KindProgressNotify is
// emitted, containing the correct TaskID, AgentID, Loop, and NotifyType.
//
// Validates: Requirements 8.1
func TestProgressNotify_TraceEventKind(t *testing.T) {
	// Set up a real trace writer to a temp directory
	traceDir := t.TempDir()
	tw, err := trace.NewWriter(traceDir, 0)
	if err != nil {
		t.Fatalf("failed to create trace writer: %v", err)
	}
	defer tw.Close()

	// Save and restore the default trace writer
	oldDefault := trace.Default()
	trace.SetDefault(tw)
	defer trace.SetDefault(oldDefault)

	// Set up mailbox registry with a sibling so broadcasts actually deliver
	reg := mailbox.NewRegistry(64)
	siblingMB := reg.Register("sibling-trace", "")
	reg.Register("worker-trace", "")

	ag := &Agent{
		ID:                    "worker-trace",
		MaxLoops:              20,
		ProgressNotifyEnabled: true,
		MailRegistry:          reg,
	}

	// Result that triggers file_write
	result := ExecuteResult{
		ToolCalled: true,
		ToolCalls: []llm.ToolCall{
			{ID: "tc1", Name: "write_file", Arguments: map[string]any{"path": "src/traced.go"}},
		},
		ToolResults: []ToolResult{
			{ToolCallID: "tc1", Content: "success"},
		},
	}

	flags := &progressFlags{}
	ctx := context.Background()
	taskID := "task-trace-001"

	ag.progressNotify(ctx, taskID, 3, result, flags)

	// Drain sibling to confirm message was sent
	if msgs := siblingMB.Drain(); len(msgs) == 0 {
		t.Fatal("expected sibling to receive file_write broadcast")
	}

	// Close the trace task to flush
	tw.CloseTask(taskID)

	// Read back trace events from the temp directory
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		t.Fatalf("failed to read trace dir: %v", err)
	}

	var found bool
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(traceDir, entry.Name()))
		if err != nil {
			t.Fatalf("failed to read trace file: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var ev trace.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatalf("failed to unmarshal trace event: %v", err)
			}
			if ev.Kind == trace.KindProgressNotify {
				found = true
				if ev.TaskID != taskID {
					t.Errorf("trace TaskID=%q, want %q", ev.TaskID, taskID)
				}
				if ev.AgentID != "worker-trace" {
					t.Errorf("trace AgentID=%q, want %q", ev.AgentID, "worker-trace")
				}
				if ev.Loop != 3 {
					t.Errorf("trace Loop=%d, want 3", ev.Loop)
				}
				if ev.NotifyType != "file_write" {
					t.Errorf("trace NotifyType=%q, want %q", ev.NotifyType, "file_write")
				}
			}
		}
	}

	if !found {
		t.Error("no trace event with KindProgressNotify found")
	}
}
