package agent

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/gate"
	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// llm_executor_suggestions_test.go 覆盖 V6 §4 H2a 的 Harness 侧契约：
// 结构化观察文本注入、过滤纪律（finalizing / 不可重试 / 有界）、重复熔断、
// suggestion disposition（adopted / abandoned / repeated）与旧文本路径回归。

// rejectingHook 是按工具名匹配、返回固定结构化拒绝决策的测试 Gate。
func rejectingHook(toolName, reasonCode string, retryable bool, actions ...hook.SuggestedAction) *mockExecutorHook {
	return &mockExecutorHook{
		name:     "test-gate",
		phase:    hook.PhasePreCall,
		matchStr: toolName,
		priority: 10,
		decision: hook.ToolHookDecision{
			Action:      hook.Abort,
			AbortReason: "测试拒绝说明",
			HookName:    "test-gate",
			ReasonCode:  reasonCode,
			Suggestions: []hook.Suggestion{
				hook.NewSuggestion("test-gate", reasonCode, "x.md", retryable, actions...),
			},
		},
	}
}

// newSuggestionExecutor 装配带一个 pre-call 拒绝 Gate 的 executor。
// tools 参数列出要注册为可真实执行的工具名（拒绝路径不会触达）。
func newSuggestionExecutor(t *testing.T, h hook.ToolHook, toolNames ...string) (TaskExecutor, *mockLLMForHookTest) {
	t.Helper()
	hookReg := gate.NewRegistry()
	if err := hookReg.Register(gate.WrapToolHook(h)); err != nil {
		t.Fatalf("register gate: %v", err)
	}
	tools := NewToolRegistry()
	for _, name := range toolNames {
		tools.Register(name, "测试工具", nil, func(ctx context.Context, args map[string]any) (string, error) {
			return "ok", nil
		})
	}
	mockLLM := &mockLLMForHookTest{}
	return NewLLMExecutor(mockLLM, tools, hookReg, nil, nil, ""), mockLLM
}

// runRound 执行一轮 ReAct 步：LLM 返回一个工具调用。
func runRound(t *testing.T, executor TaskExecutor, mockLLM *mockLLMForHookTest, taskID string, call llm.ToolCall) ExecuteResult {
	t.Helper()
	mockLLM.responses = append(mockLLM.responses, llm.Response{ToolCalls: []llm.ToolCall{call}})
	ctx := WithAgentContext(context.Background(), "agent-1", taskID, mockLLM.callIndex)
	res, err := executor(ctx, &model.Task{ID: taskID, Description: "test"}, nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// traceEventsOfKind 过滤出指定 Kind 的 trace 事件。
func traceEventsOfKind(events []trace.Event, kind trace.EventKind) []trace.Event {
	out := make([]trace.Event, 0)
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// TestExecutor_StructuredRejectionText 结构化拒绝注入文本包含原因码、
// retryable、建议动作与免责句，并发出 suggestions_returned 事件。
func TestExecutor_StructuredRejectionText(t *testing.T) {
	traceDir := setupTraceWriter(t)
	h := rejectingHook("write_file", "read_before_write", true,
		hook.ToolCallAction("read_file", map[string]any{"path": "x.md"}, "先读取目标文件"))
	executor, mockLLM := newSuggestionExecutor(t, h, "write_file", "read_file")

	res := runRound(t, executor, mockLLM, "task-s1", llm.ToolCall{
		ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "x.md"},
	})

	for _, want := range []string{
		"[拒绝] 原因码=read_before_write retryable=true 说明=测试拒绝说明",
		"建议下一步：",
		"工具调用 read_file(path=x.md)：先读取目标文件",
		"建议不自动执行，采纳需重新经过全部校验",
	} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("注入文本缺少 %q，实际：\n%s", want, res.Output)
		}
	}
	// 旧形态不应出现
	if strings.Contains(res.Output, "[hook 拒绝]") {
		t.Errorf("结构化拒绝不应走旧文本形态：\n%s", res.Output)
	}

	returned := traceEventsOfKind(p1fixesReadTraceEvents(t, traceDir), trace.KindSuggestionsReturned)
	if len(returned) != 1 {
		t.Fatalf("suggestions_returned 事件数 = %d，期望 1", len(returned))
	}
	s := returned[0].Suggestion
	if s == nil || s.ReasonCode != "read_before_write" || !s.Retryable || s.Offered != 1 || s.SuggestionID == "" {
		t.Fatalf("suggestions_returned 载荷不符预期：%+v", s)
	}
}

