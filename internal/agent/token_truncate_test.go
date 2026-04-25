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
func TestTruncateHistory_OverLimitGetsTruncated(t *testing.T) {
	// 构造 22 条 history：第 0 条头部，第 1..14 条中段（被截断的目标），
	// 第 15 条带 PromptTokens=12000 作为实测锚（位于 lastIdx），
	// 第 16..21 条尾部（6 条，对应 keepRecentForTruncate=6）。
	// 锚之后增量按 len/3 估算 ~600 tokens。预测 ≈ 12000 + 600 ≈ 12600。
	// contextLimit = 8000 → 必须截断。
	const model = "test-model"
	hist := make([]HistoryEntry, 22)
	hist[0] = HistoryEntry{IncomingMail: "<head>" + strings.Repeat("h", 500) + "</head>"}
	for i := 1; i <= 14; i++ {
		hist[i] = HistoryEntry{
			ToolCalled:       true,
			AssistantContent: "asst " + strings.Repeat("x", 200),
			ToolResults: []ToolResult{
				{ToolCallID: "tc", Content: strings.Repeat("y", 300)},
			},
		}
	}
	hist[15] = HistoryEntry{
		ToolCalled:       true,
		AssistantContent: "anchor " + strings.Repeat("a", 100),
		PromptTokens:     12000,
		Model:            model,
	}
	for i := 16; i <= 21; i++ {
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
// 但仍超 contextLimit 时返回 ErrContextLimitTooSmall。
//
// 注意 entry 形态：实际 agent.go 中 history append 路径下所有持久化 entry 都是
// ToolCalled=true（终止轮的 entry 不入 history，而是直接返回）。historyEntryRoughTokens
// 在 ToolCalled=true 分支下读 AssistantContent + ToolResults；ToolCalled=false 分支
// 读 Output（此分支主要服务于 IncomingMail 之外的退化遗留场景）。本测试构造 7 条
// ToolCalled=true 大 entry，模拟"已经被压缩到下界但仍超限"的物理上限场景。
func TestTruncateHistory_TooSmallReturnsErr(t *testing.T) {
	// keepRecentForTruncate=6 → 至少留 1+6=7 条；构造 7 条且每条都很大
	hist := make([]HistoryEntry, 7)
	for i := range hist {
		hist[i] = HistoryEntry{
			ToolCalled:       true,
			AssistantContent: strings.Repeat("x", 30000), // 30000/3 ≈ 10000 tokens/entry
		}
	}
	_, err := TruncateHistory(hist, "m", "", 1000)
	if err != ErrContextLimitTooSmall {
		t.Errorf("应当返回 ErrContextLimitTooSmall，实际: %v", err)
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
