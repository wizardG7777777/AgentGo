// promotion.go 实现 V6 §3 CM3 的 Session Memory 晋升规则：Task 进入终态后，
// 从最终 Task Memory（终态 Sealed）筛选可跨 Task 保留的内容，构建 Session
// 条目候选（纯函数，不写存储——写入与幂等由调用方负责）。
//
// 终态规则（docs/nextUpgrade-V6.md §3「不同 Task 终态采用不同晋升规则」）：
//   - completed：晋升已验证结果、产物、用户决定和仍然适用的约束；
//   - blocked：晋升阻塞原因、已经尝试的方案、现有证据和恢复条件，不宣称任务完成；
//   - failed：只晋升可复现的失败证据、已排除方案和避免重复失败所需的信息；
//   - cancelled：默认不晋升中间推断，只保留已经发生的权威 Effect、明确用户决定
//     和必要审计引用。
//
// 证据纪律：Task Memory 中 confirmed 的事实可晋升为 confirmed 条目；inferred
// 事实在 CM3 一律直接丢弃（不晋升为 confirmed，也不以 inferred 条目保留）——
// 模型文本声称不算证据，没有结构化证据支撑的内容不进入 Session 长期记忆。
//
// Key 设计：
//   - 任务级记录（结果/失败/阻塞/取消 Effect）带 taskID，每任务一键、天然不撞键；
//   - 用户决定与约束按内容哈希取键——不同任务的相同结论落到同 Key，写入时
//     经 SessionStore.Supersede 自动 supersede 旧条目（取代链保留审计）。
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"agentgo/internal/taskmem"
)

// 终态词表（与 model.TaskStatus 的终态字符串一致；promotion 不依赖 model，
// 由调用方从 trace 终态事件映射）。
const (
	TerminalCompleted = "completed"
	TerminalBlocked   = "blocked"
	TerminalFailed    = "failed"
	TerminalCancelled = "cancelled"
)

// promotionContentMaxRunes 是单条晋升条目正文的硬上限（与召回渲染预算同量级，
// 防止单条记忆在注入时挤占全部预算）。
const promotionContentMaxRunes = 1200

// userDecisionPrefix 是 Task Memory 中用户决定事实的固定前缀（taskmem.ApplyTurn
// 写入 "用户决定: <正文>"，晋升侧据此识别明确用户决定）。
const userDecisionPrefix = "用户决定: "

// BuildPromotionCandidates 从终态 Task Memory 构建 Session 晋升候选。
// terminalStatus 取 Terminal* 常量；未知终态返回 nil（调用方按无候选处理）。
// 返回的条目 Scope=ScopeSession、State 一律 confirmed（inferred 事实已丢弃）、
// ID 留空（写入侧 Supersede 生成）。
func BuildPromotionCandidates(m *taskmem.TaskMemory, terminalStatus string) []Entry {
	if m == nil || m.TaskID == "" {
		return nil
	}
	switch terminalStatus {
	case TerminalCompleted:
		return completedCandidates(m)
	case TerminalBlocked:
		return blockedCandidates(m)
	case TerminalFailed:
		return failedCandidates(m)
	case TerminalCancelled:
		return cancelledCandidates(m)
	}
	return nil
}

// completedCandidates 实现 completed 终态规则：已验证结果 + 产物 +
// 用户决定 + 仍适用约束。
func completedCandidates(m *taskmem.TaskMemory) []Entry {
	var out []Entry
	facts := confirmedFacts(m)
	if len(facts) > 0 || len(m.Files) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "任务 %s 已完成（终态 completed）。\n", m.TaskID)
		writeGoalLine(&sb, m)
		writeFactLines(&sb, "已验证结果:", nonDecisionFacts(facts))
		writeFileLines(&sb, m)
		out = append(out, promotionEntry(m, KindResult, "result:"+m.TaskID, sb.String(), resultEvidence(m, facts)))
	}
	out = append(out, decisionEntries(m, facts)...)
	out = append(out, constraintEntries(m)...)
	return out
}

