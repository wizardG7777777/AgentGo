package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/pathutil"
	"agentgo/internal/tools/schema"
)

// LocalReadGroup 提供只读的本地文件系统工具集合：
//   - read_file：按行切片读取文件并返回 content_hash
//   - list_dir ：列出目录内容，可递归成树形
//   - grep_search：在目录中搜索包含指定模式的行
//   - glob_search：按 glob 模式（支持 **）查找文件
//
// Workdir 必须非空；Cache 可选——为 nil 时禁用缓存命中逻辑。
type LocalReadGroup struct {
	Workdir WorkdirProvider       // required
	Cache   *agent.FileStateCache // optional
}

// Register 把四个只读工具注册到 r。
func (g LocalReadGroup) Register(r *agent.ToolRegistry) {
	r.Register("read_file", "读取文件内容，支持按行切片",
		schema.Object().
			String("path", "文件路径", true).
			Int("offset", "起始行号（1-based），可选；不传则从文件开头读", false).
			Int("limit", "读取行数上限，可选；不传则读到文件末尾或字符上限", false).
			Build(),
		g.readFile,
	)

	r.Register("list_dir", "列出目录内容，可选递归深度",
		schema.Object().
			String("path", "目录路径", true).
			Int("depth", "递归深度，默认 1（仅当前目录）；>1 时输出树形", false).
			Build(),
		g.listDir,
	)

	r.Register("grep_search", "在目录内按文本模式搜索匹配行",
		schema.Object().
			String("pattern", "搜索的文本模式", true).
			String("path", "搜索的目录或文件路径", true).
			Int("max_lines", "最大返回行数，默认 50", false).
			Build(),
		g.grepSearch,
	)

	r.Register("glob_search", "按 glob 模式递归查找文件（支持 **/*_test.go）",
		schema.Object().
			String("pattern", "glob 模式，如 **/*_test.go", true).
			String("root_dir", "搜索根目录，默认当前目录", false).
			Build(),
		g.globSearch,
	)
}

// computeSHA256 计算 data 的 SHA256 哈希并返回十六进制摘要字符串。
func computeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// toInt 从 map[string]any 中安全读取一个 int。支持 float64/int/int64 兼容 JSON 解析。
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// readFile 实现 read_file 工具。
//
// 输出格式（自描述头部）：
//
//	[file] <path> (lines <start>-<end> of <total>)
//	[hash] <sha256>
//	---
//	<content>
//
// 自描述头部让 LLM 即使在历史压缩之后看到 tool result，仍然知道：
//   - 自己读了哪个文件
//   - 读到的是哪段行范围（是不是已经读到末尾）
//   - 内容是否被截断
//
// 这样可以减少 LLM 重复翻页和误判文件内容的情况。
func (g LocalReadGroup) readFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("缺少 path 参数")
	}
	projectRoot := ""
	if g.Workdir != nil {
		projectRoot = g.Workdir.Get()
	}
	if projectRoot != "" {
		validPath, err := pathutil.ValidatePath(path, projectRoot)
		if err != nil {
			return "", err
		}
		path = validPath
	}

	offset, hasOffset := toInt(args["offset"])
	limit, hasLimit := toInt(args["limit"])

	// 缓存命中检查（缓存的是完整格式化内容 + hash；切片参数不参与缓存键）
	if g.Cache != nil && !hasOffset && !hasLimit {
		if content, hash, ok := g.Cache.Get(path); ok {
			// 缓存中存的是已经包含 content_hash 后缀的旧格式内容；
			// 为了头部信息一致，从缓存读出后重新构造头部。
			// 简化处理：缓存命中直接返回旧格式 + 简单头
			return formatReadFileResult(path, content, hash, 1, -1, -1, false), nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	hash := computeSHA256(data)
	content := string(data)

	// 计算总行数（用于头部信息显示）
	totalLines := strings.Count(content, "\n")
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		totalLines++ // 最后一行无 \n 结尾也算一行
	}

	// 行切片
	startLine := 1
	endLine := totalLines
	if hasOffset || hasLimit {
		lines := strings.Split(content, "\n")
		total := len(lines)
		if offset <= 0 {
			offset = 1
		}
		if offset > total {
			content = fmt.Sprintf("(offset %d 超出文件总行数 %d)", offset, total)
			startLine = offset
			endLine = offset
		} else {
			start := offset - 1
			end := total
			if hasLimit && limit > 0 {
				if start+limit < end {
					end = start + limit
				}
			}
			content = strings.Join(lines[start:end], "\n")
			startLine = offset
			endLine = end
		}
	}

	// 10000 字符截断（在切片之后）
	truncated := false
	if len(content) > 10000 {
		content = content[:10000] + "\n... (截断，文件过大)"
		truncated = true
	}

	// 写入缓存（仅对完整读取的结果）
	if g.Cache != nil && !hasOffset && !hasLimit {
		g.Cache.Put(path, content, hash)
	}

	return formatReadFileResult(path, content, hash, startLine, endLine, totalLines, truncated), nil
}

