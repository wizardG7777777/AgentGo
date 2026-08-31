package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 延迟阈值：相邻事件间隔超过此值时在 show 输出中标记 WARNING。
const slowGapThreshold = 30 * time.Second

// CLI 实现 `agentgo trace list/show/stats/graph/node` 子命令的入口。
// args 是 trace 子命令后的剩余参数。dir 是 trace 文件目录。
// graphStateDir 是 GraphStore 持久化根目录（<project_root>/.agentgo/state/graphs），
// 供 graph/node 子命令读取 snapshot 头部；传空串跳过 snapshot（头部字段由
// 事件重建）。输出写入 out（通常是 os.Stdout）。
func CLI(args []string, dir, graphStateDir string, out io.Writer) error {
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
	case "stats":
		groupBy := "task"
		if len(args) >= 2 {
			groupBy = args[1]
		}
		if groupBy != "task" && groupBy != "agent" {
			return fmt.Errorf("usage: agentgo trace stats [task|agent]")
		}
		return cmdStats(dir, groupBy, out)
	case "graph":
		if len(args) < 2 {
			return cmdGraphList(dir, graphStateDir, out)
		}
		return cmdGraph(dir, graphStateDir, args[1], out)
	case "node":
		if len(args) < 2 {
			return fmt.Errorf("usage: agentgo trace node <graph_id>/<node_id>")
		}
		return cmdGraphNode(dir, graphStateDir, args[1], out)
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
  stats [task|agent]    聚合当前 trace 目录内全部任务的 LLM 调用与
                        token 消耗（默认按 task 分组，按总 token 降序）
  graph [graph_id]      无参：列出全部已知图（trace 事件 ∪ state 目录）
                        带参：按时间顺序展示该图的全部生命周期事件
                        graph_id 可以是完整 ID 或任意唯一前缀
  node <graph_id>/<node_id>
                        只展示单个节点的事件，按 activation 分组
                        （回边重进 = 新 activation，一目了然）

示例:
  agentgo trace list
  agentgo trace show 321b561d
  agentgo trace show 321b561d-c564-422c-bfa0-b96f54edcb87
  agentgo trace stats agent
  agentgo trace graph
  agentgo trace graph deploy-pipeline
  agentgo trace node deploy-pipeline/implement

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

// printDegradedHint 在检测到 trace_degraded.marker 时向 out 打印一行降级
// 提示（V6 §7.1）；无 marker 时 no-op。
func printDegradedHint(dir string, out io.Writer) {
	if m := ReadDegradedMarker(dir); m != nil {
		fmt.Fprintf(out, "trace_degraded: 首次失败 %s，连续失败 %d 次（%s）——期间事件可能不完整\n\n",
			m.FirstFailureTime, m.Count, m.Error)
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

	// V6 §7.1：检测到降级标记时在表头前提示——marker 存在即当前处于降级态
	//（Writer 写恢复后会清除），期间事件可能不完整
	printDegradedHint(dir, out)

	// 表头
	fmt.Fprintln(out, "┌───────────────┬─────────────────────┬──────────┬────────────┬───────┬───────────┬────────┬─────────────┐")
	fmt.Fprintln(out, "│ Task          │ Published           │ Agent    │ Status     │ Loops │ Files Out │ Errors │ Duration    │")
	fmt.Fprintln(out, "├───────────────┼─────────────────────┼──────────┼────────────┼───────┼───────────┼────────┼─────────────┤")

	for _, group := range groups {
		row := summarizeTask(group)
		fmt.Fprintf(out, "│ %-13s │ %-19s │ %-8s │ %-10s │ %5d │ %9d │ %6d │ %-11s │\n",
			row.taskShortID,
			row.publishedAt.Local().Format("2006-01-02 15:04:05"),
			fitColumn(row.agentID, 8),
			row.status,
			row.loops,
			row.filesWritten,
			row.errors,
			formatDuration(row.duration),
		)
	}
	fmt.Fprintln(out, "└───────────────┴─────────────────────┴──────────┴────────────┴───────┴───────────┴────────┴─────────────┘")
	fmt.Fprintf(out, "\n共 %d 个任务，trace 目录: %s\n", len(groups), dir)
	return nil
}

// taskSummary 是 list 命令一行的汇总信息。
type taskSummary struct {
	taskShortID  string
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
// （每次 LLM 调用一条，载本轮消耗），是唯一的 token 账本。
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
// 最小聚合单位；agent 视图由它二次聚合，异常检测也基于它。
type taskStat struct {
	id     string
	agent  string
	status string
	calls  int
	agg    statsAgg
	// reads 统计同任务内 read_file 按 path 的调用结构，供重读率异常检测。
	reads map[string]*readFileStat
}

// cmdStats 实现 agentgo trace stats [task|agent]。
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
		ts := &taskStat{id: g.displayID(), agent: summary.agentID, status: summary.status, reads: make(map[string]*readFileStat)}
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
	// V6 §7.1：检测到降级标记时在 header 打 trace_degraded 提示
	if m := ReadDegradedMarker(dir); m != nil {
		fmt.Fprintf(out, " trace_degraded: 首次失败 %s，连续失败 %d 次（%s）——期间事件可能不完整\n",
			m.FirstFailureTime, m.Count, m.Error)
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
	case KindTeamGraphBound:
		// Graph-scoped Team 建立：GraphID 是资源所有权边界，TaskID 仅是
		// 发起 provision 的 Scheduler task 来源，不是 Team 的生命周期 owner。
		if ev.GraphID != "" {
			parts = append(parts, fmt.Sprintf("graph=%s", ev.GraphID))
		}
		if ev.TaskID != "" {
			parts = append(parts, fmt.Sprintf("origin_task=%s", ev.TaskID))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 200)))
		}
	case KindTeamStopped:
		// Graph 终态回收 Team：同时展示 Graph 归属和 durable stop 原因。
		if ev.GraphID != "" {
			parts = append(parts, fmt.Sprintf("graph=%s", ev.GraphID))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 200)))
		}
		parts = appendReason(parts, "reason", ev.Reason)
	case KindTaskRetry:
		parts = appendTaskTransition(parts, ev.Transition, true, false)
		if ev.AttemptNo > 0 {
			parts = append(parts, fmt.Sprintf("attempt=%d", ev.AttemptNo))
		}
		parts = appendReason(parts, "reason", ev.Reason)
		if ev.FailureKind != "" {
			parts = append(parts, fmt.Sprintf("failure=%s recovery=%s", ev.FailureKind, ev.RecoveryAction))
		}
	case KindLLMCallStart:
		parts = append(parts, fmt.Sprintf("history_entries=%d tools=%d", ev.HistoryEntries, ev.ToolCallsCount))
		if ev.ContextSnapshotID != "" {
			parts = append(parts, fmt.Sprintf("snapshot=%s policy=%s tools_snapshot=%s",
				ev.ContextSnapshotID, ev.ContextPolicyRef, ev.ToolRouterSnapshotID))
		}
	case KindLLMCallEnd:
		parts = append(parts, fmt.Sprintf("duration=%dms", ev.DurationMS))
		parts = append(parts, fmt.Sprintf("prompt_tokens=%d completion_tokens=%d tool_calls=%d",
			ev.PromptTokens, ev.CompletionTokens, ev.ToolCallsCount))
		if ev.FinishReason != "" {
			parts = append(parts, fmt.Sprintf("finish_reason=%s", ev.FinishReason))
		}
		if ev.FailureKind != "" {
			parts = append(parts, fmt.Sprintf("failure=%s phase=%s origin=%s scope=%s",
				ev.FailureKind, ev.FailurePhase, ev.FailureOrigin, ev.TimeoutScope))
			if ev.ProviderCode != "" || ev.HTTPStatus != 0 {
				parts = append(parts, fmt.Sprintf("provider_code=%s http_status=%d", ev.ProviderCode, ev.HTTPStatus))
			}
			if ev.UsageState != "" || ev.Partial {
				parts = append(parts, fmt.Sprintf("usage_state=%s partial=%t", ev.UsageState, ev.Partial))
			}
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
	case KindHistoryCompaction:
		parts = append(parts, fmt.Sprintf("tokens_before=%d tokens_after=%d strategy=%s kept_entries=%d",
			ev.PromptTokensBefore, ev.PromptTokensAfter, ev.Strategy, ev.KeptEntries))
	case KindContextManifestBuilt:
		// L2 durable Snapshot：估算量与真实 WireItems 同源；Description 是
		// metadata-only Manifest 摘要。legacy 事件可能没有 SnapshotID。
		parts = append(parts, fmt.Sprintf("est_prompt_tokens=%d history_entries=%d", ev.PromptTokens, ev.HistoryEntries))
		if ev.ContextSnapshotID != "" {
			parts = append(parts, fmt.Sprintf("snapshot=%s policy=%s tools_snapshot=%s",
				ev.ContextSnapshotID, ev.ContextPolicyRef, ev.ToolRouterSnapshotID))
		}
		if ev.PromptBuildID != "" {
			parts = append(parts, fmt.Sprintf("build=%s", ev.PromptBuildID))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("sections=%q", truncate(ev.Description, 200)))
		}
	case KindPromptCompiled:
		// P1a Prompt 有序编译：Build.ID + 逐组件身份摘要（不含正文）。
		if ev.PromptBuildID != "" {
			parts = append(parts, fmt.Sprintf("build=%s", ev.PromptBuildID))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("components=%q", truncate(ev.Description, 200)))
		}
	case KindAgentAuditStarted, KindAgentAuditWarning, KindAgentAuditCompleted:
		// P1b /doctor agents 审计：只载计数/digest/类型摘要（不含 prompt
		// 正文）；completed 的 Reason 载终态。
		parts = appendReason(parts, "reason", ev.Reason)
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("summary=%q", truncate(ev.Description, 200)))
		}
	case KindTaskMemoryCreated, KindTaskMemoryUpdated, KindTaskMemoryCheckpointed,
		KindObservationDeltaRecorded, KindObservationCheckpointFailed:
		// CM2 Task Memory：Description 是段计数 JSON 摘要（不含正文）；
		// checkpoint 事件带 Reason（history_compaction / attempt_end / terminal:*）。
		parts = appendReason(parts, "reason", ev.Reason)
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("sections=%q", truncate(ev.Description, 200)))
		}
	case KindRecoveryActionGated:
		if gate := ev.RecoveryGate; gate != nil {
			parts = append(parts, fmt.Sprintf("schema=%s stage=%s tool=%s directives=%d",
				gate.Schema, gate.Stage, gate.Tool, gate.DirectiveCount))
			if gate.Path != "" {
				parts = append(parts, fmt.Sprintf("path=%s", gate.Path))
			}
			if gate.CheckID != "" {
				parts = append(parts, fmt.Sprintf("check_id=%s", gate.CheckID))
			}
			if gate.RefID != "" {
				parts = append(parts, fmt.Sprintf("ref_id=%s offset=%d limit=%d", gate.RefID, gate.Offset, gate.Limit))
			}
		}
	case KindSessionMemoryPromotionProposed, KindSessionMemoryPromotionDecided,
		KindMemoryRecalled, KindMemoryEntryStateChanged:
		// CM3 Session Memory：Reason 载终态（晋升事件）；Description 是 JSON
		// 摘要（晋升 decided/条目数/Key 列表/状态迁移，均不含正文）。
		parts = appendReason(parts, "reason", ev.Reason)
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("summary=%q", truncate(ev.Description, 200)))
		}
	case KindProgressNotify:
		parts = append(parts, fmt.Sprintf("notify_type=%s", ev.NotifyType))
	case KindWorkspaceMaterialized, KindWorkspaceCleaned:
		// workspace 物化 / 清理：Path 是 workspace 根路径。
		if ev.Path != "" {
			parts = append(parts, fmt.Sprintf("path=%s", ev.Path))
		}
	case KindWorkspaceRetentionDecided, KindWorkspaceCleanupRejected:
		if ev.Path != "" {
			parts = append(parts, fmt.Sprintf("path=%s", ev.Path))
		}
		parts = appendReason(parts, "reason", ev.Reason)
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 160)))
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
	case KindRuntimeLoopFuseTriggered:
		// emergency fuse：Loop 载触发时的循环计数，Reason 载兜底说明。
		parts = append(parts, fmt.Sprintf("loop=%d", ev.Loop))
		parts = appendReason(parts, "reason", ev.Reason)
	case KindTaskFinalizing:
		// 结构化提交被接受：Transition.NewStatus 载自述终态（completed/blocked）。
		if ev.Transition != nil && ev.Transition.NewStatus != "" {
			parts = append(parts, fmt.Sprintf("status=%s", ev.Transition.NewStatus))
		}
	case KindToolCallSkipped:
		// finalizing fence 拦截：Tool/CallID 定位被跳过的调用，Reason 载原因。
		parts = append(parts, fmt.Sprintf("tool=%s", ev.Tool))
		if ev.CallID != "" {
			parts = append(parts, fmt.Sprintf("call_id=%s", ev.CallID))
		}
		parts = appendReason(parts, "reason", ev.Reason)
	case KindTaskResultCommitted:
		// 结构化提交的收尾事务已 durable：Transition 载终态与 cause。
		parts = appendTaskTransition(parts, ev.Transition, false, false)
		parts = appendReason(parts, "reason", ev.Reason)
	case KindSuggestionsReturned:
		// H2a：Gate 结构化拒绝给出建议——只展示计数与标识，不载正文。
		if ev.Suggestion != nil {
			if ev.Suggestion.ReasonCode != "" {
				parts = append(parts, fmt.Sprintf("reason_code=%s", ev.Suggestion.ReasonCode))
			}
			parts = append(parts, fmt.Sprintf("retryable=%v", ev.Suggestion.Retryable))
			parts = append(parts, fmt.Sprintf("offered=%d", ev.Suggestion.Offered))
			if ev.Suggestion.Filtered > 0 {
				parts = append(parts, fmt.Sprintf("filtered=%d", ev.Suggestion.Filtered))
			}
			if ev.Suggestion.FilterReason != "" {
				parts = append(parts, fmt.Sprintf("filter=%s", ev.Suggestion.FilterReason))
			}
			if ev.Suggestion.RepeatCount > 1 {
				parts = append(parts, fmt.Sprintf("repeat=%d", ev.Suggestion.RepeatCount))
			}
		}
	case KindSuggestionDisposition:
		// H2a：上一轮建议的去向（adopted / abandoned / repeated）。
		if ev.Suggestion != nil {
			if ev.Suggestion.SuggestionID != "" {
				parts = append(parts, fmt.Sprintf("id=%s", ev.Suggestion.SuggestionID))
			}
			if ev.Suggestion.Disposition != "" {
				parts = append(parts, fmt.Sprintf("disposition=%s", ev.Suggestion.Disposition))
			}
			if ev.Suggestion.ReasonCode != "" {
				parts = append(parts, fmt.Sprintf("reason_code=%s", ev.Suggestion.ReasonCode))
			}
		}
	case KindExecutionLeaseFrozen, KindExecutionLeaseRejected,
		KindExecutionLeaseReused, KindExecutionLeaseRevoked:
		// ExecutionLease（V6 §4 H1）：Lease 子载荷载 digest/工具计数/模型/
		// 隔离/Synthetic；rejected 附缺失清单，revoked 附撤销原因。
		if ev.Lease != nil {
			if ev.Lease.Digest != "" {
				parts = append(parts, fmt.Sprintf("digest=%s", ev.Lease.Digest))
			}
			parts = append(parts, fmt.Sprintf("biz=%d ctl=%d", ev.Lease.BusinessTools, ev.Lease.ControlTools))
			if ev.Lease.Model != "" {
				parts = append(parts, fmt.Sprintf("model=%s", ev.Lease.Model))
			}
			if ev.Lease.Workspace != "" {
				parts = append(parts, fmt.Sprintf("workspace=%s", ev.Lease.Workspace))
			}
			if ev.Lease.Synthetic {
				parts = append(parts, "synthetic=true")
			}
			if len(ev.Lease.Missing) > 0 {
				parts = append(parts, fmt.Sprintf("missing=%v", ev.Lease.Missing))
			}
			if ev.Lease.Cause != "" {
				parts = appendReason(parts, "cause", ev.Lease.Cause)
			}
		}
		parts = appendReason(parts, "reason", ev.Reason)
	case KindEffectPrepared, KindEffectSettled, KindEffectUnknown, KindEffectRecoveryDecided:
		// Effect Journal（V6 §4 H2b）：展示标识与摘要（effect_id / kind /
		// policy / target / result_summary / decision），不含完整参数/命令。
		if ev.Effect != nil {
			if ev.Effect.EffectID != "" {
				parts = append(parts, fmt.Sprintf("effect=%s", ev.Effect.EffectID))
			}
			parts = append(parts, fmt.Sprintf("kind=%s policy=%s", ev.Effect.Kind, ev.Effect.Policy))
			if ev.Effect.Target != "" {
				parts = append(parts, fmt.Sprintf("target=%s", truncate(ev.Effect.Target, 80)))
			}
			if ev.Effect.ResultSummary != "" {
				parts = append(parts, fmt.Sprintf("result=%q", truncate(ev.Effect.ResultSummary, 120)))
			}
			if ev.Effect.Decision != "" {
				parts = append(parts, fmt.Sprintf("decision=%s", ev.Effect.Decision))
			}
			parts = appendReason(parts, "reason", ev.Effect.Reason)
		}
	case KindGraphSubmitted, KindGraphSubmissionRejected, KindNodeActivationCreated,
		KindGraphTransitionSelected, KindGraphEnded, KindGraphJoinResolved,
		KindGraphWaitStarted, KindGraphWaitResumed, KindGraphApprovalDecided,
		KindGraphChangeRequested, KindGraphRevisionCommitted:
		// Graph Runtime 事件（V6 §6）：展示图 / 节点 / activation 归属。
		if ev.GraphID != "" {
			parts = append(parts, fmt.Sprintf("graph=%s", ev.GraphID))
		}
		if ev.NodeID != "" {
			parts = append(parts, fmt.Sprintf("node=%s", ev.NodeID))
		}
		if ev.ActivationID != "" {
			parts = append(parts, fmt.Sprintf("activation=%s", ev.ActivationID))
		}
		if ev.GraphOutcome != "" {
			parts = append(parts, fmt.Sprintf("outcome=%s", ev.GraphOutcome))
		}
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 200)))
		}
		if ev.Error != "" {
			parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 200)))
		}
		parts = appendReason(parts, "reason", ev.Reason)
	case KindAcceptanceCompleted:
		// 验收服务端核验（G1b）：图归属 + 自报 verdict / 核验结论 / 核验条数。
		if ev.GraphID != "" {
			parts = append(parts, fmt.Sprintf("graph=%s", ev.GraphID))
		}
		if ev.NodeID != "" {
			parts = append(parts, fmt.Sprintf("node=%s", ev.NodeID))
		}
		if ev.ActivationID != "" {
			parts = append(parts, fmt.Sprintf("activation=%s", ev.ActivationID))
		}
		if ev.Acceptance != nil {
			parts = append(parts, fmt.Sprintf("verdict=%s verify=%s checked=%d",
				ev.Acceptance.Verdict, ev.Acceptance.Status, ev.Acceptance.Checked))
			parts = appendReason(parts, "reason", ev.Acceptance.Reason)
		}
		parts = appendReason(parts, "reason", ev.Reason)
	default:
		// 用户事件使用 user.<name> 命名空间，payload 的 Description 是
		// 主要可读内容。未知未来事件（含旧版 plan_/replan_ 等已删除的
		// Plan 时代事件行）也至少展示已有通用字段，避免静默空白。
		if ev.Description != "" {
			parts = append(parts, fmt.Sprintf("desc=%q", truncate(ev.Description, 200)))
		}
		if ev.Error != "" {
			parts = append(parts, fmt.Sprintf("error=%q", truncate(ev.Error, 200)))
		}
		parts = appendReason(parts, "reason", ev.Reason)
	}
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