// blockedCandidates 实现 blocked 终态规则：阻塞原因 + 已尝试方案 + 现有证据 +
// 恢复条件；正文显式标注未完成，绝不宣称任务完成。
func blockedCandidates(m *taskmem.TaskMemory) []Entry {
	if len(m.Blockers) == 0 && len(m.Failures) == 0 && len(m.NextCandidates) == 0 {
		return nil
	}
	facts := confirmedFacts(m)
	var sb strings.Builder
	fmt.Fprintf(&sb, "任务 %s 阻塞（终态 blocked，未完成——不得当作已完成结论引用）。\n", m.TaskID)
	writeGoalLine(&sb, m)
	writeStringLines(&sb, "阻塞原因:", m.Blockers)
	writeStringLines(&sb, "已尝试方案（未打通）:", m.Failures)
	writeFactLines(&sb, "现有证据:", facts)
	writeStringLines(&sb, "恢复条件 / 下一步:", m.NextCandidates)
	return []Entry{promotionEntry(m, KindBlocker, "blocked:"+m.TaskID, sb.String(), resultEvidence(m, facts))}
}

// failedCandidates 实现 failed 终态规则：只晋升可复现失败证据、已排除方案
// 与避免重复失败所需的信息。
func failedCandidates(m *taskmem.TaskMemory) []Entry {
	if len(m.Failures) == 0 {
		return nil
	}
	facts := confirmedFacts(m)
	var sb strings.Builder
	fmt.Fprintf(&sb, "任务 %s 失败（终态 failed，未完成）。\n", m.TaskID)
	writeGoalLine(&sb, m)
	writeStringLines(&sb, "可复现失败证据（已排除方案）:", m.Failures)
	writeStringLines(&sb, "避免重复失败:", m.NextCandidates)
	writeFactLines(&sb, "已验证事实:", facts)
	return []Entry{promotionEntry(m, KindLearning, "failure:"+m.TaskID, sb.String(), resultEvidence(m, facts))}
}

// cancelledCandidates 实现 cancelled 终态规则：只保留已发生的权威 Effect
// （文件/产物版本）、明确用户决定与必要审计引用；中间推断（Actions/阻塞/
// 失败尝试等过程记录）一律不晋升。
func cancelledCandidates(m *taskmem.TaskMemory) []Entry {
	var out []Entry
	if len(m.Files) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "任务 %s 已取消（终态 cancelled，未完成）。\n", m.TaskID)
		writeGoalLine(&sb, m)
		writeFileLines(&sb, m)
		out = append(out, promotionEntry(m, KindResult, "effects:"+m.TaskID, sb.String(), fileEvidence(m)))
	}
	out = append(out, decisionEntries(m, confirmedFacts(m))...)
	return out
}

// --- 候选构建 helpers ---

// promotionEntry 组装一条晋升候选（ScopeSession + confirmed + 来源/标签审计
// 信息）。正文超预算时硬截断（保留前部高优先段，截断点追加省略标记）。
func promotionEntry(m *taskmem.TaskMemory, kind Kind, key, content string, evidence []string) Entry {
	return Entry{
		Scope:    ScopeSession,
		Kind:     kind,
		Key:      key,
		Content:  truncatePromotionRunes(strings.TrimSpace(content), promotionContentMaxRunes),
		State:    StateConfirmed,
		Evidence: evidence,
		Source:   m.TaskID,
		Tags:     []string{"session_promotion"},
	}
}

// confirmedFacts 返回全部 confirmed 事实（inferred 一律丢弃——证据纪律）。
func confirmedFacts(m *taskmem.TaskMemory) []taskmem.Fact {
	out := make([]taskmem.Fact, 0, len(m.Facts))
	for _, f := range m.Facts {
		if f.Confirmed {
			out = append(out, f)
		}
	}
	return out
}