// TestExecutor_LegacyRejectionTextPreserved 未迁移 Gate（无 ReasonCode /
// Suggestions）的拒绝仍走旧 [hook 拒绝] 纯文本路径（回归）。
func TestExecutor_LegacyRejectionTextPreserved(t *testing.T) {
	setupTraceWriter(t)
	legacy := &mockExecutorHook{
		name:     "legacy-gate",
		phase:    hook.PhasePreCall,
		matchStr: "write_file",
		priority: 10,
		decision: hook.ToolHookDecision{
			Action:      hook.Abort,
			AbortReason: "旧式拒绝",
			HookName:    "legacy-gate",
		},
	}
	executor, mockLLM := newSuggestionExecutor(t, legacy, "write_file")
	res := runRound(t, executor, mockLLM, "task-s2", llm.ToolCall{
		ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "x.md"},
	})
	if !strings.Contains(res.Output, "[hook 拒绝] legacy-gate: 旧式拒绝") {
		t.Fatalf("旧文本路径应保持 [hook 拒绝] 形态，实际：\n%s", res.Output)
	}
	if strings.Contains(res.Output, "建议下一步") {
		t.Fatalf("无结构化字段时不应出现建议块：\n%s", res.Output)
	}
}

// TestExecutor_EscalationOnlyRejection 不可重试拒绝只给升级标记
// （switch_mode / request_replan / blocked），不给工具动作建议。
func TestExecutor_EscalationOnlyRejection(t *testing.T) {
	setupTraceWriter(t)
	h := rejectingHook("write_file", "exec_mode_readonly", false,
		hook.EscalationAction(hook.SuggestKindSwitchMode, "请求用户用 /mode exec normal 切换执行权限模式后再写"))
	executor, mockLLM := newSuggestionExecutor(t, h, "write_file")
	res := runRound(t, executor, mockLLM, "task-s3", llm.ToolCall{
		ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "x.md"},
	})
	if !strings.Contains(res.Output, "原因码=exec_mode_readonly retryable=false") {
		t.Fatalf("应带原因码与 retryable=false：\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "切换模式（需用户）") {
		t.Fatalf("应给 switch_mode 升级标记：\n%s", res.Output)
	}
	if strings.Contains(res.Output, "工具调用 ") {
		t.Fatalf("不可重试拒绝不应给工具动作建议：\n%s", res.Output)
	}
}

