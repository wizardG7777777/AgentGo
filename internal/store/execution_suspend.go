package store

// taskExecutionSuspender is optional so alternate TaskStore implementations
// remain source compatible. MemoryTaskStore implements the durable path.
type taskExecutionSuspender interface {
	SuspendTaskExecution(agentID, taskID, reason string, lastHistory []byte) error
}

// SuspendTaskExecution cooperatively releases a processing task after its Plan
// stops execution. Older stores fall back to RetryRollback; production uses
// MemoryTaskStore and therefore does not consume a retry.
func SuspendTaskExecution(s TaskStore, agentID, taskID, reason string, lastHistory []byte) error {
	if suspender, ok := s.(taskExecutionSuspender); ok {
		return suspender.SuspendTaskExecution(agentID, taskID, reason, lastHistory)
	}
	return s.RetryRollback(agentID, taskID, reason)
}
