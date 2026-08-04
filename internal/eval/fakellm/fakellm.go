// Package fakellm 实现离线评测（V6 §7.6 Offline fake-LLM E2E）的脚本化
// OpenAI-compatible Chat Completions 假端点。
//
// 设计要点：
//   - 脚本化：YAML/JSON 脚本声明有序响应步骤，按「车道 + 子串」匹配请求，
//     一次性步骤被消费后不再命中，repeat 步骤可反复命中，全部不命中走兜底；
//   - 双响应形态：一次性 JSON 与 SSE 流式（stream: true 步骤），均严格按
//     OpenAI Chat Completions wire 格式产出（含 tool_calls 与 usage）；
//   - 可脚本化异常：HTTP 错误（429/500/401 等）与 finish_reason=length 截断；
//   - 请求记录：每个到达的请求留存原始体、车道、模型、工具清单与命中步骤，
//     供评测断言请求形态。
//
// 本包不依赖 agentgo 其它 internal 包，是纯粹的 wire 级假端点。
package fakellm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------
// 脚本类型
// ---------------------------------------------------------------

// Script 一份 LLM 响应脚本：有序步骤 + 兜底响应。
type Script struct {
	Steps []Step `yaml:"steps" json:"steps"`
	// Default 全部步骤都不命中时的兜底响应；零值时兜底为固定中文文本。
	Default ResponseSpec `yaml:"default" json:"default"`
}

// Step 一个脚本步骤：匹配条件 + 响应。默认一次性消费；repeat: true 可反复命中。
type Step struct {
	Match   MatchSpec    `yaml:"match" json:"match"`
	Repeat  bool         `yaml:"repeat" json:"repeat"`
	Respond ResponseSpec `yaml:"respond" json:"respond"`
}

// MatchSpec 请求匹配条件（全部满足才命中）。
type MatchSpec struct {
	// Lane 限定车道（classifyLane 的结果：scheduler / worker / other）；空 = 不限。
	Lane string `yaml:"lane" json:"lane"`
	// Contains 列出的子串必须全部出现在原始请求体中。
	Contains []string `yaml:"contains" json:"contains"`
}

// ToolCallSpec 一个脚本化的工具调用。
type ToolCallSpec struct {
	ID        string         `yaml:"id" json:"id"`               // 空 = 自动生成 call_N
	Name      string         `yaml:"name" json:"name"`           // 工具名（必填）
	Arguments map[string]any `yaml:"arguments" json:"arguments"` // 结构化参数，产出时序列化为 JSON 字符串
}

// ErrorSpec 脚本化的 HTTP 错误响应（如 429/500/401）。
type ErrorSpec struct {
	Status int    `yaml:"status" json:"status"` // HTTP 状态码
	Body   string `yaml:"body" json:"body"`     // 错误原文；空 = 自动生成 OpenAI 错误形态
}

// ResponseSpec 一个脚本化响应。
type ResponseSpec struct {
	Text      string         `yaml:"text" json:"text"`             // assistant 正文
	ToolCalls []ToolCallSpec `yaml:"tool_calls" json:"tool_calls"` // 工具调用清单
	// FinishReason 覆盖终止原因（stop/tool_calls/length/content_filter）；
	// 空 = 推断：有 tool_calls 则 tool_calls，否则 stop。
	FinishReason string `yaml:"finish_reason" json:"finish_reason"`
	// Error 非空时本步骤产出 HTTP 错误（其余字段忽略）。
	Error *ErrorSpec `yaml:"error" json:"error"`
	// Stream true = SSE 流式响应形态；false（默认）= 一次性 JSON。
	Stream bool `yaml:"stream" json:"stream"`
	// usage 读数；缺省 prompt=120 / completion=12，保证评测指标非零。
	PromptTokens     int `yaml:"prompt_tokens" json:"prompt_tokens"`
	CompletionTokens int `yaml:"completion_tokens" json:"completion_tokens"`
}

