package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/hook"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/roster"
	"agentgo/internal/store"
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
// 用于 PredictNextPromptTokens 的"实测锚定 + 新增估算"策略：
//   - PromptTokens：产生该条 assistant 回复时的实测 prompt token 数（来自 SDK Usage）
//   - CompletionTokens：同上，本轮 completion 实测值
//   - Model：产生该回复时使用的模型名（不同模型 tokenizer 不同，跨模型实测值不可比）
type HistoryEntry struct {
	Output           string                     `json:"output"`
	ToolCalled       bool                       `json:"tool_called"`
	AssistantContent string                     `json:"assistant_content"`
	ToolCalls        []llm.ToolCall             `json:"tool_calls"`
	ToolResults      []ToolResult               `json:"tool_results"`
	ExtraFields      map[string]json.RawMessage `json:"extra_fields,omitempty"`     // 层 1 通用透传：assistant 消息的非标字段
	IncomingMail     string                     `json:"incoming_mail,omitempty"`    // 非空时为收到的代理间邮件，注入为 user 角色消息
	PromptTokens     int                        `json:"prompt_tokens,omitempty"`    // §11.7.3 实测锚定：本轮 LLM 调用的实测 prompt tokens
	CompletionTokens int                        `json:"completion_tokens,omitempty"` // §11.7.3 实测锚定：本轮 completion tokens
	Model            string                     `json:"model,omitempty"`            // §11.7.3 模型切换基准重置：产生该条回复时使用的模型名
}

// TaskExecutor is a pluggable function that executes a task.
// For MVP this is injected as a mock; in production it will call the LLM.
type TaskExecutor func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error)

type Agent struct {
	ID                    string
	EventType             string
	Store                 store.TaskStore
	Roster                roster.Roster
	Execute               TaskExecutor
	MaxLoops              int
	MaxRetries            int // 最大重试次数，0 表示不限制
	PollInterval          time.Duration
	IdleThreshold         int // 连续空轮询退出阈值，0 表示禁用
	CancelRegistry        *store.TaskCancelRegistry
	CompactTokenThreshold int                               // Layer 2 触发阈值（prompt tokens），默认 80000
	CompactKeepRecent     int                               // 压缩时保留最近 N 条历史，默认 3
	// Model 是该 Agent 当前生效的模型名，用于 HistoryEntry.Model 记录。
	// nextUpgrade_v4.md §11.7.3：跨模型实测值不可比，PredictNextPromptTokens
	// 仅锚定当前模型一致的最近一条 PromptTokens > 0 条目。空串时退化为粗略估算。
	Model string
	// ContextLimit 是历史 token 硬上限（§11.7.4 截断保护）。0 表示不做硬限截断
	// （仅 Layer 1 + Layer 2 压缩生效，与 v3 行为兼容）。详见 nextUpgrade_v4.md §11.7.5。
	ContextLimit int
	OnTaskStart           func(taskID string)               // 任务开始处理时的回调，可选
	OnTaskEnd             func(taskID string, success bool) // 任务结束回调（defer 保证触发），可选
	FileCache             *FileStateCache                   // Agent 级别的文件读取缓存，可选
	Mailbox               *mailbox.Mailbox                  // 代理间通信收件箱，可选
	MailRegistry          *mailbox.Registry                 // 邮箱注册表，用于 DrainWithAck 自动回执
	FinalizationChecker   FinalizationChecker               // 可选；用于 finalization tool 信号检查

	// IsUserFacing 标记此 agent 是否直接对话用户（典型为 scheduler）。
	//
	// true 时：任何"自然文本完成"路径（!result.ToolCalled）都会自动把 lastOutput
	// 打印到 stdout，无需 LLM 显式调用 report_done。
	//
	// false 时（默认，worker / explorer 行为）：自然完成不打印——它们的输出由
	// scheduler 通过 board snapshot / TransferNote 间接消费。
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

	// TransferNoteMaxTokens 是 TransferNote 单条的 token 预算上限。0 或负数视为默认 3000。
	// 失败路径 buildTransferNote 按此值做 L1/L3 输出截断。Sprint 3 #5 引入。
	TransferNoteMaxTokens int

	// ProgressNotifyEnabled 控制进度通知功能是否启用。
	// 为 true 时，Agent 在文件写入、子任务发布或任务过半时通过 mailbox 发送进度消息。
	ProgressNotifyEnabled bool

	// AgentHookReg 是 Agent Hook 注册表，覆盖 processTask 的 4 个生命周期事件
	// （PhaseTaskStart / PhaseLoopPre / PhaseLoopPost / PhaseTaskEnd）。
	// nil 时全路径退化为 no-op——这是回归验证的可逆性保证。
	AgentHookReg *hook.AgentHookRegistry
	// HookStoreView / HookRosterView 是 Agent Hook 的只读视图接口。
	// AgentHookReg 非 nil 时通常同时提供这两个视图，hook 通过 AgentHookContext
	// 访问任务/占用状态。任一为 nil 时，对应视图在 hook 里为 nil，hook 需自行判空。
	HookStoreView  hook.AgentStoreView
	HookRosterView hook.AgentRosterView
}

