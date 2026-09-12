package contextruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/contextstore"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
	"agentgo/internal/testmodel"
)

func inputFixture() contextruntime.Input {
	return contextruntime.Input{Identity: llm.Identity{InvocationID: "invocation-1", AttemptID: "attempt-1", TurnID: "turn-1", TaskID: "task-1", AgentID: "agent-1", SessionID: "test-session", ContextPolicyID: policycatalog.ContextDefaultCurrent}, Instructions: contextruntime.Instructions{ProfileID: "test", System: "默认角色", Override: "任务角色", Objective: "完成任务", Output: "必须结构化提交"}, ToolRouter: contextruntime.ToolRouterBinding{SnapshotID: "tools-1", Definitions: []llm.ToolDef{{Name: "check", Strict: true, Parameters: map[string]any{"type": "object"}}}}, ExecutionLeaseRef: "lease-1", Options: testmodel.Options()}
}

type failingSnapshots struct {
	calls int
	err   error
}

func (s *failingSnapshots) Put(v contextcontract.ContextSnapshot) (contextstore.Record, error) {
	s.calls++
	return contextstore.Record{}, s.err
}

type capturingInvoker struct {
	calls   int
	request llm.Request
	fixture testmodel.Fixture
}

func (c *capturingInvoker) Invoke(ctx context.Context, r llm.Request, sink llm.EventSink) (llm.Result, error) {
	c.calls++
	c.request = r
	if err := r.Validate(); err != nil {
		return llm.Result{}, err
	}
	return c.fixture.Seal(r.Spec().Options.Protocol)
}

func TestRuntimeAssemblesAndSealsCompleteRequest(t *testing.T) {
	r := testmodel.Runtime(t)
	in := inputFixture()
	in.Options.ReasoningEffort = "low"
	c, err := r.Compile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	spec := c.Request().Spec()
	raw, _ := json.Marshal(spec.Messages)
	if !strings.Contains(string(raw), "任务角色") || strings.Contains(string(raw), "默认角色") || !spec.Tools[0].Strict {
		t.Fatalf("角色覆盖或 schema 不完整: %s", raw)
	}
	if spec.Identity.SnapshotID == "" || c.Snapshot().Schema != contextcontract.SnapshotSchemaV3 {
		t.Fatal("缺少新版本快照")
	}
	in.Identity.InvocationID = "invocation-2"
	in.Options.ReasoningEffort = "none"
	d, err := r.Compile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().EncodedRequestDigest == d.Snapshot().EncodedRequestDigest {
		t.Fatal("推理参数未进入快照请求摘要")
	}
	client := &capturingInvoker{fixture: testmodel.Fixture{Content: "完成"}}
	result, err := r.InvokeCompiled(context.Background(), c, client)
	if err != nil || result.Content() != "完成" || client.calls != 1 {
		t.Fatalf("未通过唯一调用链: %v", err)
	}
}

func TestRuntimeRejectsMissingAndFailedDependencies(t *testing.T) {
	if _, err := (contextruntime.Runtime{Snapshots: testmodel.Runtime(t).Snapshots, Options: testmodel.Runtime(t).Options, Output: testmodel.Runtime(t).Output}).Compile(context.Background(), inputFixture()); err == nil {
		t.Fatal("缺依赖时仍接受编译")
	}
	r := testmodel.Runtime(t)
	repo := &failingSnapshots{err: errors.New("写盘故障")}
	r.Snapshots = repo
	compiled, err := r.Compile(context.Background(), inputFixture())
	if err == nil || repo.calls != 1 {
		t.Fatal("未阻断快照写入失败")
	}
	client := &capturingInvoker{}
	if _, err = r.InvokeCompiled(context.Background(), compiled, client); err == nil || client.calls != 0 {
		t.Fatal("未封存请求进入模型")
	}
}

