package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"agentgo/internal/model"
)

// externalFactFingerprintPrefix 标记由控制面外部事实核验（VerifyAcceptance）
// 失败产生的 FailureFingerprint，与提交方自填的指纹区分。
const externalFactFingerprintPrefix = "extfact:"

// hardConstraintFingerprintPrefix 标记由控制面硬约束校验（acceptanceConstraintReason，
// 如 command evidence target mismatch）失败产生的 FailureFingerprint。
// 2026-07-21 事故中同因硬约束失败连续出现 3 次却无熔断——extfact 之外的
// 失败同样纳入熔断统计（仅这两种控制面前缀计入，提交方自填指纹不计）。
const hardConstraintFingerprintPrefix = "hardc:"

// externalFactFailureCircuit 是触发验收熔断的连续同指纹失败次数。
const externalFactFailureCircuit = 2

// circuitFingerprint 报告指纹是否属于计入熔断的控制面前缀。
func circuitFingerprint(fingerprint string) bool {
	return strings.HasPrefix(fingerprint, externalFactFingerprintPrefix) ||
		strings.HasPrefix(fingerprint, hardConstraintFingerprintPrefix)
}

// ExternalFactFailureFingerprint 把外部事实核验错误归一化为稳定指纹：
// 去掉双引号包裹的值和 ≥8 字符的 hex/UUID token，使同一类格式缺陷
// （即使命令串、任务 ID、证据 ID 不同）产生相同指纹。
func ExternalFactFailureFingerprint(err error) string {
	if err == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(normalizeExternalFactError(err.Error())))
	return externalFactFingerprintPrefix + hex.EncodeToString(sum[:])
}

// HardConstraintFailureFingerprint 把硬约束校验失败原因归一化为稳定指纹，
// 归一化规则与 ExternalFactFailureFingerprint 相同，仅前缀不同。
func HardConstraintFailureFingerprint(reason string) string {
	if reason == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalizeExternalFactError(reason)))
	return hardConstraintFingerprintPrefix + hex.EncodeToString(sum[:])
}

func normalizeExternalFactError(msg string) string {
	var b strings.Builder
	b.Grow(len(msg))
	inQuote := false
	var token []byte
	flushToken := func() {
		if isVolatileToken(token) {
			b.WriteByte('?')
		} else {
			b.Write(token)
		}
		token = token[:0]
	}
	for i := 0; i < len(msg); i++ {
		ch := msg[i]
		if inQuote {
			if ch == '"' {
				inQuote = false
				b.WriteString(`"?"`)
			}
			continue
		}
		switch {
		case ch == '"':
			flushToken()
			inQuote = true
		case isHexTokenChar(ch):
			token = append(token, ch)
		default:
			flushToken()
			b.WriteByte(ch)
		}
	}
	flushToken()
	if inQuote {
		b.WriteString(`"?"`)
	}
	return b.String()
}

func isHexTokenChar(ch byte) bool {
	return ch == '-' || (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

// isVolatileToken 把"≥8 字符且至少含一个数字的纯 hex（可含连字符）"视为
// 任务 ID / 证据 ID / UUID 等易变标识。
func isVolatileToken(tok []byte) bool {
	if len(tok) < 8 {
		return false
	}
	for _, ch := range tok {
		if ch >= '0' && ch <= '9' {
			return true
		}
	}
	return false
}

// leadingExternalFactFailures 统计最新连续若干个"同 epoch、同控制面指纹、
// Verdict=fail"的验收结果。epoch = 当前 SpecID/SpecRevision。
//
// 注意：GraphDigest 刻意不参与 epoch——每次 ensure_acceptance_run 都会发布
// 新的 runner task 从而改变 digest，若 digest 参与 epoch，验收重试循环里
// 熔断在构造上永远无法触发（2026-07-21 两次事故均证实）。Spec 变化
// （scheduler 改了验收标准）仍会使熔断自动复位；同一 Spec 下同因失败连续
// 出现即视为同一未解决的根因。返回指纹与连续次数。
func leadingExternalFactFailures(p *model.Plan) (string, int) {
	if p == nil || len(p.AcceptanceResults) == 0 {
		return "", 0
	}
	results := make([]model.AcceptanceResult, 0, len(p.AcceptanceResults))
	for _, result := range p.AcceptanceResults {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].CreatedAt.After(results[j].CreatedAt)
	})
	fingerprint := ""
	count := 0
	for _, result := range results {
		if result.Verdict != model.AcceptanceVerdictFail ||
			!circuitFingerprint(result.FailureFingerprint) {
			break
		}
		run, ok := p.AcceptanceRuns[result.RunID]
		if !ok || run.SpecID != p.CurrentAcceptanceSpecID ||
			run.SpecRevision != p.CurrentAcceptanceSpecRevision {
			break
		}
		if fingerprint == "" {
			fingerprint = result.FailureFingerprint
		} else if result.FailureFingerprint != fingerprint {
			break
		}
		count++
	}
	return fingerprint, count
}
