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
	"agentgo/internal/effect"
	"agentgo/internal/model"
	"agentgo/internal/pathutil"
	"agentgo/internal/roster"
	"agentgo/internal/tools/hashline"
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
// ArtifactStore 是写工具的正确性依赖：任务内 write/edit 在返回成功前
// 必须把产物事实同步登记。record-artifact Async Reactor 仍作兼容观察器，
// 但不再是 task.Artifacts / Graph artifact Evidence 的正确性权威。
type LocalWriteGroup struct {
	LocalReadGroup               // embed: 继承 Workdir + Cache
	Roster         roster.Roster // required
	AgentID        string        // required
	ArtifactStore  interface {
		AppendArtifactWithMeta(taskID string, path string, meta model.ArtifactMeta) error
	}
	WaitTimeoutSec int // §8.3：文件冲突排队等待秒数，0 = 不排队（旧行为）
	// EffectJournal 是 V6 §4 H2b 副作用账本（internal/effect）；
	// nil 时 write_file/edit_file 不记账（行为与引入账本前完全一致）。
	EffectJournal *effect.Journal
}

// requireArtifactLedger 在产生文件副作用前确认任务级 ledger 已装配。
// 无 task ID 表示工具在脱离 Agent 任务的局部调用中运行（主要用于
// 工具单测），没有可关联的 task.Artifacts，因此保持原行为。
func (g LocalWriteGroup) requireArtifactLedger(ctx context.Context) error {
	if agent.TaskIDFromContext(ctx) != "" && g.ArtifactStore == nil {
		return fmt.Errorf("artifact ledger 未装配：拒绝在无法登记产物证据时写入文件")
	}
	return nil
}

// recordArtifact 在写工具返回前同步、幂等地登记产物事实。
// logicalPath 是主根账目坐标；即使文件实际落在 workspace overlay，
// Graph 下游也应消费稳定的项目相对路径。
func (g LocalWriteGroup) recordArtifact(ctx context.Context, logicalPath string, content []byte) error {
	taskID := agent.TaskIDFromContext(ctx)
	if taskID == "" {
		return nil
	}
	if g.ArtifactStore == nil {
		return fmt.Errorf("artifact ledger 未装配")
	}
	root := ""
	if g.Workdir != nil {
		root = g.Workdir.Get()
	}
	rel := normalizeWrittenArtifactPath(logicalPath, root)
	meta := model.ArtifactMeta{SHA256: computeSHA256(content), Bytes: int64(len(content))}
	if err := g.ArtifactStore.AppendArtifactWithMeta(taskID, rel, meta); err != nil {
		return fmt.Errorf("登记产物证据失败 task=%s path=%s: %w", taskID, rel, err)
	}
	return nil
}

