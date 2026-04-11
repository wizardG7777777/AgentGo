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
}

// HistoryEntry 记录 ReAct 循环中单轮 TaskExecutor 调用的结果。
// 包含完整的 tool calling 信息，确保历史消息能正确重建为 OpenAI 协议格式。
type HistoryEntry struct {
	Output           string         `json:"output"`
	ToolCalled       bool           `json:"tool_called"`
	AssistantContent string         `json:"assistant_content"`
	ToolCalls        []llm.ToolCall `json:"tool_calls"`
	ToolResults      []ToolResult   `json:"tool_results"`
	IncomingMail     string         `json:"incoming_mail,omitempty"` // 非空时为收到的代理间邮件，注入为 user 角色消息
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
	OnTaskStart           func(taskID string)               // 任务开始处理时的回调，可选
	OnTaskEnd             func(taskID string, success bool) // 任务结束回调（defer 保证触发），可选
	FileCache             *FileStateCache                   // Agent 级别的文件读取缓存，可选
	Mailbox               *mailbox.Mailbox                  // 代理间通信收件箱，可选
	MailRegistry          *mailbox.Registry                 // 邮箱注册表，用于 DrainWithAck 自动回执
	FinalizationChecker   FinalizationChecker               // 可选；用于 finalization tool 信号检查

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

	var lastOutput string
	history := make([]HistoryEntry, 0)

	// 重试时恢复之前的历史上下文，避免 LLM 丢失上下文重复操作
	if task.RetryCount > 0 && len(task.LastHistory) > 0 {
		if err := json.Unmarshal(task.LastHistory, &history); err != nil {
			log.Printf("[agent %s] 反序列化历史记录失败，从空历史开始: %v", a.ID, err)
			history = make([]HistoryEntry, 0)
		} else {
			log.Printf("[agent %s] 任务 %s 重试 #%d，恢复 %d 条历史记录", a.ID, taskID, task.RetryCount, len(history))
		}
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
			if err := a.Store.SubmitResult(a.ID, taskID, lastOutput); err != nil {
				log.Printf("[agent %s] SubmitResult error: %v", a.ID, err)
			}
			return
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
		history = append(history, HistoryEntry{
			Output:           result.Output,
			ToolCalled:       result.ToolCalled,
			AssistantContent: result.AssistantContent,
			ToolCalls:        result.ToolCalls,
			ToolResults:      result.ToolResults,
		})

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
	}
}

func (a *Agent) handleFailure(task *model.Task, taskID string, execErr error, history []HistoryEntry) {
	var recoverable *ErrRecoverable
	if errors.As(execErr, &recoverable) {
		// Layer 3: 如果是上下文溢出错误，在重试前激进压缩历史
		if isContextOverflow(execErr) {
			log.Printf("[agent %s] 任务 %s 检测到上下文溢出，执行激进压缩", a.ID, taskID)
			snipOldToolResults(history, 1)        // 激进清理：只保留最近 1 条
			history = compressHistory(history, 1) // 激进压缩：只保留最近 1 条
		}

		// 全局重试上限：可恢复错误也要受 MaxRetries 约束，避免无限重试。
		// 此前只有 handleMaxLoops 路径检查 MaxRetries，导致 ExpectedArtifacts 校验
		// 失败、tool 错误等可恢复故障可以无限循环（实战中观察到 24+ 次重试，烧 2 小时）。
		if a.MaxRetries > 0 && task.RetryCount >= a.MaxRetries {
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
		}
	} else {
		// 不可恢复错误：终止 + 崩溃汇报
		log.Printf("[agent %s] 任务 %s 不可恢复错误：%v", a.ID, taskID, execErr)
		a.terminateTask(task, taskID, execErr.Error())
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
	if a.MailRegistry == nil || task == nil || task.EventSource == "" {
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
