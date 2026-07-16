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
// must recover it. stopped is reserved for a terminal Plan.
type Status string

const (
	StatusReady   Status = "ready"
	StatusStopped Status = "stopped"
)

var (
	ErrNotStarted             = errors.New("team manager has not started")
	ErrManagerClosed          = errors.New("team manager is closed")
	ErrProcessLimitExceeded   = errors.New("template agent process limit exceeded")
	ErrTemplateDigestMismatch = errors.New("agent template digest does not match durable team spec")
	ErrTeamNotFound           = errors.New("team not found")
)

// TeamSpec is the durable recovery record for one homogeneous team. EventType
// is a private route (team:<uuid>) and remains stable across process restarts.
// Agent instance IDs are derived deterministically from this record and do not
// need to be persisted separately.
type TeamSpec struct {
	ID               string    `json:"id"`
	TemplateRef      string    `json:"template_ref"`
	TemplateDigest   string    `json:"template_digest"`
	PlanID           string    `json:"plan_id"`
	ControllerTaskID string    `json:"controller_task_id"`
	Purpose          string    `json:"purpose"`
	EventType        string    `json:"event_type"`
	Replicas         int       `json:"replicas"`
	Status           Status    `json:"status"`
	StopReason       string    `json:"stop_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// TeamStore is the minimum durable storage contract required by Manager.
// Ensure is atomic and returns created=false for the idempotency identity
// (plan, template ref, purpose, replicas).
type TeamStore interface {
	Ensure(spec TeamSpec) (stored TeamSpec, created bool, err error)
	Get(teamID string) (TeamSpec, error)
	List() ([]TeamSpec, error)
	SetStatus(teamID string, status Status, reason string) (TeamSpec, error)
	StopPlan(planID, reason string) ([]TeamSpec, error)
}

// RouteRegistry publishes/removes runtime-only Scheduler routing facts. key is
// the stable TeamSpec.EventType so registration can be rolled back exactly.
type RouteRegistry interface {
	RegisterRoute(key, eventType, planID string, count int, role string, capabilities []string) error
	UnregisterRoute(key string) bool
}

// LLMFactory creates one client per runtime agent. Keeping the factory here
// avoids coupling the template lifecycle to static AgentKind bootstrap code.
type LLMFactory func(model string) llm.Client
