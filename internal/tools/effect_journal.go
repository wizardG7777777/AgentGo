package tools

// effect_journal.go 是 V6 §4 H2b Effect Journal 在工具层的埋点助手。
// 统一「账本失败只告警降级、绝不阻断副作用本身」的纪律（与 trace 同一
// 纪律）：journal 为 nil、任务上下文缺失或 Prepare/Settle/MarkUnknown
// 返回错误时，只 log 告警并返回空 ID，调用方继续执行副作用。

import (
	"context"
	"log"

	"agentgo/internal/agent"
	"agentgo/internal/effect"
)

// effectPrepare 在副作用执行前落账（prepared）。返回 effect ID；
// 不可用（nil journal / 无任务上下文 / 落账失败）时返回空串——调用方
// 据此跳过后续 Settle/MarkUnknown。
func effectPrepare(j *effect.Journal, ctx context.Context, agentID string,
	kind effect.Kind, target, argsDigest string, policy effect.ReplayPolicy) string {
	if j == nil {
		return ""
	}
	taskID := agent.TaskIDFromContext(ctx)
	if taskID == "" {
		return ""
	}
	e := effect.Effect{
		TaskID: taskID, AgentID: agentID,
		Kind: kind, Target: target, ArgsDigest: argsDigest, Policy: policy,
	}
	if err := j.Prepare(&e); err != nil {
		log.Printf("[EffectJournal] WARN %s 意图落账失败（降级，不阻断副作用）: %v", kind, err)
		return ""
	}
	return e.ID
}

// effectSettle 记录副作用执行结果（settled）。id 为空（未落账）时跳过。
func effectSettle(j *effect.Journal, id, summary string) {
	if j == nil || id == "" {
		return
	}
	if err := j.Settle(id, summary); err != nil {
		log.Printf("[EffectJournal] WARN 结果落账失败 (id=%s，降级，不阻断副作用): %v", id, err)
	}
}

// effectMarkUnknown 把副作用标为 unknown（结果不可知）。id 为空时跳过。
func effectMarkUnknown(j *effect.Journal, id, reason string) {
	if j == nil || id == "" {
		return
	}
	if err := j.MarkUnknown(id, reason); err != nil {
		log.Printf("[EffectJournal] WARN unknown 落账失败 (id=%s，降级，不阻断副作用): %v", id, err)
	}
}

// digest12 返回 data 的 sha256 hex 前 12（Effect ArgsDigest 口径）。
func digest12(data []byte) string {
	sum := computeSHA256(data)
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
