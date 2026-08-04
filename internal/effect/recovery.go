package effect

// recovery.go 是 Effect Journal 的崩溃恢复裁决（V6 §4 H2b 第 14 条）。
//
// 启动 Replay 之后执行：
//  1. prepared/dispatched 未 settled 的 Effect 一律标 unknown
//    （发 effect_unknown）——进程在副作用执行窗口退出，结果不可知；
//    已持久化的 unknown 每次启动继续进入裁决清单，直到明确 settle；
//  2. 按 ReplayPolicy 逐条裁决（发 effect_recovery_decided，含结论与依据）：
//     verify_first 经 Verifier 核验外部状态——一致转 settled「已核验」，
//     不一致或无法核验保持 unknown + 告警；manual_only / never_replay 不
//     自动执行任何动作，保持 unknown + 告警（交用户/Scheduler 裁决）；
//     safe_replay（预留策略，当前无一埋点使用）同样不自动重跑，只标注
//     可重放——V6 红线：「未知不得静默重跑」，本轮任何策略都不自动重放。
//
// Recover 全程只读外部状态（Verifier 只允许核验），不产生任何写副作用。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"

	"agentgo/internal/trace"
)

// 恢复裁决结论（effect_recovery_decided 事件的 Decision 值）。
const (
	// DecisionVerifiedSettled：verify_first 核验外部状态与账载一致 → 转 settled「已核验」。
	DecisionVerifiedSettled = "verified_settled"
	// DecisionKeptUnknownMismatch：verify_first 完成了核验但不一致 → 保持 unknown + 告警。
	DecisionKeptUnknownMismatch = "kept_unknown_mismatch"
	// DecisionKeptUnknownUnverifiable：无法核验（文件不可读/无核验器）→ 保持 unknown + 告警。
	DecisionKeptUnknownUnverifiable = "kept_unknown_unverifiable"
	// DecisionKeptUnknownManual：manual_only / never_replay——不自动执行任何动作，交用户/Scheduler 裁决。
	DecisionKeptUnknownManual = "kept_unknown_manual"
	// DecisionReplayableHold：safe_replay（预留）——标注可重放但仍不自动重跑（V6 红线）。
	DecisionReplayableHold = "replayable_hold"
)

// RecoveryDecision 是一条 Effect 的启动恢复裁决记录。
type RecoveryDecision struct {
	EffectID string
	TaskID   string
	Kind     Kind
	Policy   ReplayPolicy
	Target   string
	Decision string
	Reason   string
}

// VerifyResult 是一次外部状态核验的结果。
type VerifyResult struct {
	Matched    bool   // 外部状态与账载一致（副作用已生效）
	Verifiable bool   // 是否完成了核验（目标不可读/无核验器 = false）
	Detail     string // 依据（裁决 Reason）
}

// Verifier 核验一条未 settled 的 verify_first Effect 的外部状态。
// 实现必须只读——恢复阶段禁止任何写副作用。
type Verifier interface {
	Verify(e *Effect) VerifyResult
}

// FileHashVerifier 核验文件类 Effect（file_write/file_edit）：读取 Target
// 路径的盘上内容，取 sha256 前 12 与 ArgsDigest（将落盘内容的 digest）比对。
// 其它 Kind 一律不可核验。Target 是主根逻辑绝对路径；隔离任务的写落点在
// workspace 内，主根比对必然不一致/不可读 → 保守保持 unknown（正确姿态：
// 崩溃时合并未发生，主根本就不该有这份内容）。
type FileHashVerifier struct{}

// Verify 实现 Verifier。
func (FileHashVerifier) Verify(e *Effect) VerifyResult {
	if e.Kind != KindFileWrite && e.Kind != KindFileEdit {
		return VerifyResult{Verifiable: false, Detail: "非文件类 Effect，无核验器"}
	}
	data, err := os.ReadFile(e.Target)
	if err != nil {
		return VerifyResult{Verifiable: false, Detail: fmt.Sprintf("目标文件不可读: %v", err)}
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])[:argsDigestLen]
	if got != e.ArgsDigest {
		return VerifyResult{
			Verifiable: true,
			Detail:     fmt.Sprintf("hash 不一致（账载 %s，盘上 %s）", e.ArgsDigest, got),
		}
	}
	return VerifyResult{Matched: true, Verifiable: true, Detail: "文件 hash 与账载一致"}
}

