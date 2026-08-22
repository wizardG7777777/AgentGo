package agenttemplate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadExternalTemplatesNamespacesPromptAndDefaults(t *testing.T) {
	t.Parallel()

	userDir := t.TempDir()
	projectDir := t.TempDir()
	writeTestFile(t, filepath.Join(userDir, "writer.yaml"), `
name: writer
version: 2
description: Project writer
capabilities: [editing, testing]
tools: [read_file, write_file]
model: custom-model
system_prompt: |
  Work carefully.
limits:
  max_replicas: 2
`)
	writeTestFile(t, filepath.Join(projectDir, "prompt.md"), "Prompt loaded from a file.\n")
	writeTestFile(t, filepath.Join(projectDir, "auditor.yml"), `
name: auditor
version: 1
description: Project auditor
capabilities: [audit]
tools: [read_file]
system_prompt_file: prompt.md
limits:
  task_max_retries: 1
  max_replicas: 3
`)

	validated := 0
	catalog, err := Load(LoadOptions{
		UserDirs: []string{userDir}, ProjectDirs: []string{projectDir},
		DefaultModel:  "default-model",
		ValidateTools: func([]string) error { validated++; return nil },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if validated != 5 {
		t.Fatalf("validator calls = %d, want 5", validated)
	}

	writer, err := catalog.Resolve("user/writer@2")
	if err != nil {
		t.Fatalf("Resolve writer: %v", err)
	}
	if writer.Namespace != NamespaceUser || writer.Model != "custom-model" || writer.MaxReplicas != 2 {
		t.Fatalf("writer metadata = %#v", writer)
	}
	if writer.TaskMaxRetries != defaultLimits.TaskMaxRetries {
		t.Fatalf("writer defaults not applied: %#v", writer.Limits)
	}

	auditor, err := catalog.Resolve("project/auditor@1")
	if err != nil {
		t.Fatalf("Resolve auditor: %v", err)
	}
	if auditor.SystemPrompt != "Prompt loaded from a file.\n" {
		t.Fatalf("resolved prompt = %q", auditor.SystemPrompt)
	}
	if auditor.TaskMaxRetries != 1 || !filepath.IsAbs(auditor.SourceFile) {
		t.Fatalf("auditor metadata = %#v", auditor)
	}

	refs := make([]string, 0)
	for _, item := range catalog.List() {
		refs = append(refs, item.Ref)
	}
	want := []string{
		"builtin/explorer@1", "builtin/generalist@1", "builtin/verifier@1",
		"project/auditor@1", "user/writer@2",
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("List refs = %v, want %v", refs, want)
	}
}

func TestLoadRequiresValidatorAndIgnoresMissingDirectories(t *testing.T) {
	t.Parallel()

	if _, err := Load(LoadOptions{}); err == nil || !strings.Contains(err.Error(), "validator") {
		t.Fatalf("Load without validator error = %v", err)
	}
	if _, err := Load(LoadOptions{ValidateTools: func([]string) error { return nil }}); err == nil || !strings.Contains(err.Error(), "default model") {
		t.Fatalf("Load without default model error = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "not-created")
	catalog, err := Load(LoadOptions{UserDirs: []string{missing}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
	if err != nil {
		t.Fatalf("Load missing default directory: %v", err)
	}
	if len(catalog.List()) != 3 {
		t.Fatalf("catalog size = %d, want built-ins only", len(catalog.List()))
	}
}

func TestLoadResolvesDefaultModelIntoDigest(t *testing.T) {
	t.Parallel()

	load := func(model string) *Template {
		t.Helper()
		catalog, err := Load(LoadOptions{DefaultModel: model, ValidateTools: func([]string) error { return nil }})
		if err != nil {
			t.Fatalf("Load(%q): %v", model, err)
		}
		resolved, err := catalog.Resolve("builtin/generalist@1")
		if err != nil {
			t.Fatalf("Resolve(%q): %v", model, err)
		}
		return resolved
	}
	first := load("model-a")
	second := load("model-b")
	if first.Model != "model-a" || second.Model != "model-b" {
		t.Fatalf("resolved models = %q, %q", first.Model, second.Model)
	}
	if first.Digest == second.Digest {
		t.Fatal("resolved default model did not affect digest")
	}
}

func TestLoadStrictYAMLAndSingleDocument(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		content string
		match   string
	}{
		{
			name:    "unknown field",
			content: validExternalYAML("strict") + "namespace: builtin\n",
			match:   "field namespace not found",
		},
		{
			name:    "second document",
			content: validExternalYAML("strict") + "---\nname: another\n",
			match:   "exactly one YAML document",
		},
		{
			name: "unknown limits field",
			content: `name: strict
version: 1
description: Strict template
tools: [read_file]
system_prompt: test
limits:
  imaginary_limit: 1
`,
			match: "field imaginary_limit not found",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, "template.yaml"), tc.content)
			_, err := Load(LoadOptions{ProjectDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("Load error = %v, want substring %q", err, tc.match)
			}
		})
	}
}

func TestLoadPromptSelection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		extra string
	}{
		{name: "neither", extra: ""},
		{name: "both", extra: "system_prompt: inline\nsystem_prompt_file: prompt.md\n"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, "prompt.md"), "file prompt")
			content := `name: prompt-test
version: 1
description: Prompt test
tools: [read_file]
` + tc.extra
			writeTestFile(t, filepath.Join(dir, "template.yaml"), content)
			_, err := Load(LoadOptions{ProjectDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("Load error = %v, want prompt selection failure", err)
			}
		})
	}
}