func eventCarriesLoop(kind EventKind) bool {
	switch kind {
	case KindLLMCallStart, KindLLMCallEnd, KindToolCall, KindToolResult,
		KindHistoryCompaction, KindProgressNotify,
		KindTaskCancelled, KindContextManifestBuilt,
		KindTaskMemoryUpdated, KindTaskMemoryCheckpointed, KindObservationDeltaRecorded,
		KindObservationCheckpointFailed, KindRecoveryActionGated,
		KindToolCallSkipped:
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

// ============================================================
// trace graph / trace node（V6 §7.5 Graph 查询入口）
// ============================================================

// graphSnapshotHead 是 GraphStore snapshot.json 的本地最小解码结构。
// 纪律：trace 包不得 import internal/graph（graph 已 import trace 发事件，
// 反向会成环），snapshot 头部字段在 CLI 侧用本结构解码；字段名与
// internal/graph/journal.go 的 snapshotFile 保持对齐。
type graphSnapshotHead struct {
	Version      int    `json:"version"`
	GraphID      string `json:"graph_id"`
	Revision     int64  `json:"revision"`
	StateVersion int64  `json:"state_version"`
	Digest       string `json:"digest"`
	Doc          *struct {
		Status  string `json:"status"`
		Outcome *struct {
			Outcome string `json:"outcome"`
		} `json:"outcome,omitempty"`
	} `json:"doc"`
}

// graphEventSet 是一张图的全部可归事件与其物理来源。
type graphEventSet struct {
	records []traceEventRecord
	issues  []traceIssue
	files   map[string]taskFile
}

// graphScan 是一次 trace 目录扫描的结果：按事件的 GraphID 精确归组。
// 图事件主体在 graph_<前缀>.jsonl 分片；graph_change_requested 携 TaskID
// 落任务分片（internal/tools/plan_control.go），因此扫描覆盖全部 JSONL
// 分片而不是只看 graph_ 分片。
type graphScan struct {
	byGraph map[string]*graphEventSet
	files   map[string]bool // 目录内存在的 .jsonl 文件名集合（判预期分片缺失用）
}

func (s *graphScan) setFor(graphID string) *graphEventSet {
	set := s.byGraph[graphID]
	if set == nil {
		set = &graphEventSet{files: make(map[string]taskFile)}
		s.byGraph[graphID] = set
	}
	return set
}

// scanGraphEvents 读取 dir 内全部 JSONL 分片（含任务分片），把带 GraphID
// 的事件按完整 GraphID 归组，按 ts+filename+line 稳定排序。分片中的坏行
// 与读取失败转化为各图的 issue（覆盖度 partial 判定的输入）。
func scanGraphEvents(dir string) (*graphScan, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("无法读取 trace 目录 %s: %w", dir, err)
	}
	scan := &graphScan{byGraph: make(map[string]*graphEventSet), files: make(map[string]bool)}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".prompts.jsonl") {
			continue
		}
		scan.files[name] = true
		file := taskFile{path: filepath.Join(dir, name), filename: name}
		events, readErr := readAllEvents(file.path)
		graphIDs := make(map[string]struct{})
		parseErrs := 0
		for _, ev := range events {
			if ev.GraphID != "" {
				graphIDs[ev.GraphID] = struct{}{}
			}
			if ev.Kind == "<parse_error>" {
				parseErrs++
			}
		}
		for line, ev := range events {
			if ev.GraphID == "" {
				continue
			}
			set := scan.setFor(ev.GraphID)
			set.files[file.path] = file
			set.records = append(set.records, traceEventRecord{event: ev, file: file, line: line + 1})
		}
		// 坏行/读取失败归属：只记到本分片实际贡献了事件的图上。分片内可能
		// 混有多张图（父子图共享 8 位前缀），此时坏行无法精确归属，保守地
		// 全部标记并在消息中说明。
		if parseErrs > 0 || readErr != nil {
			for id := range graphIDs {
				set := scan.setFor(id)
				if parseErrs > 0 {
					msg := fmt.Sprintf("%d 行无法解析", parseErrs)
					if len(graphIDs) > 1 {
						msg = fmt.Sprintf("%d 行无法解析（分片内混有 %d 张图，无法精确归属）", parseErrs, len(graphIDs))
					}
					set.issues = append(set.issues, traceIssue{file: file, message: msg, errorCount: parseErrs})
				}
				if readErr != nil {
					set.issues = append(set.issues, traceIssue{file: file, message: readErr.Error(), errorCount: 1, readFailure: true})
				}
			}
		}
	}
	for _, set := range scan.byGraph {
		sortTraceRecords(set.records)
	}
	return scan, nil
}

