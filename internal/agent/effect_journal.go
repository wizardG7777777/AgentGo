package agent

// effect_journal.go 是 V6 §4 H2b Effect Journal 在 agent 执行面的埋点助手。
// 与 internal/tools/effect_journal.go 同构的最小重复实现：刻意不抽取共享
// 包——tools 已 import agent，反向共享会成环。Journal 一旦装配，任何
// Prepare/Settle/MarkUnknown 错误都必须向上传播 typed AuthorityError。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"agentgo/internal/effect"
)

// effectPrepare 在副作用执行前落账（prepared）。Prepare 失败的
// MayHaveHappened=false，调用方必须在调用 workspace 合并前返回。
func (a *Agent) effectPrepare(kind effect.Kind, taskID, target, argsDigest string, policy effect.ReplayPolicy) (string, error) {
	j := a.EffectJournal
	if j == nil {
		return "", nil
	}
	if taskID == "" {
		return "", effect.NewAuthorityError(effect.AuthorityPhasePrepare, "", false,
			fmt.Errorf("effect Prepare 缺少 taskID"))
	}
	e := effect.Effect{
		TaskID: taskID, AgentID: a.ID,
		Kind: kind, Target: target, ArgsDigest: argsDigest, Policy: policy,
	}
	if err := j.Prepare(&e); err != nil {
		return "", effect.NewAuthorityError(effect.AuthorityPhasePrepare, "", false, err)
	}
	return e.ID, nil
}

// effectSettle 记录副作用结果；错误不得被当成合并成功。
func (a *Agent) effectSettle(id, summary string, mayHaveHappened bool) error {
	if a.EffectJournal == nil {
		return nil
	}
	if id == "" {
		return effect.NewAuthorityError(effect.AuthorityPhaseSettle, "", mayHaveHappened,
			fmt.Errorf("effect Settle 缺少 id"))
	}
	if err := a.EffectJournal.Settle(id, summary); err != nil {
		return effect.NewAuthorityError(effect.AuthorityPhaseSettle, id, mayHaveHappened, err)
	}
	return nil
}

// effectMarkUnknown 把可能已部分改变外部世界的合并标为 unknown。
func (a *Agent) effectMarkUnknown(id, reason string) error {
	if a.EffectJournal == nil {
		return nil
	}
	if id == "" {
		return effect.NewAuthorityError(effect.AuthorityPhaseUnknown, "", true,
			fmt.Errorf("effect MarkUnknown 缺少 id"))
	}
	if err := a.EffectJournal.MarkUnknown(id, reason); err != nil {
		return effect.NewAuthorityError(effect.AuthorityPhaseUnknown, id, true, err)
	}
	return nil
}

// effectDigest12 返回 data 的 sha256 hex 前 12（Effect ArgsDigest 口径）。
func effectDigest12(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
