package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const RequestSchema = "agentgo.model-request/v1"
const ResultSchema = "agentgo.model-result/v1"

// Identity 只关联调用事实，不授予工具或存储权限。
type Identity struct {
	GraphID         string `json:"graph_id,omitempty"`
	NodeID          string `json:"node_id,omitempty"`
	ActivationID    string `json:"activation_id,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	Loop            int    `json:"loop"`
	InvocationID    string `json:"invocation_id"`
	SnapshotID      string `json:"snapshot_id"`
	ContextPolicyID string `json:"context_policy_id"`
	ToolRouterID    string `json:"tool_router_id"`
	SessionID       string `json:"session_id,omitempty"`
	AgentID         string `json:"agent_id,omitempty"`
	TaskID          string `json:"task_id,omitempty"`
	AttemptID       string `json:"attempt_id,omitempty"`
	TurnID          string `json:"turn_id,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
}

// Options 是 L2 已解析的完整执行规格；L1 不使用客户端默认值补全。
type Options struct {
	Protocol          Protocol        `json:"protocol"`
	Model             string          `json:"model"`
	CapabilityDigest  string          `json:"capability_digest"`
	ProfileRef        string          `json:"profile_ref"`
	ReasoningEffort   string          `json:"reasoning_effort"`
	ToolChoice        ToolChoice      `json:"tool_choice"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
	OutputBudget      OutputBudget    `json:"output_budget"`
	InputCapability   InputCapability `json:"input_capability"`
}

// InputCapability 明确非文本能力及机械预算；零值只允许文本。
type InputCapability struct {
	Images         bool  `json:"images" yaml:"images"`
	Files          bool  `json:"files" yaml:"files"`
	MaxPartBytes   int64 `json:"max_part_bytes" yaml:"max_part_bytes"`
	MaxTotalBytes  int64 `json:"max_total_bytes" yaml:"max_total_bytes"`
	TokensPerImage int64 `json:"tokens_per_image" yaml:"tokens_per_image"`
	TokensPerFile  int64 `json:"tokens_per_file" yaml:"tokens_per_file"`
}

// ContentPart 的来源必须恰好选一个；本地文件须在 L3 读取后交付数据。
type ContentPart struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	URL       string `json:"url,omitempty"`
	Data      []byte `json:"data,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// ProtocolReplay 保留供应商明确返回的重放数据，不作为展示文本。
type ProtocolReplay struct {
	Protocol Protocol                   `json:"protocol"`
	Items    []json.RawMessage          `json:"items,omitempty"`
	Fields   map[string]json.RawMessage `json:"fields,omitempty"`
}

// RequestSpec 仅在 L2 封存之前可变。Request 不暴露共享 map/slice。
type RequestSpec struct {
	Schema   string    `json:"schema"`
	Identity Identity  `json:"identity"`
	Options  Options   `json:"options"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools"`
}

type Request struct {
	encoded string
	digest  string
}

func Seal(spec RequestSpec) (Request, error) {
	if err := validateSpec(spec); err != nil {
		return Request{}, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return Request{}, fmt.Errorf("封存模型请求失败: %w", err)
	}
	digest := sha256.Sum256(raw)
	return Request{encoded: string(raw), digest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func (r Request) Spec() RequestSpec {
	var spec RequestSpec
	_ = json.Unmarshal([]byte(r.encoded), &spec)
	return spec
}
func (r Request) Digest() string { return r.digest }
func (r Request) Validate() error {
	if r.encoded == "" {
		return fmt.Errorf("模型请求尚未封存")
	}
	digest := sha256.Sum256([]byte(r.encoded))
	if r.digest != "sha256:"+hex.EncodeToString(digest[:]) {
		return fmt.Errorf("模型请求摘要不一致")
	}
	return validateSpec(r.Spec())
}

func validateSpec(s RequestSpec) error {
	if s.Schema != RequestSchema {
		return fmt.Errorf("拒绝模型请求版本 %q", s.Schema)
	}
	for key, value := range map[string]string{"invocation_id": s.Identity.InvocationID, "snapshot_id": s.Identity.SnapshotID, "context_policy_id": s.Identity.ContextPolicyID, "tool_router_id": s.Identity.ToolRouterID, "model": s.Options.Model, "capability_digest": s.Options.CapabilityDigest, "profile_ref": s.Options.ProfileRef} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("完整模型请求缺少 %s", key)
		}
	}
	if _, err := ParseProtocol(string(s.Options.Protocol)); err != nil {
		return err
	}
	if err := s.Options.OutputBudget.Validate(); err != nil {
		return err
	}
	if err := s.Options.ToolChoice.Validate(); err != nil {
		return err
	}
	if s.Options.ToolChoice.Mode == "" {
		return fmt.Errorf("模型请求必须显式指定 tool_choice")
	}
	if len(s.Messages) == 0 {
		return fmt.Errorf("模型请求输入为空")
	}
	names := map[string]bool{}
	for _, t := range s.Tools {
		if t.Name == "" || names[t.Name] {
			return fmt.Errorf("工具定义缺少名称或重复")
		}
		names[t.Name] = true
	}
	if s.Options.ToolChoice.Mode == ToolChoiceRequired && len(s.Tools) == 0 {
		return fmt.Errorf("required 请求没有工具")
	}
	if s.Options.ToolChoice.Mode == ToolChoiceFunction && !names[s.Options.ToolChoice.Name] {
		return fmt.Errorf("指定工具不在冻结定义中")
	}
	return validateContent(s)
}

func validateContent(s RequestSpec) error {
	if err := s.Options.InputCapability.Validate(); err != nil {
		return err
	}
	var total int64
	c := s.Options.InputCapability
	for _, m := range s.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return &ErrUnknownRole{Role: m.Role}
		}
		if len(m.Parts) > 0 && m.Content != "" {
			return fmt.Errorf("消息不能同时指定文本快捷值与内容块")
		}
		if m.Replay != nil && m.Replay.Protocol != s.Options.Protocol {
			return fmt.Errorf("拒绝跨协议重放")
		}
		for _, p := range m.Parts {
			if p.Kind == "text" {
				if p.URL != "" || p.Data != nil || p.FileID != "" {
					return fmt.Errorf("文本块带有非文本来源")
				}
				continue
			}
			if m.Role != "user" {
				return fmt.Errorf("当前非文本输入只允许 user 角色")
			}
			n := 0
			if p.URL != "" {
				n++
			}
			if p.Data != nil {
				n++
			}
			if p.FileID != "" {
				n++
			}
			if n != 1 {
				return fmt.Errorf("非文本块必须指定唯一来源")
			}
			switch p.Kind {
			case "image":
				if !c.Images || c.TokensPerImage <= 0 {
					return fmt.Errorf("模型未声明图片能力或预算")
				}
				if p.FileID != "" {
					return fmt.Errorf("图片仅支持 URL 或内联数据")
				}
			case "file":
				if !c.Files || c.TokensPerFile <= 0 {
					return fmt.Errorf("模型未声明文件能力或预算")
				}
				if p.URL != "" && s.Options.Protocol != ProtocolResponses {
					return fmt.Errorf("Chat Completions 不支持文件 URL")
				}
				if p.Data != nil && p.Filename == "" {
					return fmt.Errorf("内联文件缺少 filename")
				}
			default:
				return fmt.Errorf("未知内容块类型 %q", p.Kind)
			}
			if p.URL != "" && !strings.HasPrefix(p.URL, "https://") && !strings.HasPrefix(p.URL, "http://") {
				return fmt.Errorf("非文本 URL 必须使用 HTTP(S)")
			}
			if p.Data != nil && (len(p.Data) == 0 || p.MediaType == "") {
				return fmt.Errorf("内联数据为空或缺少 media_type")
			}
			size := int64(len(p.Data) + len(p.URL) + len(p.FileID))
			total += size
			if c.MaxPartBytes <= 0 || c.MaxTotalBytes <= 0 || size > c.MaxPartBytes || total > c.MaxTotalBytes {
				return fmt.Errorf("非文本输入超过冻结字节预算")
			}
		}
	}
	return nil
}

