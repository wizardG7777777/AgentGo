package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// TaskResult 一个黄金任务的完整运行结果：指标 + 判据 + 现场路径。
type TaskResult struct {
	Name    string        `json:"name"`
	Workdir string        `json:"workdir"` // 运行现场（trace、child.log、渲染配置），供失败排查
	Metrics RunMetrics    `json:"metrics"`
	Judges  []JudgeResult `json:"judges"`
	Passed  bool          `json:"passed"`
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

// AllPassed 报告级通过线：全部任务判据通过且无 hard 级对比告警。
func (r *RunReport) AllPassed() bool {
	for _, res := range r.Results {
		if !res.Passed {
			return false
		}
	}
	for _, a := range r.Alerts {
		if a.Level == "hard" {
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

// PrintSummary 打印人读摘要：每任务一行 PASS/FAIL + 关键指标 + 判据细节，
// 对比告警按级别分色标注（文本标记 [硬]/[软]，终端无颜色依赖）。
func PrintSummary(w io.Writer, rep *RunReport) {
	fmt.Fprintf(w, "\n========== 评测报告 %s（套件 %s）==========\n", rep.RunID, rep.Suite)
	fmt.Fprintf(w, "环境: model=%s commit=%s config=%s prompt=%s\n",
		rep.Environment.Model, shortHash(rep.Environment.AgentGoCommit),
		shortHash(rep.Environment.ConfigSHA256), shortHash(rep.Environment.PromptSHA256))
	for _, res := range rep.Results {
		mark := "PASS"
		if !res.Passed {
			mark = "FAIL"
		}
		m := res.Metrics
		fmt.Fprintf(w, "\n[%s] %s（%s，%.0fs，tokens %d+%d，loops %d，子任务 %d，replan %d，验收 %d 轮 %s）\n",
			mark, res.Name, m.TerminalStatus, m.WallSec,
			m.PromptTokens, m.CompletionTokens, m.Loops, m.Subtasks, m.Replans,
			m.AcceptanceRounds, m.AcceptanceVerdict)
		for _, j := range res.Judges {
			jmark := "✓"
			if !j.Passed {
				jmark = "✗"
			}
			fmt.Fprintf(w, "    %s %s: %s\n", jmark, j.Spec.Type, j.Detail)
		}
		if !res.Passed {
			fmt.Fprintf(w, "    现场: %s\n", res.Workdir)
		}
	}
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
