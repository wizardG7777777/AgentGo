package invocation

import (
	"context"
	"errors"
	"testing"
)

type testCarrier struct {
	failure *Failure
}

func (e *testCarrier) Error() string { return "兼容包装" }
func (e *testCarrier) InvocationFailure() *Failure {
	return e.failure
}

func TestFailureValidateAndUnwrap(t *testing.T) {
	failure := NewFailure(FailureRequestTimeout, PhaseRequestSend, OriginTransport,
		context.DeadlineExceeded)
	failure.TimeoutScope = TimeoutInvocation
	if err := failure.Validate(); err != nil {
		t.Fatalf("合法 InvocationFailure 校验失败: %v", err)
	}
	if !errors.Is(failure, context.DeadlineExceeded) {
		t.Fatal("InvocationFailure 必须保留 Cause unwrap 链")
	}
}

func TestFromErrorFindsCompatibilityCarrier(t *testing.T) {
	want := NewFailure(FailureContextWindowExceeded, PhaseResponseHeaders,
		OriginProvider, errors.New("provider context code"))
	got, ok := FromError(&testCarrier{failure: want})
	if !ok || got != want {
		t.Fatalf("FromError = (%p,%v)，want (%p,true)", got, ok, want)
	}
	if !IsContextWindowExceeded(&testCarrier{failure: want}) {
		t.Fatal("typed context-window failure 未被识别")
	}
}

func TestFailureValidateRejectsUnknownKind(t *testing.T) {
	failure := NewFailure(FailureKind("future_kind"), PhaseResponseValidate,
		OriginProtocol, nil)
	if err := failure.Validate(); err == nil {
		t.Fatal("未知 FailureKind 应 fail-closed")
	}
}
