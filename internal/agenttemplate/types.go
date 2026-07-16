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
type Limits struct {
	AgentMaxLoops                int `yaml:"agent_max_loops" json:"agent_max_loops"`
	TaskMaxRetries               int `yaml:"task_max_retries" json:"task_max_retries"`
	EnforceCompactTokenThreshold int `yaml:"enforce_compact_token_threshold" json:"enforce_compact_token_threshold"`
	ContextLimit                 int `yaml:"context_limit" json:"context_limit"`
	MaxReplicas                  int `yaml:"max_replicas" json:"max_replicas"`
}

// Template is a fully resolved, validated and immutable-by-contract template.
// SystemPrompt always contains prompt text (never a path).  Ref and Digest are
// derived by the loader and cannot be supplied by external YAML.
//
// Limits is embedded so runtime code can use either t.Limits or the convenient
// t.AgentMaxLoops style without duplicating values.
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
// The controller identity is carried explicitly so the implementation can
// enforce current-plan authority before mutating runtime state.
type ProvisionRequest struct {
	PlanID           string
	ControllerTaskID string
	TemplateRef      string
	Purpose          string
	Replicas         int
}

// ProvisionResult is the stable identity returned to Scheduler after the
// runtime has installed routes and made the team ready for task publication.
type ProvisionResult struct {
	TeamID         string   `json:"team_id"`
	EventType      string   `json:"event_type"`
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
