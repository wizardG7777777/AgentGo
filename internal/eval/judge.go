package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// JudgeResult 一条判据的执行结果。
type JudgeResult struct {
	Spec   JudgeSpec `json:"spec"`
	Passed bool      `json:"passed"`
	Detail string    `json:"detail"`
}

// RunJudges 逐个执行确定性判据。projectRoot 是本次运行的临时 project_root；
// 任一判据失败不影响其余判据执行（报告需要完整证据而非首个失败）。
func RunJudges(specs []JudgeSpec, projectRoot string, m *RunMetrics) []JudgeResult {
	results := make([]JudgeResult, 0, len(specs))
	for _, spec := range specs {
		results = append(results, runOneJudge(spec, projectRoot, m))
	}
	return results
}

func runOneJudge(spec JudgeSpec, projectRoot string, m *RunMetrics) JudgeResult {
	r := JudgeResult{Spec: spec}
	pass := func(detail string) JudgeResult { r.Passed, r.Detail = true, detail; return r }
	fail := func(detail string) JudgeResult { r.Passed, r.Detail = false, detail; return r }

	filePath := filepath.Join(projectRoot, spec.Path)

	switch spec.Type {
	case "task_completed":
		if m.TerminalStatus == "completed" {
			return pass("运行正常收敛")
		}
		return fail(fmt.Sprintf("终态 = %s（期望 completed）", m.TerminalStatus))

	case "file_exists":
		if _, err := os.Stat(filePath); err == nil {
			return pass(fmt.Sprintf("%s 存在", spec.Path))
		}
		return fail(fmt.Sprintf("%s 不存在", spec.Path))

	case "file_contains":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fail(fmt.Sprintf("读取 %s 失败: %v", spec.Path, err))
		}
		content := string(data)
		if spec.Regex {
			re, err := regexp.Compile(spec.Pattern)
			if err != nil {
				return fail(fmt.Sprintf("判据正则非法: %v", err))
			}
			if re.MatchString(content) {
				return pass(fmt.Sprintf("正则 %q 命中", spec.Pattern))
			}
			return fail(fmt.Sprintf("正则 %q 未命中（文件 %d 字节）", spec.Pattern, len(data)))
		}
		if strings.Contains(content, spec.Pattern) {
			return pass(fmt.Sprintf("含子串 %q", spec.Pattern))
		}
		return fail(fmt.Sprintf("不含子串 %q（文件 %d 字节）", spec.Pattern, len(data)))

	case "file_hash":
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fail(fmt.Sprintf("读取 %s 失败: %v", spec.Path, err))
		}
		content := data
		note := ""
		if spec.TrimTrailingBlank {
			content = normalizeTrailingBlank(data)
			note = "（尾部空白行已归一）"
		}
		sum := sha256.Sum256(content)
		got := hex.EncodeToString(sum[:])
		if strings.EqualFold(got, spec.SHA256) {
			return pass("SHA256 匹配" + note)
		}
		return fail(fmt.Sprintf("SHA256 不匹配%s: got %s, want %s", note, got, spec.SHA256))

	case "file_min_bytes":
		info, err := os.Stat(filePath)
		if err != nil {
			return fail(fmt.Sprintf("%s 不存在", spec.Path))
		}
		min := int64(0)
		if spec.Min != nil {
			min = int64(*spec.Min)
		}
		if info.Size() >= min {
			return pass(fmt.Sprintf("%d 字节 ≥ 下界 %d", info.Size(), min))
		}
		return fail(fmt.Sprintf("%d 字节 < 下界 %d（疑似截断）", info.Size(), min))

	case "acceptance_pass":
		if m.AcceptanceVerdict == "pass" {
			return pass(fmt.Sprintf("验收 verdict=pass（%d 轮）", m.AcceptanceRounds))
		}
		if m.AcceptanceRounds == 0 {
			return fail("未发生任何验收 run")
		}
		return fail(fmt.Sprintf("最终 verdict = %q（%d 轮）", m.AcceptanceVerdict, m.AcceptanceRounds))

	case "event_count":
		count := m.EventCounts[spec.Kind]
		return checkBounds(fmt.Sprintf("事件 %s 次数", spec.Kind), float64(count), spec, pass, fail)

	case "event_absent":
		count := m.EventCounts[spec.Kind]
		if count == 0 {
			return pass(fmt.Sprintf("无 %s 事件", spec.Kind))
		}
		return fail(fmt.Sprintf("出现 %d 次 %s 事件", count, spec.Kind))

	case "metric_bounds":
		value, ok := MetricValue(m, spec.Metric)
		if !ok {
			return fail(fmt.Sprintf("未知指标名: %q", spec.Metric))
		}
		return checkBounds(fmt.Sprintf("指标 %s", spec.Metric), value, spec, pass, fail)

	default:
		return fail(fmt.Sprintf("未知判据类型: %q", spec.Type))
	}
}

// normalizeTrailingBlank 把内容尾部的空白字符（空格/制表/换行/回车）归一
// 为单个换行符——file_hash 判据 trim_trailing_blank 选项的归一口径。
func normalizeTrailingBlank(data []byte) []byte {
	return append(bytes.TrimRight(data, " \t\r\n"), '\n')
}

// checkBounds 是 event_count / metric_bounds 共用的区间判定。
func checkBounds(label string, value float64, spec JudgeSpec, pass, fail func(string) JudgeResult) JudgeResult {
	if spec.Min != nil && value < *spec.Min {
		return fail(fmt.Sprintf("%s = %v < 下界 %v", label, value, *spec.Min))
	}
	if spec.Max != nil && value > *spec.Max {
		return fail(fmt.Sprintf("%s = %v > 上界 %v", label, value, *spec.Max))
	}
	return pass(fmt.Sprintf("%s = %v（在界内）", label, value))
}
