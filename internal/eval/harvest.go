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
	TerminalStatus    string         `json:"terminal_status"` // completed / timeout / spawn_error / health_timeout / inject_error
	WallSec           float64        `json:"wall_sec"`
	PromptTokens      int64          `json:"prompt_tokens"`
	CompletionTokens  int64          `json:"completion_tokens"`
	LLMCalls          int            `json:"llm_calls"`
	Loops             int            `json:"loops"`                        // 各任务 task_completed 的 LoopsUsed 求和
	Subtasks          int            `json:"subtasks"`                     // 非 __scheduler__ 的 task_published 数
	Replans           int            `json:"replans"`                      // replan_requested 计数
	AcceptanceRounds  int            `json:"acceptance_rounds"`            // acceptance_completed 计数
	AcceptanceVerdict string         `json:"acceptance_verdict,omitempty"` // 最后一次验收裁决
	Errors            int            `json:"errors"`                       // error 事件 + task_failed 计数
	EventCounts       map[string]int `json:"event_counts,omitempty"`       // 全事件按 kind 计数（judges 数据源）
}

// HarvestEvents 把一次运行收割到的全部 trace 事件聚合进指标。
// token 字段不由本函数填充（权威来源是终态 snapshot，见 SnapshotTokenFallback）。
//
// Loops 的事实源说明：task_completed.loops_used 字段当前发射端未填充
// （2026-07-28 smoke 实测为空），故以「每 (agent,task) 所见最大 loop 序号
// 求和」近似 ReAct 总轮次——重试回滚会重置 loop 计数，属轻微低估，
// 对 ±30% 带宽的对比用途足够。
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
		case trace.KindTaskPublished:
			// 根任务是 Activator 从用户输入合成的 __scheduler__ 任务，
			// 不计入子任务；publish_task 发布的 DAG 节点才计入。
			if ev.EventType != "__scheduler__" {
				m.Subtasks++
			}
		case trace.KindTaskFailed, trace.KindError:
			m.Errors++
		case trace.KindReplanRequested:
			m.Replans++
		case trace.KindAcceptanceCompleted:
			m.AcceptanceRounds++
			if ev.Acceptance != nil {
				m.AcceptanceVerdict = ev.Acceptance.Verdict
			}
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

// CollectTraceEvents 收割 project_root 下的全部 trace 事件：
// 优先 Session logs 目录，兼顾旧 .agentgo/traces 回退路径；坏文件跳过不致命。
func CollectTraceEvents(projectRoot string) []trace.Event {
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
		if err != nil && len(evs) == 0 {
			continue // 完全读不出的文件跳过；partial 读取照常收下
		}
		events = append(events, evs...)
	}
	return events
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
	case "subtasks":
		return float64(m.Subtasks), true
	case "replans":
		return float64(m.Replans), true
	case "acceptance_rounds":
		return float64(m.AcceptanceRounds), true
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
