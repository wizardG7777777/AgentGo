package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentgo/internal/effect"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/output"
	"agentgo/internal/roster"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// ErrRecoverable wraps an error to indicate it is recoverable (should trigger retry rollback).
type ErrRecoverable struct {
	Err error
}

func (e *ErrRecoverable) Error() string { return e.Err.Error() }
func (e *ErrRecoverable) Unwrap() error { return e.Err }

// ToolResult 保存单个 tool call 的执行结果，用于重建 OpenAI tool calling 协议消息。
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"` // 对应 tool call 的 ID
	Content    string `json:"content"`      // 工具执行结果（含错误信息）
}

// ExecuteResult holds the result of a single TaskExecutor invocation.
type ExecuteResult struct {
	Output           string
	ToolCalled       bool
	Finalized        bool           // 由 FinalizationChecker 设置，表示任务已完成
	AssistantContent string         // LLM 原始回复文本（assistant 消息的 content）
	ToolCalls        []llm.ToolCall // LLM 请求的工具调用列表
	ToolResults      []ToolResult   // 每个 tool call 对应的执行结果
	PromptTokens     int            // 本次 LLM 调用消耗的 prompt tokens
	CompletionTokens int            // 本次 LLM 调用消耗的 completion tokens
	// ExtraFields 是 assistant 消息里 openai-go 未识别的字段（如 DeepSeek V4 的
	// reasoning_content）。由 LLM 客户端透传上来，agent 应把它挂到 HistoryEntry
	// 上，buildMessages 下一轮重建 assistant 消息时原样回写给 API。
	ExtraFields map[string]json.RawMessage
}

// HistoryEntry 记录 ReAct 循环中单轮 TaskExecutor 调用的结果。
// 包含完整的 tool calling 信息，确保历史消息能正确重建为 OpenAI 协议格式。
//
// PromptTokens / CompletionTokens / Model 由 nextUpgrade_v4.md §11.7.3 引入，
// 供历史压缩的 token 估算做"实测锚定 + 新增估算"：
//   - PromptTokens：产生该条 assistant 回复时的实测 prompt token 数（来自 SDK Usage）
//   - CompletionTokens：同上，本轮 completion 实测值
//   - Model：产生该回复时使用的模型名（不同模型 tokenizer 不同，跨模型实测值不可比）
type HistoryEntry struct {
	Output           string                     `json:"output"`
	ToolCalled       bool                       `json:"tool_called"`
	AssistantContent string                     `json:"assistant_content"`
	ToolCalls        []llm.ToolCall             `json:"tool_calls"`
	ToolResults      []ToolResult               `json:"tool_results"`
	ExtraFields      map[string]json.RawMessage `json:"extra_fields,omitempty"`      // 层 1 通用透传：assistant 消息的非标字段
	IncomingMail     string                     `json:"incoming_mail,omitempty"`     // 非空时为收到的代理间邮件，注入为 user 角色消息
	PromptTokens     int                        `json:"prompt_tokens,omitempty"`     // §11.7.3 实测锚定：本轮 LLM 调用的实测 prompt tokens
	CompletionTokens int                        `json:"completion_tokens,omitempty"` // §11.7.3 实测锚定：本轮 completion tokens
	Model            string                     `json:"model,omitempty"`             // §11.7.3 模型切换基准重置：产生该条回复时使用的模型名
}

// TaskExecutor is a pluggable function that executes a task.
// For MVP this is injected as a mock; in production it will call the LLM.
type TaskExecutor func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error)

// TokenStats 是 Agent 级别的累计 Token 消耗统计（nextUpgrade_v4.md §11.7.3）。
// 每次 LLM 调用后累加，仅作 TUI/Web AgentCard 的实时视图数据源（经
// TokenStatsSnapshot 消费）；V6 起不再序列化进 trace——llm_call_end 事件
// 是唯一 token 账本，原 token_stats 累计事件已删除。
type TokenStats struct {
	TotalPromptTokens     int64
	TotalCompletionTokens int64
	CallCount             int
}

type Agent struct {
	ID        string
	EventType string
	Store     store.TaskStore
	Roster    roster.Roster
	Execute   TaskExecutor
	// ToolSwapper 是 executor 暴露的按任务工具注册表替换通道（*LLMExecutor
	// 实现 ToolRegistrySwapper）。任务携带 NodeCapability.Tools 时，
	// processTask 经它换入过滤视图、任务结束恢复；nil 表示 executor 不支持
	// 按任务过滤——携带工具子集的任务将 fail-closed 终止（无法保证
	// "LLM 只见子集"的隔离语义时不降级执行）。与 Execute 一样在装配期注入。
	ToolSwapper           ToolRegistrySwapper
	MaxRetries            int // 最大重试次数，0 表示不限制
	PollInterval          time.Duration
	IdleThreshold         int // 连续空轮询退出阈值，0 表示禁用
	CancelRegistry        *store.TaskCancelRegistry
	CompactTokenThreshold int // Layer 2 触发阈值（prompt tokens），默认 80000
	CompactKeepRecent     int // 压缩时保留最近 N 条历史，默认 3
	// Model 是该 Agent 当前生效的模型名，用于 HistoryEntry.Model 记录。
	// nextUpgrade_v4.md §11.7.3：跨模型实测值不可比，压缩阈值估算
	// 仅锚定当前模型一致的最近一条 PromptTokens > 0 条目。空串时退化为粗略估算。
	Model string
	// TokenStats 是 Agent 级别的累计 Token 消耗（§11.7.3），仅作 UI 实时视图
	// 数据源，不写入 trace 账本。
	// 运行期读写必须经 AddTokenStats / TokenStatsSnapshot（tokenMu 保护）——
	// UI 轮询 goroutine 与 ReAct 循环并发访问该字段（A3 修复）。
	TokenStats          TokenStats
	tokenMu             sync.Mutex
	OnTaskStart         func(taskID string)               // 任务开始处理时的回调，可选
	OnTaskEnd           func(taskID string, success bool) // 任务结束回调（defer 保证触发），可选
	FileCache           *FileStateCache                   // Agent 级别的文件读取缓存，可选
	Mailbox             *mailbox.Mailbox                  // 代理间通信收件箱，可选
	MailRegistry        *mailbox.Registry                 // 邮箱注册表，用于 DrainWithAck 自动回执
	FinalizationChecker FinalizationChecker               // 可选；用于 finalization tool 信号检查
	// SubmitState 暂存 submit_task_result 工具写入的结构化提交（已通过 ExpectedArtifacts 校验、待消费）。
	// finalization 短路分支 Take 命中时以其渲染文本替代 lastOutput 收尾（Cause=submit_task_result）；
	// nil 或 Take 未命中时走 report_done 兼容路径（lastOutput），行为与旧版完全一致。
	SubmitState *SubmitState

	// WorkspaceManager / WorkspaceActivator 是「按任务写时复制执行隔离」
	// （model.NodeCapability.Isolation）的执行面注入，runner 装配时设置：
	// 认领 Isolation 非 nil 的任务时 Materialize 任务 workspace 并经 Activator
	// 换入 overlay 视图（defer 恢复）；任务成功终态在 SubmitResult（标记
	// completed）之前经 MergeTask 合并回主根，成功后 Cleanup。任一 nil 时
	// 隔离任务在认领点 fail-closed（capability_violation）；无 Isolation 的
	// 任务零开销短路，行为完全不变。详见 workspace_merge.go。
	WorkspaceManager   WorkspaceLifecycleManager
	WorkspaceActivator WorkspaceViewActivator
	// ArtifactResolver 是 expected-artifacts 校验的磁盘兜底解析器（runner
	// 装配注入 NewArtifactPhysicalResolver）。nil 时校验退化为纯账本比对。
	ArtifactResolver ArtifactPhysicalResolver

	// EffectJournal 是 V6 §4 H2b 副作用账本（internal/effect），runner 装配
	// 注入；workspace 合并（mergeWorkspaceBeforeComplete）经它记录
	// prepared/settled。nil 时不记账（行为与引入账本前完全一致）。
	EffectJournal *effect.Journal

	// UserOutput 是用户可见内容的输出目标。非 nil 时，IsUserFacing 的自然文本完成
	// 和 report_done 等输出会写入此处，而不是直接 fmt.Printf 到 stdout。
	// TUI 模式下应设为一个把内容注入 Bubble Tea 消息流的 Writer。
	UserOutput io.Writer

	// ResultOutput 是任务最终结果块（"=== 任务完成 ==="）的输出目标。非 nil 时，
	// IsUserFacing 的自然文本完成结果写入此处；nil 时回退到 UserOutput
	// （兼容单 Writer 的既有装配）。bootstrap 将其接到 output.KindResult 事件
	// writer，让"这是最终结果"的分类在产生处完成，消费方不再做子串匹配。
	ResultOutput io.Writer

	// StreamOutput 同时接收合并后的在途快照（KindStream）和每次 LLM 调用
	// 唯一的不可变完成事实（KindTurn）。它与 UserOutput/ResultOutput 分离：
	// 流式快照原位替换一个 UI 项，完成轮次则追加到 Session 账本。nil 只
	// 禁用 UI/轮次发布，不禁用 SDK streaming。
	StreamOutput func(output.Event)

	// IsUserFacing 标记此 agent 是否直接对话用户（典型为 scheduler）。
	//
	// true 时：任何"自然文本完成"路径（!result.ToolCalled）都会自动把 lastOutput
	// 打印到 stdout，无需 LLM 显式调用 report_done。
	//
	// false 时（默认，worker / explorer 行为）：自然完成不打印——它们的输出由
	// scheduler 通过 board snapshot / 依赖结果注入间接消费。
	//
	// 设计动机（2026-04-27 架构修复）：用户提示词措辞（如"不用撰写报告"）可能让
	// LLM 词法匹配到工具名 `report_done` 而跳过该工具，导致用户终端 30+ 分钟看不到
	// 任何输出。把"用户可见输出"从 LLM 的工具选择决策中剥离出来，由 agent 框架层
	// 的"自然完成 = 用户回答"语义统一接管——跟 OpenCode 等主流 CLI agent 对齐，
	// 也跟 worker/explorer 的 ReAct 终止语义对齐。
	//
	// report_done 工具仍然保留（作为可选的 artifacts 校对块格式化工具），但不再是
	// 用户可见输出的唯一通道——LLM 不调它也能让用户看到内容。详见 v5 §11 设计文档。
	IsUserFacing bool

	// TextOnlyReportsDir 覆盖 persistTextOnlySubmission 的兜底落盘目录。
	// 空串时使用默认 ".agentgo/reports"（相对于进程 CWD）。生产线无需设置，
	// 测试用 t.TempDir() 注入隔离目录。详见 [[persistTextOnlySubmission]]。
	TextOnlyReportsDir string

	// OnTextOnlyPersisted 在 persistTextOnlySubmission 成功落盘后触发（可选）。
	// 参数为落盘文件的完整路径与正文内容，供装配层把 text-only 结果结构化记入
	// ResultSnapshot（E8：取代旧的 system.log 文本刮取恢复路径）。
	// 正文为空或写入失败时不触发。
	OnTextOnlyPersisted func(path, content string)

	// ProgressNotifyEnabled 控制进度通知功能是否启用。
	// 为 true 时，Agent 在文件写入、子任务发布或任务过半时通过 mailbox 发送进度消息。
	ProgressNotifyEnabled bool

	// Activity 是 TUI 使用的实时活动快照。它只做 best-effort 展示，不参与
	// 任务状态判定；nil 时保持非 TUI 路径的旧行为。
	Activity *ActivityTracker

	// stateGuard 是 v5 Phase 3 引入的 Agent 运行时状态机字段
	// （ReactiveSystem.md §7）。零值即 Idle，由 SetState/mustSetState 切换。
	// 字段非导出避免外部直接读写——必须经 SetState 走合法性校验 + emit trace。
	stateGuard stateGuard

	// interactionWaitMu / interactionWaiters 把并行工具调用的等待窗口
	// 折叠为一个 Agent 状态：第一个等待者进入时切到
	// waiting_interaction，最后一个退出时才恢复 processing。
	// LLM 同一次响应中的工具调用可能并行，所以这里不能只用 bool。
	interactionWaitMu  sync.Mutex
	interactionWaiters int

	// Memory 是 v5 Phase 1 Memory System 引入的记忆存储引用
	// （MemoryManageSystem.md MM5）。当前承载 team_snapshot / file_awareness
	// 两个 process-scope 上下文条目，取代 v4 时代的 TeamAwarenessHook 注入。
	// nil 时退化为不读取/不写入（行为等价于 v4 无 team-awareness 配置）。
	Memory memory.Store
	// TeamRefreshInterval 是 team_snapshot / file_awareness 的轮数刷新间隔。
	// <=0 时回退为默认 5。与 v4 TeamAwarenessConfig.SnapshotRefreshInterval 等价。
	TeamRefreshInterval int

	// TaskMemStore 是 V6 §3 Task Memory（CM2，internal/taskmem）的持久化
	// 存储。nil 时整链路关闭：不创建/更新/注入/checkpoint，行为与之前
	// 完全一致。装配见 runner.New（deps.TaskMemStore）。
	TaskMemStore *taskmem.Store

	// Modes 是 exec 轴模式源（V6 §4 H1 ExecutionLease 的 Policy 交集输入）：
	// exec=readonly 时租约从 BusinessTools 剔除写工具与 run_shell；
	// exec=strict 时租约记 ApprovalRequired=true。nil 等价 normal（兼容
	// 旧装配与单测直构）。runner / scheduler 装配注入与 Gate 相同的实例。
	Modes *modes.Store

	// PromptSource 是 V6 §2 P1a Prompt 有序编译的静态 prompt 身份源
	// （*LLMExecutor 实现 PromptIdentityProvider；runner / scheduler 装配
	// 注入，与 Execute/ToolSwapper 同一句柄）。processTask 在每个 attempt
	// 开始编译并冻结 Prompt Build（组件含 agent_role 全文 digest、
	// 当时工具清单、控制协议块），经 ctx 载体供 executor 把 Build.ID 并入
	// 每轮 context_manifest_built 事件。nil 时 agent_role/base_contract
	// 组件缺失（降级观测，不阻断任务）。
	PromptSource PromptIdentityProvider

	// loopFuse 是 emergency loop fuse 的测试覆盖口：>0 时替代包级常量
	// emergencyLoopFuse 生效。生产装配不得设置——fuse 是不可经任何 YAML/配置
	// 调低的程序缺陷防御兜底，不是正常终止条件（V6，见 processTask 循环顶部）。
	loopFuse int
}