// TestExecutor_RepeatFuse 同一（原因码，目标）连续第 3 次拒绝时熔断：
// 不再给动作建议，转为指引 blocked / replan。
func TestExecutor_RepeatFuse(t *testing.T) {
	traceDir := setupTraceWriter(t)
	h := rejectingHook("write_file", "read_before_write", true,
		hook.ToolCallAction("read_file", map[string]any{"path": "x.md"}, "先读取目标文件"))
	executor, mockLLM := newSuggestionExecutor(t, h, "write_file", "read_file")

	call := llm.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "x.md"}}
	for round := 1; round <= 3; round++ {
		res := runRound(t, executor, mockLLM, "task-s4", call)
		if round < 3 {
			if !strings.Contains(res.Output, "建议下一步：") {
				t.Fatalf("第 %d 次拒绝应仍给建议：\n%s", round, res.Output)
			}
			continue
		}
		// 第 3 次：熔断文本，无动作建议
		if !strings.Contains(res.Output, "同一问题已连续失败 3 次，请提交 blocked 或请求 replan 而不是重试") {
			t.Fatalf("第 3 次拒绝应熔断指引 blocked/replan：\n%s", res.Output)
		}
		if strings.Contains(res.Output, "建议下一步：") {
			t.Fatalf("熔断后不应再给动作建议：\n%s", res.Output)
		}
	}

	returned := traceEventsOfKind(p1fixesReadTraceEvents(t, traceDir), trace.KindSuggestionsReturned)
	if len(returned) != 3 {
		t.Fatalf("suggestions_returned 事件数 = %d，期望 3", len(returned))
	}
	last := returned[2].Suggestion
	if last == nil || last.RepeatCount != 3 || last.FilterReason != "repeat_fuse" || last.Offered != 0 {
		t.Fatalf("第 3 次事件载荷应为 repeat=3 + repeat_fuse + offered=0：%+v", last)
	}
}

// TestExecutor_DispositionAdopted 下一轮工具调用与建议动作结构化匹配
// （工具名 + 最小参数集逐项相等）且通过 Gate → adopted。
func TestExecutor_DispositionAdopted(t *testing.T) {
	traceDir := setupTraceWriter(t)
	h := rejectingHook("write_file", "read_before_write", true,
		hook.ToolCallAction("read_file", map[string]any{"path": "x.md"}, "先读取目标文件"))
	executor, mockLLM := newSuggestionExecutor(t, h, "write_file", "read_file")

	runRound(t, executor, mockLLM, "task-s5", llm.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "x.md"}})
	runRound(t, executor, mockLLM, "task-s5", llm.ToolCall{ID: "c2", Name: "read_file", Arguments: map[string]any{"path": "x.md"}})

	disps := traceEventsOfKind(p1fixesReadTraceEvents(t, traceDir), trace.KindSuggestionDisposition)
	if len(disps) != 1 {
		t.Fatalf("suggestion_disposition 事件数 = %d，期望 1", len(disps))
	}
	if disps[0].Suggestion == nil || disps[0].Suggestion.Disposition != "adopted" {
		t.Fatalf("应为 adopted：%+v", disps[0].Suggestion)
	}
}

// TestExecutor_DispositionAbandoned 下一轮调用了与建议不符的工具 → abandoned。
func TestExecutor_DispositionAbandoned(t *testing.T) {
	traceDir := setupTraceWriter(t)
	h := rejectingHook("write_file", "read_before_write", true,
		hook.ToolCallAction("read_file", map[string]any{"path": "x.md"}, "先读取目标文件"))
	executor, mockLLM := newSuggestionExecutor(t, h, "write_file", "read_file", "list_dir")

	runRound(t, executor, mockLLM, "task-s6", llm.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "x.md"}})
	runRound(t, executor, mockLLM, "task-s6", llm.ToolCall{ID: "c2", Name: "list_dir", Arguments: map[string]any{"path": "."}})

	disps := traceEventsOfKind(p1fixesReadTraceEvents(t, traceDir), trace.KindSuggestionDisposition)
	if len(disps) != 1 || disps[0].Suggestion == nil || disps[0].Suggestion.Disposition != "abandoned" {
		t.Fatalf("应为 abandoned：%+v", disps)
	}
}

// TestExecutor_DispositionRepeated 下一轮再次触发同因同目标的同一拒绝
// → repeated。
func TestExecutor_DispositionRepeated(t *testing.T) {
	traceDir := setupTraceWriter(t)
	h := rejectingHook("write_file", "read_before_write", true,
		hook.ToolCallAction("read_file", map[string]any{"path": "x.md"}, "先读取目标文件"))
	executor, mockLLM := newSuggestionExecutor(t, h, "write_file", "read_file")

	call := llm.ToolCall{ID: "c1", Name: "write_file", Arguments: map[string]any{"path": "x.md"}}
	runRound(t, executor, mockLLM, "task-s7", call)
	runRound(t, executor, mockLLM, "task-s7", call)

	disps := traceEventsOfKind(p1fixesReadTraceEvents(t, traceDir), trace.KindSuggestionDisposition)
	if len(disps) != 1 || disps[0].Suggestion == nil || disps[0].Suggestion.Disposition != "repeated" {
		t.Fatalf("应为 repeated：%+v", disps)
	}
}

