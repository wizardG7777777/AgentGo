package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 延迟阈值：相邻事件间隔超过此值时在 show 输出中标记 WARNING。
const slowGapThreshold = 30 * time.Second

// CLI 实现 `agentgo trace list/show/plan` 子命令的入口。
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
	case "plan":
		if len(args) < 2 {
			return fmt.Errorf("usage: agentgo trace plan <plan_id>")
		}
		return cmdPlan(dir, args[1], out)
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
                        task_id 可以是完整 UUID 或任意唯一前缀
  plan <plan_id>        聚合展示一个动态 DAG Plan 的跨任务事件时间线
                        plan_id 可以是完整 UUID 或唯一前缀

示例:
  agentgo trace list
  agentgo trace show 321b561d
  agentgo trace show 321b561d-c564-422c-bfa0-b96f54edcb87
  agentgo trace plan 321b561d

实时查看最新任务的事件流（用 tail -f 即可）:
  tail -f .agentgo/sessions/sess-<id>/logs/<时间戳>_<task_id前8位>.jsonl | jq

trace 文件位置:
  Session 活跃时: .agentgo/sessions/sess-<id>/logs/
  无 Session 时:  .agentgo/traces/
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

// readAllEvents 读取一个 trace 文件的所有事件。即使底层读取在中途失败，
// 也返回已经完整读到的事件，让 CLI 能展示 partial timeline，而不是丢掉全部证据。
func readAllEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	events, err := readEventStream(f)
	if err != nil {
		return events, fmt.Errorf("读取 trace 文件失败: %w", err)
	}
	return events, nil
}

// readEventStream 按 JSONL 行读取事件。bufio.Reader 不设置行长上限，因此 args 或
// tool result 即使超过旧 Scanner 的 4 MiB 限制也能完整解析。
func readEventStream(r io.Reader) ([]Event, error) {
	var events []Event
	reader := bufio.NewReader(r)
	var lastKnownTimestamp time.Time
	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var ev Event
			if err := json.Unmarshal(trimmed, &ev); err != nil {
				// 单行解析失败时降级为占位事件，便于排查。TaskID 为空，
				// 后续按物理文件内真实 TaskID 数量决定是否可安全归属。
				// 继承上一条已知时间，使文件中段坏行在稳定排序后仍紧随
				// 它前面的物理行；首行损坏时保持零时间。
				events = append(events, Event{
					Timestamp: lastKnownTimestamp,
					Kind:      "<parse_error>",
					Error:     fmt.Sprintf("invalid JSON: %v (line: %s)", err, truncate(string(trimmed), 100)),
				})
			} else {
				events = append(events, ev)
				if !ev.Timestamp.IsZero() {
					lastKnownTimestamp = ev.Timestamp
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return events, nil
			}
			return events, readErr
		}
	}
}

// traceEventRecord 为事件附带其物理来源，使跨文件聚合后的排序可严格按
// ts + filename + line 稳定复现。
type traceEventRecord struct {
	event Event
	file  taskFile
	line  int
}

type traceIssue struct {
	file        taskFile
	message     string
	errorCount  int
	readFailure bool
}

type loadedTraceFile struct {
	file    taskFile
	events  []Event
	readErr error
}

// taskTrace 是 CLI 的聚合单位。真实任务严格以完整 Event.TaskID 为 key；
// 完全没有 TaskID 的物理文件则各自形成 synthetic group，绝不借文件名的
// 8 位前缀跨文件合并。
type taskTrace struct {
	key         string
	taskID      string
	syntheticID string
	publishedAt time.Time
	files       map[string]taskFile
	records     []traceEventRecord
	issues      []traceIssue
}

func (t *taskTrace) displayID() string {
	if t.taskID != "" {
		return t.taskID
	}
	return t.syntheticID
}

func loadTraceFiles(files []taskFile) []loadedTraceFile {
	loaded := make([]loadedTraceFile, 0, len(files))
	for _, file := range files {
		events, err := readAllEvents(file.path)
		loaded = append(loaded, loadedTraceFile{file: file, events: events, readErr: err})
	}
	return loaded
}

