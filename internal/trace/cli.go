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
	case "stats":
		groupBy := "task"
		if len(args) >= 2 {
			groupBy = args[1]
		}
		if groupBy != "task" && groupBy != "agent" && groupBy != "plan" {
			return fmt.Errorf("usage: agentgo trace stats [task|agent|plan]")
		}
		return cmdStats(dir, groupBy, out)
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
  stats [task|agent|plan]  聚合当前 trace 目录内全部任务的 LLM 调用与
                        token 消耗（默认按 task 分组，按总 token 降序）

示例:
  agentgo trace list
  agentgo trace show 321b561d
  agentgo trace show 321b561d-c564-422c-bfa0-b96f54edcb87
  agentgo trace plan 321b561d
  agentgo trace stats agent

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

// statsAgg 是 stats 命令一个聚合桶的累计数据。token 取自 llm_call_end 事件
// （每次 LLM 调用一条，载本轮消耗）；token_stats 事件是 per-agent 累计值，
// 若同时纳入会重复计数，因此不读。
// wasted 口径：终态为 cancelled / failed 的任务，其全部 LLM token 计为浪费
// （产出未被下游使用）。completed 任务中间 retry 的消耗无法精确切分，
// 经 retries 计数单列，不混入 wasted。
type statsAgg struct {
	tasks      map[string]struct{}
	calls      int
	prompt     int64
	completion int64
	retries    int
	wasted     int64
}

func (a *statsAgg) total() int64 { return a.prompt + a.completion }

// readFileStat 记录单 path 的 read_file 调用结构：full 为无 offset/limit 的
// 全文读取次数，pages 为分页读取（offset → 次数）。重读判定据此区分
// "重复读同一内容"（浪费）与"大文件顺序分页"（合法新内容）。
type readFileStat struct {
	full  int
	pages map[int]int
}

// reReads 返回该 path 的浪费性重读次数：重复全文读（full-1）+
// 相同 offset 的重复分页（每页 count-1）。新 offset 的分页不计——
// 那是对大文件的合法顺序阅读（2026-07-22 指标修正：此前按 path 计数
// 把 memory.go 这类 1700+ 行文件的顺序分页误报为重读，基线 72–87%
// 被显著高估）。
func (s *readFileStat) reReads() int {
	n := 0
	if s.full > 1 {
		n += s.full - 1
	}
	for _, count := range s.pages {
		if count > 1 {
			n += count - 1
		}
	}
	return n
}

// taskStat 是一个任务（含 retry 分片合并后）的 token 统计，是 stats 的
// 最小聚合单位；agent / plan 视图由它二次聚合，异常检测也基于它。
type taskStat struct {
	id     string
	agent  string
	status string
	planID string
	calls  int
	agg    statsAgg
	// reads 统计同任务内 read_file 按 path 的调用结构，供重读率异常检测。
	reads map[string]*readFileStat
}