// publishCompletedTurn 为一次 TaskExecutor 调用发布恰好一个不可变的
// UI/Session 轮次事实。优先使用公开 assistant 文本；仅填写 Output 的自然
// 文本 executor 保持兼容。工具参数/结果与 provider reasoning 元数据有意排除。
func (a *Agent) publishCompletedTurn(
	turnID, taskID string,
	loop int,
	result ExecuteResult,
	execErr error,
	lastStreamText string,
) {
	if a == nil || a.StreamOutput == nil || turnID == "" {
		return
	}
	text := result.AssistantContent
	if text == "" && !result.ToolCalled {
		text = result.Output
	}
	if text == "" {
		text = lastStreamText
	}
	toolNames := make([]string, 0, len(result.ToolCalls))
	for _, call := range result.ToolCalls {
		if call.Name != "" {
			toolNames = append(toolNames, call.Name)
		}
	}
	errText := ""
	if execErr != nil {
		errText = execErr.Error()
	}
	a.StreamOutput(output.Event{
		Kind:      output.KindTurn,
		AgentID:   a.ID,
		TaskID:    taskID,
		StreamID:  turnID,
		Loop:      loop,
		Text:      text,
		Done:      true,
		Error:     errText,
		ToolCalls: toolNames,
	})
}

// AddTokenStats 线程安全地累加一次 LLM 调用的 token 消耗，并返回累加后的
// 一致快照（供本 goroutine 后续使用，避免再次取锁）。
func (a *Agent) AddTokenStats(prompt, completion int64) TokenStats {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	a.TokenStats.TotalPromptTokens += prompt
	a.TokenStats.TotalCompletionTokens += completion
	a.TokenStats.CallCount++
	return a.TokenStats
}

// TokenStatsSnapshot 返回累计 token 统计的一致快照，供 UI 轮询等外部
// goroutine 读取（A3：此前直接读字段，与 ReAct 循环的写入构成数据竞争）。
func (a *Agent) TokenStatsSnapshot() TokenStats {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	return a.TokenStats
}

// emitTextOnlySubmissionIfNoArtifacts 在任务自然成功的两个出口（finalization 短路 +
// react_loop_exit:natural）之后调用，判别本次提交是否属于"代理什么都没落盘，仅吐出
// 一份文字汇报"——如是则同步做两件事：
//
//  1. **持久化兜底**：把 content 写到 .agentgo/reports/text_only_<task_id>.md。
//     2026-05-18 TUI 死锁事故暴露的问题——scheduler 走 text-only 路径时
//     正文只在内存里经 ResultOutput 流向 TUI 渲染，进程退出即丢，磁盘上无
//     任何拷贝。此处兜底保证正文落盘，TUI 故障也不丢内容。
//  2. **emit trace 事件**：让 reactor `on:` 直接订阅。
//
// 判别条件（全部满足）：
//   - content 非空（有文字产出）
//   - task.Artifacts 为空（整个任务生命周期内 0 个 file_written）
//
// Store=nil 或 GetTask 出错时跳过——只是少一次额外的可观察性事件，不影响主流程。
// 文件写入失败仅打 stderr WARNING，不阻断主流程（与 trace.Emit 的"尽力记录"语义一致）。
func (a *Agent) emitTextOnlySubmissionIfNoArtifacts(taskID, content string, loopsUsed int) {
	a.emitTextOnlySubmissionIfNoArtifactsOpt(taskID, content, loopsUsed, true)
}

// emitTextOnlySubmissionIfNoArtifactsOpt 与上同，recordSnapshot=false 时仅
// 落盘 + 发 trace 事件，不触发 OnTextOnlyPersisted 覆盖 ResultSnapshot——
// 供 finalization 短路路径使用：report_done 已通过 ResultOutput 记录了权威
// 结果，pre-tool 的 lastOutput 不应再覆盖它（A4×E8 接缝修复）。
func (a *Agent) emitTextOnlySubmissionIfNoArtifactsOpt(taskID, content string, loopsUsed int, recordSnapshot bool) {
	if content == "" || a.Store == nil {
		return
	}
	got, err := a.Store.GetTask(taskID)
	if err != nil || got == nil {
		return
	}
	if len(got.Artifacts) > 0 {
		return
	}
	a.persistTextOnlySubmissionOpt(taskID, content, recordSnapshot)
	trace.Emit(trace.Event{
		Kind:      trace.KindTextOnlySubmission,
		TaskID:    taskID,
		AgentID:   a.ID,
		OutputLen: len(content),
		LoopsUsed: loopsUsed,
	})
}

// persistTextOnlySubmission 把仅文字交付的 task 正文写到 .agentgo/reports/text_only_<task_id>.md。
//
// 这是 [[emitTextOnlySubmissionIfNoArtifacts]] 的兜底落盘，保证 TUI 渲染层即使
// 失灵也不丢正文。所有运行时落盘文件统一收敛到 .agentgo/ 下，便于隔离与清理。
//
// 失败语义：仅 stderr WARNING，不返回错误——持久化失败不应影响主流程的任务完成。
// .agentgo/reports/ 不存在时自动创建（mkdir -p）。
//
// 写入成功后触发 OnTextOnlyPersisted（若已装配），把路径与正文结构化交给
// 装配层记入 ResultSnapshot；上面的 log 行仅作观测日志，不再承担恢复职责（E8）。
//
// 测试钩子：可通过 TextOnlyReportsDir 字段覆盖默认 ".agentgo/reports" 目录，便于单测隔离。
func (a *Agent) persistTextOnlySubmission(taskID, content string) {
	a.persistTextOnlySubmissionOpt(taskID, content, true)
}

// persistTextOnlySubmissionOpt 同上；recordSnapshot=false 时跳过
// OnTextOnlyPersisted 回调（仅落盘 + 观测日志），见
// emitTextOnlySubmissionIfNoArtifactsOpt 的接缝说明。
func (a *Agent) persistTextOnlySubmissionOpt(taskID, content string, recordSnapshot bool) {
	if content == "" {
		return
	}
	dir := a.TextOnlyReportsDir
	if dir == "" {
		dir = ".agentgo/reports"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[agent %s] WARNING: 创建 %s 目录失败: %v", a.ID, dir, err)
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("text_only_%s.md", taskID))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		log.Printf("[agent %s] WARNING: 写入 text-only 兜底文件 %s 失败: %v", a.ID, path, err)
		return
	}
	log.Printf("[agent %s] text-only submission 已落盘: %s (%d 字节)", a.ID, path, len(content))
	if recordSnapshot && a.OnTextOnlyPersisted != nil {
		a.OnTextOnlyPersisted(path, content)
	}
}

// Run starts the agent's main loop. It polls for available tasks and processes them.
// It blocks until ctx is cancelled or no more work is available after a poll cycle.
func (a *Agent) Run(ctx context.Context) {
	a.run(ctx, nil)
}

// RunWithReady is Run with a one-shot readiness callback. ready is invoked
// only after QueryAvailable has succeeded for the first time, which proves the
// goroutine has entered the claim loop and can observe work on its route.
func (a *Agent) RunWithReady(ctx context.Context, ready func()) {
	a.run(ctx, ready)
}

func (a *Agent) run(ctx context.Context, ready func()) {
	defer func() {
		if a.Roster != nil {
			a.Roster.ReleaseAll(a.ID)
		}
	}()

	idleCount := 0
	readySignaled := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tasks, err := a.Store.QueryAvailable(a.EventType, a.ID)
		if err != nil {
			log.Printf("[agent %s] QueryAvailable error: %v", a.ID, err)
			idleCount++
			if a.shouldRetire(idleCount) {
				log.Printf("[agent %s] 空闲回收：连续空轮询 %d 次，退出", a.ID, idleCount)
				return
			}
			a.sleep(ctx)
			continue
		}
		if !readySignaled {
			readySignaled = true
			if ready != nil {
				ready()
			}
			// A lifecycle owner may cancel while the readiness callback is
			// waiting for route publication. Do not claim from the snapshot
			// returned before that cancellation.
			select {
			case <-ctx.Done():
				return
			default:
			}
		}

		if len(tasks) == 0 {
			idleCount++
			if a.shouldRetire(idleCount) {
				log.Printf("[agent %s] 空闲回收：连续空轮询 %d 次，退出", a.ID, idleCount)
				return
			}
			a.sleep(ctx)
			continue
		}

		// Try to claim the highest priority task
		claimed := false
		for _, task := range tasks {
			if err := a.Store.ClaimTask(a.ID, task.ID); err == nil {
				idleCount = 0
				taskCtx := ctx
				if a.CancelRegistry != nil {
					taskCtx = a.CancelRegistry.GetOrCreate(ctx, task.ID)
				}
				a.processTask(taskCtx, task.ID)
				claimed = true
				break
			}
		}

		if !claimed {
			idleCount++
			if a.shouldRetire(idleCount) {
				log.Printf("[agent %s] 空闲回收：连续空轮询 %d 次，退出", a.ID, idleCount)
				return
			}
			a.sleep(ctx)
		}
	}
}

// emergencyLoopFuse 是 ReAct 循环的应急保险丝（V6，docs/nextUpgrade-V6.md
// §5 升级思路 8）：循环计数越过该值时判定为程序缺陷造成的真死循环，任务
// 进入 blocked 终态并登记 replan，绝不自动重跑同一 Task。
//
// 它不是正常终止条件——正常 Loop 由结构化终态、用户/系统取消、任务
// deadline 与 token/成本预算共同约束；循环计数本身继续作为 trace/eval 的
// 观测指标存在。本常量故意不暴露任何 YAML/配置入口，不可调低；测试可经
// Agent.loopFuse 未导出字段覆盖。
const emergencyLoopFuse = 10000

