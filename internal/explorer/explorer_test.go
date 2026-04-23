package explorer

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// mockLLMClient 用于测试的 LLM mock。
type mockLLMClient struct {
	responses []llm.Response
	callIndex int
}

func (m *mockLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	if m.callIndex < len(m.responses) {
		resp := m.responses[m.callIndex]
		m.callIndex++
		return resp, nil
	}
	return llm.Response{Content: "done"}, nil
}

func setup() (store.TaskStore, roster.Roster, chan model.Event) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	r := roster.NewMemoryRoster()
	return s, r, ch
}

func TestExplorer_OnlyClaimsExploreEvents(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()

	mock := &mockLLMClient{
		responses: []llm.Response{{Content: "调查结果"}},
	}

	exploreTask := &model.Task{Description: "调查文件结构", EventType: "explore"}
	s.PublishTask(exploreTask)
	codeTask := &model.Task{Description: "写代码", EventType: "code"}
	s.PublishTask(codeTask)

	exp := New(s, r, mock, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 5

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	exp.Run(ctx)

	got, _ := s.GetTask(exploreTask.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("explore task status = %s, want completed", got.Status)
	}

	got2, _ := s.GetTask(codeTask.ID)
	if got2.Status != model.TaskStatusPending {
		t.Errorf("code task status = %s, want pending", got2.Status)
	}
}

func TestExplorer_UsesReadOnlyTools(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()

	mock := &mockLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{
					{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "nonexistent.txt"}},
				},
			},
			{Content: "文件不存在"},
		},
	}

	task := &model.Task{Description: "检查文件", EventType: "explore"}
	s.PublishTask(task)

	exp := New(s, r, mock, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 3

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	exp.Run(ctx)

	got, _ := s.GetTask(task.ID)
	if got.Status != model.TaskStatusCompleted {
		t.Errorf("task status = %s, want completed", got.Status)
	}
}

func TestExplorer_ContextCancellation(t *testing.T) {
	s, r, _ := setup()
	cfg := config.DefaultConfig()
	mock := &mockLLMClient{}

	exp := New(s, r, mock, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	exp.agent.PollInterval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		exp.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("explorer did not stop after context cancellation")
	}
}

// TestExplorerSystemPrompt_ContainsReportDoneBudgetGuidance 是 2026-04-23
// 随机测试暴露的 P0 "Explorer 无 report_done 预算感知"回归锁。
//
// 现象：用户请求一个开放式调查（"调查...你可以启动多个代理..."），explorer
// 连续 9+ 轮 read_file 直到 watchdog 5m30s 超时强杀，全程从未调 report_done。
// Trace 显示 52 分钟内 7 个连续发布的子任务全部按这个剧本死于 timeout。
//
// 根因：explorer 的 system prompt 没有"预算感知"指引——没告诉 LLM
// "loop 接近上限时应停止探索立即 report_done 汇报当前已掌握的信息"，
// 也没告诉它"被 watchdog kill 会导致全部工作丢失，report_done 是唯一产出通道"。
//
// 本测试在修复落地前 🔴 RED：断言 systemPrompt 包含"report_done"+"预算/上限/时间"
// 相关词，以及"被 watchdog/超时 kill 会丢失"这类后果警示。
//
// 修复方向（任选其一或组合）：
//  1. Prompt 补一段："当接近 MaxLoops 或感觉 context 快满时，**立即停止探索**，
//     调 report_done 汇报已掌握的信息 —— 即使不完整。被超时 kill 会丢失全部结果。"
//  2. 新增 AgentHook（PhaseLoopPre）在 loop >= MaxLoops*0.7 时注入预算警告
//     IncomingMail（更硬，不靠 LLM 自觉）
//
// 修复后把 t.Fatal 改为 t.Log 记录版本即可自然转绿。
func TestExplorerSystemPrompt_ContainsReportDoneBudgetGuidance(t *testing.T) {
	// 关键词组：每组至少匹配 1 个才算合格
	budgetSignals := []string{"预算", "上限", "接近", "即将", "时间有限", "MaxLoops", "超时"}
	reportSignals := []string{"report_done"}
	consequenceSignals := []string{"丢失", "被杀", "watchdog", "timeout", "超时 kill", "没有产出"}

	hasAny := func(keywords []string) (string, bool) {
		for _, k := range keywords {
			if strings.Contains(systemPrompt, k) {
				return k, true
			}
		}
		return "", false
	}

	if _, ok := hasAny(reportSignals); !ok {
		t.Fatalf("Explorer systemPrompt 必须提及 report_done（目前 explorer 对 report_done 的时机没有任何指引）。见 KNOWN_ISSUES.md 2026-04-23 P0")
	}
	if _, ok := hasAny(budgetSignals); !ok {
		t.Fatalf("Explorer systemPrompt 缺少预算感知指引（期望含 %v 之一）——LLM 会一直读文件到 context 溢出或 watchdog 超时。见 KNOWN_ISSUES.md 2026-04-23 P0", budgetSignals)
	}
	if _, ok := hasAny(consequenceSignals); !ok {
		t.Fatalf("Explorer systemPrompt 缺少后果警示（期望含 %v 之一）——LLM 不知道 `被 kill 就丢失全部工作` 的代价，倾向无限探索。见 KNOWN_ISSUES.md 2026-04-23 P0", consequenceSignals)
	}
}