// formatReadFileResult 构造 read_file 工具的标准输出格式。
// 头部含路径、行范围、总行数、hash，让 LLM 一眼判断"还需不需要继续读"。
func formatReadFileResult(path, content, hash string, startLine, endLine, totalLines int, truncated bool) string {
	var sb strings.Builder
	sb.WriteString("[file] ")
	sb.WriteString(path)

	if totalLines >= 0 {
		// 完整模式：显示行范围 + 总行数
		if startLine == 1 && endLine == totalLines {
			sb.WriteString(fmt.Sprintf(" (%d lines, full)", totalLines))
		} else {
			sb.WriteString(fmt.Sprintf(" (lines %d-%d of %d)", startLine, endLine, totalLines))
		}
	}
	if truncated {
		sb.WriteString(" [truncated to 10000 chars]")
	}
	sb.WriteString("\n[hash] ")
	sb.WriteString(hash)
	sb.WriteString("\n---\n")
	sb.WriteString(content)
	return sb.String()
}

// listDir 实现 list_dir 工具。
func (g LocalReadGroup) listDir(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	projectRoot := ""
	if g.Workdir != nil {
		projectRoot = g.Workdir.Get()
	}
	if projectRoot != "" {
		validPath, err := pathutil.ValidatePath(path, projectRoot)
		if err != nil {
			return "", err
		}
		path = validPath
	}

	depth, _ := toInt(args["depth"])
	if depth <= 0 {
		depth = 1
	}
	if depth > 5 {
		depth = 5
	}

	if depth == 1 {
		entries, err := os.ReadDir(path)
		if err != nil {
			return "", fmt.Errorf("读取目录失败: %w", err)
		}
		var sb strings.Builder
		for _, entry := range entries {
			if entry.IsDir() {
				sb.WriteString(fmt.Sprintf("[目录] %s/\n", entry.Name()))
			} else {
				sb.WriteString(fmt.Sprintf("[文件] %s\n", entry.Name()))
			}
		}
		return sb.String(), nil
	}

	// depth > 1：树形输出
	var sb strings.Builder
	if err := writeTree(&sb, path, 0, depth); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// writeTree 递归写入树形目录结构。level 从 0 开始。跳过以 "." 开头的隐藏条目。
func writeTree(sb *strings.Builder, dir string, level, maxDepth int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读取目录失败: %w", err)
	}
	// os.ReadDir 已按名字排序，但显式 sort 一次以防未来变动。
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	indent := strings.Repeat("  ", level)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			sb.WriteString(fmt.Sprintf("%s[目录] %s/\n", indent, name))
			if level+1 < maxDepth {
				if err := writeTree(sb, filepath.Join(dir, name), level+1, maxDepth); err != nil {
					return err
				}
			}
		} else {
			sb.WriteString(fmt.Sprintf("%s[文件] %s\n", indent, name))
		}
	}
	return nil
}

// grepSearch 实现 grep_search 工具。
func (g LocalReadGroup) grepSearch(ctx context.Context, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	searchPath, _ := args["path"].(string)
	if pattern == "" || searchPath == "" {
		return "", fmt.Errorf("缺少 pattern 或 path 参数")
	}
	projectRoot := ""
	if g.Workdir != nil {
		projectRoot = g.Workdir.Get()
	}
	if projectRoot != "" {
		validPath, err := pathutil.ValidatePath(searchPath, projectRoot)
		if err != nil {
			return "", err
		}
		searchPath = validPath
	}

	maxLines := 50
	if n, ok := toInt(args["max_lines"]); ok && n > 0 {
		maxLines = n
	}

	var results []string
	_ = filepath.Walk(searchPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") || info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if len(results) >= maxLines {
				return filepath.SkipAll
			}
			if strings.Contains(line, pattern) {
				results = append(results, fmt.Sprintf("%s:%d: %s", path, i+1, line))
			}
		}
		return nil
	})

	if len(results) == 0 {
		return "未找到匹配项", nil
	}
	return strings.Join(results, "\n"), nil
}