// loopFuseLimit 返回本 Agent 生效的 fuse 值：未设置测试覆盖时恒为
// emergencyLoopFuse。
func (a *Agent) loopFuseLimit() int {
	if a.loopFuse > 0 {
		return a.loopFuse
	}
	return emergencyLoopFuse
}

func (a *Agent) processTask(ctx context.Context, taskID string) {
	task, err := a.Store.GetTask(taskID)
	if err != nil {
		log.Printf("[agent %s] GetTask error: %v", a.ID, err)
		return
	}

	// 进度通知：每任务级别的去重标志，在 processTask 入口初始化
	pFlags := progressFlags{}

	// === v5 Phase 3：Agent 状态机切入（ReactiveSystem.md §7.3.4）===
	// idle → processing 必须在所有 SetState(Terminating/Idle) 等 defer 注册前完成，
	// 否则 defer 跑时 prev 还是 idle，转换表会拒绝 idle→terminating 走 panic。
	a.mustSetState(AgentStateProcessing, "task_claimed:"+taskID, taskID)
	agentType := a.EventType
	if agentType == "" {
		agentType = "worker"
	}
	a.Activity.TaskClaimed(a.ID, agentType, taskID, task.Description)

	// terminatingCause 由闭包捕获，panic 路径与显式分支会覆盖默认值。
	// 默认 "react_loop_exit:natural" 覆盖正常完成（finalization / SubmitResult 后退出）；
	// runtime_loop_fuse / handleFailure 等显式分支跑前会赋值；panic 路径在合并 defer 中覆盖。
	terminatingCause := "react_loop_exit:natural"
	taskSuccess := false
	enteredTerminating := false
	enterTerminating := func(cause string) {
		if enteredTerminating {
			return
		}
		terminatingCause = cause
		enteredTerminating = true
		a.mustSetState(AgentStateTerminating, cause, taskID)
	}
	defer func() {
		a.Activity.TaskFinished(a.ID, taskID, taskSuccess, terminatingCause)
	}()

	// === defer 注册顺序（LIFO 反向 = 执行顺序）===
	// 注册 1（执行最后）：trace.CloseTask 关闭 trace 文件
	// 注册 2：SetState(Idle, "task_end_hook_done") — terminating → idle
	// 注册 3：OnTaskEnd 回调（scheduler 单例路径仍使用）
	// 注册 4（执行最先）：合并 defer：处理 panic（恢复 + L3 备忘 + FailTask + emit
	//                     KindTaskFailed + crashReport），再 SetState(Terminating, cause)
	defer trace.CloseTask(taskID)

	defer func() {
		// terminating → idle：Phase 3 第 6 个状态转换边
		a.mustSetState(AgentStateIdle, "task_end_hook_done", taskID)
	}()

	// 任务结束回调（defer 确保所有退出路径都触发；目前仅 holder 清理使用）
	if a.OnTaskEnd != nil {
		defer func() {
			a.OnTaskEnd(taskID, taskSuccess)
		}()
	}

	// 注：v5 Phase 4 后期 (MM7) AgentHook 子系统整体删除——PhaseTaskStart /
	// PhaseLoopPre / PhaseLoopPost / PhaseTaskEnd 调用点已移除。这些观察/注入
	// 语义现在由 trace.Event + Reactor 承接（KindAgentStateChanged / KindLLMCallStart
	// / KindTaskCompleted 等覆盖 phase 边界）；inject 类需求由 Memory System 承接。

	// === 合并 defer：panic 恢复 + processing → terminating 切换 ===
	//
	// panic 路径需要把 terminatingCause 设为 "react_loop_exit:panic"，
	// 这必须在 SetState(Terminating) 调用 *之前* 完成——解决办法是让二者
	// 共用同一个 defer：recover 后立即覆盖 cause，再走 SetState。
	// 注册顺序上本 defer 必须最后注册，才能在所有其它 defer 之前先跑。
	//
	// panic 路径不再生成交接备忘（TransferNote 机制已于 V6 CM4 删除）：
	// 重试接手上下文由 Task Memory + LastHistory 承担（task_memory.go）。
	defer func() {
		if rec := recover(); rec != nil {
			terminatingCause = "react_loop_exit:panic"
			log.Printf("[agent %s] 任务 %s processTask panic 被恢复: %v", a.ID, taskID, rec)
			enterTerminating(terminatingCause)
			// 终止任务，避免卡在 processing 状态
			reason := fmt.Sprintf("agent panic: %v", rec)
			if err := a.Store.FailTask(a.ID, taskID, reason); err != nil {
				log.Printf("[agent %s] panic 恢复后 FailTask error: %v", a.ID, err)
			}
			// §11.8 S11：panic-recovery 路径补 KindTaskFailed emit。
			trace.Emit(trace.Event{
				Kind:    trace.KindTaskFailed,
				TaskID:  taskID,
				AgentID: a.ID,
				Reason:  reason,
				Transition: &trace.Transition{
					PrevStatus: string(model.TaskStatusProcessing),
					NewStatus:  string(model.TaskStatusFailed),
					Cause:      "react_loop_exit:panic",
				},
			})
			// 尝试发送崩溃汇报（有 EventSource 时）
			if task != nil {
				a.sendCrashReport(task, taskID, reason)
			}
		}
		// processing → terminating：Phase 3 第 5 个状态转换边
		enterTerminating(terminatingCause)
	}()

	// Trace：记录任务被代理认领（KindTaskClaimed 是 task 状态机事件，
	// 与 SetState 触发的 KindAgentStateChanged 是两条不同事件流）。
	trace.Emit(trace.Event{
		Kind:    trace.KindTaskClaimed,
		TaskID:  taskID,
		AgentID: a.ID,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusPending),
			NewStatus:  string(model.TaskStatusProcessing),
			Cause:      "task_claimed:" + taskID,
		},
	})

	// 任务开始回调（用于 publish_subtask 跟踪当前任务 ID 等扩展点）
	if a.OnTaskStart != nil {
		a.OnTaskStart(taskID)
	}

	// === ExecutionLease（V6 §4 H1 冻结执行租约）应用点 ===
	// 认领点冻结/复用租约（计算式：NodeRequirement ∩ RouteCeiling ∩ Policy，
	// 见 execution_lease.go）；executor 可见工具集换入
	// BusinessTools ∪ ControlTools 的过滤视图，模型与隔离从租约冻结值读取，
	// 任务结束经 defer 全部恢复。与 FinalizationChecker/SubmitState 装配正交。
	lease, leaseReject := a.acquireExecutionLease(task)
	if lease == nil {
		// fail-closed（execution_lease_rejected 已 emit）：显式声明越界或
		// executor 无换入面——用超集工具集跑一个声明了子集的任务会打破
		// "LLM 只见子集"的隔离语义，不降级执行。Store 层 QueryAvailable 已按
		// CapabilityChecker 预过滤，这里是执行面的第二道防线。
		terminatingCause = "react_loop_exit:error"
		enterTerminating(terminatingCause)
		a.terminateTask(task, taskID, leaseReject, "capability_violation")
		return
	}
	// 工具视图换入（Lease 驱动，取代旧直接读 task.Capability.Tools 的短路）：
	// 视图 = BusinessTools ∪ ControlTools——显式声明漏带控制工具时节点仍能
	// 收尾。并集覆盖注册全集时跳过换入（恒等变换，无能力声明任务零开销）。
	if a.ToolSwapper != nil && leaseViewNeedsSwap(a.ToolSwapper.ToolRegistry(), lease) {
		full := a.ToolSwapper.ToolRegistry()
		filtered := full.Filtered(lease.ToolUnion())
		old := a.ToolSwapper.SwapToolRegistry(filtered)
		defer a.ToolSwapper.SwapToolRegistry(old)
		log.Printf("[agent %s] 任务 %s 执行租约生效：%s（%d/%d 已注册）",
			a.ID, taskID, describeLeaseTools(lease), filtered.RegisteredCount(), full.RegisteredCount())
	}
	// 模型覆盖：租约冻结值 ≠ 当前模型时换入（capability 覆盖的冻结产物）。
	// a.Model 在 HistoryEntry.Model 记录处读取，替换后自动跟随；defer 恢复。
	// wire 层请求模型经 llm.WithModelOverride 写入 ctx——processTask 的 ctx
	// 派生出每轮 execCtx 直达 client.Chat，SDKClient 读取后替换请求模型
	//（llm/client.go modelOverrideKey），因此覆盖对实际 API 请求同样生效。
	if lease.Model != "" && lease.Model != a.Model {
		origModel := a.Model
		a.Model = lease.Model
		ctx = llm.WithModelOverride(ctx, lease.Model)
		defer func() { a.Model = origModel }()
		log.Printf("[agent %s] 任务 %s 执行租约生效：模型覆盖 %s → %s", a.ID, taskID, origModel, lease.Model)
	}
	// 执行隔离（写时复制 overlay）：认领时物化任务 workspace 并把本 Runner 的
	// Swapper 换入该任务视图，defer 恢复。执行期读穿透主根、写落任务 workspace；
	// 成功终态由 mergeWorkspaceBeforeComplete 在 SubmitResult 前合并回主根。
	// 与工具子集同款 fail-closed：模式未知或执行面未装配时不降级执行。
	if lease.Workspace != "" {
		if lease.Workspace != model.IsolationModeWorkspace {
			reason := fmt.Sprintf("节点能力要求未知隔离模式 %q（当前仅支持 %q），不降级执行",
				lease.Workspace, model.IsolationModeWorkspace)
			log.Printf("[agent %s] 任务 %s %s", a.ID, taskID, reason)
			terminatingCause = "react_loop_exit:error"
			enterTerminating(terminatingCause)
			a.terminateTask(task, taskID, reason, "capability_violation")
			return
		}
		if a.WorkspaceManager == nil || a.WorkspaceActivator == nil {
			reason := "节点能力要求 workspace 执行隔离，但执行面未装配（Agent.WorkspaceManager/WorkspaceActivator 为 nil），不降级执行"
			log.Printf("[agent %s] 任务 %s %s", a.ID, taskID, reason)
			terminatingCause = "react_loop_exit:error"
			enterTerminating(terminatingCause)
			a.terminateTask(task, taskID, reason, "capability_violation")
			return
		}
		view, err := a.WorkspaceManager.Materialize(taskID)
		if err != nil {
			reason := fmt.Sprintf("workspace 物化失败: %v，不降级执行", err)
			log.Printf("[agent %s] 任务 %s %s", a.ID, taskID, reason)
			terminatingCause = "react_loop_exit:error"
			enterTerminating(terminatingCause)
			a.terminateTask(task, taskID, reason, "capability_violation")
			return
		}
		restore := a.WorkspaceActivator.Activate(view)
		defer restore()
		log.Printf("[agent %s] 任务 %s 执行租约生效：workspace 执行隔离（视图根 %s）", a.ID, taskID, view.Root())
		// KindWorkspaceMaterialized 由 Manager 负责 emit（types.go 契约），这里不重复。
	}

	// 清空文件缓存（任务切换时避免脏读）
	if a.FileCache != nil {
		a.FileCache.Clear()
	}

	depResults, err := a.Store.GetDependencyResults(taskID)
	if err != nil {
		log.Printf("[agent %s] GetDependencyResults error: %v", a.ID, err)
	}

	// 拉取依赖任务的 Artifacts（实际写入的文件路径），与 SubmitResult 文本合并
	// 注入到 user prompt 中，让下游 worker 知道上游具体写了哪些文件，避免凭空捏造
	depArtifacts, artErr := a.Store.GetDependencyArtifacts(taskID)
	if artErr != nil {
		log.Printf("[agent %s] GetDependencyArtifacts error: %v", a.ID, artErr)
	}
	if len(depArtifacts) > 0 {
		depResults = mergeArtifactsIntoDeps(depResults, depArtifacts)
	}

	var lastOutput string
	history := make([]HistoryEntry, 0)

	// CM1（V6 §3）：Context Manifest 侧信息载体，每个 attempt 一份，经 ctx
	// 传给 executor 只读——承载 Memory 段 UpdatedAt（新鲜度判定）与本 attempt
	// 内的压缩处置（L2 strategy / L3 truncated）。taskStartedAt 取认领时刻，
	// 是 Memory 段 live/stale 判定的比较基准。
	manifestInfo := newManifestSideInfo(time.Now())
	ctx = withManifestSideInfo(ctx, manifestInfo)

	// CM2（V6 §3）：Task Memory 加载或创建（attempt 恢复=加载既有，继续
	// 滚动；新建=初始化 Goal/Constraints 并落盘 + emit task_memory_created）。
	// Agent.TaskMemStore 为 nil 时返回 nil，整链路关闭。defer finalize 按
	// 任务最终状态收口：终态置 Sealed 封存，重试回滚只 checkpoint 不封存。
	taskMem := a.initTaskMemory(task)
	if taskMem != nil {
		ctx = withTaskMemCarrier(ctx, taskMem.carrier)
		defer taskMem.finalize(a, taskID)
	}

	// 重试时恢复之前的历史上下文，避免 LLM 丢失上下文重复操作。
	// 重试/接手的上下文恢复由 LastHistory（完整 tool_call/tool_result 序列）
	// + Task Memory（滚动工作状态，task_memory.go）共同承担；V6 CM4 已删除
	// TransferNote 精炼备忘机制。
	if len(task.LastHistory) > 0 {
		if err := json.Unmarshal(task.LastHistory, &history); err != nil {
			log.Printf("[agent %s] 反序列化历史记录失败，从空历史开始: %v", a.ID, err)
			history = make([]HistoryEntry, 0)
		} else {
			log.Printf("[agent %s] 任务 %s 恢复执行（retry=%d），载入 %d 条历史记录", a.ID, taskID, task.RetryCount, len(history))
		}
	}

	// CM4：依赖任务的 Task Memory 交接注入（取代已删除的
	// <upstream-transfer-notes> TransferNote 注入）。下游代理除「前置任务
	// 结果 + Artifacts」外，还能看到上游终结时封存的滚动工作状态。
	if block := a.buildDepTaskMemoryBlock(task, manifestInfo); block != "" {
		history = append(history, HistoryEntry{IncomingMail: block})
	}

	// 任务级注入点：team_snapshot / file_awareness 走 Memory System
	// （MemoryManageSystem.md MM6 取代 v4 TeamAwarenessHook）。
	// MM7 之后 AgentHook 子系统整体删除——再无 PhaseTaskStart 注入路径。
	if injected := a.injectMemoryContext(ctx, taskID, -1, false); injected != "" {
		history = append(history, HistoryEntry{
			IncomingMail: injected,
		})
	}

	// CM3（V6 §3）：Session Memory 召回注入（任务入口一次）。重试 attempt
	// 跳过——LastHistory 已含上次注入，重复注入会让 LLM 看到旧时间戳的
	// 重复块（与 team_snapshot 的 RetryCount>0 短路同款理由）。
	if task.RetryCount == 0 {
		if recalled := a.recallSessionMemory(ctx, taskID); recalled != "" {
			history = append(history, HistoryEntry{
				IncomingMail: recalled,
			})
		}
	}

	// V6 §2 P1a：Prompt 有序编译——每个 attempt 在执行租约冻结、工具视图
	// 换入之后编译一次并冻结（重试新 attempt 重新编译；输入不变则
	// Build.ID 稳定，重试天然复用同 Build.ID）。组件含 agent_role（system
	// prompt 全文，task.SystemPrompt 覆盖时是另一个 Build）、当时工具清单
	//（来自冻结租约/注册全集）与控制协议块；Build.Text 与 buildMessages
	// 的 system+user 首条逐字节一致——不改变任何消息字节，编译产物只
	// 用于身份与观测。核心指令在任务执行中不改变（现状语义的钉住）。
	// executor 每轮把 Build.ID 并入 context_manifest_built 事件
	//（prompt_bound 不独立成事件，避免同频双账本）。
	promptBuild := a.compilePromptBuild(task, depResults, lease)
	ctx = withPromptBuild(ctx, promptBuild)
	a.emitPromptCompiled(taskID, promptBuild)
	log.Printf("[agent %s] 任务 %s prompt 已编译冻结：build=%s 组件=%d digest=%s",
		a.ID, taskID, promptBuild.ID, len(promptBuild.Components), promptBuild.Digest)

	// Layer 2: token 累计跟踪，用于触发摘要压缩
	var totalPromptTokens int
	summarized := false // 每次任务执行最多触发一次摘要压缩

	compactThreshold := a.CompactTokenThreshold
	if compactThreshold <= 0 {
		compactThreshold = 80000
	}
	keepRecent := a.CompactKeepRecent
	if keepRecent <= 0 {
		keepRecent = 3
	}

	// V6：ReAct 循环不再有固定轮数上限（docs/nextUpgrade-V6.md §5 升级思路
	// 5/6/8）。循环只经以下路径退出：结构化终态（自然完成 / finalization
	// 短路）、ctx 取消（watchdog / 用户 / 系统）、错误处理（重试回滚或终止）、
	// 以及循环顶部 emergency fuse 对程序性死循环的兜底。循环计数 i 继续用于
	// trace / turns / 进度观测，不再是终止条件。
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			// 2026-04-25 P1 #2：取消类终态 trace 事件。由外部（watchdog /
			// cancel_task / 用户 /cancel / agent 关停）触发的 ctx 取消。
			cancelSource := CancelSourceFromContext(ctx)
			if cancelSource == "" && a.CancelRegistry != nil {
				cancelSource = a.CancelRegistry.Source(taskID)
			}
			if cancelSource != "" {
				terminatingCause = "react_loop_exit:cancel:" + cancelSource
			} else {
				terminatingCause = "react_loop_exit:cancel"
			}
			enterTerminating(terminatingCause)
			// FailTaskBySystem also cancels the task context so the worker exits.
			// In that case the Store has already emitted the authoritative failed
			// terminal event; emitting cancelled here would create contradictory
			// terminal facts. A real cancellation has Store status cancelled and
			// remains observable. Pending (retry) and other terminal states are
			// likewise owned by their state-transition path.
			emitCancelled := true
			if current, getErr := a.Store.GetTask(taskID); getErr == nil && current != nil {
				emitCancelled = current.Status == model.TaskStatusProcessing ||
					current.Status == model.TaskStatusCancelled
			}
			if emitCancelled {
				trace.Emit(trace.Event{
					Kind:    trace.KindTaskCancelled,
					TaskID:  taskID,
					AgentID: a.ID,
					Loop:    i,
					Reason:  ctx.Err().Error(),
					Transition: &trace.Transition{
						PrevStatus:   string(model.TaskStatusProcessing),
						NewStatus:    string(model.TaskStatusCancelled),
						CancelSource: cancelSource,
					},
				})
			}
			return
		default:
		}

		// emergency fuse：循环计数越过兜底阈值，判定为程序缺陷造成的真死循环。
		// 这不是正常终止条件（正常退出靠结构化终态 / 取消 / 错误处理），fuse
		// 触发后任务进 blocked + 登记 replan，绝不自动重跑同一 Task。
		// 放在 ctx 取消检查之后：外部取消是更高优先级的权威终止语义。
		if i >= a.loopFuseLimit() {
			terminatingCause = "react_loop_exit:runtime_loop_fuse"
			enterTerminating(terminatingCause)
			a.tripRuntimeLoopFuse(ctx, task, taskID, i, history)
			return
		}

		// 排水信箱：将收到的代理间消息注入历史，作为 user 角色消息；同时向发信方自动发送回执
		hasNewMail := false
		if a.Mailbox != nil {
			if msgs := a.Mailbox.DrainWithAck(a.MailRegistry); len(msgs) > 0 {
				history = append(history, HistoryEntry{
					IncomingMail: formatMailMessages(msgs),
				})
				hasNewMail = true
			}
		}

		// 每轮开头的上下文注入点。v5 Phase 1 把团队快照刷新移交
		// injectMemoryContext（参考 TeamRefreshInterval 与 hasNewMail）；
		// MM7 后 AgentHookRegistry 已删除。
		if injected := a.injectMemoryContext(ctx, taskID, i, hasNewMail); injected != "" {
			history = append(history, HistoryEntry{
				IncomingMail: injected,
			})
		}

		// 前置检查：如果设置了 FinalizationChecker 且已 finalized，
		// 说明上一轮调用了 finalization tool，立即终止 reactLoop。
		if a.FinalizationChecker != nil && a.FinalizationChecker.IsFinalized() {
			log.Printf("[agent %s] FinalizationChecker.IsFinalized()=true，终止 reactLoop (task=%s)", a.ID, taskID)
			// V6 §4 H1：finalizing 被接受即撤销执行租约——此后任何工具
			// dispatch 拒绝（防御层，与 L1 finalizing fence 互补）。
			a.revokeLeaseOnFinalizing(taskID)
			// 使用上一轮保存的 lastOutput 完成任务（不进行 ExpectedArtifacts 校验，因为 finalization tool 负责最终汇报）。
			// 若上一轮调用的是 submit_task_result，SubmitState 里暂存了已通过校验的结构化提交：
			// 其渲染文本替代 lastOutput 成为权威结果负载，
			// Transition.Cause 记为 submit_task_result 以区别于 report_done 兼容路径。
			resultText := lastOutput
			cause := "finalization_short_circuit"
			submitEvent := ""
			submitVerdict := ""
			submitEvidenceItems := ""
			submitStatus := ""
			submitBlockedReason := ""
			submitSummary := ""
			if a.SubmitState != nil {
				if sub, ok := a.SubmitState.Take(taskID); ok {
					resultText = sub.Format()
					cause = "submit_task_result"
					submitEvent = sub.Event
					submitVerdict = sub.Verdict
					submitEvidenceItems = sub.EvidenceItems
					submitStatus = sub.Status
					submitBlockedReason = sub.BlockedReason
					submitSummary = sub.Summary
					// 结构化提交路径：渲染文本是权威最终响应，覆盖 worker 的自由文本自述。
					if err := a.Store.RecordLastResponse(taskID, resultText); err != nil {
						log.Printf("[agent %s] RecordLastResponse error: %v", a.ID, err)
					}
				}
			}
			// 结构化 blocked（V6 §5）：与 completed 路径分岔——同一收尾事务内
			// 先落 blocked 终态（durable）、再为**非图任务**发布 replan 唤醒任务；
			// 不走 workspace 合并 / SubmitResult（blocked 不是成功终态，隔离
			// 副本不合并回主根），blocked 也永远不满足下游依赖。
			if submitStatus == SubmitStatusBlocked {
				enterTerminating("react_loop_exit:agent_reported_blocked")
				a.commitStructuredBlocked(task, taskMem, taskID, resultText, submitBlockedReason, submitSummary)
				return
			}
			// 写时复制隔离：合并必须在 SubmitResult（标记 completed）之前完成。
			// 合并失败/冲突时 helper 已把任务转 failed 并发布 replan 唤醒任务，直接返回。
			if !a.mergeWorkspaceBeforeComplete(ctx, task, taskID) {
				terminatingCause = "react_loop_exit:error"
				enterTerminating(terminatingCause)
				return
			}
			// Graph 事件键（C5b）：submit_task_result 携带的 event 在 SubmitResult 前
			// 写入 Results["event"]，graph-terminal-feed 随后把它并入 TerminalFact.Result
			// 驱动事件形态转移条件。位置约束：必须在 workspace 合并成功之后（合并失败
			// 任务转 failed，不应留下 ready 类事件键误导路由）、SubmitResult 之前
			// （Results 键随完成快照一起对外可见）。Store 不支持时静默跳过。
			if submitEvent != "" {
				if err := store.RecordResultField(a.Store, taskID, "event", submitEvent); err != nil {
					log.Printf("[agent %s] RecordResultField(event) error: %v", a.ID, err)
				}
			}
			// Graph 验收键（C6b）：与 event 同理，submit_task_result 携带的
			// verdict 在 SubmitResult 前写入 Results["verdict"]，驱动 acceptance
			// 节点的 $.verdict 路径形态转移条件。
			if submitVerdict != "" {
				if err := store.RecordResultField(a.Store, taskID, "verdict", submitVerdict); err != nil {
					log.Printf("[agent %s] RecordResultField(verdict) error: %v", a.ID, err)
				}
			}
			// Graph 验收证据键（G1b）：evidence_items 的原始 JSON 数组写入
			// Results["evidence"]，由 Graph Runtime 的服务端核验器逐条核验
			// （缺失或核验不通过时 verdict 不被采信）。
			if submitEvidenceItems != "" {
				if err := store.RecordResultField(a.Store, taskID, "evidence", submitEvidenceItems); err != nil {
					log.Printf("[agent %s] RecordResultField(evidence) error: %v", a.ID, err)
				}
			}
			enterTerminating(terminatingCause)
			if err := a.Store.SubmitResult(a.ID, taskID, resultText); err != nil {
				log.Printf("[agent %s] SubmitResult error: %v", a.ID, err)
				trace.Emit(trace.Event{
					Kind:    trace.KindError,
					TaskID:  taskID,
					AgentID: a.ID,
					Error:   "SubmitResult failed: " + err.Error(),
				})
			} else {
				// 跨轮短路也要 emit，否则 trace list 将任务错标为 running/loops=0。
				// LoopsUsed=i（不是 i+1）——本轮 LLM 调用尚未发生即短路退出。
				trace.Emit(trace.Event{
					Kind:      trace.KindTaskSubmitted,
					TaskID:    taskID,
					AgentID:   a.ID,
					OutputLen: len(resultText),
					LoopsUsed: i,
				})
				// finalization 短路：report_done / submit_task_result 已记录权威结果，
				// text-only 兜底仅落盘留档，不覆盖 ResultSnapshot（A4×E8 接缝）。
				a.emitTextOnlySubmissionIfNoArtifactsOpt(taskID, resultText, i, false)
				trace.Emit(trace.Event{
					Kind:    trace.KindTaskCompleted,
					TaskID:  taskID,
					AgentID: a.ID,
					Transition: &trace.Transition{
						PrevStatus: string(model.TaskStatusProcessing),
						NewStatus:  string(model.TaskStatusCompleted),
						Cause:      cause,
					},
				})
				// 结构化提交的收尾事务已 durable 提交 completed 终态
				//（report_done 兼容路径不算结构化提交，不重复记账）。
				if cause == "submit_task_result" {
					trace.Emit(trace.Event{
						Kind:    trace.KindTaskResultCommitted,
						TaskID:  taskID,
						AgentID: a.ID,
						Transition: &trace.Transition{
							PrevStatus: string(model.TaskStatusProcessing),
							NewStatus:  string(model.TaskStatusCompleted),
							Cause:      cause,
						},
					})
				}
			}
			return
		}

		// 构建只读副本传入 executor
		histCopy := make([]HistoryEntry, len(history))
		copy(histCopy, history)

		// 注入 agentID、taskID、循环轮次到 context，供 llm_executor 和工具层日志/trace 使用
		a.Activity.LoopStarted(a.ID, taskID, i)
		execCtx := WithAgentContext(ctx, a.ID, taskID, i)
		if a.Activity != nil {
			execCtx = WithActivityContext(execCtx, a.Activity)
		}
		turnID := fmt.Sprintf("%s:%s:%d:%d", a.ID, taskID, i, time.Now().UnixNano())
		lastStreamText := ""
		if a.Activity != nil || a.StreamOutput != nil {
			lastPublished := time.Time{}
			execCtx = llm.WithStreamHandler(execCtx, func(ev llm.StreamEvent) {
				if ev.AccumulatedContent != "" {
					lastStreamText = ev.AccumulatedContent
					if a.Activity != nil {
						a.Activity.LLMDelta(a.ID, taskID, i, ev.AccumulatedContent)
					}
				}
				if a.StreamOutput == nil || (ev.AccumulatedContent == "" && ev.Error == "") {
					return
				}
				now := time.Now()
				if !ev.Done && !lastPublished.IsZero() && now.Sub(lastPublished) < 50*time.Millisecond {
					return
				}
				lastPublished = now
				a.StreamOutput(output.Event{
					Kind: output.KindStream, AgentID: a.ID, TaskID: taskID,
					StreamID: turnID, Loop: i, Text: ev.AccumulatedContent,
					Done: ev.Done, Error: ev.Error,
				})
			})
		}
		result, execErr := a.Execute(execCtx, task, depResults, histCopy)
		a.publishCompletedTurn(turnID, taskID, i, result, execErr, lastStreamText)

		if execErr != nil {
			terminatingCause = "react_loop_exit:error"
			a.Activity.LLMEnd(a.ID, taskID, i, "", 0, execErr)
			enterTerminating(terminatingCause)
			// CM2：Attempt 结束前立即 checkpoint（含 L3 压缩前的状态保全）。
			taskMem.checkpoint(a, taskID, i, "attempt_end")
			a.handleFailure(task, taskID, execErr, history, manifestInfo)
			return
		}

		lastOutput = result.Output
		totalPromptTokens += result.PromptTokens

		// §11.7.3 TokenStats 内存计数器累计——仅作 UI 实时视图数据源。
		// V6 起不再 emit token_stats 事件（与 llm_call_end 重复的第二账本已删除）。
		a.AddTokenStats(int64(result.PromptTokens), int64(result.CompletionTokens))

		// 终止条件：LLM 没有调用工具（自然完成），或 Executor 返回 Finalized=true（finalization tool 信号）
		if !result.ToolCalled || result.Finalized {
			// IsUserFacing 自然文本完成打印（2026-04-27 架构修复）：
			//   - !result.ToolCalled：LLM 这一轮选择不调工具——result.Output 即是 LLM 的自然
			//     文本回复（来自 llm_executor.go:155 处 resp.Content 直接落 Output）
			//   - 仅当 IsUserFacing=true 时才打印——worker/explorer 路径保持 v3 兼容（不打印）
			//   - 不进 result.Finalized 分支——report_done 路径有自己的 fmt.Printf（含 artifacts
			//     校对块），且 result.Output 在该路径下是工具 ack 串而非 summary
			//
			// 详见 Agent.IsUserFacing 字段注释。
			if a.IsUserFacing && !result.ToolCalled && lastOutput != "" {
				// 结果块优先写 ResultOutput（KindResult 分类在产生处完成）；
				// 未装配 ResultOutput 时回退 UserOutput（单 Writer 用法兼容）。
				resultOut := a.ResultOutput
				if resultOut == nil {
					resultOut = a.UserOutput
				}
				if resultOut != nil {
					fmt.Fprintf(resultOut, "\n=== 任务完成 ===\n%s\n================\n\n", lastOutput)
				} else {
					log.Printf("=== 任务完成 ===\n%s\n================", lastOutput)
				}
			}

			// 持久化 worker 的最终响应文本——无论后续校验是否通过，scheduler 都能看到
			// worker 自述了什么。这是修复"失败路径上 lastOutput 被静默丢弃"的关键一环。
			if lastOutput != "" {
				if err := a.Store.RecordLastResponse(taskID, lastOutput); err != nil {
					log.Printf("[agent %s] RecordLastResponse error: %v", a.ID, err)
				}
			}

			// 校验 ExpectedArtifacts：如果发布者声明了预期产出文件，
			// 但任务结束时这些文件没有出现在 task.Artifacts 中，则任务失败重试。
			// 这是 Level 3 的硬性合约校验，防止 worker 在没有真正写文件的情况下"假装完成"。
			//
			// 三种结果：
			//   - Missing 非空：完全没写，必须重试
			//   - Drifted 非空但 Missing 空：basename 命中但路径漂移，视作成功，记 warning
			//   - 两者都空：完美通过
			check := checkExpectedArtifactsWithResolver(a.Store, taskID, a.ArtifactResolver)
			if len(check.Recovered) > 0 {
				log.Printf("[agent %s] 任务 %s 预期产物经磁盘兜底找回: %v", a.ID, taskID, check.Recovered)
			}
			if len(check.Missing) > 0 {
				reason := buildArtifactFailureReason(check)
				log.Printf("[agent %s] 任务 %s 缺少预期产出文件: %v (实际写入: %v)",
					a.ID, taskID, check.Missing, check.Actual)
				trace.Emit(trace.Event{
					Kind:    trace.KindError,
					TaskID:  taskID,
					AgentID: a.ID,
					Error:   reason,
				})
				// 把校验反馈作为 IncomingMail 注入历史，让下一次重试 LLM 能看见原因
				history = appendValidationFeedback(history, check)
				terminatingCause = "react_loop_exit:error"
				enterTerminating(terminatingCause)
				// CM2：Attempt 结束前立即 checkpoint。
				taskMem.checkpoint(a, taskID, i, "attempt_end")
				a.handleFailure(task, taskID, &ErrRecoverable{Err: fmt.Errorf("%s", reason)}, history, manifestInfo)
				return
			}
			if len(check.Drifted) > 0 {
				log.Printf("[agent %s] 任务 %s 路径漂移已容忍: %v", a.ID, taskID, check.Drifted)
			}

			// 写时复制隔离：合并必须在 SubmitResult（标记 completed）之前完成。
			// 合并失败/冲突时 helper 已把任务转 failed 并发布 replan 唤醒任务，直接返回。
			if !a.mergeWorkspaceBeforeComplete(ctx, task, taskID) {
				terminatingCause = "react_loop_exit:error"
				enterTerminating(terminatingCause)
				return
			}

			enterTerminating(terminatingCause)
			if err := a.Store.SubmitResult(a.ID, taskID, lastOutput); err != nil {
				log.Printf("[agent %s] SubmitResult error: %v", a.ID, err)
				trace.Emit(trace.Event{
					Kind:    trace.KindError,
					TaskID:  taskID,
					AgentID: a.ID,
					Error:   "SubmitResult failed: " + err.Error(),
				})
			} else {
				taskSuccess = true
				trace.Emit(trace.Event{
					Kind:      trace.KindTaskSubmitted,
					TaskID:    taskID,
					AgentID:   a.ID,
					OutputLen: len(lastOutput),
					LoopsUsed: i + 1,
				})
				a.emitTextOnlySubmissionIfNoArtifacts(taskID, lastOutput, i+1)
				trace.Emit(trace.Event{
					Kind:    trace.KindTaskCompleted,
					TaskID:  taskID,
					AgentID: a.ID,
					Transition: &trace.Transition{
						PrevStatus: string(model.TaskStatusProcessing),
						NewStatus:  string(model.TaskStatusCompleted),
						Cause:      "react_loop_exit:natural",
					},
				})
			}
			return
		}

		// 流式进度写回：每步工具执行结果追加到 Store，供 Scheduler 快照读取
		if err := a.Store.AppendOutput(a.ID, taskID, result.Output); err != nil {
			log.Printf("[agent %s] AppendOutput error: %v", a.ID, err)
		}

		// ToolCalled == true：追加到历史，继续循环
		// PromptTokens / CompletionTokens / Model 用于 §11.7.3 实测锚定，供历史压缩
		// 的 token 估算使用。Model 字段为空串时（Agent
		// 未注入模型名）退化为 v3 行为——估算时不做模型一致性筛选。
		history = append(history, HistoryEntry{
			Output:           result.Output,
			ToolCalled:       result.ToolCalled,
			AssistantContent: result.AssistantContent,
			ToolCalls:        result.ToolCalls,
			ToolResults:      result.ToolResults,
			ExtraFields:      result.ExtraFields, // 层 1：透传 reasoning_content 等非标字段
			PromptTokens:     result.PromptTokens,
			CompletionTokens: result.CompletionTokens,
			Model:            a.Model,
		})

		// 进度通知：在 history append 之后、PhaseLoopPost 之前发送
		a.progressNotify(ctx, taskID, i, result, &pFlags)

		// CM2（V6 §3）：settled Turn 收口——从结构化账本收集本轮事实滚动
		// 更新 Task Memory；仅实质变化时调版本/落盘/发 task_memory_updated。
		taskMem.applySettledTurn(a, taskID, result, i)

		// 注：MM7 之后 PhaseLoopPost AgentHook 调用点已删除——观察类需求走 trace.Emit
		// （ReactLoop 的逐轮节奏可通过 KindLLMCallEnd / KindToolResult 等事件还原）。

		// Layer 1: 清理旧的高输出工具结果
		snipOldToolResults(history, keepRecent)

		// Layer 2: token 累计超过阈值时触发摘要压缩（每次任务最多一次）
		if !summarized && totalPromptTokens > compactThreshold {
			tokensBefore := totalPromptTokens
			entriesBefore := len(history)
			// CM2：历史压缩前强制 checkpoint——即将被压缩的旧轮次其关键
			// 事实须已沉淀进 Task Memory 并落盘。
			taskMem.checkpoint(a, taskID, i, "history_compaction")
			history = compressHistory(history, keepRecent)
			summarized = true
			strategy := fmt.Sprintf("summary+keep_recent=%d", keepRecent)
			// CM1：回填压缩处置——后续轮次的 Manifest 中 history 段
			// Disposition 记为 compressed:<strategy>。
			manifestInfo.l2Strategy = strategy
			log.Printf("[agent %s] 任务 %s 触发历史摘要压缩，当前 prompt tokens: %d", a.ID, taskID, totalPromptTokens)
			trace.Emit(trace.Event{
				Kind:               trace.KindHistoryCompaction,
				TaskID:             taskID,
				AgentID:            a.ID,
				Loop:               i,
				PromptTokensBefore: tokensBefore,
				PromptTokensAfter:  0, // 实际值要等下次 LLM 调用才能拿到，这里只记录"压缩前"信号
				Strategy:           strategy,
				KeptEntries:        entriesBefore,
			})
		}
	}
}

