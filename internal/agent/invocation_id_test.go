package agent

import (
	"context"
	"fmt"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// V6 §7.2：LLMExecutor.Execute 每次调用生成 InvocationID 并挂到本轮
// context_manifest_built / llm_call_start / llm_call_end 三处事件——同一轮
// 三事件可经 invocation_id 精确关联，不同轮次 id 不同。
func TestProcessTask_InvocationIDCorrelatesTripleEvents(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()

	task := &model.Task{Description: "invocation id e2e", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	// 两轮：第一轮调工具，第二轮纯文本完成
	mock := &mockLLMClient{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}}},
		{Content: "完成"},
	}}
	tools := NewToolRegistry()
	tools.Register("read_file", "读取文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "ok", nil
	})
	executor := NewLLMExecutor(mock, tools, nil, nil, nil, "", "系统提示")
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	// 按 loop 归集三类事件的 InvocationID
	type triple struct {
		manifest, start, end string
	}
	loops := map[int]*triple{}
	for _, ev := range readTraceEventsFromDir(t, dir) {
		if ev.TaskID != task.ID {
			continue
		}
		tr := loops[ev.Loop]
		if tr == nil {
			tr = &triple{}
			loops[ev.Loop] = tr
		}
		switch ev.Kind {
		case trace.KindContextManifestBuilt:
			tr.manifest = ev.InvocationID
		case trace.KindLLMCallStart:
			tr.start = ev.InvocationID
		case trace.KindLLMCallEnd:
			tr.end = ev.InvocationID
		}
	}

	if len(loops) != 2 {
		t.Fatalf("期望 2 个 loop 的事件，实际 %d", len(loops))
	}
	for loop := 0; loop < 2; loop++ {
		tr := loops[loop]
		if tr == nil || tr.manifest == "" || tr.start == "" || tr.end == "" {
			t.Fatalf("loop %d 三事件 InvocationID 有空缺: %+v", loop, tr)
		}
		if tr.manifest != tr.start || tr.start != tr.end {
			t.Errorf("loop %d 三事件 InvocationID 不一致: manifest=%q start=%q end=%q",
				loop, tr.manifest, tr.start, tr.end)
		}
		// 格式 <AttemptID>/turn-N/invocation-seq，跨重试/恢复不碰撞。
		wantPrefix := fmt.Sprintf("%s/attempt-1/turn-%d/invocation-", task.ID, loop+1)
		if got := tr.start; len(got) <= len(wantPrefix) || got[:len(wantPrefix)] != wantPrefix {
			t.Errorf("loop %d InvocationID = %q, 期望前缀 %q", loop, got, wantPrefix)
		}
	}
	if loops[0].start == loops[1].start {
		t.Errorf("不同轮次 InvocationID 应不同，均为 %q", loops[0].start)
	}
}