// nonDecisionFacts 过滤掉用户决定事实（用户决定单独成条目晋升）。
func nonDecisionFacts(facts []taskmem.Fact) []taskmem.Fact {
	out := make([]taskmem.Fact, 0, len(facts))
	for _, f := range facts {
		if strings.HasPrefix(f.Text, userDecisionPrefix) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// decisionEntries 把 confirmed 的用户决定事实逐条晋升为 KindDecision 条目。
// 键取内容哈希：不同任务的相同决定落同 Key，写入时自动 supersede 旧条目。
func decisionEntries(m *taskmem.TaskMemory, facts []taskmem.Fact) []Entry {
	var out []Entry
	for _, f := range facts {
		if !strings.HasPrefix(f.Text, userDecisionPrefix) {
			continue
		}
		out = append(out, promotionEntry(m, KindDecision,
			"decision:"+shortContentHash(f.Text), f.Text, factEvidence(f)))
	}
	return out
}

// constraintEntries 把任务约束逐条晋升为 KindConstraint 条目（内容哈希键，
// 同约束跨任务刷新时自动 supersede）。约束来自任务契约（capability/预期产物），
// 属控制面事实，证据记 status 引用。
func constraintEntries(m *taskmem.TaskMemory) []Entry {
	out := make([]Entry, 0, len(m.Constraints))
	for _, c := range m.Constraints {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		out = append(out, promotionEntry(m, KindConstraint,
			"constraint:"+shortContentHash(c), c,
			[]string{taskmem.EvidenceStatus + ":task_contract"}))
	}
	return out
}

// resultEvidence 汇总结果类条目的证据引用：confirmed 事实的证据 +
// 文件/产物版本证据。只含引用摘要，不含正文。
func resultEvidence(m *taskmem.TaskMemory, facts []taskmem.Fact) []string {
	out := fileEvidence(m)
	for _, f := range facts {
		out = append(out, factEvidence(f)...)
	}
	return dedupStrings(out)
}

// factEvidence 把单条事实的 EvidenceRef 拍平为 "kind:ref" 摘要串。
func factEvidence(f taskmem.Fact) []string {
	out := make([]string, 0, len(f.Evidence))
	for _, ref := range f.Evidence {
		out = append(out, evidenceRefString(ref))
	}
	return out
}

// fileEvidence 为文件/产物版本合成 file_effect 证据引用（带 hash 前 8 位）。
func fileEvidence(m *taskmem.TaskMemory) []string {
	out := make([]string, 0, len(m.Files))
	for _, fv := range m.Files {
		ref := taskmem.EvidenceFileEffect + ":" + fv.Path
		if fv.Hash != "" {
			hash := fv.Hash
			if len(hash) > 8 {
				hash = hash[:8]
			}
			ref += "@" + hash
		}
		out = append(out, ref)
	}
	return out
}

// evidenceRefString 渲染单条证据引用（digest 取前 8 位）。
func evidenceRefString(ref taskmem.EvidenceRef) string {
	s := ref.Kind + ":" + ref.Ref
	if ref.Digest != "" {
		d := ref.Digest
		if len(d) > 8 {
			d = d[:8]
		}
		s += "@" + d
	}
	return s
}

// shortContentHash 取内容 sha256 前 8 位 hex（内容寻址键）。
func shortContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:8]
}

// --- 正文渲染 helpers（按重要性排序写段：截断时尾部先丢） ---

func writeGoalLine(sb *strings.Builder, m *taskmem.TaskMemory) {
	if g := strings.TrimSpace(m.Goal); g != "" {
		sb.WriteString("目标: " + g + "\n")
	}
}

func writeStringLines(sb *strings.Builder, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	sb.WriteString(title + "\n")
	for _, l := range lines {
		sb.WriteString("- " + l + "\n")
	}
}

func writeFactLines(sb *strings.Builder, title string, facts []taskmem.Fact) {
	if len(facts) == 0 {
		return
	}
	sb.WriteString(title + "\n")
	for _, f := range facts {
		sb.WriteString("- " + f.Text + "\n")
	}
}

func writeFileLines(sb *strings.Builder, m *taskmem.TaskMemory) {
	if len(m.Files) == 0 {
		return
	}
	sb.WriteString("产物与文件版本:\n")
	for _, fv := range m.Files {
		line := "- " + fv.Path
		if fv.Hash != "" {
			hash := fv.Hash
			if len(hash) > 8 {
				hash = hash[:8]
			}
			line += " (hash:" + hash + ")"
		}
		sb.WriteString(line + "\n")
	}
}

func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// truncatePromotionRunes 按 rune 截断并追加省略标记（与 taskmem 同款手法）。
func truncatePromotionRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