// tripRuntimeLoopFuse 是 emergency loop fuse 触发时的统一收口（V6，见
// processTask 循环顶部）。它替代已删除的「MaxLoops 耗尽 → 交接备忘 →
// 回滚重试」路径，语义刻意不同：
//
//  1. emit KindRuntimeLoopFuseTriggered——运行时异常审计事实；
//  2. 任务 processing → blocked（BlockProcessingTaskBySystem），写入原因；
//     blocked 是终态，绝不自动重跑同一 Task；
//  3. 向任务发布者发送崩溃汇报（与 terminateTask 同款邮件通道），避免上游
//     静默等待；
//  4. 非图任务发布「通用 replan 唤醒任务」（reason_code=runtime_loop_fuse，
//     幂等键 <taskID>/replan，见 replan_wake.go）唤醒 Scheduler 裁决后续
//     编排；图任务跳过——终态由 graph-terminal-feed 回填引擎按边条件路由。
//
// 设计上本路径不再调用 LLM——fuse 的存在就是为了阻断失控行为，失控路径上
// 再发起 LLM 调用违背其目的。历史经 saveHistory 落盘仅作事后排查证据，
// 不为重试服务（blocked 不重试）。
func (a *Agent) tripRuntimeLoopFuse(ctx context.Context, task *model.Task, taskID string, loop int, history []HistoryEntry) {
	reason := fmt.Sprintf("emergency loop fuse 触发：ReAct 循环计数 %d 越过兜底阈值 %d，判定为程序缺陷造成的死循环；任务已转 blocked，不会自动重跑",
		loop, a.loopFuseLimit())
	log.Printf("[agent %s] 任务 %s %s", a.ID, taskID, reason)

	trace.Emit(trace.Event{
		Kind:    trace.KindRuntimeLoopFuseTriggered,
		TaskID:  taskID,
		AgentID: a.ID,
		Loop:    loop,
		Reason:  reason,
	})

	// 历史落盘仅供事后排查；blocked 任务不会被重试恢复。
	a.saveHistory(task, history)

	if blocker, ok := a.Store.(processingTaskBlocker); ok {
		if err := blocker.BlockProcessingTaskBySystem(taskID, reason, "runtime_loop_fuse"); err != nil {
			log.Printf("[agent %s] 任务 %s BlockProcessingTaskBySystem error: %v", a.ID, taskID, err)
		}
	} else {
		// Store 不支持 processing→blocked 时降级为 failed——终态语义必须达成，
		// 不能让任务滞留 processing。
		log.Printf("[agent %s] 任务 %s Store 不支持 BlockProcessingTaskBySystem，降级 FailTask", a.ID, taskID)
		a.terminateTask(task, taskID, reason, "runtime_loop_fuse")
	}
	a.sendCrashReport(task, taskID, reason)

	// 自动 replan 唤醒（V6 C6b 起取代原 Plan 控制面的 RequestReplan 登记通道）：
	// 非图任务发布通用 replan 唤醒任务交 Scheduler 裁决后续编排；图任务由
	// publishReplanWakeTask 内部跳过（终态经 graph-terminal-feed 回填路由）。
	// 发布失败不掩盖 fuse 事实本身。
	detail := "emergency loop fuse 触发，任务已转 blocked（终态，不重跑）。\n" +
		"这通常意味着程序缺陷或模型行为死循环，请评估是否调整该任务的执行方式（拆分 / 更换方式 / 终止）。\n原因: " + reason
	a.publishReplanWakeTask(task, taskID, "runtime_loop_fuse", detail)
}

