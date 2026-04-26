package agent

import (
	"strings"
	"testing"
)

// TestTruncateHistory_OverLimitGetsTruncated 主回归用例：
// 构造 prompt_tokens 显著超 contextLimit 的人造历史，断言：
//   - 返回的 history 短于输入
//   - 返回 history 的预测 tokens 满足 ≤ contextLimit（或返回 ErrContextLimitTooSmall）
//   - 头部第 0 条不丢
//   - 尾部 keepRecentForTruncate 条不丢
//
// 这条用例直接锁住 §11.7.4"S7 接通"不变量——下次有人误删 processTask 中的
// TruncateHistory 调用点，单测仍然会 pass（因为函数本身正确），但实际运行时
// 会回到死代码状态；故一并加 §11.7.4 的 trace KindHistoryTruncated 事件，
// 并配套 V12 端到端验证（见验证计划）。
//
// 2026-04-27 适配 keepRecentForTruncate 6→3：tail 块改为 hist[19..21] 共 3 条。
func TestTruncateHistory_OverLimitGetsTruncated(t *testing.T) {
	// 构造 22 条 history：第 0 条头部，第 1..15 条中段（被截断的目标），
	// 第 16 条带 PromptTokens=12000 作为实测锚（位于 lastIdx），
	// 第 17..21 条尾部（5 条 buffer，含 keepRecentForTruncate=3 的最后 3 条）。
	// 锚之后增量按 len/3 估算 ~600 tokens。预测 ≈ 12000 + 600 ≈ 12600。
	// contextLimit = 8000 → 必须截断。
	const model = "test-model"
	hist := make([]HistoryEntry, 22)
	hist[0] = HistoryEntry{IncomingMail: "<head>" + strings.Repeat("h", 500) + "</head>"}
	for i := 1; i <= 15; i++ {
		hist[i] = HistoryEntry{
			ToolCalled:       true,
			AssistantContent: "asst " + strings.Repeat("x", 200),
			ToolResults: []ToolResult{
				{ToolCallID: "tc", Content: strings.Repeat("y", 300)},
			},
		}
	}
	hist[16] = HistoryEntry{
		ToolCalled:       true,
		AssistantContent: "anchor " + strings.Repeat("a", 100),
		PromptTokens:     12000,
		Model:            model,
	}
	for i := 17; i <= 21; i++ {
		hist[i] = HistoryEntry{
			ToolCalled:       true,
			AssistantContent: "tail " + strings.Repeat("t", 200),
			ToolResults: []ToolResult{
				{ToolCallID: "tc", Content: strings.Repeat("z", 300)},
			},
		}
	}

	const contextLimit = 8000
	beforePredicted := PredictNextPromptTokens(hist, model, "", "")
	if beforePredicted <= contextLimit {
		t.Fatalf("测试预设无效：beforePredicted=%d 应当 > contextLimit=%d", beforePredicted, contextLimit)
	}

	truncated, err := TruncateHistory(hist, model, "", contextLimit)

	if len(truncated) >= len(hist) {
		t.Errorf("truncated 长度 %d 应当 < 原始长度 %d", len(truncated), len(hist))
	}

	afterPredicted := PredictNextPromptTokens(truncated, model, "", "")
	if err == nil && afterPredicted > contextLimit {
		t.Errorf("err==nil 但预测 tokens=%d > contextLimit=%d", afterPredicted, contextLimit)
	}
	if err != nil && err != ErrContextLimitTooSmall {
		t.Errorf("意外错误类型: %v", err)
	}

	// 头部不变（第 0 条）
	if truncated[0].IncomingMail == "" || !strings.Contains(truncated[0].IncomingMail, "<head>") {
		t.Errorf("第 0 条头部丢失：%+v", truncated[0])
	}

	// 尾部 keepRecentForTruncate 条仍在（按 AssistantContent 含 "tail " 前缀识别）
	tailStart := len(truncated) - keepRecentForTruncate
	if tailStart < 0 {
		t.Fatalf("truncated 长度 %d 比 keepRecentForTruncate=%d 还小", len(truncated), keepRecentForTruncate)
	}
	for i := tailStart; i < len(truncated); i++ {
		if !strings.HasPrefix(truncated[i].AssistantContent, "tail ") {
			t.Errorf("尾部第 %d 条（绝对索引 %d）不是预期的 'tail' 条目：AssistantContent=%q",
				i-tailStart, i, truncated[i].AssistantContent)
		}
	}
}