func groupTraceFiles(loaded []loadedTraceFile) []*taskTrace {
	groups := make(map[string]*taskTrace)
	getRealGroup := func(taskID string) *taskTrace {
		key := "task:" + taskID
		if group := groups[key]; group != nil {
			return group
		}
		group := &taskTrace{key: key, taskID: taskID, files: make(map[string]taskFile)}
		groups[key] = group
		return group
	}
	getSyntheticGroup := func(file taskFile) *taskTrace {
		key := "file:" + file.path
		if group := groups[key]; group != nil {
			return group
		}
		group := &taskTrace{
			key: key, syntheticID: syntheticTraceID(file), files: make(map[string]taskFile),
		}
		groups[key] = group
		return group
	}
	addFile := func(group *taskTrace, file taskFile) {
		group.files[file.path] = file
		if group.publishedAt.IsZero() || file.publishedAt.Before(group.publishedAt) {
			group.publishedAt = file.publishedAt
		}
	}

	for _, item := range loaded {
		taskIDs := make(map[string]struct{})
		for _, ev := range item.events {
			if ev.TaskID != "" {
				taskIDs[ev.TaskID] = struct{}{}
			}
		}
		ids := make([]string, 0, len(taskIDs))
		for taskID := range taskIDs {
			ids = append(ids, taskID)
		}
		sort.Strings(ids)

		if len(ids) == 0 {
			group := getSyntheticGroup(item.file)
			addFile(group, item.file)
			countedEventErrors := 0
			for line, ev := range item.events {
				group.records = append(group.records, traceEventRecord{
					event: ev, file: item.file, line: line + 1,
				})
				if ev.Kind == "<parse_error>" || ev.Kind == KindError {
					countedEventErrors++
				}
			}
			if len(item.events) > 0 {
				group.issues = append(group.issues, traceIssue{
					file:       item.file,
					message:    fmt.Sprintf("%d 个事件均缺少 task_id；已保留为独立 synthetic file group", len(item.events)),
					errorCount: len(item.events) - countedEventErrors,
				})
			} else if item.readErr == nil {
				group.issues = append(group.issues, traceIssue{
					file: item.file, message: "空 trace 文件；没有可恢复的事件", errorCount: 1,
				})
			}
			if item.readErr != nil {
				group.issues = append(group.issues, traceIssue{
					file: item.file, message: item.readErr.Error(), errorCount: 1, readFailure: true,
				})
			}
			continue
		}

		for _, taskID := range ids {
			addFile(getRealGroup(taskID), item.file)
		}
		emptyCount := 0
		countedEventErrors := 0
		for line, ev := range item.events {
			record := traceEventRecord{event: ev, file: item.file, line: line + 1}
			if ev.TaskID != "" {
				getRealGroup(ev.TaskID).records = append(getRealGroup(ev.TaskID).records, record)
				continue
			}
			emptyCount++
			if ev.Kind == "<parse_error>" || ev.Kind == KindError {
				countedEventErrors++
			}
			if len(ids) == 1 {
				getRealGroup(ids[0]).records = append(getRealGroup(ids[0]).records, record)
			}
		}

		if emptyCount > 0 {
			if len(ids) == 1 {
				group := getRealGroup(ids[0])
				group.issues = append(group.issues, traceIssue{
					file:       item.file,
					message:    fmt.Sprintf("%d 个无 task_id 事件按该文件唯一 TaskID 归属", emptyCount),
					errorCount: emptyCount - countedEventErrors,
				})
			} else {
				for _, taskID := range ids {
					group := getRealGroup(taskID)
					group.issues = append(group.issues, traceIssue{
						file:       item.file,
						message:    fmt.Sprintf("物理文件包含 %d 个 TaskID；%d 个无 task_id 事件无法安全归属，未纳入 timeline", len(ids), emptyCount),
						errorCount: emptyCount,
					})
				}
			}
		}
		if item.readErr != nil {
			for _, taskID := range ids {
				group := getRealGroup(taskID)
				group.issues = append(group.issues, traceIssue{
					file: item.file, message: item.readErr.Error(), errorCount: 1, readFailure: true,
				})
			}
		}
	}

	result := make([]*taskTrace, 0, len(groups))
	for _, group := range groups {
		sortTraceRecords(group.records)
		result = append(result, group)
	}
	assignUniqueSyntheticIDs(result)
	sort.Slice(result, func(i, j int) bool {
		if result[i].publishedAt.Equal(result[j].publishedAt) {
			return result[i].displayID() < result[j].displayID()
		}
		return result[i].publishedAt.After(result[j].publishedAt)
	})
	return result
}

func sortTraceRecords(records []traceEventRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].event.Timestamp.Equal(records[j].event.Timestamp) {
			return records[i].event.Timestamp.Before(records[j].event.Timestamp)
		}
		if records[i].file.filename != records[j].file.filename {
			return records[i].file.filename < records[j].file.filename
		}
		return records[i].line < records[j].line
	})
}

func syntheticTraceID(file taskFile) string {
	return syntheticTraceIDFromSeed(file.path)
}

func syntheticTraceIDFromSeed(seed string) string {
	h := fnv.New32a()
	_, _ = io.WriteString(h, seed)
	return fmt.Sprintf("file-%08x", h.Sum32())
}

