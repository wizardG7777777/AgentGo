package userdef

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agentgo/internal/reactor"
	"agentgo/internal/spawn"
	"agentgo/internal/trace"
)

// 已知 EventKind 集合，启动期校验 `on:` 字段命中。
//
// 与 trace.EventKind 常量同步——新增 EventKind 时需同步加入此集合（v5 阶段事件清单稳定，
// Phase 5 内部不会再扩；v5.x 加新事件时此处也需更新）。
var knownEventKinds = map[trace.EventKind]struct{}{
	trace.KindTaskPublished:             {},
	trace.KindTaskClaimed:               {},
	trace.KindTaskSubmitted:             {},
	trace.KindTaskCompleted:             {},
	trace.KindTextOnlySubmission:        {},
	trace.KindTaskRetry:                 {},
	trace.KindTaskFailed:                {},
	trace.KindTaskCancelled:             {},
	trace.KindLLMCallStart:              {},
	trace.KindLLMCallEnd:                {},
	trace.KindToolCall:                  {},
	trace.KindToolResult:                {},
	trace.KindHistoryCompaction:         {},
	trace.KindHistoryTruncated:          {},
	trace.KindTokenStats:                {},
	trace.KindFileWritten:               {},
	trace.KindFileWriteQueued:           {},
	trace.KindProgressNotify:            {},
	trace.KindError:                     {},
	trace.KindAgentStateChanged:         {},
	trace.KindShellExecuted:             {},
	trace.KindShellTimeoutPending:       {},
	trace.KindShellTimeoutResolved:      {},
	trace.KindReactorSpawnDepthExceeded: {},
}

// Deps 聚合 loader 需要的全部外部依赖。
//
// nil 字段语义：
//   - Store=nil：发现 publish_task reactor → 启动期报错
//   - LLM/LLMFactory 均 nil：发现 invoke_llm reactor → 启动期报错
//   - LLMFactory=nil 且 invoke_llm.model 非空：启动期报错，避免静默忽略模型覆盖
//   - Mailbox=nil：发现 invoke_llm.send_message → 启动期报错
//   - Emitter=nil：emit_trace 退化到包级 trace.Emit
//   - KindEventTypes=nil：跳过 publish_task.kind 路由校验（测试场景）
type Deps struct {
	Store          PublishStore
	LLM            LLMCompleter
	LLMFactory     func(model string) LLMCompleter
	Mailbox        MailboxSender
	Emitter        TraceEmitter
	KindEventTypes map[string]string
	// SpawnHost 是 spawn_agent reactor 的依赖（通常由 internal/spawn.Manager 实现）。
	// 为 nil 时，发现 spawn_agent reactor → 启动期报错。
	SpawnHost spawn.SpawnHost
	// AgentKindOf 用于 §6.2 per-kind 过滤：根据事件中的 AgentID 反查它所属的 kind。
	// 未注册的 AgentID 应返回 ""，wrapper 视为不命中过滤。
	// 为 nil 时，YAML 含 kind: 字段的 reactor 启动期报错。
	AgentKindOf func(agentID string) string
	// LLMTimeout 是 invoke_llm 单次调用的超时；零值默认 60s。
	LLMTimeout time.Duration
}

// LoadFromFile 解析 reactors.yaml 并返回构造好的 reactor.Reactor 列表。
//
// 启动期校验：
//  1. YAML 语法 + schema 类型
//  2. `on:` 命中已知 EventKind
//  3. 四个动作字段恰好一个非 nil
//  4. PublishTask: Kind 非空 + description.File 在 projectRoot 内 + 模板路径有效
//  5. InvokeLLM: prompt.File 在 projectRoot 内 + output 三 sink 恰一非 nil
//  6. when 表达式可解析
//  7. 依赖完整性：动作所需 Deps 字段非 nil
func LoadFromFile(path string, projectRoot string, deps Deps) ([]reactor.Reactor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read reactors file %q: %w", path, err)
	}
	return Load(data, filepath.Dir(path), projectRoot, deps)
}

