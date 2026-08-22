package agenttemplate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadOptions defines the two external trust domains. A template's namespace
// is derived exclusively from the list in which its directory appears; YAML
// cannot claim or override a namespace.
type LoadOptions struct {
	UserDirs    []string
	ProjectDirs []string
	// DefaultModel is resolved into every template that omits model before the
	// digest is computed. This prevents a persisted ref+digest from silently
	// changing behavior when the process-wide default changes later.
	DefaultModel string

	// ValidateTools must validate every supplied tool name against the runtime's
	// actual registry. It is injected to keep this package below internal/tools
	// in the dependency graph.
	ValidateTools func([]string) error
}

type unresolvedTemplate struct {
	Name             string           `yaml:"name"`
	Version          int              `yaml:"version"`
	Description      string           `yaml:"description"`
	Capabilities     []string         `yaml:"capabilities"`
	Tools            []string         `yaml:"tools"`
	Model            string           `yaml:"model,omitempty"`
	SystemPrompt     string           `yaml:"system_prompt,omitempty"`
	SystemPromptFile string           `yaml:"system_prompt_file,omitempty"`
	Limits           unresolvedLimits `yaml:"limits,omitempty"`
	SourceFile       string           `yaml:"-"`
}

// unresolvedLimits uses pointers so omitted values can receive defaults while
// an explicitly configured zero remains an error instead of being mistaken
// for omission.
//
// AgentMaxLoops 与 ContextLimit 已于 V6 移除：保留解析位以便对显式设置这些
// 字段的旧模板给出明确的迁移诊断错误（与 config 层 llm.provider /
// agent_max_loops 同款模式），而不是静默忽略。
type unresolvedLimits struct {
	AgentMaxLoops                *int `yaml:"agent_max_loops"`
	TaskMaxRetries               *int `yaml:"task_max_retries"`
	EnforceCompactTokenThreshold *int `yaml:"enforce_compact_token_threshold"`
	ContextLimit                 *int `yaml:"context_limit"`
	MaxReplicas                  *int `yaml:"max_replicas"`
}

var defaultLimits = Limits{
	TaskMaxRetries: 3,
	MaxReplicas:    4,
}

// Tools that can mutate or finalize the DAG/control plane are never grantable
// through an AgentTemplate. request_replan is intentionally absent: workers
// may request a decision but cannot perform it. submit_task_result is
// likewise allowed because formal verifier templates require it.
var forbiddenTemplateTools = map[string]struct{}{
	"publish_task":         {},
	"cancel_task":          {},
	"get_task_result":      {},
	"report_done":          {},
	"report_progress":      {},
	"probe_directory":      {},
	"list_agent_templates": {},
	"provision_agent_team": {},
	"submit_graph":         {},
	"read_graph":           {},
	"patch_graph":          {},
}

// Load builds a new immutable catalog. Built-ins are always present. Missing
// external directories are treated as empty, which permits default user and
// project search paths to be configured before users create them.
func Load(opts LoadOptions) (*Catalog, error) {
	if opts.ValidateTools == nil {
		return nil, fmt.Errorf("agent template tool validator is required")
	}
	defaultModel := strings.TrimSpace(opts.DefaultModel)
	if defaultModel == "" {
		return nil, fmt.Errorf("agent template default model is required")
	}
	catalog := &Catalog{byRef: make(map[string]*Template)}
	for _, raw := range builtinDefinitions() {
		if err := catalog.resolveAndAdd(NamespaceBuiltin, raw, defaultModel, opts.ValidateTools); err != nil {
			return nil, fmt.Errorf("load built-in agent template %q: %w", raw.Name, err)
		}
	}
	if err := catalog.loadDirs(NamespaceUser, opts.UserDirs, defaultModel, opts.ValidateTools); err != nil {
		return nil, err
	}
	if err := catalog.loadDirs(NamespaceProject, opts.ProjectDirs, defaultModel, opts.ValidateTools); err != nil {
		return nil, err
	}
	return catalog, nil
}