func assignUniqueSyntheticIDs(groups []*taskTrace) {
	used := make(map[string]struct{}, len(groups))
	var synthetic []*taskTrace
	for _, group := range groups {
		if group.taskID != "" {
			used[group.taskID] = struct{}{}
			continue
		}
		synthetic = append(synthetic, group)
	}
	sort.Slice(synthetic, func(i, j int) bool { return synthetic[i].key < synthetic[j].key })
	for _, group := range synthetic {
		candidate := group.syntheticID
		for salt := 1; ; salt++ {
			if _, exists := used[candidate]; !exists {
				break
			}
			candidate = syntheticTraceIDFromSeed(fmt.Sprintf("%s#%d", group.key, salt))
		}
		group.syntheticID = candidate
		used[candidate] = struct{}{}
	}
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

	groups := groupTraceFiles(loadTraceFiles(files))

	// 表头
	fmt.Fprintln(out, "┌───────────────┬──────────┬─────────────────────┬──────────┬────────────┬───────┬───────────┬────────┬─────────────┐")
	fmt.Fprintln(out, "│ Task          │ Plan     │ Published           │ Agent    │ Status     │ Loops │ Files Out │ Errors │ Duration    │")
	fmt.Fprintln(out, "├───────────────┼──────────┼─────────────────────┼──────────┼────────────┼───────┼───────────┼────────┼─────────────┤")

	for _, group := range groups {
		row := summarizeTask(group)
		fmt.Fprintf(out, "│ %-13s │ %-8s │ %-19s │ %-8s │ %-10s │ %5d │ %9d │ %6d │ %-11s │\n",
			row.taskShortID,
			shortIdentifier(row.planID),
			row.publishedAt.Local().Format("2006-01-02 15:04:05"),
			fitColumn(row.agentID, 8),
			row.status,
			row.loops,
			row.filesWritten,
			row.errors,
			formatDuration(row.duration),
		)
	}
	fmt.Fprintln(out, "└───────────────┴──────────┴─────────────────────┴──────────┴────────────┴───────┴───────────┴────────┴─────────────┘")
	fmt.Fprintf(out, "\n共 %d 个任务，trace 目录: %s\n", len(groups), dir)
	return nil
}

// taskSummary 是 list 命令一行的汇总信息。
type taskSummary struct {
	taskShortID  string
	planID       string
	publishedAt  time.Time
	agentID      string
	status       string // 对齐 model.TaskStatus：pending / processing / pending(retry) / completed / failed / cancelled；诊断值：malformed / unknown / read_err
	loops        int
	filesWritten int
	errors       int
	duration     time.Duration
}

func summarize(f taskFile) taskSummary {
	loaded := loadTraceFiles([]taskFile{f})
	groups := groupTraceFiles(loaded)
	if len(groups) == 0 {
		return taskSummary{taskShortID: f.taskShortID, publishedAt: f.publishedAt, status: "read_err", errors: 1}
	}
	return summarizeTask(groups[0])
}

func summarizeTask(group *taskTrace) taskSummary {
	taskLabel := shortIdentifier(group.displayID())
	if group.taskID == "" {
		taskLabel = group.displayID()
	}
	row := taskSummary{
		taskShortID: taskLabel,
		publishedAt: group.publishedAt,
		status:      "unknown",
	}
	var firstTS, lastTS time.Time
	llmStarts := 0
	legacyLoops := 0
	fallbackAgentID := ""
	for _, record := range group.records {
		ev := record.event
		if ev.LoopsUsed > legacyLoops {
			legacyLoops = ev.LoopsUsed
		}
		if ev.AgentID != "" {
			fallbackAgentID = ev.AgentID
		}
		if ev.Plan != nil && ev.Plan.PlanID != "" {
			row.planID = ev.Plan.PlanID
		}
		if !ev.Timestamp.IsZero() {
			if firstTS.IsZero() || ev.Timestamp.Before(firstTS) {
				firstTS = ev.Timestamp
			}
			if lastTS.IsZero() || ev.Timestamp.After(lastTS) {
				lastTS = ev.Timestamp
			}
		}
		switch ev.Kind {
		case KindTaskClaimed:
			row.status = "processing"
			if ev.AgentID != "" {
				row.agentID = ev.AgentID
			}
		case KindTaskPublished:
			if row.status == "unknown" {
				row.status = "pending"
			}
		case KindTaskCompleted:
			row.status = "completed"
			if ev.AgentID != "" {
				row.agentID = ev.AgentID
			}
		case KindTaskFailed:
			row.status = "failed"
			if ev.AgentID != "" {
				row.agentID = ev.AgentID
			}
		case KindTaskBlocked:
			row.status = "blocked"
			if ev.AgentID != "" {
				row.agentID = ev.AgentID
			}
		case KindTaskCancelled:
			row.status = "cancelled"
			if ev.AgentID != "" {
				row.agentID = ev.AgentID
			}
		case KindTaskRetry:
			// retry 事件对应 processing→pending 回滚（model 词表），
			// 等待重新认领期间标注为 pending(retry)。
			row.status = "pending(retry)"
		case KindLLMCallStart:
			// 聚合 retry 分片后，Loop 可能重新从 0 开始；因此计数实际
			// start 事件，而不是取 max(loop)+1。transfer-note 的 -1 不计。
			if ev.Loop >= 0 {
				llmStarts++
			}
		case KindFileWritten:
			row.filesWritten++
		case KindError:
			// KindError 是非终态诊断事件，不能覆盖 Task 生命周期状态。
			row.errors++
		case "<parse_error>":
			row.errors++
			if !isTerminalSummaryStatus(row.status) {
				row.status = "malformed"
			}
		}
	}
	if llmStarts > 0 {
		row.loops = llmStarts
	} else {
		row.loops = legacyLoops
	}
	hasReadFailure := false
	for _, issue := range group.issues {
		row.errors += issue.errorCount
		hasReadFailure = hasReadFailure || issue.readFailure
	}
	if row.agentID == "" {
		row.agentID = fallbackAgentID
	}
	if len(group.issues) > 0 && (group.taskID == "" || !isTerminalSummaryStatus(row.status)) {
		if hasReadFailure && len(group.records) == 0 {
			row.status = "read_err"
		} else {
			row.status = "malformed"
		}
	}
	if !firstTS.IsZero() && !lastTS.IsZero() {
		row.duration = lastTS.Sub(firstTS)
	}
	return row
}

func isTerminalSummaryStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "blocked" || status == "cancelled"
}

// cmdShow 实现 agentgo trace show <task_id>。
func cmdShow(dir, taskIDQuery string, out io.Writer) error {
	files, err := listTaskFiles(dir)
	if err != nil {
		return err
	}
	groups := groupTraceFiles(loadTraceFiles(files))
	matches, err := matchTaskGroups(groups, strings.TrimSpace(taskIDQuery))
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("未找到匹配 task_id=%s 的 trace 任务", taskIDQuery)
	}
	if len(matches) > 1 {
		fmt.Fprintf(out, "找到 %d 个匹配的任务，请使用更长的 task_id 区分:\n", len(matches))
		for _, group := range matches {
			fmt.Fprintf(out, "  %s  %s  trace_files=%d\n", group.displayID(),
				group.publishedAt.Local().Format("2006-01-02 15:04:05"), len(group.files))
		}
		return nil
	}

	group := matches[0]
	events := eventsFromRecords(group.records)

	// 头部信息
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	fmt.Fprintf(out, " Task: %s\n", group.displayID())
	fmt.Fprintf(out, " Trace Files: %d\n", len(group.files))
	fmt.Fprintf(out, " Events: %d\n", len(events))
	if planCtx := latestPlanContext(events, ""); planCtx != nil {
		fmt.Fprintf(out, " Plan: %s  revision=%d  state_version=%d  acceptance_revision=%d\n",
			planCtx.PlanID, planCtx.PlanRevision, planCtx.ExecutionStateVersion, planCtx.AcceptanceSpecRevision)
		if planCtx.GraphDigest != "" {
			fmt.Fprintf(out, " Graph Digest: %s\n", planCtx.GraphDigest)
		}
	}
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	printTimelineIssues(out, group.issues)

	// 按时间顺序打印事件，相邻事件超过阈值标 WARNING
	var prev time.Time
	for i, record := range group.records {
		ev := record.event
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
		if eventCarriesLoop(ev.Kind) && ev.Loop >= 0 {
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
	stats := summarizeTask(group)
	fmt.Fprintf(out, " status=%s  agent=%s  loops=%d  files_written=%d  errors=%d  duration=%s\n",
		stats.status, stats.agentID, stats.loops, stats.filesWritten, stats.errors, formatDuration(stats.duration))

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

func matchTaskGroups(groups []*taskTrace, query string) ([]*taskTrace, error) {
	if query == "" {
		return nil, fmt.Errorf("task_id 不能为空")
	}
	var matches []*taskTrace
	for _, group := range groups {
		if strings.HasPrefix(group.displayID(), query) {
			matches = append(matches, group)
		}
	}
	return matches, nil
}

func eventsFromRecords(records []traceEventRecord) []Event {
	events := make([]Event, 0, len(records))
	for _, record := range records {
		events = append(events, record.event)
	}
	return events
}

func printTimelineIssues(out io.Writer, issues []traceIssue) {
	issues = uniqueTraceIssues(issues)
	if len(issues) == 0 {
		return
	}
	fmt.Fprintf(out, " WARNING: timeline incomplete (%d issue(s))\n", len(issues))
	for _, issue := range issues {
		fmt.Fprintf(out, "   - %s: %s\n", issue.file.filename, issue.message)
	}
}

func uniqueTraceIssues(issues []traceIssue) []traceIssue {
	seen := make(map[string]struct{})
	unique := make([]traceIssue, 0, len(issues))
	for _, issue := range issues {
		key := issue.file.path + "\x00" + issue.message
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, issue)
	}
	sort.Slice(unique, func(i, j int) bool {
		if unique[i].file.filename == unique[j].file.filename {
			return unique[i].message < unique[j].message
		}
		return unique[i].file.filename < unique[j].file.filename
	})
	return unique
}

type planTimelineEvent struct {
	record traceEventRecord
	taskID string
}

// cmdPlan 把同一 Plan 下分散在 controller、runner 与 acceptance Task
// 的事件聚合成一条稳定时间线。成员身份来自任一 Plan context；一旦某个
// Task 成为成员，就纳入该完整 Task 的全部 trace 分片，包括没有 Plan payload
// 的 retry 文件。PlanID 对应的根 Task 也始终纳入。
func cmdPlan(dir, planIDQuery string, out io.Writer) error {
	query := strings.TrimSpace(planIDQuery)
	if query == "" {
		return fmt.Errorf("plan_id 不能为空")
	}
	files, err := listTaskFiles(dir)
	if err != nil {
		return err
	}
	groups := groupTraceFiles(loadTraceFiles(files))
	planIDs := make(map[string]struct{})
	for _, group := range groups {
		for _, record := range group.records {
			ev := record.event
			if ev.Plan != nil && ev.Plan.PlanID != "" {
				planIDs[ev.Plan.PlanID] = struct{}{}
			}
		}
	}

	planID, candidates := resolvePlanID(planIDs, query)
	if len(candidates) == 0 {
		return fmt.Errorf("未找到匹配 plan_id=%s 的 trace 事件", planIDQuery)
	}
	if len(candidates) > 1 {
		fmt.Fprintf(out, "找到 %d 个匹配的 Plan，请使用更长的 plan_id 区分:\n", len(candidates))
		for _, candidate := range candidates {
			fmt.Fprintf(out, "  %s\n", candidate)
		}
		return nil
	}

	var timeline []planTimelineEvent
	planTasks := make(map[string]struct{})
	planFiles := make(map[string]struct{})
	var issues []traceIssue
	for _, group := range groups {
		if !taskTraceBelongsToPlan(group, planID) {
			continue
		}
		if group.taskID != "" {
			planTasks[group.taskID] = struct{}{}
		}
		for path := range group.files {
			planFiles[path] = struct{}{}
		}
		issues = append(issues, group.issues...)
		for _, record := range group.records {
			timeline = append(timeline, planTimelineEvent{
				record: record, taskID: group.displayID(),
			})
		}
	}
	sort.SliceStable(timeline, func(i, j int) bool {
		left, right := timeline[i].record, timeline[j].record
		if !left.event.Timestamp.Equal(right.event.Timestamp) {
			return left.event.Timestamp.Before(right.event.Timestamp)
		}
		if left.file.filename != right.file.filename {
			return left.file.filename < right.file.filename
		}
		return left.line < right.line
	})

	allEvents := make([]Event, 0, len(timeline))
	for _, item := range timeline {
		allEvents = append(allEvents, item.record.event)
	}
	planCtx := latestPlanContext(allEvents, planID)
	acceptance := latestAcceptanceContext(timeline, planID)

	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	fmt.Fprintf(out, " Plan: %s\n", planID)
	fmt.Fprintf(out, " Tasks: %d\n", len(planTasks))
	fmt.Fprintf(out, " Trace Files: %d\n", len(planFiles))
	fmt.Fprintf(out, " Events: %d\n", len(timeline))
	if planCtx != nil {
		fmt.Fprintf(out, " Revision: %d  State Version: %d  Acceptance Revision: %d\n",
			planCtx.PlanRevision, planCtx.ExecutionStateVersion, planCtx.AcceptanceSpecRevision)
		if planCtx.GraphDigest != "" {
			fmt.Fprintf(out, " Graph Digest: %s\n", planCtx.GraphDigest)
		}
	}
	if acceptance != nil {
		fmt.Fprintf(out, " Latest Acceptance: status=%s verdict=%s run=%s result=%s\n",
			acceptance.Status, acceptance.Verdict, acceptance.AcceptanceRunID, acceptance.ResultID)
	}
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	printTimelineIssues(out, issues)

	var prev time.Time
	for i, item := range timeline {
		ev := item.record.event
		if i > 0 && !prev.IsZero() && ev.Timestamp.Sub(prev) > slowGapThreshold {
			fmt.Fprintf(out, "  WARNING: Plan 距离上一条事件间隔 %s（超过 %s 阈值）\n",
				formatDuration(ev.Timestamp.Sub(prev)), formatDuration(slowGapThreshold))
		}
		prev = ev.Timestamp
		fmt.Fprintf(out, "[%s] task=%s %-30s", ev.Timestamp.Local().Format("15:04:05.000"),
			item.taskID, ev.Kind)
		if ev.AgentID != "" {
			fmt.Fprintf(out, " agent=%s", ev.AgentID)
		}
		if eventCarriesLoop(ev.Kind) && ev.Loop >= 0 {
			fmt.Fprintf(out, " loop=%d", ev.Loop)
		}
		fmt.Fprintln(out)
		if details := formatEventDetails(ev); details != "" {
			fmt.Fprintf(out, "             %s\n", details)
		}
	}
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	return nil
}

func resolvePlanID(planIDs map[string]struct{}, query string) (string, []string) {
	if _, ok := planIDs[query]; ok {
		return query, []string{query}
	}
	var candidates []string
	for planID := range planIDs {
		if strings.HasPrefix(planID, query) {
			candidates = append(candidates, planID)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 1 {
		return candidates[0], candidates
	}
	return "", candidates
}

func taskTraceBelongsToPlan(group *taskTrace, planID string) bool {
	if group.taskID == planID {
		return true
	}
	for _, record := range group.records {
		ev := record.event
		if ev.Plan != nil && ev.Plan.PlanID == planID {
			return true
		}
	}
	return false
}

func latestPlanContext(events []Event, planID string) *PlanTraceContext {
	targetPlanID := planID
	if targetPlanID == "" {
		// show 视图通常只有一个 Plan；若历史数据混入多个，维持旧行为，
		// 选择时间线上最后出现的 Plan 身份，再只汇总该 Plan。
		for _, ev := range events {
			if ev.Plan != nil && ev.Plan.PlanID != "" {
				targetPlanID = ev.Plan.PlanID
			}
		}
	}
	if targetPlanID == "" {
		return nil
	}
	latest := &PlanTraceContext{PlanID: targetPlanID}
	found := false
	var digest string
	digestRevision := int64(-1)
	for _, ev := range events {
		if ev.Plan == nil || ev.Plan.PlanID != targetPlanID {
			continue
		}
		found = true
		if ev.Plan.PlanRevision > latest.PlanRevision {
			latest.PlanRevision = ev.Plan.PlanRevision
		}
		if ev.Plan.ExecutionStateVersion > latest.ExecutionStateVersion {
			latest.ExecutionStateVersion = ev.Plan.ExecutionStateVersion
		}
		if ev.Plan.AcceptanceSpecRevision > latest.AcceptanceSpecRevision {
			latest.AcceptanceSpecRevision = ev.Plan.AcceptanceSpecRevision
		}
		if ev.Plan.GraphDigest != "" && ev.Plan.PlanRevision >= digestRevision {
			digest = ev.Plan.GraphDigest
			digestRevision = ev.Plan.PlanRevision
		}
	}
	if !found {
		return nil
	}
	// Digest 描述具体图 revision。只有在最高已知 revision 上观测到 digest
	// 才展示，避免把旧图 digest 错配到新 revision；只带 state 的 partial
	// context 不会清掉同 revision 已知 digest。
	if digestRevision == latest.PlanRevision {
		latest.GraphDigest = digest
	}
	return latest
}

func latestAcceptanceContext(timeline []planTimelineEvent, planID string) *AcceptanceTraceContext {
	var latest *AcceptanceTraceContext
	for _, item := range timeline {
		ev := item.record.event
		if ev.Acceptance == nil || ev.Plan == nil || ev.Plan.PlanID != planID {
			continue
		}
		copyCtx := *ev.Acceptance
		latest = &copyCtx
	}
	return latest
}

// formatEventDetails 把事件的可选字段格式化为单行可读文本。
func formatEventDetails(ev Event) string {
	var parts []string
	switch ev.Kind {
	case KindTaskPublished:
		if ev.PublishedBy != "" {
			parts = append(parts, fmt.Sprintf("by=%s", ev.PublishedBy))
		}
		if ev.ParentTaskID != "" {
			parts = append(parts, fmt.Sprintf("parent=%s", ev.ParentTaskID))
		}
		if ev.BatchID != "" {
			parts = append(parts, fmt.Sprintf("batch=%s", ev.BatchID))
		}
		if len(ev.Dependencies) > 0 {
			parts = append(parts, fmt.Sprintf("deps=%v", ev.Dependencies))
		} else {
			parts = append(parts, "deps=[]")
		}
		if ev.EventType != "" {
			parts = append(parts, fmt.Sprintf("type=%s", ev.EventType))
		}
		if ev.Priority != "" {
			parts = append(parts, fmt.Sprintf("priority=%s", ev.Priority))
		}
		parts = append(parts, fmt.Sprintf("depth=%d", ev.Depth))
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 80)))
		}
	case KindTaskClaimed:
		parts = appendTaskTransition(parts, ev.Transition, false, false)
	case KindTaskSubmitted, KindTextOnlySubmission:
		parts = append(parts, fmt.Sprintf("output_len=%d loops_used=%d", ev.OutputLen, ev.LoopsUsed))
	case KindTaskCompleted:
		parts = appendTaskTransition(parts, ev.Transition, false, false)
		parts = append(parts, fmt.Sprintf("output_len=%d", ev.OutputLen))
		if ev.LoopsUsed > 0 {
			parts = append(parts, fmt.Sprintf("loops_used=%d", ev.LoopsUsed))
		}
	case KindTaskFailed, KindTaskBlocked:
		parts = appendTaskTransition(parts, ev.Transition, true, false)
		parts = appendReason(parts, "reason", ev.Reason)
	case KindTaskCancelled:
		parts = appendTaskTransition(parts, ev.Transition, false, true)
		parts = appendReason(parts, "reason", ev.Reason)
	case KindTaskRetry:
		parts = appendTaskTransition(parts, ev.Transition, true, false)
		if ev.AttemptNo > 0 {
			parts = append(parts, fmt.Sprintf("attempt=%d", ev.AttemptNo))
		}
		parts = appendReason(parts, "reason", ev.Reason)
	case KindLLMCallStart:
		parts = append(parts, fmt.Sprintf("history_entries=%d tools=%d", ev.HistoryEntries, ev.ToolCallsCount))
	case KindLLMCallEnd:
		parts = append(parts, fmt.Sprintf("duration=%dms", ev.DurationMS))
		parts = append(parts, fmt.Sprintf("prompt_tokens=%d completion_tokens=%d tool_calls=%d",
			ev.PromptTokens, ev.CompletionTokens, ev.ToolCallsCount))
		if ev.FinishReason != "" {
			parts = append(parts, fmt.Sprintf("finish_reason=%s", ev.FinishReason))
		}
		if ev.Error != "" {
			parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 80)))
		}
	case KindToolCall:
		parts = append(parts, fmt.Sprintf("tool=%s", ev.Tool))
		if ev.CallID != "" {
			parts = append(parts, fmt.Sprintf("call_id=%s", ev.CallID))
		}
		parts = appendArgs(parts, ev.Args)
	case KindToolResult:
		parts = append(parts, fmt.Sprintf("tool=%s duration=%dms", ev.Tool, ev.DurationMS))
		if ev.CallID != "" {
			parts = append(parts, fmt.Sprintf("call_id=%s", ev.CallID))
		}
		parts = appendArgs(parts, ev.Args)
		if ev.Error != "" {
			parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 100)))
		} else {
			parts = append(parts, fmt.Sprintf("result_len=%d", ev.ResultLen))
		}
	case KindFileWritten:
		parts = append(parts, fmt.Sprintf("path=%s bytes=%d hash=%s", ev.Path, ev.Bytes, ev.Hash))
		if ev.Tool != "" {
			parts = append(parts, fmt.Sprintf("tool=%s", ev.Tool))
		}
	case KindFileWriteQueued:
		parts = append(parts, fmt.Sprintf("path=%s", ev.Path))
		if ev.QueueLen > 0 {
			parts = append(parts, fmt.Sprintf("queue_len=%d", ev.QueueLen))
		}
		if ev.WaitMS > 0 {
			parts = append(parts, fmt.Sprintf("wait_ms=%d", ev.WaitMS))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 120)))
		}
	case KindHistoryCompaction, KindHistoryTruncated:
		parts = append(parts, fmt.Sprintf("tokens_before=%d tokens_after=%d strategy=%s kept_entries=%d",
			ev.PromptTokensBefore, ev.PromptTokensAfter, ev.Strategy, ev.KeptEntries))
	case KindTokenStats:
		parts = append(parts, fmt.Sprintf(
			"call=%d prompt_tokens=%d completion_tokens=%d total_prompt_tokens=%d total_completion_tokens=%d",
			ev.CallCount, ev.PromptTokens, ev.CompletionTokens,
			ev.TotalPromptTokens, ev.TotalCompletionTokens))
	case KindProgressNotify:
		parts = append(parts, fmt.Sprintf("notify_type=%s", ev.NotifyType))
	case KindError:
		parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 200)))
		parts = appendReason(parts, "reason", ev.Reason)
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
			parts = appendExcerpt(parts, "stdout", ev.ShellExec.StdoutExcerpt)
			parts = appendExcerpt(parts, "stderr", ev.ShellExec.StderrExcerpt)
		}
		if ev.Tool != "" {
			parts = append(parts, fmt.Sprintf("tool=%s", ev.Tool))
		}
		parts = appendArgs(parts, ev.Args)
	case KindShellTimeoutPending:
		if ev.ShellTimeout != nil {
			parts = append(parts, fmt.Sprintf("cmd=%q elapsed=%ds waits=%d",
				truncate(ev.ShellTimeout.Command, 60),
				ev.ShellTimeout.ElapsedSec,
				ev.ShellTimeout.PreviousWaits))
			parts = appendExcerpt(parts, "stdout", ev.ShellTimeout.StdoutExcerpt)
			parts = appendExcerpt(parts, "stderr", ev.ShellTimeout.StderrExcerpt)
		}
	case KindShellTimeoutResolved:
		if ev.ShellTimeout != nil {
			parts = append(parts, fmt.Sprintf("cmd=%q elapsed=%ds waits=%d decision=%s",
				truncate(ev.ShellTimeout.Command, 60),
				ev.ShellTimeout.ElapsedSec,
				ev.ShellTimeout.PreviousWaits,
				ev.ShellTimeout.Decision))
			if ev.ShellTimeout.Decision == "wait" && ev.ShellTimeout.ExtraSeconds > 0 {
				parts = append(parts, fmt.Sprintf("extra=%ds", ev.ShellTimeout.ExtraSeconds))
			}
		}
	case KindReactorSpawnDepthExceeded:
		parts = append(parts, fmt.Sprintf("depth=%d", ev.Depth))
		parts = appendReason(parts, "reason", ev.Reason)
	case KindReplanRequested, KindReplanCoalesced, KindReplanDecided,
		KindPlanRevisionChanged, KindPlanPaused, KindPlanTerminal:
		parts = appendReason(parts, "reason", ev.Reason)
	case KindAcceptanceCompleted:
		parts = appendReason(parts, "reason", ev.Reason)
	default:
		// 用户事件使用 user.<name> 命名空间，payload 的 Description 是
		// 主要可读内容。未知未来事件也至少展示已有通用字段，避免静默空白。
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 200)))
		}
		if ev.Error != "" {
			parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 200)))
		}
		parts = appendReason(parts, "reason", ev.Reason)
	}
	parts = appendPlanContext(parts, ev.Plan)
	parts = appendAcceptanceContext(parts, ev.Acceptance)
	return strings.Join(parts, " ")
}

