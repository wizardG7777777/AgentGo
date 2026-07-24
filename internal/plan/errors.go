package plan

import "errors"

var (
	ErrPlanNotFound             = errors.New("plan not found")
	ErrPlanAlreadyExists        = errors.New("plan already exists")
	ErrPlanTerminal             = errors.New("plan is terminal")
	ErrPlanPaused               = errors.New("plan is paused")
	ErrPlanPendingRequests      = errors.New("plan has pending replan requests")
	ErrRevisionConflict         = errors.New("plan revision conflict")
	ErrControllerConflict       = errors.New("plan controller authority conflict")
	ErrNodeNotFound             = errors.New("plan node not found")
	ErrNodeAlreadyExists        = errors.New("plan node already exists")
	ErrDependencyNotFound       = errors.New("plan node dependency not found")
	ErrGraphCycle               = errors.New("plan graph contains a cycle")
	ErrBudgetExceeded           = errors.New("plan budget exhausted")
	ErrPauseConflict            = errors.New("plan pause state conflict")
	ErrAcceptanceSpecNotDefined = errors.New("acceptance spec not defined")
	ErrAcceptanceSpecWeakening  = errors.New("acceptance spec cannot weaken protected criteria")
	ErrAcceptanceRunNotFound    = errors.New("acceptance run not found")
	ErrAcceptanceStale          = errors.New("acceptance result is stale")
	ErrAcceptanceConstraint     = errors.New("acceptance hard constraint failed")
	ErrAcceptanceNotPassed      = errors.New("latest acceptance has not passed")
	ErrAcceptanceCircuitOpen    = errors.New("acceptance circuit open: repeated identical external fact verification failures")
	ErrInvalidPauseResolution   = errors.New("invalid pause resolution")
)
