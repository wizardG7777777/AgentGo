package loopcontrol

import (
	"testing"

	"agentgo/internal/llm"
)

func TestDecideInvocationFailureSeparatesQuotaAndUnknown(t *testing.T) {
	quota := llm.NewFailure(llm.FailureProviderQuotaExhausted,
		llm.PhaseResponseHeaders, llm.OriginProvider, nil)
	if got := DecideInvocationFailure(quota); got.Action != RecoveryBlock ||
		got.FailureKind != llm.FailureProviderQuotaExhausted {
		t.Fatalf("provider quota 必须等待外部资源，不得 retry/recovery: %+v", got)
	}
	unknown := llm.NewFailure(llm.FailureUnknown,
		llm.PhaseResponseHeaders, llm.OriginProvider, nil)
	if got := DecideInvocationFailure(unknown); got.Action != RecoveryRequestIntervene {
		t.Fatalf("unknown 必须交 L5 裁决，不得静默 fail: %+v", got)
	}
}