func appendTaskTransition(parts []string, transition *Transition, includeRetry, includeCancelSource bool) []string {
	if transition == nil {
		return parts
	}
	parts = append(parts, fmt.Sprintf("prev=%s new=%s", transition.PrevStatus, transition.NewStatus))
	if includeRetry {
		parts = append(parts, fmt.Sprintf("retry=%d", transition.RetryCount))
	}
	if includeCancelSource && transition.CancelSource != "" {
		parts = append(parts, fmt.Sprintf("source=%s", transition.CancelSource))
	}
	if transition.Cause != "" {
		parts = append(parts, fmt.Sprintf("cause=%s", transition.Cause))
	}
	return parts
}

func appendArgs(parts []string, args map[string]any) []string {
	if len(args) == 0 {
		return parts
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return append(parts, fmt.Sprintf("args_error=%q", err.Error()))
	}
	return append(parts, fmt.Sprintf("args=%s", truncate(string(argsJSON), 200)))
}

func appendReason(parts []string, key, reason string) []string {
	if reason == "" {
		return parts
	}
	return append(parts, fmt.Sprintf("%s=%q", key, truncate(reason, 200)))
}

func appendExcerpt(parts []string, key, excerpt string) []string {
	if excerpt == "" {
		return parts
	}
	return append(parts, fmt.Sprintf("%s=%q", key, truncate(excerpt, 160)))
}

