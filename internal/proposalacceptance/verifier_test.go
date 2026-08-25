package proposalacceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/contextcontract"
	"agentgo/internal/contextstore"
	"agentgo/internal/graph"
	"agentgo/internal/invocation"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
)

type fakeVerifierClient struct {
	content    string
	err        error
	wait       bool
	calls      int
	messages   []llm.Message
	tools      []llm.ToolDef
	binding    invocation.ContextBinding
	started    chan struct{}
	duplicates int
}

func (f *fakeVerifierClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	f.calls++
	f.messages = append([]llm.Message(nil), messages...)
	f.tools = append([]llm.ToolDef(nil), tools...)
	f.binding, _ = invocation.ContextBindingFrom(ctx)
	if f.started != nil {
		close(f.started)
	}
	if f.wait {
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}
	if f.err != nil {
		return llm.Response{}, f.err
	}
	if len(tools) == 1 && tools[0].Name == proposalVerdictToolName {
		arguments := make(map[string]any)
		if err := json.Unmarshal([]byte(f.content), &arguments); err != nil {
			arguments = map[string]any{"raw_output": f.content}
		}
		count := f.duplicates
		if count < 1 {
			count = 1
		}
		calls := make([]llm.ToolCall, count)
		for index := range calls {
			calls[index] = llm.ToolCall{
				ID: fmt.Sprintf("proposal-verdict-%d", index), Name: proposalVerdictToolName, Arguments: arguments,
			}
		}
		return llm.Response{FinishReason: llm.FinishReasonToolCalls, ToolCalls: calls}, nil
	}
	return llm.Response{Content: f.content, FinishReason: llm.FinishReasonStop}, nil
}

type capturingSnapshotStore struct {
	inner     *contextstore.Store
	snapshots []contextcontract.ContextSnapshot
}

func (s *capturingSnapshotStore) Put(snapshot contextcontract.ContextSnapshot) (contextstore.Record, error) {
	s.snapshots = append(s.snapshots, snapshot)
	return s.inner.Put(snapshot)
}

func proposalFixture(rawRequest string) graph.ProposalAcceptanceInput {
	body := graph.GraphDefinitionBody{
		Schema: graph.SchemaV2, Root: "work",
		Nodes: map[string]graph.GraphDefinitionNode{
			"work": {Kind: graph.KindAgent, Task: &graph.NodeTask{Title: "完成请求"}, Next: []graph.Transition{{To: "done"}}},
			"done": {Kind: graph.KindEnd, Task: &graph.NodeTask{Title: "成功"}, Next: []graph.Transition{}, EndOutcome: graph.DefinitionEndSuccess},
		},
	}
	requestDigest := schedulerRequestDigest("", rawRequest)
	contract := graph.GraphContract{
		RequestRef: "request:1", RequestDigest: requestDigest,
		ExecutionClass: graph.ExecutionReadOnly,
		Deliverables:   []graph.ContractRequirement{{ID: "answer", Kind: "answer"}},
	}
	return graph.ProposalAcceptanceInput{
		ProposalID: "proposal-1", GraphID: "graph-1", DefinitionRevision: 1,
		RequestRef: contract.RequestRef, RequestDigest: requestDigest,
		Contract: contract, ContractDigest: graph.ComputeGraphContractDigest(contract),
		Definition: body, DefinitionDigest: graph.ComputeGraphDefinitionDigest("graph-1", 1, body),
	}
}

