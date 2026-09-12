package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/pathutil"
	"agentgo/internal/suggest"
	"agentgo/internal/tools/hashline"
	"agentgo/internal/tools/schema"
)

// LocalReadGroup 提供只读的本地文件系统工具集合：
//   - read_file：按行切片读取文件并返回 content_hash
//
// Workdir 必须非空；Cache 可选——为 nil 时禁用缓存命中逻辑。
// HashlineEnabled 控制 read_file 输出是否附加行哈希前缀（§7）。
type LocalReadGroup struct {
	Workdir         WorkdirProvider       // required
	Cache           *agent.FileStateCache // optional
	HashlineEnabled bool                  // §7：默认 false，启动时由 cfg 注入
}

const readFileOutputMaxChars = 64 << 10

// Register 把四个只读工具注册到 r。
func (g LocalReadGroup) Register(r *agent.ToolRegistry) {
	r.Register("read_file", "读取文件内容，支持按行切片。默认输出每行带有行哈希前缀（如 1#VK|content），可用作 apply_change 的 line_anchors 锚点。",
		schema.Object().
			String("path", "文件路径", true).
			Int("offset", "起始行号（1-based），可选；不传则从文件开头读", false).
			Int("limit", "读取行数上限，可选；大文件建议分页。单次输出上限 64 Ki 字符，结果正文直接交付模型，不替换为引用", false).
			Build(),
		g.readFile,
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
	if _, retired := args["force_full"]; retired {
		return "", fmt.Errorf("force_full 已退役；read_file 始终返回所选范围的正文")
	}
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
	logicalPath := path
	// 按任务写时复制隔离：Workdir 同时实现 PathOverlayer 时（runner 装配的
	// workspace.Swapper），把主根逻辑路径解析为物理读取位置——workspace 中
	// 已有本任务写过的副本则读副本，否则读该视图的输入基线；无隔离时 passthrough
	// （零开销）。path 在此之后即为物理路径，FileStateCache 的 Get/Put 统一以
	// 它为键，保证读侧缓存键与写侧 Invalidate 键一致。
	if ov, ok := g.Workdir.(PathOverlayer); ok {
		path = ov.ReadPath(path)
	}

	offset, hasOffset := toInt(args["offset"])
	limit, hasLimit := toInt(args["limit"])

	data, err := os.ReadFile(path)
	if err != nil {
		// 物理位置只用于 I/O；模型必须能够把回显路径直接交给后续文件工具。
		if pathErr, ok := err.(*os.PathError); ok {
			err = &os.PathError{Op: pathErr.Op, Path: logicalPath, Err: pathErr.Err}
		}
		// §10 Did-You-Mean：路径不存在时，列父目录的 basename 作为候选。
		// 不跨目录提示——避免 internal/foo/x.go 不存在时建议 cmd/bar/x.go 误导
		// （详见 nextUpgrade_v4.md §10.4 read_file 候选构造范围）。
		if os.IsNotExist(err) {
			suggestion := buildPathDidYouMean(path)
			return "", fmt.Errorf("读取文件失败: %w%s", err, suggestion)
		}
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	hash := computeSHA256(data)
	// 展示层 CRLF 归一化：hash 仍按磁盘原始字节计算（expected_hash 乐观锁语义
	// 不变），LLM 看到的内容统一为 LF（2026-07-21 跨平台排查 M4）。
	content, crlfNormalized := normalizeCRLF(string(data))
	crlfNote := ""
	if crlfNormalized {
		crlfNote = "CRLF→LF 已归一化展示，磁盘文件行尾不变"
	}

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

	// 64 Ki 字符安全上限（在切片之后）；不再用旧 10k 阈值制造高频重读。
	truncated := false
	if len(content) > readFileOutputMaxChars {
		origChars := len(content)
		// 带元数据的截断公告（2026-07-22 闸 2，参考 Codex "Warning: truncated
		// (original token count: N)"）：给出本段原大小与续读行号，让模型用
		// offset 精确续读，而不是重读全文或凭猜测续翻页。
		nextLine := strings.Count(content[:readFileOutputMaxChars], "\n") + 1
		if hasOffset || hasLimit {
			nextLine += startLine - 1
		}
		content = content[:readFileOutputMaxChars] + fmt.Sprintf("\n... [已截断：本段原 %d 字符，仅显示前 %d；用 offset=%d 续读]", origChars, readFileOutputMaxChars, nextLine)
		truncated = true
	}

	// 写入缓存（仅对完整读取的结果）
	if g.Cache != nil && !hasOffset && !hasLimit {
		g.Cache.Put(path, content, hash)
	}

	if g.HashlineEnabled {
		content = hashline.FormatHashLines(startLine, content)
	}
	return formatReadFileResult(logicalPath, content, hash, startLine, endLine, totalLines, truncated, crlfNote), nil
}

// formatReadFileResult 构造 read_file 工具的标准输出格式。
// 头部含路径、行范围、总行数、hash，让 LLM 一眼判断"还需不需要继续读"。
// note 为非空时以方括号附注形式追加到头部（如 CRLF 归一化提示）。
func formatReadFileResult(path, content, hash string, startLine, endLine, totalLines int, truncated bool, note string) string {
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
		sb.WriteString(fmt.Sprintf(" [truncated to %d chars]", readFileOutputMaxChars))
	}
	if note != "" {
		sb.WriteString(" [")
		sb.WriteString(note)
		sb.WriteString("]")
	}
	sb.WriteString("\n[hash] ")
	sb.WriteString(hash)
	sb.WriteString("\n---\n")
	sb.WriteString(content)
	return sb.String()
}

// buildPathDidYouMean 在 read_file 等以路径为参数的工具失败（路径不存在）时，
// 返回 "\n\nDid you mean: ..." 文本段。候选 = 父目录的 ReadDir 列表（§10.4）。
// 父目录也不存在 / 读取失败时返回空串——降级为单纯的 IsNotExist 错误，无误导。
func buildPathDidYouMean(failedPath string) string {
	parent := filepath.Dir(failedPath)
	if parent == "" || parent == failedPath {
		return ""
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	candidates := make([]string, 0, len(entries))
	for _, e := range entries {
		candidates = append(candidates, e.Name())
	}
	hits := suggest.Suggest(filepath.Base(failedPath), candidates, 3)
	return suggest.FormatForToolMessage(hits)
}