func appendPlanContext(parts []string, plan *PlanTraceContext) []string {
	if plan == nil {
		return parts
	}
	parts = append(parts, fmt.Sprintf(
		"plan=%s plan_revision=%d state_version=%d acceptance_revision=%d",
		plan.PlanID, plan.PlanRevision, plan.ExecutionStateVersion, plan.AcceptanceSpecRevision))
	if plan.GraphDigest != "" {
		parts = append(parts, fmt.Sprintf("graph_digest=%s", plan.GraphDigest))
	}
	return parts
}

func appendAcceptanceContext(parts []string, acceptance *AcceptanceTraceContext) []string {
	if acceptance == nil {
		return parts
	}
	parts = append(parts,
		fmt.Sprintf("acceptance_run=%s", acceptance.AcceptanceRunID),
		fmt.Sprintf("result=%s", acceptance.ResultID),
		fmt.Sprintf("spec=%s", acceptance.SpecID),
		fmt.Sprintf("spec_revision=%d", acceptance.SpecRevision),
		fmt.Sprintf("target_revision=%d", acceptance.TargetRevision),
		fmt.Sprintf("target_digest=%s", acceptance.TargetGraphDigest),
		fmt.Sprintf("runner_task=%s", acceptance.RunnerTaskID),
	)
	if acceptance.RunnerKind != "" {
		parts = append(parts, fmt.Sprintf("runner_kind=%s", acceptance.RunnerKind))
	}
	parts = append(parts,
		fmt.Sprintf("verdict=%s", acceptance.Verdict),
		fmt.Sprintf("status=%s", acceptance.Status),
	)
	return appendReason(parts, "acceptance_reason", acceptance.Reason)
}