func newTestVerifier(t *testing.T, client *fakeVerifierClient, rawRequest string, options Options) (*Verifier, *capturingSnapshotStore) {
	t.Helper()
	store, err := contextstore.New(filepath.Join(t.TempDir(), "contexts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capturing := &capturingSnapshotStore{inner: store}
	verifier, err := New(client, RequestTextResolverFunc(func(ctx context.Context, ref string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if ref != "request:1" {
			return "", errors.New("未知 request ref")
		}
		return rawRequest, nil
	}), capturing, options)
	if err != nil {
		t.Fatal(err)
	}
	return verifier, capturing
}

func TestVerifierPassUsesCompiledContextEmptyToolsAndFrameworkRef(t *testing.T) {
	rawRequest := "请调查并回答问题"
	client := &fakeVerifierClient{content: `{"verdict":"pass"}`}
	verifier, snapshots := newTestVerifier(t, client, rawRequest, Options{})
	input := proposalFixture(rawRequest)

	first, err := verifier.EvaluateProposal(context.Background(), input)
	if err != nil {
		t.Fatalf("EvaluateProposal: %v", err)
	}
	second, err := verifier.EvaluateProposal(context.Background(), input)
	if err != nil {
		t.Fatalf("EvaluateProposal second: %v", err)
	}
	if first.Verdict != graph.ProposalAcceptancePass || first.Ref == "" ||
		first.Ref == "model-controlled-ref" || !strings.HasPrefix(first.Ref, "proposal-acceptance:") {
		t.Fatalf("pass decision/ref 非框架权威: %+v", first)
	}
	if first.Ref != second.Ref {
		t.Fatalf("相同输入+输出的 acceptance ref 不稳定: first=%s second=%s", first.Ref, second.Ref)
	}
	client.content = `{"verdict":"pass"}`
	third, err := verifier.EvaluateProposal(context.Background(), input)
	if err != nil || third.Ref != first.Ref {
		t.Fatalf("模型 ref 不得扰动 framework ref: first=%s third=%s err=%v", first.Ref, third.Ref, err)
	}
	if client.calls != 3 || len(client.tools) != 1 || client.tools[0].Name != proposalVerdictToolName {
		t.Fatalf("Verifier 调用/工具面错误: calls=%d tools=%d", client.calls, len(client.tools))
	}
	if client.binding.ToolChoice.Mode != invocation.ToolChoiceAuto || client.binding.ToolChoice.Name != "" {
		t.Fatalf("Verifier 未冻结 auto-singleton verdict ToolChoice: %+v", client.binding.ToolChoice)
	}
	if len(client.messages) != 4 || client.messages[0].Role != "system" ||
		!strings.Contains(client.messages[0].Content, "独立 Graph Proposal Verifier") {
		t.Fatalf("独立 verifier prompt 未经 L2 注入: %+v", client.messages)
	}
	joined := ""
	for _, message := range client.messages {
		joined += message.Content
	}
	for _, required := range []string{rawRequest, "GraphContract JSON", "Normalized GraphDefinition"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("Verifier context 缺少 %q", required)
		}
	}
	if len(snapshots.snapshots) != 3 {
		t.Fatalf("每次 LLM 调用前必须持久化 Snapshot: %d", len(snapshots.snapshots))
	}
	latest := snapshots.snapshots[len(snapshots.snapshots)-1]
	if client.binding.ContextSnapshotID != latest.SnapshotID ||
		client.binding.InvocationID != latest.InvocationID ||
		client.binding.EncodedRequestDigest != latest.EncodedRequestDigest {
		t.Fatalf("Verifier provider request 未绑定 durable Snapshot: binding=%+v snapshot=%+v", client.binding, latest)
	}
	for _, snapshot := range snapshots.snapshots {
		if snapshot.ContextPolicyID != policycatalog.ContextDefaultCurrent ||
			!strings.Contains(snapshot.ToolRouterSnapshotID, "proposal-verifier-verdict") {
			t.Fatalf("Snapshot policy/tool router 未冻结: %+v", snapshot)
		}
	}
}

func TestVerifierAcceptsProviderDuplicateAutoSingletonVerdicts(t *testing.T) {
	rawRequest := "请调查并回答问题"
	client := &fakeVerifierClient{content: `{"verdict":"pass"}`, duplicates: 3}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{})
	decision, err := verifier.EvaluateProposal(context.Background(), proposalFixture(rawRequest))
	if err != nil {
		t.Fatalf("provider auto-singleton fan-out 不应阻断 proposal acceptance: %v", err)
	}
	if decision.Verdict != graph.ProposalAcceptancePass || client.calls != 1 {
		t.Fatalf("应按 provider 顺序消费首个 verdict: decision=%+v calls=%d", decision, client.calls)
	}
}

func TestVerifierFixableMapsBoundedIssuesAndWarnings(t *testing.T) {
	rawRequest := "实现并验证"
	client := &fakeVerifierClient{content: `{
  "verdict":"fixable",
  "issue_code":"MISSING_FAILURE_PATH",
  "message":"缺少失败出口"
}`}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{})
	decision, err := verifier.EvaluateProposal(context.Background(), proposalFixture(rawRequest))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Verdict != graph.ProposalAcceptanceFixable || len(decision.Issues) != 1 ||
		len(decision.Warnings) != 0 || decision.Issues[0].Code != "MISSING_FAILURE_PATH" || !decision.Issues[0].Retryable {
		t.Fatalf("fixable decision 映射错误: %+v", decision)
	}
}

