package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 延迟阈值：相邻事件间隔超过此值时在 show 输出中标记 WARNING。
const slowGapThreshold = 30 * time.Second

// CLI 实现 `agentgo trace list/show` 子命令的入口。
// args 是 trace 子命令后的剩余参数。dir 是 trace 文件目录。
// 输出写入 out（通常是 os.Stdout）。
func CLI(args []string, dir string, out io.Writer) error {
	if len(args) == 0 {
		return printUsage(out)
	}
	switch args[0] {
	case "list":
		return cmdList(dir, out)
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("usage: agentgo trace show <task_id>")
		}
		return cmdShow(dir, args[1], out)
	case "help", "-h", "--help":
		return printUsage(out)
	default:
		return fmt.Errorf("unknown trace subcommand: %s\n\nrun `agentgo trace help` for usage", args[0])
	}
}

func printUsage(out io.Writer) error {
	_, err := fmt.Fprint(out, `usage: agentgo trace <subcommand> [args]

subcommands:
  list                  列出最近的任务（按发布时间倒序）
  show <task_id>        按时间顺序展示某个任务的全部事件
                        task_id 可以是完整 UUID 或前 8 位短 ID

示例:
  agentgo trace list
  agentgo trace show 321b561d
  agentgo trace show 321b561d-c564-422c-bfa0-b96f54edcb87

实时查看最新任务的事件流（用 tail -f 即可）:
  tail -f .agentgo/traces/$(ls -t .agentgo/traces | head -1) | jq

trace 文件位置: .agentgo/traces/<时间戳>_<task_id前8位>.jsonl
`)
	return err
}

// taskFile 描述一个 trace 文件的元信息（不含内容）。
type taskFile struct {
	path        string
	filename    string
	taskShortID string
	publishedAt time.Time
}

// listTaskFiles 扫描 dir 中的所有 .jsonl trace 文件，按 publishedAt 倒序返回。
// 排除 .prompts.jsonl（属于 prompt dump，不算独立任务文件）。
func listTaskFiles(dir string) ([]taskFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("无法读取 trace 目录 %s: %w", dir, err)
	}
	var files []taskFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".prompts.jsonl") {
			continue
		}
		// 文件名格式: 2026-04-08T04-17-06_321b561d.jsonl
		base := strings.TrimSuffix(name, ".jsonl")
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 {
			continue
		}
		ts, err := time.Parse("2006-01-02T15-04-05", parts[0])
		if err != nil {
			continue
		}
		files = append(files, taskFile{
			path:        filepath.Join(dir, name),
			filename:    name,
			taskShortID: parts[1],
			publishedAt: ts,
		})
	}
	// 按发布时间倒序
	sort.Slice(files, func(i, j int) bool {
		return files[i].publishedAt.After(files[j].publishedAt)
	})
	return files, nil
}

// readAllEvents 读取一个 trace 文件的所有事件。
func readAllEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 4<<20) // 4MB 行缓冲，应对大型 args
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			// 单行解析失败时降级为占位事件，便于排查
			events = append(events, Event{
				Kind:  "<parse_error>",
				Error: fmt.Sprintf("invalid JSON: %v (line: %s)", err, truncate(string(line), 100)),
			})
			continue
		}
		events = append(events, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取 trace 文件失败: %w", err)
	}
	return events, nil
}

// cmdList 实现 agentgo trace list。
func cmdList(dir string, out io.Writer) error {
	files, err := listTaskFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintf(out, "trace 目录 %s 中没有任务文件\n", dir)
		return nil
	}

	// 表头
	fmt.Fprintln(out, "┌──────────┬─────────────────────┬──────────┬────────────┬───────┬───────────┬─────────────┐")
	fmt.Fprintln(out, "│ Task     │ Published           │ Agent    │ Status     │ Loops │ Files Out │ Duration    │")
	fmt.Fprintln(out, "├──────────┼─────────────────────┼──────────┼────────────┼───────┼───────────┼─────────────┤")

	for _, f := range files {
		row := summarize(f)
		fmt.Fprintf(out, "│ %-8s │ %-19s │ %-8s │ %-10s │ %5d │ %9d │ %-11s │\n",
			row.taskShortID,
			row.publishedAt.Local().Format("2006-01-02 15:04:05"),
			truncate(row.agentID, 8),
			row.status,
			row.loops,
			row.filesWritten,
			formatDuration(row.duration),
		)
	}
	fmt.Fprintln(out, "└──────────┴─────────────────────┴──────────┴────────────┴───────┴───────────┴─────────────┘")
	fmt.Fprintf(out, "\n共 %d 个任务，trace 目录: %s\n", len(files), dir)
	return nil
}

