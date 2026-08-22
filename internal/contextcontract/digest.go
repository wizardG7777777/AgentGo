package contextcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const digestHexLength = sha256.Size * 2

// DigestBytes 返回正文/编码载荷的完整 sha256 hex。短 digest 只适合 UI 展示，
// 不得作为 Context identity 或内容寻址权威。
func DigestBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// StableDigest 对 typed canonical DTO 求带域分隔的稳定 sha256。Go 的
// encoding/json 会按字符串 map key 排序；调用者仍须先规范化语义上属于集合的
// slice（ContextBudgetPolicy/ProviderReplayPolicy 的方法已处理）。
func StableDigest(domain string, value any) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return "", fmt.Errorf("稳定 digest 缺少 domain")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("序列化稳定 digest 输入失败: %w", err)
	}
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ValidDigest 报告 s 是否是 canonical 完整 sha256 hex。
func ValidDigest(s string) bool {
	if len(s) != digestHexLength || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ComputeDigest 返回 ContextBudgetPolicy 的稳定身份。AllowedDispositions 与
// TransformIDs 是集合语义，计算前排序、去重；调用方输入对象不被修改。
func (p ContextBudgetPolicy) ComputeDigest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	canonical := p
	canonical.FragmentRules = make(map[FragmentKind]FragmentBudgetRule, len(p.FragmentRules))
	for kind, rule := range p.FragmentRules {
		rule.AllowedDispositions = canonicalDispositions(rule.AllowedDispositions)
		canonical.FragmentRules[kind] = rule
	}
	canonical.AtomicGroupRules = make(map[AtomicGroupKind]AtomicGroupBudgetRule, len(p.AtomicGroupRules))
	for kind, rule := range p.AtomicGroupRules {
		rule.TransformIDs = canonicalStrings(rule.TransformIDs)
		canonical.AtomicGroupRules[kind] = rule
	}
	canonical.SectionBudgets = make(map[ContextSection]Budget, len(p.SectionBudgets))
	for section, budget := range p.SectionBudgets {
		canonical.SectionBudgets[section] = budget
	}
	return StableDigest("agentgo.context-policy/v1", canonical)
}

// ComputeDigest 返回 ProviderReplayPolicy 的稳定身份。
func (p ProviderReplayPolicy) ComputeDigest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	canonical := p
	canonical.Fields = make(map[string]ReplayRequirement, len(p.Fields))
	for field, requirement := range p.Fields {
		canonical.Fields[field] = requirement
	}
	canonical.GroupTransforms = append([]ReplayTransform(nil), p.GroupTransforms...)
	sort.Slice(canonical.GroupTransforms, func(i, j int) bool {
		left, right := canonical.GroupTransforms[i], canonical.GroupTransforms[j]
		if left.GroupKind != right.GroupKind {
			return left.GroupKind < right.GroupKind
		}
		return left.TransformID < right.TransformID
	})
	return StableDigest("agentgo.provider-replay/v1", canonical)
}

// SemanticDigest 返回 Snapshot 的编译语义摘要。生命周期 identity、封存时间与
// recovery lineage 不进入摘要；Fragment/Wire 顺序保留，因为顺序本身影响模型
// 看到的上下文。EncodedRequestDigest 仍是实际 provider 请求字节的最终权威。
func (s ContextSnapshot) SemanticDigest() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	canonical := s
	canonical.SnapshotID = ""
	canonical.AttemptID = ""
	canonical.InvocationID = ""
	canonical.ParentSnapshotRef = ""
	canonical.RecoveryReason = ""
	// time.Time 的零值用显式赋值，避免 wall clock/时区进入语义摘要。
	canonical.SealedAt = time.Time{}
	canonical.Manifest.SnapshotID = ""
	return StableDigest("agentgo.context-snapshot-semantic/v1", canonical)
}

func canonicalDispositions(values []Disposition) []Disposition {
	seen := make(map[Disposition]struct{}, len(values))
	out := make([]Disposition, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func canonicalStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