func TestVerifierMalformedJSONReturnsErrorAfterSingleCall(t *testing.T) {
	rawRequest := "回答"
	client := &fakeVerifierClient{content: "```json\n{\"verdict\":\"pass\"}\n```"}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{})
	if _, err := verifier.EvaluateProposal(context.Background(), proposalFixture(rawRequest)); err == nil {
		t.Fatal("Markdown/畸形输出必须返回 error，让 commit blocked")
	}
	if client.calls != 1 {
		t.Fatalf("malformed 不得触发内部 retry: calls=%d", client.calls)
	}
}

func TestVerifierNonPassStillRequiresPrimaryIssue(t *testing.T) {
	rawRequest := "实现并验证"
	client := &fakeVerifierClient{content: `{"verdict":"fixable"}`}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{})
	if _, err := verifier.EvaluateProposal(context.Background(), proposalFixture(rawRequest)); err == nil ||
		!strings.Contains(err.Error(), "主 issue") {
		t.Fatalf("非 pass 省略主诊断必须 fail-closed: %v", err)
	}
}

func TestVerifierInvocationFailureReturnsErrorWithoutRetry(t *testing.T) {
	rawRequest := "回答"
	sentinel := errors.New("provider unavailable")
	client := &fakeVerifierClient{err: sentinel}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{})
	if _, err := verifier.EvaluateProposal(context.Background(), proposalFixture(rawRequest)); !errors.Is(err, sentinel) {
		t.Fatalf("Invocation failure 未原样交 Graph compiler blocked: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("Invocation failure 不得内部 retry: calls=%d", client.calls)
	}
}

func TestVerifierCancellationStopsOnlyInvocation(t *testing.T) {
	rawRequest := "回答"
	client := &fakeVerifierClient{wait: true, started: make(chan struct{})}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := verifier.EvaluateProposal(ctx, proposalFixture(rawRequest))
		done <- err
	}()
	<-client.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消错误=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Verifier 未响应 ctx cancel")
	}
	if client.calls != 1 {
		t.Fatalf("取消不得重试: calls=%d", client.calls)
	}
}

func TestVerifierHonorsRunDeadline(t *testing.T) {
	rawRequest := "回答"
	base := time.Now().UTC()
	client := &fakeVerifierClient{wait: true}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{Now: func() time.Time { return base }})
	input := proposalFixture(rawRequest)
	input.Definition.RunID = "run-verifier"
	input.Definition.RunContract = &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: "run-verifier", CreatedAt: base.Add(-time.Second),
		DeadlineAt: base.Add(120 * time.Millisecond), FinalizationReserve: 10 * time.Millisecond,
		RecoveryReserve: 10 * time.Millisecond, BudgetProfile: "test/v1",
	}
	input.RequestDigest = schedulerRequestDigest("run-verifier", rawRequest)
	input.Contract.RequestDigest = input.RequestDigest
	input.ContractDigest = graph.ComputeGraphContractDigest(input.Contract)
	input.DefinitionDigest = graph.ComputeGraphDefinitionDigest(input.GraphID, input.DefinitionRevision, input.Definition)

	_, err := verifier.EvaluateProposal(context.Background(), input)
	if !errors.Is(err, invocation.ErrRunDeadline) {
		t.Fatalf("Run deadline cause 未保留: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("Run deadline 不得触发 retry: calls=%d", client.calls)
	}
}

func TestVerifierOversizedOutputReturnsError(t *testing.T) {
	rawRequest := "回答"
	client := &fakeVerifierClient{content: strings.Repeat("x", 1025)}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{MaxOutputBytes: 1024})
	if _, err := verifier.EvaluateProposal(context.Background(), proposalFixture(rawRequest)); err == nil ||
		!strings.Contains(err.Error(), "超过") {
		t.Fatalf("超大输出未被有界拒绝: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("超大输出不得重试: calls=%d", client.calls)
	}
}

func TestVerifierContextOversizeFailsBeforeLLM(t *testing.T) {
	rawRequest := strings.Repeat("request", 600_000)
	client := &fakeVerifierClient{content: `{"verdict":"pass"}`}
	verifier, _ := newTestVerifier(t, client, rawRequest, Options{})
	if _, err := verifier.EvaluateProposal(context.Background(), proposalFixture(rawRequest)); err == nil ||
		!strings.Contains(err.Error(), "Context 编译失败") {
		t.Fatalf("超大输入未在 L2 fail-closed: %v", err)
	}
	if client.calls != 0 {
		t.Fatalf("Context 失败后不得调用模型: calls=%d", client.calls)
	}
}
