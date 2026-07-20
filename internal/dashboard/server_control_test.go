package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentgo/internal/interaction"
	"agentgo/internal/ui"
)

// fakeController 是记录调用的 ui.Controller 假实现：每个方法记录入参，
// 返回预设的结果 / 错误。
type fakeController struct {
	userText            string
	userTextErr         error
	cancelPrefix        string
	cancelTaskID        string
	cancelErr           error
	cancelLatestCalls   int
	cancelLatestSummary string
	cancelLatestErr     error
	steerAgentID        string
	steerMessage        string
	steerErr            error
	modeCalls           []bool
	execModeCalls       []string
	execModeErr         error
	topoModeCalls       []string
	topoModeErr         error
	newSessID           string
	newSessErr          error
	switchID            string
	switchChanged       bool
	switchErr           error
	interactionInput    interaction.ResolveInput
	interactionResult   ui.InteractionResult
	interactionErr      error
	interactionCalls    int
	sessionsList        []ui.SessionInfo
	sessionsErr         error
	quitCalled          bool
}

func (f *fakeController) SendUserText(_ context.Context, text string) error {
	f.userText = text
	return f.userTextErr
}

func (f *fakeController) CancelTask(idPrefix string) (string, error) {
	f.cancelPrefix = idPrefix
	return f.cancelTaskID, f.cancelErr
}

func (f *fakeController) CancelLatestRequest() (string, error) {
	f.cancelLatestCalls++
	return f.cancelLatestSummary, f.cancelLatestErr
}

func (f *fakeController) SteerAgent(agentID, message string) error {
	f.steerAgentID, f.steerMessage = agentID, message
	return f.steerErr
}

func (f *fakeController) SetMode(plan bool) { f.modeCalls = append(f.modeCalls, plan) }

func (f *fakeController) SetExecMode(mode string) error {
	f.execModeCalls = append(f.execModeCalls, mode)
	return f.execModeErr
}

func (f *fakeController) SetTopoMode(mode string) error {
	f.topoModeCalls = append(f.topoModeCalls, mode)
	return f.topoModeErr
}

// ApprovePlan / RejectPlan / PendingPlanReviews 仅为满足 ui.Controller 接口
// 扩展而存在——dashboard 本切片不新增对应端点，fake 返回零值。
func (f *fakeController) ApprovePlan(idPrefix string) (string, error) { return "", nil }

func (f *fakeController) RejectPlan(idPrefix string) (string, error) { return "", nil }

func (f *fakeController) PendingPlanReviews() ([]ui.PlanReviewItem, error) { return nil, nil }

func (f *fakeController) NewSession() (string, error) { return f.newSessID, f.newSessErr }

func (f *fakeController) SwitchSession(id string) (bool, error) {
	f.switchID = id
	return f.switchChanged, f.switchErr
}

func (f *fakeController) ListSessions() ([]ui.SessionInfo, error) {
	return f.sessionsList, f.sessionsErr
}

func (f *fakeController) RespondInteraction(_ context.Context, input interaction.ResolveInput) (ui.InteractionResult, error) {
	f.interactionCalls++
	f.interactionInput = input
	if f.interactionResult.ID == "" {
		f.interactionResult = ui.InteractionResult{
			ID:      input.RequestID,
			Version: input.ExpectedVersion + 1,
			State:   interaction.StateResolved,
		}
	}
	return f.interactionResult, f.interactionErr
}

func (f *fakeController) RequestQuit() { f.quitCalled = true }

// newControlServer 构造带 fake 控制面 + 空快照观测面的测试服务器。
func newControlServer(fc *fakeController) *Server {
	srv := NewServer(&fakeObserver{snap: ui.Snapshot{}}, "127.0.0.1:0", "")
	srv.SetController(fc)
	return srv
}

// post 发起 POST 请求并在返回前读出响应体（body 经 json.Unmarshal 解析，可为 nil）。
func post(t *testing.T, ts *httptest.Server, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("响应体不是 JSON: %q", raw)
		}
	}
	return resp.StatusCode, parsed
}

