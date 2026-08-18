package fakellm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"agentgo/internal/llm"
)

// postRaw 向假端点发一个最小 chat completion 请求，返回状态码与响应体。
func postRaw(t *testing.T, url string, payload map[string]any) (int, []byte) {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["model"]; !ok {
		payload["model"] = "offline-model"
	}
	if _, ok := payload["messages"]; !ok {
		payload["messages"] = []map[string]string{{"role": "user", "content": "ping"}}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url+"/chat/completions", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp.StatusCode, body
}

// oneShotChoice 解码一次性响应的 choice。
type oneShotChoice struct {
	Message struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

func decodeChoice(t *testing.T, body []byte) oneShotChoice {
	t.Helper()
	var doc struct {
		Choices []oneShotChoice `json:"choices"`
		Usage   struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, body)
	}
	if len(doc.Choices) != 1 {
		t.Fatalf("choices 数 = %d，期望 1", len(doc.Choices))
	}
	if doc.Usage.PromptTokens <= 0 || doc.Usage.CompletionTokens <= 0 {
		t.Fatalf("usage 读数应为正: %+v", doc.Usage)
	}
	return doc.Choices[0]
}

func TestScript_TextToolCallErrorLength(t *testing.T) {
	script, err := ParseScript([]byte(`
steps:
  - match: {contains: ["第一轮"]}
    respond: {text: "你好，离线世界"}
  - match: {contains: ["第二轮"]}
    respond:
      tool_calls:
        - name: write_file
          arguments: {path: a.txt, content: "hi"}
  - match: {contains: ["限流"]}
    respond:
      error: {status: 429}
  - match: {contains: ["截断"]}
    respond: {text: "半句话", finish_reason: length}
default: {text: "兜底"}
`))
	if err != nil {
		t.Fatalf("脚本解析失败: %v", err)
	}
	srv := NewServer(script)
	defer srv.Close()

	// ① 文本响应
	code, body := postRaw(t, srv.URL(), map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "第一轮"}}})
	if code != 200 {
		t.Fatalf("文本响应状态码 = %d（%s）", code, body)
	}
	if c := decodeChoice(t, body); c.Message.Content != "你好，离线世界" || c.FinishReason != "stop" {
		t.Fatalf("文本响应内容不符: %+v", c)
	}

	// ② tool_calls 响应（一次性步骤被消费后不再命中）
	code, body = postRaw(t, srv.URL(), map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "第二轮"}}})
	if code != 200 {
		t.Fatalf("tool_calls 响应状态码 = %d", code)
	}
	c := decodeChoice(t, body)
	if c.FinishReason != "tool_calls" || len(c.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls 响应不符: %+v", c)
	}
	tc := c.Message.ToolCalls[0]
	if tc.Function.Name != "write_file" || !strings.Contains(tc.Function.Arguments, `"a.txt"`) {
		t.Fatalf("tool_call 内容不符: %+v", tc)
	}
	if tc.ID == "" {
		t.Fatalf("tool_call 缺 id（应自动生成）")
	}

	// ③ 脚本化 429
	code, body = postRaw(t, srv.URL(), map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "限流"}}})
	if code != 429 {
		t.Fatalf("错误步骤状态码 = %d，期望 429", code)
	}
	if !strings.Contains(string(body), "fake_scripted_error") {
		t.Fatalf("错误体应为 OpenAI 错误形态: %s", body)
	}

	// ④ length 截断
	code, body = postRaw(t, srv.URL(), map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "截断"}}})
	if code != 200 {
		t.Fatalf("截断响应状态码 = %d", code)
	}
	if c := decodeChoice(t, body); c.FinishReason != "length" {
		t.Fatalf("截断响应 finish_reason = %q，期望 length", c.FinishReason)
	}

	// ⑤ 已消费的一次性步骤不再命中 → 走兜底
	_, body = postRaw(t, srv.URL(), map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "第一轮"}}})
	if c := decodeChoice(t, body); c.Message.Content != "兜底" {
		t.Fatalf("一次性步骤被重复命中: %+v", c)
	}
}