// Recover 在启动 Replay 之后执行崩溃恢复裁决，返回逐条未解决
// Effect 裁决清单，供 bootstrap 对对应非终态任务做 quarantine。
// prepared/dispatched 首先 durable 转 unknown；已是 unknown 的条目也会在
// 每次启动重新裁决并返回，直到 verify_first 可证明已生效而
// settle，避免第二次启动遗忘未处理 unknown。verifier 为 nil 时所有
// verify_first 按「无法核验」处理。
func (j *Journal) Recover(verifier Verifier) []RecoveryDecision {
	// 先持锁快照待裁决目标（prepared/dispatched/unknown），再逐条走公开的
	// MarkUnknown/Settle——避免在遍历索引时嵌套改索引。
	j.mu.Lock()
	var pending []*Effect
	for _, id := range j.order {
		e := j.index[id]
		if e.Status == StatusPrepared || e.Status == StatusDispatched || e.Status == StatusUnknown {
			cp := *e
			pending = append(pending, &cp)
		}
	}
	j.mu.Unlock()

	var decisions []RecoveryDecision
	for _, e := range pending {
		if e.Status != StatusUnknown {
			markReason := "进程在副作用执行窗口退出（prepared 后未见 settled），结果不可知"
			if err := j.MarkUnknown(e.ID, markReason); err != nil {
				// journal 写失败不能把一个客观上结果未知的 Effect 从
				// bootstrap quarantine 清单中漏掉，否则对应任务仍会按
				// prepared 快照重跑。即使 durable 状态未能改成 unknown，
				// 也返回 fail-closed 裁决，让恢复面阻断任务。
				d := RecoveryDecision{
					EffectID: e.ID, TaskID: e.TaskID, Kind: e.Kind,
					Policy: e.Policy, Target: e.Target,
					Decision: DecisionKeptUnknownUnverifiable,
					Reason:   "恢复标记 unknown 落账失败: " + err.Error(),
				}
				decisions = append(decisions, d)
				emitRecoveryDecided(e, d)
				log.Printf("[EffectJournal] WARN 恢复标记 unknown 失败 (id=%s): %v——对应任务仍进入 quarantine", e.ID, err)
				continue
			}
			e.Status = StatusUnknown
			e.UnknownReason = markReason
		}
		d := RecoveryDecision{
			EffectID: e.ID, TaskID: e.TaskID, Kind: e.Kind,
			Policy: e.Policy, Target: e.Target,
		}
		switch e.Policy {
		case PolicyVerifyFirst:
			res := VerifyResult{Verifiable: false, Detail: "无可用核验器"}
			if verifier != nil {
				res = verifier.Verify(e)
			}
			switch {
			case res.Matched:
				d.Decision = DecisionVerifiedSettled
				d.Reason = res.Detail
				// 转 settled「已核验」——把恢复结论 durable 落账。
				if err := j.Settle(e.ID, "恢复核验已确认: "+res.Detail); err != nil {
					log.Printf("[EffectJournal] WARN 恢复核验落账失败 (id=%s): %v", e.ID, err)
					d.Decision = DecisionKeptUnknownUnverifiable
					d.Reason = "核验一致但 settle 落账失败: " + err.Error()
				}
			case res.Verifiable:
				d.Decision = DecisionKeptUnknownMismatch
				d.Reason = res.Detail
			default:
				d.Decision = DecisionKeptUnknownUnverifiable
				d.Reason = res.Detail
			}
		case PolicySafeReplay:
			d.Decision = DecisionReplayableHold
			d.Reason = "策略声明可安全重放，但本轮不自动重跑（V6 红线：未知不得静默重跑），保持 unknown 待裁决"
		default: // manual_only / never_replay / 未声明策略
			d.Decision = DecisionKeptUnknownManual
			d.Reason = fmt.Sprintf("策略 %s 禁止自动重放，不自动执行任何动作，交用户/Scheduler 裁决", e.Policy)
		}
		decisions = append(decisions, d)
		emitRecoveryDecided(e, d)
		if d.Decision == DecisionVerifiedSettled {
			log.Printf("[EffectJournal] 恢复裁决 effect=%s kind=%s policy=%s decision=%s（%s）",
				d.EffectID, d.Kind, d.Policy, d.Decision, d.Reason)
		} else {
			// unknown 残留必须向用户可见（日志 + trace 事件双通道）。
			log.Printf("[EffectJournal] WARN 恢复裁决 effect=%s kind=%s policy=%s target=%s decision=%s（%s）——保持 unknown，需人工/Scheduler 裁决",
				d.EffectID, d.Kind, d.Policy, d.Target, d.Decision, d.Reason)
		}
	}
	return decisions
}

// emitRecoveryDecided 发送 effect_recovery_decided 事件（含结论与依据）。
func emitRecoveryDecided(e *Effect, d RecoveryDecision) {
	status := StatusUnknown
	if d.Decision == DecisionVerifiedSettled {
		status = StatusSettled
	}
	trace.Emit(trace.Event{
		Kind:    trace.KindEffectRecoveryDecided,
		TaskID:  e.TaskID,
		AgentID: e.AgentID,
		Effect: &trace.EffectPayload{
			EffectID:   d.EffectID,
			Kind:       string(d.Kind),
			Policy:     string(d.Policy),
			Status:     string(status),
			Target:     d.Target,
			ArgsDigest: e.ArgsDigest,
			Decision:   d.Decision,
			Reason:     d.Reason,
		},
	})
}