func (c *Catalog) loadDirs(namespace string, dirs []string, defaultModel string, validateTools func([]string) error) error {
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			return fmt.Errorf("load %s agent templates: directory path is empty", namespace)
		}
		root, err := canonicalDirectory(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("load %s agent templates from %q: %w", namespace, dir, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return fmt.Errorf("read agent template directory %q: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !isYAMLFile(entry.Name()) {
				continue
			}
			candidate := filepath.Join(root, entry.Name())
			resolved, err := resolveFileWithinRoot(root, candidate)
			if err != nil {
				return fmt.Errorf("resolve agent template file %q: %w", candidate, err)
			}
			raw, err := decodeTemplateFile(resolved)
			if err != nil {
				return fmt.Errorf("decode agent template file %q: %w", resolved, err)
			}
			raw.SourceFile = resolved
			hasInlinePrompt := strings.TrimSpace(raw.SystemPrompt) != ""
			hasPromptFile := strings.TrimSpace(raw.SystemPromptFile) != ""
			if hasInlinePrompt == hasPromptFile {
				return fmt.Errorf("load agent template file %q: exactly one of system_prompt and system_prompt_file is required", resolved)
			}
			if raw.SystemPromptFile != "" {
				promptPath, err := resolveRelativeFile(root, raw.SystemPromptFile)
				if err != nil {
					return fmt.Errorf("resolve system_prompt_file for %q: %w", resolved, err)
				}
				prompt, err := os.ReadFile(promptPath)
				if err != nil {
					return fmt.Errorf("read system_prompt_file for %q: %w", resolved, err)
				}
				raw.SystemPrompt = string(prompt)
				raw.SystemPromptFile = ""
			}
			if err := c.resolveAndAdd(namespace, raw, defaultModel, validateTools); err != nil {
				return fmt.Errorf("load agent template file %q: %w", resolved, err)
			}
		}
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory")
	}
	return filepath.Clean(resolved), nil
}

func isYAMLFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

func decodeTemplateFile(path string) (unresolvedTemplate, error) {
	f, err := os.Open(path)
	if err != nil {
		return unresolvedTemplate{}, err
	}
	defer f.Close()

	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	var raw unresolvedTemplate
	if err := decoder.Decode(&raw); err != nil {
		return unresolvedTemplate{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return unresolvedTemplate{}, fmt.Errorf("each file must contain exactly one YAML document")
		}
		return unresolvedTemplate{}, fmt.Errorf("decode trailing YAML document: %w", err)
	}
	return raw, nil
}

