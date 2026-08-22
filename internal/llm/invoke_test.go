package llm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agentgo/internal/invocation"
)

type bindingCaptureClient struct {
	binding invocation.ContextBinding
	called  bool
	err     error
}

func TestProductionCallersDoNotBypassInvoke(t *testing.T) {
	var bypasses []string
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		normalized := filepath.ToSlash(path)
		if strings.HasSuffix(normalized, "/llm/invoke.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), ".Chat(") {
			bypasses = append(bypasses, normalized)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bypasses) != 0 {
		t.Fatalf("生产 Model Invocation 绕过 llm.Invoke: %v", bypasses)
	}
}

func (c *bindingCaptureClient) Chat(ctx context.Context, _ []Message, _ []ToolDef) (Response, error) {
	c.called = true
	c.binding, _ = invocation.ContextBindingFrom(ctx)
	return Response{Content: "ok"}, c.err
}

func TestInvokeBindsCanonicalFailureToSnapshot(t *testing.T) {
	failure := invocation.NewFailure(invocation.FailureProviderUnavailable,
		invocation.PhaseResponseHeaders, invocation.OriginProvider, errors.New("down"))
	client := &bindingCaptureClient{err: failure}
	binding := invocation.ContextBinding{
		Schema: invocation.ContextBindingSchemaV1, InvocationID: "invocation-1",
		ContextSnapshotID: "snapshot-1", ContextPolicyID: "context:default/v1",
		ToolRouterSnapshotID: "tool-router-1", EncodedRequestDigest: "sha256:request",
		OutputBudget: DefaultOutputBudget(),
	}
	_, err := Invoke(context.Background(), client, InvocationRequest{
		Binding: binding, Messages: []Message{{Role: "user", Content: "hello"}},
	})
	got, ok := invocation.FromError(err)
	if !ok || got.InvocationID != binding.InvocationID || got.SnapshotID != binding.ContextSnapshotID ||
		got.ProviderPolicy != binding.ContextPolicyID {
		t.Fatalf("canonical Failure 未绑定 Snapshot: %+v,%v", got, ok)
	}
}

func TestInvokeRequiresAndPropagatesContextBinding(t *testing.T) {
	client := &bindingCaptureClient{}
	binding := invocation.ContextBinding{
		Schema: invocation.ContextBindingSchemaV1, InvocationID: "invocation-1",
		ContextSnapshotID: "snapshot-1", ContextPolicyID: "context:default/v1",
		ToolRouterSnapshotID: "tool-router-1", EncodedRequestDigest: "sha256:request",
		OutputBudget: DefaultOutputBudget(),
	}
	response, err := Invoke(context.Background(), client, InvocationRequest{
		Binding: binding, Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil || response.Content != "ok" || !client.called || !reflect.DeepEqual(client.binding, binding) {
		t.Fatalf("Invoke binding 传播失败: response=%+v err=%v called=%v binding=%+v", response, err, client.called, client.binding)
	}
}

func TestInvokeRejectsUnboundRequestBeforeClient(t *testing.T) {
	client := &bindingCaptureClient{}
	_, err := Invoke(context.Background(), client, InvocationRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err == nil || client.called {
		t.Fatalf("未绑定请求必须在 provider 前拒绝: err=%v called=%v", err, client.called)
	}
	failure, ok := invocation.FromError(err)
	if !ok || failure.Kind != invocation.FailureContextAssembly {
		t.Fatalf("未绑定请求必须形成 canonical context assembly failure: %+v,%v", failure, ok)
	}
}