// TestControlEndpoints_MapToController 端点 → Controller 调用的映射矩阵：
// 每个端点以正确参数调用正确的方法，成功响应体含约定字段。
func TestControlEndpoints_MapToController(t *testing.T) {
	fc := &fakeController{cancelTaskID: "task-abcdef", newSessID: "sess-new"}
	obs := &fakeObserver{snap: ui.Snapshot{}}
	srv := NewServer(obs, "127.0.0.1:0", "")
	srv.SetController(fc)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	t.Run("input", func(t *testing.T) {
		status, body := post(t, ts, "/api/input", "", `{"text":"帮我写个脚本"}`)
		if status != http.StatusOK || fc.userText != "帮我写个脚本" {
			t.Fatalf("status=%d userText=%q", status, fc.userText)
		}
		if body["ok"] != true {
			t.Fatalf("body=%v", body)
		}
	})

	t.Run("tasks/cancel", func(t *testing.T) {
		status, body := post(t, ts, "/api/tasks/cancel", "", `{"id_prefix":"task-ab"}`)
		if status != http.StatusOK || fc.cancelPrefix != "task-ab" {
			t.Fatalf("status=%d prefix=%q", status, fc.cancelPrefix)
		}
		if body["task_id"] != "task-abcdef" {
			t.Fatalf("body=%v", body)
		}
	})

	t.Run("steer", func(t *testing.T) {
		status, _ := post(t, ts, "/api/steer", "", `{"agent_id":"worker-1","message":"先读 README"}`)
		if status != http.StatusOK || fc.steerAgentID != "worker-1" || fc.steerMessage != "先读 README" {
			t.Fatalf("status=%d steer=%q/%q", status, fc.steerAgentID, fc.steerMessage)
		}
	})

	t.Run("mode gate（兼容旧 body）", func(t *testing.T) {
		status, body := post(t, ts, "/api/mode", "", `{"mode":"plan"}`)
		if status != http.StatusOK || body["axis"] != "gate" || body["value"] != "plan" || body["mode"] != "plan" {
			t.Fatalf("status=%d body=%v", status, body)
		}
		status, _ = post(t, ts, "/api/mode", "", `{"axis":"gate","value":"immediate"}`)
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
		if len(fc.modeCalls) != 2 || !fc.modeCalls[0] || fc.modeCalls[1] {
			t.Fatalf("modeCalls=%v，期望 [true false]", fc.modeCalls)
		}
	})

	t.Run("mode exec/topo", func(t *testing.T) {
		status, body := post(t, ts, "/api/mode", "", `{"axis":"exec","value":"strict"}`)
		if status != http.StatusOK || body["axis"] != "exec" || body["value"] != "strict" {
			t.Fatalf("exec status=%d body=%v", status, body)
		}
		status, body = post(t, ts, "/api/mode", "", `{"axis":"topo","value":"solo"}`)
		if status != http.StatusOK || body["axis"] != "topo" || body["value"] != "solo" {
			t.Fatalf("topo status=%d body=%v", status, body)
		}
		if len(fc.execModeCalls) != 1 || fc.execModeCalls[0] != "strict" ||
			len(fc.topoModeCalls) != 1 || fc.topoModeCalls[0] != "solo" {
			t.Fatalf("exec=%v topo=%v", fc.execModeCalls, fc.topoModeCalls)
		}
	})

	t.Run("interaction response", func(t *testing.T) {
		fc.interactionResult = ui.InteractionResult{ID: "choice-1", Version: 8, State: interaction.StateResolved}
		status, body := post(t, ts, "/api/interactions/choice-1/response", "",
			`{"expected_version":7,"option_id":"revise","text":"补充测试","responded_by":"伪造来源"}`)
		if status != http.StatusOK || body["request_id"] != "choice-1" || body["state"] != "resolved" {
			t.Fatalf("status=%d body=%v", status, body)
		}
		want := interaction.ResolveInput{
			RequestID:       "choice-1",
			ExpectedVersion: 7,
			OptionID:        "revise",
			Text:            "补充测试",
			RespondedBy:     "web",
		}
		if fc.interactionInput != want {
			t.Fatalf("RespondInteraction input=%+v, want %+v", fc.interactionInput, want)
		}
	})

	t.Run("session/new", func(t *testing.T) {
		status, body := post(t, ts, "/api/session/new", "", "")
		if status != http.StatusOK || body["session_id"] != "sess-new" {
			t.Fatalf("status=%d body=%v", status, body)
		}
	})

	t.Run("session/switch", func(t *testing.T) {
		fc.switchChanged = true
		status, body := post(t, ts, "/api/session/switch", "", `{"id":"sess-old"}`)
		if status != http.StatusOK || fc.switchID != "sess-old" || body["changed"] != true {
			t.Fatalf("status=%d switchID=%q body=%v", status, fc.switchID, body)
		}
	})

	t.Run("session/switch current is no-op", func(t *testing.T) {
		fc.switchChanged = false
		status, body := post(t, ts, "/api/session/switch", "", `{"id":"sess-old"}`)
		if status != http.StatusOK || body["changed"] != false {
			t.Fatalf("status=%d body=%v", status, body)
		}
	})
}