// TestTruncateHistory_NoOpWhenUnderLimit 反向断言：history 已在限制内时不动 history。
func TestTruncateHistory_NoOpWhenUnderLimit(t *testing.T) {
	hist := []HistoryEntry{
		{IncomingMail: "head"},
		{ToolCalled: true, AssistantContent: "x", PromptTokens: 100, Model: "m"},
		{ToolCalled: true, AssistantContent: "y"},
	}
	truncated, err := TruncateHistory(hist, "m", "", 50000)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if len(truncated) != len(hist) {
		t.Errorf("contextLimit 充裕时应不截断：原 %d → 现 %d", len(hist), len(truncated))
	}
}

// TestTruncateHistory_ContextLimitZeroIsNoOp ContextLimit<=0 应当 no-op
// （v3 兼容路径下 Agent.ContextLimit 未注入，此时整段不应触发截断）。
func TestTruncateHistory_ContextLimitZeroIsNoOp(t *testing.T) {
	hist := []HistoryEntry{
		{IncomingMail: "head"},
		{AssistantContent: strings.Repeat("x", 100000)}, // 大 content
	}
	truncated, err := TruncateHistory(hist, "m", "", 0)
	if err != nil {
		t.Fatalf("ContextLimit=0 不应当返回错误: %v", err)
	}
	if len(truncated) != len(hist) {
		t.Errorf("ContextLimit=0 应当 no-op，但 history 被截断了")
	}
}

// TestTruncateHistory_TooSmallReturnsErr 校验下界：history 已经只剩 head+tail
// 且所有 entry 的 AssistantContent 都很大（无 ToolResults 可缩减）时，
// Layer A 删完 middle、Layer B 没东西 shrink，最终返回 ErrContextLimitTooSmall。
//
// 注意 entry 形态：实际 agent.go 中 history append 路径下所有持久化 entry 都是
// ToolCalled=true（终止轮的 entry 不入 history，而是直接返回）。historyEntryRoughTokens
// 在 ToolCalled=true 分支下读 AssistantContent + ToolResults；ToolCalled=false 分支
// 读 Output（此分支主要服务于 IncomingMail 之外的退化遗留场景）。本测试构造 4 条
// ToolCalled=true + 大 AssistantContent 的 entry，模拟"已经被压缩到下界且无可缩减
// 的物理上限场景"。
//
// 2026-04-27 适配 keepRecentForTruncate 6→3：从 7 条改为 4 条（1 head + 3 tail）。
func TestTruncateHistory_TooSmallReturnsErr(t *testing.T) {
	// keepRecentForTruncate=3 → 至少留 1+3=4 条；构造 4 条且每条都很大但无 ToolResults
	hist := make([]HistoryEntry, 4)
	for i := range hist {
		hist[i] = HistoryEntry{
			ToolCalled:       true,
			AssistantContent: strings.Repeat("x", 30000), // 30000/3 ≈ 10000 tokens/entry
			// 无 ToolResults——Layer B 没东西可缩
		}
	}
	_, err := TruncateHistory(hist, "m", "", 1000)
	if err != ErrContextLimitTooSmall {
		t.Errorf("应当返回 ErrContextLimitTooSmall，实际: %v", err)
	}
}