// processingTaskBlocker 是 store 层的窄接口（MemoryTaskStore 实现）：
// processing → blocked 的系统侧迁移。定义为接口保持 agent → store 只依赖
// TaskStore 主接口的装配形状，测试可用 fake 断言迁移语义。
type processingTaskBlocker interface {
	BlockProcessingTaskBySystem(taskID string, reason string, cause string) error
}

// commitStructuredBlocked 是 submit_task_result status=blocked 的收尾事务
// （V6 §5 升级思路 2+3）：agent 自报无法完成时，任务以 blocked 终态收尾而
// 不是 completed——「报 blocked 却放行下游」路径由此关闭（认领闸只认
// completed，blocked 永远不满足依赖）。
//
// 收尾事务顺序（同一收尾路径内，终态先 durable）：
//  1. 结果摘要保留在 task.Results[a.ID]（与 SubmitResult 同键位），阻塞原因
//     由 store 写入 task.Error；event/verdict 键刻意不写——graph 的
//     eventNameOf 让 Result["event"] 优先于终态映射，写入会把 blocked 节点
//     错误路由成事件命中；
//  2. processing → blocked（BlockProcessingTaskBySystem，cause=
//     agent_reported_blocked 与系统兜底拦截区分）；Store 不支持该迁移时
//     降级 failed——终态语义必须达成，不能让任务滞留 processing；
//  3. 终态落盘后 emit task_result_committed（结构化 status/cause）；
//  4. 非图任务发布通用 replan 唤醒任务（幂等键 <taskID>/replan）交
//     Scheduler 裁决；图任务由 graph-terminal-feed 回填驱动边路由。
//     唤醒发布失败时终态事实保留，额外 emit error 事件。
func (a *Agent) commitStructuredBlocked(task *model.Task, taskMem *taskMemRuntime, taskID, resultText, blockedReason, summary string) {
	// blocked_reason 是 submit_task_result 经 schema 校验后的权威终态事实。
	// 必须先写 Task Memory，再让 Store 发出 blocked 终态事件；异步
	// Session promotion 随后等待 finalize 的 sealed checkpoint。
	taskMem.recordBlockedReason(a, taskID, blockedReason)

	// 结果摘要保留（best-effort，与 SubmitResult 同键位）：终态后公告板
	// 快照与 Scheduler 仍能看到 agent 的自述与证据。
	if err := store.RecordResultField(a.Store, taskID, a.ID, resultText); err != nil {
		log.Printf("[agent %s] 任务 %s blocked 收尾写入结果摘要失败: %v", a.ID, taskID, err)
	}

	blocker, ok := a.Store.(processingTaskBlocker)
	if !ok {
		// Store 不支持 processing→blocked 时降级为 failed——终态语义必须达成，
		// 不能让任务滞留 processing（与 runtime loop fuse 同款兜底）。
		log.Printf("[agent %s] 任务 %s Store 不支持 BlockProcessingTaskBySystem，结构化 blocked 降级 FailTask", a.ID, taskID)
		a.terminateTask(task, taskID, "agent 自报 blocked（store 不支持 blocked 迁移，降级 failed）: "+blockedReason, "agent_reported_blocked")
		return
	}
	if err := blocker.BlockProcessingTaskBySystem(taskID, blockedReason, "agent_reported_blocked"); err != nil {
		// 迁移失败（如并发取消已先落终态）：终态事实归竞态获胜方，不补发
		// 唤醒任务，只留错误账本。
		log.Printf("[agent %s] 任务 %s 结构化 blocked 终态迁移失败: %v", a.ID, taskID, err)
		trace.Emit(trace.Event{
			Kind:    trace.KindError,
			TaskID:  taskID,
			AgentID: a.ID,
			Error:   "结构化 blocked 终态迁移失败: " + err.Error(),
		})
		return
	}

	trace.Emit(trace.Event{
		Kind:    trace.KindTaskResultCommitted,
		TaskID:  taskID,
		AgentID: a.ID,
		Reason:  blockedReason,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusProcessing),
			NewStatus:  string(model.TaskStatusBlocked),
			Cause:      "agent_reported_blocked",
		},
	})

	// replan 唤醒与终态同一收尾事务：终态已 durable，唤醒失败不推翻终态，
	// 记 error 事件（Scheduler 也可经公告板巡检发现 blocked 任务）。
	detail := "blocked_reason: " + blockedReason + "\nsummary: " + summary
	if err := a.publishReplanWakeTask(task, taskID, "agent_reported_blocked", detail); err != nil {
		trace.Emit(trace.Event{
			Kind:    trace.KindError,
			TaskID:  taskID,
			AgentID: a.ID,
			Error:   "blocked 终态已落盘，但 replan 唤醒任务发布失败: " + err.Error(),
		})
	}
}

