package llm

import (
	"encoding/json"
	"testing"
)

// ============================================================================
// Registry 查找
// ============================================================================

func TestGetProvider_KnownNames(t *testing.T) {
	cases := []struct {
		name     string
		wantType string // 期望的 Name() 返回值
	}{
		{"openai", "openai"},
		{"deepseek-v4", "deepseek-v4"},
		{"deepseek-r1", "deepseek-r1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := GetProvider(c.name)
			if p == nil {
				t.Fatalf("GetProvider(%q) 返回 nil", c.name)
			}
			if got := p.Name(); got != c.wantType {
				t.Errorf("GetProvider(%q).Name() = %q, want %q", c.name, got, c.wantType)
			}
		})
	}
}

func TestGetProvider_EmptyName_FallsBackToOpenAI(t *testing.T) {
	p := GetProvider("")
	if p.Name() != "openai" {
		t.Errorf("空 name 应 fallback 到 openai，实际 = %q", p.Name())
	}
}

func TestGetProvider_UnknownName_FallsBackToOpenAI(t *testing.T) {
	p := GetProvider("nonexistent-xyz-provider")
	if p.Name() != "openai" {
		t.Errorf("未知 name 应 fallback 到 openai，实际 = %q", p.Name())
	}
}

func TestRegisteredProviders_ContainsBuiltins(t *testing.T) {
	names := RegisteredProviders()
	want := map[string]bool{
		"openai":      false,
		"deepseek-v4": false,
		"deepseek-r1": false,
	}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("内置 provider %q 未注册", n)
		}
	}
}

// ============================================================================
// OpenAIProvider —— no-op
// ============================================================================

func TestOpenAIProvider_PrepareMessages_PassThrough(t *testing.T) {
	p := &OpenAIProvider{}
	in := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello", ExtraFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"thinking..."`),
		}},
	}
	out := p.PrepareMessages(in)
	if len(out) != len(in) {
		t.Fatalf("长度改变：got %d, want %d", len(out), len(in))
	}
	// OpenAIProvider 不应修改 extras
	if _, has := out[1].ExtraFields["reasoning_content"]; !has {
		t.Error("OpenAIProvider 不应剥离 reasoning_content")
	}
}

func TestOpenAIProvider_RequestOptions_Empty(t *testing.T) {
	p := &OpenAIProvider{}
	if opts := p.RequestOptions(); len(opts) != 0 {
		t.Errorf("OpenAIProvider.RequestOptions() 应为空，got %d 个", len(opts))
	}
}

// ============================================================================
// DeepSeekV4Provider —— no-op（层 1 通用透传已覆盖 reasoning_content 往返）
// ============================================================================

func TestDeepSeekV4Provider_PrepareMessages_NoOp(t *testing.T) {
	p := &DeepSeekV4Provider{}
	in := []Message{
		{Role: "assistant", Content: "x", ExtraFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"chain of thought"`),
		}},
	}
	out := p.PrepareMessages(in)
	if _, has := out[0].ExtraFields["reasoning_content"]; !has {
		t.Error("DeepSeekV4Provider 不应剥离 reasoning_content（V4 要求保留）")
	}
}

// ============================================================================
// DeepSeekR1Provider —— 剥离 assistant 消息的 reasoning_content
// ============================================================================

func TestDeepSeekR1Provider_StripsReasoningContent_FromAssistant(t *testing.T) {
	p := &DeepSeekR1Provider{}
	in := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1", ExtraFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"thinking..."`),
			"other_field":       json.RawMessage(`"keep me"`),
		}},
		{Role: "user", Content: "q2"},
	}
	out := p.PrepareMessages(in)

	if len(out) != 3 {
		t.Fatalf("长度错误：got %d, want 3", len(out))
	}
	asst := out[1]
	if _, has := asst.ExtraFields["reasoning_content"]; has {
		t.Error("reasoning_content 应被剥离")
	}
	if _, has := asst.ExtraFields["other_field"]; !has {
		t.Error("其它 extras 字段应保留")
	}
	// 不应修改 user 消息
	if out[0].Role != "user" || out[0].Content != "q1" {
		t.Error("user 消息被误改")
	}
}

func TestDeepSeekR1Provider_DoesNotMutateInput(t *testing.T) {
	p := &DeepSeekR1Provider{}
	asstExtras := map[string]json.RawMessage{
		"reasoning_content": json.RawMessage(`"x"`),
	}
	in := []Message{
		{Role: "assistant", Content: "a", ExtraFields: asstExtras},
	}
	p.PrepareMessages(in)
	// 原始输入不应被修改
	if _, has := asstExtras["reasoning_content"]; !has {
		t.Error("PrepareMessages 不应原地修改输入的 ExtraFields map")
	}
}

func TestDeepSeekR1Provider_NoAssistantNoOp(t *testing.T) {
	p := &DeepSeekR1Provider{}
	in := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
		{Role: "tool", Content: "t", ToolCallID: "call_1"},
	}
	out := p.PrepareMessages(in)
	if len(out) != 3 {
		t.Fatalf("长度错误：got %d, want 3", len(out))
	}
	for i := range in {
		if out[i].Role != in[i].Role || out[i].Content != in[i].Content {
			t.Errorf("第 %d 条被误改：%+v → %+v", i, in[i], out[i])
		}
	}
}

func TestDeepSeekR1Provider_EmptyExtrasAfterStrip_SetsNil(t *testing.T) {
	p := &DeepSeekR1Provider{}
	in := []Message{
		{Role: "assistant", Content: "a", ExtraFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"only one"`),
		}},
	}
	out := p.PrepareMessages(in)
	if out[0].ExtraFields != nil {
		t.Errorf("剥光后应是 nil（不是 empty map），实际 = %+v", out[0].ExtraFields)
	}
}

func TestDeepSeekR1Provider_NoReasoningContent_NoOp(t *testing.T) {
	p := &DeepSeekR1Provider{}
	other := map[string]json.RawMessage{"custom": json.RawMessage(`"x"`)}
	in := []Message{
		{Role: "assistant", Content: "a", ExtraFields: other},
	}
	out := p.PrepareMessages(in)
	if _, has := out[0].ExtraFields["custom"]; !has {
		t.Error("未命中 reasoning_content 时不应影响其它 extras")
	}
}
