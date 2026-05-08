// Package userdef 是 v5 用户 YAML Reactor 加载层（Phase 5 + v5.x 增量）。
//
// 已实现动词（互斥，每个 reactor 恰好一个）：
//   - publish_task — 投递任务到公告板
//   - invoke_llm — 一次性 LLM 调用 + 三 sink 输出（write_file / send_message / emit_trace）
//   - spawn_agent — 启动 ad-hoc agent（可含 via_translator 二次加工）
//   - call — §6.1 B 选项：直接调用内置工具（v1 仅支持 send_message）
//
// 可选过滤维度：
//   - when — §6.1.7 条件表达式（7 算子，无逻辑组合）
//   - kind — §6.2 per-kind 粒度过滤（限定 source agent 的 kind）
//
// 设计依据：docs/activate/ReactiveSystem.md §6.1 / §6.2。
package userdef

// ReactorConfig 是 reactors.yaml 中单个 reactor 条目的 YAML 解析结果。
//
// 四个动作字段（PublishTask / InvokeLLM / SpawnAgent / Call）互斥——loader 校验恰一非 nil。
type ReactorConfig struct {
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	On   string `yaml:"on" json:"on"`                         // 必填，trace.EventKind 名称（如 "task_failed"）
	When string `yaml:"when,omitempty" json:"when,omitempty"` // 可选条件表达式
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"` // §6.2 per-kind 过滤：限定 source agent kind；空=全局

	// 四个动作（均已真实实现，loader 校验恰一非 nil）
	PublishTask *PublishTaskAction `yaml:"publish_task,omitempty" json:"publish_task,omitempty"`
	InvokeLLM   *InvokeLLMAction   `yaml:"invoke_llm,omitempty" json:"invoke_llm,omitempty"`
	SpawnAgent  *SpawnAgentAction  `yaml:"spawn_agent,omitempty" json:"spawn_agent,omitempty"`

	// Call 是 §6.1 B 选项：直接调用内置工具（无 LLM）。
	// YAML 写法：`call: send_message` + `args: {to: ..., content: ...}`。
	// v1 仅支持 send_message；其他工具会在 loader 阶段拒绝。
	Call string            `yaml:"call,omitempty" json:"call,omitempty"`
	Args map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
}

// PromptSpec 是三动词共享的 prompt 来源结构（§6.1.3）。
//
// v5 首版 File 字段真实生效；URL / Inline 是预留位，运行期遇到直接报错
// （§6.1.6 占位 schema fail-fast 约定）。ViaTranslator 仅在
// spawn_agent.initial_task.description 下真实生效。
type PromptSpec struct {
	File   string            `yaml:"file,omitempty" json:"file,omitempty"`
	URL    string            `yaml:"url,omitempty" json:"url,omitempty"`       // 占位，未实现
	Inline string            `yaml:"inline,omitempty" json:"inline,omitempty"` // 占位，未实现
	Args   map[string]string `yaml:"args,omitempty" json:"args,omitempty"`

	// ViaTranslator 是 §6.1.4 场景 4：description 由 reactor 独立 LLM 二次加工。
	// 仅 spawn_agent.initial_task.description 支持该字段；其他 PromptSpec 位置会报错。
	ViaTranslator *ViaTranslatorSpec `yaml:"via_translator,omitempty" json:"via_translator,omitempty"`
}

// ViaTranslatorSpec 配置 translator prompt；translator 输出会成为 spawned agent 的
// initial_task.description。
type ViaTranslatorSpec struct {
	TranslatorPrompt PromptSpec `yaml:"translator_prompt" json:"translator_prompt"`
}

