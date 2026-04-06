package worker

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/mailbox"
)

func TestMakeSendMessageTool_PointToPoint(t *testing.T) {
	reg := mailbox.NewRegistry(8)
	reg.Register("worker-1", "")
	target := reg.Register("worker-2", "")

	tool := MakeSendMessageTool(reg, "worker-1")
	result, err := tool(context.Background(), map[string]any{
		"to":      "worker-2",
		"content": "请先跑测试",
	})
	if err != nil {
		t.Fatalf("send_message should succeed: %v", err)
	}
	if !strings.Contains(result, "消息已发送给 worker-2") {
		t.Fatalf("unexpected result: %q", result)
	}

	msgs := target.Drain()
	if len(msgs) != 1 {
		t.Fatalf("target mailbox message count = %d, want 1", len(msgs))
	}
	if msgs[0].From != "worker-1" {
		t.Errorf("message.From = %q, want %q", msgs[0].From, "worker-1")
	}
	if msgs[0].To != "worker-2" {
		t.Errorf("message.To = %q, want %q", msgs[0].To, "worker-2")
	}
	if msgs[0].Content != "请先跑测试" {
		t.Errorf("message.Content = %q, want %q", msgs[0].Content, "请先跑测试")
	}
}

func TestMakeSendMessageTool_BroadcastSkipsSender(t *testing.T) {
	reg := mailbox.NewRegistry(8)
	sender := reg.Register("worker-1", "")
	receiverA := reg.Register("worker-2", "")
	receiverB := reg.Register("explorer-1", "explore")

	tool := MakeSendMessageTool(reg, "worker-1")
	result, err := tool(context.Background(), map[string]any{
		"to":      "*",
		"content": "全员同步进度",
	})
	if err != nil {
		t.Fatalf("broadcast send_message should succeed: %v", err)
	}
	if !strings.Contains(result, "消息已广播给所有代理") {
		t.Fatalf("unexpected result: %q", result)
	}

	if msgs := sender.Drain(); len(msgs) != 0 {
		t.Fatalf("sender should not receive own broadcast, got %d messages", len(msgs))
	}
	if msgs := receiverA.Drain(); len(msgs) != 1 || msgs[0].Content != "全员同步进度" {
		t.Fatalf("worker-2 should receive broadcast once, got: %+v", msgs)
	}
	if msgs := receiverB.Drain(); len(msgs) != 1 || msgs[0].Content != "全员同步进度" {
		t.Fatalf("explorer-1 should receive broadcast once, got: %+v", msgs)
	}
}
