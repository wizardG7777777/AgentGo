package contextruntime_test

import (
	"strings"
	"testing"

	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
	"agentgo/internal/testmodel"
)

func TestOutputSnapshotReplayAndSlowSubscriber(t *testing.T) {
	s := contextruntime.NewOutputService(nil)
	id := llm.Identity{InvocationID: "one", SessionID: "session", AgentID: "agent"}
	o := contextruntime.WatchOptions{SessionID: "session", AgentID: "agent", Buffer: 1}
	ch, cancel, err := s.WatchModelOutput(o)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	initial := <-ch
	if initial.Kind != "snapshot" || initial.EventCursor == "" {
		t.Fatal("缺少初始快照或游标")
	}
	s.Start(id)
	s.Accept(llm.Event{InvocationID: "one", Kind: "text_delta", Delta: "你"})
	s.Accept(llm.Event{InvocationID: "one", Kind: "text_delta", Delta: "好"})
	reset := <-ch
	if reset.Kind != "resync" || len(reset.Snapshot) != 1 || reset.Snapshot[0].Text != "你好" {
		t.Fatalf("慢订阅者静默丢字: %+v", reset)
	}
	o.EventCursor = reset.EventCursor
	o.Buffer = 8
	s.Accept(llm.Event{InvocationID: "one", Kind: "text_delta", Delta: "！"})
	reconnected, stop, err := s.WatchModelOutput(o)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e := <-reconnected
	if e.Kind != "delta" || e.Delta.Delta != "！" {
		t.Fatalf("游标补发不正确: %+v", e)
	}
	o.AgentID = "other"
	if _, _, err = s.WatchModelOutput(o); err == nil {
		t.Fatal("接受跨范围游标")
	}
}

func TestOutputPersistsWithoutUIAndHidesReplayFromSubscription(t *testing.T) {
	var records []contextruntime.OutputRecord
	s := contextruntime.NewOutputService(func(r contextruntime.OutputRecord) error { records = append(records, r); return nil })
	id := llm.Identity{InvocationID: "one", SessionID: "session", AgentID: "agent"}
	s.Start(id)
	r, err := (testmodel.Fixture{Content: "完整结果"}).Seal(llm.ProtocolResponses)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(id, r, nil); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Result == nil {
		t.Fatal("无 UI 时未保存完整响应")
	}
	if err = s.Finish(id, r, nil); err == nil {
		t.Fatal("重复发布完成轮次")
	}
	view := s.Snapshot(contextruntime.WatchOptions{})
	if view[0].Result != nil {
		t.Fatal("UI 投影泄漏协议重放载荷")
	}
	newProcess := contextruntime.NewOutputService(nil)
	if err = newProcess.Restore(records); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newProcess.Snapshot(contextruntime.WatchOptions{})[0].Text, "完整结果") {
		t.Fatal("重启后完整文本丢失")
	}
}