func (a *Agent) handleFailure(task *model.Task, taskID string, execErr error, history []HistoryEntry, manifestInfo *manifestSideInfo) {
	var recoverable *ErrRecoverable
	if errors.As(execErr, &recoverable) {
		// Layer 3: 如果是上下文溢出错误，在重试前激进压缩历史
		overflow := isContextOverflow(execErr)
		if overflow {
			log.Printf("[agent %s] 任务 %s 检测到上下文溢出，执行激进压缩", a.ID, taskID)
			snipOldToolResults(history, 1)        // 激进清理：只保留最近 1 条
			history = compressHistory(history, 1) // 激进压缩：只保留最近 1 条
			// CM1：回填 L3 处置——本 attempt 随后的 LLM 调用的 Manifest 中
			// history 段 Disposition 记为 truncated。
			if manifestInfo != nil {
				manifestInfo.l3Truncated = true
			}
		}

		// 预判是否即将 terminate。
		willTerminate := a.MaxRetries > 0 && task.RetryCount >= a.MaxRetries

		// 全局重试上限：可恢复错误也要受 MaxRetries 约束，避免无限重试。
		// 此前可恢复故障不受限（实战中观察到 24+ 次重试，烧 2 小时）。
		if willTerminate {
			failReason := fmt.Sprintf("重试次数耗尽 (%d/%d): %s",
				task.RetryCount, a.MaxRetries, execErr.Error())
			log.Printf("[agent %s] 任务 %s 终止：%s", a.ID, taskID, failReason)
			a.terminateTask(task, taskID, failReason, "recoverable_error_retries_exhausted")
			return
		}

		// 可恢复错误：保存历史上下文后重试
		a.saveHistory(task, history)
		if err := a.Store.RetryRollback(a.ID, taskID, execErr.Error()); err != nil {
			// "task is not in processing state" 通常意味着 watchdog 已经接管，
			// 不算 agent 自身的故障，降级为 warning。
			if errors.Is(err, store.ErrTaskNotProcessing) {
				log.Printf("[agent %s] 任务 %s RetryRollback 跳过：状态已被外部转换 (可能 watchdog 接管)", a.ID, taskID)
			} else {
				log.Printf("[agent %s] RetryRollback error: %v", a.ID, err)
			}
		} else {
			// 2026-04-25 P1 #2：重试 trace 事件（可恢复错误路径）。
			trace.Emit(trace.Event{
				Kind:      trace.KindTaskRetry,
				TaskID:    taskID,
				AgentID:   a.ID,
				Reason:    "recoverable_error: " + execErr.Error(),
				AttemptNo: task.RetryCount,
				Transition: &trace.Transition{
					PrevStatus: string(model.TaskStatusProcessing),
					NewStatus:  string(model.TaskStatusPending),
					Cause:      "recoverable_error",
					RetryCount: task.RetryCount,
				},
			})
		}
	} else {
		// 不可恢复错误：诊断原因后终止 + 崩溃汇报（V6 CM4 起不再生成
		// TransferNote——重试/下游交接由 Task Memory + LastHistory 承担）。
		reason := diagnoseLLMError(execErr, history, a.Model)
		log.Printf("[agent %s] 任务 %s 不可恢复错误：%s", a.ID, taskID, reason)
		a.terminateTask(task, taskID, reason, "non_recoverable_error")
	}
}

