// Package dashboard 是 UI Hub 的 Web 前端：内嵌单页应用（SPA）+
// JSON/SSE HTTP 端点，把 ui.Observer 的快照与 Update 流暴露给浏览器，
// 并把 ui.Controller 的控制面 1:1 映射为一组 POST 端点（Step 4）。
//
// 安全模型：
//   - 配置层（config.validateUI）强制：非 loopback 监听必须设置 token；
//   - token 非空时 / 与 /api/* 要求 Bearer 头或 ?token= 查询参数；
//     /healthz 始终豁免（探活用途）；
//   - SSE 事件流只是 UI Hub 的一个普通订阅者，慢客户端由 Hub 的
//     drop-oldest 背压保护，绝不反向阻塞系统；
//   - CSRF：不提供独立 CSRF token。Bearer token 本身就是跨站防护——
//     第三方站点不知道 token，无法伪造 Authorization 头或 ?token= 参数；
//     且控制端点只接受 JSON 请求体，跨站 <form> POST（Content-Type 只能是
//     urlencoded / multipart / text/plain）必然 JSON 解析失败被 400。
//     默认 loopback 绑定进一步限定暴露面。
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"agentgo/internal/interaction"
	"agentgo/internal/runcontract"
	"agentgo/internal/ui"
)

// sseSubscriberBuf 是 SSE 订阅通道的缓冲。trace 高频事件（llm_call_start/end、
// tool_call/result）叠加 500ms 轮询的 AgentsChanged，256 给
// 浏览器渲染留出充足余量；满了由 Hub drop-oldest 兜底。
const sseSubscriberBuf = 256

// sseHeartbeat 是 SSE 心跳间隔（注释行 ": heartbeat"），防止中间件 / 浏览器
// 把空闲连接判死，也让前端能区分"无事件"与"连接已断"。
const sseHeartbeat = 15 * time.Second

// shutdownTimeout 是 ctx 取消后 HTTP 服务器优雅关闭的上限。
const shutdownTimeout = 5 * time.Second

// maxControlBody 是控制端点请求体上限（1MB）。控制载荷是短 JSON，
// 该上限仅为防御畸形请求占用内存。
const maxControlBody = 1 << 20

// Server 是 Web Dashboard 的 HTTP 服务器。仅依赖 ui.Observer（观测面）
// 与 ui.Controller（控制面），不感知 bootstrap 装配细节。
type Server struct {
	observer   ui.Observer
	controller ui.Controller // nil 表示只读模式：控制端点一律 503
	addr       string
	token      string

	mu        sync.RWMutex
	boundAddr string // Run 成功 listen 后的实际地址（:0 时为临时端口）
}

// NewServer 构造 Dashboard 服务器。addr 形如 "127.0.0.1:8399"；token 为空
// 表示不启用鉴权（配置层已保证此时 addr 必为 loopback）。
// 构造后为只读模式；控制面经 SetController 装配。
func NewServer(observer ui.Observer, addr, token string) *Server {
	return &Server{observer: observer, addr: addr, token: token}
}

// SetController 装配控制面（ui.Controller）。传 nil 保持只读模式。
func (s *Server) SetController(c ui.Controller) { s.controller = c }

// Addr 返回实际监听地址；Run 尚未成功 listen 时为空串。
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.boundAddr
}

// Run 启动 HTTP 服务器并阻塞，直到 ctx 取消（优雅关闭）或 listen/serve 失败。
// ctx 取消导致的正常退出返回 nil。
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.boundAddr = ln.Addr().String()
	s.mu.Unlock()

	httpSrv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	err = httpSrv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// handler 构造路由 + 鉴权中间件（测试经 httptest 直接使用，不起真实监听）。