// Load 直接从 YAML bytes 加载，descBaseDir 是相对路径解析的基准（通常是 yaml 文件所在目录）。
func Load(data []byte, descBaseDir, projectRoot string, deps Deps) ([]reactor.Reactor, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse reactors yaml: %w", err)
	}
	if len(f.Reactors) == 0 {
		return nil, nil
	}
	if deps.Emitter == nil {
		deps.Emitter = defaultTraceEmitter{}
	}
	if deps.LLMTimeout <= 0 {
		deps.LLMTimeout = 60 * time.Second
	}

	out := make([]reactor.Reactor, 0, len(f.Reactors))
	for i, rc := range f.Reactors {
		r, err := buildReactor(rc, i, descBaseDir, projectRoot, deps)
		if err != nil {
			return nil, fmt.Errorf("reactors[%d]: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func buildReactor(rc ReactorConfig, idx int, descBaseDir, projectRoot string, deps Deps) (reactor.Reactor, error) {
	if rc.On == "" {
		return nil, fmt.Errorf("missing required field 'on'")
	}
	kind := trace.EventKind(rc.On)
	if _, ok := knownEventKinds[kind]; !ok {
		return nil, fmt.Errorf("unknown event kind %q in 'on'", rc.On)
	}

	actionCount := 0
	if rc.PublishTask != nil {
		actionCount++
	}
	if rc.InvokeLLM != nil {
		actionCount++
	}
	if rc.SpawnAgent != nil {
		actionCount++
	}
	if rc.Call != "" {
		actionCount++
	}
	if rc.Call == "" && len(rc.Args) > 0 {
		return nil, fmt.Errorf("args is only valid with call")
	}
	if actionCount == 0 {
		return nil, fmt.Errorf("must specify exactly one of: publish_task / invoke_llm / spawn_agent / call")
	}
	if actionCount > 1 {
		return nil, fmt.Errorf("must specify exactly one action, found %d", actionCount)
	}

	when, err := parseWhen(rc.When)
	if err != nil {
		return nil, err
	}

	name := rc.Name
	if name == "" {
		name = fmt.Sprintf("userdef-%s-%d", rc.On, idx)
	}

	// 校验 per-kind 过滤的 kind 存在（与 publish_task.kind 同源 KindEventTypes 映射）
	if rc.Kind != "" && deps.KindEventTypes != nil {
		if _, ok := deps.KindEventTypes[rc.Kind]; !ok {
			return nil, fmt.Errorf("unknown reactor kind %q (must match a declared agents[*].kind)", rc.Kind)
		}
	}

	var inner reactor.Reactor
	switch {
	case rc.PublishTask != nil:
		inner, err = buildPublishTask(name, kind, when, rc.PublishTask, descBaseDir, projectRoot, deps)
	case rc.InvokeLLM != nil:
		inner, err = buildInvokeLLM(name, kind, when, rc.InvokeLLM, descBaseDir, projectRoot, deps)
	case rc.SpawnAgent != nil:
		inner, err = buildSpawnAgent(name, kind, when, rc.SpawnAgent, descBaseDir, projectRoot, deps)
	case rc.Call != "":
		inner, err = buildCall(name, kind, when, rc.Call, rc.Args, deps)
	default:
		return nil, fmt.Errorf("unreachable: action validated above")
	}
	if err != nil {
		return nil, err
	}

	// per-kind 过滤包装：rc.Kind 非空时把 inner reactor 包装为 kindFilteredReactor。
	// AgentKindOf 必须可用，否则 wrapper 拿不到实际 kind 值，所有事件都会被过滤掉。
	if rc.Kind != "" {
		if deps.AgentKindOf == nil {
			return nil, fmt.Errorf("reactor kind=%q requires Deps.AgentKindOf, got nil", rc.Kind)
		}
		return &kindFilteredReactor{inner: inner, kind: rc.Kind, lookup: deps.AgentKindOf}, nil
	}
	return inner, nil
}

func buildCall(name string, kind trace.EventKind, when *whenCond, tool string, args map[string]string, deps Deps) (reactor.Reactor, error) {
	if _, ok := supportedCallTools[tool]; !ok {
		return nil, fmt.Errorf("call: tool %q not supported in v5 (supported: send_message)", tool)
	}
	// send_message 必填字段
	if tool == "send_message" {
		if args["to"] == "" {
			return nil, fmt.Errorf("call: send_message requires 'args.to'")
		}
		if args["content"] == "" {
			return nil, fmt.Errorf("call: send_message requires 'args.content'")
		}
		if err := validatePaths(args["to"]); err != nil {
			return nil, fmt.Errorf("call.args.to: %w", err)
		}
		if err := validatePaths(args["content"]); err != nil {
			return nil, fmt.Errorf("call.args.content: %w", err)
		}
		if err := validatePaths(args["type"]); err != nil {
			return nil, fmt.Errorf("call.args.type: %w", err)
		}
		if err := validatePaths(args["priority"]); err != nil {
			return nil, fmt.Errorf("call.args.priority: %w", err)
		}
		if deps.Mailbox == nil {
			return nil, fmt.Errorf("call: send_message requires Deps.Mailbox, got nil")
		}
	}
	return &callReactor{
		name:   name,
		onKind: kind,
		when:   when,
		tool:   tool,
		args:   args,
		sender: deps.Mailbox,
	}, nil
}

func buildSpawnAgent(name string, kind trace.EventKind, when *whenCond, a *SpawnAgentAction, descBaseDir, projectRoot string, deps Deps) (reactor.Reactor, error) {
	if a.BaseKind == "" {
		return nil, fmt.Errorf("spawn_agent: 'base_kind' is required")
	}
	if a.InitialTask == nil {
		return nil, fmt.Errorf("spawn_agent: 'initial_task' is required")
	}
	if deps.SpawnHost == nil {
		return nil, fmt.Errorf("spawn_agent requires Deps.SpawnHost, got nil")
	}
	// base_kind 校验：与 publish_task.kind 同一份 KindEventTypes 映射
	if deps.KindEventTypes != nil {
		if _, ok := deps.KindEventTypes[a.BaseKind]; !ok {
			return nil, fmt.Errorf("spawn_agent: unknown base_kind %q", a.BaseKind)
		}
	}
	if a.Lifecycle != "" && a.Lifecycle != "one_shot" && a.Lifecycle != "persistent" {
		return nil, fmt.Errorf("spawn_agent: unknown lifecycle %q (supported: one_shot; persistent is v5.x placeholder)", a.Lifecycle)
	}

	// 加载 initial_task.description（S7：支持 via_translator 分支）
	desc, err := loadDescriptionWithTranslator(a.InitialTask.Description, descBaseDir, projectRoot, deps)
	if err != nil {
		return nil, fmt.Errorf("spawn_agent.initial_task.description: %w", err)
	}

	// 构造 RuntimeOverride（数值字段 + 可选 system_prompt）
	override := spawn.RuntimeOverride{}
	var sysPromTpl *promptTemplate
	if a.Override != nil {
		if err := validateSpawnOverride(*a.Override); err != nil {
			return nil, err
		}
		override.Model = a.Override.Model
		override.AgentMaxLoops = a.Override.AgentMaxLoops
		override.TaskMaxRetries = a.Override.TaskMaxRetries
		override.EnforceCompactTokenThreshold = a.Override.EnforceCompactTokenThreshold
		override.ContextLimit = a.Override.ContextLimit
		if a.Override.SystemPrompt != nil {
			tpl, err := loadPrompt(*a.Override.SystemPrompt, descBaseDir, projectRoot)
			if err != nil {
				return nil, fmt.Errorf("spawn_agent.override.system_prompt: %w", err)
			}
			sysPromTpl = tpl
		}
	}

	return &spawnAgentReactor{
		name:       name,
		onKind:     kind,
		when:       when,
		baseKind:   a.BaseKind,
		override:   override,
		desc:       desc,
		sysPromTpl: sysPromTpl,
		lifecycle:  a.Lifecycle,
		host:       deps.SpawnHost,
	}, nil
}

// loadDescriptionWithTranslator 处理 spawn_agent.initial_task.description 的两种形态：
//
//  1. 普通 PromptSpec.File → 走 loadPrompt 静态模板
//  2. via_translator → translator_prompt 渲染后用 reactor LLM 二次加工
//
// via_translator 不允许嵌套（translator_prompt 内不能再有 via_translator）；
// 不允许与 file/url/inline 同时存在。LLMCompleter 必须可解析（通过 Deps.LLM 直接注入
// 或 Deps.LLMFactory("") 构造），缺则启动期报错。
func loadDescriptionWithTranslator(spec PromptSpec, baseDir, projectRoot string, deps Deps) (descRenderer, error) {
	if spec.ViaTranslator == nil {
		// 普通路径：ViaTranslator==nil 时走纯静态模板。
		tpl, err := loadPrompt(spec, baseDir, projectRoot)
		if err != nil {
			return nil, err
		}
		return &staticDesc{tpl: tpl}, nil
	}

	// via_translator 分支：外层 PromptSpec 只承载 via_translator；args 必须写在
	// translator_prompt 下，避免被静默忽略。
	if spec.File != "" || spec.URL != "" || spec.Inline != "" {
		return nil, fmt.Errorf("via_translator is mutually exclusive with file/url/inline")
	}
	if len(spec.Args) > 0 {
		return nil, fmt.Errorf("via_translator outer args are ignored; put args under translator_prompt")
	}

	// 解析 LLMCompleter：Deps.LLMFactory 优先（与 invoke_llm 一致），否则 Deps.LLM
	var llm LLMCompleter
	if deps.LLMFactory != nil {
		llm = deps.LLMFactory("")
	} else if deps.LLM != nil {
		llm = deps.LLM
	} else {
		return nil, fmt.Errorf("via_translator requires Deps.LLM or Deps.LLMFactory, got nil")
	}
	if llm == nil {
		return nil, fmt.Errorf("via_translator: resolved LLMCompleter is nil")
	}

	tplSpec := spec.ViaTranslator.TranslatorPrompt
	// 不允许 translator_prompt 内嵌套 via_translator——递归翻译会让行为难以推理
	if tplSpec.ViaTranslator != nil {
		return nil, fmt.Errorf("via_translator.translator_prompt cannot itself use via_translator (no nesting)")
	}
	tpl, err := loadPrompt(tplSpec, baseDir, projectRoot)
	if err != nil {
		return nil, fmt.Errorf("via_translator.translator_prompt: %w", err)
	}
	return &translatedDesc{translatorTpl: tpl, llm: llm, timeout: deps.LLMTimeout}, nil
}

func validateSpawnOverride(o SpawnOverride) error {
	switch {
	case o.AgentMaxLoops < 0:
		return fmt.Errorf("spawn_agent.override.agent_max_loops must be >= 0")
	case o.TaskMaxRetries < 0:
		return fmt.Errorf("spawn_agent.override.task_max_retries must be >= 0")
	case o.EnforceCompactTokenThreshold < 0:
		return fmt.Errorf("spawn_agent.override.enforce_compact_token_threshold must be >= 0")
	case o.ContextLimit < 0:
		return fmt.Errorf("spawn_agent.override.context_limit must be >= 0")
	}
	return nil
}

func buildPublishTask(name string, kind trace.EventKind, when *whenCond, a *PublishTaskAction, descBaseDir, projectRoot string, deps Deps) (reactor.Reactor, error) {
	if a.Kind == "" {
		return nil, fmt.Errorf("publish_task: missing required 'kind'")
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("publish_task requires Deps.Store, got nil")
	}
	eventType := a.EventType
	if deps.KindEventTypes != nil {
		declaredEventType, ok := deps.KindEventTypes[a.Kind]
		if !ok {
			return nil, fmt.Errorf("publish_task: unknown kind %q", a.Kind)
		}
		if eventType == "" {
			eventType = declaredEventType
		} else if eventType != declaredEventType {
			return nil, fmt.Errorf("publish_task: kind %q routes to event_type %q, got event_type %q", a.Kind, declaredEventType, eventType)
		}
	}
	tpl, err := loadPrompt(a.Description, descBaseDir, projectRoot)
	if err != nil {
		return nil, fmt.Errorf("publish_task.description: %w", err)
	}
	for i, depTpl := range a.Dependencies {
		if err := validatePaths(depTpl); err != nil {
			return nil, fmt.Errorf("publish_task.dependencies[%d]: %w", i, err)
		}
	}
	return &publishTaskReactor{
		name:         name,
		onKind:       kind,
		when:         when,
		desc:         tpl,
		kind:         a.Kind,
		eventType:    eventType,
		priority:     a.Priority,
		store:        deps.Store,
		depTemplates: a.Dependencies,
	}, nil
}

func buildInvokeLLM(name string, kind trace.EventKind, when *whenCond, a *InvokeLLMAction, descBaseDir, projectRoot string, deps Deps) (reactor.Reactor, error) {
	llm := deps.LLM
	if deps.LLMFactory != nil {
		llm = deps.LLMFactory(a.Model)
	} else if a.Model != "" {
		return nil, fmt.Errorf("invoke_llm.model requires Deps.LLMFactory, got only Deps.LLM")
	}
	if llm == nil {
		return nil, fmt.Errorf("invoke_llm requires Deps.LLM, got nil")
	}
	tpl, err := loadPrompt(a.Prompt, descBaseDir, projectRoot)
	if err != nil {
		return nil, fmt.Errorf("invoke_llm.prompt: %w", err)
	}
	sink, err := buildOutputSink(a.Output, descBaseDir, projectRoot, deps)
	if err != nil {
		return nil, fmt.Errorf("invoke_llm.output: %w", err)
	}
	return &invokeLLMReactor{
		name:       name,
		onKind:     kind,
		when:       when,
		prompt:     tpl,
		llm:        llm,
		output:     sink,
		llmTimeout: deps.LLMTimeout,
	}, nil
}

// buildOutputSink 校验 OutputSpec 恰好一个非 nil 子字段，构造对应 sink。
func buildOutputSink(spec OutputSpec, descBaseDir, projectRoot string, deps Deps) (outputSink, error) {
	count := 0
	if spec.WriteFile != nil {
		count++
	}
	if spec.SendMessage != nil {
		count++
	}
	if spec.EmitTrace != nil {
		count++
	}
	if count == 0 {
		return nil, fmt.Errorf("must specify exactly one of: write_file / send_message / emit_trace")
	}
	if count > 1 {
		return nil, fmt.Errorf("must specify exactly one output sink, found %d", count)
	}

	switch {
	case spec.WriteFile != nil:
		if spec.WriteFile.Path == "" {
			return nil, fmt.Errorf("write_file: 'path' is required")
		}
		if err := validatePaths(spec.WriteFile.Path); err != nil {
			return nil, fmt.Errorf("write_file.path: %w", err)
		}
		baseDir := projectRoot
		if baseDir == "" {
			baseDir = descBaseDir
		}
		return &writeFileSinkImpl{pathTpl: spec.WriteFile.Path, baseDir: baseDir, projectRoot: projectRoot}, nil

	case spec.SendMessage != nil:
		if deps.Mailbox == nil {
			return nil, fmt.Errorf("send_message requires Deps.Mailbox, got nil")
		}
		if spec.SendMessage.To == "" {
			return nil, fmt.Errorf("send_message: 'to' is required")
		}
		if err := validatePaths(spec.SendMessage.To); err != nil {
			return nil, fmt.Errorf("send_message.to: %w", err)
		}
		if cv := spec.SendMessage.ContentVar; cv != "" && cv != "output" {
			return nil, fmt.Errorf("send_message.content_var: only \"output\" is supported (got %q)", cv)
		}
		msgType := spec.SendMessage.Type
		if msgType == "" {
			msgType = "info" // mailbox.MsgTypeInfo
		}
		priority := spec.SendMessage.Priority
		if priority == "" {
			priority = "normal" // mailbox.PriorityNormal
		}
		return &sendMessageSinkImpl{
			toTpl:    spec.SendMessage.To,
			msgType:  msgType,
			priority: priority,
			sender:   deps.Mailbox,
		}, nil

	case spec.EmitTrace != nil:
		if spec.EmitTrace.Kind == "" {
			return nil, fmt.Errorf("emit_trace: 'kind' is required")
		}
		// 不强制 Kind 在 knownEventKinds 内：用户标记事件允许自定义 kind。
		return &emitTraceSinkImpl{
			kind:    trace.EventKind(spec.EmitTrace.Kind),
			emitter: deps.Emitter,
		}, nil
	}
	return nil, fmt.Errorf("unreachable: sink validated above")
}

// loadPrompt 启动期把 PromptSpec.File 读入内存并校验模板路径。
//
// 安全约束（与 v4 §11.5.2 system_prompt_file 同档）：
//   - URL / Inline 字段非空 → 报 "v5.x 未实现"
//   - File 路径解析为绝对路径，必须在 projectRoot 内（projectRoot 为空时跳过此检查）
//   - 文件必须存在 + 可读
//   - 文件内容 + args 中的所有 ${event.x} 引用必须命中已知 trace.Event 字段
func loadPrompt(spec PromptSpec, baseDir, projectRoot string) (*promptTemplate, error) {
	if spec.URL != "" {
		return nil, fmt.Errorf("prompt.url not implemented (v5.x)")
	}
	if spec.Inline != "" {
		return nil, fmt.Errorf("prompt.inline not implemented (v5.x)")
	}
	if spec.ViaTranslator != nil {
		return nil, fmt.Errorf("prompt.via_translator is only supported for spawn_agent.initial_task.description")
	}
	if spec.File == "" {
		return nil, fmt.Errorf("prompt.file is required")
	}

	abs := spec.File
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(baseDir, abs)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve prompt path: %w", err)
	}
	if projectRoot != "" {
		root, err := filepath.Abs(projectRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve project root: %w", err)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("prompt path %q is outside project root %q", abs, root)
		}
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read prompt file %q: %w", abs, err)
	}
	if err := validatePromptTemplate(string(content), spec.Args); err != nil {
		return nil, fmt.Errorf("validate prompt %q: %w", abs, err)
	}
	return &promptTemplate{content: string(content), args: spec.Args}, nil
}

// 注：S7 起 spawn_agent.initial_task.description.via_translator 已实现。
// 其他 PromptSpec 位置遇到 ViaTranslator 仍由 loadPrompt 直接 reject，避免把
// translator 语义扩散到 publish_task / invoke_llm / override.system_prompt。
