package tools

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	LocalReadGroup                // embed: 继承 Workdir + Cache
	Roster         roster.Roster  // required
	AgentID        string         // required
	WaitTimeoutSec int            // §8.3：文件冲突排队等待秒数，0 = 不排队（旧行为）
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

// claimOrWait 尝试 TryClaim；失败时排队等待前任释放后重试一次。
// 返回 nil 表示声明成功（调用方需 defer Release）。
// 返回 error 表示最终失败（含占用者信息）。
// LLM 感知不到排队——阻塞发生在工具函数内部，对 LLM 而言只是工具调用耗时变长。
func (g LocalWriteGroup) claimOrWait(ctx context.Context, path, verb string) error {
	claimed, err := g.Roster.TryClaim(g.AgentID, path)
	if err != nil {
		return fmt.Errorf("文件锁声明失败: %w", err)
	}
	if claimed {
		return nil
	}

	// 首次声明失败——文件被占用
	occupiedBy, _, _ := g.Roster.IsOccupied(path)

	timeout := time.Duration(g.WaitTimeoutSec) * time.Second
	if timeout <= 0 {
		return fmt.Errorf("文件 %s 正被代理 %s 占用，无法%s", path, occupiedBy, verb)
	}

	// Trace：入队事件
	trace.Emit(trace.Event{
		Kind:        trace.KindFileWriteQueued,
		TaskID:      agent.TaskIDFromContext(ctx),
		AgentID:     g.AgentID,
		Path:        path,
		Description: fmt.Sprintf("等待 %s 释放文件", occupiedBy),
	})

	start := time.Now()
	waitErr := g.Roster.WaitForRelease(ctx, g.AgentID, path, timeout)
	waitDuration := time.Since(start)

	if waitErr != nil {
		// 超时或 ctx 取消
		return fmt.Errorf("文件 %s 正被代理 %s 占用（等待 %dms 后超时），无法%s",
			path, occupiedBy, waitDuration.Milliseconds(), verb)
	}

	// 被唤醒，重试一次 TryClaim
	claimed, err = g.Roster.TryClaim(g.AgentID, path)
	if err != nil {
		return fmt.Errorf("文件锁声明失败: %w", err)
	}
	if !claimed {
		occupiedBy, _, _ = g.Roster.IsOccupied(path)
		return fmt.Errorf("文件 %s 正被代理 %s 占用（排队唤醒后被抢先），无法%s", path, occupiedBy, verb)
	}

	log.Printf("[roster] %s 排队等待文件 %s 成功（等待 %dms）", g.AgentID, path, waitDuration.Milliseconds())

	// Trace：排队结束，记录实际等待耗时
	trace.Emit(trace.Event{
		Kind:    trace.KindFileWriteQueued,
		TaskID:  agent.TaskIDFromContext(ctx),
		AgentID: g.AgentID,
		Path:    path,
		WaitMS:  waitDuration.Milliseconds(),
		Description: "排队等待结束，成功获得文件锁",
	})

	return nil
}

// writeFile 实现 write_file 工具。端口自 worker.makeWriteFileTool。
// 严格顺序：validate → claimOrWait → (defer Release) → MkdirAll → WriteFile → 缓存失效。
// 注：expected_hash 校验在 C7 后由 ValidateExpectedHashHook 接管，不再在工具内部读取。
func (g LocalWriteGroup) writeFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
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

	// §8.3：通过 claimOrWait 声明文件写入权——冲突时排队等待前任释放
	if err := g.claimOrWait(ctx, path, "写入"); err != nil {
		return "", err
	}
	defer g.Roster.Release(g.AgentID, path)

	// C7 迁移：原 expected_hash 校验段已删除。
	// 乐观并发控制由 ValidateExpectedHashHook（PreCall, prio=20）接管。
	// 决策 B1：接受微小 TOCTOU 窗口（hook 校验在 Roster 锁外）。

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
// 注：expected_hash 校验在 C7 后由 ValidateExpectedHashHook 接管。
func (g LocalWriteGroup) editFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_str"].(string)
	newStr, _ := args["new_str"].(string)

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

	// §8.3：通过 claimOrWait 声明文件写入权——冲突时排队等待前任释放
	if err := g.claimOrWait(ctx, path, "编辑"); err != nil {
		return "", err
	}
	defer g.Roster.Release(g.AgentID, path)

	// 读取文件（锁持有期间）
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("文件不存在: %s", path)
	}

	// C7 迁移：原 expected_hash 校验段已删除。
	// 由 ValidateExpectedHashHook 在 PreCall 阶段接管（决策 B1：接受微小 TOCTOU）。

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
