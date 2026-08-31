// Package delivery 定义 Graph v3 的交付事务不可变身份与封闭状态机。
//
// 它刻意不依赖 graph、workspace 或 effect：L5 用它把候选、验收和主根
// promotion 串成一个可恢复的事实，L3/L4 只提供候选与证据引用。
package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const SchemaV1 = "agentgo.delivery/v1"

type Status string

const (
	StatusOpen           Status = "open"
	StatusPrepared       Status = "prepared"
	StatusVerifying      Status = "verifying"
	StatusRepairing      Status = "repairing"
	StatusCommitPrepared Status = "commit_prepared"
	StatusCommitted      Status = "committed"
	StatusQuarantined    Status = "quarantined"
	StatusCommitUnknown  Status = "commit_unknown"
)

func (s Status) IsValid() bool {
	switch s {
	case StatusOpen, StatusPrepared, StatusVerifying, StatusRepairing,
		StatusCommitPrepared, StatusCommitted, StatusQuarantined, StatusCommitUnknown:
		return true
	default:
		return false
	}
}

// Candidate 是冻结后的只读候选视图身份。PatchDigest 必须由 L3 按稳定
// manifest 顺序计算；本包只校验其存在，不从展示文本推导交付事实。
type Candidate struct {
	Ref                  string `json:"ref"`
	WorkspaceRevisionRef string `json:"workspace_revision_ref"`
	PatchDigest          string `json:"patch_digest"`
	ManifestDigest       string `json:"manifest_digest,omitempty"`
}

// Transaction 是一个 mutating producer 的交付事实。Generation 在 fixable
// repair 时递增，但 ID 永远不变；prepared 未 settled 的 promotion 绝不允许
// 自动重放，恢复后必须由 Effect Journal 给出明确结论。
type Transaction struct {
	Schema               string     `json:"schema"`
	ID                   string     `json:"delivery_id"`
	RunID                string     `json:"run_id"`
	GraphID              string     `json:"graph_id"`
	ProducerActivationID string     `json:"producer_activation_id"`
	Generation           int        `json:"generation"`
	Status               Status     `json:"status"`
	Candidate            *Candidate `json:"candidate,omitempty"`
	FulfillmentRef       string     `json:"fulfillment_ref,omitempty"`
	EvidenceRefs         []string   `json:"evidence_refs,omitempty"`
	ProducerOutcomeRef   string     `json:"producer_outcome_ref,omitempty"`
	AcceptanceOutcomeRef string     `json:"acceptance_outcome_ref,omitempty"`
	CommitIntentRef      string     `json:"commit_intent_ref,omitempty"`
	CommitEffectRef      string     `json:"commit_effect_ref,omitempty"`
	CommittedRevisionRef string     `json:"committed_revision_ref,omitempty"`
	QuarantineReason     string     `json:"quarantine_reason,omitempty"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// StableID 以 Run/Graph/首次 producer activation 的字节为身份来源；不含
// generation，确保 fixable repair 继续同一 Delivery Transaction。
func StableID(runID, graphID, producerActivationID string) string {
	payload := strings.Join([]string{runID, graphID, producerActivationID}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return "delivery:" + hex.EncodeToString(sum[:16])
}

func (t Transaction) Validate() error {
	if t.Schema != SchemaV1 {
		return fmt.Errorf("Delivery schema=%q，无效", t.Schema)
	}
	for name, value := range map[string]string{
		"delivery_id": t.ID, "run_id": t.RunID, "graph_id": t.GraphID,
		"producer_activation_id": t.ProducerActivationID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Delivery %s 不能为空", name)
		}
	}
	if t.ID != StableID(t.RunID, t.GraphID, t.ProducerActivationID) {
		return fmt.Errorf("Delivery ID 与 Run/Graph/producer identity 不一致")
	}
	if t.Generation < 0 || !t.Status.IsValid() {
		return fmt.Errorf("Delivery generation/status 非法: %d/%q", t.Generation, t.Status)
	}
	if t.Status != StatusOpen && t.Status != StatusQuarantined && t.Candidate == nil {
		return fmt.Errorf("Delivery status=%s 必须携带 candidate", t.Status)
	}
	if t.Status == StatusQuarantined && strings.TrimSpace(t.QuarantineReason) == "" {
		return fmt.Errorf("quarantined Delivery 必须携带 quarantine_reason")
	}
	if t.Candidate != nil {
		if strings.TrimSpace(t.Candidate.Ref) == "" || strings.TrimSpace(t.Candidate.WorkspaceRevisionRef) == "" || strings.TrimSpace(t.Candidate.PatchDigest) == "" {
			return fmt.Errorf("Delivery candidate 缺少 ref/workspace_revision_ref/patch_digest")
		}
	}
	if t.Status == StatusCommitted && (strings.TrimSpace(t.CommitEffectRef) == "" || strings.TrimSpace(t.CommittedRevisionRef) == "") {
		return fmt.Errorf("committed Delivery 必须携带 commit_effect_ref/committed_revision_ref")
	}
	if t.Status == StatusCommitPrepared && strings.TrimSpace(t.CommitIntentRef) == "" {
		return fmt.Errorf("commit_prepared Delivery 必须携带 commit_intent_ref")
	}
	return nil
}

// CanTransition 是闭合状态机。commit_unknown 与 quarantined 均是人工/明确
// recovery 边界，不提供隐式回到可提交状态的路径。
func (t Transaction) CanTransition(to Status) bool {
	if !t.Status.IsValid() || !to.IsValid() || t.Status == to {
		return false
	}
	switch t.Status {
	case StatusOpen:
		return to == StatusPrepared || to == StatusQuarantined
	case StatusPrepared:
		return to == StatusVerifying || to == StatusRepairing || to == StatusQuarantined
	case StatusVerifying:
		return to == StatusRepairing || to == StatusCommitPrepared || to == StatusQuarantined
	case StatusRepairing:
		return to == StatusPrepared || to == StatusQuarantined
	case StatusCommitPrepared:
		return to == StatusCommitted || to == StatusQuarantined || to == StatusCommitUnknown
	default:
		return false
	}
}

// Transition 返回带新状态与更新时间的副本；generation 只能在 repair→prepared
// 时由调用方先递增，防止任意状态绕过验收伪造 repair generation。
func (t Transaction) Transition(to Status, now time.Time) (Transaction, error) {
	if !t.CanTransition(to) {
		return Transaction{}, fmt.Errorf("Delivery 不能从 %s 迁移到 %s", t.Status, to)
	}
	if t.Status == StatusRepairing && to == StatusPrepared {
		t.Generation++
	}
	t.Status, t.UpdatedAt = to, now.UTC()
	return t, nil
}
