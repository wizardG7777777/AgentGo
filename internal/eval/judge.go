package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agentgo/internal/trace"
)

// JudgeResult 一条判据的执行结果。
type JudgeResult struct {
	Spec   JudgeSpec `json:"spec"`
	Passed bool      `json:"passed"`
	// Status 是类型化结论：pass / fail / trace_incomplete。
	// trace_incomplete 时 Passed=false——证据不完整不是通过。
	Status string `json:"status"`
	Detail string `json:"detail"`
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
	pass := func(detail string) JudgeResult {
		r.Passed, r.Status, r.Detail = true, StatusPass, detail
		return r
	}
	fail := func(detail string) JudgeResult {
		r.Passed, r.Status, r.Detail = false, StatusFail, detail
		return r
	}
	// incomplete 是完整性护栏结论（V6 §7.7）：证据不完整时判据不得报 pass。
	incomplete := func(detail string) JudgeResult {
		r.Passed, r.Status, r.Detail = false, StatusTraceIncomplete, detail
		return r
	}

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

	case "file_absent":
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			return pass(fmt.Sprintf("%s 未落盘（符合禁止行为契约）", spec.Path))
		} else if err != nil {
			return fail(fmt.Sprintf("检查 %s 失败: %v", spec.Path, err))
		}
		return fail(fmt.Sprintf("%s 已落盘（禁止行为被击穿）", spec.Path))

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

	case "event_count":
		count := m.EventCounts[spec.Kind]
		return checkBounds(fmt.Sprintf("事件 %s 次数", spec.Kind), float64(count), spec, pass, fail)

	case "event_order":
		return judgeEventOrder(spec, m, pass, fail)

	case "event_field":
		return judgeEventField(spec, m, pass, fail)

	case "glob_count":
		matches, err := filepath.Glob(filepath.Join(projectRoot, filepath.FromSlash(spec.Pattern)))
		if err != nil {
			return fail(fmt.Sprintf("glob 模式非法: %v", err))
		}
		return checkBounds(fmt.Sprintf("glob %s 命中文件数", spec.Pattern), float64(len(matches)), spec, pass, fail)

	case "event_absent":
		// 完整性护栏（V6 §7.7）：trace 证据不完整（degraded marker / 分片损坏 /
		// 部分读取）时，「事件未发生」的结论不可信——报 trace_incomplete 而非 pass。
		if len(m.TraceIncompleteReasons) > 0 {
			return incomplete(fmt.Sprintf("trace 证据不完整（%s），无法支撑 %s 事件缺席结论",
				strings.Join(m.TraceIncompleteReasons, "；"), spec.Kind))
		}
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

// judgeEventOrder 断言 kinds 清单中各事件 kind 的首现顺序严格递增
// （任一 kind 未出现即失败）。事件来源是收割后按时间戳排序的 m.TraceEvents。
func judgeEventOrder(spec JudgeSpec, m *RunMetrics, pass, fail func(string) JudgeResult) JudgeResult {
	if len(spec.Kinds) < 2 {
		return fail("event_order 至少需要 2 个 kinds")
	}
	prevIdx := -1
	var detail []string
	for _, kind := range spec.Kinds {
		idx := -1
		for i, ev := range m.TraceEvents {
			if string(ev.Kind) == kind {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fail(fmt.Sprintf("事件 %s 未出现（顺序链断）", kind))
		}
		if idx <= prevIdx {
			return fail(fmt.Sprintf("事件 %s 首现位置 %d 未严格晚于前序（%d）", kind, idx, prevIdx))
		}
		detail = append(detail, fmt.Sprintf("%s@%d", kind, idx))
		prevIdx = idx
	}
	return pass("顺序成立: " + strings.Join(detail, " → "))
}

// judgeEventField 断言至少一个 kind 事件的字段满足条件：equals（字符串化相等）
// 或 non_empty（字段存在且非空）。字段以事件 JSON 表示的点路径寻址
// （如 graph_id、lease.digest）。
func judgeEventField(spec JudgeSpec, m *RunMetrics, pass, fail func(string) JudgeResult) JudgeResult {
	if spec.Kind == "" || spec.Field == "" {
		return fail("event_field 需要 kind 与 field")
	}
	matched := 0
	for _, ev := range m.TraceEvents {
		if string(ev.Kind) != spec.Kind {
			continue
		}
		value, ok := eventFieldValue(ev, spec.Field)
		if !ok {
			continue
		}
		if spec.NonEmpty && value == "" {
			continue
		}
		if spec.Equals != "" && value != spec.Equals {
			continue
		}
		matched++
	}
	if matched > 0 {
		return pass(fmt.Sprintf("%d 个 %s 事件的 %s 满足条件", matched, spec.Kind, spec.Field))
	}
	return fail(fmt.Sprintf("无 %s 事件的字段 %s 满足条件（equals=%q non_empty=%v）",
		spec.Kind, spec.Field, spec.Equals, spec.NonEmpty))
}

// eventFieldValue 经事件 JSON 表示导航点路径取值（字符串化）。
func eventFieldValue(ev trace.Event, dotPath string) (string, bool) {
	data, err := json.Marshal(ev)
	if err != nil {
		return "", false
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", false
	}
	var cur any = doc
	for _, seg := range strings.Split(dotPath, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case float64:
		return fmt.Sprintf("%v", v), true
	case bool:
		return fmt.Sprintf("%v", v), true
	case nil:
		return "", false
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(raw), true
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