// Event 是 L1 的模型增量，流式事件游标由 L2 另外生成。
type Event struct {
	InvocationID string `json:"invocation_id"`
	ItemID       string `json:"item_id"`
	ItemIndex    int64  `json:"item_index"`
	Kind         string `json:"kind"`
	Delta        string `json:"delta"`
}
type EventSink func(Event)
type Invoker interface {
	Invoke(context.Context, Request, EventSink) (Result, error)
}

// Result 只封存有序输出项；文本、reasoning 与工具调用均为派生视图。
type Result struct{ encoded string }
type ResultData struct {
	Schema       string         `json:"schema"`
	Items        []OutputItem   `json:"items"`
	FinishReason FinishReason   `json:"finish_reason"`
	Usage        Usage          `json:"usage"`
	Replay       ProtocolReplay `json:"replay"`
}

func NewResult(data ResultData) (Result, error) {
	if data.Schema != ResultSchema {
		return Result{}, fmt.Errorf("拒绝模型结果版本 %q", data.Schema)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Result{}, err
	}
	return Result{encoded: string(raw)}, nil
}
func (r Result) Data() ResultData {
	var d ResultData
	_ = json.Unmarshal([]byte(r.encoded), &d)
	return d
}
func (r Result) Content() string {
	var b strings.Builder
	for _, i := range r.Data().Items {
		if i.Kind == OutputItemMessage {
			b.WriteString(i.Text)
		}
	}
	return b.String()
}
func (r Result) Reasoning() string {
	var b strings.Builder
	for _, i := range r.Data().Items {
		if i.Kind == OutputItemReasoning {
			b.WriteString(i.Reasoning)
		}
	}
	return b.String()
}
func (r Result) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, i := range r.Data().Items {
		if i.Kind == OutputItemFunctionCall && i.ToolCall != nil {
			out = append(out, *i.ToolCall)
		}
	}
	return out
}
func (r Result) MarshalJSON() ([]byte, error) {
	if r.encoded == "" {
		return nil, fmt.Errorf("模型结果未封存")
	}
	return []byte(r.encoded), nil
}
func (r *Result) UnmarshalJSON(raw []byte) error {
	var d ResultData
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	v, err := NewResult(d)
	if err == nil {
		*r = v
	}
	return err
}

// Validate 验证显式媒体能力；零值仅表示文本能力，不代表无限预算。
func (c InputCapability) Validate() error {
	if c.MaxPartBytes < 0 || c.MaxTotalBytes < 0 || c.TokensPerImage < 0 || c.TokensPerFile < 0 {
		return fmt.Errorf("媒体能力预算不能为负")
	}
	if c.Images && c.TokensPerImage <= 0 || c.Files && c.TokensPerFile <= 0 {
		return fmt.Errorf("启用媒体能力必须指定 token 预算")
	}
	if (c.Images || c.Files) && (c.MaxPartBytes <= 0 || c.MaxTotalBytes < c.MaxPartBytes) {
		return fmt.Errorf("媒体能力必须指定有效的单项/总字节预算")
	}
	return nil
}