// graphStateEntry 是 .agentgo/state/graphs 下一图持久化目录的发现结果。
type graphStateEntry struct {
	id           string
	snapshotPath string // 空串表示该图尚无 snapshot.json（仅 journal，属正常）
}

// scanGraphStateDir 发现 GraphStore 持久化目录下的全部图。graph_id 的 "/"
// 段映射为嵌套目录（子图嵌在父图目录下，见 store.graphDir）；含
// snapshot.json 或 journal.jsonl 的目录计为一张图。目录不存在返回空结果
// （没跑过图不是错误）。
func scanGraphStateDir(graphStateDir string) map[string]graphStateEntry {
	out := make(map[string]graphStateEntry)
	if graphStateDir == "" {
		return out
	}
	root := filepath.Clean(graphStateDir)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || rel == "." {
			return nil
		}
		// 子图按 <父图>/<activationID> 嵌套（MaxSubgraphDepth 另有上限），
		// 深度设防御性上限，避免异常目录结构导致无限下钻。
		if len(strings.Split(filepath.ToSlash(rel), "/")) > 8 {
			return filepath.SkipDir
		}
		snap := filepath.Join(path, "snapshot.json")
		if _, serr := os.Stat(snap); serr == nil {
			out[filepath.ToSlash(rel)] = graphStateEntry{id: filepath.ToSlash(rel), snapshotPath: snap}
			return nil
		}
		if _, jerr := os.Stat(filepath.Join(path, "journal.jsonl")); jerr == nil {
			out[filepath.ToSlash(rel)] = graphStateEntry{id: filepath.ToSlash(rel)}
		}
		return nil
	})
	return out
}