// PublishTaskAction 对应 §6.1.4 场景 1：投递任务到公告板。
//
// Description 是任务文本，必填。EventType / Priority 可选，缺省走默认队列。
//
// Dependencies 把渲染后的任务 ID（通常是 ${event.task.id}）写到 Task.Dependencies，
// 让被派任务通过 dep 通道自动拿到上游任务的 LastResponse。常用于"text_only_submission
// → 派审核任务"这种场景：reactor 触发时上游还在跑，但 ClaimTask 会等上游 completed
// 再把任务交给下游，下游的 system prompt 里会自动出现"前置任务结果"段。
type PublishTaskAction struct {
	Kind         string     `yaml:"kind" json:"kind"` // 必填，路由到已声明的 agent kind
	EventType    string     `yaml:"event_type,omitempty" json:"event_type,omitempty"`
	Priority     int        `yaml:"priority,omitempty" json:"priority,omitempty"`
	Description  PromptSpec `yaml:"description" json:"description"`
	Dependencies []string   `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
}

// InvokeLLMAction 对应 §6.1.4 场景 2：一次性 LLM 调用（S4 已实现）。
//
// LLM 调用走 reactor 自带的独立 client（无工具 / 无 history / 无 system prompt 注入，
// 详见 §6.1.4 + 原则 5）。Output 是三选一的输出去向，启动期校验恰好一个非 nil。
type InvokeLLMAction struct {
	Model  string     `yaml:"model,omitempty" json:"model,omitempty"`
	Prompt PromptSpec `yaml:"prompt" json:"prompt"`
	Output OutputSpec `yaml:"output" json:"output"`
}

// OutputSpec 是 invoke_llm 的输出去向（三选一）。
//
// 启动期校验：恰好一个字段非 nil。运行时把 LLM 文本输出投递到该 sink。
type OutputSpec struct {
	WriteFile   *WriteFileSink   `yaml:"write_file,omitempty" json:"write_file,omitempty"`
	SendMessage *SendMessageSink `yaml:"send_message,omitempty" json:"send_message,omitempty"`
	EmitTrace   *EmitTraceSink   `yaml:"emit_trace,omitempty" json:"emit_trace,omitempty"`
}

// WriteFileSink 把 LLM 输出写到文件。
//
// Path 支持 ${event.x} 模板（启动期校验路径中所有变量引用合法；运行时渲染后的
// 实际路径必须在 ProjectRoot 内，否则拒绝写入）。
type WriteFileSink struct {
	Path string `yaml:"path" json:"path"`
}

// UnmarshalYAML 同时接受字符串短形式 (write_file: ./logs/x.md) 和结构形式 (write_file: {path: ...})。
// 短形式与 §6.1.4 spec 的 YAML 例子一致。
func (s *WriteFileSink) UnmarshalYAML(unmarshal func(any) error) error {
	var asStr string
	if err := unmarshal(&asStr); err == nil && asStr != "" {
		s.Path = asStr
		return nil
	}
	type raw WriteFileSink
	var r raw
	if err := unmarshal(&r); err != nil {
		return err
	}
	*s = WriteFileSink(r)
	return nil
}

// SendMessageSink 把 LLM 输出作为 mailbox 消息发送给指定 agent。
//
// To 支持 ${event.x} 模板。Type 默认 mailbox.MsgTypeInfo，Priority 默认 normal。
// LLM 输出文本进入 Message.Content，同时复制到 Summary 截断。
type SendMessageSink struct {
	To       string `yaml:"to" json:"to"`
	Type     string `yaml:"type,omitempty" json:"type,omitempty"`
	Priority string `yaml:"priority,omitempty" json:"priority,omitempty"`
	// ContentVar 是 spec §6.1.4 的标记字段，目前只接受 "output"（LLM 输出）。
	// 缺省也按 "output" 处理。保留字段是为了向前兼容未来"多变量"语义。
	ContentVar string `yaml:"content_var,omitempty" json:"content_var,omitempty"`
}

// EmitTraceSink 发射一条 trace 事件。
//
// Kind 是事件标签，不强制为 trace 内置 EventKind——允许用户自定义标记
// （但对应的 ${event.x} 路径仍按内置字段解析）。LLM 输出落入 ev.Description。
// 当前阶段：用户自定义 kind 不能被其他 reactor 通过 `on:` 订阅（on: 仍只接受内置
// EventKind）。等"两遍 loader"实现后可解锁，详见 §6.1.4 末注释。
type EmitTraceSink struct {
	Kind string `yaml:"kind" json:"kind"`
}

// SpawnAgentAction 对应 §6.1.4 场景 3-4：启动 ad-hoc agent（S6 已实现）。
//
// BaseKind 必须命中 cfg.Agents 中已声明的 kind；Override 中的字段按"零值=不覆盖"
// 语义合并到从 base 派生的 AgentRuntimeConfig 上。Lifecycle 当前仅 one_shot 真实生效。
type SpawnAgentAction struct {
	BaseKind    string            `yaml:"base_kind" json:"base_kind"`
	Override    *SpawnOverride    `yaml:"override,omitempty" json:"override,omitempty"`
	InitialTask *SpawnInitialTask `yaml:"initial_task" json:"initial_task"`
	Lifecycle   string            `yaml:"lifecycle,omitempty" json:"lifecycle,omitempty"` // one_shot / persistent (占位)
}

// SpawnOverride 是 spawn_agent.override 中允许覆盖的字段集合。
//
// **明确不可 override** 的字段：Kind / EventType / InstanceID / AllowedTools / Profile / Tools。
// 这些一旦覆盖会破坏 ad-hoc 路由（EventType）或工具/Gate 集合的闭合（Tools），
// 当前阶段不暴露。
type SpawnOverride struct {
	SystemPrompt                 *PromptSpec `yaml:"system_prompt,omitempty" json:"system_prompt,omitempty"`
	Model                        string      `yaml:"model,omitempty" json:"model,omitempty"`
	AgentMaxLoops                int         `yaml:"agent_max_loops,omitempty" json:"agent_max_loops,omitempty"`
	TaskMaxRetries               int         `yaml:"task_max_retries,omitempty" json:"task_max_retries,omitempty"`
	EnforceCompactTokenThreshold int         `yaml:"enforce_compact_token_threshold,omitempty" json:"enforce_compact_token_threshold,omitempty"`
	ContextLimit                 int         `yaml:"context_limit,omitempty" json:"context_limit,omitempty"`
}

// SpawnInitialTask 是 spawn_agent.initial_task 的内容。
//
// Description 走 PromptSpec（与 publish_task / invoke_llm 一致）；
// via_translator 模式由 PromptSpec.ViaTranslator 字段表达，loader 会走独立 LLM
// 二次加工路径。
type SpawnInitialTask struct {
	Description PromptSpec `yaml:"description" json:"description"`
}

// File 是顶层 reactors.yaml 文件结构。
type File struct {
	Reactors []ReactorConfig `yaml:"reactors" json:"reactors"`
}
