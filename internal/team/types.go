// Package team owns the durable identity and process-local lifecycle of
// homogeneous agent teams provisioned from AgentTemplates.
package team

import (
	"errors"
	"time"

	"agentgo/internal/llm"
)

const (
	// DefaultMaxInstances is deliberately small. Config may lower or raise it,
	// but the manager always applies one process-wide bound to recovered and
	// newly provisioned template agents together.
	DefaultMaxInstances = 8
	HardMaxInstances    = 32
)

// Status is the durable lifecycle state of a TeamSpec. Runtime cancellation
// during a normal process shutdown does not change ready: a subsequent process
// must recover it. stopped is reserved for Teams whose lifecycle owner reached
// a terminal state or disappeared from its authoritative store.
type Status string

const (
	StatusReady   Status = "ready"
	StatusStopped Status = "stopped"
)

var (
	ErrNotStarted              = errors.New("team manager has not started")
	ErrManagerClosed           = errors.New("team manager is closed")
	ErrProcessLimitExceeded    = errors.New("template agent process limit exceeded")
	ErrTemplateDigestMismatch  = errors.New("agent template digest does not match durable team spec")
	ErrTeamNotFound            = errors.New("team not found")
	ErrLegacyMigrationRequired = errors.New("team store v1 graph ownership migration is required")
	ErrLegacyGraphAmbiguous    = errors.New("team store v1 graph ownership is ambiguous")
)

// TeamSpec is the durable recovery record for one homogeneous team. EventType
// is a private route (team:<uuid>) and remains stable across process restarts.
// Agent instance IDs are derived deterministically from this record and do not
// need to be persisted separately.
type TeamSpec struct {
	ID               string    `json:"id"`
	TemplateRef      string    `json:"template_ref"`
	TemplateDigest   string    `json:"template_digest"`
	ControllerTaskID string    `json:"controller_task_id"`
	GraphID          string    `json:"graph_id,omitempty"`
	Purpose          string    `json:"purpose"`
	EventType        string    `json:"event_type"`
	Replicas         int       `json:"replicas"`
	Status           Status    `json:"status"`
	StopReason       string    `json:"stop_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// TeamStore is the minimum durable storage contract required by Manager.
// Ensure is atomic and returns created=false for the lifecycle-owner identity
// plus template ref, purpose and replicas. GraphID is the owner when non-empty;
// otherwise ControllerTaskID is the legacy owner.
type TeamStore interface {
	Ensure(spec TeamSpec) (stored TeamSpec, created bool, err error)
	Get(teamID string) (TeamSpec, error)
	List() ([]TeamSpec, error)
	SetStatus(teamID string, status Status, reason string) (TeamSpec, error)
	StopController(controllerTaskID, reason string) ([]TeamSpec, error)
	StopGraph(graphID, reason string) ([]TeamSpec, error)
}

// GraphStateResolver is the minimum durable Graph authority needed during Team
// recovery and graph_ended cleanup. terminal is authoritative only when
// exists=true.
type GraphStateResolver func(graphID string) (status string, terminal, exists bool)

// GraphBindingResolver returns all durable Graphs whose current definition or
// frozen execution definition explicitly references eventType. Terminal Graphs
// are included: a uniquely referenced Team must remain Graph-owned so Manager
// can durably stop it instead of reviving it as a legacy live route. V1
// migration accepts exactly zero (legacy task ownership) or one Graph; more
// than one candidate is ambiguous and must fail closed.
type GraphBindingResolver func(eventType string) (graphIDs []string, err error)

// RouteRegistry publishes/removes runtime-only Scheduler routing facts. key is
// the stable TeamSpec.EventType so registration can be rolled back exactly.
// ownerScope is an opaque, namespaced lifecycle scope (task:<id> or
// graph:<id>), never a raw task/Graph ID. Concrete claimant IDs are bound
// before runners enter their claim loop so EventType equality alone never
// grants cross-scope claim authority.
type RouteRegistry interface {
	RegisterRoute(key, eventType, ownerScope string, count int, role string, capabilities []string) error
	BindRouteClaimants(key string, agentIDs []string) error
	UnregisterRoute(key string) bool
}

// LLMFactory creates one client per runtime agent. Keeping the factory here
// avoids coupling the template lifecycle to static AgentKind bootstrap code.
type LLMFactory func(model string) llm.Client
