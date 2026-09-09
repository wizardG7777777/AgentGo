package tools

import (
	"context"
	"encoding/json"
	"testing"

	"agentgo/internal/mailbox"
)

func TestSendMessageInformationReceiptAndControlRejection(t *testing.T) {
	reg := mailbox.NewRegistry(8)
	reg.Register("sender", "")
	box := reg.Register("receiver", "")
	g := CommunicationGroup{MBRegistry: reg, AgentID: "sender"}
	for _, args := range []map[string]any{
		{"to": "receiver", "content": "通知", "priority": "high"},
		{"to": "receiver", "content": "通知", "msg_type": "steer"},
		{"to": "receiver", "content": "回复", "msg_type": "reply"},
	} {
		if _, err := g.sendMessage(context.Background(), args); err == nil {
			t.Fatalf("非法控制或缺少关联的参数应拒绝：%v", args)
		}
	}
	if box.Len() != 0 {
		t.Fatal("非法消息不应发生投递")
	}
	out, err := g.sendMessage(context.Background(), map[string]any{
		"to": "receiver", "content": "调查发现", "msg_type": "question",
	})
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		ID     string `json:"message_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatal(err)
	}
	msgs := box.Drain()
	if len(msgs) != 1 || !msgs[0].DeliveryOnly || msgs[0].ID != receipt.ID || receipt.ID == "" || receipt.Status != "delivered" {
		t.Fatalf("投递事实与回执不一致：%s %v", out, msgs)
	}
}
