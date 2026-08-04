package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 类型化任务结果状态（V6 §7.7）：skipped / unqualified / not_comparable /
// trace_incomplete 一律不算 pass，不得满足任何 Release gate。
const (
	StatusPass           = "pass"            // 全部判据通过
	StatusFail           = "fail"            // 存在判据失败或行为性异常终态
	StatusSkipped        = "skipped"         // 显式跳过（case skip 标记）
	StatusBlocked        = "blocked"         // 基础设施阻塞（启动/健康/注入失败、取消）
	StatusUnqualified    = "unqualified"     // 资格不全（如 offline case 缺 LLM 脚本）
	StatusNotComparable  = "not_comparable"  // 指纹关键项与基线不同，失去可比性
	StatusTraceIncomplete = "trace_incomplete" // trace 证据不完整，无法支撑结论
)

// TaskResult 一个黄金任务的完整运行结果：状态 + 指标 + 判据 + 现场路径。
type TaskResult struct {
	Name    string        `json:"name"`
	Status  string        `json:"status"`  // 类型化状态（见 Status* 常量）
	Workdir string        `json:"workdir"` // 运行现场（trace、child.log、渲染配置），供失败排查
	Metrics RunMetrics    `json:"metrics"`
	Judges  []JudgeResult `json:"judges"`
}

// deriveTaskStatus 由驱动终态 + 判据结果推导类型化任务状态。
// 优先级：基础设施终态（blocked/fail）先行；判据 fail 优先于 trace_incomplete——
// 有确凿失败证据时如实报 fail，证据不足才降级 trace_incomplete。
func deriveTaskStatus(terminalStatus string, judges []JudgeResult) string {
	switch terminalStatus {
	case "spawn_error", "health_timeout", "inject_error", "script_error", "cancelled":
		return StatusBlocked
	case "skipped":
		return StatusSkipped
	case "unqualified":
		return StatusUnqualified
	case "completed":
		// 正常收敛：由判据定结论（见下）
	case "timeout", "child_exited":
		// 行为性异常：先按判据记录证据，再兜底 fail
	default:
		// 未知终态：同样走判据，兜底 fail
	}
	sawIncomplete := false
	for _, j := range judges {
		if j.Status == StatusTraceIncomplete {
			sawIncomplete = true
			continue
		}
		if !j.Passed {
			return StatusFail
		}
	}
	if sawIncomplete {
		return StatusTraceIncomplete
	}
	switch terminalStatus {
	case "completed":
		return StatusPass
	default:
		// timeout / child_exited / 未知终态：判据全绿但系统未正常收敛，
		// 不能算 pass（行为异常本身就是失败证据）。
		return StatusFail
	}
}

// RunReport 一次 eval run 的总报告（JSON 落盘 + 人读摘要）。
type RunReport struct {
	RunID          string       `json:"run_id"`
	Suite          string       `json:"suite"`
	StartedAt      time.Time    `json:"started_at"`
	Environment    Environment  `json:"environment"`
	Results        []TaskResult `json:"results"`
	Alerts         []Alert      `json:"alerts,omitempty"`
	CompareSkipped string       `json:"compare_skipped,omitempty"` // 跳过基线对比的原因
}

// AllPassed 报告级通过线：全部任务状态为 pass 且无 hard / not_comparable 级对比告警。
// skipped / unqualified / not_comparable / trace_incomplete 一律不算 pass
// （V6 §7.7：这些状态不得满足任何 Release gate）。
func (r *RunReport) AllPassed() bool {
	for _, res := range r.Results {
		if res.Status != StatusPass {
			return false
		}
	}
	for _, a := range r.Alerts {
		if a.Level == "hard" || a.Level == "not_comparable" {
			return false
		}
	}
	return true
}

// SaveReport 把报告 JSON 落盘（父目录自动创建）。
func SaveReport(path string, rep *RunReport) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// PrintSummary 打印人读摘要：每任务一行状态 + 关键指标 + 判据细节，
// 末尾按类型化状态分组汇总；对比告警按级别分色标注（文本标记 [硬]/[软]）。
func PrintSummary(w io.Writer, rep *RunReport) {
	fmt.Fprintf(w, "\n========== 评测报告 %s（套件 %s）==========\n", rep.RunID, rep.Suite)
	fmt.Fprintf(w, "环境: model=%s commit=%s config=%s prompt=%s\n",
		rep.Environment.Model, shortHash(rep.Environment.AgentGoCommit),
		shortHash(rep.Environment.ConfigSHA256), shortHash(rep.Environment.PromptSHA256))
	statusCounts := map[string]int{}
	var statusOrder []string
	for _, res := range rep.Results {
		if _, ok := statusCounts[res.Status]; !ok {
			statusOrder = append(statusOrder, res.Status)
		}
		statusCounts[res.Status]++
		mark := strings.ToUpper(res.Status)
		m := res.Metrics
		fmt.Fprintf(w, "\n[%s] %s（%s，%.0fs，tokens %d+%d，loops %d，errors %d）\n",
			mark, res.Name, m.TerminalStatus, m.WallSec,
			m.PromptTokens, m.CompletionTokens, m.Loops, m.Errors)
		if len(m.TraceIncompleteReasons) > 0 {
			fmt.Fprintf(w, "    ! trace 证据不完整: %s\n", strings.Join(m.TraceIncompleteReasons, "；"))
		}
		for _, j := range res.Judges {
			jmark := "✓"
			if j.Status == StatusTraceIncomplete {
				jmark = "?"
			} else if !j.Passed {
				jmark = "✗"
			}
			fmt.Fprintf(w, "    %s %s: %s\n", jmark, j.Spec.Type, j.Detail)
		}
		if res.Status != StatusPass {
			fmt.Fprintf(w, "    现场: %s\n", res.Workdir)
		}
	}
	// 状态分组汇总（顺序按首现稳定）
	var parts []string
	for _, s := range statusOrder {
		parts = append(parts, fmt.Sprintf("%s %d", s, statusCounts[s]))
	}
	fmt.Fprintf(w, "\n汇总: %s（共 %d 个任务）\n", strings.Join(parts, "，"), len(rep.Results))
	if rep.CompareSkipped != "" {
		fmt.Fprintf(w, "\n[提示] 跳过基线对比: %s\n", rep.CompareSkipped)
	}
	if len(rep.Alerts) > 0 {
		fmt.Fprintf(w, "\n对比告警:\n")
		for _, a := range rep.Alerts {
			tag := "[软]"
			if a.Level == "hard" {
				tag = "[硬]"
			}
			fmt.Fprintf(w, "  %s %s: %s\n", tag, a.Task, a.Detail)
		}
	}
	fmt.Fprintln(w, "")
}

// shortHash 截断 hash 展示（报告可读性）。
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
