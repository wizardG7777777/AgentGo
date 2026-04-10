// scheduler_probe.go 实现 Scheduler 专属的目录探针工具。
// 与 scheduler.go 中的编排工具（cancel_task / report_done）分离，
// 便于后续扩展更多探针（count_files / file_summary 等）。
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentgo/internal/pathutil"
)

// probeDirectory 探测指定目录的完整结构，返回树状目录（含文件大小）、
// 文件类型分布和统计综述。Scheduler 专属，用于任务规划前了解工作区全貌。
func (g SchedulerGroup) probeDirectory(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	if g.ProjectRoot != "" {
		validPath, err := pathutil.ValidatePath(path, g.ProjectRoot)
		if err != nil {
			return "", err
		}
		path = validPath
	}

	depth, _ := toInt(args["depth"])
	if depth <= 0 {
		depth = 3
	}
	if depth > 10 {
		depth = 10
	}

	stats := &probeStats{extCount: make(map[string]int)}
	var tree strings.Builder
	probeWalkTree(&tree, stats, path, 0, depth, 500)

	// 构建输出：综述 + 类型分布 + 树
	var out strings.Builder
	absPath, _ := filepath.Abs(path)
	fmt.Fprintf(&out, "[综述] 根目录: %s | 文件夹: %d | 文件: %d | 总大小: %s\n",
		absPath, stats.dirCount, stats.fileCount, formatSize(stats.totalSize))
	out.WriteString("[类型分布] ")
	out.WriteString(stats.formatTypeDistribution())
	out.WriteString("\n\n")
	out.WriteString(tree.String())

	return out.String(), nil
}

// probeStats 收集目录探测过程中的统计数据。
type probeStats struct {
	dirCount  int
	fileCount int
	totalSize int64
	extCount  map[string]int // ".go" → 31
	rendered  int            // 已渲染到树的文件/目录条目数（用于上限控制）
}

// formatTypeDistribution 把 extCount 按数量降序格式化，超过 5 种时尾部合并为"其他"。
func (s *probeStats) formatTypeDistribution() string {
	if len(s.extCount) == 0 {
		return "无文件"
	}
	type extEntry struct {
		ext   string
		count int
	}
	sorted := make([]extEntry, 0, len(s.extCount))
	for ext, count := range s.extCount {
		sorted = append(sorted, extEntry{ext, count})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })

	var parts []string
	otherCount := 0
	for i, e := range sorted {
		if i >= 5 {
			otherCount += e.count
			continue
		}
		pct := 0
		if s.fileCount > 0 {
			pct = e.count * 100 / s.fileCount
		}
		parts = append(parts, fmt.Sprintf("%s: %d (%d%%)", e.ext, e.count, pct))
	}
	if otherCount > 0 {
		pct := otherCount * 100 / s.fileCount
		parts = append(parts, fmt.Sprintf("其他: %d (%d%%)", otherCount, pct))
	}
	return strings.Join(parts, " | ")
}

// probeWalkTree 递归遍历目录，同时构建树状输出和收集统计数据。
// maxEntries 控制树状输出的条目上限（综述统计不受此限制）。
func probeWalkTree(sb *strings.Builder, stats *probeStats, dir string, level, maxDepth, maxEntries int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	indent := strings.Repeat("  ", level)

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}

		if entry.IsDir() {
			stats.dirCount++
			if stats.rendered < maxEntries {
				fmt.Fprintf(sb, "%s%s/\n", indent, name)
				stats.rendered++
			}
			if level+1 < maxDepth {
				probeWalkTree(sb, stats, filepath.Join(dir, name), level+1, maxDepth, maxEntries)
			}
		} else {
			stats.fileCount++
			ext := strings.ToLower(filepath.Ext(name))
			if ext == "" {
				ext = "(无扩展名)"
			}
			stats.extCount[ext]++

			info, err := entry.Info()
			size := int64(0)
			if err == nil {
				size = info.Size()
			}
			stats.totalSize += size

			if stats.rendered < maxEntries {
				// 对齐文件大小：名称左对齐，大小右对齐
				padding := 40 - len(indent) - len(name)
				if padding < 2 {
					padding = 2
				}
				fmt.Fprintf(sb, "%s%s%s%s\n", indent, name, strings.Repeat(" ", padding), formatSize(size))
				stats.rendered++
			}
		}
	}
}

// formatSize 把字节数格式化为人类可读的字符串。
func formatSize(bytes int64) string {
	switch {
	case bytes >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