// transferNoteMaxTokens 返回实际使用的 TransferNote 预算。
// TransferNoteMaxTokens 字段 <= 0 时使用默认值 3000。
func (a *Agent) transferNoteMaxTokens() int {
	if a.TransferNoteMaxTokens > 0 {
		return a.TransferNoteMaxTokens
	}
	return 3000
}

// runAgentInject 构造 AgentHookContext 并调用 Registry 的 RunInject。
// nil Registry 安全——返回空串即可。
// Phase 只应传入 PhaseTaskStart / PhaseLoopPre（注入类阶段）。
func (a *Agent) runAgentInject(
	ctx context.Context,
	phase hook.AgentHookPhase,
	taskID string,
	loopIdx int,
	hasNewMail bool,
) string {
	if a.AgentHookReg == nil {
		return ""
	}
	results := a.AgentHookReg.RunInject(hook.AgentHookContext{
		Ctx:        ctx,
		Phase:      phase,
		AgentID:    a.ID,
		TaskID:     taskID,
		LoopIndex:  loopIdx,
		HasNewMail: hasNewMail,
		Store:      a.HookStoreView,
		Roster:     a.HookRosterView,
	})
	return hook.MergeInjectContents(results)
}

// runAgentObserve 构造 AgentHookContext 并调用 Registry 的 RunObserve。
// nil Registry 安全——直接 no-op。
// Phase 只应传入 PhaseLoopPost / PhaseTaskEnd（观察类阶段）。
func (a *Agent) runAgentObserve(
	ctx context.Context,
	phase hook.AgentHookPhase,
	taskID string,
	loopIdx int,
) {
	if a.AgentHookReg == nil {
		return
	}
	a.AgentHookReg.RunObserve(hook.AgentHookContext{
		Ctx:       ctx,
		Phase:     phase,
		AgentID:   a.ID,
		TaskID:    taskID,
		LoopIndex: loopIdx,
		Store:     a.HookStoreView,
		Roster:    a.HookRosterView,
	})
}