// finishReason 推断本响应的终止原因。
func (r ResponseSpec) finishReason() string {
	if r.FinishReason != "" {
		return r.FinishReason
	}
	if len(r.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// usage 返回归一后的 usage 读数。
func (r ResponseSpec) usage() (prompt, completion int) {
	prompt, completion = r.PromptTokens, r.CompletionTokens
	if prompt <= 0 {
		prompt = 120
	}
	if completion <= 0 {
		completion = 12
	}
	return prompt, completion
}

// ---------------------------------------------------------------
// 脚本加载
// ---------------------------------------------------------------

// LoadScript 从文件加载脚本（YAML 或 JSON，按 yaml.v3 解析——JSON 是其子集）。
func LoadScript(path string) (*Script, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 LLM 脚本失败: %w", err)
	}
	return ParseScript(data)
}

// ParseScript 解析脚本字节并做轻量校验。
func ParseScript(data []byte) (*Script, error) {
	var s Script
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("LLM 脚本解析失败: %w", err)
	}
	for i, st := range s.Steps {
		if st.Respond.Error != nil && st.Respond.Error.Status <= 0 {
			return nil, fmt.Errorf("脚本步骤[%d] 的 error.status 必须为正整数", i)
		}
		for j, tc := range st.Respond.ToolCalls {
			if tc.Name == "" {
				return nil, fmt.Errorf("脚本步骤[%d] 的 tool_calls[%d] 缺少 name", i, j)
			}
		}
	}
	return &s, nil
}

// ---------------------------------------------------------------
// 车道分类
// ---------------------------------------------------------------

// 车道常量：classifyLane 对请求体的分类结果。
const (
	LaneScheduler = "scheduler" // 请求工具面含 report_done（Scheduler 专属）
	LaneWorker    = "worker"    // 请求工具面含 submit_task_result（执行代理）
	LaneOther     = "other"
)

// classifyLane 按请求体中出现的专属工具名分类车道。
// Scheduler 工具面含 report_done 且不含 submit_task_result，先判 scheduler。
func classifyLane(body []byte) string {
	s := string(body)
	if strings.Contains(s, `"report_done"`) {
		return LaneScheduler
	}
	if strings.Contains(s, `"submit_task_result"`) {
		return LaneWorker
	}
	return LaneOther
}

// ---------------------------------------------------------------
// 请求记录
// ---------------------------------------------------------------

// RecordedRequest 一个到达假端点的请求的事实记录（断言请求形态用）。
type RecordedRequest struct {
	Time            time.Time `json:"time"`
	Lane            string    `json:"lane"`
	Model           string    `json:"model"`
	StreamRequested bool      `json:"stream_requested"`
	MessageCount    int       `json:"message_count"`
	ToolNames       []string  `json:"tool_names"`
	Step            int       `json:"step"` // 命中的步骤序号；-1 = 兜底
	Body            []byte    `json:"body"` // 原始请求体
}

// ---------------------------------------------------------------
// Server
// ---------------------------------------------------------------

// Server 是脚本化假端点：httptest.Server + 步骤状态机 + 请求记录。
type Server struct {
	srv    *httptest.Server
	script *Script

	mu       sync.Mutex
	consumed []bool
	records  []RecordedRequest
	seq      int // 响应 id 单调序号
}

// NewServer 以脚本起假端点（监听 127.0.0.1 随机端口）。nil 脚本 = 空脚本（全部走兜底）。
func NewServer(script *Script) *Server {
	if script == nil {
		script = &Script{}
	}
	s := &Server{script: script, consumed: make([]bool, len(script.Steps))}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// URL 返回端点 base URL（直接可作 llm.base_url）。
func (s *Server) URL() string { return s.srv.URL }

// Close 关闭端点。
func (s *Server) Close() { s.srv.Close() }

// Records 返回请求记录的快照（按到达顺序）。
func (s *Server) Records() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.records))
	copy(out, s.records)
	return out
}