// TestControlEndpoints_DomainError Controller 领域错误 → 400 且透传中文信息。
func TestControlEndpoints_DomainError(t *testing.T) {
	fc := &fakeController{
		userTextErr: errors.New("事件通道超时，调度器可能阻塞"),
		cancelErr:   errors.New("未找到以 zzz9 开头的任务"),
		steerErr:    errors.New("代理不存在"),
		execModeErr: errors.New("未知执行权限模式"),
		topoModeErr: errors.New("未知编排拓扑模式"),
		newSessErr:  errors.New("session 管理器未初始化"),
		switchErr:   errors.New("session sess-x 不存在"),
	}
	ts := httptest.NewServer(newControlServer(fc).handler())
	t.Cleanup(ts.Close)

	cases := []struct {
		path    string
		body    string
		wantMsg string
	}{
		{"/api/input", `{"text":"hi"}`, "事件通道超时"},
		{"/api/tasks/cancel", `{"id_prefix":"zzz9"}`, "未找到以 zzz9 开头的任务"},
		{"/api/steer", `{"agent_id":"a1","message":"m"}`, "代理不存在"},
		{"/api/mode", `{"axis":"exec","value":"unknown"}`, "未知执行权限模式"},
		{"/api/mode", `{"axis":"topo","value":"unknown"}`, "未知编排拓扑模式"},
		{"/api/session/new", ``, "session 管理器未初始化"},
		{"/api/session/switch", `{"id":"sess-x"}`, "不存在"},
	}
	for _, c := range cases {
		status, body := post(t, ts, c.path, "", c.body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s status=%d，期望 400", c.path, status)
		}
		msg, _ := body["error"].(string)
		if !strings.Contains(msg, c.wantMsg) {
			t.Fatalf("%s error=%q，期望含 %q", c.path, msg, c.wantMsg)
		}
	}
}

