package memory

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/taskmem"
)

// newSealedTaskMem 构造一份带典型内容的终态 Task Memory：
// confirmed 事实（含一条用户决定）、inferred 事实、文件版本、失败尝试、
// 阻塞、下一步候选与任务约束。
func newSealedTaskMem(taskID string) *taskmem.TaskMemory {
	m := taskmem.New(taskID)
	m.Goal = "写月度报告"
	m.Constraints = []string{"预期产物: docs/report.md"}
	m.Actions = []taskmem.ActionRecord{
		{Caption: "write_file docs/report.md", Evidence: taskmem.EvidenceRef{Kind: taskmem.EvidenceToolResult, Ref: "write_file docs/report.md"}},
	}
	m.Facts = []taskmem.Fact{
		{
			Text:      "报告已写入 docs/report.md",
			Confirmed: true,
			Evidence:  []taskmem.EvidenceRef{{Kind: taskmem.EvidenceFileEffect, Ref: "docs/report.md", Digest: "abcdef0123456789"}},
			UpdatedAt: time.Now(),
		},
		{
			Text:      "用户决定: 采用简洁版格式",
			Confirmed: true,
			Evidence:  []taskmem.EvidenceRef{{Kind: taskmem.EvidenceUser, Ref: "request_user_input"}},
			UpdatedAt: time.Now(),
		},
		{
			// inferred：模型自称"测试通过"但无证据——任何终态都不得晋升。
			Text:      "测试应该通过了",
			Confirmed: false,
			UpdatedAt: time.Now(),
		},
	}
	m.Files = []taskmem.FileVersion{
		{Path: "docs/report.md", Hash: "abcdef0123456789", UpdatedAt: time.Now()},
	}
	m.Failures = []string{"run_shell 命令失败 (exit=1): go test — build failed"}
	m.Blockers = []string{"write_file docs/report.md — 文件被占用"}
	m.NextCandidates = []string{"解除文件占用后重写"}
	m.Sealed = true
	return m
}

// findCandidate 按 Kind 找候选条目。
func findCandidate(cands []Entry, kind Kind) *Entry {
	for i := range cands {
		if cands[i].Kind == kind {
			return &cands[i]
		}
	}
	return nil
}