// cmdStats 实现 agentgo trace stats [task|agent|plan]。
// 回答"这个 session 的 token 都烧在哪"：把目录内全部任务（含 retry 分片，
// 由 groupTraceFiles 按完整 TaskID 合并）的 llm_call_end 事件按维度聚合。
func cmdStats(dir, groupBy string, out io.Writer) error {
	files, err := listTaskFiles(dir)
	if err != nil {
		return err
	}
	groups := groupTraceFiles(loadTraceFiles(files))

	// 第一层：per-task 聚合。
	taskStats := make([]*taskStat, 0, len(groups))
	for _, g := range groups {
		summary := summarizeTask(g)
		ts := &taskStat{id: g.displayID(), agent: summary.agentID, status: summary.status, planID: summary.planID, reads: make(map[string]*readFileStat)}
		for _, record := range g.records {
			switch record.event.Kind {
			case KindLLMCallEnd:
				ts.agg.calls++
				ts.agg.prompt += int64(record.event.PromptTokens)
				ts.agg.completion += int64(record.event.CompletionTokens)
			case KindTaskRetry:
				ts.agg.retries++
			case KindToolCall:
				if record.event.Tool == "read_file" {
					path, _ := record.event.Args["path"].(string)
					rs := ts.reads[path]
					if rs == nil {
						rs = &readFileStat{pages: make(map[int]int)}
						ts.reads[path] = rs
					}
					_, hasOffset := record.event.Args["offset"]
					_, hasLimit := record.event.Args["limit"]
					if !hasOffset && !hasLimit {
						rs.full++
					} else {
						offset := 1
						if v, ok := record.event.Args["offset"].(float64); ok && v > 0 {
							offset = int(v)
						}
						rs.pages[offset]++
					}
				}
			}
		}
		if ts.status == "cancelled" || ts.status == "failed" {
			ts.agg.wasted = ts.agg.total()
		}
		taskStats = append(taskStats, ts)
	}

	// 第二层：按分组维度聚合。
	type statsRow struct {
		key    string
		agent  string // 仅 task 视图使用
		status string // 仅 task 视图使用
		agg    *statsAgg
	}
	buckets := make(map[string]*statsAgg)
	rowsByKey := make(map[string]*statsRow)
	for _, ts := range taskStats {
		key := ""
		switch groupBy {
		case "agent":
			key = ts.agent
			if key == "" {
				key = "(unknown)"
			}
		case "plan":
			key = ts.planID
			if key == "" {
				key = "(no-plan)"
			}
		default: // task
			key = ts.id
		}
		bucket := buckets[key]
		if bucket == nil {
			bucket = &statsAgg{tasks: make(map[string]struct{})}
			buckets[key] = bucket
			rowsByKey[key] = &statsRow{key: key, agent: ts.agent, status: ts.status, agg: bucket}
		}
		bucket.tasks[ts.id] = struct{}{}
		bucket.calls += ts.agg.calls
		bucket.prompt += ts.agg.prompt
		bucket.completion += ts.agg.completion
		bucket.retries += ts.agg.retries
		bucket.wasted += ts.agg.wasted
	}

	rows := make([]*statsRow, 0, len(rowsByKey))
	var session statsAgg
	for _, row := range rowsByKey {
		rows = append(rows, row)
		session.calls += row.agg.calls
		session.prompt += row.agg.prompt
		session.completion += row.agg.completion
		session.retries += row.agg.retries
		session.wasted += row.agg.wasted
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].agg.total() != rows[j].agg.total() {
			return rows[i].agg.total() > rows[j].agg.total()
		}
		return rows[i].key < rows[j].key
	})

	wastedPct := 0.0
	if session.total() > 0 {
		wastedPct = float64(session.wasted) / float64(session.total()) * 100
	}
	fmt.Fprintf(out, "session 总计: %d 个任务, %d 次 LLM 调用, prompt=%s, completion=%s, 合计=%s tokens, 重试=%d 次, 浪费=%s tokens (%.0f%%)\n",
		len(groups), session.calls,
		formatTokenCount(session.prompt), formatTokenCount(session.completion),
		formatTokenCount(session.total()), session.retries,
		formatTokenCount(session.wasted), wastedPct)
	fmt.Fprintln(out, "  （浪费口径：终态 cancelled/failed 任务的全部 token；completed 任务的 retry 消耗无法切分，见 RETRIES 列）")
	if session.calls == 0 {
		fmt.Fprintln(out, "\n（目录内没有 LLM 调用记录）")
		return nil
	}

	switch groupBy {
	case "task":
		fmt.Fprintln(out, "\n按 task 聚合（合计 token 降序）:")
		fmt.Fprintln(out, "TASK      AGENT            CALLS   RETRIES  PROMPT     COMPLETION  TOTAL      WASTED     STATUS")
		for _, row := range rows {
			fmt.Fprintf(out, "%-9s %-16s %-7d %-8d %-10s %-11s %-10s %-10s %s\n",
				fitColumn(shortIdentifier(row.key), 9), fitColumn(row.agent, 16),
				row.agg.calls, row.agg.retries,
				formatTokenCount(row.agg.prompt), formatTokenCount(row.agg.completion),
				formatTokenCount(row.agg.total()), formatTokenCount(row.agg.wasted), row.status)
		}
	case "agent":
		fmt.Fprintln(out, "\n按 agent 聚合（合计 token 降序）:")
		fmt.Fprintln(out, "AGENT            TASKS   CALLS   RETRIES  PROMPT     COMPLETION  TOTAL      WASTED")
		for _, row := range rows {
			fmt.Fprintf(out, "%-16s %-7d %-7d %-8d %-10s %-11s %-10s %-10s\n",
				fitColumn(row.key, 16), len(row.agg.tasks), row.agg.calls, row.agg.retries,
				formatTokenCount(row.agg.prompt), formatTokenCount(row.agg.completion),
				formatTokenCount(row.agg.total()), formatTokenCount(row.agg.wasted))
		}
	case "plan":
		fmt.Fprintln(out, "\n按 plan 聚合（合计 token 降序）:")
		fmt.Fprintln(out, "PLAN      TASKS   CALLS   RETRIES  PROMPT     COMPLETION  TOTAL      WASTED")
		for _, row := range rows {
			label := row.key
			if label != "(no-plan)" {
				label = shortIdentifier(label)
			}
			fmt.Fprintf(out, "%-9s %-7d %-7d %-8d %-10s %-11s %-10s %-10s\n",
				fitColumn(label, 9), len(row.agg.tasks), row.agg.calls, row.agg.retries,
				formatTokenCount(row.agg.prompt), formatTokenCount(row.agg.completion),
				formatTokenCount(row.agg.total()), formatTokenCount(row.agg.wasted))
		}
	}
	printStatsAnomalies(out, taskStats, &session)
	fmt.Fprintf(out, "\ntrace 目录: %s\n", dir)
	return nil
}

