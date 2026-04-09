package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/pathutil"
	"agentgo/internal/roster"
	"agentgo/internal/tools/schema"
	"agentgo/internal/trace"
)

// LocalWriteGroup 提供会修改本地文件系统的工具集合：
//   - write_file：整文件写入，支持可选的乐观并发 hash 校验
//   - edit_file ：精准 old_str -> new_str 单次替换
//
// 通过嵌入 LocalReadGroup 继承 Workdir 与 Cache 依赖，
// 保持与只读工具共用的 workdir 解析和缓存失效语义一致。
//
// 两个工具都在调用 Roster.TryClaim 获取文件写入权之后才读取文件内容，
// 严格遵循「先锁后读」的顺序，避免 TOCTOU 竞态。
//
// C5 迁移：原 Store / ProjectRoot 字段以及 recordArtifact 方法已删除。
// 写入产物事实流的登记由 Hook System 的 RecordArtifactHook 在 PostCall
// 阶段接管，详见 internal/hook/builtin/record_artifact.go。
type LocalWriteGroup struct {
	LocalReadGroup               // embed: 继承 Workdir + Cache
	Roster         roster.Roster // required
	AgentID        string        // required
}

// Register 把 write_file / edit_file 注册到 r。
func (g LocalWriteGroup) Register(r *agent.ToolRegistry) {
	r.Register("write_file", "写入文件内容（覆盖式），支持可选的乐观并发 hash 校验",
		schema.Object().
			String("path", "文件路径", true).
			String("content", "要写入的内容", true).
			String("expected_hash", "期望的当前文件 SHA256 哈希；若提供且与实际不符则拒绝写入（用于乐观并发控制）", false).
			Build(),
		g.writeFile,
	)

	r.Register("edit_file", "在文件中做精准的 old_str -> new_str 单次替换",
		schema.Object().
			String("path", "文件路径", true).
			String("old_str", "要替换的旧字符串（必须在文件中唯一匹配）", true).
			String("new_str", "替换后的新字符串", true).
			String("expected_hash", "期望的当前文件 SHA256 哈希", false).
			Build(),
		g.editFile,
	)
}

// writeFile 实现 write_file 工具。端口自 worker.makeWriteFileTool。
// 严格顺序：validate → TryClaim → (defer Release) → ReadFile → hash 校验 → MkdirAll → WriteFile → 缓存失效。
func (g LocalWriteGroup) writeFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	expectedHash, _ := args["expected_hash"].(string)
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

	// 通过 Roster 声明文件写入权——必须在任何文件读取之前
	claimed, err := g.Roster.TryClaim(g.AgentID, path)
	if err != nil {
		return "", fmt.Errorf("文件锁声明失败: %w", err)
	}
	if !claimed {
		occupiedBy, _, _ := g.Roster.IsOccupied(path)
		return "", fmt.Errorf("文件 %s 正被代理 %s 占用，无法写入", path, occupiedBy)
	}
	defer g.Roster.Release(g.AgentID, path)

	// 乐观并发校验：若提供了 expected_hash 且文件已存在，校验哈希一致性
	if expectedHash != "" {
		existing, readErr := os.ReadFile(path)
		if readErr == nil {
			currentHash := computeSHA256(existing)
			if currentHash != expectedHash {
				return "", fmt.Errorf("写入冲突：文件 %s 的内容已被其他代理修改（期望哈希 %s，当前哈希 %s）。请重新调用 read_file 获取最新内容后再试", path, expectedHash, currentHash)
			}
		}
		// 若文件不存在（os.IsNotExist 或其他），跳过校验，允许新建
	}

	// 确保父目录存在
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	// 写入后使缓存失效
	if g.Cache != nil {
		g.Cache.Invalidate(path)
	}

	// Trace：file_written 事件（可审计的落盘记录）
	trace.Emit(trace.Event{
		Kind:    trace.KindFileWritten,
		TaskID:  agent.TaskIDFromContext(ctx),
		AgentID: g.AgentID,
		Tool:    "write_file",
		Path:    path,
		Bytes:   len(content),
		Hash:    computeSHA256([]byte(content)),
	})

	// Artifacts：C5 迁移后由 RecordArtifactHook（PostCall）记录到 task.Artifacts。
	// 详见 internal/hook/builtin/record_artifact.go。

	return fmt.Sprintf("文件已写入: %s (%d 字节)", path, len(content)), nil
}

// editFile 实现 edit_file 工具。端口自 worker.makeEditFileTool。
// 读取、匹配计数、替换写入三步在同一个 Roster 锁持有期间完成。
func (g LocalWriteGroup) editFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_str"].(string)
	newStr, _ := args["new_str"].(string)
	expectedHash, _ := args["expected_hash"].(string)

	if path == "" {
		return "", fmt.Errorf("缺少 path 参数")
	}
	if oldStr == "" {
		return "", fmt.Errorf("缺少 old_str 参数")
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

	// 通过 Roster 声明文件写入权——必须在任何文件读取之前
	claimed, err := g.Roster.TryClaim(g.AgentID, path)
	if err != nil {
		return "", fmt.Errorf("文件锁声明失败: %w", err)
	}
	if !claimed {
		occupiedBy, _, _ := g.Roster.IsOccupied(path)
		return "", fmt.Errorf("文件 %s 正被代理 %s 占用，无法编辑", path, occupiedBy)
	}
	defer g.Roster.Release(g.AgentID, path)

	// 读取文件（锁持有期间）
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("文件不存在: %s", path)
	}

	// 乐观并发校验
	if expectedHash != "" {
		currentHash := computeSHA256(data)
		if currentHash != expectedHash {
			return "", fmt.Errorf("编辑冲突：文件 %s 的内容已被其他代理修改（期望哈希 %s，当前哈希 %s）。请重新调用 read_file 获取最新内容后再试", path, expectedHash, currentHash)
		}
	}

	content := string(data)

	// 计数匹配
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", fmt.Errorf("未找到匹配内容，old_str 在文件中不存在")
	}
	if count > 1 {
		return "", fmt.Errorf("匹配到 %d 处，请提供更精确的 old_str", count)
	}

	// 执行替换
	newContent := strings.Replace(content, oldStr, newStr, 1)

	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	// 写入后使缓存失效
	if g.Cache != nil {
		g.Cache.Invalidate(path)
	}

	// Trace：file_written 事件（edit 也算一次落盘）
	trace.Emit(trace.Event{
		Kind:    trace.KindFileWritten,
		TaskID:  agent.TaskIDFromContext(ctx),
		AgentID: g.AgentID,
		Tool:    "edit_file",
		Path:    path,
		Bytes:   len(newContent),
		Hash:    computeSHA256([]byte(newContent)),
	})

	// Artifacts：C5 迁移后由 RecordArtifactHook（PostCall）记录到 task.Artifacts。

	oldLen := len(content)
	newLen := len(newContent)
	added := 0
	removed := 0
	if newLen > oldLen {
		added = newLen - oldLen
	} else {
		removed = oldLen - newLen
	}

	return fmt.Sprintf("文件已编辑: %s (字节变化: +%d/-%d)", path, added, removed), nil
}
