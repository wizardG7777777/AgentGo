package builtin

import (
	"fmt"
	"log"
	"sync"

	"agentgo/internal/reactor"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// anomalyStoreView 是 AnomalyReactor 依赖的最小 Store 接口——全部只读。
// store.StoreHookView 天然满足本接口（GetToolCallHistory 在契约内），
// bootstrap 直接传入现有 storeView 即可。
type anomalyStoreView interface {
	GetToolCallHistory(taskID string) []store.ToolCallRecord
}

// 编译期断言：store.StoreHookView 满足本接口（*store.MemoryTaskStore 随之满足）。
var _ anomalyStoreView = (store.StoreHookView)(nil)

// 异常码。C6b 删除 Plan 控制面后仅作为告警事件里的机器可读归类码
// （历史上同时用作 ReplanRequest.ReasonCode 与幂等键组成段）。
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
//
// workspace 隔离任务的适用性（2026-07-26 C 线核查）：本检测只消费
// ToolCallRecord（工具名 + Success 标志），不做任何文件系统 stat——隔离任务
// 的写入在合并前落在 workspace 副本而非主根，但这不影响记录流：write_file
// 落副本同样记 Success=true，read_file 读穿透主根同样留记录，因此
// fabricated_write / tool_error_rate 两条启发式对隔离与非隔离任务行为一致，
// 无需经 workspace.Manager.ResolveForTask 做路径解析（record-artifact reactor
// 才需要——它按落盘文件重算 sha256）。
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
// 运行时数据（ToolCallRecord）复刻最高信号的两条启发式，命中即发 trace 告警。
//
// C6b 起 Plan 控制面（重规划请求通道）已删除，本 Reactor
// 降级为纯观测：异常仅以 KindError trace 事件形式告警（trace show /
// Dashboard 可见），不再请求重规划。需要按异常重新编排时，由用户 YAML
// Reactor 订阅事件并经 request_replan 动作发布通用唤醒任务。
//
// 纪律（Reactor 原则 4——不直接驱动新状态转换）：
//   - 本 Reactor 不调用 TransitionState / FailTask / RetryRollback 等任何任务
//     状态 API；依赖的 anomalyStoreView 只暴露读方法，结构上无法改任务。
//
// 防循环设计：
//   - 同一 (taskID, 异常码) 进程内只报一次（reported 集合，mark-before-report）。
//     任务重试后再次 completed 触发同一事件时不会重复报警。
//   - 本 Reactor 只订阅 KindTaskCompleted，自身发出的 KindError 告警不会
//     重新触发自己，无自激回路。
type AnomalyReactor struct {
	store anomalyStoreView

	mu       sync.Mutex
	reported map[string]struct{} // "taskID|code" → 已上报标记
}

// NewAnomalyReactor 构造 Reactor。store 为 nil 时 Run 全部静默 no-op
// （测试 / 最小注册场景）。
func NewAnomalyReactor(s anomalyStoreView) *AnomalyReactor {
	return &AnomalyReactor{
		store:    s,
		reported: make(map[string]struct{}),
	}
}

func (r *AnomalyReactor) Name() string  { return "runtime-anomaly" }
func (r *AnomalyReactor) IsSync() bool  { return false }
func (r *AnomalyReactor) Priority() int { return 950 }

func (r *AnomalyReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindTaskCompleted}
}

// Run 在任务完成事件上跑异常启发式并告警（纯观测，无失败路径）。
func (r *AnomalyReactor) Run(ev trace.Event) error {
	if r.store == nil || ev.TaskID == "" {
		return nil
	}
	for _, a := range detectTaskAnomalies(r.store.GetToolCallHistory(ev.TaskID)) {
		if !r.markReported(ev.TaskID, a.code) {
			continue // 同一任务同一异常码已报过（如重试后再次 completed）
		}
		log.Printf("[anomaly] 任务 %s 命中 %s，已发 trace 告警", ev.TaskID, a.code)
		emitAnomalyWarning(ev, a)
	}
	return nil
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

// emitAnomalyWarning 发一条 KindError trace 事件——异常的唯一出口，
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