func eventCarriesLoop(kind EventKind) bool {
	switch kind {
	case KindLLMCallStart, KindLLMCallEnd, KindToolCall, KindToolResult,
		KindHistoryCompaction, KindHistoryTruncated, KindTokenStats, KindProgressNotify,
		KindTaskCancelled:
		return true
	default:
		return false
	}
}

func shortIdentifier(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8]
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

	// 2. 检测：任务完成但全程无 file_written 事件。text_only_submission
	// 是明确、成功的纯文本交付，不能误判为 report-only 失败。
	hasComplete := false
	hasFileWritten := false
	hasTextOnlySubmission := false
	hasReadFile := false
	for _, ev := range events {
		switch ev.Kind {
		case KindTaskCompleted:
			hasComplete = true
		case KindFileWritten:
			hasFileWritten = true
		case KindTextOnlySubmission:
			hasTextOnlySubmission = true
		case KindToolCall:
			if ev.Tool == "read_file" {
				hasReadFile = true
			}
		}
	}
	if hasComplete && !hasFileWritten && !hasTextOnlySubmission {
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

	// 6. 检测：agent 在 waiting_interaction 累计时长 > 5min（用户长时间未响应）
	{
		var waitingInteractionEnter time.Time
		var totalWaiting time.Duration
		for _, ev := range events {
			if ev.Kind != KindAgentStateChanged || ev.Transition == nil {
				continue
			}
			if ev.Transition.NewState == "waiting_interaction" {
				waitingInteractionEnter = ev.Timestamp
			}
			if ev.Transition.PrevState == "waiting_interaction" && !waitingInteractionEnter.IsZero() {
				totalWaiting += ev.Timestamp.Sub(waitingInteractionEnter)
				waitingInteractionEnter = time.Time{}
			}
		}
		if totalWaiting > 5*time.Minute {
			anomalies = append(anomalies, fmt.Sprintf(
				"WARNING agent 累计在 waiting_interaction 状态 %s（用户长时间未响应？）",
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

func fitColumn(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
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