// TestTruncateHistory_LayerB_ContentShrinkSucceeds 守 Layer B 的核心修复：
// 当 history 已经在下界（4 条）且仍超 contextLimit，但 tail entries 中含有
// 超大 ToolResult.Content 时，Layer B 应当对这些 result 做内容级缩减，
// 让最终预测 tokens 满足 contextLimit。
//
// 这是 2026-04-27 修复的核心场景——之前的 Layer A 只到这里就 ErrContextLimitTooSmall
// 短路返回了，trace 上看到 before==after。Layer B 的引入让算法在下界仍能继续降熵。
func TestTruncateHistory_LayerB_ContentShrinkSucceeds(t *testing.T) {
	// 4 条（已在下界）：1 head + 3 tail，每条 tail 含一条 5000 字符的 fat ToolResult
	// 5000 字符 ≈ 1666 tokens/entry，3 条 = ~5000 tokens
	// contextLimit = 3000 → Layer A 删不了（已在下界），必须靠 Layer B
	hist := []HistoryEntry{
		{IncomingMail: "<head>" + strings.Repeat("h", 100) + "</head>"},
	}
	for i := 0; i < 3; i++ {
		hist = append(hist, HistoryEntry{
			ToolCalled:       true,
			AssistantContent: "tail " + strings.Repeat("t", 50),
			ToolResults: []ToolResult{
				{ToolCallID: "tc", Content: strings.Repeat("Z", 5000)},
			},
		})
	}

	const contextLimit = 3000
	beforePredicted := PredictNextPromptTokens(hist, "m", "", "")
	if beforePredicted <= contextLimit {
		t.Fatalf("测试预设无效：beforePredicted=%d 应当 > contextLimit=%d", beforePredicted, contextLimit)
	}

	truncated, err := TruncateHistory(hist, "m", "", contextLimit)
	if err != nil {
		t.Fatalf("Layer B 应当能让历史满足 contextLimit，实际错误: %v", err)
	}

	afterPredicted := PredictNextPromptTokens(truncated, "m", "", "")
	if afterPredicted > contextLimit {
		t.Errorf("Layer B shrink 后仍超限：predicted=%d, contextLimit=%d", afterPredicted, contextLimit)
	}

	// 长度应当不变（Layer A 没动，Layer B 不删 entry，只缩 content）
	if len(truncated) != len(hist) {
		t.Errorf("Layer B 不应改变 entry 数量：原 %d → 现 %d", len(hist), len(truncated))
	}

	// 验证 tail entries 的 ToolResults.Content 真的被缩减过（而不是巧合达标）
	shrinkedAtLeastOne := false
	for i := 1; i < len(truncated); i++ {
		for _, r := range truncated[i].ToolResults {
			if strings.Contains(r.Content, "已截断") {
				shrinkedAtLeastOne = true
			}
		}
	}
	if !shrinkedAtLeastOne {
		t.Error("Layer B 应当至少缩减一个 fat ToolResult.Content，未发现 '已截断' 标记")
	}

	// 头部不能被缩（即使 head 含 ToolResults，shrinkLargeToolResults 从 protectedHead=1 起）
	if !strings.Contains(truncated[0].IncomingMail, "<head>") {
		t.Errorf("Layer B 不应触碰 head[0]，实际: %+v", truncated[0])
	}
}

// TestShrinkLargeToolResults_BasicShrink 单测 Layer B 的核心 helper：
// 单条超阈值 ToolResult 应当被缩减，标记格式正确。
func TestShrinkLargeToolResults_BasicShrink(t *testing.T) {
	original := strings.Repeat("X", 5000)
	e := HistoryEntry{
		ToolCalled: true,
		ToolResults: []ToolResult{
			{ToolCallID: "tc1", Content: original},
		},
	}
	out, ok := shrinkLargeToolResults(e, 2000, 1500, 200)
	if !ok {
		t.Fatal("应当返回 ok=true 表示触发了 shrink")
	}
	if len(out.ToolResults) != 1 {
		t.Fatalf("ToolResults 数量应不变，实际 %d", len(out.ToolResults))
	}
	got := out.ToolResults[0].Content
	if len(got) >= len(original) {
		t.Errorf("shrink 后长度 %d 应 < 原始 %d", len(got), len(original))
	}
	if !strings.Contains(got, "已截断") {
		t.Errorf("shrink 后应含 '已截断' 标记，实际: %q", got[:min(50, len(got))])
	}
	if !strings.HasPrefix(got, original[:1500]) {
		t.Error("shrink 后开头 1500 字符应保持不变")
	}
	if !strings.HasSuffix(got, original[len(original)-200:]) {
		t.Error("shrink 后结尾 200 字符应保持不变")
	}
	// ToolCallID 必须保留（OpenAI 协议配对）
	if out.ToolResults[0].ToolCallID != "tc1" {
		t.Errorf("ToolCallID 不应被改动，实际: %q", out.ToolResults[0].ToolCallID)
	}
}

