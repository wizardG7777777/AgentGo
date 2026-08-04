package eval

import (
	"os"
	"path/filepath"
	"sort"

	"agentgo/internal/trace"
)

// RunMetrics 一次黄金任务运行的全部量化指标。
// 来源双路：trace 事件聚合（行为细节）+ 终态 snapshot 的 Session 级
// token 累计（成本真相——团队销毁后消耗仍在，见 ui.Snapshot 注释）。
type RunMetrics struct {
	TerminalStatus   string         `json:"terminal_status"` // completed / timeout / spawn_error / health_timeout / inject_error
	WallSec          float64        `json:"wall_sec"`
	PromptTokens     int64          `json:"prompt_tokens"`
	CompletionTokens int64          `json:"completion_tokens"`
	LLMCalls         int            `json:"llm_calls"`
	Loops            int            `json:"loops"`                  // 各任务 task_completed 的 LoopsUsed 求和
	Errors           int            `json:"errors"`                 // error 事件 + task_failed 计数
	EventCounts      map[string]int `json:"event_counts,omitempty"` // 全事件按 kind 计数（judges 数据源）
	// TraceEvents 是收割到的全部事件（按时间戳排序），event_order / event_field
	// 等 Trace 事实判据的数据源；只进内存，不落报告 JSON。
	TraceEvents []trace.Event `json:"-"`
	// TraceIncompleteReasons 记录 trace 证据不完整的全部原因（degraded marker
	// 存在 / 分片不可读 / 分片部分读取）。空 = 证据完整。event_absent 等
	// 「缺席即通过」的判据以此为完整性护栏（V6 §7.7）。
	TraceIncompleteReasons []string `json:"trace_incomplete_reasons,omitempty"`
}

// HarvestEvents 把一次运行收割到的全部 trace 事件聚合进指标。
// token 字段不由本函数填充（权威来源是终态 snapshot，见 SnapshotTokenFallback）。
//
// Loops 的事实源说明：task_completed.loops_used 字段当前发射端未填充
// （2026-07-28 smoke 实测为空），故以「每 (agent,task) 所见最大 loop 序号
// 求和」近似 ReAct 总轮次——重试回滚会重置 loop 计数，属轻微低估，
// 对 ±30% 带宽的对比用途足够。
//
// C6c：V5 Plan 时代的 Subtasks（task_published 计数）/ Replans
// （replan_requested 计数）/ AcceptanceRounds / AcceptanceVerdict
// （acceptance_completed 计数与裁决）指标已随对应 trace 事件一并删除；
// V6 验收语义由 Graph acceptance 节点 + submit_task_result 的 verdict
// 契约承担，trace 侧不再产出可聚合的验收事实事件。
func HarvestEvents(events []trace.Event, m *RunMetrics) {
	if m.EventCounts == nil {
		m.EventCounts = map[string]int{}
	}
	maxLoop := map[string]int{}
	for _, ev := range events {
		m.EventCounts[string(ev.Kind)]++
		if ev.Loop > 0 && ev.AgentID != "" {
			key := ev.AgentID + "|" + ev.TaskID
			if ev.Loop > maxLoop[key] {
				maxLoop[key] = ev.Loop
			}
		}
		switch ev.Kind {
		case trace.KindTaskFailed, trace.KindError:
			m.Errors++
		case trace.KindLLMCallEnd:
			m.LLMCalls++
		}
	}
	for _, l := range maxLoop {
		m.Loops += l
	}
}

// SnapshotTokenFallback 在 snapshot 读数缺失时（如启动即失败）用
// llm_call_end 事件求和兜底 token 成本，保证报告不出现误导性的 0。
func SnapshotTokenFallback(events []trace.Event, m *RunMetrics) {
	if m.PromptTokens != 0 || m.CompletionTokens != 0 {
		return
	}
	for _, ev := range events {
		if ev.Kind == trace.KindLLMCallEnd {
			m.PromptTokens += int64(ev.PromptTokens)
			m.CompletionTokens += int64(ev.CompletionTokens)
		}
	}
}

// CollectTraceEvents 收割 project_root 下的全部 trace 事件（完整性状态丢弃）。
// 判据需要完整性护栏时应改用 CollectTraceEventsWithStatus。
func CollectTraceEvents(projectRoot string) []trace.Event {
	events, _ := CollectTraceEventsWithStatus(projectRoot)
	return events
}

// CollectTraceEventsWithStatus 收割 project_root 下的全部 trace 事件并报告
// 证据完整性状态（V6 §7.7：judge 必须先验证证据完整再下结论）：
//   - trace_degraded.marker 存在（session logs / 回退目录任一）→ 不完整；
//   - 分片完全读不出 → 不完整（跳过但不静默）；
//   - 分片部分读取（中途坏行）→ 不完整（已读部分照常收下）。
//
// 返回的事件按时间戳排序——分片文件名序不等于事件时间序（graph 分片与任务
// 分片并行写），event_order 等判据需要真实时序。
func CollectTraceEventsWithStatus(projectRoot string) ([]trace.Event, []string) {
	var incomplete []string
	// degraded marker：Writer 连续写失败期间落下的降级标记
	for _, pattern := range []string{
		filepath.Join(projectRoot, ".agentgo", "sessions", "*", "logs", trace.DegradedMarkerFileName),
		filepath.Join(projectRoot, ".agentgo", "traces", trace.DegradedMarkerFileName),
	} {
		matched, _ := filepath.Glob(pattern)
		for _, p := range matched {
			incomplete = append(incomplete, "trace 降级标记存在: "+filepath.Base(filepath.Dir(p))+"/"+trace.DegradedMarkerFileName)
		}
	}

	var paths []string
	for _, pattern := range []string{
		filepath.Join(projectRoot, ".agentgo", "sessions", "*", "logs", "*.jsonl"),
		filepath.Join(projectRoot, ".agentgo", "traces", "*.jsonl"),
	} {
		matched, _ := filepath.Glob(pattern)
		paths = append(paths, matched...)
	}
	sort.Strings(paths)
	var events []trace.Event
	for _, p := range paths {
		evs, err := trace.ReadEvents(p)
		switch {
		case err != nil && len(evs) == 0:
			incomplete = append(incomplete, "分片不可读: "+filepath.Base(p))
		case err != nil:
			incomplete = append(incomplete, "分片部分读取（存在坏行）: "+filepath.Base(p))
		}
		events = append(events, evs...)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Timestamp.Before(events[j].Timestamp) })
	return events, incomplete
}

// MetricValue 把指标名解析为数值（metric_bounds 判据的求值器）。
// 返回 ok=false 表示未知指标名。
func MetricValue(m *RunMetrics, name string) (float64, bool) {
	switch name {
	case "wall_sec":
		return m.WallSec, true
	case "total_tokens":
		return float64(m.PromptTokens + m.CompletionTokens), true
	case "prompt_tokens":
		return float64(m.PromptTokens), true
	case "completion_tokens":
		return float64(m.CompletionTokens), true
	case "llm_calls":
		return float64(m.LLMCalls), true
	case "loops":
		return float64(m.Loops), true
	case "errors":
		return float64(m.Errors), true
	default:
		return 0, false
	}
}

// ensureDir 是 os.MkdirAll 的语义化别名，保持 runner 代码可读。
func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
