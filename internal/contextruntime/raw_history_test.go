package contextruntime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
	"agentgo/internal/testmodel"
)

func TestRawContextPreservesLargeRepeatedToolHistory(t *testing.T) {
	r := testmodel.Runtime(t)
	in := inputFixture()
	in.WindowTokens, in.CompletionTokens = 880000, 65536
	in.Instructions.System = strings.Repeat("角色原文，不做引用替换。", 7000)
	in.Instructions.Override = ""
	const turns = 12
	content := strings.Repeat("原始工具内容 \"quoted\" \\path\n", 2500)
	for i := 0; i < turns; i++ {
		call := fmt.Sprintf("call-%d", i)
		in.History = append(in.History, contextcontract.HistoryEntry{
			TurnID: fmt.Sprintf("turn-%d", i), AssistantContent: fmt.Sprintf("原始回复-%d", i),
			ToolCalls:   []llm.ToolCall{{ID: call, Name: "read_evidence", Arguments: map[string]any{"ref_id": "同一个业务证据"}}},
			ToolResults: []contextcontract.ToolResult{{ToolCallID: call, Content: content}},
		})
	}
	before, _ := json.Marshal(in.History)
	compiled, err := r.Compile(context.Background(), in)
	if err != nil {
		t.Fatalf("880K 容量内原文被局部规则阻断: %v", err)
	}
	count := 0
	roleFound := false
	for _, message := range compiled.Request().Spec().Messages {
		if message.Content == in.Instructions.System {
			roleFound = true
		}
		if message.Role == "tool" {
			if message.Content != content {
				t.Fatal("主动读取的正文被裁剪、去重或替换为引用")
			}
			count++
		}
	}
	if count != turns || !roleFound {
		t.Fatalf("历史轮次丢失: %d/%d", count, turns)
	}
	for _, fragment := range compiled.Snapshot().Fragments {
		if fragment.Disposition != contextcontract.DispositionInline || fragment.ContentRef != "" {
			t.Fatalf("新请求存在正文引用投影: %+v", fragment)
		}
	}
	after, _ := json.Marshal(in.History)
	if string(before) != string(after) {
		t.Fatal("装配修改了原始历史")
	}
}

func TestRawContextRejectsRetiredProjectedHistory(t *testing.T) {
	if _, err := contextcontract.DecodeHistory([]byte(`{"schema":"agentgo.model-history/v1","entries":[]}`)); err == nil {
		t.Fatal("旧投影历史不能转成新全文历史执行")
	}
	r := testmodel.Runtime(t)
	in := inputFixture()
	in.Identity.ContextPolicyID = "context:default/v11"
	if _, err := r.Compile(context.Background(), in); err == nil {
		t.Fatal("退役的 Context policy 仍可执行")
	}
	in = inputFixture()
	in.History = []contextcontract.HistoryEntry{{ContextProjection: "aggressive"}}
	if _, err := r.Compile(context.Background(), in); err == nil {
		t.Fatal("退役历史裁剪控制仍可执行")
	}
}

func TestRawContextPreservesExplicitCurrentMaterials(t *testing.T) {
	r := testmodel.Runtime(t)
	in := inputFixture()
	text := strings.Repeat("上游原文", 18000)
	in.Upstream = []contextruntime.MessageBinding{{Message: llm.Message{Role: "user", Content: text}, Kind: contextcontract.FragmentUpstreamResult, Section: contextcontract.SectionUpstreamInputs, SourceRef: "node-result", Scope: contextcontract.ScopeActivation, Authority: contextcontract.AuthorityInformational, Freshness: contextcontract.FreshnessSnapshot}}
	compiled, err := r.Compile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range compiled.Request().Spec().Messages {
		if message.Content == text {
			return
		}
	}
	t.Fatal("上游原文没有进入请求")
}
