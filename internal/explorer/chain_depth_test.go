package explorer

import (
	"context"
	"sync"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

// ================================================================
// ⚠️  2026-04-20 回归锁（预期红态 —— 请勿删除断言！）
// ================================================================
//
// 本文件下列测试当前**故意失败**，用于锁定 P1-2 "Mail chain_depth 全程为 0" 缺陷：
// 在修复完成前它应保持红灯。如果 CI 报此测试失败，**不是回归**，这是提醒 bug
// 还没修。修复路径：给 Explorer 的 MetaGroup 注入 Store + Holder（不再以
// Store 的 nil 判断 publish_task 注册与否 —— 工具权限应由白名单或专门的
// capability 位控制，而非和 Store 耦合），让 send_message 能读到当前任务的
// MailChainDepth 并递增。
//
// ❌ 错误处理：删除断言 / 改 Skip / 调整期望值 —— 这样会掩盖 bug 信号
// ✅ 正确处理：修 internal/explorer/explorer.go:81 的 MetaGroup 构造，此处自动变绿
//
// 背景（bug 现象）：2026-04-20 并发测试中 40+ 次邮件唤醒任务 chain_depth 全为 0，
// 即使 explorer 连续多次发 reply 类消息也无一例递增。ChainDepthLimitHook（max=3）
// 在 Explorer 收发路径上从未被触发过 Abort —— 之前"已修复的邮件级联爆炸"可能仅
// 凭 prompt 削弱止血，hook 形同 dead code。
//
// 参见：docs/activate/KNOWN_ISSUES.md "Mail chain_depth 全程为 0，ChainDepthLimit 效果存疑"
// ================================================================

// TestExplorerSendMessage_InheritsChainDepthFromCurrentTask 验证 Explorer
// 处理 MailChainDepth=2 的任务并调 send_message 时，outgoing ChainDepth=3。
//
// 当前行为（bug）：outgoing ChainDepth=0，因为 MetaGroup 缺 Holder/Store。
// 期望行为：ChainDepth=parent.MailChainDepth+1=3，与 Worker 一致。
func TestExplorerSendMessage_InheritsChainDepthFromCurrentTask(t *testing.T) {
	eventCh := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(eventCh, 100, 2, 300)
	r := roster.NewMemoryRoster()
	cfg := config.DefaultConfig()

	mbReg := mailbox.NewRegistry(8)
	recvBox := mbReg.Register("worker-1", "")

	targetTask := &model.Task{
		Description:    "发一条消息给 worker-1",
		EventType:      cfg.ExplorerEventType,
		MailChainDepth: 2,
	}
	if err := s.PublishTask(targetTask); err != nil {
		t.Fatalf("publish task: %v", err)
	}

	// 第一次 LLM 响应：调 send_message；第二次：无工具调用，Agent 自然 finalize
	mock := &chainDepthLLMClient{
		responses: []llm.Response{
			{
				ToolCalls: []llm.ToolCall{{
					ID:   "call-1",
					Name: "send_message",
					Arguments: map[string]any{
						"to":       "worker-1",
						"content":  "chain_depth 继承回归测试",
						"msg_type": "info",
					},
				}},
				FinishReason: llm.FinishReasonToolCalls,
			},
			{Content: "done"},
		},
	}

	exp := New(s, r, mock, cfg, nil, mbReg, nil, nil, nil, nil, nil, nil, nil)
	exp.agent.PollInterval = 10 * time.Millisecond
	exp.agent.IdleThreshold = 3

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		exp.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := s.GetTask(targetTask.ID)
		if task != nil && (task.Status == model.TaskStatusCompleted || task.Status == model.TaskStatusFailed) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("explorer did not stop after cancel")
	}

	msgs := recvBox.Drain()
	if len(msgs) != 1 {
		t.Fatalf("worker-1 应收到 1 条消息，实际: %d", len(msgs))
	}
	if msgs[0].ChainDepth != 3 {
		t.Errorf("ChainDepth = %d, want 3 (parent.MailChainDepth=2 + 1)；"+
			"Explorer MetaGroup 缺 Store/Holder 导致继承失败，见 KNOWN_ISSUES.md 2026-04-20",
			msgs[0].ChainDepth)
	}
}

// chainDepthLLMClient 按顺序吐出预设响应；耗尽后返回空 content 让 Agent finalize。
type chainDepthLLMClient struct {
	mu        sync.Mutex
	responses []llm.Response
	callIndex int
}

func (m *chainDepthLLMClient) Chat(ctx context.Context, messages []llm.Message, tools []llm.ToolDef) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.callIndex < len(m.responses) {
		resp := m.responses[m.callIndex]
		m.callIndex++
		return resp, nil
	}
	return llm.Response{Content: "done"}, nil
}
