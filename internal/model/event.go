package model

type EventType string

const (
	EventTaskCompleted EventType = "task_completed"
	EventTaskFailed    EventType = "task_failed"
	EventTaskCancelled EventType = "task_cancelled"
	EventTaskRetry     EventType = "task_retry"
	EventUserInput     EventType = "user_input"
	EventWatchdogAlert EventType = "watchdog_alert"
	EventTickerWakeup  EventType = "ticker_wakeup"
)

type Event struct {
	Type    EventType
	TaskID  string
	Payload map[string]string // 用于 EventUserInput 传递用户文本，其他事件为 nil
}
