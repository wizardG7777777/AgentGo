package agent

import (
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
)

// bindTaskMailEnvelope 把当前 Task 的冻结来源身份投影到新消息。legacy Task
// RunID 为空时保持 envelope 全空；不得只写 SourceTaskID 制造半绑定消息。
func (a *Agent) bindTaskMailEnvelope(msg *mailbox.Message, task *model.Task) {
	if msg == nil || task == nil || task.RunID == "" {
		return
	}
	msg.SourceTaskID = task.ID
	msg.RunID = task.RunID
	if a != nil && a.SessionID != nil {
		msg.SessionID = a.SessionID()
	}
}
