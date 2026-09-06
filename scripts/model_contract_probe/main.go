// 模型契约定向探针只发送合成输入，不输出凭据、端点或供应商正文。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/contextcontract"
	"agentgo/internal/contextruntime"
	"agentgo/internal/contextstore"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
	"agentgo/internal/session"
	"github.com/google/uuid"
)

type report struct {
	CancelAfterDelta   bool   `json:"cancel_after_delta"`
	CancelFailureKind  string `json:"cancel_failure_kind,omitempty"`
	CancelUsageUnknown bool   `json:"cancel_usage_unknown"`
	Protocol           string `json:"protocol"`
	Success            bool   `json:"success"`
	Calls              int    `json:"calls"`
	Events             int    `json:"events"`
	FailureKind        string `json:"failure_kind,omitempty"`
	Stage              string `json:"stage"`
	FirstSSEMS         *int64 `json:"first_sse_ms,omitempty"`
	CompletedMS        *int64 `json:"completed_ms,omitempty"`
	PromptTokens       int    `json:"prompt_tokens"`
	CompletionTokens   int    `json:"completion_tokens"`
	MediaValidation    string `json:"media_validation"`
}

func main() {
	configPath := flag.String("config", "setting.yaml", "当前请求契约配置")
	protocol := flag.String("protocol", "responses", "显式选择协议，不自动回退")
	timeout := flag.Duration("timeout", 90*time.Second, "定向验证总时限")
	flag.Parse()
	r := run(*configPath, *protocol, *timeout)
	_ = json.NewEncoder(os.Stdout).Encode(r)
	if !r.Success {
		os.Exit(1)
	}
}

