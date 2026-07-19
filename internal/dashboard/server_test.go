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
		Agents:  []ui.AgentCard{{ID: "worker-1", Type: "worker", State: "processing", Loop: 2}},
		Tasks:   []ui.BoardTask{{ID: "task-1", Desc: "演示任务", Status: "processing"}},
		Mode:    "plan",
		Session: ui.SessionInfo{ID: "sess-1", Status: "active"},
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
		Mode    string `json:"mode"`
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
		PendingApprovals []any `json:"pending_approvals"`
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
	if body.Mode != "plan" || body.Session.ID != "sess-1" {
		t.Fatalf("mode=%q session=%+v", body.Mode, body.Session)
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