func resolveRelativeFile(root, relative string) (string, error) {
	if relative != strings.TrimSpace(relative) || relative == "" {
		return "", fmt.Errorf("path must not be empty or padded with whitespace")
	}
	if strings.Contains(relative, `\`) {
		return "", fmt.Errorf("path %q contains a backslash; use forward slashes", relative)
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("path %q must be relative to the template directory", relative)
	}
	for _, segment := range strings.Split(relative, "/") {
		if segment == ".." {
			return "", fmt.Errorf("path %q contains '..' and is not allowed", relative)
		}
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the template directory", relative)
	}
	return resolveFileWithinRoot(root, filepath.Join(root, clean))
}

func resolveFileWithinRoot(root, candidate string) (string, error) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("resolved path %q escapes template directory %q", resolved, root)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("resolved path %q is not a regular file", resolved)
	}
	return filepath.Clean(resolved), nil
}

func (c *Catalog) resolveAndAdd(namespace string, raw unresolvedTemplate, defaultModel string, validateTools func([]string) error) error {
	name := strings.TrimSpace(raw.Name)
	if name != raw.Name || !templateNamePattern.MatchString(name) {
		return fmt.Errorf("invalid name %q: use lowercase letters, digits, '_' or '-' and begin with a letter", raw.Name)
	}
	if raw.Version <= 0 {
		return fmt.Errorf("version must be a positive integer")
	}
	description := strings.TrimSpace(raw.Description)
	if description == "" {
		return fmt.Errorf("description must not be empty")
	}
	if strings.TrimSpace(raw.SystemPrompt) == "" {
		return fmt.Errorf("system_prompt or a readable system_prompt_file is required")
	}
	tools, err := normalizeUniqueList("tools", raw.Tools, true)
	if err != nil {
		return err
	}
	if err := validateTools(append([]string(nil), tools...)); err != nil {
		return fmt.Errorf("validate tools: %w", err)
	}
	for _, tool := range tools {
		if _, forbidden := forbiddenTemplateTools[tool]; forbidden {
			return fmt.Errorf("tool %q is reserved for Scheduler/control-plane use and cannot be granted by an agent template", tool)
		}
	}
	capabilities, err := normalizeUniqueList("capabilities", raw.Capabilities, false)
	if err != nil {
		return err
	}
	limits, err := resolveLimits(raw.Limits)
	if err != nil {
		return err
	}
	ref := Ref{Namespace: namespace, Name: name, Version: raw.Version}.String()
	if _, exists := c.byRef[ref]; exists {
		return fmt.Errorf("duplicate agent template ref %q", ref)
	}
	model := strings.TrimSpace(raw.Model)
	if model == "" {
		model = defaultModel
	}
	if model == "" {
		return fmt.Errorf("model is empty and no default model is available")
	}
	t := Template{
		Ref:          ref,
		Namespace:    namespace,
		Name:         name,
		Version:      raw.Version,
		Description:  description,
		Capabilities: capabilities,
		Tools:        tools,
		Model:        model,
		SystemPrompt: raw.SystemPrompt,
		Limits:       limits,
		SourceFile:   raw.SourceFile,
	}
	t.Digest, err = templateDigest(t)
	if err != nil {
		return fmt.Errorf("compute digest: %w", err)
	}
	stored := cloneTemplate(&t)
	c.byRef[ref] = &stored
	return nil
}

func normalizeUniqueList(field string, values []string, required bool) ([]string, error) {
	if required && len(values) == 0 {
		return nil, fmt.Errorf("%s must contain at least one entry", field)
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		clean := strings.TrimSpace(value)
		if clean == "" {
			return nil, fmt.Errorf("%s[%d] must not be empty", field, i)
		}
		if _, exists := seen[clean]; exists {
			return nil, fmt.Errorf("%s contains duplicate entry %q", field, clean)
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out, nil
}

func resolveLimits(in unresolvedLimits) (Limits, error) {
	// limits.agent_max_loops 已于 V6 移除（固定循环上限不再是终止条件，见
	// docs/nextUpgrade-V6.md §5）：显式设置即迁移诊断，不静默忽略。
	if in.AgentMaxLoops != nil {
		return Limits{}, fmt.Errorf("limits.agent_max_loops was removed in V6: loops are bounded by structured terminal states, cancellation, deadlines and budgets, not a fixed round cap; delete this field from the template")
	}
	// limits.context_limit 已于 V6 移除（固定上下文硬限截断层已删除，见
	// docs/nextUpgrade-V6.md §7.4）：显式设置即迁移诊断，不静默忽略。
	if in.ContextLimit != nil {
		return Limits{}, fmt.Errorf("limits.context_limit was removed in V6: context fitting is handled by history compaction and overflow retry, not a fixed hard cap; delete this field from the template")
	}
	if in.EnforceCompactTokenThreshold != nil {
		return Limits{}, fmt.Errorf("limits.enforce_compact_token_threshold was removed: Context v3 derives replay projection from current Snapshot pressure and preserves Raw History; delete this field from the template")
	}
	out := defaultLimits
	values := []struct {
		name     string
		provided *int
		value    *int
	}{
		{"limits.task_max_retries", in.TaskMaxRetries, &out.TaskMaxRetries},
		{"limits.max_replicas", in.MaxReplicas, &out.MaxReplicas},
	}
	for _, item := range values {
		if item.provided == nil {
			continue
		}
		if *item.provided <= 0 {
			return Limits{}, fmt.Errorf("%s must be greater than zero", item.name)
		}
		*item.value = *item.provided
	}
	return out, nil
}

func fixedLimits(in Limits) unresolvedLimits {
	return unresolvedLimits{
		TaskMaxRetries: intPtr(in.TaskMaxRetries),
		MaxReplicas:    intPtr(in.MaxReplicas),
	}
}

func intPtr(value int) *int { return &value }

func templateDigest(t Template) (string, error) {
	tools := append([]string(nil), t.Tools...)
	capabilities := append([]string(nil), t.Capabilities...)
	sort.Strings(tools)
	sort.Strings(capabilities)
	canonical := struct {
		Description  string   `json:"description"`
		Capabilities []string `json:"capabilities"`
		Tools        []string `json:"tools"`
		Model        string   `json:"model"`
		SystemPrompt string   `json:"system_prompt"`
		Limits       Limits   `json:"limits"`
	}{
		Description:  t.Description,
		Capabilities: capabilities,
		Tools:        tools,
		Model:        t.Model,
		SystemPrompt: t.SystemPrompt,
		Limits:       t.Limits,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