// terminateTask 是任务最终失败的统一收口：
//  1. 通过 FailTask 把任务状态原子转换到 failed
//  2. 向任务的 EventSource（发布者，通常是 scheduler 或父代理）发送一条结构化崩溃邮件，
//     避免上游静默等待。崩溃邮件遵循固定格式："代理 X 在执行任务 Y 时崩溃，原因 Z"。
//
// cause 是 trace.Transition.Cause 的结构化原因 enum（v5 Phase 2 引入），让 Reactor when
// 条件能精确匹配 runtime_loop_fuse / non_recoverable_error 等分支。
func (a *Agent) terminateTask(task *model.Task, taskID string, reason string, cause string) {
	if err := a.Store.FailTask(a.ID, taskID, reason); err != nil {
		log.Printf("[agent %s] FailTask error: %v", a.ID, err)
	}
	// 2026-04-25 P1 #2：失败终态 trace 事件。在此前 trace 只记 task_submitted /
	// task_completed 两种成功终态，非成功路径对 trace reader 完全不可见。
	retryCount := 0
	if task != nil {
		retryCount = task.RetryCount
	}
	trace.Emit(trace.Event{
		Kind:    trace.KindTaskFailed,
		TaskID:  taskID,
		AgentID: a.ID,
		Reason:  reason,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusProcessing),
			NewStatus:  string(model.TaskStatusFailed),
			Cause:      cause,
			RetryCount: retryCount,
		},
	})
	a.sendCrashReport(task, taskID, reason)
}

// sendCrashReport 向 task.ReplyToAgentID 发送结构化崩溃通知。
// 旧任务仅在 EventSource 确实对应已注册邮箱时兼容回退；父 Task UUID
// 绝不会再被误当成收件人。没有可路由收件人时静默跳过。
//
// 邮件正文不仅包含失败原因，还会附上：
//   - 任务实际写入的文件清单（task.Artifacts）—— 让 scheduler 立刻知道
//     "worker 不是没干活，是写到了别处"，可以决定是否接收漂移产物
//   - worker 最后一次 LLM 响应的原文（task.LastResponse）—— 让 scheduler
//     看到 worker 自述了什么，理解失败语境
//
// 重新读取一次 task 是因为 reason 路径里 task 指针可能已陈旧，
// 没拿到 RecordLastResponse / AppendArtifact 的最新写入。
func (a *Agent) sendCrashReport(task *model.Task, taskID string, reason string) {
	if a.MailRegistry == nil || task == nil {
		return
	}
	// 重读 task 以拿到最新的 Artifacts / LastResponse
	if fresh, err := a.Store.GetTask(taskID); err == nil && fresh != nil {
		task = fresh
	}
	recipient := a.MailRegistry.ResolveReplyRecipient(
		task.ReplyToAgentID, task.EventSource, task.ID, task.ParentTaskID)
	if recipient == "" {
		return
	}
	desc := task.Description
	if len([]rune(desc)) > 100 {
		desc = string([]rune(desc)[:100]) + "..."
	}
	summary := fmt.Sprintf("代理 %s 在执行任务 %s 时崩溃", a.ID, taskID[:8])

	var sb strings.Builder
	fmt.Fprintf(&sb, "代理 %s 在执行任务 %s 时崩溃。\n", a.ID, taskID)
	fmt.Fprintf(&sb, "任务描述: %s\n", desc)
	fmt.Fprintf(&sb, "重试次数: %d\n", task.RetryCount)
	fmt.Fprintf(&sb, "失败原因: %s\n", reason)

	if len(task.ExpectedArtifacts) > 0 {
		fmt.Fprintf(&sb, "\n预期产出 (expected_artifacts): %v\n", task.ExpectedArtifacts)
	}
	if len(task.Artifacts) > 0 {
		sb.WriteString("\n实际写入的文件 (按字面路径列出):\n")
		for _, p := range task.Artifacts {
			fmt.Fprintf(&sb, "  - %s\n", p)
		}
		sb.WriteString("（如果上述文件已经满足任务意图但路径名不同，可考虑直接接收，或重新发布修正路径的任务。）\n")
	} else {
		sb.WriteString("\n实际写入的文件: 无（worker 完全没有产出文件）\n")
	}
	if task.LastResponse != "" {
		// 截断防止超长
		resp := task.LastResponse
		if len([]rune(resp)) > 500 {
			resp = string([]rune(resp)[:500]) + "...[已截断]"
		}
		fmt.Fprintf(&sb, "\nworker 最后一次响应原文:\n%s\n", resp)
	}
	body := sb.String()

	msg := mailbox.Message{
		From:     a.ID,
		To:       recipient,
		Type:     mailbox.MsgTypeInfo,
		Priority: mailbox.PriorityHigh,
		Summary:  summary,
		Content:  body,
		SentAt:   time.Now(),
	}
	if err := a.MailRegistry.Send(msg); err != nil {
		log.Printf("[agent %s] 发送崩溃汇报失败: %v", a.ID, err)
	} else {
		log.Printf("[agent %s] 已向 %s 汇报任务 %s 崩溃", a.ID, recipient, taskID[:8])
	}
}

// diagnoseLLMError 将不可恢复 LLM 错误映射为面向用户/scheduler 的诊断提示。
// 基于 v4.md §9.4 的诊断映射规则，从 llm.ErrUnrecoverable 中提取 Code / StatusCode /
// Message / Endpoint 生成可操作的错误描述。非 llm 错误原样返回。
func diagnoseLLMError(execErr error, history []HistoryEntry, model string) string {
	var unrecov *llm.ErrUnrecoverable
	if !errors.As(execErr, &unrecov) {
		return execErr.Error()
	}

	// 轻量估算当前历史 token 长度（用于 context_length_exceeded 提示）
	estTokens := 0
	for _, h := range history {
		estTokens += len(h.AssistantContent) / 3
		estTokens += len(h.Output) / 3
		for _, tr := range h.ToolResults {
			estTokens += len(tr.Content) / 3
		}
	}

	msgLower := strings.ToLower(unrecov.Message)
	switch {
	// Go 优先级 && > ||，下面两个分支等价；显式括号让"或"的两侧在视觉上对齐，
	// 防止维护者误读为 (Code=="model_not_found" || strings.Contains(...,"model")) && strings.Contains(...,"not found")。
	case unrecov.Code == "model_not_found" ||
		(strings.Contains(msgLower, "model") && strings.Contains(msgLower, "not found")):
		return fmt.Sprintf("模型名 '%s' 不存在。请检查 setting.yaml 中的 model 配置。当前使用的 endpoint: %s", model, unrecov.Endpoint)
	case unrecov.Code == "invalid_api_key" || unrecov.StatusCode == 401:
		return "API key 无效或已过期。请检查 setting.yaml 中的 api_key 或环境变量。"
	case unrecov.Code == "insufficient_quota":
		return "API 配额不足。请检查账户余额或联系 provider。"
	case unrecov.StatusCode == 404 && strings.Contains(unrecov.Endpoint, "/chat/completions"):
		return fmt.Sprintf("端点返回 404。请检查 setting.yaml 中的 base_url 是否包含正确的 API 路径（如 %s）。", unrecov.Endpoint)
	case unrecov.StatusCode == 404:
		return fmt.Sprintf("无法连接到 %s。请检查网络连通性或 base_url 配置。", unrecov.Endpoint)
	case unrecov.Code == "context_length_exceeded":
		return fmt.Sprintf("请求超出模型上下文上限。当前历史长度约 %d tokens，请考虑调低 enforce_compact_token_threshold 让压缩更早触发，或拆分任务缩短上下文。", estTokens)
	default:
		return fmt.Sprintf("LLM 调用失败: %s (status=%d, code=%s)。完整响应: %s", unrecov.Message, unrecov.StatusCode, unrecov.Code, unrecov.Err.Error())
	}
}

// buildArtifactFailureReason 把校验结果格式化为返给 ErrRecoverable 的失败原因。
func buildArtifactFailureReason(check ArtifactCheckResult) string {
	var sb strings.Builder
	sb.WriteString("任务声称完成但 expected_artifacts 校验失败。\n")
	if len(check.Missing) > 0 {
		fmt.Fprintf(&sb, "缺失的预期文件: %v\n", check.Missing)
	}
	if len(check.Actual) > 0 {
		fmt.Fprintf(&sb, "你实际写入的文件: %v\n", check.Actual)
	} else {
		sb.WriteString("你实际没有写入任何文件。\n")
	}
	sb.WriteString("请按 expected_artifacts 字面给出的相对路径写入文件——不要自作主张加 docs/ 前缀，也不要改名。")
	return sb.String()
}

// appendValidationFeedback 把校验失败的诊断信息追加为一条 IncomingMail 历史条目。
// 重试时这条会作为 user 角色消息进入下一轮 LLM 上下文，让 LLM 看见自己上次为什么被打回。
func appendValidationFeedback(history []HistoryEntry, check ArtifactCheckResult) []HistoryEntry {
	var sb strings.Builder
	sb.WriteString("<validation-feedback>\n")
	sb.WriteString("  上一次 LLM 响应被系统拦截：你声称任务完成，但 expected_artifacts 校验未通过。\n")
	if len(check.Missing) > 0 {
		fmt.Fprintf(&sb, "  缺失的预期文件: %v\n", check.Missing)
	}
	if len(check.Drifted) > 0 {
		fmt.Fprintf(&sb, "  路径漂移（basename 匹配但路径不一致）: %v\n", check.Drifted)
	}
	if len(check.Actual) > 0 {
		fmt.Fprintf(&sb, "  你实际写入的文件: %v\n", check.Actual)
	} else {
		sb.WriteString("  你实际没有写入任何文件。\n")
	}
	sb.WriteString("  纠正策略：使用 write_file 工具，path 参数严格按 expected_artifacts 字面给出的相对路径。\n")
	sb.WriteString("  不要把文件写到 docs/ 子目录除非 expected 路径就是 docs/xxx。\n")
	sb.WriteString("</validation-feedback>")
	return append(history, HistoryEntry{IncomingMail: sb.String()})
}

// saveHistory 将当前历史序列化并保存到任务中，供重试时恢复。
func (a *Agent) saveHistory(task *model.Task, history []HistoryEntry) {
	if len(history) == 0 || task == nil || a.Store == nil {
		return
	}
	data, err := json.Marshal(history)
	if err != nil {
		log.Printf("[agent %s] 序列化历史记录失败: %v", a.ID, err)
		return
	}
	if err := a.Store.RecordLastHistory(task.ID, data); err != nil {
		log.Printf("[agent %s] 持久化历史记录失败 task=%s: %v", a.ID, task.ID, err)
		return
	}
	// Keep the detached execution snapshot coherent for code that still uses it
	// later in the same termination path; the store remains the authority.
	task.LastHistory = append([]byte(nil), data...)
}

func (a *Agent) shouldRetire(idleCount int) bool {
	return a.IdleThreshold > 0 && idleCount >= a.IdleThreshold
}

func (a *Agent) sleep(ctx context.Context) {
	interval := a.PollInterval
	if interval == 0 {
		interval = 500 * time.Millisecond
	}
	select {
	case <-ctx.Done():
	case <-time.After(interval):
	}
}

