package loopcontrol

import (
	"testing"

	"agentgo/internal/invocation"
)

func TestDecideInvocationFailureSeparatesQuotaAndUnknown(t *testing.T) {
	quota := invocation.NewFailure(invocation.FailureProviderQuotaExhausted,
		invocation.PhaseResponseHeaders, invocation.OriginProvider, nil)
	if got := DecideInvocationFailure(quota); got.Action != RecoveryBlock ||
		got.FailureKind != invocation.FailureProviderQuotaExhausted {
		t.Fatalf("provider quota 必须等待外部资源，不得 retry/recovery: %+v", got)
	}
	unknown := invocation.NewFailure(invocation.FailureUnknown,
		invocation.PhaseResponseHeaders, invocation.OriginProvider, nil)
	if got := DecideInvocationFailure(unknown); got.Action != RecoveryRequestIntervene {
		t.Fatalf("unknown 必须交 L5 裁决，不得静默 fail: %+v", got)
	}
}