// taskSummary 是 list 命令一行的汇总信息。
type taskSummary struct {
	taskShortID  string
	publishedAt  time.Time
	agentID      string
	status       string // pending / running / completed / error / unknown
	loops        int
	filesWritten int
	duration     time.Duration
}

func summarize(f taskFile) taskSummary {
	row := taskSummary{
		taskShortID: f.taskShortID,
		publishedAt: f.publishedAt,
		status:      "unknown",
	}
	events, err := readAllEvents(f.path)
	if err != nil {
		row.status = "read_err"
		return row
	}
	var firstTS, lastTS time.Time
	for _, ev := range events {
		if firstTS.IsZero() || ev.Timestamp.Before(firstTS) {
			firstTS = ev.Timestamp
		}
		if ev.Timestamp.After(lastTS) {
			lastTS = ev.Timestamp
		}
		switch ev.Kind {
		case KindTaskClaimed:
			if row.agentID == "" {
				row.agentID = ev.AgentID
			}
			if row.status == "unknown" || row.status == "pending" {
				row.status = "running"
			}
		case KindTaskPublished:
			if row.status == "unknown" {
				row.status = "pending"
			}
		case KindTaskCompleted:
			row.status = "completed"
		case KindTaskSubmitted:
			if ev.LoopsUsed > row.loops {
				row.loops = ev.LoopsUsed
			}
		case KindFileWritten:
			row.filesWritten++
		case KindError:
			if row.status != "completed" {
				row.status = "error"
			}
		}
	}
	if !firstTS.IsZero() && !lastTS.IsZero() {
		row.duration = lastTS.Sub(firstTS)
	}
	return row
}

// cmdShow 实现 agentgo trace show <task_id>。
func cmdShow(dir, taskIDQuery string, out io.Writer) error {
	files, err := listTaskFiles(dir)
	if err != nil {
		return err
	}

	// 模糊匹配 taskID：完整 UUID、前 8 位短 ID 都接受
	query := taskIDQuery
	if len(query) > 8 {
		query = query[:8]
	}

	var matches []taskFile
	for _, f := range files {
		if strings.HasPrefix(f.taskShortID, query) {
			matches = append(matches, f)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("未找到匹配 task_id=%s 的 trace 文件", taskIDQuery)
	}
	if len(matches) > 1 {
		fmt.Fprintf(out, "找到 %d 个匹配的任务，请使用更长的 task_id 区分:\n", len(matches))
		for _, m := range matches {
			fmt.Fprintf(out, "  %s  %s\n", m.taskShortID, m.publishedAt.Local().Format("2006-01-02 15:04:05"))
		}
		return nil
	}

	f := matches[0]
	events, err := readAllEvents(f.path)
	if err != nil {
		return err
	}

	// 头部信息
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	fmt.Fprintf(out, " Task: %s\n", f.taskShortID)
	fmt.Fprintf(out, " File: %s\n", f.filename)
	fmt.Fprintf(out, " Events: %d\n", len(events))
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")

	// 按时间顺序打印事件，相邻事件超过阈值标 WARNING
	var prev time.Time
	for i, ev := range events {
		ts := ev.Timestamp.Local().Format("15:04:05.000")
		// 检测时间间隔异常（除了首条事件）
		warnPrefix := ""
		if i > 0 && !prev.IsZero() {
			gap := ev.Timestamp.Sub(prev)
			if gap > slowGapThreshold {
				fmt.Fprintf(out, "  WARNING: 距离上一条事件间隔 %s（超过 %s 阈值）\n",
					formatDuration(gap), formatDuration(slowGapThreshold))
				warnPrefix = "  "
			}
		}
		prev = ev.Timestamp

		fmt.Fprintf(out, "%s[%s] %-22s", warnPrefix, ts, ev.Kind)
		if ev.AgentID != "" {
			fmt.Fprintf(out, " agent=%s", ev.AgentID)
		}
		if ev.Loop > 0 || ev.Kind == KindToolCall || ev.Kind == KindToolResult || ev.Kind == KindLLMCallStart || ev.Kind == KindLLMCallEnd {
			fmt.Fprintf(out, " loop=%d", ev.Loop)
		}
		fmt.Fprintln(out)

		// 第二行：事件特定字段
		details := formatEventDetails(ev)
		if details != "" {
			fmt.Fprintf(out, "             %s\n", details)
		}
	}

	// 尾部汇总
	fmt.Fprintln(out, "────────────────────────────────────────────────────────────────────────────────")
	stats := summarize(f)
	fmt.Fprintf(out, " status=%s  agent=%s  loops=%d  files_written=%d  duration=%s\n",
		stats.status, stats.agentID, stats.loops, stats.filesWritten, formatDuration(stats.duration))

	// 异常检测
	anomalies := detectAnomalies(events)
	if len(anomalies) > 0 {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, " WARNING 异常检测:")
		for _, a := range anomalies {
			fmt.Fprintf(out, "   - %s\n", a)
		}
	}
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	return nil
}