// TestControlEndpoints_NotAssembledError ErrNotAssembled 属装配缺陷 → 500。
func TestControlEndpoints_NotAssembledError(t *testing.T) {
	fc := &fakeController{cancelErr: fmt.Errorf("包装: %w", ui.ErrNotAssembled)}
	ts := httptest.NewServer(newControlServer(fc).handler())
	t.Cleanup(ts.Close)

	status, body := post(t, ts, "/api/tasks/cancel", "", `{"id_prefix":"zzzz"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("status=%d，期望 500；body=%v", status, body)
	}
}

// TestControlEndpoints_Validation 参数校验矩阵：非法输入 → 400，且不触达 Controller。
func TestControlEndpoints_Validation(t *testing.T) {
	fc := &fakeController{}
	ts := httptest.NewServer(newControlServer(fc).handler())
	t.Cleanup(ts.Close)

	cases := []struct {
		name string
		path string
		body string
	}{
		{"空 text", "/api/input", `{"text":"  "}`},
		{"斜杠命令", "/api/input", `{"text":"/help"}`},
		{"带前导空白的斜杠命令", "/api/input", `{"text":"  /command args"}`},
		{"空 id_prefix", "/api/tasks/cancel", `{"id_prefix":""}`},
		{"steer 缺 message", "/api/steer", `{"agent_id":"a1"}`},
		{"steer 缺 agent_id", "/api/steer", `{"message":"m"}`},
		{"非法 mode", "/api/mode", `{"mode":"nope"}`},
		{"非法 mode axis", "/api/mode", `{"axis":"layout","value":"solo"}`},
		{"mode 新旧协议混用", "/api/mode", `{"axis":"gate","value":"plan","mode":"plan"}`},
		{"interaction 缺 version", "/api/interactions/req-1/response", `{"option_id":"continue"}`},
		{"interaction 缺回答", "/api/interactions/req-1/response", `{"expected_version":1}`},
		{"interaction 路径多余层级", "/api/interactions/group/req-1/response", `{"expected_version":1,"option_id":"continue"}`},
		{"session/switch 空 id", "/api/session/switch", `{"id":" "}`},
		{"请求体非 JSON", "/api/input", `not-json`},
		{"请求体非 JSON（Interaction）", "/api/interactions/req-1/response", `{`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := post(t, ts, c.path, "", c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d，期望 400；body=%v", status, body)
			}
			if body["error"] == nil {
				t.Fatalf("响应缺少 error 字段: %v", body)
			}
		})
	}
	// 校验失败不得触达 Controller
	if fc.userText != "" || fc.cancelPrefix != "" || fc.steerAgentID != "" ||
		len(fc.modeCalls) != 0 || len(fc.execModeCalls) != 0 || len(fc.topoModeCalls) != 0 ||
		fc.switchID != "" || fc.interactionCalls != 0 {
		t.Fatalf("校验失败的请求触达了 Controller: %+v", fc)
	}
}

// TestInteractionEndpoint_StatusMapping 固定 Interaction 领域错误与 HTTP
// 状态的映射，避免多前端竞态被误报为普通 400。
func TestInteractionEndpoint_StatusMapping(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		state interaction.State
		want  int
	}{
		{"请求无效", interaction.ErrInvalidRequest, "", http.StatusBadRequest},
		{"选项无效", interaction.ErrInvalidOption, "", http.StatusBadRequest},
		{"版本冲突", interaction.ErrVersionConflict, "", http.StatusConflict},
		{"已被回答", interaction.ErrAlreadyAnswered, "", http.StatusConflict},
		{"转换冲突", interaction.ErrInvalidTransition, "", http.StatusConflict},
		{"已取消", interaction.ErrCancelled, "", http.StatusGone},
		{"已过期", interaction.ErrExpired, "", http.StatusGone},
		{"已中断", interaction.ErrInterrupted, "", http.StatusGone},
		{"处理失败", interaction.ErrFailed, "", http.StatusGone},
		{"返回记录已失败", errors.New("副作用失败"), interaction.StateFailed, http.StatusGone},
		{"不存在", interaction.ErrNotFound, "", http.StatusNotFound},
		{"控制面未装配", fmt.Errorf("包装: %w", ui.ErrNotAssembled), "", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeController{
				interactionErr: tc.err,
				interactionResult: ui.InteractionResult{
					ID: "req-1", Version: 2, State: tc.state,
				},
			}
			ts := httptest.NewServer(newControlServer(fc).handler())
			t.Cleanup(ts.Close)
			status, body := post(t, ts, "/api/interactions/req-1/response", "",
				`{"expected_version":1,"option_id":"continue"}`)
			if status != tc.want || body["error"] == nil {
				t.Fatalf("status=%d, want %d; body=%v", status, tc.want, body)
			}
		})
	}
}

// TestControlEndpoints_MethodMismatch 非 POST 方法 → 405。
func TestControlEndpoints_MethodMismatch(t *testing.T) {
	fc := &fakeController{}
	ts := httptest.NewServer(newControlServer(fc).handler())
	t.Cleanup(ts.Close)

	paths := []string{
		"/api/input", "/api/tasks/cancel", "/api/steer", "/api/mode",
		"/api/interactions/req-1/response", "/api/session/new", "/api/session/switch",
	}
	for _, p := range paths {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status=%d，期望 405", p, resp.StatusCode)
		}
	}
}

// TestControlEndpoints_Auth 控制端点同样走 Bearer 鉴权：无凭据 401，正确凭据放行。
func TestControlEndpoints_Auth(t *testing.T) {
	fc := &fakeController{}
	srv := NewServer(&fakeObserver{snap: ui.Snapshot{}}, "127.0.0.1:0", "secret")
	srv.SetController(fc)
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	status, _ := post(t, ts, "/api/mode", "", `{"mode":"plan"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("无凭据 status=%d，期望 401", status)
	}
	if len(fc.modeCalls) != 0 {
		t.Fatal("未授权请求不得触达 Controller")
	}
	status, _ = post(t, ts, "/api/mode", "wrong", `{"mode":"plan"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("错误凭据 status=%d，期望 401", status)
	}
	status, _ = post(t, ts, "/api/mode", "secret", `{"mode":"plan"}`)
	if status != http.StatusOK || len(fc.modeCalls) != 1 {
		t.Fatalf("正确凭据 status=%d modeCalls=%v", status, fc.modeCalls)
	}
}

// TestControlEndpoints_NilController 未装配控制面（只读模式）→ 全部控制端点 503 JSON。
func TestControlEndpoints_NilController(t *testing.T) {
	srv := NewServer(&fakeObserver{snap: ui.Snapshot{}}, "127.0.0.1:0", "")
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	cases := []struct{ path, body string }{
		{"/api/input", `{"text":"hi"}`},
		{"/api/tasks/cancel", `{"id_prefix":"zzzz"}`},
		{"/api/steer", `{"agent_id":"a","message":"m"}`},
		{"/api/mode", `{"mode":"plan"}`},
		{"/api/interactions/req-1/response", `{"expected_version":1,"option_id":"continue"}`},
		{"/api/session/new", ``},
		{"/api/session/switch", `{"id":"s1"}`},
	}
	for _, c := range cases {
		status, body := post(t, ts, c.path, "", c.body)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d，期望 503", c.path, status)
		}
		if body["error"] == nil {
			t.Fatalf("%s 响应缺少 error 字段: %v", c.path, body)
		}
	}
	// 只读端点不受影响
	resp, err := http.Get(ts.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("只读端点 status=%d，期望 200", resp.StatusCode)
	}
}
