// Package testagent 为工具和循环测试提供显式的当前模型调用身份；生产代码不得导入。
package testagent

import (
	"agentgo/internal/agent"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"context"
	"github.com/google/uuid"
)

// Wrap 在隔离 L3 测试中扮演调用规格提供者，仍执行正式 L2/L1。
func Wrap(step agent.TaskExecutor) agent.TaskExecutor {
	return func(ctx context.Context, task *model.Task, deps map[string]string, history []contextcontract.HistoryEntry, budget llm.OutputBudget) (agent.ExecuteResult, error) {
		task.ContextPolicyRef = policycatalog.ContextDefaultCurrent
		if task.Lease == nil {
			task.Lease = &model.ExecutionLease{Schema: model.ExecutionLeaseSchemaCurrent,Digest: "test-lease", Model: "test-model", ModelCapabilityDigest: "test-capability"}
		}
		if task.AttemptID == "" {
			task.AttemptID = "test-attempt:" + task.ID
		}
		ctx = agent.WithExecutionIdentity(ctx, string(task.RunID), task.AttemptID, "test-turn:"+uuid.NewString())
		return step(ctx, task, deps, history, budget)
	}
}
