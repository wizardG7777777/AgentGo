// graph_worklog.go 实现上游工作记录的聚合渲染（2026-08-21 上游摘要）。
//
// 定位：下游节点与 verifier 据此感知「上游实际做了什么」——机械事实，
// 而非上游自述（声称「已修复并验证」但 read/edit/shell 全零即假成功的
// 机械探测器）。数据来自 ToolCallRecord 账本（SWE-002 清洗后工具名已可信），
// 转移结算时由 graph.Runtime 调用一次并随 EdgeInput 冻结——本文件只在
// 结算时被调用，绝不在下游任务发布时按 task_id 回查；下游只消费冻结的
// TaskContextInput。
//
// 渲染纪律：Args 一律不进摘要（可能含文件正文，既膨胀又泄漏）；工具统计
// 按次数降序至多 8 类；文件清单去重、各 ≤10 条；摘要只是事实，不是 verdict。
package bootstrap

import (
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/checkstore"
	"agentgo/internal/store"
)

// workLogToolStatCap 是工作记录中工具统计的最大类别数（按次数降序截取）。
const workLogToolStatCap = 8

// workLogFileListCap 是编辑/写入文件清单各自的最大条数。
const workLogFileListCap = 10

// newGraphWorkLogProvider 构造 graph.Runtime 的工作记录 provider：按来源
// Task ID 聚合 ToolCallRecord，渲染为压缩多行文本（首行工具统计，随后
// 编辑/写入文件清单）。任务无调用记录时返回「（无调用记录）」——它本身
// 就是强信号；查询失败返回空串（装配处跳过整行，不误报）。
type taskCheckReader interface {
	ListTask(taskID string) ([]checkstore.Record, error)
}

func newGraphWorkLogProvider(taskStore store.TaskStore, checkReaders ...taskCheckReader) func(taskID string) string {
	var checks taskCheckReader
	if len(checkReaders) > 0 {
		checks = checkReaders[0]
	}
	return func(taskID string) string {
		if taskStore == nil || taskID == "" {
			return ""
		}
		recs, err := taskStore.QueryToolCalls(taskID, "")
		if err != nil {
			return ""
		}
		var checkRecords []checkstore.Record
		if checks != nil {
			checkRecords, err = checks.ListTask(taskID)
			if err != nil {
				return ""
			}
		}
		return renderGraphWorkLogWithChecks(recs, checkRecords)
	}
}

// renderGraphWorkLog 把一组工具调用记录聚合渲染为压缩文本。纯函数，
// 与 store 解耦以便单测。
func renderGraphWorkLog(recs []store.ToolCallRecord) string {
	return renderGraphWorkLogWithChecks(recs, nil)
}

func renderGraphWorkLogWithChecks(recs []store.ToolCallRecord, checks []checkstore.Record) string {
	if len(recs) == 0 {
		if len(checks) == 0 {
			return "（无调用记录）"
		}
	}

	counts := make(map[string]int)
	successes := make(map[string]int)
	failures := make(map[string]int)
	shellNonZero := 0
	edited := make(map[string]struct{})
	written := make(map[string]struct{})
	for _, rec := range recs {
		counts[rec.ToolName]++
		if rec.Success {
			successes[rec.ToolName]++
		} else {
			failures[rec.ToolName]++
		}
		switch rec.ToolName {
		case "run_shell":
			if rec.ExitCode != nil && *rec.ExitCode != 0 {
				shellNonZero++
			}
		case "edit_file":
			if p, _ := rec.Args["path"].(string); rec.Success && p != "" {
				edited[p] = struct{}{}
			}
		case "write_file":
			if p, _ := rec.Args["path"].(string); rec.Success && p != "" {
				written[p] = struct{}{}
			}
		}
	}

	type stat struct {
		name  string
		count int
	}
	stats := make([]stat, 0, len(counts))
	for name, count := range counts {
		stats = append(stats, stat{name, count})
	}
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].count != stats[j].count {
			return stats[i].count > stats[j].count
		}
		return stats[i].name < stats[j].name
	})
	parts := make([]string, 0, len(stats)+1)
	for i, s := range stats {
		if i >= workLogToolStatCap {
			parts = append(parts, fmt.Sprintf("(+%d 类工具)", len(stats)-workLogToolStatCap))
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d(ok=%d fail=%d)",
			s.name, s.count, successes[s.name], failures[s.name]))
	}
	first := strings.Join(parts, ", ")
	if counts["run_shell"] > 0 {
		first += fmt.Sprintf(" (exit≠0: %d)", shellNonZero)
	}

	checkLine := "\n检查记录: （无）"
	if len(checks) > 0 {
		latestByID := make(map[string]checkstore.Record)
		for _, record := range checks {
			previous, exists := latestByID[record.CheckID]
			if !exists || previous.SettledAt.IsZero() || !record.SettledAt.Before(previous.SettledAt) {
				latestByID[record.CheckID] = record
			}
		}
		ids := make([]string, 0, len(latestByID))
		for id := range latestByID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		latestParts := make([]string, 0, len(ids))
		for _, id := range ids {
			latest := latestByID[id]
			latestParts = append(latestParts, fmt.Sprintf("%s/%s status=%s exit=%d workspace=%s",
				latest.CheckID, latest.Kind, latest.Status, latest.ExitCode, latest.WorkspaceRevisionRef))
		}
		checkLine = fmt.Sprintf("\n检查记录: latest_by_check_id=[%s] superseded=%d",
			strings.Join(latestParts, "; "), len(checks)-len(latestByID))
	}
	return first +
		"\n编辑文件: " + renderWorkLogFileList(edited) +
		"\n写入文件: " + renderWorkLogFileList(written) + checkLine
}

// renderWorkLogFileList 渲染去重排序后的文件清单（≤workLogFileListCap 条，
// 超出标 (+N more)；空集显示（无））。
func renderWorkLogFileList(set map[string]struct{}) string {
	if len(set) == 0 {
		return "（无）"
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	if len(files) > workLogFileListCap {
		return strings.Join(files[:workLogFileListCap], ", ") +
			fmt.Sprintf(" (+%d more)", len(files)-workLogFileListCap)
	}
	return strings.Join(files, ", ")
}
