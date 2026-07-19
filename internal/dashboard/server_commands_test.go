package dashboard

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentgo/internal/ui"
)

// GET /api/commands 与 GET /api/sessions 是 WebUI 斜杠命令体系的数据
// 源：前者把共享命令目录暴露给前端补全 / 帮助，后者支撑 /session 的
// 列表与编号切换。

func TestGetCommands_ReturnsCatalog(t *testing.T) {
	srv := NewServer(&fakeObserver{snap: ui.Snapshot{}}, "127.0.0.1:0", "")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/commands")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var catalog []ui.CommandSpec
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatalf("响应不是命令目录 JSON: %v", err)
	}
	if len(catalog) != len(ui.CommandCatalog()) {
		t.Fatalf("目录条数 = %d, want %d", len(catalog), len(ui.CommandCatalog()))
	}
	// WebUI 依赖 shared scope 过滤可执行命令；/help 必须在内。
	found := false
	for _, c := range catalog {
		if c.Name == "help" && c.Scope == ui.ScopeShared {
			found = true
		}
	}
	if !found {
		t.Fatal("目录缺少 shared 的 /help")
	}
}

func TestGetCommands_RejectsPost(t *testing.T) {
	srv := NewServer(&fakeObserver{snap: ui.Snapshot{}}, "127.0.0.1:0", "")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/commands", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/commands status = %d, want 405", resp.StatusCode)
	}
}

func TestGetCommands_RequiresTokenWhenConfigured(t *testing.T) {
	srv := NewServer(&fakeObserver{snap: ui.Snapshot{}}, "127.0.0.1:0", "secret")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/commands")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据 status = %d, want 401", resp.StatusCode)
	}
	resp2, err := http.Get(ts.URL + "/api/commands?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("正确 token status = %d, want 200", resp2.StatusCode)
	}
}

func TestGetSessions_ReturnsControllerList(t *testing.T) {
	fc := &fakeController{sessionsList: []ui.SessionInfo{
		{ID: "sess-1", Status: "active"},
		{ID: "sess-2", Status: "closed"},
	}}
	ts := httptest.NewServer(newControlServer(fc).handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Sessions []ui.SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("响应 JSON 解析失败: %v", err)
	}
	if len(body.Sessions) != 2 || body.Sessions[0].ID != "sess-1" {
		t.Fatalf("Session 列表未透传 Controller 数据: %+v", body.Sessions)
	}
}

func TestGetSessions_ReadOnlyMode503(t *testing.T) {
	srv := NewServer(&fakeObserver{snap: ui.Snapshot{}}, "127.0.0.1:0", "") // 未装配控制面
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("只读模式 status = %d, want 503", resp.StatusCode)
	}
}

func TestGetSessions_ControllerError400(t *testing.T) {
	fc := &fakeController{sessionsErr: errors.New("metadata corrupt")}
	ts := httptest.NewServer(newControlServer(fc).handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("领域错误 status = %d, want 400", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]string
	_ = json.Unmarshal(raw, &body)
	if body["error"] != "metadata corrupt" {
		t.Fatalf("错误信息未透传: %q", body["error"])
	}
}

func TestGetSessions_RejectsPost(t *testing.T) {
	ts := httptest.NewServer(newControlServer(&fakeController{}).handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/sessions", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/sessions status = %d, want 405", resp.StatusCode)
	}
}