// Run starts the agent's main loop. It polls for available tasks and processes them.
// It blocks until ctx is cancelled or no more work is available after a poll cycle.
func (a *Agent) Run(ctx context.Context) {
	defer func() {
		if a.Roster != nil {
			a.Roster.ReleaseAll(a.ID)
		}
	}()

	idleCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tasks, err := a.Store.QueryAvailable(a.EventType)
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

func (a *Agent) processTask(ctx context.Context, taskID string) {
	task, err := a.Store.GetTask(taskID)
	if err != nil {
		log.Printf("[agent %s] GetTask error: %v", a.ID, err)
		return
	}

	// 进度通知：每任务级别的去重标志，在 processTask 入口初始化
	pFlags := progressFlags{}

	// Panic 恢复（Sprint 3 #5 引入）：processTask 的任意路径 panic 都会
	// 被这里捕获，走纯机械 L3 生成 TransferNote + 终止任务，避免"panic →
	// 任务永久 processing 卡在花名册"的历史潜在 P0。
	//
	// 为什么不走 buildTransferNote(L1→L3)？
	//   - panic 发生时 ctx 可能已取消、LLM client 状态未知
	//   - 直接再调一次 LLM（L1）高概率二次失败
	//   - L3 纯代码拼装够用：工具轨迹 + Artifacts + 最后响应已经构成可读的交接
	//
	// 注意：这条 defer 必须**最先**注册，这样它在 LIFO 的最后一层执行，
	// 让其他 defer（trace.CloseTask / OnTaskEnd / PhaseTaskEnd）仍有机会运行。
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[agent %s] 任务 %s processTask panic 被恢复: %v", a.ID, taskID, rec)
			// 构造 L3 交接备忘
			var toolHistory []store.ToolCallRecord
			if a.Store != nil {
				toolHistory, _ = a.Store.QueryToolCalls(taskID, "")
			}
			note := mechanicalTransferNote(task, nil, toolHistory, a.transferNoteMaxTokens())
			if note != "" {
				_ = a.Store.SetTransferNote(taskID, note)
			}
			// 终止任务，避免卡在 processing 状态
			reason := fmt.Sprintf("agent panic: %v", rec)
			if err := a.Store.FailTask(a.ID, taskID, reason); err != nil {
				log.Printf("[agent %s] panic 恢复后 FailTask error: %v", a.ID, err)
			}
			// 2026-04-26 §11.8 S11：panic-recovery 路径补 KindTaskFailed emit。
			// 此前与 terminateTask（agent.go:811）的非对称——同样调 FailTask 但 panic
			// 路径不 emit——导致 trace 观察者对 panic 引发的任务失败完全失明。
			// 该缺陷由 §11.8 S11 对称扫描测试首次发现并同 commit 修复。
			trace.Emit(trace.Event{
				Kind:    trace.KindTaskFailed,
				TaskID:  taskID,
				AgentID: a.ID,
				Reason:  reason,
			})
			// 尝试发送崩溃汇报（有 EventSource 时）
			if task != nil {
				a.sendCrashReport(task, taskID, reason)
			}
		}
	}()

	// Trace：记录任务被代理认领
	trace.Emit(trace.Event{
		Kind:    trace.KindTaskClaimed,
		TaskID:  taskID,
		AgentID: a.ID,
	})

	// Trace：CloseTask 必须在 OnTaskEnd 之后执行，以便后者发出的
	// 收尾事件（如 file_written）仍然写入同一 trace 文件。
	// Go defer 是 LIFO，所以这条 defer 必须**先注册**才能**最后执行**。
	defer trace.CloseTask(taskID)

	// 任务开始回调（用于 publish_subtask 跟踪当前任务 ID 等扩展点）
	if a.OnTaskStart != nil {
		a.OnTaskStart(taskID)
	}

	// 任务结束回调（defer 确保所有退出路径都触发；目前仅 holder 清理使用）
	taskSuccess := false
	if a.OnTaskEnd != nil {
		defer func() {
			a.OnTaskEnd(taskID, taskSuccess)
		}()
	}

	// PhaseTaskEnd：每任务一次的观察类 hook 触发点。
	// defer 注册顺序与 LIFO：本行在 OnTaskEnd defer 之后注册，
	// 所以 PhaseTaskEnd 先于 OnTaskEnd 执行——hook 看到的是 task 状态
	// 还未被 holder 清理时的"刚完成"状态。
	defer a.runAgentObserve(ctx, hook.PhaseTaskEnd, taskID, -1)

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

	// 拉取依赖任务的 TransferNote（上游代理在终止前留下的压缩交接备忘），
	// 以 <upstream-transfer-notes> 形式作为首条 user 消息注入 history。
	// Sprint 3 #5：让下游代理既能看到上游的 Artifacts 文件清单，又能看到
	// 上游对工作的自述总结——两层信息互补。
	depNotes, noteErr := a.Store.GetDependencyTransferNotes(taskID)
	if noteErr != nil {
		log.Printf("[agent %s] GetDependencyTransferNotes error: %v", a.ID, noteErr)
	}

	var lastOutput string
	history := make([]HistoryEntry, 0)

	// 重试时恢复之前的历史上下文，避免 LLM 丢失上下文重复操作。
	// 本最小版 TransferNote 与 LastHistory 并存：LastHistory 作为完整的历史副本
	// 提供原始 tool_call/tool_result 序列；TransferNote 作为精炼文本提供接手者
	// "为什么要做这件事 + 前任遇到什么障碍"的决策上下文。两者在 Execute 调用时
	// 都会出现在 LLM 的上下文里。未来 TransferNote 实测稳定后可考虑删除 LastHistory。
	if task.RetryCount > 0 && len(task.LastHistory) > 0 {
		if err := json.Unmarshal(task.LastHistory, &history); err != nil {
			log.Printf("[agent %s] 反序列化历史记录失败，从空历史开始: %v", a.ID, err)
			history = make([]HistoryEntry, 0)
		} else {
			log.Printf("[agent %s] 任务 %s 重试 #%d，恢复 %d 条历史记录", a.ID, taskID, task.RetryCount, len(history))
		}
	}

	// 依赖 TransferNote 注入：依赖链场景。每条上游备忘作为单独条目，
	// 用 XML 标签分隔便于 LLM 识别"这是其他任务的交接，不是我自己的历史"。
	if len(depNotes) > 0 {
		var sb strings.Builder
		sb.WriteString("<upstream-transfer-notes>\n")
		for depID, note := range depNotes {
			fmt.Fprintf(&sb, "<note from=\"%s\">\n%s\n</note>\n", depID, strings.TrimSpace(note))
		}
		sb.WriteString("</upstream-transfer-notes>")
		history = append(history, HistoryEntry{IncomingMail: sb.String()})
	}

	// 自身 TransferNote 注入：重试换手场景。前任留下的备忘让新 agent 知道
	// "我为什么被叫醒 + 前任遇到了什么障碍"。即便 LastHistory 已恢复了完整
	// 对话，TransferNote 作为"精炼提示"仍然有价值——它不会被 Layer 2 压缩吞掉。
	if task.RetryCount > 0 && task.TransferNote != "" {
		hint := fmt.Sprintf(
			"<transfer-note>\n"+
				"这是第 %d 次重试。以下是前任代理在终止前留下的交接备忘：\n\n%s\n\n"+
				"请结合上方的 LastHistory（如有）+ 本备忘，从任务目标重新出发，避免重蹈覆辙。\n"+
				"</transfer-note>",
			task.RetryCount, strings.TrimSpace(task.TransferNote),
		)
		history = append(history, HistoryEntry{IncomingMail: hint})
	}

	// PhaseTaskStart：任务级 hook 注入点。
	// 每次 processTask 入口都触发——是否真正注入由各 hook 自行决定
	// （如 TeamAwarenessHook 在 RetryCount > 0 时返回空，避免与 LastHistory
	// 恢复的旧快照重复）。
	// C6 之后，硬编码的 TeamSnapshot 注入被 TeamAwarenessHook 完全取代。
	if injected := a.runAgentInject(ctx, hook.PhaseTaskStart, taskID, -1, false); injected != "" {
		history = append(history, HistoryEntry{
			IncomingMail: injected,
		})
	}

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

	for i := 0; i < a.MaxLoops; i++ {
		select {
		case <-ctx.Done():
			// 2026-04-25 P1 #2：取消类终态 trace 事件。由外部（watchdog /
			// cancel_task / 用户 /cancel / agent 关停）触发的 ctx 取消。
			trace.Emit(trace.Event{
				Kind:    trace.KindTaskCancelled,
				TaskID:  taskID,
				AgentID: a.ID,
				Loop:    i,
				Reason:  ctx.Err().Error(),
			})
			return
		default:
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

		// PhaseLoopPre：每轮开头的注入类 hook 触发点。
		// 必须在 mailbox drain 之后——hook 通过 HasNewMail 决定是否强制刷新
		// （例如 TeamAwarenessHook 在收到消息后下一轮立即重算团队快照）。
		// 必须在 LLM 调用之前——注入内容参与本轮 LLM 决策。
		if injected := a.runAgentInject(ctx, hook.PhaseLoopPre, taskID, i, hasNewMail); injected != "" {
			history = append(history, HistoryEntry{
				IncomingMail: injected,
			})
		}

		// 前置检查：如果设置了 FinalizationChecker 且已 finalized，
		// 说明上一轮调用了 finalization tool，立即终止 reactLoop。
		if a.FinalizationChecker != nil && a.FinalizationChecker.IsFinalized() {
			log.Printf("[agent %s] FinalizationChecker.IsFinalized()=true，终止 reactLoop (task=%s)", a.ID, taskID)
			// 使用上一轮保存的 lastOutput 完成任务（不进行 ExpectedArtifacts 校验，因为 finalization tool 负责最终汇报）
			// TransferNote：成功路径直接用 lastOutput（LLM 自述已经是合理总结，不需二次压缩）
			if lastOutput != "" {
				_ = a.Store.SetTransferNote(taskID, lastOutput)
			}
			if err := a.Store.SubmitResult(a.ID, taskID, lastOutput); err != nil {
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
					OutputLen: len(lastOutput),
					LoopsUsed: i,
				})
				trace.Emit(trace.Event{
					Kind:    trace.KindTaskCompleted,
					TaskID:  taskID,
					AgentID: a.ID,
				})
			}
			return
		}

		// §11.7.4 layer-3 截断保护：每轮 LLM 调用前校验预测 prompt_tokens 不超
		// per-kind ContextLimit。2026-04-27 升级为双层降级 cascade（详见
		// token_truncate.go TruncateHistory）：
		//   Layer A：从最老条目开始删 middle，保护 head=1 + tail=3
		//   Layer B：tail 内 fat ToolResult 内容级缩减（head/tail 保留 + 中间截断标记）
		// 失败模式（双层都跑过仍超）：warn + 用尽力截断的 history 继续——让 LLM 调用
		// 真不行时由 §9.3 ErrUnrecoverable 兜底。ContextLimit<=0（v3 兼容路径）no-op。
		if a.ContextLimit > 0 {
			before := PredictNextPromptTokens(history, a.Model, task.SystemPrompt, "")
			truncated, terr := TruncateHistory(history, a.Model, task.SystemPrompt, a.ContextLimit)
			// "已经发生了改动"判定：长度变了或某 entry 内的 ToolResults 被 shrink 过。
			// 仅看长度会漏掉 Layer B 的内容级缩减；用预测值差异更可靠。
			afterPredicted := PredictNextPromptTokens(truncated, a.Model, task.SystemPrompt, "")
			if len(truncated) != len(history) || afterPredicted != before || terr != nil {
				log.Printf("[agent %s] task=%s loop=%d 历史截断: %d→%d entries, ~%d→~%d prompt tokens",
					a.ID, taskID, i, len(history), len(truncated), before, afterPredicted)
				trace.Emit(trace.Event{
					Kind:               trace.KindHistoryTruncated,
					TaskID:             taskID,
					AgentID:            a.ID,
					Loop:               i,
					PromptTokensBefore: before,
					PromptTokensAfter:  afterPredicted,
					KeptEntries:        len(truncated),
					Strategy:           "drop_middle+shrink_tail",
				})
				if terr != nil {
					log.Printf("[agent %s] task=%s loop=%d 双层截断（删 middle + 缩 tail 内容）后仍 ~%d > context_limit=%d: %v（用截断 history 继续，依赖 §9.3 错误码兜底）",
						a.ID, taskID, i, afterPredicted, a.ContextLimit, terr)
				}
				history = truncated
			}
		}

		// 构建只读副本传入 executor
		histCopy := make([]HistoryEntry, len(history))
		copy(histCopy, history)

		// 注入 agentID、taskID、循环轮次到 context，供 llm_executor 和工具层日志/trace 使用
		execCtx := WithAgentContext(ctx, a.ID, taskID, i)
		result, execErr := a.Execute(execCtx, task, depResults, histCopy)

		if execErr != nil {
			a.handleFailure(task, taskID, execErr, history)
			return
		}

		lastOutput = result.Output
		totalPromptTokens += result.PromptTokens

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
				fmt.Printf("\n=== 任务完成 ===\n%s\n================\n\n", lastOutput)
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
			check := checkExpectedArtifacts(a.Store, taskID)
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
				a.handleFailure(task, taskID, &ErrRecoverable{Err: fmt.Errorf("%s", reason)}, history)
				return
			}
			if len(check.Drifted) > 0 {
				log.Printf("[agent %s] 任务 %s 路径漂移已容忍: %v", a.ID, taskID, check.Drifted)
			}

			// TransferNote：成功路径把 lastOutput 直接写入 TransferNote。
			// lastOutput 是 LLM 对任务的最终文本响应，本身就是合理的自述总结，
			// 不需要额外的 LLM 压缩调用。下游依赖任务通过 GetDependencyTransferNotes
			// 读取这一段作为上游交接备忘。失败路径走 handleFailure 里的 buildTransferNote。
			if lastOutput != "" {
				_ = a.Store.SetTransferNote(taskID, lastOutput)
			}

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
				trace.Emit(trace.Event{
					Kind:    trace.KindTaskCompleted,
					TaskID:  taskID,
					AgentID: a.ID,
				})
			}
			return
		}

		// 流式进度写回：每步工具执行结果追加到 Store，供 Scheduler 快照读取
		if err := a.Store.AppendOutput(a.ID, taskID, result.Output); err != nil {
			log.Printf("[agent %s] AppendOutput error: %v", a.ID, err)
		}

		// ToolCalled == true：追加到历史，继续循环
		// PromptTokens / CompletionTokens / Model 用于 §11.7.3 实测锚定的下次预测，
		// 详见 PredictNextPromptTokens / TruncateHistory。Model 字段为空串时（Agent
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

		// PhaseLoopPost：每轮末尾的观察类 hook 触发点。
		// 触发时机：tool results 已追加到 history、压缩策略尚未执行。
		// hook 能看到本轮完整结果，但不会影响后续压缩逻辑。
		a.runAgentObserve(ctx, hook.PhaseLoopPost, taskID, i)

		// Layer 1: 清理旧的高输出工具结果
		snipOldToolResults(history, keepRecent)

		// Layer 2: token 累计超过阈值时触发摘要压缩（每次任务最多一次）
		if !summarized && totalPromptTokens > compactThreshold {
			tokensBefore := totalPromptTokens
			entriesBefore := len(history)
			history = compressHistory(history, keepRecent)
			summarized = true
			log.Printf("[agent %s] 任务 %s 触发历史摘要压缩，当前 prompt tokens: %d", a.ID, taskID, totalPromptTokens)
			trace.Emit(trace.Event{
				Kind:               trace.KindHistoryCompaction,
				TaskID:             taskID,
				AgentID:            a.ID,
				Loop:               i,
				PromptTokensBefore: tokensBefore,
				PromptTokensAfter:  0, // 实际值要等下次 LLM 调用才能拿到，这里只记录"压缩前"信号
				Strategy:           fmt.Sprintf("summary+keep_recent=%d", keepRecent),
				KeptEntries:        entriesBefore,
			})
		}
	}

	reason := fmt.Sprintf("因循环上限终止: 已执行 %d 轮，部分结果: %s", a.MaxLoops, lastOutput)

	// 保存当前历史到任务，供下次重试恢复上下文
	a.saveHistory(task, history)

	// TransferNote：MaxLoops 耗尽路径走 buildTransferNote（L1 → L3 链）。
	// L1 会追加 <transfer-request> 指令做最后一次 LLM 压缩；失败则 L3 机械兑底。
	// 注意：此处 history 已经是最后状态（包含所有 loop 的结果），
	// buildTransferNote 内部对 history 只读，不修改原切片。
	//
	// 关键：processTask 循环里用的是 `execCtx := WithAgentContext(...)`（新变量），
	// 入参 ctx 本身从未被注入。直接传 ctx 进 buildTransferNote 会导致 L1 那次
	// LLM 调用在 trace / log 里缺 agent_id / loop 字段（2026-04-25 P1 #1）。
	// 此处显式补一次注入，loop=-1 标记"非 ReactLoop 的终止路径调用"，
	// 便于 trace 工具区分主循环事件与交接备忘事件。
	tnCtx := WithAgentContext(ctx, a.ID, taskID, -1)
	note := a.buildTransferNote(tnCtx, task, depResults, history, a.transferNoteMaxTokens())
	if note != "" {
		_ = a.Store.SetTransferNote(taskID, note)
	}

	// 检查重试次数是否已耗尽，避免无限重试
	if a.MaxRetries > 0 && task.RetryCount >= a.MaxRetries {
		failReason := fmt.Sprintf("重试次数耗尽 (%d/%d): %s", task.RetryCount, a.MaxRetries, reason)
		a.terminateTask(task, taskID, failReason)
		return
	}

	if err := a.Store.RetryRollback(a.ID, taskID, reason); err != nil {
		if errors.Is(err, store.ErrTaskNotProcessing) {
			log.Printf("[agent %s] 任务 %s RetryRollback (max loops) 跳过：状态已被外部转换", a.ID, taskID)
		} else {
			log.Printf("[agent %s] RetryRollback (max loops) error: %v", a.ID, err)
		}
	} else {
		// 2026-04-25 P1 #2：重试 trace 事件。AttemptNo 记新一轮的序号
		// （task.RetryCount 在 RetryRollback 内部递增，读当前值即是下一次尝试号）。
		trace.Emit(trace.Event{
			Kind:      trace.KindTaskRetry,
			TaskID:    taskID,
			AgentID:   a.ID,
			Reason:    "max_loops: " + reason,
			AttemptNo: task.RetryCount,
		})
	}
}