// formatEventDetails 把事件的可选字段格式化为单行可读文本。
func formatEventDetails(ev Event) string {
	var parts []string
	switch ev.Kind {
	case KindTaskPublished:
		parts = append(parts, fmt.Sprintf("by=%s", ev.PublishedBy))
		if len(ev.Dependencies) > 0 {
			parts = append(parts, fmt.Sprintf("deps=%v", ev.Dependencies))
		} else {
			parts = append(parts, "deps=[]")
		}
		if ev.EventType != "" {
			parts = append(parts, fmt.Sprintf("type=%s", ev.EventType))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 80)))
		}
	case KindLLMCallStart:
		parts = append(parts, fmt.Sprintf("history_entries=%d tools=%d", ev.HistoryEntries, ev.ToolCallsCount))
	case KindLLMCallEnd:
		parts = append(parts, fmt.Sprintf("duration=%dms", ev.DurationMS))
		if ev.PromptTokens > 0 {
			parts = append(parts, fmt.Sprintf("prompt_tokens=%d", ev.PromptTokens))
		}
		if ev.CompletionTokens > 0 {
			parts = append(parts, fmt.Sprintf("completion_tokens=%d", ev.CompletionTokens))
		}
		if ev.ToolCallsCount > 0 {
			parts = append(parts, fmt.Sprintf("tool_calls=%d", ev.ToolCallsCount))
		}
		if ev.Error != "" {
			parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 80)))
		}
	case KindToolCall:
		parts = append(parts, fmt.Sprintf("tool=%s", ev.Tool))
		if len(ev.Args) > 0 {
			argsJSON, _ := json.Marshal(ev.Args)
			parts = append(parts, fmt.Sprintf("args=%s", truncate(string(argsJSON), 200)))
		}
	case KindToolResult:
		parts = append(parts, fmt.Sprintf("tool=%s duration=%dms", ev.Tool, ev.DurationMS))
		if ev.Error != "" {
			parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 100)))
		} else {
			parts = append(parts, fmt.Sprintf("result_len=%d", ev.ResultLen))
		}
	case KindFileWritten:
		parts = append(parts, fmt.Sprintf("path=%s bytes=%d hash=%s", ev.Path, ev.Bytes, truncate(ev.Hash, 12)))
	case KindHistoryCompaction:
		parts = append(parts, fmt.Sprintf("tokens_before=%d strategy=%s entries=%d",
			ev.PromptTokensBefore, ev.Strategy, ev.KeptEntries))
	case KindTaskSubmitted:
		parts = append(parts, fmt.Sprintf("output_len=%d loops_used=%d", ev.OutputLen, ev.LoopsUsed))
	case KindError:
		parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 200)))

	// === v5 Phase 2 新增：task lifecycle 补 Transition 渲染 ===
	case KindTaskClaimed:
		if ev.Transition != nil {
			parts = append(parts, fmt.Sprintf("prev=%s new=%s",
				ev.Transition.PrevStatus, ev.Transition.NewStatus))
		}
	case KindTaskCompleted:
		if ev.Transition != nil {
			parts = append(parts, fmt.Sprintf("prev=%s new=%s",
				ev.Transition.PrevStatus, ev.Transition.NewStatus))
			if ev.Transition.Cause != "" {
				parts = append(parts, fmt.Sprintf("cause=%s", ev.Transition.Cause))
			}
		}
		if ev.OutputLen > 0 {
			parts = append(parts, fmt.Sprintf("output_len=%d", ev.OutputLen))
		}
	case KindTaskFailed:
		if ev.Transition != nil {
			parts = append(parts, fmt.Sprintf("prev=%s new=%s retry=%d",
				ev.Transition.PrevStatus, ev.Transition.NewStatus,
				ev.Transition.RetryCount))
			if ev.Transition.Cause != "" {
				parts = append(parts, fmt.Sprintf("cause=%s", ev.Transition.Cause))
			}
		}
		if ev.Reason != "" {
			parts = append(parts, fmt.Sprintf("reason=%q", truncate(ev.Reason, 80)))
		}
	case KindTaskCancelled:
		if ev.Transition != nil {
			parts = append(parts, fmt.Sprintf("prev=%s new=%s",
				ev.Transition.PrevStatus, ev.Transition.NewStatus))
			if ev.Transition.CancelSource != "" {
				parts = append(parts, fmt.Sprintf("source=%s", ev.Transition.CancelSource))
			}
		}
		if ev.Reason != "" {
			parts = append(parts, fmt.Sprintf("reason=%q", truncate(ev.Reason, 80)))
		}
	case KindTaskRetry:
		if ev.Transition != nil {
			parts = append(parts, fmt.Sprintf("prev=%s new=%s retry=%d",
				ev.Transition.PrevStatus, ev.Transition.NewStatus,
				ev.Transition.RetryCount))
			if ev.Transition.Cause != "" {
				parts = append(parts, fmt.Sprintf("cause=%s", ev.Transition.Cause))
			}
		}
		if ev.AttemptNo > 0 {
			parts = append(parts, fmt.Sprintf("attempt=%d", ev.AttemptNo))
		}
		if ev.Reason != "" {
			parts = append(parts, fmt.Sprintf("reason=%q", truncate(ev.Reason, 80)))
		}

	// === v5 Phase 2 新增：Agent 状态机 + Shell 三事件 ===
	case KindAgentStateChanged:
		if ev.Transition != nil {
			parts = append(parts, fmt.Sprintf("prev=%s new=%s",
				ev.Transition.PrevState, ev.Transition.NewState))
			if ev.Transition.Cause != "" {
				parts = append(parts, fmt.Sprintf("cause=%s", ev.Transition.Cause))
			}
		}
	case KindShellExecuted:
		if ev.ShellExec != nil {
			parts = append(parts, fmt.Sprintf("cmd=%q exit=%d duration=%dms outcome=%s",
				truncate(ev.ShellExec.Command, 60),
				ev.ShellExec.ExitCode,
				ev.ShellExec.DurationMS,
				ev.ShellExec.Outcome))
		}
	case KindShellTimeoutPending:
		if ev.ShellTimeout != nil {
			parts = append(parts, fmt.Sprintf("cmd=%q elapsed=%ds waits=%d",
				truncate(ev.ShellTimeout.Command, 60),
				ev.ShellTimeout.ElapsedSec,
				ev.ShellTimeout.PreviousWaits))
		}
	case KindShellTimeoutResolved:
		if ev.ShellTimeout != nil {
			parts = append(parts, fmt.Sprintf("cmd=%q decision=%s",
				truncate(ev.ShellTimeout.Command, 60),
				ev.ShellTimeout.Decision))
			if ev.ShellTimeout.Decision == "wait" && ev.ShellTimeout.ExtraSeconds > 0 {
				parts = append(parts, fmt.Sprintf("extra=%ds", ev.ShellTimeout.ExtraSeconds))
			}
		}
	}
	return strings.Join(parts, " ")
}

