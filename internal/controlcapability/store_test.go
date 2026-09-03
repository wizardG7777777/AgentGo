package controlcapability

import (
	"testing"

	"agentgo/internal/invocation"
)

func TestStorePersistsAndScopesDeterministicIncompatibility(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := Key{RunID: "run-1", EffectiveModel: "m1", InvocationProfile: "v8", ToolSchemaDigest: "schema-a"}
	failure := invocation.NewFailure(invocation.FailureInvalidRequest, invocation.PhaseResponseValidate, invocation.OriginProvider, nil)
	if created, err := store.Mark(key, failure); err != nil || !created {
		t.Fatalf("Mark=%t err=%v", created, err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Incompatible(key); !ok {
		t.Fatal("重启后未恢复 incompatible")
	}
	for _, other := range []Key{
		{RunID: "run-2", EffectiveModel: "m1", InvocationProfile: "v8", ToolSchemaDigest: "schema-a"},
		{RunID: "run-1", EffectiveModel: "m2", InvocationProfile: "v8", ToolSchemaDigest: "schema-a"},
		{RunID: "run-1", EffectiveModel: "m1", InvocationProfile: "v7", ToolSchemaDigest: "schema-a"},
		{RunID: "run-1", EffectiveModel: "m1", InvocationProfile: "v8", ToolSchemaDigest: "schema-b"},
	} {
		if _, ok := reopened.Incompatible(other); ok {
			t.Fatalf("scope 污染: %+v", other)
		}
	}
}

func TestStoreIgnoresTransientFailure(t *testing.T) {
	store, _ := Open(t.TempDir())
	key := Key{RunID: "run", EffectiveModel: "m", InvocationProfile: "v8", ToolSchemaDigest: "s"}
	for _, kind := range []invocation.FailureKind{invocation.FailureRateLimited, invocation.FailureProviderUnavailable, invocation.FailureRequestTimeout} {
		created, err := store.Mark(key, invocation.NewFailure(kind, invocation.PhaseRequestSend, invocation.OriginProvider, nil))
		if err != nil || created {
			t.Fatalf("瞬时错误不应熔断 kind=%s created=%t err=%v", kind, created, err)
		}
	}
}