func TestRuntimeRejectsOldPolicyAndIncompleteToolExchange(t *testing.T) {
	r := testmodel.Runtime(t)
	in := inputFixture()
	in.Identity.ContextPolicyID = "context:default/v10"
	if _, err := r.Compile(context.Background(), in); err == nil {
		t.Fatal("接受旧 Context policy")
	}
	in = inputFixture()
	in.History = []contextcontract.HistoryEntry{{TurnID: "old-turn", AssistantContent: "执行", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "check", Arguments: map[string]any{}}}}}
	if _, err := r.Compile(context.Background(), in); err == nil {
		t.Fatal("接受缺少工具结果的历史")
	}
	in.History[0].ToolResults = []contextcontract.ToolResult{{ToolCallID: "call-1", Content: "成功"}}
	if _, err := r.Compile(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePreservesTypedReplayAndRawHistory(t *testing.T) {
	r := testmodel.Runtime(t)
	in := inputFixture()
	in.History = []contextcontract.HistoryEntry{{TurnID: "old-turn", AssistantContent: "完成", Replay: &llm.ProtocolReplay{Protocol: llm.ProtocolChatCompletions, Fields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"原始状态"`)}}}}
	before, _ := json.Marshal(in.History)
	c, err := r.Compile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(in.History)
	if string(before) != string(after) {
		t.Fatal("编译修改 Raw History")
	}
	messages := c.Request().Spec().Messages
	last := messages[len(messages)-1]
	if last.Replay == nil || string(last.Replay.Fields["reasoning_content"]) != `"原始状态"` {
		t.Fatalf("重放字段丢失: %+v", last)
	}
	in.Options.Protocol = llm.ProtocolResponses
	in.Identity.InvocationID = "invocation-other"
	if _, err = r.Compile(context.Background(), in); err == nil {
		t.Fatal("接受跨协议重放")
	}
}

func TestRequiredReplayFailureAndPersistenceFailureRejectResult(t *testing.T) {
	r := testmodel.Runtime(t)
	in := inputFixture()
	c, err := r.Compile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	client := &capturingInvoker{fixture: testmodel.Fixture{Content: "完成", ExtraFields: map[string]json.RawMessage{"unknown_field": json.RawMessage(`"未知重放语义"`)}}}
	if _, err = r.InvokeCompiled(context.Background(), c, client); err == nil {
		t.Fatal("未知重放字段未拒绝")
	}
	in.Identity.InvocationID = "invocation-2"
	c, err = r.Compile(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	r.Output = contextruntime.NewOutputService(func(contextruntime.OutputRecord) error { return errors.New("轮次落盘故障") })
	client.fixture = testmodel.Fixture{Content: "完成"}
	if _, err = r.InvokeCompiled(context.Background(), c, client); err == nil {
		t.Fatal("轮次持久化失败未阻断结果")
	}
}

func TestStaticInstructionsBudgetAndOperationUseSameCompiler(t *testing.T) {
	r := testmodel.Runtime(t)
	if err := r.ValidateStaticPrompt(context.Background(), contextruntime.StaticPromptProfile{ProfileID: "role", SystemPrompt: strings.Repeat("长指令", 65536)}); err != nil {
		t.Fatalf("模型整体容量内的角色原文不应被局部限额拒绝: %v", err)
	}
	client := &capturingInvoker{fixture: testmodel.Fixture{Content: "验证成功"}}
	_, err := r.InvokeOperation(context.Background(), contextruntime.Instructions{ProfileID: "probe", Objective: "验证"}, nil, nil, testmodel.Options(), client)
	if err != nil {
		t.Fatal(err)
	}
	id := client.request.Spec().Identity
	if id.OperationID == "" || id.TaskID != "" || id.SnapshotID == "probe-snapshot" {
		t.Fatalf("操作身份不正确: %+v", id)
	}
}

func TestHistoryRejectsLegacyJSON(t *testing.T) {
	if _, err := contextcontract.DecodeHistory([]byte(`[{"output":"旧历史"}]`)); err == nil {
		t.Fatal("接受旧数组历史")
	}
	raw, err := contextcontract.EncodeHistory([]contextcontract.HistoryEntry{{TurnID: "turn-1"}})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := contextcontract.DecodeHistory(raw)
	if err != nil || len(entries) != 1 {
		t.Fatalf("当前历史不能往返: %v", err)
	}
}
