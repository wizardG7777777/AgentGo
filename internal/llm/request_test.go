package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func requestFixture(protocol Protocol) RequestSpec {
	return RequestSpec{Schema: RequestSchema, Identity: Identity{InvocationID: "调用一", SnapshotID: "快照一", ContextPolicyID: "context:default/v12", ToolRouterID: "工具一", OperationID: "操作一"}, Options: Options{Protocol: protocol, Model: "模型一", CapabilityDigest: "能力一", ProfileRef: "测试", ToolChoice: ToolChoice{Mode: ToolChoiceAuto}, OutputBudget: DefaultOutputBudget()}, Messages: []Message{{Role: "user", Content: "测试输入"}}}
}

func TestRequestSealsAllSemanticFields(t *testing.T) {
	s := requestFixture(ProtocolResponses)
	s.Tools = []ToolDef{{Name: "检查", Strict: true, Parameters: map[string]any{"type": "object"}}}
	r, err := Seal(s)
	if err != nil {
		t.Fatal(err)
	}
	s.Tools[0].Parameters["type"] = "array"
	s.Messages[0].Content = "篡改"
	if r.Spec().Tools[0].Parameters["type"] != "object" || r.Spec().Messages[0].Content != "测试输入" {
		t.Fatal("封存请求仍共享原始数据")
	}
	copy := r.Spec()
	copy.Tools[0].Strict = false
	r2, err := Seal(copy)
	if err != nil {
		t.Fatal(err)
	}
	if r.Digest() == r2.Digest() {
		t.Fatal("strict 未进入请求摘要")
	}
	if err := (Request{}).Validate(); err == nil {
		t.Fatal("接受未封存请求")
	}
}

func TestRequestContentCapabilityAndProtocol(t *testing.T) {
	s := requestFixture(ProtocolResponses)
	s.Messages = []Message{{Role: "user", Parts: []ContentPart{{Kind: "image", URL: "https://example.test/image.png"}}}}
	if _, err := Seal(s); err == nil {
		t.Fatal("未声明图片能力却接受请求")
	}
	s.Options.InputCapability = InputCapability{Images: true, Files: true, TokensPerImage: 1000, TokensPerFile: 2000, MaxPartBytes: 4096, MaxTotalBytes: 8192}
	if _, err := Seal(s); err != nil {
		t.Fatal(err)
	}
	s.Messages[0].Parts = []ContentPart{{Kind: "file", URL: "https://example.test/file.pdf"}}
	s.Options.Protocol = ProtocolChatCompletions
	if _, err := Seal(s); err == nil {
		t.Fatal("Chat 接受文件 URL")
	}
	s.Messages[0].Parts = []ContentPart{{Kind: "file", Data: []byte("文件正文"), Filename: "test.txt", MediaType: "text/plain"}}
	if _, err := Seal(s); err != nil {
		t.Fatal(err)
	}
	if size, err := MeasureRequest(s); err != nil || size <= 0 {
		t.Fatalf("文件未编码: %d %v", size, err)
	}
}

func TestStreamingProtocolsDeliverOrderedText(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolResponses, ProtocolChatCompletions} {
		t.Run(string(protocol), func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == ProtocolResponses {
					for _, chunk := range []string{`{"type":"response.output_text.delta","output_index":0,"item_id":"m1","delta":"你"}`, `{"type":"response.output_text.delta","output_index":0,"item_id":"m1","delta":"好"}`, `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"你好","annotations":[]}]}}`, `{"type":"response.completed","response":{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":4,"output_tokens":2}}}`} {
						fmt.Fprintf(w, "data: %s\n\n", chunk)
					}
				} else {
					for _, chunk := range []string{`{"id":"r1","choices":[{"index":0,"delta":{"role":"assistant","content":"你"},"finish_reason":null}]}`, `{"id":"r1","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`, `{"id":"r1","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}`, `[DONE]`} {
						fmt.Fprintf(w, "data: %s\n\n", chunk)
					}
				}
			}))
			defer server.Close()
			r, err := Seal(requestFixture(protocol))
			if err != nil {
				t.Fatal(err)
			}
			var events []Event
			result, err := NewTransport(TransportConfig{BaseURL: server.URL, APIKey: "测试"}).Invoke(context.Background(), r, func(e Event) { events = append(events, e) })
			if err != nil {
				t.Fatal(err)
			}
			if result.Content() != "你好" || body["stream"] != true {
				t.Fatalf("流式请求或结果错误: %s %+v", result.Content(), body)
			}
			var deltas []string
			for _, e := range events {
				if e.Kind == "text_delta" {
					deltas = append(deltas, e.Delta)
				}
				if e.InvocationID != "调用一" {
					t.Fatal("增量缺少调用身份")
				}
			}
			if !reflect.DeepEqual(deltas, []string{"你", "好"}) {
				t.Fatalf("增量不完整: %v", deltas)
			}
		})
	}
}

func TestStreamingRejectsEOFAndDoesNotRetry(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolResponses, ProtocolChatCompletions} {
		t.Run(string(protocol), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == ProtocolResponses {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"未完成\"}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"未完成\"}}]}\n\n")
				}
			}))
			defer server.Close()
			r, _ := Seal(requestFixture(protocol))
			_, err := NewTransport(TransportConfig{BaseURL: server.URL, APIKey: "测试"}).Invoke(context.Background(), r, nil)
			if err == nil || calls != 1 {
				t.Fatalf("提前 EOF 未拒绝或隐藏重试: %v 次数=%d", err, calls)
			}
		})
	}
}

func TestHTTPFailuresNeverRetryInsideL1(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"测试故障","type":"测试","code":"测试"}}`)
			}))
			defer server.Close()
			r, _ := Seal(requestFixture(ProtocolResponses))
			_, err := NewTransport(TransportConfig{BaseURL: server.URL, APIKey: "测试"}).Invoke(context.Background(), r, nil)
			f, ok := FromError(err)
			if !ok || calls != 1 || f.InvocationID != "调用一" {
				t.Fatalf("失败分类/关联或重试错误: %v %d", err, calls)
			}
			if status == 402 && f.Kind != FailureProviderQuotaExhausted {
				t.Fatalf("402 错误分类为 %s", f.Kind)
			}
		})
	}
}

func TestResultProjectionIsImmutable(t *testing.T) {
	r, err := NewResult(ResultData{Schema: ResultSchema, Items: []OutputItem{{Kind: OutputItemMessage, Text: "内容"}, {Kind: OutputItemFunctionCall, ToolCall: &ToolCall{ID: "一", Name: "检查", Arguments: map[string]any{"路径": "原始"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	r.ToolCalls()[0].Arguments["路径"] = "篡改"
	if r.ToolCalls()[0].Arguments["路径"] != "原始" || !strings.Contains(r.Content(), "内容") {
		t.Fatal("结果投影共享可变状态")
	}
}