func TestLoadRejectsDuplicateUnknownAndControlTools(t *testing.T) {
	t.Parallel()

	validatorErr := errors.New("unknown runtime tool")
	for _, tc := range []struct {
		name      string
		tools     string
		validator func([]string) error
		match     string
	}{
		{name: "empty", tools: "[]", validator: func([]string) error { return nil }, match: "at least one"},
		{name: "duplicate", tools: "[read_file, read_file]", validator: func([]string) error { return nil }, match: "duplicate"},
		{name: "publish", tools: "[publish_task]", validator: func([]string) error { return nil }, match: "reserved"},
		{name: "scheduler control", tools: "[report_done]", validator: func([]string) error { return nil }, match: "reserved"},
		{name: "team provisioning", tools: "[provision_agent_team]", validator: func([]string) error { return nil }, match: "reserved"},
		{name: "scheduler result read", tools: "[get_task_result]", validator: func([]string) error { return nil }, match: "reserved"},
		{name: "graph submit", tools: "[submit_graph]", validator: func([]string) error { return nil }, match: "reserved"},
		{name: "graph read", tools: "[read_graph]", validator: func([]string) error { return nil }, match: "reserved"},
		{name: "graph patch", tools: "[patch_graph]", validator: func([]string) error { return nil }, match: "reserved"},
		{name: "runtime unknown", tools: "[not_registered]", validator: func([]string) error { return validatorErr }, match: validatorErr.Error()},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			content := fmt.Sprintf(`name: unsafe
version: 1
description: Unsafe template
tools: %s
system_prompt: test
`, tc.tools)
			writeTestFile(t, filepath.Join(dir, "unsafe.yaml"), content)
			_, err := Load(LoadOptions{UserDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: tc.validator})
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("Load error = %v, want substring %q", err, tc.match)
			}
		})
	}
}

func TestLoadAllowsOrdinaryWorkerControlTools(t *testing.T) {
	t.Parallel()

	// request_replan / submit_task_result 是普通工作代理可持有的控制面工具，
	// 不在模板加载器的保留清单内。正式 acceptance verifier 另由 Graph
	// 提交校验与 ExecutionLease 正向闭集拒绝 request_replan。
	dir := t.TempDir()
	content := `name: custom-worker
version: 1
description: Custom worker
tools: [read_file, request_replan, submit_task_result]
system_prompt: work
`
	writeTestFile(t, filepath.Join(dir, "worker.yaml"), content)
	if _, err := Load(LoadOptions{ProjectDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }}); err != nil {
		t.Fatalf("Load worker template: %v", err)
	}
}

func TestLoadRejectsDuplicateRef(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "one.yaml"), validExternalYAML("duplicate"))
	writeTestFile(t, filepath.Join(dir, "two.yml"), validExternalYAML("duplicate"))
	_, err := Load(LoadOptions{UserDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "duplicate agent template ref") {
		t.Fatalf("Load error = %v, want duplicate ref", err)
	}
}

