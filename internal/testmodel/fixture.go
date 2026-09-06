// Package testmodel 只供测试构造显式 L1/L2 契约，生产代码不得导入。
package testmodel

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextruntime"
	"agentgo/internal/contextstore"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
)

// Fixture 是测试数据描述；Seal 只生成当前的不可变 Result，无模型调用旁路。
type Fixture struct {
	Content      string
	Reasoning    string
	ToolCalls    []llm.ToolCall
	Items        []llm.OutputItem
	ExtraFields  map[string]json.RawMessage
	FinishReason llm.FinishReason
	Usage        llm.Usage
}

func (f Fixture) Seal(protocol llm.Protocol) (llm.Result, error) {
	items := f.Items
	if len(items) == 0 {
		if f.Reasoning != "" {
			items = append(items, llm.OutputItem{Kind: llm.OutputItemReasoning, Reasoning: f.Reasoning})
		}
		if f.Content != "" {
			items = append(items, llm.OutputItem{Kind: llm.OutputItemMessage, Text: f.Content})
		}
		for _, call := range f.ToolCalls {
			c := call
			items = append(items, llm.OutputItem{Kind: llm.OutputItemFunctionCall, ID: c.ID, ToolCall: &c})
		}
	}
	replay := llm.ProtocolReplay{Protocol: protocol, Fields: map[string]json.RawMessage{}}
	for k, v := range f.ExtraFields {
		if k == llm.ReplayItemsBudgetKey {
			if err := json.Unmarshal(v, &replay.Items); err != nil {
				return llm.Result{}, err
			}
		} else {
			replay.Fields[k] = v
		}
	}
	finish := f.FinishReason
	if finish == "" {
		finish = llm.FinishReasonStop
		if len(f.ToolCalls) > 0 {
			finish = llm.FinishReasonToolCalls
		}
	}
	return llm.NewResult(llm.ResultData{Schema: llm.ResultSchema, Items: items, Usage: f.Usage, FinishReason: finish, Replay: replay})
}
func Options() llm.Options {
	return llm.Options{Protocol: llm.ProtocolChatCompletions, Model: "test-model", CapabilityDigest: "test-capability", ProfileRef: "test", ToolChoice: llm.ToolChoice{Mode: llm.ToolChoiceAuto}, OutputBudget: llm.DefaultOutputBudget()}
}
func Runtime(t testing.TB) contextruntime.Runtime {
	t.Helper()
	root := t.TempDir()
	snapshots, err := contextstore.New(filepath.Join(root, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshots.Close() })
	content, err := contentstore.Open(filepath.Join(root, "content"), contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = content.Close() })
	policies, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	return contextruntime.Runtime{Assembler: contextruntime.NewAssembler(), Policies: policies, Snapshots: snapshots, Content: content, Options: Options(), SessionID: func() string { return "test-session" }, Output: contextruntime.NewOutputService(func(contextruntime.OutputRecord) error { return nil })}
}
