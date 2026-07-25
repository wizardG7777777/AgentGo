package builtin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// anomalyStoreView 是 AnomalyReactor 依赖的最小 Store 接口——全部只读。
// store.StoreHookView 天然满足本接口（GetTask / GetToolCallHistory 均在契约内），
// bootstrap 直接传入现有 storeView 即可。
type anomalyStoreView interface {
	GetTask(taskID string) (*model.Task, error)
	GetToolCallHistory(taskID string) []store.ToolCallRecord
}

// 编译期断言：store.StoreHookView 满足本接口（*store.MemoryTaskStore 随之满足）。
var _ anomalyStoreView = (store.StoreHookView)(nil)

// anomalyReplanRequester 是 AnomalyReactor 可见的唯一控制面写接口。
// 与 userdef.ReplanRequester 同级——Reactor 只能"请求"重规划，决策权在
// Scheduler；这是 Reactor 原则 4 下唯一被允许的状态影响通道。
type anomalyReplanRequester interface {
	RequestReplan(ctx context.Context, request model.ReplanRequest) (*model.ReplanRequest, error)
}

// 编译期断言：*plan.Coordinator 满足本接口。
var _ anomalyReplanRequester = (*plan.Coordinator)(nil)

// 异常码。同时用作 ReplanRequest.ReasonCode 与幂等键组成段——
// Coordinator 侧审计（trace plan / PlanSignal）可直接按码归类。
const (
	// anomalyCodeFabricatedWrite：任务成功 write_file 但全程未 read_file
	// （疑似无源材料的捏造写入）。对应 cli.go detectAnomalies #3 的运行时口径。
	anomalyCodeFabricatedWrite = "anomaly_fabricated_write"
	// anomalyCodeToolErrorRate：工具调用错误率 >30%（样本 >=5）。
	// 对应 cli.go detectAnomalies #5 的运行时口径。
	anomalyCodeToolErrorRate = "anomaly_tool_error_rate"
)

// taskAnomaly 是一条检出的异常：机器可读的码 + 人类可读的详情。
type taskAnomaly struct {
	code   string
	detail string
}

// detectTaskAnomalies 在任务的 ToolCallRecord 序列上运行运行时异常启发式（纯函数）。
//
// 与 internal/trace/cli.go detectAnomalies 的口径对应关系（按 cli.go 内编号）：
//   - fabricated_write 对应 cli.go #3「write_file 但全程无 read_file」
//   - tool_error_rate  对应 cli.go #5「tool 错误率超过 30%」
//
// 刻意的口径分叉（运行时 Store 数据 vs 事后 trace 事件审计）：
//  1. cli.go 统计 trace 事件（KindToolCall / KindToolResult），本函数统计 Store 的
//     ToolCallRecord——两者记录同一批调用，但 record 显式携带 Success 标志。
//  2. fabricated_write 只把 Success=true 的 write_file 记为"写入发生"：被 Gate
//     Abort 的写（Success=false）没有落盘，不构成"捏造写入"事实。cli.go #3 从事后
//     事件流不区分 Gate Abort 与真实写入，一律计入——运行时口径更严（更少误报）。
//  3. read_file 只要调用过即算"有源材料尝试"（不强制 Success），与 cli.go #3 的
//     ToolCall 口径一致——避免"尝试读了不存在的文件再创建"被误报。
//  4. tool_error_rate 把 Success=false 全量计入（含被 Gate 拒绝的调用）——反复被
//     Gate 拦截同样是执行异常信号；cli.go #5 的 KindToolResult.Error 口径同样包含
//     Abort 后返回给 LLM 的错误结果，两者语义对齐。
//  5. 与 cli.go 一致：list_dir / grep_search / web_fetch 等不算"已读"（只有
//     read_file 提供文件级源材料）；样本 <5 跳过防小样本抖动；阈值严格 >30%。
//
// 为什么不实现 cli.go #2（任务完成但无 file_written）：
// 运行时该场景已被 enforce-expected-artifacts Gate + agent.checkExpectedArtifacts
// 双重把守（声明 ExpectedArtifacts 的任务无法绕过校验完成），剩余未声明场景从
// Store 数据无法区分"纯文本交付"（KindTextOnlySubmission 是 trace 事件侧的判别，
// Store 无对应事实可查）——做了只会与既有防线重复报警且误报率高，故只落地
// 信号最高的 ②③ 两条。
func detectTaskAnomalies(history []store.ToolCallRecord) []taskAnomaly {
	var anomalies []taskAnomaly

	hasWrite := false // 至少一次 write_file 成功落盘
	hasRead := false  // 至少一次 read_file 调用（不限成败，口径见上）
	total := len(history)
	errs := 0
	for _, rec := range history {
		switch rec.ToolName {
		case "write_file":
			if rec.Success {
				hasWrite = true
			}
		case "read_file":
			hasRead = true
		}
		if !rec.Success {
			errs++
		}
	}

	// fabricated_write（cli.go #3）：写入真实发生，但全程无任何 read_file。
	if hasWrite && !hasRead {
		anomalies = append(anomalies, taskAnomaly{
			code:   anomalyCodeFabricatedWrite,
			detail: "任务成功调用 write_file 写入文件，但全程未调用 read_file（疑似无源材料的捏造写入）",
		})
	}

	// tool_error_rate（cli.go #5）：错误率 >30%，样本 <5 跳过。
	if total >= 5 && errs*100/total > 30 {
		anomalies = append(anomalies, taskAnomaly{
			code: anomalyCodeToolErrorRate,
			detail: fmt.Sprintf("工具调用错误率 %d%% (%d/%d)——工具集、参数或路径校验可能有问题",
				errs*100/total, errs, total),
		})
	}

	return anomalies
}

