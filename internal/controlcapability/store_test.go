package controlcapability

import (
	"testing"

	"agentgo/internal/llm"
)

func TestStorePersistsAndScopesDeterministicIncompatibility(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := Key{RunID: "run-1", EffectiveModel: "m1", InvocationProfile: "v8", ToolSchemaDigest: "schema-a"}
	failure := llm.NewFailure(llm.FailureInvalidRequest, llm.PhaseResponseValidate, llm.OriginProvider, nil)
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
	for _, kind := range []llm.FailureKind{llm.FailureRateLimited, llm.FailureProviderUnavailable, llm.FailureRequestTimeout} {
		created, err := store.Mark(key, llm.NewFailure(kind, llm.PhaseRequestSend, llm.OriginProvider, nil))
		if err != nil || created {
			t.Fatalf("瞬时错误不应熔断 kind=%s created=%t err=%v", kind, created, err)
		}
	}
}