// readGraphSnapshotHead 读取一图的 snapshot 头。snapshotPath 为空（图尚无
// snapshot，压缩前属正常）时 ok=false、err=nil；文件存在但读不出/解码失败/
// graph_id 不符时 ok=false 且 err 非 nil（覆盖度 degraded 判定）。
func readGraphSnapshotHead(snapshotPath, graphID string) (head graphSnapshotHead, ok bool, err error) {
	if snapshotPath == "" {
		return graphSnapshotHead{}, false, nil
	}
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		return graphSnapshotHead{}, false, err
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return graphSnapshotHead{}, false, fmt.Errorf("snapshot JSON 解析失败: %w", err)
	}
	if head.GraphID != "" && head.GraphID != graphID {
		return graphSnapshotHead{}, false, fmt.Errorf("snapshot 的 graph_id=%q 与查询的图 %q 不符", head.GraphID, graphID)
	}
	return head, true, nil
}

// resolveGraphID 按完整 ID 或唯一前缀定位图；候选集合 = trace 事件里的
// GraphID ∪ state 目录里发现的图。语义与 task_id 前缀一致：碰撞列候选。
func resolveGraphID(scan *graphScan, state map[string]graphStateEntry, query string) ([]string, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("graph_id 不能为空")
	}
	seen := make(map[string]struct{})
	for id := range scan.byGraph {
		seen[id] = struct{}{}
	}
	for id := range state {
		seen[id] = struct{}{}
	}
	var matches []string
	for id := range seen {
		if strings.HasPrefix(id, query) {
			matches = append(matches, id)
		}
	}
	sort.Strings(matches)
	return matches, nil
}

