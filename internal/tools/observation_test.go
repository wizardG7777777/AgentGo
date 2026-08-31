package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
)

func TestObservationGroupValidatesCurrentAttemptEvidence(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 8, 1, 60)
	task := &model.Task{ID: "task-observation", EventType: "code"}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := tasks.GetTask(task.ID)
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{Timestamp: time.Now().UTC(),
		AttemptID: current.AttemptID, CallID: "call-read", AgentID: "worker-1", ToolName: "read_file", Success: true}); err != nil {
		t.Fatal(err)
	}
	mem := taskmem.NewStore(t.TempDir())
	registry := agent.NewToolRegistry()
	ObservationGroup{Store: tasks, TaskMem: mem, Holder: &fakeHolder{id: task.ID}, AgentID: "worker-1"}.Register(registry)
	result, err := registry.Dispatch(context.Background(), llm.ToolCall{ID: "obs", Name: "record_observation_delta",
		Arguments: map[string]any{"phase": taskmem.ObservationPhaseInvestigate, "facts": []any{map[string]any{
			"text": "已读取目标文件", "evidence_refs": []any{"tool-call:call-read"},
		}}, "resolved_candidates": []any{}, "next_candidates": []any{"执行编辑"}}})
	if err != nil || result == "" {
		t.Fatalf("record_observation_delta: result=%q err=%v", result, err)
	}
	loaded, _ := mem.Load(task.ID)
	if loaded == nil || len(loaded.Facts) != 1 || loaded.Facts[0].Confirmed ||
		loaded.LatestObservationAttemptID != current.AttemptID {
		t.Fatalf("Observation TaskMemory=%+v", loaded)
	}
	var receipt struct {
		Schema          string   `json:"schema"`
		OpenCandidates  []string `json:"open_candidate_refs"`
		SemanticAdvance bool     `json:"semantic_advance"`
	}
	if json.Unmarshal([]byte(result), &receipt) != nil || receipt.Schema != taskmem.ObservationDeltaSchemaV3 ||
		len(receipt.OpenCandidates) != 1 || !receipt.SemanticAdvance {
		t.Fatalf("Observation v3 receipt 非法: %s", result)
	}
	resolveArgs := map[string]any{
		"phase": taskmem.ObservationPhaseImplement,
		"facts": []any{map[string]any{"text": "候选已定位", "evidence_refs": []any{"tool-call:call-read"}}},
		"resolved_candidates": []any{map[string]any{
			"candidate_ref": receipt.OpenCandidates[0], "evidence_refs": []any{"tool-call:call-read"},
		}},
		"next_candidates": []any{"执行验证"},
	}
	if _, err := registry.Dispatch(context.Background(), llm.ToolCall{ID: "old-proof", Name: "record_observation_delta",
		Arguments: resolveArgs}); err == nil || !strings.Contains(err.Error(), "不晚于 predecessor") {
		t.Fatalf("checkpoint 前的旧 evidence 不得关闭新候选: %v", err)
	}
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{Timestamp: time.Now().UTC().Add(time.Second),
		AttemptID: current.AttemptID, CallID: "call-new-read", AgentID: "worker-1", ToolName: "read_file", Success: true}); err != nil {
		t.Fatal(err)
	}
	resolveArgs["facts"] = []any{map[string]any{"text": "候选已定位", "evidence_refs": []any{"tool-call:call-new-read"}}}
	resolveArgs["resolved_candidates"] = []any{map[string]any{
		"candidate_ref": receipt.OpenCandidates[0], "evidence_refs": []any{"tool-call:call-new-read"},
	}}
	if _, err := registry.Dispatch(context.Background(), llm.ToolCall{ID: "new-proof", Name: "record_observation_delta",
		Arguments: resolveArgs}); err != nil {
		t.Fatalf("predecessor 后的新 evidence 应能关闭候选: %v", err)
	}
	if _, err := registry.Dispatch(context.Background(), llm.ToolCall{ID: "bad", Name: "record_observation_delta",
		Arguments: map[string]any{"facts": []any{map[string]any{
			"text": "越权事实", "evidence_refs": []any{"tool-call:foreign"},
		}}}}); err == nil {
		t.Fatal("越权 evidence_ref 必须拒绝")
	}
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{Timestamp: time.Now().UTC(),
		AttemptID: "old-attempt", CallID: "call-old", AgentID: "worker-1", ToolName: "read_file", Success: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Dispatch(context.Background(), llm.ToolCall{ID: "old", Name: "record_observation_delta",
		Arguments: map[string]any{"facts": []any{map[string]any{
			"text": "旧 Attempt 事实", "evidence_refs": []any{"tool-call:call-old"},
		}}}}); err == nil {
		t.Fatal("旧 Attempt evidence_ref 必须拒绝")
	}
	if err := tasks.AppendArtifact(task.ID, "old.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Dispatch(context.Background(), llm.ToolCall{ID: "old-artifact", Name: "record_observation_delta",
		Arguments: map[string]any{"facts": []any{map[string]any{
			"text": "旧产物事实", "evidence_refs": []any{"artifact:old.md"},
		}}}}); err == nil {
		t.Fatal("没有当前 Attempt settled write 的 Artifact 必须拒绝")
	}
}

func TestObservationAuthorityAcceptsAllSettledCurrentAttemptChecks(t *testing.T) {
	tasks := store.NewMemoryTaskStore(make(chan model.Event, 16), 8, 1, 60)
	task := &model.Task{ID: "task-check-authority", EventType: "code"}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	if err := tasks.ClaimTask("worker-1", task.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := tasks.GetTask(task.ID)
	if err := tasks.AppendToolCall(task.ID, store.ToolCallRecord{Timestamp: time.Now().UTC(),
		AttemptID: current.AttemptID, CallID: "call-check", ToolName: "run_check",
		Args: map[string]any{"check_id": "verification"}, Success: true}); err != nil {
		t.Fatal(err)
	}
	checks := checkstore.New(t.TempDir())
	now := time.Now().UTC()
	refs := make([]string, 0, 2)
	for index := 0; index < 2; index++ {
		ref, err := checks.Put(checkstore.Record{Schema: checkstore.SchemaV1, RunID: "run-1",
			TaskID: task.ID, AttemptID: current.AttemptID, CheckID: "verification", Kind: "test",
			CommandDigest: "sha256:" + string(rune('a'+index)), Status: checkstore.StatusFailed,
			ExitCode: 1, WorkspaceRevisionRef: "workspace:empty",
			StartedAt: now.Add(time.Duration(index) * time.Second), SettledAt: now.Add(time.Duration(index+1) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	group := ObservationGroup{Store: tasks, Checks: checks}
	authority, _, latest, err := group.evidenceAuthority(current)
	if err != nil || latest != refs[1] {
		t.Fatalf("Check authority/latest 错误: latest=%s refs=%v err=%v", latest, refs, err)
	}
	for _, ref := range refs {
		if _, ok := authority[ref]; !ok {
			t.Fatalf("schema 可见的 settled CheckRef 必须被 handler 接受: %s authority=%v", ref, authority)
		}
	}
}
