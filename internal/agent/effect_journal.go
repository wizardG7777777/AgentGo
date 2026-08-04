package agent

// effect_journal.go 是 V6 §4 H2b Effect Journal 在 agent 执行面的埋点助手。
// 与 internal/tools/effect_journal.go 同构的最小重复实现：刻意不抽取共享
// 包——tools 已 import agent，反向共享会成环。同一纪律：账本失败只告警
// 降级，绝不阻断副作用本身（与 trace 同一纪律）。

import (
	"crypto/sha256"
	"encoding/hex"
	"log"

	"agentgo/internal/effect"
)

// effectPrepare 在副作用执行前落账（prepared）。返回 effect ID；
// 不可用（nil journal / 落账失败）时返回空串，调用方据此跳过后续记录。
func (a *Agent) effectPrepare(kind effect.Kind, taskID, target, argsDigest string, policy effect.ReplayPolicy) string {
	j := a.EffectJournal
	if j == nil || taskID == "" {
		return ""
	}
	e := effect.Effect{
		TaskID: taskID, AgentID: a.ID,
		Kind: kind, Target: target, ArgsDigest: argsDigest, Policy: policy,
	}
	if err := j.Prepare(&e); err != nil {
		log.Printf("[EffectJournal] WARN %s 意图落账失败（降级，不阻断副作用）: %v", kind, err)
		return ""
	}
	return e.ID
}

// effectSettle 记录副作用执行结果（settled）。id 为空（未落账）时跳过。
func (a *Agent) effectSettle(id, summary string) {
	if a.EffectJournal == nil || id == "" {
		return
	}
	if err := a.EffectJournal.Settle(id, summary); err != nil {
		log.Printf("[EffectJournal] WARN 结果落账失败 (id=%s，降级，不阻断副作用): %v", id, err)
	}
}

// effectMarkUnknown 把副作用标为 unknown（结果不可知）。id 为空时跳过。
func (a *Agent) effectMarkUnknown(id, reason string) {
	if a.EffectJournal == nil || id == "" {
		return
	}
	if err := a.EffectJournal.MarkUnknown(id, reason); err != nil {
		log.Printf("[EffectJournal] WARN unknown 落账失败 (id=%s，降级，不阻断副作用): %v", id, err)
	}
}

// effectDigest12 返回 data 的 sha256 hex 前 12（Effect ArgsDigest 口径）。
func effectDigest12(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
