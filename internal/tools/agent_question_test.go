package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/llm"
)

const validAgentQuestionOptions = `[
  {"id":"keep_scope","label":"保持当前范围"},
  {"id":"change_scope","label":"调整范围","description":"请说明需要如何调整","requires_text":true}
]`

func dispatchAgentQuestion(ctx context.Context, group MetaGroup, args map[string]any) (string, error) {
	registry := agent.NewToolRegistry()
	group.Register(registry)
	return registry.Dispatch(ctx, llm.ToolCall{Name: "request_user_input", Arguments: args})
}

func waitAgentQuestion(t *testing.T, service *interaction.Service, sessionID string) interaction.Request {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		requests, err := service.ListPending(context.Background(), sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(requests) == 1 {
			return requests[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 agent_question Interaction 超时，pending=%d", len(requests))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMetaGroup_RequestUserInputSchemaAndConditionalRegistration(t *testing.T) {
	withoutService := agent.NewToolRegistry()
	MetaGroup{AgentID: "worker-1"}.Register(withoutService)
	for _, def := range withoutService.Defs() {
		if def.Name == "request_user_input" {
			t.Fatal("Interaction Service 为 nil 时不应注册 request_user_input")
		}
	}

	registry := agent.NewToolRegistry()
	MetaGroup{Interactions: interaction.NewService(nil), AgentID: "worker-1"}.Register(registry)
	if len(registry.Defs()) != 1 || registry.Defs()[0].Name != "request_user_input" {
		t.Fatalf("Defs = %+v", registry.Defs())
	}
	schema := registry.Defs()[0].Parameters
	if allow, ok := schema["additionalProperties"].(bool); !ok || allow {
		t.Fatalf("additionalProperties = %#v, want false", schema["additionalProperties"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok || len(properties) != 2 || properties["prompt"] == nil || properties["options_json"] == nil {
		t.Fatalf("properties = %#v", schema["properties"])
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 2 {
		t.Fatalf("required = %#v", schema["required"])
	}
}

func TestRequestUserInputResponseAndTrustedBinding(t *testing.T) {
	service := interaction.NewService(nil)
	var mu sync.Mutex
	var hookCalls []bool
	group := MetaGroup{
		Interactions: service,
		SessionID:    func() string { return "session-question" },
		AgentID:      "worker-1",
		InteractionWaitHook: func(waiting bool) {
			mu.Lock()
			hookCalls = append(hookCalls, waiting)
			mu.Unlock()
		},
	}
	baseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx := agent.WithAgentContext(baseCtx, "worker-1", "task-42", 1)

	type result struct {
		output string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		out, err := dispatchAgentQuestion(ctx, group, map[string]any{
			"prompt": "是否保持当前范围？", "options_json": validAgentQuestionOptions,
		})
		done <- result{output: out, err: err}
	}()

	request := waitAgentQuestion(t, service, "session-question")
	if request.Kind != interaction.KindChoice || request.Purpose != PurposeAgentQuestion {
		t.Fatalf("kind/purpose = %s/%s", request.Kind, request.Purpose)
	}
	if request.Origin != (interaction.Origin{Component: "agent", AgentID: "worker-1", TaskID: "task-42"}) {
		t.Fatalf("Origin = %+v", request.Origin)
	}
	if request.Subject.Kind != "task" || request.Subject.ID != "task-42" || request.Subject.TaskID != "task-42" {
		t.Fatalf("Subject = %+v", request.Subject)
	}
	if request.Resolution.Handler != ResolutionHandlerAgentResponse ||
		request.Resolution.TargetID != "task-42" || request.Resolution.AgentID != "worker-1" ||
		request.Resolution.TaskID != "task-42" {
		t.Fatalf("Resolution = %+v", request.Resolution)
	}
	if len(request.Metadata) != 0 {
		t.Fatalf("普通 Agent 问题不应携带 Metadata: %+v", request.Metadata)
	}
	if request.ExpiresAt.IsZero() {
		t.Fatal("context deadline 未映射到 ExpiresAt")
	}
	for _, option := range request.Options {
		if option.ActionRef != "" {
			t.Fatalf("普通 Agent 选项泄漏 ActionRef: %+v", option)
		}
	}

	locked, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "keep_scope", RespondedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), locked.ID, locked.Version); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		var response agentQuestionResult
		if err := json.Unmarshal([]byte(got.output), &response); err != nil {
			t.Fatalf("output = %q: %v", got.output, err)
		}
		if response.RequestID != request.ID || response.OptionID != "keep_scope" || response.Text != "" {
			t.Fatalf("response = %+v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("回答完成后 request_user_input 未返回")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hookCalls) != 2 || !hookCalls[0] || hookCalls[1] {
		t.Fatalf("InteractionWaitHook calls = %v", hookCalls)
	}
}

func TestRequestUserInputRequiresText(t *testing.T) {
	service := interaction.NewService(nil)
	group := MetaGroup{
		Interactions: service, SessionID: func() string { return "session-text" }, AgentID: "worker-2",
	}
	ctx := agent.WithAgentContext(context.Background(), "worker-2", "task-text", 1)
	done := make(chan struct {
		out string
		err error
	}, 1)
	go func() {
		out, err := dispatchAgentQuestion(ctx, group, map[string]any{
			"prompt": "如何继续？", "options_json": validAgentQuestionOptions,
		})
		done <- struct {
			out string
			err error
		}{out: out, err: err}
	}()
	request := waitAgentQuestion(t, service, "session-text")
	_, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "change_scope",
	})
	if !errors.Is(err, interaction.ErrInvalidRequest) {
		t.Fatalf("缺少 requires_text 回答 error = %v", err)
	}
	locked, err := service.BeginResolve(context.Background(), interaction.ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "change_scope", Text: "只修改 API 层",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), locked.ID, locked.Version); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	var response agentQuestionResult
	if err := json.Unmarshal([]byte(got.out), &response); err != nil {
		t.Fatal(err)
	}
	if response.OptionID != "change_scope" || response.Text != "只修改 API 层" {
		t.Fatalf("response = %+v", response)
	}
}

func TestRequestUserInputStrictValidation(t *testing.T) {
	service := interaction.NewService(nil)
	group := MetaGroup{Interactions: service, AgentID: "worker-1"}
	ctx := agent.WithAgentContext(context.Background(), "worker-1", "task-validation", 1)
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "empty prompt", args: map[string]any{"prompt": " ", "options_json": validAgentQuestionOptions}},
		{name: "options not string", args: map[string]any{"prompt": "p", "options_json": []any{}}},
		{name: "malformed", args: map[string]any{"prompt": "p", "options_json": `[`}},
		{name: "one option", args: map[string]any{"prompt": "p", "options_json": `[{"id":"one","label":"One"}]`}},
		{name: "nine options", args: map[string]any{"prompt": "p", "options_json": `[{"id":"a","label":"A"},{"id":"b","label":"B"},{"id":"c","label":"C"},{"id":"d","label":"D"},{"id":"e","label":"E"},{"id":"f","label":"F"},{"id":"g","label":"G"},{"id":"h","label":"H"},{"id":"i","label":"I"}]`}},
		{name: "action ref forbidden", args: map[string]any{"prompt": "p", "options_json": `[{"id":"a","label":"A","action_ref":"shell.allow"},{"id":"b","label":"B"}]`}},
		{name: "resolution forbidden", args: map[string]any{"prompt": "p", "options_json": `[{"id":"a","label":"A","resolution":{"handler":"plan_control"}},{"id":"b","label":"B"}]`}},
		{name: "metadata forbidden", args: map[string]any{"prompt": "p", "options_json": `[{"id":"a","label":"A","metadata":{"command":"x"}},{"id":"b","label":"B"}]`}},
		{name: "duplicate id", args: map[string]any{"prompt": "p", "options_json": `[{"id":"same","label":"A"},{"id":"same","label":"B"}]`}},
		{name: "unstable id", args: map[string]any{"prompt": "p", "options_json": `[{"id":"Not Stable","label":"A"},{"id":"b","label":"B"}]`}},
		{name: "empty label", args: map[string]any{"prompt": "p", "options_json": `[{"id":"a","label":" "},{"id":"b","label":"B"}]`}},
		{name: "trailing json", args: map[string]any{"prompt": "p", "options_json": `[{"id":"a","label":"A"},{"id":"b","label":"B"}] {}`}},
		{name: "top level privileged field", args: map[string]any{"prompt": "p", "options_json": validAgentQuestionOptions, "metadata": map[string]any{"x": "y"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := group.requestUserInput(ctx, test.args); err == nil {
				t.Fatal("invalid input unexpectedly accepted")
			}
		})
	}
	requests, err := service.List(context.Background(), interaction.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("validation failures created %d requests", len(requests))
	}
}

func TestRequestUserInputCancellationInterruptsAndFailsClosed(t *testing.T) {
	service := interaction.NewService(nil)
	var mu sync.Mutex
	var calls []bool
	group := MetaGroup{
		Interactions: service, SessionID: func() string { return "session-cancel" }, AgentID: "worker-3",
		InteractionWaitHook: func(waiting bool) {
			mu.Lock()
			calls = append(calls, waiting)
			mu.Unlock()
		},
	}
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := agent.WithAgentContext(baseCtx, "worker-3", "task-cancel", 1)
	done := make(chan error, 1)
	go func() {
		_, err := dispatchAgentQuestion(ctx, group, map[string]any{
			"prompt": "继续吗？", "options_json": validAgentQuestionOptions,
		})
		done <- err
	}()
	request := waitAgentQuestion(t, service, "session-cancel")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancel 后工具未返回")
	}
	latest, err := service.Get(context.Background(), request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != interaction.StateInterrupted {
		t.Fatalf("state = %s, want interrupted", latest.State)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("InteractionWaitHook calls = %v", calls)
	}
}

func TestRequestUserInputNilServiceFailsClosed(t *testing.T) {
	group := MetaGroup{AgentID: "worker-1"}
	ctx := agent.WithAgentContext(context.Background(), "worker-1", "task-1", 1)
	_, err := group.requestUserInput(ctx, map[string]any{
		"prompt": "p", "options_json": validAgentQuestionOptions,
	})
	if err == nil || !strings.Contains(err.Error(), "Interaction 服务不可用") {
		t.Fatalf("error = %v", err)
	}
}
