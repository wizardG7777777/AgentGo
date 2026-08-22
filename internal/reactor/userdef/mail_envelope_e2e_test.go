package userdef

import (
	"path/filepath"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/trace"
)

func TestTaskPublishedReactorMailGetsWriterSessionAndRunEnvelope(t *testing.T) {
	mail := mailbox.NewRegistry(8)
	receiver := mail.Register("receiver", "")
	rs, err := Load([]byte(`
reactors:
  - name: published-mail
    on: task_published
    call: send_message
    args: {to: receiver, content: "new task", type: steer, priority: high}
`), ".", "", Deps{Mailbox: mail})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := reactor.NewRegistry()
	if err := dispatcher.Register(rs[0]); err != nil {
		t.Fatal(err)
	}
	writer, err := trace.NewWriter(filepath.Join(t.TempDir(), "traces"), 0)
	if err != nil {
		t.Fatal(err)
	}
	writer.SetSessionID("session-reactor")
	oldWriter, oldDispatcher := trace.Default(), trace.DefaultDispatcher()
	trace.SetDefault(writer)
	trace.SetDefaultDispatcher(dispatcher)
	t.Cleanup(func() {
		dispatcher.Quiesce(time.Second)
		trace.SetDefault(oldWriter)
		trace.SetDefaultDispatcher(oldDispatcher)
		writer.Close()
	})

	tasks := store.NewMemoryTaskStore(nil, 8, 1, 60)
	source := &model.Task{ID: "reactor-source", Description: "source"}
	if err := taskcontract.Start(source, loopcontract.WorkInvestigation, "test-reactor-mail/v1",
		time.Hour, 5*time.Minute, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := tasks.PublishTask(source); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		msgs := receiver.DrainRunWithAck(nil, source.RunID, "session-reactor", "consumer")
		if len(msgs) == 1 {
			if msgs[0].SourceTaskID != source.ID || msgs[0].RunID != source.RunID || msgs[0].SessionID != "session-reactor" {
				t.Fatalf("reactor mail correlation 错误: %+v", msgs[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("1s 内未收到带 Run/Session envelope 的 reactor mail")
}