// TestFilterSuggestedActions 过滤纪律（V6 §4 思路 9）单元测试。
func TestFilterSuggestedActions(t *testing.T) {
	toolAction := hook.ToolCallAction("read_file", map[string]any{"path": "x.md"}, "读取")
	userAction := hook.EscalationAction(hook.SuggestKindUser, "需用户处理")

	cases := []struct {
		name         string
		s            gate.Suggestion
		finalizing   bool
		wantOffered  int
		wantFiltered int
		wantReason   string
	}{
		{"retryable 保留 tool_call", gate.Suggestion{Retryable: true, Actions: []gate.SuggestedAction{toolAction}}, false, 1, 0, ""},
		{"finalizing 剔除 tool_call", gate.Suggestion{Retryable: true, Actions: []gate.SuggestedAction{toolAction}}, true, 0, 1, "finalizing"},
		{"不可重试剔除 tool_call", gate.Suggestion{Retryable: false, Actions: []gate.SuggestedAction{toolAction}}, false, 0, 1, "not_retryable"},
		{"升级标记始终保留", gate.Suggestion{Retryable: false, Actions: []gate.SuggestedAction{userAction}}, true, 1, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offered, filtered, reason := filterSuggestedActions(tc.s, tc.finalizing)
			if len(offered) != tc.wantOffered || filtered != tc.wantFiltered || reason != tc.wantReason {
				t.Fatalf("got offered=%d filtered=%d reason=%q，期望 %d/%d/%q",
					len(offered), filtered, reason, tc.wantOffered, tc.wantFiltered, tc.wantReason)
			}
		})
	}

	// 有界：超过 3 条截断
	many := gate.Suggestion{Retryable: true, Actions: []gate.SuggestedAction{toolAction, toolAction, toolAction, toolAction}}
	offered, filtered, reason := filterSuggestedActions(many, false)
	if len(offered) != gate.MaxSuggestedActions || filtered != 1 || reason != "over_limit" {
		t.Fatalf("超过上界应截断为 %d 条：offered=%d filtered=%d reason=%q",
			gate.MaxSuggestedActions, len(offered), filtered, reason)
	}
}

// TestActionMatchesCall 结构化匹配：工具名 + 最小参数集逐项相等。
func TestActionMatchesCall(t *testing.T) {
	a := hook.ToolCallAction("read_file", map[string]any{"path": "x.md"}, "读取")
	if !actionMatchesCall(a, llm.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "x.md"}}) {
		t.Fatalf("工具名与参数一致应匹配")
	}
	if actionMatchesCall(a, llm.ToolCall{Name: "write_file", Arguments: map[string]any{"path": "x.md"}}) {
		t.Fatalf("工具名不同不应匹配")
	}
	if actionMatchesCall(a, llm.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "y.md"}}) {
		t.Fatalf("参数值不同不应匹配")
	}
	if actionMatchesCall(a, llm.ToolCall{Name: "read_file", Arguments: map[string]any{}}) {
		t.Fatalf("缺少建议声明的参数不应匹配")
	}
	// 升级类动作永不匹配工具调用
	esc := hook.EscalationAction(hook.SuggestKindUser, "需用户")
	if actionMatchesCall(esc, llm.ToolCall{Name: "read_file", Arguments: map[string]any{"path": "x.md"}}) {
		t.Fatalf("升级类动作不应匹配工具调用")
	}
}