// printStatsAnomalies 在 stats 表格后输出 task 粒度的异常提示。规则刻意
// 保守（只报高置信信号），阈值与 detectAnomalies 无关、互不影响：
//   - session 浪费占比 > 20%：取消/失败任务消耗过高，检查 DAG 依赖与级联取消；
//   - 单任务重试 >= 3 次：同因重试可能存在系统性失败；
//   - 单任务消耗 > session 总量 40%（任务数 >= 3 时）：消耗异常集中；
//   - 单任务 read_file 重读率 > 30%（总读取 >= 4 次）：重复读取同一内容
//     （重复全文读 / 相同 offset 重复分页；新 offset 的顺序分页不算），
//     多为 Layer-1 snip 清掉旧内容后的重读循环（见
//     docs/activate/explorer-reread-waste-analysis-2026-07-22.md）。
func printStatsAnomalies(out io.Writer, taskStats []*taskStat, session *statsAgg) {
	var warnings []string
	if session.total() > 0 && session.wasted*5 > session.total() {
		warnings = append(warnings, fmt.Sprintf(
			"浪费占比 %.0f%% 超过 20%%（取消/失败任务消耗过高，检查 DAG 依赖结构与级联取消来源）",
			float64(session.wasted)/float64(session.total())*100))
	}
	for _, ts := range taskStats {
		if ts.agg.retries >= 3 {
			warnings = append(warnings, fmt.Sprintf(
				"任务 %s 重试 %d 次（同因重试可能存在系统性失败，用 trace show %s 排查）",
				shortIdentifier(ts.id), ts.agg.retries, shortIdentifier(ts.id)))
		}
		if len(taskStats) >= 3 && session.total() > 0 && ts.agg.total()*5 > session.total()*2 {
			warnings = append(warnings, fmt.Sprintf(
				"任务 %s 消耗 %s tokens，占 session 总量 %.0f%%（单任务消耗异常集中，检查是否跑满 loops 或上下文膨胀）",
				shortIdentifier(ts.id), formatTokenCount(ts.agg.total()),
				float64(ts.agg.total())/float64(session.total())*100))
		}
		// read_file 重读率：只计"重复读同一内容"（重复全文读 + 相同 offset
		// 重复分页），大文件顺序分页不算浪费。
		totalReads, reReads := 0, 0
		worstPath, worstCount := "", 0
		for path, rs := range ts.reads {
			totalReads += rs.full
			for _, count := range rs.pages {
				totalReads += count
			}
			if n := rs.reReads(); n > worstCount {
				worstPath, worstCount = path, n
			}
			reReads += rs.reReads()
		}
		if totalReads >= 4 && reReads*10 > totalReads*3 {
			warnings = append(warnings, fmt.Sprintf(
				"任务 %s read_file 重读率 %.0f%%（%d/%d 次为重复读取；最高 %s 被重读 %d 次——Layer-1 snip 后的重读循环，参考 explorer-reread-waste 分析）",
				shortIdentifier(ts.id), float64(reReads)/float64(totalReads)*100,
				reReads, totalReads, worstPath, worstCount))
		}
	}
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(out, "\n异常提示:")
	for _, w := range warnings {
		fmt.Fprintf(out, "  WARNING %s\n", w)
	}
}

