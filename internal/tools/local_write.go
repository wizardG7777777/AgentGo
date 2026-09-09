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

// LocalWriteGroup 提供统一文件变更入口，持有路径、并发、产物和副作用记录依赖。
// 写入与替换共享一次加锁、版本验证、落盘和结算流程。
type LocalWriteGroup struct {
	LocalReadGroup               // embed: 继承 Workdir + Cache
	Roster         roster.Roster // required
	AgentID        string        // required
	ArtifactStore  interface {
		AppendArtifactWithMeta(taskID string, path string, meta model.ArtifactMeta) error
	}
	WaitTimeoutSec int // §8.3：文件冲突排队等待秒数，0 = 不排队（旧行为）
	// EffectJournal 是 V6 §4 H2b 副作用账本（internal/effect）；
	// nil 时不记录副作用，仅供无任务上下文的工具测试使用。
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

func rejectRuntimeStateWrite(absPath, projectRoot string) error {
	if strings.TrimSpace(projectRoot) == "" {
		return nil
	}
	rel := strings.TrimPrefix(normalizeWrittenArtifactPath(absPath, projectRoot), "./")
	if rel == ".agentgo" || strings.HasPrefix(rel, ".agentgo/") {
		return fmt.Errorf("reason_code=runtime_state_write_forbidden：业务文件工具禁止写入 .agentgo 控制面路径")
	}
	return nil
}

// Register 只注册 apply_change，不保留旧写入工具别名。
func (g LocalWriteGroup) Register(r *agent.ToolRegistry) {
	params := schema.Object().
		String("path", "项目相对文件路径", true).
		Enum("operation", "write=创建或覆盖，create=仅新建，replace=精确替换；省略时由 content 或 old_str 确定", []string{"write", "create", "replace"}, false).
		String("content", "创建或覆盖后的完整文件内容，可以为空字符串", false).
		String("old_str", "精确替换的原文，必须唯一匹配", false).
		String("new_str", "替换后的内容，可以为空字符串", false).
		String("expected_hash", "读取时获得的 SHA256，提供时必须与当前文件一致", false).
		StringArray("line_anchors", "替换操作使用的行哈希锚点", false).Build()
	params["additionalProperties"] = false
	r.Register("apply_change", "创建、覆盖或精确修改项目文件。返回实际文件变更事实，不修改图定义。", params, g.applyChange)
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

// applyChange 在一个执行事务中完成输入校验、读取、变换、写入和事实记录。
func (g LocalWriteGroup) applyChange(ctx context.Context, args map[string]any) (string, error) {
	for _, key := range []string{"path", "operation", "content", "old_str", "new_str", "expected_hash"} {
		if value, present := args[key]; present {
			if _, valid := value.(string); !valid {
				return "", fmt.Errorf("apply_change 参数 %s 必须是字符串", key)
			}
		}
	}
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("缺少 path 参数")
	}
	for key := range args {
		switch key {
		case "path", "operation", "content", "old_str", "new_str", "expected_hash", "line_anchors":
		default:
			return "", fmt.Errorf("apply_change 不支持参数 %q", key)
		}
	}
	content, hasContent := args["content"].(string)
	oldStr, hasOld := args["old_str"].(string)
	newStr, hasNew := args["new_str"].(string)
	operation, _ := args["operation"].(string)
	if operation == "" {
		if hasContent {
			operation = "write"
		} else {
			operation = "replace"
		}
	}
	switch operation {
	case "write", "create":
		if !hasContent || hasOld || hasNew {
			return "", fmt.Errorf("创建或覆盖必须提供 content，不能混用替换参数")
		}
	case "replace":
		if hasContent || !hasOld || oldStr == "" || !hasNew {
			return "", fmt.Errorf("替换必须提供非空 old_str 和 new_str，不能混用 content")
		}
	default:
		return "", fmt.Errorf("未知文件变更操作 %q", operation)
	}
	if err := g.requireArtifactLedger(ctx); err != nil {
		return "", err
	}
	root := ""
	if g.Workdir != nil {
		root = g.Workdir.Get()
	}
	if root != "" {
		resolved, err := pathutil.ValidatePath(path, root)
		if err != nil {
			return "", err
		}
		path = resolved
		if err := rejectRuntimeStateWrite(path, root); err != nil {
			return "", err
		}
	}
	logicalPath := path
	// 先锁定逻辑路径，再准备写时复制副本；避免两个 Agent 同时复制旧基线。
	if g.Roster == nil {
		return "", fmt.Errorf("文件变更缺少并发锁")
	}
	if err := g.claimOrWait(ctx, logicalPath, "应用变更"); err != nil {
		return "", err
	}
	defer g.Roster.Release(g.AgentID, logicalPath)
	physicalPath, isolated, err := g.resolveWritePath(logicalPath)
	if err != nil {
		return "", err
	}
	path = physicalPath
	previous, readErr := os.ReadFile(path)
	exists := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", fmt.Errorf("读取目标文件失败: %w", readErr)
	}
	if operation == "create" && exists {
		return "", fmt.Errorf("创建目标已存在: %s", logicalPath)
	}
	if operation == "replace" && !exists {
		return "", fmt.Errorf("文件不存在: %s", logicalPath)
	}
	var anchors []string
	switch values := args["line_anchors"].(type) {
	case []string:
		anchors = values
	case []any:
		for _, value := range values {
			anchor, ok := value.(string)
			if !ok {
				return "", fmt.Errorf("line_anchors 必须是字符串数组")
			}
			anchors = append(anchors, anchor)
		}
	case nil:
		if _, present := args["line_anchors"]; present {
			return "", fmt.Errorf("line_anchors 必须是字符串数组")
		}
	default:
		return "", fmt.Errorf("line_anchors 必须是字符串数组")
	}
	if len(anchors) > 0 {
		if operation != "replace" {
			return "", fmt.Errorf("行锚点只用于精确替换")
		}
		lines := strings.Split(string(previous), "\n")
		for _, anchor := range anchors {
			ref, err := hashline.ParseLineRef(anchor)
			if err != nil {
				return "", err
			}
			if ref.Line <= 0 || ref.Line > len(lines) || hashline.ComputeLineHash(ref.Line, lines[ref.Line-1]) != ref.Hash {
				return "", fmt.Errorf("行锚点冲突：%s 与当前文件不一致", anchor)
			}
		}
	}
	expected, _ := args["expected_hash"].(string)
	if expected != "" && (!exists || expected != computeSHA256(previous)) {
		return "", fmt.Errorf("文件版本冲突：expected_hash 与当前内容不一致")
	}
	crlfRetried := false
	if operation == "replace" {
		oldStr, newStr = hashline.StripHashPrefix(oldStr), hashline.StripHashPrefix(newStr)
		source := string(previous)
		count := strings.Count(source, oldStr)
		if count == 0 && isFullCRLF(source) {
			source, _ = normalizeCRLF(source)
			oldStr, _ = normalizeCRLF(oldStr)
			newStr, _ = normalizeCRLF(newStr)
			count = strings.Count(source, oldStr)
			crlfRetried = true
		}
		if count == 0 {
			return "", fmt.Errorf("未找到匹配内容，old_str 在文件中不存在")
		}
		if count != 1 {
			return "", fmt.Errorf("匹配到 %d 处，请提供更精确的 old_str", count)
		}
		content = strings.Replace(source, oldStr, newStr, 1)
		if crlfRetried {
			content = strings.ReplaceAll(content, "\n", "\r\n")
		}
	}
	kind := effect.KindFileWrite
	eventKind := trace.KindFileWritten
	if operation == "replace" {
		kind = effect.KindFileEdit
	}
	effID, err := effectPrepare(g.EffectJournal, ctx, g.AgentID, kind,
		logicalPath, digest12([]byte(content)), effect.PolicyVerifyFirst)
	if err != nil {
		return "", err
	}
	fail := func(cause error) (string, error) {
		if journalErr := effectMarkUnknown(g.EffectJournal, effID, cause.Error()); journalErr != nil {
			return "", journalErr
		}
		return "", cause
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fail(fmt.Errorf("创建目录失败: %w", err))
	}
	mode := os.FileMode(0644)
	if stat, err := os.Stat(path); err == nil {
		mode = stat.Mode().Perm()
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".agentgo-change-*")
	if err != nil {
		return fail(fmt.Errorf("创建变更临时文件失败: %w", err))
	}
	tempPath := temporary.Name()
	defer os.Remove(tempPath)
	if err = temporary.Chmod(mode); err == nil {
		_, err = temporary.Write([]byte(content))
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fail(fmt.Errorf("写入临时文件失败: %w", err))
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fail(fmt.Errorf("提交文件变更失败: %w", err))
	}
	if g.Cache != nil {
		g.Cache.Invalidate(path)
	}
	digest := computeSHA256([]byte(content))
	if err := effectSettle(g.EffectJournal, effID, fmt.Sprintf("bytes=%d sha256=%s", len(content), digest), true); err != nil {
		return "", err
	}
	if err := g.recordArtifact(ctx, logicalPath, []byte(content)); err != nil {
		return "", err
	}
	description := ""
	if isolated {
		description = fmt.Sprintf("写时复制隔离：落点 %s", path)
	}
	trace.Emit(trace.Event{Kind: eventKind, TaskID: agent.TaskIDFromContext(ctx), AgentID: g.AgentID,
		Tool: "apply_change", Path: logicalPath, Bytes: len(content), Hash: digest, Description: description})
	beforeHash := "absent"
	if exists {
		beforeHash = computeSHA256(previous)
	}
	result := fmt.Sprintf("文件变更已应用: %s (%d 字节，operation=%s，sha256=%s)", logicalPath, len(content), operation, digest)
	result += " before_hash=" + beforeHash
	if isolated {
		result += "（已落入隔离工作区）"
	}
	if crlfRetried {
		result += "（保留 CRLF 行尾）"
	}
	return result, nil
}