// AnomalyReactor 把 trace CLI 的事后异常检测（agentgo trace stats →
// detectAnomalies）前置为运行时监视循环：订阅 KindTaskCompleted，用 Store 的
// 运行时数据（ToolCallRecord / Task.PlanID）复刻最高信号的两条启发式，
// 命中即请求重规划（任务有 Plan）或发 trace 告警（无 Plan）。
//
// 纪律（Reactor 原则 4——不直接驱动新状态转换）：
//   - 本 Reactor 不调用 TransitionState / FailTask / RetryRollback 等任何任务
//     状态 API；依赖的 anomalyStoreView 只暴露读方法，结构上无法改任务。
//   - 唯一控制面出口是 Coordinator.RequestReplan：写一条 ReplanRequest 事实，
//     由 Scheduler 在适当时机决策（与 userdef request_replan 同一通道）。
//
// 防循环设计：
//   - 同一 (taskID, 异常码) 进程内只报一次（reported 集合，mark-before-report）。
//     任务重试后再次 completed 触发同一事件时不会重复报警。
//   - 幂等键 anomaly_reactor|<taskID>|<code> 让 Coordinator 侧持久去重——
//     命中 RequestKeyIndex 时返回已存请求而不新增（跨进程重启仍有效）。
//   - 重规划产生的新任务有新 taskID，不受上述去重影响：同一异常在新执行事实上
//     再次出现时仍会报警——这是刻意设计（每次执行独立审计），但 Scheduler 看到
//     的是同一 Plan 下收敛的 PendingReplanRequests，不会无限放大。
//   - 本 Reactor 只订阅 KindTaskCompleted，自身发出的 KindError 告警不会
//     重新触发自己，无自激回路。
type AnomalyReactor struct {
	store     anomalyStoreView
	requester anomalyReplanRequester

	mu       sync.Mutex
	reported map[string]struct{} // "taskID|code" → 已上报标记
}

// NewAnomalyReactor 构造 Reactor。store 为 nil 时 Run 全部静默 no-op
// （测试 / 最小注册场景）；requester 为 nil 时有 Plan 的任务也降级为 trace 告警。
func NewAnomalyReactor(s anomalyStoreView, requester anomalyReplanRequester) *AnomalyReactor {
	return &AnomalyReactor{
		store:     s,
		requester: requester,
		reported:  make(map[string]struct{}),
	}
}