var (
	// graph_submitted 的 Description 形如 "revision=1 digest=abc123def456"。
	graphSubmittedDescRe = regexp.MustCompile(`\brevision=(\d+)(?:\s+digest=(\S+))?`)
	// graph_revision_committed 的 Description 形如 "new_revision=2 upsert=[...]"。
	graphRevisionDescRe = regexp.MustCompile(`\bnew_revision=(\d+)\b`)
)

// graphFactsFromEvents 从图事件时间线（已按时间排序）重建头部事实：
// status 取最新生命周期事件，revision/digest 取最新 submitted/committed
// 描述里的值；没有对应事件时保持零值。
func graphFactsFromEvents(records []traceEventRecord) (status string, revision int64, hasRevision bool, digest string) {
	for _, r := range records {
		ev := r.event
		switch ev.Kind {
		case KindGraphSubmitted:
			status = "running"
			if m := graphSubmittedDescRe.FindStringSubmatch(ev.Description); m != nil {
				if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					revision, hasRevision = v, true
				}
				if m[2] != "" {
					digest = m[2]
				}
			}
		case KindGraphRevisionCommitted:
			if m := graphRevisionDescRe.FindStringSubmatch(ev.Description); m != nil {
				if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					revision, hasRevision = v, true
				}
			}
		case KindGraphSubmissionRejected:
			status = "submission_rejected"
		case KindGraphEnded:
			// 新事件使用 typed business outcome；旧事件才回退 Reason 猜测。
			switch ev.GraphOutcome {
			case "success":
				status = "completed"
			case "failed":
				status = "failed"
			case "blocked":
				status = "blocked"
			case "cancelled":
				status = "cancelled"
			default:
				if ev.GraphOutcome != "" {
					status = "invalid_outcome"
				} else if ev.Reason == "" {
					status = "completed"
				} else {
					status = "failed"
				}
			}
		}
	}
	return status, revision, hasRevision, digest
}