// detectAnomalies 在事件序列上运行一些基本的异常检测启发式。
// 这是 P0 系统测试中暴露的几类问题的自动检测器。
func detectAnomalies(events []Event) []string {
	var anomalies []string

	// 1. 检测：task_published 但 dependencies 为空，而 description 暗示有依赖
	for _, ev := range events {
		if ev.Kind != KindTaskPublished {
			continue
		}
		if len(ev.Dependencies) > 0 {
			continue
		}
		desc := ev.Description
		hints := []string{"前两个", "前一个", "前序", "依赖", "整合", "汇总", "合并这", "基于上"}
		for _, h := range hints {
			if strings.Contains(desc, h) {
				anomalies = append(anomalies, fmt.Sprintf(
					"WARNING task_published.dependencies=[] 但描述中含 %q（疑似缺少依赖声明）", h))
				break
			}
		}
	}

	// 2. 检测：任务完成但全程无 file_written 事件
	hasComplete := false
	hasFileWritten := false
	hasReadFile := false
	for _, ev := range events {
		switch ev.Kind {
		case KindTaskCompleted:
			hasComplete = true
		case KindFileWritten:
			hasFileWritten = true
		case KindToolCall:
			if ev.Tool == "read_file" {
				hasReadFile = true
			}
		}
	}
	if hasComplete && !hasFileWritten {
		anomalies = append(anomalies, "WARNING 任务已完成但无任何 file_written 事件（report-only 失败模式）")
	}

	// 3. 检测：write_file 出现但全程无 read_file（可能是凭空捏造）
	hasWriteFile := false
	for _, ev := range events {
		if ev.Kind == KindToolCall && ev.Tool == "write_file" {
			hasWriteFile = true
			break
		}
	}
	if hasWriteFile && !hasReadFile {
		anomalies = append(anomalies, "WARNING 任务调用 write_file 但全程未调用 read_file（疑似无源材料的捏造写入）")
	}

	// 4. 检测：history_compaction 触发多次
	compactionCount := 0
	for _, ev := range events {
		if ev.Kind == KindHistoryCompaction {
			compactionCount++
		}
	}
	if compactionCount > 1 {
		anomalies = append(anomalies, fmt.Sprintf("WARNING 历史压缩触发 %d 次（>1 次通常意味着 prompt 持续膨胀）", compactionCount))
	}

	// 5. 检测：tool 错误率超过 30%
	totalCalls := 0
	errCalls := 0
	for _, ev := range events {
		if ev.Kind == KindToolResult {
			totalCalls++
			if ev.Error != "" {
				errCalls++
			}
		}
	}
	if totalCalls >= 5 && errCalls*100/totalCalls > 30 {
		anomalies = append(anomalies, fmt.Sprintf(
			"WARNING 工具调用错误率 %d%% (%d/%d) — 工具集或路径校验可能有问题",
			errCalls*100/totalCalls, errCalls, totalCalls))
	}

	// === v5 Phase 2 新增（TraceUpgrade.md §6.3）===

	// 6. 检测：agent 在 waiting_approval 累计时长 > 5min（用户长时间不批准）
	{
		var waitingApprovalEnter time.Time
		var totalWaiting time.Duration
		for _, ev := range events {
			if ev.Kind != KindAgentStateChanged || ev.Transition == nil {
				continue
			}
			if ev.Transition.NewState == "waiting_approval" {
				waitingApprovalEnter = ev.Timestamp
			}
			if ev.Transition.PrevState == "waiting_approval" && !waitingApprovalEnter.IsZero() {
				totalWaiting += ev.Timestamp.Sub(waitingApprovalEnter)
				waitingApprovalEnter = time.Time{}
			}
		}
		if totalWaiting > 5*time.Minute {
			anomalies = append(anomalies, fmt.Sprintf(
				"WARNING agent 累计在 waiting_approval 状态 %s（用户长时间未批准？）",
				formatDuration(totalWaiting)))
		}
	}

	// 7. 检测：shell timeout 总数异常（同 task 内 KindShellTimeoutPending 数量 > 3）
	{
		timeoutCount := 0
		for _, ev := range events {
			if ev.Kind == KindShellTimeoutPending {
				timeoutCount++
			}
		}
		if timeoutCount > 3 {
			anomalies = append(anomalies, fmt.Sprintf(
				"WARNING 同 task 内出现 %d 次 shell timeout（命令选择或 timeout 阈值可能不合理）",
				timeoutCount))
		}
	}

	// 8. 检测：task_failed 且 cause=panic（区别于业务级失败）
	for _, ev := range events {
		if ev.Kind == KindTaskFailed && ev.Transition != nil &&
			strings.HasPrefix(ev.Transition.Cause, "react_loop_exit:panic") {
			anomalies = append(anomalies, fmt.Sprintf(
				"ERROR task 因 panic 失败：%s（程序错误而非业务错误，需查 panic 堆栈）",
				truncate(ev.Reason, 120)))
		}
	}

	// 9. 检测：cancel_source=watchdog 出现（兜底取消应该罕见）
	{
		watchdogCancels := 0
		for _, ev := range events {
			if ev.Kind == KindTaskCancelled && ev.Transition != nil &&
				ev.Transition.CancelSource == "watchdog" {
				watchdogCancels++
			}
		}
		if watchdogCancels > 0 {
			anomalies = append(anomalies, fmt.Sprintf(
				"WARNING watchdog 兜底取消 %d 次（主流程可能存在卡死或泄漏）",
				watchdogCancels))
		}
	}

	return anomalies
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func formatDuration(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