func (r *AnomalyReactor) Name() string  { return "runtime-anomaly" }
func (r *AnomalyReactor) IsSync() bool  { return false }
func (r *AnomalyReactor) Priority() int { return 950 }

func (r *AnomalyReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindTaskCompleted}
}

// Run 在任务完成事件上跑异常启发式并上报。失败仅返回 error 由 Async 路径记日志。
func (r *AnomalyReactor) Run(ev trace.Event) error {
	if r.store == nil || ev.TaskID == "" {
		return nil
	}
	anomalies := detectTaskAnomalies(r.store.GetToolCallHistory(ev.TaskID))
	if len(anomalies) == 0 {
		return nil
	}
	// PlanID 决定出口。GetTask 失败（任务已被淘汰等）按无 Plan 降级——
	// 异常信号不丢，只是失去重规划通道。
	planID := ""
	if task, err := r.store.GetTask(ev.TaskID); err == nil && task != nil {
		planID = task.PlanID
	}
	var firstErr error
	for _, a := range anomalies {
		if !r.markReported(ev.TaskID, a.code) {
			continue // 同一任务同一异常码已报过（如重试后再次 completed）
		}
		if err := r.report(ev, planID, a); err != nil {
			log.Printf("[anomaly] 任务 %s 命中 %s 但上报失败: %v", ev.TaskID, a.code, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// markReported 返回 true 表示该 (taskID, code) 首次上报；false 表示已报过。
func (r *AnomalyReactor) markReported(taskID, code string) bool {
	key := taskID + "|" + code
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.reported[key]; ok {
		return false
	}
	r.reported[key] = struct{}{}
	return true
}

// report 把单条异常上报到控制面（有 Plan → RequestReplan）或 trace（无 Plan →
// KindError 告警）。Plan 已终态 / 不存在时降级为 trace 告警，保住信号。
func (r *AnomalyReactor) report(ev trace.Event, planID string, a taskAnomaly) error {
	if planID != "" && r.requester != nil {
		_, err := r.requester.RequestReplan(context.Background(), model.ReplanRequest{
			PlanID:         planID,
			SourceTaskID:   ev.TaskID,
			SourceEvent:    "anomaly_reactor",
			ReasonCode:     a.code,
			Detail:         a.detail,
			Urgency:        model.ReplanUrgencyNormal,
			IdempotencyKey: fmt.Sprintf("anomaly_reactor|%s|%s", ev.TaskID, a.code),
			// ObservedRevision / ObservedStateVersion 留零——appendRequest 会以
			// Plan 当前版本填充，Reactor 不需要也不应猜测控制面版本。
		})
		if err == nil {
			log.Printf("[anomaly] 任务 %s 命中 %s，已请求重规划 plan=%s", ev.TaskID, a.code, planID)
			return nil
		}
		// Plan 不可重规划（已终态 / 不存在）：重规划无意义，降级为 trace 告警。
		if !errors.Is(err, plan.ErrPlanNotFound) && !errors.Is(err, plan.ErrPlanTerminal) {
			return fmt.Errorf("request_replan: %w", err)
		}
		log.Printf("[anomaly] 任务 %s 命中 %s，plan=%s 不可重规划（%v），降级为 trace 告警",
			ev.TaskID, a.code, planID, err)
	}
	emitAnomalyWarning(ev, a)
	return nil
}

// emitAnomalyWarning 发一条 KindError trace 事件——无 Plan 任务的告警出口，
// 与 cli.go detectAnomalies 输出的 "WARNING ..." 行对应（trace show / Dashboard 可见）。
func emitAnomalyWarning(ev trace.Event, a taskAnomaly) {
	trace.Emit(trace.Event{
		Kind:    trace.KindError,
		TaskID:  ev.TaskID,
		AgentID: ev.AgentID,
		Error:   fmt.Sprintf("WARNING anomaly_reactor: %s [%s]", a.detail, a.code),
	})
}

// 编译期断言 AnomalyReactor 实现 Reactor 接口。
var _ reactor.Reactor = (*AnomalyReactor)(nil)