// NewAgent creates a new agent with the given configuration.
func NewAgent(id, eventType string, s store.TaskStore, r roster.Roster, exec TaskExecutor) *Agent {
	return &Agent{
		ID:           id,
		EventType:    eventType,
		Store:        s,
		Roster:       r,
		Execute:      exec,
		PollInterval: 500 * time.Millisecond,
	}
}

// String returns a description of the agent for logging.
func (a *Agent) String() string {
	return fmt.Sprintf("Agent[%s, type=%s]", a.ID, a.EventType)
}

// --- 历史压缩（3 层） ---

// snipTargetTools 是 Layer 1 清理目标工具名称集合。
var snipTargetTools = map[string]bool{
	"run_shell":       true,
	"read_file":       true,
	"grep_search":     true,
	"glob_search":     true,
	"get_task_result": true,
}

// snipStub 生成结构化墓碑（2026-07-22 分层记忆 v2 M1 层）。
// 旧占位符 "[已清空，内容过长]" 只告知"被清了"，模型不知道清的是哪个文件、
// 原内容多大、怎么取回，重读决策是盲目的（explorer 重读浪费事故，实测同
// 文件最多被读 12 次）。墓碑携带工具名 + 目标（path/command/pattern）+
// 原内容长度 + 取回指引，让模型的重读决策从盲目变成知情；并优先引导它
// 回顾自己在 assistant 消息里写的笔记（笔记不被 Layer-1 清理）。
func snipStub(toolName string, args map[string]any, originalLen int) string {
	target := ""
	for _, key := range []string{"path", "command", "pattern", "task_id"} {
		if v, ok := args[key].(string); ok && v != "" {
			target = v
			break
		}
	}
	if runes := []rune(target); len(runes) > 60 {
		target = string(runes[:57]) + "..."
	}
	desc := toolName
	if target != "" {
		desc += " " + target
	}
	return fmt.Sprintf("[已清空] %s（原 %d 字符）：内容已被历史压缩清理；请先回顾你在前文写的笔记，确需内容可重新调用 %s（read_file 命中缓存时仅返回摘要，需全文传 force_full=true）",
		desc, originalLen, toolName)
}

// snipOldToolResults 清理历史中旧的高输出工具结果（Layer 1）。
// 对每种目标工具，保留最近 keepRecent 条结果不变，更早的结果用结构化墓碑
// 替换 Content（见 snipStub）。直接修改 history 切片中的 ToolResults。
func snipOldToolResults(history []HistoryEntry, keepRecent int) {
	// 从后往前遍历，保留最近 keepRecent 条，清理更早的
	seen := make(map[string]int)
	for i := len(history) - 1; i >= 0; i-- {
		entry := &history[i]
		for j := 0; j < len(entry.ToolCalls) && j < len(entry.ToolResults); j++ {
			name := entry.ToolCalls[j].Name
			if !snipTargetTools[name] {
				continue
			}
			seen[name]++
			if seen[name] > keepRecent {
				entry.ToolResults[j].Content = snipStub(name, entry.ToolCalls[j].Arguments, len(entry.ToolResults[j].Content))
			}
		}
	}
}

// buildHistorySummary 从历史条目中构建文本摘要（不调用 LLM）。
func buildHistorySummary(history []HistoryEntry) string {
	var sb strings.Builder
	sb.WriteString("=== 历史摘要 ===\n")
	for i, entry := range history {
		fmt.Fprintf(&sb, "步骤 %d: ", i+1)
		if entry.ToolCalled && len(entry.ToolCalls) > 0 {
			for _, tc := range entry.ToolCalls {
				fmt.Fprintf(&sb, "[%s] ", tc.Name)
			}
		}
		// 包含 assistant 内容（LLM 推理），截断到 200 字符
		if entry.AssistantContent != "" {
			content := entry.AssistantContent
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			sb.WriteString(content)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// compressHistory 将旧历史条目压缩为一条摘要，保留最近 keepRecent 条（Layer 2）。
// 如果历史条目数不超过 keepRecent，不做任何压缩。
func compressHistory(history []HistoryEntry, keepRecent int) []HistoryEntry {
	if len(history) <= keepRecent {
		return history
	}
	oldEntries := history[:len(history)-keepRecent]
	recentEntries := history[len(history)-keepRecent:]

	summaryText := buildHistorySummary(oldEntries)
	summaryEntry := HistoryEntry{
		Output:     summaryText,
		ToolCalled: false,
	}

	result := make([]HistoryEntry, 0, 1+keepRecent)
	result = append(result, summaryEntry)
	result = append(result, recentEntries...)
	return result
}

// isContextOverflow 检查错误是否表示上下文溢出（Layer 3）。
func isContextOverflow(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "length") || strings.Contains(msg, "截断") || strings.Contains(msg, "context")
}

// ArtifactCheckResult 描述 ExpectedArtifacts 校验的结果。
type ArtifactCheckResult struct {
	Missing []string // 完全找不到的预期路径（精确匹配 + basename 兜底都失败）
	Drifted []string // basename 兜底命中但路径不一致的预期项（"expected: X, actual: docs/X" 形式）
	Actual  []string // 任务实际写入的全部 artifacts，便于注入到反馈消息
	// Recovered 是账本缺失但经磁盘兜底命中的预期项：重试/替代任务换新
	// 任务 ID 后账本失忆，前次尝试写好的文件还在盘上——stat 命中即视为
	// 满足契约（文件系统是唯一真实来源），不再强制 LLM 重写一遍。
	Recovered []string
}

// checkExpectedArtifacts 校验任务的 ExpectedArtifacts 是否全部出现在 Artifacts 中。
//
// 这是 Level 3 的硬性合约校验：如果发布者明确声明"任务必须产出文件 X.md"，
// 但任务结束时 X.md 没有被任何 write_file/edit_file 调用记录到 Artifacts 中，
// 则认定任务"假完成"，触发失败重试。
//
// 匹配策略（按顺序尝试，命中即停）：
//  1. 精确匹配：expected == artifact 字符串完全相等
//  2. basename 兜底：filepath.Base(artifact) == filepath.Base(expected)
//     命中后视为契约满足，但记录 drift（路径漂移）以便提示 LLM 修正
//
// basename 兜底的动机：LLM 经常把 expected="foo.md" 写到 "docs/foo.md"。这种"差点对了"
// 的情况硬卡校验只会陷入死循环，而正确的重试反馈又让 LLM 困惑。允许 basename 命中，
// 同时把漂移信息注入下次重试历史，比强制精确匹配更鲁棒。
func checkExpectedArtifacts(store storeReader, taskID string) ArtifactCheckResult {
	var res ArtifactCheckResult
	task, err := store.GetTask(taskID)
	if err != nil || task == nil {
		return res // 拿不到任务无法校验，视作通过
	}
	if len(task.ExpectedArtifacts) == 0 {
		return res // 无声明，无校验
	}
	res.Actual = append(res.Actual, task.Artifacts...)

	// 建立精确匹配集合 + basename → 完整路径 索引
	exact := make(map[string]bool)
	byBase := make(map[string]string)
	for _, p := range task.Artifacts {
		exact[p] = true
		byBase[filepath.Base(p)] = p
	}

	for _, expected := range task.ExpectedArtifacts {
		if exact[expected] {
			continue
		}
		expectedBase := filepath.Base(expected)
		if actual, ok := byBase[expectedBase]; ok {
			// basename 命中，记 drift
			res.Drifted = append(res.Drifted, fmt.Sprintf("expected=%s, actual=%s", expected, actual))
			continue
		}
		res.Missing = append(res.Missing, expected)
	}
	return res
}

// storeReader 是 checkExpectedArtifacts 需要的最小 Store 接口子集，方便测试。
type storeReader interface {
	GetTask(taskID string) (*model.Task, error)
}

// checkExpectedArtifactsWithResolver 在账本比对之上加磁盘兜底：账本缺失的
// 预期项经 resolve 解析到物理位置 stat 一次，存在（且非目录）即转入
// Recovered 视为满足——覆盖「重试换新任务 ID 后账本失忆、文件其实已在盘上」
// 的空转场景；resolve 为 nil 时与纯账本比对完全一致。
func checkExpectedArtifactsWithResolver(s storeReader, taskID string, resolve ArtifactPhysicalResolver) ArtifactCheckResult {
	res := checkExpectedArtifacts(s, taskID)
	if len(res.Missing) == 0 || resolve == nil {
		return res
	}
	var stillMissing []string
	for _, expected := range res.Missing {
		fi, err := os.Stat(resolve(taskID, expected))
		if err == nil && !fi.IsDir() {
			res.Recovered = append(res.Recovered, expected)
			continue
		}
		stillMissing = append(stillMissing, expected)
	}
	res.Missing = stillMissing
	return res
}

// CheckExpectedArtifacts 是 checkExpectedArtifacts 的导出包装，
// 供 tools 包（submit_task_result）在工具层复用同一套 ExpectedArtifacts 合约校验，
// 保证工具提交与自然完成路径的校验语义完全一致。
func CheckExpectedArtifacts(s storeReader, taskID string) ArtifactCheckResult {
	return checkExpectedArtifacts(s, taskID)
}

// CheckExpectedArtifactsWithDisk 是 checkExpectedArtifactsWithResolver 的导出
// 包装（submit_task_result 工具层磁盘兜底入口），语义同上。
func CheckExpectedArtifactsWithDisk(s storeReader, taskID string, resolve ArtifactPhysicalResolver) ArtifactCheckResult {
	return checkExpectedArtifactsWithResolver(s, taskID, resolve)
}

// BuildArtifactFailureReason 是 buildArtifactFailureReason 的导出包装，理由同上。
func BuildArtifactFailureReason(check ArtifactCheckResult) string {
	return buildArtifactFailureReason(check)
}

// mergeArtifactsIntoDeps 把每个依赖任务的 Artifacts 文件路径列表追加到对应的 SubmitResult 文本后面。
// 合并后的字符串作为 depResults map 的值，由 buildMessages 注入到 user prompt 的"前置任务结果"段。
//
// 输出格式（每个依赖任务，仅当 Artifacts 非空时追加）：
//
//	<原 SubmitResult 文本>
//
//	【该任务实际写入的文件】
//	  - docs/output/foo.md
//	  - docs/output/bar.md
//	（你必须 read_file 这些文件来获取一手数据，不要凭空总结）
//
// 如果某依赖任务的 Artifacts 为空（任务未写文件或 report-only 模式），保持原 depResults 不变，
// 不追加任何内容——无信息可注入。Worker 仍能从原文本看到上游的 SubmitResult。
func mergeArtifactsIntoDeps(depResults map[string]string, depArtifacts map[string][]string) map[string]string {
	if depResults == nil {
		depResults = make(map[string]string)
	}
	for depID, artifacts := range depArtifacts {
		if len(artifacts) == 0 {
			continue // 上游未产出文件，无信息可注入
		}
		base := depResults[depID]
		var sb strings.Builder
		sb.WriteString(base)
		if base != "" {
			sb.WriteString("\n\n")
		}
		sb.WriteString("【该任务实际写入的文件】\n")
		for _, p := range artifacts {
			fmt.Fprintf(&sb, "  - %s\n", p)
		}
		sb.WriteString("（你必须 read_file 这些文件来获取一手数据，不要仅凭上面的总结文本就凭空生成下游产出）")
		depResults[depID] = sb.String()
	}
	return depResults
}

// formatMailMessages 将邮箱消息格式化为带类型/优先级子标签的 XML，注入 LLM 上下文。
// 接收方 LLM 可先看 summary 决定是否需要读 body。
func formatMailMessages(msgs []mailbox.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		msgType := m.Type
		if msgType == "" {
			msgType = mailbox.MsgTypeInfo
		}
		priority := m.Priority
		if priority == "" {
			priority = mailbox.PriorityNormal
		}
		fmt.Fprintf(&sb, "<agent-mail type=%q priority=%q>\n", msgType, priority)
		fmt.Fprintf(&sb, "  <from>%s @ %s</from>\n", m.From, m.SentAt.Format("15:04:05"))
		if m.Summary != "" {
			fmt.Fprintf(&sb, "  <summary>%s</summary>\n", m.Summary)
		}
		fmt.Fprintf(&sb, "  <body>%s</body>\n", m.Content)
		sb.WriteString("</agent-mail>\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
