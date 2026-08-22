package model

import (
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/runcontract"
)

type EventType string

const (
	EventTaskCompleted EventType = "task_completed"
	EventTaskFailed    EventType = "task_failed"
	EventTaskCancelled EventType = "task_cancelled"
	EventTaskBlocked   EventType = "task_blocked"
	EventTaskRetry     EventType = "task_retry"
	EventUserInput     EventType = "user_input"
	EventWatchdogAlert EventType = "watchdog_alert"
	// EventWatchdogObservation 是不携带控制权的 typed liveness 观测。
	// 它与 legacy EventWatchdogAlert 分离，Activator 不应据此合成普通文本 Task。
	EventWatchdogObservation EventType = "watchdog_observation"
	EventTickerWakeup        EventType = "ticker_wakeup"
	EventPlanSignal          EventType = "plan_signal"
	EventAcceptanceCompleted EventType = "acceptance_completed"
)

// WatchdogObservationKind 是 Watchdog 可以报告、但不能自行处置的封闭
// liveness fault。语义进展和 Task 迁移仍由 L4 Loop authority 决定。
type WatchdogObservationKind string

const (
	WatchdogHeartbeatStalled WatchdogObservationKind = "heartbeat_stalled"
	WatchdogHardDeadlineRisk WatchdogObservationKind = "hard_deadline_risk"
)

type WatchdogCheckpointState string

const (
	WatchdogCheckpointAvailable  WatchdogCheckpointState = "available"
	WatchdogCheckpointStale      WatchdogCheckpointState = "stale"
	WatchdogCheckpointMissing    WatchdogCheckpointState = "missing"
	WatchdogCheckpointReadError  WatchdogCheckpointState = "read_error"
	WatchdogCheckpointInvalid    WatchdogCheckpointState = "invalid"
	WatchdogCheckpointOldAttempt WatchdogCheckpointState = "old_attempt"
)

type WatchdogDeadlineState string

const (
	WatchdogDeadlineAtRisk   WatchdogDeadlineState = "risk"
	WatchdogDeadlineExceeded WatchdogDeadlineState = "exceeded"
)

// WatchdogObservation 保存一次结构化 liveness 事实。CheckpointState 用于
// 区分 stale/missing/read_error/invalid；DeadlineScope 只在 deadline 风险时设置。
type WatchdogObservation struct {
	Kind                WatchdogObservationKind
	TaskID              string
	RunID               runcontract.RunID
	AttemptID           string
	CheckpointID        string
	CheckpointState     WatchdogCheckpointState
	CheckpointUpdatedAt time.Time
	HeartbeatLease      time.Duration
	InterventionStage   loopcontract.InterventionStage
	DeadlineScope       runcontract.DeadlineScope
	DeadlineState       WatchdogDeadlineState
	RiskAt              time.Time
	HardDeadlineAt      time.Time
	ObservedAt          time.Time
}

type Event struct {
	Type        EventType
	TaskID      string
	Payload     map[string]string // 用户输入与兼容消费者所需的有界结构化字段
	RunContract *runcontract.RunContract
	Observation *WatchdogObservation
}