// formatTokenCount 把 token 数格式化为 k/M 缩写，供 stats 表格对齐。
func formatTokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
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
		// 节点能力覆盖：仅发布方显式声明时出现（NodeCapability 投影）。
		if len(ev.ToolsOverride) > 0 {
			parts = append(parts, fmt.Sprintf("tools_override=%v", ev.ToolsOverride))
		}
		if ev.ModelOverride != "" {
			parts = append(parts, fmt.Sprintf("model_override=%s", ev.ModelOverride))
		}
		if ev.IsolationOverride != "" {
			parts = append(parts, fmt.Sprintf("isolation_override=%s", ev.IsolationOverride))
		}
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
	case KindMemoryContextInject:
		parts = append(parts, fmt.Sprintf("source=%s", ev.NotifyType))
		if ev.Path != "" {
			parts = append(parts, fmt.Sprintf("key=%s", ev.Path))
		}
		parts = append(parts, fmt.Sprintf("runes=%d", ev.OutputLen))
	case KindWorkspaceMaterialized, KindWorkspaceCleaned:
		// workspace 物化 / 清理：Path 是 workspace 根路径。
		if ev.Path != "" {
			parts = append(parts, fmt.Sprintf("path=%s", ev.Path))
		}
	case KindWorkspaceMerged:
		// 合并完成：Description 载逐文件结果摘要（fast-forward / auto-merged 计数）。
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 160)))
		}
	case KindWorkspaceMergeConflict:
		// 合并冲突：Path=冲突文件，Description=冲突详情。
		if ev.Path != "" {
			parts = append(parts, fmt.Sprintf("path=%s", ev.Path))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 160)))
		}
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
		KindMemoryContextInject, KindTaskCancelled:
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

	// 9. 检测：级联取消出现（cancel_source=dependency_failure；依赖失败传播，
	// 频繁出现说明 DAG 结构或依赖管理有问题）。注意真实来源字符串是
	// "dependency_failure"（watchdog.go / TransitionStateWithCancelSource），
	// 不要写成 "watchdog"——那只是历史 fixture 里的占位值。
	{
		cascadeCancels := 0
		for _, ev := range events {
			if ev.Kind == KindTaskCancelled && ev.Transition != nil &&
				ev.Transition.CancelSource == "dependency_failure" {
				cascadeCancels++
			}
		}
		if cascadeCancels > 0 {
			anomalies = append(anomalies, fmt.Sprintf(
				"WARNING 级联取消 %d 次（依赖失败传播；若频繁出现，检查 DAG 依赖结构与上游失败原因）",
				cascadeCancels))
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
