package tools

// effect_journal.go 是 V6 §4 H2b Effect Journal 在工具层的权威助手。
// Journal 一旦装配，Prepare/Settle/MarkUnknown 失败必须返回 typed
// AuthorityError。nil 只保留给 legacy/隔离单测；生产在启动期通过
// effect.RequireJournal 校验，不允许 nil 进入执行面。

import (
	"context"
	"fmt"

	"agentgo/internal/agent"
	"agentgo/internal/effect"
)

// effectPrepare 在副作用执行前落账（prepared）。错误的
// MayHaveHappened 始终为 false，调用方必须在任何外部变更前返回。
func effectPrepare(j *effect.Journal, ctx context.Context, agentID string,
	kind effect.Kind, target, argsDigest string, policy effect.ReplayPolicy) (string, error) {
	if j == nil {
		return "", nil
	}
	taskID := agent.TaskIDFromContext(ctx)
	if taskID == "" {
		return "", effect.NewAuthorityError(effect.AuthorityPhasePrepare, "", false,
			fmt.Errorf("effect Prepare 缺少任务上下文"))
	}
	e := effect.Effect{
		TaskID: taskID, AgentID: agentID,
		Kind: kind, Target: target, ArgsDigest: argsDigest, Policy: policy,
	}
	if err := j.Prepare(&e); err != nil {
		return "", effect.NewAuthorityError(effect.AuthorityPhasePrepare, "", false, err)
	}
	return e.ID, nil
}

// effectSettle 记录副作用执行结果。mayHaveHappened 由调用点根据
// 外部执行边界显式给出；落账失败不得被当成工具成功。
func effectSettle(j *effect.Journal, id, summary string, mayHaveHappened bool) error {
	if j == nil {
		return nil
	}
	if id == "" {
		return effect.NewAuthorityError(effect.AuthorityPhaseSettle, "", mayHaveHappened,
			fmt.Errorf("effect Settle 缺少 id"))
	}
	if err := j.Settle(id, summary); err != nil {
		return effect.NewAuthorityError(effect.AuthorityPhaseSettle, id, mayHaveHappened, err)
	}
	return nil
}

// effectMarkUnknown 把返回错误但可能已产生外部变更的副作用标为
// unknown。该写本身失败时固定 may_have_happened=true。
func effectMarkUnknown(j *effect.Journal, id, reason string) error {
	if j == nil {
		return nil
	}
	if id == "" {
		return effect.NewAuthorityError(effect.AuthorityPhaseUnknown, "", true,
			fmt.Errorf("effect MarkUnknown 缺少 id"))
	}
	if err := j.MarkUnknown(id, reason); err != nil {
		return effect.NewAuthorityError(effect.AuthorityPhaseUnknown, id, true, err)
	}
	return nil
}

// digest12 返回 data 的 sha256 hex 前 12（Effect ArgsDigest 口径）。
func digest12(data []byte) string {
	sum := computeSHA256(data)
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
