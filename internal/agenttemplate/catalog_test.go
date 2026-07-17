package agenttemplate

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	t.Parallel()

	got, err := ParseRef("project/rust-migrator@12")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	want := Ref{Namespace: NamespaceProject, Name: "rust-migrator", Version: 12}
	if got != want || got.String() != "project/rust-migrator@12" {
		t.Fatalf("ParseRef = %#v (%q), want %#v", got, got.String(), want)
	}

	for _, value := range []string{
		"", " generalist@1", "builtin/generalist", "builtin/generalist@0",
		"builtin/Generalist@1", "company/generalist@1", "builtin/a/b@1",
		"builtin/generalist@1@2", "builtin/generalist@01", "builtin/generalist@+1",
	} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseRef(value); err == nil {
				t.Fatalf("ParseRef(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestLoadBuiltinsAndCatalogCopies(t *testing.T) {
	t.Parallel()

	validationCalls := 0
	catalog, err := Load(LoadOptions{DefaultModel: "test-model", ValidateTools: func(names []string) error {
		validationCalls++
		if len(names) == 0 {
			t.Fatal("validator received an empty tool list")
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if validationCalls != 3 {
		t.Fatalf("validator calls = %d, want 3", validationCalls)
	}

	summaries := catalog.List()
	gotRefs := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		gotRefs = append(gotRefs, summary.Ref)
		if !strings.HasPrefix(summary.Digest, "sha256:") || len(summary.Digest) != 71 {
			t.Errorf("summary %s has invalid digest %q", summary.Ref, summary.Digest)
		}
		if summary.MaxReplicas <= 0 {
			t.Errorf("summary %s has invalid MaxReplicas %d", summary.Ref, summary.MaxReplicas)
		}
	}
	wantRefs := []string{"builtin/explorer@1", "builtin/generalist@1", "builtin/verifier@1"}
	if !reflect.DeepEqual(gotRefs, wantRefs) {
		t.Fatalf("List refs = %v, want %v", gotRefs, wantRefs)
	}

	first, err := catalog.Resolve("builtin/generalist@1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.Contains(first.SystemPrompt, "Scheduler") || first.SourceFile != "embed:prompts/generalist.md" {
		t.Fatalf("builtin prompt/source were not resolved: %#v", first)
	}
	first.Tools[0] = "mutated"
	first.Capabilities[0] = "mutated"
	first.Description = "mutated"
	summaries[0].Capabilities[0] = "mutated"
	summaries[0].Tools[0] = "mutated"

	second, err := catalog.Resolve("builtin/generalist@1")
	if err != nil {
		t.Fatalf("Resolve after mutation: %v", err)
	}
	if second.Tools[0] == "mutated" || second.Capabilities[0] == "mutated" || second.Description == "mutated" {
		t.Fatal("Resolve leaked mutable catalog storage")
	}
	listedAgain := catalog.List()
	if listedAgain[0].Capabilities[0] == "mutated" || listedAgain[0].Tools[0] == "mutated" {
		t.Fatal("List leaked mutable catalog storage")
	}
}

func TestCatalogNilBehavior(t *testing.T) {
	t.Parallel()

	var catalog *Catalog
	if got := catalog.List(); got != nil {
		t.Fatalf("nil Catalog.List = %#v, want nil", got)
	}
	if _, err := catalog.Resolve("builtin/generalist@1"); err == nil {
		t.Fatal("nil Catalog.Resolve unexpectedly succeeded")
	}
}

func TestTemplateDigestCoversResolvedExecutionContract(t *testing.T) {
	t.Parallel()

	base := Template{
		Description: "description", Capabilities: []string{"a", "b"},
		Tools: []string{"read_file", "run_shell"}, Model: "model-a", SystemPrompt: "prompt",
		Limits: Limits{AgentMaxLoops: 1, TaskMaxRetries: 2, EnforceCompactTokenThreshold: 3, ContextLimit: 4, MaxReplicas: 5},
	}
	baseDigest, err := templateDigest(base)
	if err != nil {
		t.Fatalf("templateDigest(base): %v", err)
	}

	mutations := map[string]func(*Template){
		"description":       func(v *Template) { v.Description = "changed" },
		"capabilities":      func(v *Template) { v.Capabilities = []string{"changed"} },
		"tools":             func(v *Template) { v.Tools = []string{"read_file"} },
		"model":             func(v *Template) { v.Model = "model-b" },
		"prompt":            func(v *Template) { v.SystemPrompt = "changed" },
		"agent max loops":   func(v *Template) { v.AgentMaxLoops++ },
		"task retries":      func(v *Template) { v.TaskMaxRetries++ },
		"compact threshold": func(v *Template) { v.EnforceCompactTokenThreshold++ },
		"context limit":     func(v *Template) { v.ContextLimit++ },
		"maximum replicas":  func(v *Template) { v.MaxReplicas++ },
	}
	for name, mutate := range mutations {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			candidate := cloneTemplate(&base)
			mutate(&candidate)
			got, err := templateDigest(candidate)
			if err != nil {
				t.Fatalf("templateDigest: %v", err)
			}
			if got == baseDigest {
				t.Fatalf("digest did not change after %s mutation", name)
			}
		})
	}

	reordered := cloneTemplate(&base)
	reordered.Tools = []string{"run_shell", "read_file"}
	reordered.Capabilities = []string{"b", "a"}
	got, err := templateDigest(reordered)
	if err != nil {
		t.Fatalf("templateDigest(reordered): %v", err)
	}
	if got != baseDigest {
		t.Fatalf("set-like ordering changed digest: got %s want %s", got, baseDigest)
	}
}

type compileTimeProvisioner struct{}

func (compileTimeProvisioner) Provision(context.Context, ProvisionRequest) (ProvisionResult, error) {
	return ProvisionResult{}, nil
}

var _ Provisioner = compileTimeProvisioner{}