func findCandidates(cands []Entry, kind Kind) []Entry {
	var out []Entry
	for _, c := range cands {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func TestPromotion_Completed_PromotesConfirmedResultsDecisionsConstraints(t *testing.T) {
	m := newSealedTaskMem("task-1")
	cands := BuildPromotionCandidates(m, TerminalCompleted)

	result := findCandidate(cands, KindResult)
	if result == nil {
		t.Fatal("completed 应晋升 KindResult 结果条目")
	}
	if result.Key != "result:task-1" {
		t.Errorf("结果条目 Key = %q, want result:task-1", result.Key)
	}
	if result.EffectiveState() != StateConfirmed {
		t.Errorf("结果条目 State = %q, want confirmed", result.EffectiveState())
	}
	for _, want := range []string{"报告已写入 docs/report.md", "docs/report.md", "写月度报告"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("结果条目正文应含 %q: %q", want, result.Content)
		}
	}
	// 证据纪律：flatten 后的证据引用含 file_effect 摘要；inferred 事实绝不出现。
	if !strings.Contains(strings.Join(result.Evidence, " "), "file_effect:docs/report.md") {
		t.Errorf("结果条目证据应含 file_effect 引用: %v", result.Evidence)
	}
	if strings.Contains(result.Content, "测试应该通过了") {
		t.Errorf("inferred 事实不得晋升为 confirmed 条目: %q", result.Content)
	}

	decisions := findCandidates(cands, KindDecision)
	if len(decisions) != 1 {
		t.Fatalf("completed 应晋升 1 条用户决定, got %d", len(decisions))
	}
	if !strings.Contains(decisions[0].Content, "用户决定: 采用简洁版格式") {
		t.Errorf("决定条目正文 = %q", decisions[0].Content)
	}
	if !strings.HasPrefix(decisions[0].Key, "decision:") {
		t.Errorf("决定条目 Key = %q, want decision: 前缀", decisions[0].Key)
	}

	constraints := findCandidates(cands, KindConstraint)
	if len(constraints) != 1 || !strings.Contains(constraints[0].Content, "预期产物") {
		t.Errorf("completed 应晋升仍适用约束: %+v", constraints)
	}
}

func TestPromotion_Blocked_PromotesBlockersWithoutClaimingDone(t *testing.T) {
	m := newSealedTaskMem("task-2")
	cands := BuildPromotionCandidates(m, TerminalBlocked)

	if len(cands) != 1 {
		t.Fatalf("blocked 应晋升恰好 1 条, got %d: %+v", len(cands), cands)
	}
	e := cands[0]
	if e.Kind != KindBlocker || e.Key != "blocked:task-2" {
		t.Errorf("阻塞条目 Kind/Key = %s/%s", e.Kind, e.Key)
	}
	for _, want := range []string{"文件被占用", "已尝试", "解除文件占用后重写", "报告已写入"} {
		if !strings.Contains(e.Content, want) {
			t.Errorf("阻塞条目正文应含 %q: %q", want, e.Content)
		}
	}
	// 不得宣称任务完成 + inferred 事实不得出现。
	if !strings.Contains(e.Content, "未完成") {
		t.Errorf("blocked 条目必须显式标注未完成: %q", e.Content)
	}
	if strings.Contains(e.Content, "测试应该通过了") {
		t.Errorf("inferred 事实不得晋升: %q", e.Content)
	}
}

func TestPromotion_Failed_PromotesOnlyFailureEvidence(t *testing.T) {
	m := newSealedTaskMem("task-3")
	cands := BuildPromotionCandidates(m, TerminalFailed)

	if len(cands) != 1 {
		t.Fatalf("failed 应晋升恰好 1 条, got %d", len(cands))
	}
	e := cands[0]
	if e.Kind != KindLearning || e.Key != "failure:task-3" {
		t.Errorf("失败条目 Kind/Key = %s/%s", e.Kind, e.Key)
	}
	if !strings.Contains(e.Content, "exit=1") || !strings.Contains(e.Content, "避免重复失败") {
		t.Errorf("失败条目应含可复现失败证据与避免重复失败信息: %q", e.Content)
	}
	if strings.Contains(e.Content, "测试应该通过了") {
		t.Errorf("inferred 事实不得晋升: %q", e.Content)
	}

	// 无失败证据的 failed 任务：无候选。
	m2 := taskmem.New("task-x")
	m2.Sealed = true
	if got := BuildPromotionCandidates(m2, TerminalFailed); len(got) != 0 {
		t.Errorf("无失败证据的 failed 任务不应有晋升候选: %+v", got)
	}
}

func TestPromotion_Cancelled_KeepsOnlyEffectsAndDecisions(t *testing.T) {
	m := newSealedTaskMem("task-4")
	cands := BuildPromotionCandidates(m, TerminalCancelled)

	effects := findCandidate(cands, KindResult)
	if effects == nil {
		t.Fatal("cancelled 应保留已发生的权威 Effect（文件版本）")
	}
	if effects.Key != "effects:task-4" {
		t.Errorf("Effect 条目 Key = %q", effects.Key)
	}
	if !strings.Contains(effects.Content, "docs/report.md") || !strings.Contains(effects.Content, "已取消") {
		t.Errorf("Effect 条目正文 = %q", effects.Content)
	}
	// 中间推断（过程记录）不得晋升：正文不得含失败尝试与阻塞。
	if strings.Contains(effects.Content, "exit=1") || strings.Contains(effects.Content, "文件被占用") {
		t.Errorf("cancelled 不得晋升中间过程记录: %q", effects.Content)
	}
	// 明确用户决定保留；除此之外不得有别的条目（无约束/无阻塞/无失败）。
	decisions := findCandidates(cands, KindDecision)
	if len(decisions) != 1 {
		t.Errorf("cancelled 应保留 1 条明确用户决定, got %d", len(decisions))
	}
	if len(cands) != 2 {
		t.Errorf("cancelled 候选总数 = %d, want 2（Effect + 决定）: %+v", len(cands), cands)
	}

	// 取消前无任何 Effect 与决定：无候选。
	m2 := taskmem.New("task-y")
	m2.Sealed = true
	if got := BuildPromotionCandidates(m2, TerminalCancelled); len(got) != 0 {
		t.Errorf("空 cancelled 任务不应有晋升候选: %+v", got)
	}
}

func TestPromotion_EvidenceDiscipline_InferredNeverPromoted(t *testing.T) {
	// inferred 事实即使带证据引用，CM3 也一律丢弃（不允许借 inferred
	// 条目晋升——设计决议见 promotion.go 文件头注释）。
	m := taskmem.New("task-5")
	m.Facts = []taskmem.Fact{
		{
			Text:      "猜测：配置在 etc/config.yaml",
			Confirmed: false,
			Evidence:  []taskmem.EvidenceRef{{Kind: taskmem.EvidenceToolResult, Ref: "read_file etc/config.yaml"}},
		},
	}
	m.Sealed = true
	for _, status := range []string{TerminalCompleted, TerminalBlocked, TerminalFailed, TerminalCancelled} {
		for _, c := range BuildPromotionCandidates(m, status) {
			if strings.Contains(c.Content, "猜测") {
				t.Errorf("终态 %s 不得晋升 inferred 事实: %+v", status, c)
			}
			if c.EffectiveState() == StateInferred {
				t.Errorf("终态 %s 的晋升条目不得为 inferred: %+v", status, c)
			}
		}
	}
}

func TestPromotion_DecisionKeyIsContentAddressed(t *testing.T) {
	// 不同任务的相同用户决定 → 同 Key（写入时经 Supersede 刷新取代）。
	build := func(taskID string) []Entry {
		m := taskmem.New(taskID)
		m.Facts = []taskmem.Fact{
			{Text: "用户决定: 采用简洁版格式", Confirmed: true,
				Evidence: []taskmem.EvidenceRef{{Kind: taskmem.EvidenceUser, Ref: "request_user_input"}}},
		}
		m.Sealed = true
		return BuildPromotionCandidates(m, TerminalCompleted)
	}
	d1 := findCandidate(build("task-a"), KindDecision)
	d2 := findCandidate(build("task-b"), KindDecision)
	if d1 == nil || d2 == nil {
		t.Fatal("两任务都应晋升用户决定条目")
	}
	if d1.Key != d2.Key {
		t.Errorf("相同决定的 Key 应一致（内容寻址）: %q vs %q", d1.Key, d2.Key)
	}
	if d1.Source != "task-a" || d2.Source != "task-b" {
		t.Errorf("决定条目 Source 应各自记任务 ID: %q / %q", d1.Source, d2.Source)
	}
}

func TestPromotion_UnknownTerminalReturnsNil(t *testing.T) {
	m := newSealedTaskMem("task-6")
	if got := BuildPromotionCandidates(m, "processing"); got != nil {
		t.Errorf("非终态不得产生候选: %+v", got)
	}
	if got := BuildPromotionCandidates(nil, TerminalCompleted); got != nil {
		t.Errorf("nil Task Memory 不得产生候选: %+v", got)
	}
}

func TestPromotion_ContentBudgetCapped(t *testing.T) {
	m := taskmem.New("task-7")
	m.Goal = strings.Repeat("长", 100)
	for i := 0; i < taskmem.MaxFacts; i++ {
		m.Facts = append(m.Facts, taskmem.Fact{
			Text: strings.Repeat("长事实", 40), Confirmed: true,
			Evidence: []taskmem.EvidenceRef{{Kind: taskmem.EvidenceToolResult, Ref: "tool"}},
		})
	}
	m.Sealed = true
	for _, c := range BuildPromotionCandidates(m, TerminalCompleted) {
		if got := len([]rune(c.Content)); got > promotionContentMaxRunes {
			t.Errorf("条目 %s 正文 %d runes 超预算 %d", c.Key, got, promotionContentMaxRunes)
		}
	}
}