// 只读端点（GET）与控制端点（POST）共用同一鉴权中间件。
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/commands", s.handleCommands)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/healthz", s.handleHealthz)
	// 控制端点：POST only，1:1 映射 ui.Controller。不提供 /api/quit——
	// 退出权属于主前端（TUI）。
	mux.HandleFunc("/api/input", s.handlePostInput)
	mux.HandleFunc("/api/tasks/cancel", s.handlePostCancelTask)
	mux.HandleFunc("/api/steer", s.handlePostSteer)
	mux.HandleFunc("/api/mode", s.handlePostMode)
	mux.HandleFunc("/api/interactions/", s.handlePostInteractionResponse)
	mux.HandleFunc("/api/session/new", s.handlePostSessionNew)
	mux.HandleFunc("/api/session/switch", s.handlePostSessionSwitch)
	mux.HandleFunc("/api/doctor/agents", s.handlePostDoctorAgents)
	mux.HandleFunc("/api/graphs/event", s.handlePostGraphEvent)
	return s.authMiddleware(mux)
}

// authMiddleware 在 token 非空时保护 / 与 /api/*；/healthz 始终豁免。
// 接受 "Authorization: Bearer <token>" 或 "?token=<token>"（EventSource
// 无法自定义请求头，浏览器 SSE 只能走查询参数）。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		token := r.URL.Query().Get("token")
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
		if token != s.token {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"未授权：缺少或错误的访问令牌"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleIndex 返回内嵌单页应用。仅匹配精确路径 "/"，其余路径 404。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// handleSnapshot 返回当前 UI Hub 快照（JSON）。
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.observer.Snapshot())
}

// handleHealthz 无需鉴权的探活端点。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// requireGet 拒绝非 GET 方法（405）。
func requireGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSONError(w, http.StatusMethodNotAllowed, "该端点仅支持 GET")
		return false
	}
	return true
}

// handleCommands 返回斜杠命令目录（ui.CommandCatalog，静态数据）。
// WebUI 用它渲染 "/" 输入补全下拉与 /help 帮助面板，与 TUI 共用同一
// 数据源，两个前端的命令集不会漂移。
func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, ui.CommandCatalog())
}

// handleSessions 返回 Session 列表，供 WebUI /session 命令展示与编号
// 切换（编号语义与 TUI 一致：按列表顺序 1 起）。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) || !s.controlAvailable(w) {
		return
	}
	sessions, err := s.controller.ListSessions()
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// ---------- 控制端点（POST，1:1 映射 ui.Controller） ----------
// handler 全部保持薄壳：参数校验 + HTTP 状态映射，业务逻辑都在 Controller。

// requirePost 拒绝非 POST 方法（405）。
func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "该端点仅支持 POST")
		return false
	}
	return true
}

// controlAvailable 检查控制面是否装配；未装配写 503 并返回 false。
func (s *Server) controlAvailable(w http.ResponseWriter) bool {
	if s.controller == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "控制面未装配：本实例以只读模式运行")
		return false
	}
	return true
}

// decodeControlBody 解析控制端点的 JSON 请求体（上限 maxControlBody）。
// 失败时写 400 并返回 false。
func decodeControlBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

// writeControlError 把 Controller 返回的错误映射为 HTTP 状态：
// ErrNotAssembled 属服务端装配缺陷 → 500；其余视为领域错误
// （未找到 / 前缀歧义 / plan 守卫拒绝等）→ 400，原样透传中文错误信息。
func writeControlError(w http.ResponseWriter, err error) {
	if errors.Is(err, ui.ErrNotAssembled) {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSONError(w, http.StatusBadRequest, err.Error())
}

// handlePostGraphEvent 投递图外部事件：{graph_id, event, data?} →
// Controller.EmitGraphEvent。时点信号语义：未命中 waiting 的 wait_event
// 节点（未等待 / 图终态 / 会话冻结）时静默忽略，仍返回 ok。
func (s *Server) handlePostGraphEvent(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	var body struct {
		GraphID string         `json:"graph_id"`
		Event   string         `json:"event"`
		Data    map[string]any `json:"data"`
	}
	if !decodeControlBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.GraphID) == "" || strings.TrimSpace(body.Event) == "" {
		writeJSONError(w, http.StatusBadRequest, "graph_id 与 event 不能为空")
		return
	}
	if err := s.controller.EmitGraphEvent(body.GraphID, body.Event, body.Data); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePostInput 投递用户输入：{text} → Controller.SendUserText。
func (s *Server) handlePostInput(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	var body struct {
		Text        string                   `json:"text"`
		RunContract *runcontract.RunContract `json:"run_contract,omitempty"`
	}
	if !decodeControlBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSONError(w, http.StatusBadRequest, "text 不能为空")
		return
	}
	if ui.IsSlashCommandText(body.Text) {
		writeJSONError(w, http.StatusBadRequest, "斜杠命令必须通过命令控制接口执行，不能提交为普通任务")
		return
	}
	var err error
	if body.RunContract != nil {
		if sender, ok := s.controller.(interface {
			SendUserTextWithRunContract(context.Context, string, *runcontract.RunContract) error
		}); ok {
			err = sender.SendUserTextWithRunContract(r.Context(), body.Text, body.RunContract)
		} else {
			err = fmt.Errorf("当前控制器不支持 RunContract")
		}
	} else {
		err = s.controller.SendUserText(r.Context(), body.Text)
	}
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePostCancelTask 按 ID 前缀取消任务：{id_prefix} → {task_id}。
func (s *Server) handlePostCancelTask(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	var body struct {
		IDPrefix string `json:"id_prefix"`
	}
	if !decodeControlBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.IDPrefix) == "" {
		writeJSONError(w, http.StatusBadRequest, "id_prefix 不能为空")
		return
	}
	taskID, err := s.controller.CancelTask(body.IDPrefix)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"task_id": taskID})
}