func TestLoadEnforcesPromptSymlinkAndTraversalBoundary(t *testing.T) {
	t.Parallel()

	t.Run("traversal", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dir := filepath.Join(parent, "templates")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(parent, "outside.md"), "outside")
		writeTestFile(t, filepath.Join(dir, "template.yaml"), externalWithPromptFile("../outside.md"))
		_, err := Load(LoadOptions{ProjectDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
		if err == nil || !strings.Contains(err.Error(), "contains '..'") {
			t.Fatalf("Load traversal error = %v", err)
		}
	})

	t.Run("normalized traversal is still rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, "prompt.md"), "inside")
		writeTestFile(t, filepath.Join(dir, "template.yaml"), externalWithPromptFile("nested/../prompt.md"))
		_, err := Load(LoadOptions{ProjectDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
		if err == nil || !strings.Contains(err.Error(), "contains '..'") {
			t.Fatalf("Load normalized traversal error = %v", err)
		}
	})

	t.Run("symlink escape", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dir := filepath.Join(parent, "templates")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(parent, "outside.md")
		writeTestFile(t, outside, "outside")
		if err := os.Symlink(outside, filepath.Join(dir, "prompt.md")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		writeTestFile(t, filepath.Join(dir, "template.yaml"), externalWithPromptFile("prompt.md"))
		_, err := Load(LoadOptions{ProjectDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("Load symlink escape error = %v", err)
		}
	})

	t.Run("inside symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "real.md")
		writeTestFile(t, target, "inside")
		if err := os.Symlink(target, filepath.Join(dir, "prompt.md")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		writeTestFile(t, filepath.Join(dir, "template.yaml"), externalWithPromptFile("prompt.md"))
		catalog, err := Load(LoadOptions{ProjectDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
		if err != nil {
			t.Fatalf("Load inside symlink: %v", err)
		}
		resolved, err := catalog.Resolve("project/prompt-file@1")
		if err != nil || resolved.SystemPrompt != "inside" {
			t.Fatalf("Resolve = %#v, %v", resolved, err)
		}
	})
}

func TestLoadEnforcesTemplateFileSymlinkBoundary(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	dir := filepath.Join(parent, "templates")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.yaml")
	writeTestFile(t, outside, validExternalYAML("outside"))
	if err := os.Symlink(outside, filepath.Join(dir, "leak.yaml")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := Load(LoadOptions{UserDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("Load template symlink escape error = %v", err)
	}
}

func TestLoadRejectsInvalidLimits(t *testing.T) {
	t.Parallel()

	for _, value := range []int{-1, 0} {
		value := value
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, "invalid.yaml"), fmt.Sprintf(`
name: invalid-limits
version: 1
description: Invalid limits
tools: [read_file]
system_prompt: test
limits:
  max_replicas: %d
`, value))
			_, err := Load(LoadOptions{UserDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
			if err == nil || !strings.Contains(err.Error(), "max_replicas") {
				t.Fatalf("Load limits error = %v", err)
			}
		})
	}
}

func TestLoadRejectsRemovedAgentMaxLoops(t *testing.T) {
	t.Parallel()

	// limits.agent_max_loops 已于 V6 移除：显式设置必须报迁移诊断，不静默忽略。
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "legacy.yaml"), `
name: legacy-loops
version: 1
description: Legacy loops template
tools: [read_file]
system_prompt: test
limits:
  agent_max_loops: 7
`)
	_, err := Load(LoadOptions{UserDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "agent_max_loops") || !strings.Contains(err.Error(), "removed in V6") {
		t.Fatalf("Load legacy agent_max_loops error = %v, want V6 migration diagnostic", err)
	}
}

func TestLoadRejectsRemovedContextLimit(t *testing.T) {
	t.Parallel()

	// limits.context_limit 已于 V6 移除：显式设置必须报迁移诊断，不静默忽略。
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "legacy.yaml"), `
name: legacy-context
version: 1
description: Legacy context template
tools: [read_file]
system_prompt: test
limits:
  context_limit: 9000
`)
	_, err := Load(LoadOptions{UserDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "context_limit") || !strings.Contains(err.Error(), "removed in V6") {
		t.Fatalf("Load legacy context_limit error = %v, want V6 migration diagnostic", err)
	}
}

func TestLoadRejectsRemovedCompactTokenThreshold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "legacy.yaml"), `
name: legacy-compact
version: 1
description: Legacy compact template
tools: [read_file]
system_prompt: test
limits:
  enforce_compact_token_threshold: 4000
`)
	_, err := Load(LoadOptions{UserDirs: []string{dir}, DefaultModel: "test-model", ValidateTools: func([]string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "enforce_compact_token_threshold") || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("legacy compact threshold 应返回迁移诊断: %v", err)
	}
}

func validExternalYAML(name string) string {
	return fmt.Sprintf(`name: %s
version: 1
description: Test template
capabilities: [test]
tools: [read_file]
system_prompt: test prompt
`, name)
}

func externalWithPromptFile(path string) string {
	return fmt.Sprintf(`name: prompt-file
version: 1
description: Prompt file template
tools: [read_file]
system_prompt_file: %s
`, path)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
