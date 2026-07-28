package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentgo/internal/output"
	"agentgo/internal/ui"
)

// fakeObserver 实现 ui.Observer：固定快照 + 手动注入更新的订阅通道。
type fakeObserver struct {
	snap ui.Snapshot

	mu   sync.Mutex
	subs []chan ui.Update
}

func (f *fakeObserver) Snapshot() ui.Snapshot { return f.snap }

func (f *fakeObserver) Subscribe(buf int) (<-chan ui.Update, func()) {
	if buf < 1 {
		buf = 1
	}
	ch := make(chan ui.Update, buf)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			f.mu.Lock()
			for i, c := range f.subs {
				if c == ch {
					f.subs = append(f.subs[:i], f.subs[i+1:]...)
					break
				}
			}
			f.mu.Unlock()
		})
	}
	return ch, cancel
}

// push 向所有订阅者注入一条更新（非阻塞）。
func (f *fakeObserver) push(u ui.Update) {
	f.mu.Lock()
	subs := append([]chan ui.Update(nil), f.subs...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- u:
		default:
		}
	}
}

func testSnapshot() ui.Snapshot {
	return ui.Snapshot{
		Agents:     []ui.AgentCard{{ID: "worker-1", Type: "worker", State: "processing", Loop: 2}},
		Tasks:      []ui.BoardTask{{ID: "task-1", Desc: "演示任务", Status: "processing"}},
		Mode:       "plan",
		ExecMode:   "strict",
		TopoMode:   "team",
		Session:    ui.SessionInfo{ID: "sess-1", Status: "active"},
		LastResult: &ui.ResultItem{AgentID: "scheduler-1", Text: "明确的最终回复"},
		Turns: []ui.AgentTurn{{
			ID: "turn-1", SessionID: "sess-1", AgentID: "worker-1",
			TaskID: "task-1", Loop: 2, Text: "完整轮次", Status: "completed",
			ToolCalls: []string{"read_file"}, CompletedAt: time.Now(),
		}},
		Feed: ui.FeedSnapshot{
			Outputs: []ui.FeedOutput{{Kind: "stream", AgentID: "worker-1", StreamID: "stream-1", Text: "partial", At: time.Now()}},
			Logs:    []ui.LogItem{{Text: "diagnostic", At: time.Now()}},
			Traces:  []ui.TraceEvent{{Kind: "tool_call", AgentID: "worker-1", Tool: "read_file", At: time.Now()}},
		},
		PendingInteractions: []ui.InteractionItem{{
			ID: "interaction-1", Version: 3, Kind: "choice", Purpose: "plan_pause", Prompt: "如何继续？",
		}},
	}
}

// TestSnapshotEndpoint /api/snapshot 返回 Observer 当前快照（JSON，含 agents/tasks 键）。
func TestSnapshotEndpoint(t *testing.T) {
	obs := &fakeObserver{snap: testSnapshot()}
	srv := NewServer(obs, "127.0.0.1:0", "")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body struct {
		Agents []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"agents"`
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
		Mode     string `json:"mode"`
		ExecMode string `json:"exec_mode"`
		TopoMode string `json:"topo_mode"`
		Session  struct {
			ID string `json:"id"`
		} `json:"session"`
		PendingInteractions []ui.InteractionItem `json:"pending_interactions"`
		LastResult          *ui.ResultItem       `json:"last_result"`
		Turns               []ui.AgentTurn       `json:"turns"`
		Feed                ui.FeedSnapshot      `json:"feed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Agents) != 1 || body.Agents[0].ID != "worker-1" || body.Agents[0].State != "processing" {
		t.Fatalf("agents = %+v", body.Agents)
	}
	if len(body.Tasks) != 1 || body.Tasks[0].ID != "task-1" {
		t.Fatalf("tasks = %+v", body.Tasks)
	}
	if body.Mode != "plan" || body.ExecMode != "strict" || body.TopoMode != "team" || body.Session.ID != "sess-1" {
		t.Fatalf("modes=%q/%q/%q session=%+v", body.Mode, body.ExecMode, body.TopoMode, body.Session)
	}
	if len(body.PendingInteractions) != 1 || body.PendingInteractions[0].ID != "interaction-1" {
		t.Fatalf("pending_interactions=%+v", body.PendingInteractions)
	}
	if body.LastResult == nil || body.LastResult.AgentID != "scheduler-1" || body.LastResult.Text != "明确的最终回复" {
		t.Fatalf("last_result=%+v", body.LastResult)
	}
	if len(body.Turns) != 1 || body.Turns[0].ID != "turn-1" || body.Turns[0].Text != "完整轮次" {
		t.Fatalf("turns=%+v", body.Turns)
	}
	if len(body.Feed.Outputs) != 1 || body.Feed.Outputs[0].StreamID != "stream-1" ||
		len(body.Feed.Logs) != 1 || len(body.Feed.Traces) != 1 || body.Feed.Traces[0].Tool != "read_file" {
		t.Fatalf("feed=%+v", body.Feed)
	}
}