// globSearch 实现 glob_search 工具。
func (g LocalReadGroup) globSearch(ctx context.Context, args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", fmt.Errorf("缺少 pattern 参数")
	}
	projectRoot := ""
	if g.Workdir != nil {
		projectRoot = g.Workdir.Get()
	}

	rootDir, _ := args["root_dir"].(string)
	if rootDir == "" && projectRoot != "" {
		rootDir = projectRoot
	}
	if rootDir != "" && projectRoot != "" {
		validPath, err := pathutil.ValidatePath(rootDir, projectRoot)
		if err != nil {
			return "", err
		}
		rootDir = validPath
	}
	if rootDir == "" {
		rootDir = "."
	}

	return toolGlobSearch(ctx, pattern, rootDir)
}

// toolGlobSearch 递归遍历 rootDir，返回匹配 pattern 的文件相对路径列表。
// 支持 ** 递归通配，跳过隐藏目录，结果上限 200 条。
func toolGlobSearch(ctx context.Context, pattern, rootDir string) (string, error) {
	info, err := os.Stat(rootDir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("目录不存在: %s", rootDir)
	}

	const resultLimit = 200
	var matches []string
	totalMatched := 0

	_ = filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && strings.HasPrefix(info.Name(), ".") && info.Name() != "." {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		relPath, relErr := filepath.Rel(rootDir, path)
		if relErr != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		matched, matchErr := matchGlob(pattern, relPath)
		if matchErr != nil {
			return nil
		}
		if matched {
			totalMatched++
			if len(matches) < resultLimit {
				matches = append(matches, relPath)
			}
		}
		return nil
	})

	if totalMatched == 0 {
		return "未找到匹配文件", nil
	}
	result := strings.Join(matches, "\n")
	if totalMatched > resultLimit {
		result += fmt.Sprintf("\n... 结果已截断，共匹配 %d 个文件，仅显示前 200 个", totalMatched)
	}
	return result, nil
}

// matchGlob 判断文件的相对路径是否匹配 glob 模式。
// 当模式包含 ** 时按 segments 匹配路径组件；否则仅匹配文件名。
func matchGlob(pattern, relPath string) (bool, error) {
	if !strings.Contains(pattern, "**") {
		filename := filepath.Base(relPath)
		return filepath.Match(pattern, filename)
	}

	segments := strings.Split(pattern, "**")
	parts := strings.Split(filepath.ToSlash(relPath), "/")

	prefix := strings.Trim(segments[0], "/")
	suffix := strings.Trim(segments[len(segments)-1], "/")

	if prefix != "" {
		prefixParts := strings.Split(prefix, "/")
		if len(prefixParts) > len(parts) {
			return false, nil
		}
		for i, pp := range prefixParts {
			matched, err := filepath.Match(pp, parts[i])
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
		parts = parts[len(prefixParts):]
	}

	if suffix != "" {
		suffixParts := strings.Split(suffix, "/")
		if len(suffixParts) > len(parts) {
			return false, nil
		}
		for i := 0; i < len(suffixParts); i++ {
			matched, err := filepath.Match(suffixParts[len(suffixParts)-1-i], parts[len(parts)-1-i])
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
		parts = parts[:len(parts)-len(suffixParts)]
	}

	for i := 1; i < len(segments)-1; i++ {
		mid := strings.Trim(segments[i], "/")
		if mid == "" {
			continue
		}
		midParts := strings.Split(mid, "/")
		found := false
		for start := 0; start <= len(parts)-len(midParts); start++ {
			allMatch := true
			for j, mp := range midParts {
				matched, err := filepath.Match(mp, parts[start+j])
				if err != nil {
					return false, err
				}
				if !matched {
					allMatch = false
					break
				}
			}
			if allMatch {
				parts = parts[start+len(midParts):]
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}

	return true, nil
}