func (a *Agent) handleFailure(task *model.Task, taskID string, execErr error, history []HistoryEntry) {
	var recoverable *ErrRecoverable
	if errors.As(execErr, &recoverable) {
		// Layer 3: 如果是上下文溢出错误，在重试前激进压缩历史
		overflow := isContextOverflow(execErr)
		if overflow {
			log.Printf("[agent %s] 任务 %s 检测到上下文溢出，执行激进压缩", a.ID, taskID)
			snipOldToolResults(history, 1)        // 激进清理：只保留最近 1 条
			history = compressHistory(history, 1) // 激进压缩：只保留最近 1 条
		}

		// 预判是否即将 terminate——这个判断决定 note 的成本策略。
		willTerminate := a.MaxRetries > 0 && task.RetryCount >= a.MaxRetries

		// TransferNote 分类策略（2026-04-25 重构）：
		//
		// 旧行为：任何 recoverable 失败都调 L1（一次额外 LLM 调用），L1 失败降级 L3。
		// 代价：LLM 服务宕机时每次 retry 都烧一次无效 dial；即使服务正常，
		// 普通 transient 失败也调 LLM 做"信息与 history 高度重复"的总结。
		//
		// 新行为按场景分派：
		//   - overflow：压缩已经把 history 缩到 1 条，L1 是唯一保住 reasoning 链的路径——调 L1
		//   - willTerminate：任务即将进入 failed 终态，note 会被下游 + crashReport 消费——调 L1
		//   - 其他 transient（网络 / 5xx / rate limit / ExpectedArtifacts 失败）：
		//     LastHistory 完整保留、retry 接手者能靠 history 恢复，L1 价值低且大概率失败——
		//     走零成本的 L3 mechanical 兜底，保留结构化交接文本，但不烧 LLM 调用
		//
		// 两条路径都经过 SetTransferNote；下游依赖 / retry 接手者读到的
		// note 格式一致（都是 <transfer-note> / <transfer-note level="raw"> 包裹的文本）。
		tnCtx := WithAgentContext(context.Background(), a.ID, taskID, -1)
		var note string
		if overflow || willTerminate {
			// L1 高价值路径：允许一次 LLM 调用做自然语言压缩，失败自动降级 L3
			note = a.buildTransferNote(tnCtx, task, nil, history, a.transferNoteMaxTokens())
		} else {
			// 普通 transient：直接 L3 机械拼装，零 LLM 调用
			var toolHistory []store.ToolCallRecord
			if a.Store != nil && task != nil {
				toolHistory, _ = a.Store.QueryToolCalls(task.ID, "")
			}
			note = mechanicalTransferNote(task, history, toolHistory, a.transferNoteMaxTokens())
		}
		if note != "" {
			_ = a.Store.SetTransferNote(taskID, note)
		}

		// 全局重试上限：可恢复错误也要受 MaxRetries 约束，避免无限重试。
		// 此前只有 handleMaxLoops 路径检查 MaxRetries，导致 ExpectedArtifacts 校验
		// 失败、tool 错误等可恢复故障可以无限循环（实战中观察到 24+ 次重试，烧 2 小时）。
		if willTerminate {
			failReason := fmt.Sprintf("重试次数耗尽 (%d/%d): %s",
				task.RetryCount, a.MaxRetries, execErr.Error())
			log.Printf("[agent %s] 任务 %s 终止：%s", a.ID, taskID, failReason)
			a.terminateTask(task, taskID, failReason)
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
			})
		}
	} else {
		// 不可恢复错误：先记 TransferNote（走纯机械 L3，不调 LLM——execErr 很可能是
		// LLM 自身故障，再调一次只会失败），然后终止 + 崩溃汇报
		var toolHistory []store.ToolCallRecord
		if a.Store != nil && task != nil {
			toolHistory, _ = a.Store.QueryToolCalls(task.ID, "")
		}
		note := mechanicalTransferNote(task, history, toolHistory, a.transferNoteMaxTokens())
		if note != "" {
			_ = a.Store.SetTransferNote(taskID, note)
		}
		reason := diagnoseLLMError(execErr, history, a.Model)
		log.Printf("[agent %s] 任务 %s 不可恢复错误：%s", a.ID, taskID, reason)
		a.terminateTask(task, taskID, reason)
	}
}

