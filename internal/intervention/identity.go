package intervention

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const wakeTaskPrefix = "loop-intervention-wake-"

// WakeTaskID 把 durable CommandID 映射为稳定、定长的 Scheduler coordination
// Task identity。重复投递只会命中同一个显式 Task ID。
func WakeTaskID(commandID string) string {
	commandID = strings.TrimSpace(commandID)
	if commandID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(commandID))
	return wakeTaskPrefix + hex.EncodeToString(digest[:12])
}

// WakeDecisionRef 是 Ensure 阶段返回的稳定引用；它只表示 coordination Task
// identity 已被幂等物化，不等于 durable Ack。最终 Ack 使用该 Task 的
// TaskOutcome ref。
func WakeDecisionRef(commandID string) string {
	if taskID := WakeTaskID(commandID); taskID != "" {
		return "coordination-task:" + taskID
	}
	return ""
}