// normalizeWrittenArtifactPath 与兼容 Reactor 的路径归一规则保持一致，
// 使同步登记和稍后到达的异步观察命中同一个幂等键。
func normalizeWrittenArtifactPath(absPath, projectRoot string) string {
	cleaned := filepath.Clean(absPath)
	if projectRoot != "" {
		// ValidatePath 返回 canonical 绝对路径，因此 root 也必须
		// 使用同一权威规则规范化后再 Rel（特别是 macOS /var ->
		// /private/var 与配置中的相对 project_root）。
		if canonicalRoot, err := pathutil.CanonicalizeRoot(projectRoot); err == nil {
			if rel, err := filepath.Rel(canonicalRoot, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
		if rel, err := filepath.Rel(projectRoot, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		if rootAbs, err := filepath.Abs(projectRoot); err == nil {
			if rel, err := filepath.Rel(rootAbs, cleaned); err == nil && !strings.HasPrefix(rel, "..") {
				return filepath.ToSlash(rel)
			}
		}
	}
	return filepath.ToSlash(cleaned)
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

	r.Register("edit_file", "在文件中做精准的 old_str -> new_str 单次替换。可选提供 line_anchors 做行级哈希校验（比 expected_hash 更细粒度）；提供 line_anchors 时 expected_hash 会被忽略。",
		schema.Object().
			String("path", "文件路径", true).
			String("old_str", "要替换的旧字符串（必须在文件中唯一匹配）", true).
			String("new_str", "替换后的新字符串", true).
			String("expected_hash", "期望的当前文件 SHA256 哈希", false).
			StringArray("line_anchors",
				"行哈希锚点列表，如 [\"12#VK\",\"13#QZ\"]。提供时 expected_hash 会被忽略；任一行哈希失配则拒绝并返回当前哈希。", false).
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
		Kind:        trace.KindFileWriteQueued,
		TaskID:      agent.TaskIDFromContext(ctx),
		AgentID:     g.AgentID,
		Path:        path,
		WaitMS:      waitDuration.Milliseconds(),
		Description: "排队等待结束，成功获得文件锁",
	})

	return nil
}

// resolveWritePath 在 pathutil.ValidatePath 之后把主根逻辑路径解析为物理写入位置
// （按任务写时复制隔离）。Workdir 同时实现 PathOverlayer 时（runner 装配的
// workspace.Swapper）经它解析：edit 场景由实现方完成 copy-on-write 基线复制，
// 新文件直接落任务 workspace；无隔离（未实现或 passthrough）时原样返回。
// isolated=true 表示隔离生效——workspace 内本任务独占，调用方据此跳过
// claimOrWait / Roster.Release（主根锁在任务终态合并时由 workspace.Manager
// 逐文件统一声明）。
func (g LocalWriteGroup) resolveWritePath(logicalPath string) (physicalPath string, isolated bool, err error) {
	ov, ok := g.Workdir.(PathOverlayer)
	if !ok {
		return logicalPath, false, nil
	}
	newPath, err := ov.WritePath(logicalPath)
	if err != nil {
		return "", false, fmt.Errorf("解析隔离写入位置失败: %w", err)
	}
	if newPath != logicalPath {
		return newPath, true, nil
	}
	return logicalPath, false, nil
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
	if err := g.requireArtifactLedger(ctx); err != nil {
		return "", err
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

	// 按任务写时复制隔离：logicalPath 始终是主根逻辑路径（trace 事件与返回
	// 消息的账目坐标恒为主根；物理定位由 workspace.Manager.ResolveForTask
	// 负责），path 在此之后为实际落盘的物理路径。
	logicalPath := path
	physicalPath, isolated, err := g.resolveWritePath(logicalPath)
	if err != nil {
		return "", err
	}
	path = physicalPath

	// §8.3：通过 claimOrWait 声明文件写入权——冲突时排队等待前任释放。
	// 隔离生效时跳过：workspace 内本任务独占，主根锁由合并时统一声明。
	if !isolated {
		if err := g.claimOrWait(ctx, path, "写入"); err != nil {
			return "", err
		}
		defer g.Roster.Release(g.AgentID, path)
	}

	// C7 迁移：原 expected_hash 校验段已删除。
	// 乐观并发控制由 ValidateExpectedHashHook（PreCall, prio=20）接管。
	// 决策 B1：接受微小 TOCTOU 窗口（hook 校验在 Roster 锁外）。

	// H2b Effect Journal：执行前先落账（prepared），ArgsDigest 取将落盘
	// 内容的 sha256 前 12——恢复裁决据此与盘上事实比对（verify_first）。
	effID, err := effectPrepare(g.EffectJournal, ctx, g.AgentID,
		effect.KindFileWrite, logicalPath, digest12([]byte(content)), effect.PolicyVerifyFirst)
	if err != nil {
		return "", err
	}

	// 确保父目录存在
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		if journalErr := effectMarkUnknown(g.EffectJournal, effID, "创建目录失败: "+err.Error()); journalErr != nil {
			return "", journalErr
		}
		return "", fmt.Errorf("创建目录失败: %w", err)
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		// 写入返回错误时盘上可能残留部分内容——结果不可知，标 unknown
		// 交恢复裁决（核验盘上 hash 定论），不静默定论。
		if journalErr := effectMarkUnknown(g.EffectJournal, effID, "写入返回错误: "+err.Error()); journalErr != nil {
			return "", journalErr
		}
		return "", fmt.Errorf("写入文件失败: %w", err)
	}
	contentHash := computeSHA256([]byte(content))

	// 写入后使缓存失效（键为最终物理路径，与 read_file 的 Get/Put 键一致）
	if g.Cache != nil {
		g.Cache.Invalidate(path)
	}
	if err := effectSettle(g.EffectJournal, effID,
		fmt.Sprintf("bytes=%d sha256=%s", len(content), contentHash), true); err != nil {
		return "", err
	}
	if err := g.recordArtifact(ctx, logicalPath, []byte(content)); err != nil {
		return "", err
	}

	// Trace：file_written 事件（可审计的落盘记录）。
	// Path 保持主根逻辑路径——record-artifact / 验收的账目坐标恒为主根。
	writeEv := trace.Event{
		Kind:    trace.KindFileWritten,
		TaskID:  agent.TaskIDFromContext(ctx),
		AgentID: g.AgentID,
		Tool:    "write_file",
		Path:    logicalPath,
		Bytes:   len(content),
		Hash:    contentHash,
	}
	if isolated {
		writeEv.Description = fmt.Sprintf("写时复制隔离：落点 %s，任务成功终态合并回主根", path)
	}
	trace.Emit(writeEv)

	result := fmt.Sprintf("文件已写入: %s (%d 字节)", logicalPath, len(content))
	if isolated {
		result += "（写时复制隔离：已落入任务工作区，任务完成后合并回主根）"
	}
	return result, nil
}

// editFile 实现 edit_file 工具。端口自 worker.makeEditFileTool。
// 读取、匹配计数、替换写入三步在同一个 Roster 锁持有期间完成。
// 注：expected_hash 校验在 C7 后由 ValidateExpectedHashHook 接管。
func (g LocalWriteGroup) editFile(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_str"].(string)
	newStr, _ := args["new_str"].(string)

	// §7：宽容解析——剥去可能的 hashline 前缀（LLM 经常把 read_file 输出直接粘回 old_str / new_str）。
	// new_str 必须同样剥：否则 "12#VK|content" 这种字面前缀会被原样写入文件，把哈希前缀污染到产物里。
	// StripHashPrefix 内置 50% 阈值，对非 hashline 内容是 no-op，HashlineEnabled=false 路径也安全。
	// 该缺陷由首次评审发现，TestEditFile_StripHashPrefix_NewStr 是配套回归护栏。
	oldStr = hashline.StripHashPrefix(oldStr)
	newStr = hashline.StripHashPrefix(newStr)

	if path == "" {
		return "", fmt.Errorf("缺少 path 参数")
	}
	if err := g.requireArtifactLedger(ctx); err != nil {
		return "", err
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

	// 按任务写时复制隔离：logicalPath 始终是主根逻辑路径（trace 事件与返回
	// 消息的账目坐标恒为主根）；WritePath 顺带完成 copy-on-write——主根已有
	// 文件先复制基线进 workspace，下方读取/替换/写回都作用于副本。
	logicalPath := path
	physicalPath, isolated, err := g.resolveWritePath(logicalPath)
	if err != nil {
		return "", err
	}
	path = physicalPath

	// §8.3：通过 claimOrWait 声明文件写入权——冲突时排队等待前任释放。
	// 隔离生效时跳过：workspace 内本任务独占，主根锁由合并时统一声明。
	if !isolated {
		if err := g.claimOrWait(ctx, path, "编辑"); err != nil {
			return "", err
		}
		defer g.Roster.Release(g.AgentID, path)
	}

	// 读取文件（锁持有期间；隔离时读 workspace 内的 copy-on-write 基线副本）
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("文件不存在: %s", path)
	}

	// C7 迁移：原 expected_hash 校验段已删除。
	// 由 ValidateExpectedHashHook 在 PreCall 阶段接管（决策 B1：接受微小 TOCTOU）。

	content := string(data)

	// 计数匹配
	count := strings.Count(content, oldStr)
	if count > 1 {
		return "", fmt.Errorf("匹配到 %d 处，请提供更精确的 old_str", count)
	}

	matched := false
	crlfRetried := false
	newContent := ""
	if count == 1 {
		newContent = strings.Replace(content, oldStr, newStr, 1)
		matched = true
	} else if isFullCRLF(content) {
		// CRLF 重试：read_file 展示层已归一化为 LF，LLM 按展示构造的 old_str
		// 与磁盘 CRLF 内容必然失配。仅在全量 CRLF 文件上重试，替换后逆变换
		// 回 CRLF 保证无损往返；混合行尾文件不重试（2026-07-21 排查 M4）。
		normContent, _ := normalizeCRLF(content)
		normOld, _ := normalizeCRLF(oldStr)
		switch strings.Count(normContent, normOld) {
		case 1:
			normNew, _ := normalizeCRLF(newStr)
			newContent = strings.ReplaceAll(strings.Replace(normContent, normOld, normNew, 1), "\n", "\r\n")
			matched = true
			crlfRetried = true
		case 0:
			// 归一化后仍无匹配，落入下方统一错误
		default:
			return "", fmt.Errorf("匹配到多处（CRLF 归一化后），请提供更精确的 old_str")
		}
	}
	if !matched {
		return "", fmt.Errorf("未找到匹配内容，old_str 在文件中不存在")
	}

	// H2b Effect Journal：newContent 已确定、写盘前先落账（prepared），
	// ArgsDigest 取替换后全文的 sha256 前 12——恢复裁决据此与盘上事实比对。
	effID, err := effectPrepare(g.EffectJournal, ctx, g.AgentID,
		effect.KindFileEdit, logicalPath, digest12([]byte(newContent)), effect.PolicyVerifyFirst)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		if journalErr := effectMarkUnknown(g.EffectJournal, effID, "写入返回错误: "+err.Error()); journalErr != nil {
			return "", journalErr
		}
		return "", fmt.Errorf("写入文件失败: %w", err)
	}
	newHash := computeSHA256([]byte(newContent))

	// 写入后使缓存失效（键为最终物理路径，与 read_file 的 Get/Put 键一致）
	if g.Cache != nil {
		g.Cache.Invalidate(path)
	}
	if err := effectSettle(g.EffectJournal, effID,
		fmt.Sprintf("bytes=%d sha256=%s", len(newContent), newHash), true); err != nil {
		return "", err
	}
	if err := g.recordArtifact(ctx, logicalPath, []byte(newContent)); err != nil {
		return "", err
	}

	// Trace：file_written 事件（edit 也算一次落盘）。
	// Path 保持主根逻辑路径——record-artifact / 验收的账目坐标恒为主根。
	writeEv := trace.Event{
		Kind:    trace.KindFileWritten,
		TaskID:  agent.TaskIDFromContext(ctx),
		AgentID: g.AgentID,
		Tool:    "edit_file",
		Path:    logicalPath,
		Bytes:   len(newContent),
		Hash:    newHash,
	}
	if isolated {
		writeEv.Description = fmt.Sprintf("写时复制隔离：落点 %s，任务成功终态合并回主根", path)
	}
	trace.Emit(writeEv)

	oldLen := len(content)
	newLen := len(newContent)
	added := 0
	removed := 0
	if newLen > oldLen {
		added = newLen - oldLen
	} else {
		removed = oldLen - newLen
	}

	result := fmt.Sprintf("文件已编辑: %s (字节变化: +%d/-%d)", logicalPath, added, removed)
	if crlfRetried {
		result += "（提示：该文件为 CRLF 行尾，已按 CRLF 兼容模式完成替换，行尾保持 CRLF 不变）"
	}
	if isolated {
		result += "（写时复制隔离：已落入任务工作区，任务完成后合并回主根）"
	}
	return result, nil
}