func TestScript_RepeatAndLane(t *testing.T) {
	script, err := ParseScript([]byte(`
steps:
  - match: {lane: scheduler}
    repeat: true
    respond: {text: "调度车道"}
  - match: {lane: worker}
    respond: {text: "执行车道"}
default: {text: "兜底"}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(script)
	defer srv.Close()

	schedTools := []map[string]any{
		{"type": "function", "function": map[string]string{"name": "report_done"}},
	}
	workerTools := []map[string]any{
		{"type": "function", "function": map[string]string{"name": "submit_task_result"}},
	}

	// scheduler 车道 repeat 步骤可连续命中
	for i := 0; i < 2; i++ {
		_, body := postRaw(t, srv.URL(), map[string]any{"tools": schedTools})
		if c := decodeChoice(t, body); c.Message.Content != "调度车道" {
			t.Fatalf("第 %d 次 scheduler 命中失败: %+v", i, c)
		}
	}
	// worker 车道命中一次性步骤后，第二次回落兜底
	_, body := postRaw(t, srv.URL(), map[string]any{"tools": workerTools})
	if c := decodeChoice(t, body); c.Message.Content != "执行车道" {
		t.Fatalf("worker 首次命中失败: %+v", c)
	}
	_, body = postRaw(t, srv.URL(), map[string]any{"tools": workerTools})
	if c := decodeChoice(t, body); c.Message.Content != "兜底" {
		t.Fatalf("worker 二次应走兜底: %+v", c)
	}
	// 无专属工具 = other 车道 → 兜底
	_, body = postRaw(t, srv.URL(), nil)
	if c := decodeChoice(t, body); c.Message.Content != "兜底" {
		t.Fatalf("other 车道应走兜底: %+v", c)
	}
}

func TestScript_LatestToolResultPlaceholderRecursivelyExpandsResponseStrings(t *testing.T) {
	script, err := ParseScript([]byte(`
steps:
  - match: {contains: ["bind route"]}
    repeat: true
    respond:
      text: "using {{latest_tool_result.event_type}}"
      tool_calls:
        - name: submit_graph
          arguments:
            graph: '{"graph_id":"{{latest_tool_result.graph.id}}","route":"{{latest_tool_result.event_type}}"}'
            labels: ["team={{latest_tool_result.team_id}}", "stable"]
default: {text: "fallback"}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(script)
	defer srv.Close()

	messages := []map[string]any{
		{"role": "user", "content": "bind route"},
		{"role": "tool", "tool_call_id": "provision", "content": `{"event_type":"team:abc-123","team_id":"abc-123","graph":{"id":"g-dynamic"}}`},
	}
	code, body := postRaw(t, srv.URL(), map[string]any{"messages": messages})
	if code != http.StatusOK {
		t.Fatalf("占位符响应状态码 = %d（%s）", code, body)
	}
	choice := decodeChoice(t, body)
	if choice.Message.Content != "using team:abc-123" || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("展开后响应不符: %+v", choice)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(choice.Message.ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("工具参数不是 JSON: %v", err)
	}
	if args["graph"] != `{"graph_id":"g-dynamic","route":"team:abc-123"}` {
		t.Fatalf("graph 占位符未展开: %#v", args["graph"])
	}
	labels, ok := args["labels"].([]any)
	if !ok || len(labels) != 2 || labels[0] != "team=abc-123" {
		t.Fatalf("嵌套 slice 占位符未展开: %#v", args["labels"])
	}
	// repeat 步骤不得被首次展开就地污染；第二个工具结果应生成新 route。
	messages[1]["content"] = `{"event_type":"team:def-456","team_id":"def-456","graph":{"id":"g-second"}}`
	_, body = postRaw(t, srv.URL(), map[string]any{"messages": messages})
	choice = decodeChoice(t, body)
	if choice.Message.Content != "using team:def-456" ||
		!strings.Contains(choice.Message.ToolCalls[0].Function.Arguments, "team:def-456") {
		t.Fatalf("repeat 步骤复用了旧展开值: %+v", choice)
	}
	if got := script.Steps[0].Respond.Text; got != "using {{latest_tool_result.event_type}}" {
		t.Fatalf("原脚本被就地改写: %q", got)
	}
}

func TestScript_LatestToolResultPlaceholderMissingFieldFailsClosed(t *testing.T) {
	script, err := ParseScript([]byte(`
steps:
  - respond: {text: "{{latest_tool_result.event_type}}"}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(script)
	defer srv.Close()
	code, body := postRaw(t, srv.URL(), map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "no tool result"}},
	})
	if code != http.StatusInternalServerError || !strings.Contains(string(body), "event_type") {
		t.Fatalf("缺字段应 fail-closed: code=%d body=%s", code, body)
	}
}

func TestServer_Records(t *testing.T) {
	srv := NewServer(&Script{})
	defer srv.Close()
	postRaw(t, srv.URL(), map[string]any{
		"model": "m-x",
		"tools": []map[string]any{
			{"type": "function", "function": map[string]string{"name": "report_done"}},
			{"type": "function", "function": map[string]string{"name": "publish_task"}},
		},
	})
	recs := srv.Records()
	if len(recs) != 1 {
		t.Fatalf("记录数 = %d，期望 1", len(recs))
	}
	rec := recs[0]
	if rec.Lane != LaneScheduler || rec.Model != "m-x" || rec.Step != -1 {
		t.Fatalf("记录字段不符: %+v", rec)
	}
	if len(rec.ToolNames) != 2 || rec.ToolNames[0] != "publish_task" || rec.ToolNames[1] != "report_done" {
		t.Fatalf("工具清单应排序记录: %v", rec.ToolNames)
	}
	if !strings.Contains(string(rec.Body), `"m-x"`) {
		t.Fatalf("原始请求体应留存")
	}
}

// TestSDKClientContract 用真实 llm.SDKClient 打假端点，验证 wire 契约：
// 一次性文本、一次性 tool_calls、SSE 流式三形态都应被 SDK 正常解析。
func TestSDKClientContract(t *testing.T) {
	script, err := ParseScript([]byte(`
steps:
  - match: {contains: ["纯文本"]}
    respond: {text: "契约文本", prompt_tokens: 101, completion_tokens: 7}
  - match: {contains: ["调工具"]}
    respond:
      tool_calls:
        - name: read_file
          arguments: {path: x.txt}
  - match: {contains: ["流式"]}
    repeat: true
    respond:
      stream: true
      text: "流式正文一二三四五六七八九十"
      tool_calls:
        - name: write_file
          arguments: {path: s.txt, content: "v"}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(script)
	defer srv.Close()

	ctx := context.Background()
	client := llm.NewSDKClient(srv.URL(), "fake-key", "offline-model", "", 10*time.Second)

	// ① 一次性文本
	resp, err := client.Chat(ctx, []llm.Message{{Role: "user", Content: "纯文本"}}, nil)
	if err != nil {
		t.Fatalf("一次性文本调用失败: %v", err)
	}
	if resp.Content != "契约文本" || resp.FinishReason != llm.FinishReasonStop {
		t.Fatalf("一次性文本解析不符: %+v", resp)
	}
	if resp.Usage.PromptTokens != 101 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("usage 未透传: %+v", resp.Usage)
	}

	// ② 一次性 tool_calls
	resp, err = client.Chat(ctx, []llm.Message{{Role: "user", Content: "调工具"}}, nil)
	if err != nil {
		t.Fatalf("一次性 tool_calls 调用失败: %v", err)
	}
	if resp.FinishReason != llm.FinishReasonToolCalls || len(resp.ToolCalls) != 1 {
		t.Fatalf("tool_calls 解析不符: %+v", resp)
	}
	if resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Arguments["path"] != "x.txt" {
		t.Fatalf("tool_call 参数不符: %+v", resp.ToolCalls[0])
	}

	// ③ SSE 流式（含 tool_calls）
	streamClient := llm.NewSDKClientWithConfig(srv.URL(), "fake-key", "offline-model", "", 10*time.Second, llm.ClientConfig{Stream: true})
	resp, err = streamClient.Chat(ctx, []llm.Message{{Role: "user", Content: "流式"}}, nil)
	if err != nil {
		t.Fatalf("流式调用失败: %v", err)
	}
	if resp.Content != "流式正文一二三四五六七八九十" {
		t.Fatalf("流式正文聚合不符: %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "write_file" || resp.ToolCalls[0].Arguments["content"] != "v" {
		t.Fatalf("流式 tool_calls 聚合不符: %+v", resp.ToolCalls)
	}
	if resp.Usage.PromptTokens <= 0 || resp.Usage.CompletionTokens <= 0 {
		t.Fatalf("流式 usage 缺失: %+v", resp.Usage)
	}
}

// TestLoadScript 文件加载路径（YAML 与 JSON 同 parser）。
func TestLoadScript(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/script.json"
	if err := writeFile(path, `{"steps":[{"respond":{"text":"json 形态"}}]}`); err != nil {
		t.Fatal(err)
	}
	s, err := LoadScript(path)
	if err != nil {
		t.Fatalf("JSON 脚本加载失败: %v", err)
	}
	if len(s.Steps) != 1 || s.Steps[0].Respond.Text != "json 形态" {
		t.Fatalf("JSON 脚本内容不符: %+v", s)
	}
	if _, err := LoadScript(dir + "/不存在.yaml"); err == nil {
		t.Fatalf("缺失文件应报错")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
