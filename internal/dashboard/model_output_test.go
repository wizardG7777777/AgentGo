package dashboard

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
)

type modelObserver struct {
	*fakeObserver
	output *contextruntime.OutputService
}

func (o *modelObserver) WatchModelOutput(options contextruntime.WatchOptions) (<-chan contextruntime.OutputEvent, func(), error) {
	return o.output.WatchModelOutput(options)
}
func readModelFrame(t *testing.T, reader *bufio.Reader) (string, contextruntime.OutputEvent) {
	t.Helper()
	id := ""
	data := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSpace(line)
		if line == "" && data != "" {
			break
		}
		if strings.HasPrefix(line, "id:") {
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		}
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	var event contextruntime.OutputEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		t.Fatal(err)
	}
	if id == "" || id != event.EventCursor {
		t.Fatal("SSE id 与流式事件游标不一致")
	}
	return id, event
}

func TestModelOutputSSEUsesL2CursorAndRecoversAfterDisconnect(t *testing.T) {
	service := contextruntime.NewOutputService(nil)
	server := NewServer(&modelObserver{fakeObserver: &fakeObserver{}, output: service}, "127.0.0.1:0", "")
	httpServer := httptest.NewServer(http.HandlerFunc(server.handleModelOutputEvents))
	defer httpServer.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(httpServer.URL + "?session_id=session&agent_id=agent")
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	_, first := readModelFrame(t, reader)
	if first.Kind != "snapshot" {
		t.Fatal("首次订阅缺少快照")
	}
	id := llm.Identity{InvocationID: "invocation", SessionID: "session", AgentID: "agent"}
	service.Start(id)
	_, started := readModelFrame(t, reader)
	if started.Kind != "started" {
		t.Fatal("缺少开始事件")
	}
	service.Accept(llm.Event{InvocationID: id.InvocationID, Kind: "text_delta", Delta: "你好"})
	eventCursor, delta := readModelFrame(t, reader)
	if delta.Delta == nil || delta.Delta.Delta != "你好" {
		t.Fatal("SSE 文本增量丢失")
	}
	_ = response.Body.Close()
	result, err := llm.NewResult(llm.ResultData{Schema: llm.ResultSchema, Items: []llm.OutputItem{{Kind: llm.OutputItemMessage, Text: "你好"}}, Replay: llm.ProtocolReplay{Protocol: llm.ProtocolResponses, Items: []json.RawMessage{json.RawMessage(`{"encrypted_content":"private-state"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Finish(id, result, nil); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"?session_id=session&agent_id=agent", nil)
	request.Header.Set("Last-Event-ID", eventCursor)
	reconnected, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Body.Close()
	_, finished := readModelFrame(t, bufio.NewReader(reconnected.Body))
	if finished.Kind != "finished" || finished.Record == nil || finished.Record.Text != "你好" || finished.Record.Result != nil {
		t.Fatal("续接终态不完整或泄漏协议重放载荷")
	}
}