// handlePostSteer 向代理发送指导：{agent_id, message}。
func (s *Server) handlePostSteer(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
		Message string `json:"message"`
	}
	if !decodeControlBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.AgentID) == "" || strings.TrimSpace(body.Message) == "" {
		writeJSONError(w, http.StatusBadRequest, "agent_id 与 message 均不能为空")
		return
	}
	if err := s.controller.SteerAgent(body.AgentID, body.Message); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePostMode 切换 exec / topo 两轴模式。旧 {mode} 与 axis=gate 请求
// 仅用于返回明确的 V6 迁移诊断，不再进入控制器。
func (s *Server) handlePostMode(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	var body struct {
		Axis  string `json:"axis"`
		Value string `json:"value"`
		Mode  string `json:"mode"`
	}
	if !decodeControlBody(w, r, &body) {
		return
	}
	axis := strings.ToLower(strings.TrimSpace(body.Axis))
	value := strings.ToLower(strings.TrimSpace(body.Value))
	legacyMode := strings.ToLower(strings.TrimSpace(body.Mode))
	if axis == "" && value == "" && legacyMode != "" {
		writeJSONError(w, http.StatusBadRequest, "gate 轴已于 V6 移除：请使用 axis=exec|topo；执行前审阅改由 Graph approval 节点承担")
		return
	} else if legacyMode != "" {
		writeJSONError(w, http.StatusBadRequest, "mode 不能与 axis/value 同时使用")
		return
	}

	switch axis {
	case "gate":
		writeJSONError(w, http.StatusBadRequest, "gate 轴已于 V6 移除：执行前审阅改由 Graph approval 节点承担")
		return
	case "exec":
		if err := s.controller.SetExecMode(value); err != nil {
			writeControlError(w, err)
			return
		}
	case "topo":
		if err := s.controller.SetTopoMode(value); err != nil {
			writeControlError(w, err)
			return
		}
	default:
		writeJSONError(w, http.StatusBadRequest, "axis 仅允许 exec / topo")
		return
	}
	response := map[string]string{"axis": axis, "value": value}
	writeJSON(w, http.StatusOK, response)
}

