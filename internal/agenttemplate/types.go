// Package agenttemplate owns immutable AgentTemplate definitions and the
// narrow provisioning contract used by the Scheduler.  It deliberately does
// not depend on the tool or runtime packages: callers inject tool validation,
// and runtime provisioning is expressed through Provisioner.
package agenttemplate

import "context"

const (
	NamespaceBuiltin = "builtin"
	NamespaceUser    = "user"
	NamespaceProject = "project"
)

// Limits bounds one provisioned agent and the number of homogeneous replicas
// that may be requested from the template.
// V6 起不含循环上限与上下文硬限（两者均已删除，见 docs/nextUpgrade-V6.md §5/§7.4）。
type Limits struct {
	TaskMaxRetries               int `yaml:"task_max_retries" json:"task_max_retries"`
	EnforceCompactTokenThreshold int `yaml:"enforce_compact_token_threshold" json:"enforce_compact_token_threshold"`
	MaxReplicas                  int `yaml:"max_replicas" json:"max_replicas"`
}

// Template is a fully resolved, validated and immutable-by-contract template.
// SystemPrompt always contains prompt text (never a path).  Ref and Digest are
// derived by the loader and cannot be supplied by external YAML.
//
// Limits is embedded so runtime code can use either t.Limits or the convenient
// t.TaskMaxRetries style without duplicating values.
type Template struct {
	Ref          string   `json:"ref"`
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Version      int      `json:"version"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Tools        []string `json:"tools"`
	Model        string   `json:"model,omitempty"`
	SystemPrompt string   `json:"system_prompt"`
	Limits
	Digest     string `json:"digest"`
	SourceFile string `json:"source_file"`
}

// Summary is the bounded metadata exposed to Scheduler template discovery.
// It excludes the full system prompt but includes the real tool allowlist so
// Scheduler can distinguish authorization from descriptive capability labels.
type Summary struct {
	Ref          string   `json:"ref"`
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	Version      int      `json:"version"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Tools        []string `json:"tools"`
	Model        string   `json:"model,omitempty"`
	MaxReplicas  int      `json:"max_replicas"`
	Digest       string   `json:"digest"`
}

// ProvisionRequest asks the runtime to create or reuse one homogeneous team.
// ControllerTaskID is always retained as provisioning provenance. GraphID,
// when non-empty, moves runtime ownership to that durable Graph; otherwise the
// Team remains a legacy controller-task-scoped resource.
type ProvisionRequest struct {
	ControllerTaskID string
	GraphID          string
	TemplateRef      string
	Purpose          string
	Replicas         int
}

// ProvisionResult is the stable identity returned to Scheduler after the
// runtime has installed routes and made the team ready for task publication.
type ProvisionResult struct {
	TeamID         string   `json:"team_id"`
	EventType      string   `json:"event_type"`
	GraphID        string   `json:"graph_id,omitempty"`
	TemplateRef    string   `json:"template_ref"`
	TemplateDigest string   `json:"template_digest"`
	AgentIDs       []string `json:"agent_ids"`
	Tools          []string `json:"tools"`
	Replicas       int      `json:"replicas"`
	Reused         bool     `json:"reused"`
}

// Provisioner is the only runtime mutation capability exposed by this
// package. Implementations must make provisioning atomic from the caller's
// perspective: success means the returned EventType is ready to consume work.
type Provisioner interface {
	Provision(context.Context, ProvisionRequest) (ProvisionResult, error)
}