func run(configPath, protocol string, timeout time.Duration) report {
	out := report{Protocol: protocol, Stage: "configuration", MediaValidation: "not_declared"}
	cfg, err := config.LoadConfig(configPath, true)
	if err != nil {
		out.FailureKind = "configuration_rejected"
		return out
	}
	if cfg.LLM.APIKey == "" {
		out.FailureKind = "credentials_missing"
		return out
	}
	p, err := llm.ParseProtocol(protocol)
	if err != nil {
		out.FailureKind = "protocol_rejected"
		return out
	}
	root, err := os.MkdirTemp("", "agentgo-model-contract-")
	if err != nil {
		out.FailureKind = "temporary_store_failed"
		return out
	}
	defer os.RemoveAll(root)
	snapshots, err := contextstore.New(filepath.Join(root, "snapshots"))
	if err != nil {
		out.FailureKind = "snapshot_store_failed"
		return out
	}
	defer snapshots.Close()
	policies, err := policycatalog.NewDefault()
	if err != nil {
		out.FailureKind = "policy_failed"
		return out
	}
	options := cfg.LLM.InvocationOptions(cfg.LLM.DefaultModel)
	options.Protocol = p
	options.ProfileRef = "contract-probe"
	options.OutputBudget = llm.DefaultOutputBudget()
	options.OutputBudget.MaxCompletionTokens = 512
	options.ToolChoice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	group := uuid.NewString()
	runtime := contextruntime.Runtime{Assembler: contextruntime.NewAssembler(), Policies: policies, Snapshots: snapshots, Options: options,
		SessionID: func() string { return group }, Output: contextruntime.NewOutputService(func(record contextruntime.OutputRecord) error {
			return session.AppendModelOutputFile(filepath.Join(root, "model-outputs.jsonl"), record)
		})}
	transport := llm.NewTransport(llm.TransportConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Timeout: timeout})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	invoke := func(input contextruntime.Input) (llm.Result, error) {
		input.Identity = llm.Identity{InvocationID: uuid.NewString(), OperationID: group, AttemptID: group + "/attempt", TurnID: uuid.NewString(), SessionID: group, ContextPolicyID: policycatalog.ContextDefaultCurrent}
		input.Options = options
		input.OutputLimit = &options.OutputBudget
		input.ExecutionLeaseRef = "operation-no-dispatch:" + group
		input.ToolRouter.SnapshotID = "contract-tools:" + input.Identity.InvocationID
		compiled, err := runtime.Compile(ctx, input)
		if err != nil {
			return llm.Result{}, err
		}
		timing := llm.NewInvocationTiming(time.Now())
		callCtx := llm.WithInvocationTiming(ctx, timing)
		result, err := runtime.InvokeCompiled(callCtx, compiled, transport)
		out.Calls++
		facts := timing.Snapshot()
		out.Events += facts.StreamEventCount
		if out.FirstSSEMS == nil {
			out.FirstSSEMS = facts.FirstSSEEventMS
		}
		if facts.CompletedMS != nil {
			out.CompletedMS = facts.CompletedMS
		}
		if err == nil {
			out.PromptTokens += result.Data().Usage.PromptTokens
			out.CompletionTokens += result.Data().Usage.CompletionTokens
		}
		return result, err
	}
	fail := func(err error) report {
		out.FailureKind = "contract_rejected"
		if f, ok := llm.FromError(err); ok {
			out.FailureKind = string(f.Kind)
		}
		return out
	}
	nonce := uuid.NewString()
	tool := llm.ToolDef{Name: "contract_echo", Description: "返回本次验证的 nonce", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"nonce": map[string]any{"type": "string", "const": nonce}}, "required": []string{"nonce"}, "additionalProperties": false}}
	prompt := "调用 contract_echo 一次，nonce 为 " + nonce + "。不要输出其它内容。"
	out.Stage = "tool_call"
	first, err := invoke(contextruntime.Input{Instructions: contextruntime.Instructions{ProfileID: "contract-probe", Objective: prompt}, ToolRouter: contextruntime.ToolRouterBinding{Definitions: []llm.ToolDef{tool}}})
	if err != nil {
		return fail(err)
	}
	calls := first.ToolCalls()
	if len(calls) != 1 || calls[0].Name != tool.Name || calls[0].Arguments["nonce"] != nonce {
		return fail(fmt.Errorf("nonce 不匹配"))
	}
	data := first.Data()
	turn := contextruntime.SettledTurn{TurnID: "probe-first", Assistant: llm.Message{Role: "assistant", Content: first.Content(), ToolCalls: calls, Replay: &data.Replay}, ToolResults: []llm.Message{{Role: "tool", ToolCallID: calls[0].ID, Content: `{"ok":true}`}}}
	out.Stage = "exact_replay"
	second, err := invoke(contextruntime.Input{Instructions: contextruntime.Instructions{ProfileID: "contract-probe", Objective: prompt}, Conversation: []contextruntime.ConversationItem{{Turn: &turn}, {Message: &contextruntime.MessageBinding{Message: llm.Message{Role: "user", Content: "工具已完成。请直接回复已完成，不再调用工具。"}, Kind: contextcontract.FragmentUserTask, Section: contextcontract.SectionTaskContract, SourceRef: "probe-followup", Scope: contextcontract.ScopeTask, Authority: contextcontract.AuthorityAuthoritative, Freshness: contextcontract.FreshnessLive}}}})
	if err != nil {
		return fail(err)
	}
	if second.Content() == "" || len(second.ToolCalls()) != 0 {
		return fail(fmt.Errorf("缺少最终文本"))
	}
	out.Stage = "cancel_after_delta"
	cancelCtx, cancelCall := context.WithCancel(ctx)
	input := contextruntime.Input{Identity: llm.Identity{InvocationID: uuid.NewString(), OperationID: group, AttemptID: group + "/cancel", SessionID: group, ContextPolicyID: policycatalog.ContextDefaultCurrent}, Instructions: contextruntime.Instructions{ProfileID: "contract-probe", Objective: "请连续输出至少两百个不重复的中文词语。"}, Options: options, OutputLimit: &options.OutputBudget, ExecutionLeaseRef: "operation-no-dispatch:" + group, ToolRouter: contextruntime.ToolRouterBinding{SnapshotID: "cancel-tools:" + group}}
	compiled, compileErr := runtime.Compile(cancelCtx, input)
	if compileErr != nil {
		cancelCall()
		return fail(compileErr)
	}
	cancelTransport := cancelOnDelta{inner: transport, cancel: cancelCall, seen: &out.CancelAfterDelta}
	_, cancelErr := runtime.InvokeCompiled(cancelCtx, compiled, cancelTransport)
	out.Calls++
	cancelCall()
	if f, ok := llm.FromError(cancelErr); ok {
		out.CancelFailureKind = string(f.Kind)
		out.CancelUsageUnknown = f.UsageState != llm.UsageSettled
	}
	if !out.CancelAfterDelta || out.CancelFailureKind != string(llm.FailureCallerCancelled) {
		return fail(fmt.Errorf("未验证流式取消"))
	}
	out.Success = true
	out.Stage = "completed"
	return out
}

type cancelOnDelta struct {
	inner  llm.Invoker
	cancel context.CancelFunc
	seen   *bool
}

func (c cancelOnDelta) Invoke(ctx context.Context, r llm.Request, sink llm.EventSink) (llm.Result, error) {
	return c.inner.Invoke(ctx, r, func(e llm.Event) {
		if sink != nil {
			sink(e)
		}
		if !*c.seen {
			*c.seen = true
			c.cancel()
		}
	})
}