// handlePostInteractionResponse 提交一条结构化用户回答：
// POST /api/interactions/{id}/response。
// 客户端只提交稳定 option_id / 文本；RespondedBy 固定由服务端标记为 web。
func (s *Server) handlePostInteractionResponse(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	const prefix = "/api/interactions/"
	const suffix = "/response"
	tail := strings.TrimPrefix(r.URL.Path, prefix)
	if !strings.HasSuffix(tail, suffix) {
		http.NotFound(w, r)
		return
	}
	requestID := strings.TrimSuffix(tail, suffix)
	if requestID == "" || strings.Contains(requestID, "/") {
		writeJSONError(w, http.StatusBadRequest, "路径缺少合法的 Interaction ID")
		return
	}
	var body struct {
		ExpectedVersion int64  `json:"expected_version"`
		OptionID        string `json:"option_id"`
		Text            string `json:"text"`
	}
	if !decodeControlBody(w, r, &body) {
		return
	}
	if body.ExpectedVersion <= 0 {
		writeJSONError(w, http.StatusBadRequest, "expected_version 必须为正整数")
		return
	}
	if strings.TrimSpace(body.OptionID) == "" && strings.TrimSpace(body.Text) == "" {
		writeJSONError(w, http.StatusBadRequest, "option_id 和 text 至少填写一个")
		return
	}
	resolved, err := s.controller.RespondInteraction(r.Context(), interaction.ResolveInput{
		RequestID:       requestID,
		ExpectedVersion: body.ExpectedVersion,
		OptionID:        strings.TrimSpace(body.OptionID),
		Text:            body.Text,
		RespondedBy:     "web",
	})
	if err != nil {
		writeInteractionError(w, resolved, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"request_id": resolved.ID,
		"version":    resolved.Version,
		"state":      resolved.State,
	})
}

// writeInteractionError 按 Interaction 协议把回答失败映射为稳定 HTTP 状态。
// 若受信任处理器已把请求推进到终态，优先依据返回记录判定 410。
func writeInteractionError(w http.ResponseWriter, request ui.InteractionResult, err error) {
	switch request.State {
	case interaction.StateCancelled, interaction.StateExpired,
		interaction.StateInterrupted, interaction.StateFailed:
		writeJSONError(w, http.StatusGone, err.Error())
		return
	}
	switch {
	case errors.Is(err, interaction.ErrCancelled),
		errors.Is(err, interaction.ErrExpired),
		errors.Is(err, interaction.ErrInterrupted),
		errors.Is(err, interaction.ErrFailed):
		writeJSONError(w, http.StatusGone, err.Error())
	case errors.Is(err, interaction.ErrVersionConflict),
		errors.Is(err, interaction.ErrAlreadyAnswered),
		errors.Is(err, interaction.ErrInvalidTransition):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, interaction.ErrInvalidRequest),
		errors.Is(err, interaction.ErrInvalidOption):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, interaction.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())
	default:
		writeControlError(w, err)
	}
}

// handlePostSessionNew 创建并切换到新 Session：→ {session_id}。
// 可选请求体 {force: true}：先终止当前 Session 的全部运行内容再新建。
func (s *Server) handlePostSessionNew(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if r.ContentLength > 0 && !decodeControlBody(w, r, &body) {
		return
	}
	if body.Force {
		id, err := s.controller.NewSessionForce()
		if err != nil {
			writeControlError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_id": id})
		return
	}
	id, err := s.controller.NewSession()
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": id})
}

// handlePostSessionSwitch 切换到指定 Session：{id}。
func (s *Server) handlePostSessionSwitch(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if !decodeControlBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ID) == "" {
		writeJSONError(w, http.StatusBadRequest, "id 不能为空")
		return
	}
	changed, err := s.controller.SwitchSession(body.ID)
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "changed": changed})
}

// handlePostDoctorAgents 触发只读代理审计（/doctor agents，V6 §2 P1b）：
// 无请求体，创建审计任务并返回 {task_id}；审计报告作为普通任务结果回显。
func (s *Server) handlePostDoctorAgents(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) || !s.controlAvailable(w) {
		return
	}
	taskID, err := s.controller.RequestAgentAudit()
	if err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"task_id": taskID})
}