// chatRequest 是请求体的最小解码（只取记录与响应所需的字段）。
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []json.RawMessage `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// handle 是唯一的 HTTP 入口：任何 POST 都按 chat completion 处理
// （openai-go 会把 base_url 拼成 <base>/chat/completions，这里不校验路径，
// 避免 base_url 形态差异造成的假 404）。
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "fake-llm 只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = readAllLimited(r)
	}
	var req chatRequest
	_ = json.Unmarshal(body, &req)

	s.mu.Lock()
	stepIdx, step := s.pickLocked(classifyLane(body), body)
	rec := RecordedRequest{
		Time:            time.Now(),
		Lane:            classifyLane(body),
		Model:           req.Model,
		StreamRequested: req.Stream,
		MessageCount:    len(req.Messages),
		Step:            stepIdx,
		Body:            body,
	}
	for _, t := range req.Tools {
		rec.ToolNames = append(rec.ToolNames, t.Function.Name)
	}
	sort.Strings(rec.ToolNames)
	s.records = append(s.records, rec)
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	resp := s.script.Default
	if step != nil {
		resp = step.Respond
	}
	if resp.Error != nil {
		writeError(w, resp.Error)
		return
	}
	if resp.Stream {
		writeSSE(w, req.Model, resp, seq)
		return
	}
	writeOneShot(w, req.Model, resp, seq)
}

// pickLocked 按序选择第一个可命中的步骤：一次性步骤被消费后跳过，
// repeat 步骤可反复命中。返回步骤序号（-1 = 无命中，走兜底）。
func (s *Server) pickLocked(lane string, body []byte) (int, *Step) {
	for i := range s.script.Steps {
		st := &s.script.Steps[i]
		if s.consumed[i] && !st.Repeat {
			continue
		}
		if !matchStep(st, lane, body) {
			continue
		}
		if !st.Repeat {
			s.consumed[i] = true
		}
		return i, st
	}
	return -1, nil
}

// matchStep 判定步骤是否命中：车道限定 + 全部子串出现。
func matchStep(st *Step, lane string, body []byte) bool {
	if st.Match.Lane != "" && st.Match.Lane != lane {
		return false
	}
	for _, sub := range st.Match.Contains {
		if !strings.Contains(string(body), sub) {
			return false
		}
	}
	return true
}

// readAllLimited 读请求体（上限 8 MiB，防御异常大请求）。
func readAllLimited(r *http.Request) ([]byte, error) {
	const maxBody = 8 << 20
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32<<10)
	total := 0
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			total += n
			if total > maxBody {
				return nil, fmt.Errorf("请求体超过 %d 字节上限", maxBody)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// ---------------------------------------------------------------
// 响应产出（wire 格式）
// ---------------------------------------------------------------

// toolCallWire 是 OpenAI wire 格式的工具调用。
type toolCallWire struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// buildToolCalls 把脚本工具调用转成 wire 形态（arguments 序列化为 JSON 字符串）。
func buildToolCalls(specs []ToolCallSpec) []toolCallWire {
	out := make([]toolCallWire, 0, len(specs))
	for i, tc := range specs {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		args := "{}"
		if tc.Arguments != nil {
			if data, err := json.Marshal(tc.Arguments); err == nil {
				args = string(data)
			}
		}
		w := toolCallWire{ID: id, Type: "function"}
		w.Function.Name = tc.Name
		w.Function.Arguments = args
		out = append(out, w)
	}
	return out
}

// writeOneShot 产出一次性 JSON 响应。
func writeOneShot(w http.ResponseWriter, model string, resp ResponseSpec, seq int) {
	prompt, completion := resp.usage()
	message := map[string]any{"role": "assistant"}
	if resp.Text != "" || len(resp.ToolCalls) == 0 {
		message["content"] = resp.Text
	}
	if len(resp.ToolCalls) > 0 {
		message["tool_calls"] = buildToolCalls(resp.ToolCalls)
	}
	doc := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-fake-%d", seq),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": resp.finishReason(),
		}},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// writeSSE 产出 SSE 流式响应：role chunk → content deltas → tool_call chunks →
// finish chunk → usage chunk → [DONE]。与 openai-go 的 accumulator 对齐。
func writeSSE(w http.ResponseWriter, model string, resp ResponseSpec, seq int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	id := fmt.Sprintf("chatcmpl-fake-%d", seq)
	created := time.Now().Unix()
	chunk := func(delta map[string]any, finishReason any, withUsage bool) {
		var choices []any
		choices = append(choices, map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		})
		doc := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": choices,
		}
		data, _ := json.Marshal(doc)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	chunk(map[string]any{"role": "assistant"}, nil, false)
	if resp.Text != "" {
		// 正文按 16 rune 切片产出，模拟真实流式增量。
		runes := []rune(resp.Text)
		const sliceLen = 16
		for i := 0; i < len(runes); i += sliceLen {
			end := i + sliceLen
			if end > len(runes) {
				end = len(runes)
			}
			chunk(map[string]any{"content": string(runes[i:end])}, nil, false)
		}
	}
	for i, tc := range buildToolCalls(resp.ToolCalls) {
		chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    i,
			"id":       tc.ID,
			"type":     tc.Type,
			"function": tc.Function,
		}}}, nil, false)
	}
	chunk(map[string]any{}, resp.finishReason(), false)

	// usage chunk（客户端 stream_options.include_usage 时消费；不带 choices）。
	prompt, completion := resp.usage()
	usageDoc := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	data, _ := json.Marshal(usageDoc)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeError 产出脚本化的 HTTP 错误（OpenAI 错误形态）。
func writeError(w http.ResponseWriter, spec *ErrorSpec) {
	body := spec.Body
	if body == "" {
		doc := map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("fake-llm 脚本化错误（status=%d）", spec.Status),
			"type":    "fake_scripted_error",
			"code":    fmt.Sprintf("fake_%d", spec.Status),
		}}
		data, _ := json.Marshal(doc)
		body = string(data)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(spec.Status)
	_, _ = w.Write([]byte(body))
}