// resolveGraphStatus 合并事件与 snapshot 两个来源的图状态：终态
// （completed/failed/blocked/cancelled）一旦出现以它为准（snapshot 可能旧于 trace，trace
// 也可能已被 GC 而只剩 snapshot）；否则 snapshot 优先，事件兜底。
func resolveGraphStatus(eventStatus, snapStatus string) string {
	if eventStatus == "invalid_outcome" {
		return eventStatus
	}
	if isTerminalGraphStatus(eventStatus) {
		return eventStatus
	}
	if isTerminalGraphStatus(snapStatus) {
		return snapStatus
	}
	if snapStatus != "" {
		return snapStatus
	}
	if eventStatus != "" {
		return eventStatus
	}
	return "unknown"
}

func isTerminalGraphStatus(s string) bool {
	return s == "completed" || s == "failed" || s == "blocked" || s == "cancelled"
}

func graphOutcomeFromEvents(records []traceEventRecord) string {
	outcome := ""
	for _, record := range records {
		if record.event.Kind != KindGraphEnded {
			continue
		}
		if record.event.GraphOutcome == "" {
			outcome = "legacy"
		} else {
			outcome = record.event.GraphOutcome
		}
	}
	return outcome
}

// truncateDigest12 与 internal/graph 的 truncateDigest 同口径（前 12 位）。
func truncateDigest12(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// graphHeader 是 trace graph / trace node 输出的头部信息。
type graphHeader struct {
	id           string
	status       string
	outcome      string
	revision     string
	stateVersion string
	digest       string
	fromSnapshot bool
	rebuilt      bool // snapshot 不可用且头部字段由事件重建
	events       int
	shards       int
	coverage     string // complete / partial / degraded
	reasons      []string
}

// buildGraphHeader 汇总 snapshot 头（可读时优先）与事件重建信息，并给出
// 覆盖度判定：
//   - complete：预期分片存在、贡献文件无坏行/读取失败、snapshot 可读或缺席；
//   - partial：预期分片缺失（被 GC 或写入失败），或贡献文件有坏行/读取失败；
//   - degraded：snapshot 存在但不可读/不符（时间线照常展示）。
//
// degraded 优先于 partial。
func buildGraphHeader(id string, set *graphEventSet, entry graphStateEntry, scan *graphScan) graphHeader {
	h := graphHeader{
		id: id, status: "unknown", outcome: "unknown", revision: "unknown", stateVersion: "unknown", digest: "unknown",
	}
	var records []traceEventRecord
	if set != nil {
		h.events = len(set.records)
		h.shards = len(set.files)
		records = set.records
	}
	eventStatus, eventRev, hasEventRev, eventDigest := graphFactsFromEvents(records)
	eventOutcome := graphOutcomeFromEvents(records)

	head, snapOK, snapErr := readGraphSnapshotHead(entry.snapshotPath, id)
	snapStatus := ""
	switch {
	case snapErr != nil:
		h.coverage = "degraded"
		h.reasons = append(h.reasons, fmt.Sprintf("snapshot 不可读（%v），头部字段由事件重建", snapErr))
	case snapOK:
		h.fromSnapshot = true
		h.revision = strconv.FormatInt(head.Revision, 10)
		h.stateVersion = strconv.FormatInt(head.StateVersion, 10)
		if head.Digest != "" {
			h.digest = truncateDigest12(head.Digest)
		}
		if head.Doc != nil {
			snapStatus = head.Doc.Status
			if head.Doc.Outcome != nil && head.Doc.Outcome.Outcome != "" {
				h.outcome = head.Doc.Outcome.Outcome
			}
		}
	}
	h.status = resolveGraphStatus(eventStatus, snapStatus)
	if eventOutcome != "" {
		h.outcome = eventOutcome
	}
	if !h.fromSnapshot {
		if hasEventRev {
			h.revision = strconv.FormatInt(eventRev, 10)
		}
		if eventDigest != "" {
			h.digest = eventDigest
		}
		h.rebuilt = len(records) > 0
	}

	// partial 判定：预期分片缺失，或贡献文件存在坏行/读取失败。
	expectedShard := graphShardFileName(id)
	if !scan.files[expectedShard] {
		h.reasons = append(h.reasons,
			fmt.Sprintf("预期分片 %s 不存在（事件可能已被 GC 或从未写入）", expectedShard))
	}
	if set != nil {
		for _, issue := range uniqueTraceIssues(set.issues) {
			h.reasons = append(h.reasons, fmt.Sprintf("%s: %s", issue.file.filename, issue.message))
		}
	}
	if h.coverage == "" {
		if len(h.reasons) > 0 {
			h.coverage = "partial"
		} else {
			h.coverage = "complete"
		}
	}
	return h
}

func printGraphHeader(out io.Writer, h graphHeader) {
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	fmt.Fprintf(out, " Graph: %s\n", h.id)
	fmt.Fprintf(out, " Status: %s  Revision: %s  StateVersion: %s  Digest: %s\n",
		h.status, h.revision, h.stateVersion, h.digest)
	fmt.Fprintf(out, " Outcome: %s\n", h.outcome)
	fmt.Fprintf(out, " Events: %d  Shards: %d  Coverage: %s\n", h.events, h.shards, h.coverage)
	fmt.Fprintln(out, "════════════════════════════════════════════════════════════════════════════════")
	if h.rebuilt {
		fmt.Fprintln(out, " （snapshot 不可用，头部字段由事件重建；state_version 仅 snapshot 携带）")
	}
	for _, reason := range h.reasons {
		fmt.Fprintf(out, " WARNING: %s\n", reason)
	}
}

// printGraphTimeline 按时间序打印图事件（与 cmdShow 同一排版：首行时间 +
// kind + 归属引用，次行 formatEventDetails 细节——graph/node/activation
// 归属字段由后者展示；相邻事件间隔超阈值时标 WARNING）。
func printGraphTimeline(out io.Writer, records []traceEventRecord) {
	var prev time.Time
	for i, record := range records {
		ev := record.event
		ts := ev.Timestamp.Local().Format("15:04:05.000")
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

		fmt.Fprintf(out, "%s[%s] %-24s", warnPrefix, ts, ev.Kind)
		// 携 TaskID 的图事件（graph_change_requested）以引用形式展示任务，
		// 不合并任务时间线（任务细节走 trace show <task_id>）。
		if ev.TaskID != "" {
			fmt.Fprintf(out, " task=%s", shortIdentifier(ev.TaskID))
		}
		fmt.Fprintln(out)

		if details := formatEventDetails(ev); details != "" {
			fmt.Fprintf(out, "             %s\n", details)
		}
	}
}

// resolveSingleGraph 是 graph/node 两个命令共用的定位步骤：扫描 + 前缀
// 解析。0 个匹配报中文错误；多个匹配列候选并返回空串（调用方直接返回）。
func resolveSingleGraph(dir, graphStateDir, query string, out io.Writer) (string, *graphScan, *graphEventSet, graphStateEntry, error) {
	scan, err := scanGraphEvents(dir)
	if err != nil {
		return "", nil, nil, graphStateEntry{}, err
	}
	state := scanGraphStateDir(graphStateDir)
	matches, err := resolveGraphID(scan, state, strings.TrimSpace(query))
	if err != nil {
		return "", nil, nil, graphStateEntry{}, err
	}
	if len(matches) == 0 {
		return "", nil, nil, graphStateEntry{}, fmt.Errorf("未找到匹配 graph_id=%s 的图（trace 目录: %s）", query, dir)
	}
	if len(matches) > 1 {
		fmt.Fprintf(out, "找到 %d 个匹配的图，请使用更长的 graph_id 区分:\n", len(matches))
		for _, id := range matches {
			fmt.Fprintf(out, "  %s\n", id)
		}
		return "", scan, nil, graphStateEntry{}, nil
	}
	id := matches[0]
	return id, scan, scan.byGraph[id], state[id], nil
}

// cmdGraph 实现 agentgo trace graph <graph_id>：图级生命周期时间线。
func cmdGraph(dir, graphStateDir, query string, out io.Writer) error {
	id, scan, set, entry, err := resolveSingleGraph(dir, graphStateDir, query, out)
	if err != nil || id == "" {
		return err
	}
	header := buildGraphHeader(id, set, entry, scan)
	printGraphHeader(out, header)
	if set == nil || len(set.records) == 0 {
		fmt.Fprintln(out, "（该图没有可追溯的事件：分片可能已被 GC，或事件从未写入）")
		return nil
	}
	printGraphTimeline(out, set.records)
	fmt.Fprintln(out, "────────────────────────────────────────────────────────────────────────────────")
	fmt.Fprintf(out, " events=%d  shards=%d  coverage=%s\n", header.events, header.shards, header.coverage)
	return nil
}

// cmdGraphList 实现无参 agentgo trace graph：列出全部已知图
// （trace 事件 ∪ state 目录，去重），含状态与最近事件时间。
func cmdGraphList(dir, graphStateDir string, out io.Writer) error {
	scan, err := scanGraphEvents(dir)
	if err != nil {
		return err
	}
	state := scanGraphStateDir(graphStateDir)
	ids := make(map[string]struct{})
	for id := range scan.byGraph {
		ids[id] = struct{}{}
	}
	for id := range state {
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		fmt.Fprintf(out, "trace 目录 %s 与 state 目录中没有已知的图\n", dir)
		return nil
	}
	type graphRow struct {
		id        string
		status    string
		revision  string
		events    int
		lastEvent time.Time
	}
	rows := make([]graphRow, 0, len(ids))
	for id := range ids {
		row := graphRow{id: id, status: "unknown", revision: "-"}
		eventStatus := ""
		if set := scan.byGraph[id]; set != nil {
			row.events = len(set.records)
			status, rev, hasRev, _ := graphFactsFromEvents(set.records)
			eventStatus = status
			if hasRev {
				row.revision = strconv.FormatInt(rev, 10)
			}
			for _, rec := range set.records {
				if rec.event.Timestamp.After(row.lastEvent) {
					row.lastEvent = rec.event.Timestamp
				}
			}
		}
		snapStatus := ""
		if entry, ok := state[id]; ok {
			if head, snapOK, snapErr := readGraphSnapshotHead(entry.snapshotPath, id); snapErr == nil && snapOK {
				if head.Doc != nil {
					snapStatus = head.Doc.Status
				}
				// revision 与头部一致：snapshot 可读时优先（比事件里的旧值更新）。
				row.revision = strconv.FormatInt(head.Revision, 10)
				if row.lastEvent.IsZero() {
					if info, serr := os.Stat(entry.snapshotPath); serr == nil {
						row.lastEvent = info.ModTime()
					}
				}
			}
		}
		row.status = resolveGraphStatus(eventStatus, snapStatus)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].lastEvent.Equal(rows[j].lastEvent) {
			return rows[i].lastEvent.After(rows[j].lastEvent)
		}
		return rows[i].id < rows[j].id
	})

	fmt.Fprintln(out, "GRAPH                              STATUS       REV   EVENTS  LAST EVENT")
	for _, row := range rows {
		last := "-"
		if !row.lastEvent.IsZero() {
			last = row.lastEvent.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(out, "%-34s %-12s %-5s %-7d %s\n",
			fitColumn(row.id, 34), row.status, row.revision, row.events, last)
	}
	fmt.Fprintf(out, "\n共 %d 张图，trace 目录: %s\n", len(rows), dir)
	return nil
}