// handleEvents 是 SSE 事件流端点：订阅 UI Hub（缓冲 sseSubscriberBuf），
// 把每条 Update 编码为一帧 "data: {json}\n\n"；每 sseHeartbeat 发一行心跳
// 注释。客户端断开或服务器关闭时退出。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前环境不支持 SSE（ResponseWriter 无 Flusher）", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	updates, cancel := s.observer.Subscribe(sseSubscriberBuf)
	defer cancel()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	// 首帧立即 flush，让浏览器 onopen（拿到 200 + headers）尽早触发。
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case u := <-updates:
			data, err := encodeUpdate(u)
			if err != nil {
				log.Printf("[dashboard] 编码 SSE 更新失败（跳过本帧）: %v", err)
				continue
			}
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if _, err := w.Write(data); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// updateWire 是 SSE 帧的 JSON 结构：kind 用字符串名（前端 switch 友好），
// 载荷按 kind 只填一个字段，与 ui.Update 的载荷约定一一对应。
type updateWire struct {
	Kind         string                `json:"kind"`
	Output       *outputWire           `json:"output,omitempty"`
	LogLine      string                `json:"log_line,omitempty"`
	Interactions *[]ui.InteractionItem `json:"interactions,omitempty"`
	Agents       []ui.AgentCard        `json:"agents,omitempty"`
	Tasks        []ui.BoardTask        `json:"tasks,omitempty"`
	Graphs       []ui.GraphView        `json:"graphs,omitempty"`
	Turns        *[]ui.AgentTurn       `json:"turns,omitempty"`
	Snapshot     *ui.Snapshot          `json:"snapshot,omitempty"`
	Trace        *ui.TraceEvent        `json:"trace,omitempty"`
	At           time.Time             `json:"at"`
}

// outputWire 是 output.Event 的 JSON 形态（Kind 用字符串，与 UpdateKind 对齐）。
type outputWire struct {
	Kind      string   `json:"kind"` // OutputResult / OutputText / OutputStream / OutputTurn
	AgentID   string   `json:"agent_id"`
	SessionID string   `json:"session_id,omitempty"`
	Text      string   `json:"text"`
	StreamID  string   `json:"stream_id,omitempty"`
	TaskID    string   `json:"task_id,omitempty"`
	Loop      int      `json:"loop,omitempty"`
	Done      bool     `json:"done,omitempty"`
	Error     string   `json:"error,omitempty"`
	ToolCalls []string `json:"tool_calls,omitempty"`
}

// encodeUpdate 把一条 ui.Update 编码为 SSE 帧载荷。
func encodeUpdate(u ui.Update) ([]byte, error) {
	w := updateWire{Kind: u.Kind.String(), At: u.At}
	switch u.Kind {
	case ui.KindSnapshotSync:
		w.Snapshot = &u.Snapshot
	case ui.KindOutputResult:
		w.Output = &outputWire{Kind: "OutputResult", AgentID: u.Output.AgentID, Text: u.Output.Text}
	case ui.KindOutputText:
		w.Output = &outputWire{Kind: "OutputText", AgentID: u.Output.AgentID, Text: u.Output.Text}
	case ui.KindOutputStream:
		w.Output = &outputWire{
			Kind: "OutputStream", AgentID: u.Output.AgentID, SessionID: u.Output.SessionID, Text: u.Output.Text,
			StreamID: u.Output.StreamID, TaskID: u.Output.TaskID, Loop: u.Output.Loop,
			Done: u.Output.Done, Error: u.Output.Error,
		}
	case ui.KindOutputTurn:
		w.Output = &outputWire{
			Kind: "OutputTurn", AgentID: u.Output.AgentID, SessionID: u.Output.SessionID, Text: u.Output.Text,
			StreamID: u.Output.StreamID, TaskID: u.Output.TaskID, Loop: u.Output.Loop,
			Done: true, Error: u.Output.Error, ToolCalls: append([]string(nil), u.Output.ToolCalls...),
		}
	case ui.KindTurnsChanged:
		turns := append([]ui.AgentTurn(nil), u.Turns...)
		if turns == nil {
			turns = []ui.AgentTurn{}
		}
		w.Turns = &turns
	case ui.KindLogLine:
		w.LogLine = u.LogLine
	case ui.KindInteractionsChanged:
		items := u.Interactions
		if items == nil {
			items = []ui.InteractionItem{}
		}
		w.Interactions = &items
	case ui.KindAgentsChanged:
		w.Agents = u.Agents
		w.Tasks = u.Tasks
		w.Graphs = u.Graphs
	case ui.KindTraceEvent:
		w.Trace = &u.Trace
	}
	return json.Marshal(w)
}

// writeJSONError 以统一 {"error": msg} 形态返回错误响应。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeJSON 以 UTF-8 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "JSON 编码失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
