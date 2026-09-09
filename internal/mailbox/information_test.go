package mailbox

import (
	"testing"
	"time"

	"agentgo/internal/store"
)

func TestInformationNeverWakesAfterSnapshotRestore(t *testing.T) {
	reg := NewRegistry(8)
	reg.Register("sender", "")
	reg.Register("receiver", "")
	for _, kind := range []string{MsgTypeInfo, MsgTypeQuestion, MsgTypeReply} {
		if err := reg.Send(Message{ID: kind, ReplyTo: "original", DeliveryOnly: true,
			From: "sender", To: "receiver", Type: kind, Priority: PriorityHigh,
			Content: "仅供下次执行时阅读", SentAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	restored := NewRegistry(8)
	if err := restored.ImportSnapshot(reg.ExportSnapshot()); err != nil {
		t.Fatal(err)
	}
	tasks := store.NewMemoryTaskStore(nil, 32, 2, 300)
	notifier := NewMailNotifier(restored, tasks, time.Second)
	notifier.scan()
	notifier.scan()
	all, err := tasks.ScanAll()
	if err != nil || len(all) != 0 {
		t.Fatalf("普通消息不得产生唤醒任务：%v %v", all, err)
	}
	box, exists := restored.lookup("receiver")
	if !exists {
		t.Fatal("恢复后收件箱缺失")
	}
	msgs := box.DrainWithAck(restored)
	if len(msgs) != 3 {
		t.Fatalf("普通消息必须保留至正常读取：%v", msgs)
	}
	for _, msg := range msgs {
		if !msg.DeliveryOnly || msg.ID == "" || msg.ReplyTo != "original" {
			t.Fatalf("恢复丢失信息投递契约：%+v", msg)
		}
	}
	if sender, ok := restored.lookup("sender"); !ok || sender.Len() != 0 {
		t.Fatal("普通问题消息不应生成自动回复")
	}
}