// terminateTask 是任务最终失败的统一收口：
//  1. 通过 FailTask 把任务状态原子转换到 failed
//  2. 向任务的 EventSource（发布者，通常是 scheduler 或父代理）发送一条结构化崩溃邮件，
//     避免上游静默等待。崩溃邮件遵循固定格式："代理 X 在执行任务 Y 时崩溃，原因 Z"。
func (a *Agent) terminateTask(task *model.Task, taskID string, reason string) {
	if err := a.Store.FailTask(a.ID, taskID, reason); err != nil {
		log.Printf("[agent %s] FailTask error: %v", a.ID, err)
	}
	// 2026-04-25 P1 #2：失败终态 trace 事件。在此前 trace 只记 task_submitted /
	// task_completed 两种成功终态，非成功路径对 trace reader 完全不可见。
	trace.Emit(trace.Event{
		Kind:    trace.KindTaskFailed,
		TaskID:  taskID,
		AgentID: a.ID,
		Reason:  reason,
	})
	a.sendCrashReport(task, taskID, reason)
}

// sendCrashReport 向 task.EventSource 发送结构化崩溃通知。
// 没有 EventSource 或没有邮箱注册表时静默跳过（避免在测试场景报错）。
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
	if a.MailRegistry == nil || task == nil || task.EventSource == "" || task.EventSource == "user" {
		return
	}
	// 重读 task 以拿到最新的 Artifacts / LastResponse
	if fresh, err := a.Store.GetTask(taskID); err == nil && fresh != nil {
		task = fresh
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
		To:       task.EventSource,
		Type:     mailbox.MsgTypeInfo,
		Priority: mailbox.PriorityHigh,
		Summary:  summary,
		Content:  body,
		SentAt:   time.Now(),
	}
	if err := a.MailRegistry.Send(msg); err != nil {
		log.Printf("[agent %s] 发送崩溃汇报失败: %v", a.ID, err)
	} else {
		log.Printf("[agent %s] 已向 %s 汇报任务 %s 崩溃", a.ID, task.EventSource, taskID[:8])
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
		return fmt.Sprintf("请求超出模型上下文上限。当前历史长度约 %d tokens，请考虑降低 context_limit 或开启更积极的历史压缩。", estTokens)
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
	if len(history) == 0 {
		return
	}
	data, err := json.Marshal(history)
	if err != nil {
		log.Printf("[agent %s] 序列化历史记录失败: %v", a.ID, err)
		return
	}
	task.LastHistory = data
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
func NewAgent(id, eventType string, s store.TaskStore, r roster.Roster, exec TaskExecutor, maxLoops int) *Agent {
	return &Agent{
		ID:           id,
		EventType:    eventType,
		Store:        s,
		Roster:       r,
		Execute:      exec,
		MaxLoops:     maxLoops,
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
	"run_shell":   true,
	"read_file":   true,
	"grep_search": true,
	"glob_search": true,
}

// snipOldToolResults 清理历史中旧的高输出工具结果（Layer 1）。
// 对每种目标工具，保留最近 keepRecent 条结果不变，更早的结果用占位符替换 Content。
// 直接修改 history 切片中的 ToolResults。
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
				entry.ToolResults[j].Content = "[已清空，内容过长]"
			}
		}
	}
}

// buildHistorySummary 从历史条目中构建文本摘要（不调用 LLM）。
func buildHistorySummary(history []HistoryEntry) string {
	var sb strings.Builder
	sb.WriteString("=== 历史摘要 ===\n")
	for i, entry := range history {
		sb.WriteString(fmt.Sprintf("步骤 %d: ", i+1))
		if entry.ToolCalled && len(entry.ToolCalls) > 0 {
			for _, tc := range entry.ToolCalls {
				sb.WriteString(fmt.Sprintf("[%s] ", tc.Name))
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