// cmdGraphNode 实现 agentgo trace node <graph_id>/<node_id>：单节点视图，
// 事件按 activation 分组（回边重进 = 新 activation，一目了然）。
// graph_id 自身可能含 "/"（子图），node_id 不含（validate.go idCharset），
// 因此在最后一个 "/" 处切分。
func cmdGraphNode(dir, graphStateDir, arg string, out io.Writer) error {
	i := strings.LastIndexByte(arg, '/')
	if i <= 0 || i == len(arg)-1 {
		return fmt.Errorf("usage: agentgo trace node <graph_id>/<node_id>（子图的 graph_id 自身含 /，命令在最后一个 / 处切分）")
	}
	graphQuery, nodeID := arg[:i], arg[i+1:]
	id, scan, set, entry, err := resolveSingleGraph(dir, graphStateDir, graphQuery, out)
	if err != nil || id == "" {
		return err
	}
	header := buildGraphHeader(id, set, entry, scan)
	printGraphHeader(out, header)
	fmt.Fprintf(out, " Node: %s\n", nodeID)

	var nodeRecords []traceEventRecord
	if set != nil {
		for _, rec := range set.records {
			if rec.event.NodeID == nodeID {
				nodeRecords = append(nodeRecords, rec)
			}
		}
	}
	if len(nodeRecords) == 0 {
		fmt.Fprintf(out, "（图 %s 中没有节点 %s 的事件）\n", id, nodeID)
		return nil
	}

	// 按 activation 分组，保持首次出现顺序；无 activation 的事件归兜底组。
	type actGroup struct {
		activation string
		records    []traceEventRecord
	}
	var groups []*actGroup
	byActivation := make(map[string]*actGroup)
	for _, rec := range nodeRecords {
		key := rec.event.ActivationID
		if key == "" {
			key = "(无 activation)"
		}
		g := byActivation[key]
		if g == nil {
			g = &actGroup{activation: key}
			byActivation[key] = g
			groups = append(groups, g)
		}
		g.records = append(g.records, rec)
	}
	for _, g := range groups {
		fmt.Fprintf(out, "──── activation %s（%d 事件）────\n", g.activation, len(g.records))
		printGraphTimeline(out, g.records)
	}
	fmt.Fprintln(out, "────────────────────────────────────────────────────────────────────────────────")
	fmt.Fprintf(out, " node=%s  activations=%d  events=%d\n", nodeID, len(groups), len(nodeRecords))
	return nil
}