// TestShrinkLargeToolResults_BelowThresholdNoOp 阈值以下的小 result 应当原样返回。
func TestShrinkLargeToolResults_BelowThresholdNoOp(t *testing.T) {
	small := strings.Repeat("Y", 1000) // < threshold=2000
	e := HistoryEntry{
		ToolCalled:  true,
		ToolResults: []ToolResult{{ToolCallID: "tc1", Content: small}},
	}
	out, ok := shrinkLargeToolResults(e, 2000, 1500, 200)
	if ok {
		t.Error("阈值以下不应触发 shrink，但 ok=true")
	}
	if out.ToolResults[0].Content != small {
		t.Error("阈值以下 Content 应原样保留")
	}
}

// TestShrinkLargeToolResults_MixedResults 混合大小 result：只缩大的，小的不动。
func TestShrinkLargeToolResults_MixedResults(t *testing.T) {
	big := strings.Repeat("B", 5000)
	small := strings.Repeat("S", 500)
	e := HistoryEntry{
		ToolCalled: true,
		ToolResults: []ToolResult{
			{ToolCallID: "big", Content: big},
			{ToolCallID: "small", Content: small},
			{ToolCallID: "big2", Content: big},
		},
	}
	out, ok := shrinkLargeToolResults(e, 2000, 1500, 200)
	if !ok {
		t.Fatal("含大 result 应当触发 shrink")
	}
	if len(out.ToolResults) != 3 {
		t.Fatalf("ToolResults 数量应保持 3，实际 %d", len(out.ToolResults))
	}
	// big / big2 应该被缩
	for _, idx := range []int{0, 2} {
		if !strings.Contains(out.ToolResults[idx].Content, "已截断") {
			t.Errorf("ToolResults[%d] (id=%s) 应被缩减", idx, out.ToolResults[idx].ToolCallID)
		}
	}
	// small 应保持原样
	if out.ToolResults[1].Content != small {
		t.Error("ToolResults[1] (small) 不应被改动")
	}
}

// TestShrinkLargeToolResults_NoToolCallEntry 非 ToolCalled entry 直接 no-op。
func TestShrinkLargeToolResults_NoToolCallEntry(t *testing.T) {
	e := HistoryEntry{
		IncomingMail: strings.Repeat("M", 10000),
	}
	out, ok := shrinkLargeToolResults(e, 2000, 1500, 200)
	if ok {
		t.Error("非 ToolCalled entry 不应触发 shrink")
	}
	if out.IncomingMail != e.IncomingMail {
		t.Error("IncomingMail 不应被改动")
	}
}

// TestPredictNextPromptTokens_AnchorAndAdded 验证锚 + 新增逻辑：
// 历史中已有 PromptTokens=5000 的锚，锚之后又加了 600 字符的内容；
// 预测应当 ≈ 5000 + 600/3 = 5200。
func TestPredictNextPromptTokens_AnchorAndAdded(t *testing.T) {
	hist := []HistoryEntry{
		{IncomingMail: "head"},
		{
			ToolCalled:       true,
			AssistantContent: "anchor",
			PromptTokens:     5000,
			Model:            "m",
		},
		{
			ToolCalled:       true,
			AssistantContent: strings.Repeat("a", 600),
			ToolResults:      []ToolResult{},
		},
	}
	predicted := PredictNextPromptTokens(hist, "m", "", "")
	// 锚=5000，锚之后增量 ≈ 600/3 = 200
	if predicted < 5100 || predicted > 5300 {
		t.Errorf("预测 tokens=%d 与预期 ~5200 偏差过大", predicted)
	}
}

// TestPredictNextPromptTokens_ModelSwitchResetsAnchor 模型切换后失效：
// 历史中有 PromptTokens=5000 但 Model="m1"，currentModel="m2"——应当退化到
// estimateFromScratch（不锚定 m1 的实测值）。
func TestPredictNextPromptTokens_ModelSwitchResetsAnchor(t *testing.T) {
	hist := []HistoryEntry{
		{
			ToolCalled:       true,
			AssistantContent: "x",
			PromptTokens:     5000,
			Model:            "m1",
		},
	}
	// currentModel="m2"
	predicted := PredictNextPromptTokens(hist, "m2", "system_prompt_text", "user")
	// 锚不可用 → estimateFromScratch
	// = len("system_prompt_text")/3 + entries/3 + len("user")/3 + 100
	// ≈ 6 + 0 + 1 + 100 ≈ 107
	if predicted > 200 {
		t.Errorf("跨模型应退化到粗略估算，但 predicted=%d 看起来用了 m1 的 5000 锚", predicted)
	}
}