// TestEncodeUpdate_InteractionsChangedFullList 确保 Interaction SSE 始终携带
// 完整列表，空列表也编码为 []，让前端可以可靠清空旧状态。
func TestEncodeUpdate_InteractionsChangedFullList(t *testing.T) {
	now := time.Now()
	data, err := encodeUpdate(ui.Update{
		Kind: ui.KindInteractionsChanged,
		Interactions: []ui.InteractionItem{{
			ID: "interaction-1", Version: 2, Kind: "choice", Prompt: "选择下一步",
		}},
		At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Kind         string               `json:"kind"`
		Interactions []ui.InteractionItem `json:"interactions"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Kind != "InteractionsChanged" || len(frame.Interactions) != 1 || frame.Interactions[0].ID != "interaction-1" {
		t.Fatalf("frame=%s", data)
	}

	empty, err := encodeUpdate(ui.Update{Kind: ui.KindInteractionsChanged, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"interactions":[]`) {
		t.Fatalf("空列表必须显式编码为 []: %s", empty)
	}
}

func TestEncodeUpdate_OutputStreamCarriesStableIdentity(t *testing.T) {
	data, err := encodeUpdate(ui.Update{
		Kind: ui.KindOutputStream,
		Output: output.Event{
			Kind: output.KindStream, AgentID: "worker-1", TaskID: "task-1",
			StreamID: "stream-1", Text: "partial", Loop: 3, Done: true,
		},
		At: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Kind   string `json:"kind"`
		Output struct {
			Kind     string `json:"kind"`
			StreamID string `json:"stream_id"`
			TaskID   string `json:"task_id"`
			Text     string `json:"text"`
			Loop     int    `json:"loop"`
			Done     bool   `json:"done"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Kind != "OutputStream" || frame.Output.Kind != "OutputStream" ||
		frame.Output.StreamID != "stream-1" || frame.Output.TaskID != "task-1" ||
		frame.Output.Text != "partial" || frame.Output.Loop != 3 || !frame.Output.Done {
		t.Fatalf("frame = %s", data)
	}
}

func TestEncodeUpdate_OutputTurnAndTurnsChanged(t *testing.T) {
	now := time.Now()
	data, err := encodeUpdate(ui.Update{
		Kind: ui.KindOutputTurn,
		Output: output.Event{
			Kind: output.KindTurn, AgentID: "scheduler-1", TaskID: "task-1",
			StreamID: "turn-1", Text: "本轮正文", Loop: 4, Done: true,
			ToolCalls: []string{"get_task_result"},
		},
		At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Kind   string `json:"kind"`
		Output struct {
			Kind      string   `json:"kind"`
			StreamID  string   `json:"stream_id"`
			Text      string   `json:"text"`
			ToolCalls []string `json:"tool_calls"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Kind != "OutputTurn" || frame.Output.Kind != "OutputTurn" ||
		frame.Output.StreamID != "turn-1" || frame.Output.Text != "本轮正文" ||
		len(frame.Output.ToolCalls) != 1 || frame.Output.ToolCalls[0] != "get_task_result" {
		t.Fatalf("完成轮次 SSE 编码错误: %s", data)
	}

	changed, err := encodeUpdate(ui.Update{
		Kind: ui.KindTurnsChanged,
		Turns: []ui.AgentTurn{{
			ID: "turn-2", AgentID: "worker-1", Loop: 1, Text: "恢复轮次", Status: "completed",
		}},
		At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var changedFrame struct {
		Kind  string         `json:"kind"`
		Turns []ui.AgentTurn `json:"turns"`
	}
	if err := json.Unmarshal(changed, &changedFrame); err != nil {
		t.Fatal(err)
	}
	if changedFrame.Kind != "TurnsChanged" || len(changedFrame.Turns) != 1 ||
		changedFrame.Turns[0].ID != "turn-2" {
		t.Fatalf("完整轮次列表 SSE 编码错误: %s", changed)
	}
	empty, err := encodeUpdate(ui.Update{Kind: ui.KindTurnsChanged, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"turns":[]`) {
		t.Fatalf("空轮次列表必须显式编码以清空旧 Session 视图: %s", empty)
	}
}

// TestSSEEndpointStreamsUpdateThenClosesOnCancel SSE 端点推送注入的更新，
// 客户端取消后服务端 handler 退出（goroutine 不泄漏）。
func TestSSEEndpointStreamsUpdateThenClosesOnCancel(t *testing.T) {
	obs := &fakeObserver{snap: testSnapshot()}
	srv := NewServer(obs, "127.0.0.1:0", "")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	lines := make(chan string, 8)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	// 等 SSE 订阅建立（observer 侧出现订阅者）再注入，避免丢失。
	waitFor(t, "SSE 订阅建立", func() bool {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		return len(obs.subs) == 1
	})
	obs.push(ui.Update{Kind: ui.KindLogLine, LogLine: "hello-sse", At: time.Now()})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-lines:
			if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if !strings.Contains(payload, `"kind":"LogLine"`) || !strings.Contains(payload, "hello-sse") {
					t.Fatalf("SSE 帧载荷不符: %s", payload)
				}
				// 客户端断开 → 服务端 handler 应退出（Scanner 结束）。
				cancel()
				select {
				case <-scanDone:
					return
				case <-time.After(3 * time.Second):
					t.Fatal("客户端取消后 SSE handler 未退出")
				}
			}
		case <-deadline:
			t.Fatal("3s 内未收到 data 帧")
		}
	}
}

// TestAuthMiddleware token 鉴权矩阵：无 token 配置全放行；有 token 时
// Bearer / ?token= 正确放行、错误拒绝；/healthz 始终豁免。
func TestAuthMiddleware(t *testing.T) {
	obs := &fakeObserver{snap: testSnapshot()}

	newReq := func(t *testing.T, url string) *http.Response {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("无 token 配置全放行", func(t *testing.T) {
		ts := httptest.NewServer(NewServer(obs, "127.0.0.1:0", "").handler())
		t.Cleanup(ts.Close)
		if r := newReq(t, ts.URL+"/api/snapshot"); r.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", r.StatusCode)
		}
	})

	t.Run("有 token 的接受/拒绝矩阵", func(t *testing.T) {
		ts := httptest.NewServer(NewServer(obs, "127.0.0.1:0", "secret").handler())
		t.Cleanup(ts.Close)

		// 无凭据 → 401（/ 与 /api/*）
		for _, path := range []string{"/", "/api/snapshot"} {
			if r := newReq(t, ts.URL+path); r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s 无凭据 status = %d，期望 401", path, r.StatusCode)
			}
		}
		// ?token= 错误 → 401；正确 → 200
		if r := newReq(t, ts.URL+"/api/snapshot?token=wrong"); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("错误 token status = %d", r.StatusCode)
		}
		if r := newReq(t, ts.URL+"/api/snapshot?token=secret"); r.StatusCode != http.StatusOK {
			t.Fatalf("?token= 正确 status = %d", r.StatusCode)
		}
		// Bearer 正确 → 200；错误 → 401
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/snapshot", nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Bearer 正确 status = %d", resp.StatusCode)
		}
		req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		req2.Header.Set("Authorization", "Bearer wrong")
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Bearer 错误 status = %d", resp2.StatusCode)
		}
		// /healthz 始终豁免
		if r := newReq(t, ts.URL+"/healthz"); r.StatusCode != http.StatusOK {
			t.Fatalf("/healthz status = %d", r.StatusCode)
		}
	})
}

// TestHealthzOpen /healthz 无鉴权返回 200 "ok"。
func TestHealthzOpen(t *testing.T) {
	obs := &fakeObserver{snap: testSnapshot()}
	ts := httptest.NewServer(NewServer(obs, "127.0.0.1:0", "secret").handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	buf := make([]byte, 16)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "ok" {
		t.Fatalf("body = %q，期望 ok", string(buf[:n]))
	}
}

// TestIndexServesSPA / 返回内嵌 HTML；未知路径 404。
func TestIndexServesSPA(t *testing.T) {
	obs := &fakeObserver{snap: testSnapshot()}
	ts := httptest.NewServer(NewServer(obs, "127.0.0.1:0", "").handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := make([]byte, 1024)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "AgentGo") {
		t.Fatalf("首页不含预期内容: %q", string(body[:n]))
	}

	resp2, err := http.Get(ts.URL + "/no-such-path")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("未知路径 status = %d，期望 404", resp2.StatusCode)
	}
}

func TestIndexContainsLayeredViewsAndAgentWorkbench(t *testing.T) {
	html := string(indexHTML)
	for _, marker := range []string{
		`data-view="overview"`, `data-view="agents"`, `data-view="activity"`, `data-view="trace"`,
		`id="agentTabs"`, `id="agentOutput"`, `id="agentActivity"`, `id="activityBody"`,
		`完整轮次输出`, `Trace / 工具调用 / 系统日志`, `snap.feed`, `state.turns`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("首页缺少分层 UI 标记 %q", marker)
		}
	}
}

// TestServerRunBindsAndShutsDown Run 监听真实地址（:0 临时端口），
// ctx 取消后优雅退出返回 nil。
func TestServerRunBindsAndShutsDown(t *testing.T) {
	obs := &fakeObserver{snap: testSnapshot()}
	srv := NewServer(obs, "127.0.0.1:0", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	waitFor(t, "服务器完成监听", func() bool { return srv.Addr() != "" })
	resp, err := http.Get("http://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("healthz 请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回非 nil: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("ctx 取消后 Run 未退出")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待条件超时：%s", what)
}
