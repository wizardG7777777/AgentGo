package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/interaction"
	"agentgo/internal/modes"
	"agentgo/internal/shell"
)

const (
	// PurposeFileWrite 标识 strict 执行模式下的文件写入授权请求。
	PurposeFileWrite interaction.Purpose = "file_write"
	// ResolutionHandlerFileWrite 是 Bootstrap 回答路由使用的稳定 handler。
	// 与 shell_command 相同：服务端零 effect——写文件副作用留在等待中的工具
	// wrapper，控制面只负责锁定并完成回答。
	ResolutionHandlerFileWrite = "file_write"

	// 授权请求 Metadata 键。Metadata 不进 UI 投影，仅供 Await 返回后逐项复核。
	metadataFileTool          = "tool"
	metadataFilePath          = "path"
	metadataFilePayloadDigest = "payload_digest"
	metadataFileDigest        = "digest"
)

// 预览上限。TUI 按 6 行分页，预览只帮助用户快速判断写入意图，不替代完整 diff。
const (
	fileWritePreviewMaxLines = 5
	fileWritePreviewMaxRunes = 300
)

// FileWriteApprover 是 strict 执行模式下 write_file / edit_file 的 Interaction
// 审批包装器，集成形态复刻 shell.WrapShellTool（internal/shell/intercept.go）：
// 装配期经 ToolRegistry.WrapHandler 包装，defs 不变，LLM 侧无感知。
//
//   - exec=strict：每次写入先创建 file_write 授权请求并阻塞等待用户回答；
//     allow_once / allow_session 才执行 inner，deny / guidance 返回中文错误
//     （guidance 把用户文本回灌给 LLM）。
//   - 其它 exec 档位：完全透传。normal/yolo 对写工具行为一致；readonly 的拦截
//     由 exec-mode-guard Gate 在 PreCall 阶段完成，不经过本包装器。
//   - Interaction 服务不可用、创建/等待失败、ctx 取消、过期或绑定被篡改：
//     一律 fail-closed，不执行写入。
//
// 每个 runner / scheduler 各持一个实例；sessionAllowed 是该实例进程生命期内的
// 记忆，粒度为确切路径（经 filepath.Clean 归一）。不跨 agent 共享、不持久化——
// 与 shell 运行时白名单"进程重启失效"的安全边界一致；目录级粒度留待后续版本。
type FileWriteApprover struct {
	modeStore    *modes.Store
	interactions *interaction.Service
	sessionID    func() string
	agentID      string
	waitHook     func(waiting bool)

	mu             sync.RWMutex
	sessionAllowed map[string]struct{}
}

// NewFileWriteApprover 构造审批包装器。modeStore 为 nil 等价 normal（永不询问，
// nil 只出现在单测直构场景）；interactions 为 nil 时 strict 下 fail-closed。
// sessionID 可为 nil；waitHook 在成功创建请求后、开始等待前收到 true，并在
// Await 返回后收到 false（接线 agent 状态机 waiting_interaction），nil 为 no-op。
func NewFileWriteApprover(modeStore *modes.Store, interactions *interaction.Service,
	sessionID func() string, agentID string, waitHook func(waiting bool)) *FileWriteApprover {
	return &FileWriteApprover{
		modeStore:      modeStore,
		interactions:   interactions,
		sessionID:      sessionID,
		agentID:        agentID,
		waitHook:       waitHook,
		sessionAllowed: make(map[string]struct{}),
	}
}

// WrapHandler 返回 agent.ToolRegistry.WrapHandler 所需的包装函数。
// toolName 写入提示与 digest 绑定（"write_file" / "edit_file"），必须与
// ToolRegistry 中的注册名一致。
func (a *FileWriteApprover) WrapHandler(toolName string) func(agent.ToolFunc) agent.ToolFunc {
	return func(inner agent.ToolFunc) agent.ToolFunc {
		return func(ctx context.Context, args map[string]any) (string, error) {
			// 非 strict 一律透传。
			if a.modeStore == nil || a.modeStore.GetExec() != modes.ExecStrict {
				return inner(ctx, args)
			}
			path, _ := args["path"].(string)
			if path == "" {
				return inner(ctx, args) // 缺参数交给 inner 报"缺少 path 参数"
			}
			if a.isSessionAllowed(path) {
				return inner(ctx, args)
			}
			if a.interactions == nil {
				return "", fmt.Errorf("⚠ strict 模式下写入文件需要人工批准，但 Interaction 服务不可用；已拒绝执行")
			}

			payloadDigest := sha256Hex([]byte(fileWritePayload(toolName, args)))
			digest := fileWriteDigest(toolName, path, payloadDigest)
			taskID := agent.TaskIDFromContext(ctx)
			currentSessionID := ""
			if a.sessionID != nil {
				currentSessionID = a.sessionID()
			}
			var expiresAt time.Time
			if deadline, ok := ctx.Deadline(); ok {
				expiresAt = deadline
			}
			request, err := a.interactions.Create(ctx, interaction.CreateRequest{
				SessionID: currentSessionID,
				Kind:      interaction.KindAuthorization,
				Purpose:   PurposeFileWrite,
				Prompt:    buildFileWritePrompt(a.agentID, toolName, path, args),
				// guidance 选项 RequiresText，协议要求 Request 级 AllowFreeText。
				AllowFreeText: true,
				Options: []interaction.Option{
					{ID: shell.ActionAllowOnce, Label: "仅允许本次", ActionRef: shell.ActionAllowOnce},
					{ID: shell.ActionDeny, Label: "拒绝", ActionRef: shell.ActionDeny},
					{ID: shell.ActionGuidance, Label: "提供指导", RequiresText: true, ActionRef: shell.ActionGuidance},
					{ID: shell.ActionAllowSession, Label: "本会话内不再询问该路径", ActionRef: shell.ActionAllowSession},
				},
				Origin: interaction.Origin{Component: "file_write", AgentID: a.agentID, TaskID: taskID},
				// 工具执行 ctx 当前不携带 tool_call_id（llm_executor 未注入），
				// Subject.ToolCallID 留空；绑定强度由 Digest 承担。
				Subject: interaction.Subject{Kind: "file_write", ID: path, TaskID: taskID, Digest: digest},
				Resolution: interaction.ResolutionSpec{
					Handler: ResolutionHandlerFileWrite, TargetID: digest,
					AgentID: a.agentID, TaskID: taskID,
				},
				Metadata: map[string]string{
					metadataFileTool: toolName, metadataFilePath: path,
					metadataFilePayloadDigest: payloadDigest, metadataFileDigest: digest,
				},
				ExpiresAt: expiresAt,
			})
			if err != nil {
				return "", fmt.Errorf("⚠ 无法创建文件写入审批请求；已拒绝执行: %w", err)
			}

			if a.waitHook != nil {
				a.waitHook(true)
			}
			resolved, err := a.interactions.Await(ctx, request.ID)
			if a.waitHook != nil {
				a.waitHook(false)
			}
			if err != nil {
				if ctx.Err() != nil {
					bestEffortInterruptFileWrite(a.interactions, request.ID, "写入任务已取消或系统正在关闭")
					return "", fmt.Errorf("文件写入审批等待被取消（任务结束、超时或系统关闭）: %w", ctx.Err())
				}
				switch {
				case errors.Is(err, interaction.ErrCancelled):
					return "", fmt.Errorf("⚠ 文件写入审批已取消: %w", err)
				case errors.Is(err, interaction.ErrExpired):
					return "", fmt.Errorf("⚠ 文件写入审批已过期: %w", err)
				case errors.Is(err, interaction.ErrInterrupted):
					return "", fmt.Errorf("⚠ 文件写入审批已中断: %w", err)
				default:
					return "", fmt.Errorf("⚠ 文件写入审批未完成；已拒绝执行: %w", err)
				}
			}
			if !matchesFileWriteRequest(resolved, toolName, path, payloadDigest, digest, a.agentID, taskID) {
				return "", fmt.Errorf("⚠ 文件写入审批与当前调用不匹配；已拒绝执行")
			}
			selected, ok := resolved.SelectedOption()
			if !ok {
				return "", fmt.Errorf("⚠ 文件写入审批缺少有效选项；已拒绝执行")
			}
			switch selected.ActionRef {
			case shell.ActionAllowOnce:
				// 仅放行当前精确调用。
			case shell.ActionAllowSession:
				// 只记住创建请求时捕获的确切路径；用户回答与 ActionRef 均不能
				// 提供替代路径（与 shell 会话白名单同一原则）。
				a.rememberSessionAllowed(path)
				log.Printf("[file-write] 会话内放行写入路径: agent=%s, path=%s", a.agentID, path)
			case shell.ActionDeny:
				return "", fmt.Errorf("⚠ 文件写入被用户拒绝。请调整方案后重试。")
			case shell.ActionGuidance:
				if resolved.Response == nil || resolved.Response.Text == "" {
					return "", fmt.Errorf("⚠ 用户指导为空；文件未写入")
				}
				return "", fmt.Errorf("用户指导: %s", resolved.Response.Text)
			default:
				return "", fmt.Errorf("⚠ 未知文件写入审批动作 %q；已拒绝执行", selected.ActionRef)
			}
			return inner(ctx, args)
		}
	}
}

// isSessionAllowed 报告该路径是否已被 allow_session 放行（进程内记忆）。
func (a *FileWriteApprover) isSessionAllowed(path string) bool {
	key := filepath.Clean(path)
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.sessionAllowed[key]
	return ok
}

// rememberSessionAllowed 把确切路径记入 allow_session 放行集。
func (a *FileWriteApprover) rememberSessionAllowed(path string) {
	key := filepath.Clean(path)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionAllowed[key] = struct{}{}
}

// fileWritePayload 提取与本次写入副作用绑定的载荷：
// write_file = content；edit_file = old_str + NUL + new_str。
func fileWritePayload(toolName string, args map[string]any) string {
	if toolName == "edit_file" {
		oldStr, _ := args["old_str"].(string)
		newStr, _ := args["new_str"].(string)
		return oldStr + "\x00" + newStr
	}
	content, _ := args["content"].(string)
	return content
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileWriteDigest 绑定工具名、路径与内容摘要（载荷 SHA-256），
// Await 返回后逐项复核，确保回答只作用于被批准的那一次精确调用。
func fileWriteDigest(toolName, path, payloadDigest string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(toolName))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(path))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(payloadDigest))
	return hex.EncodeToString(hash.Sum(nil))
}

// matchesFileWriteRequest 复核 Await 返回的请求仍与当前调用绑定
// （docs/design/interaction.md 实现不变量 5：effect 前重新核对 digest 与业务身份）。
func matchesFileWriteRequest(request interaction.Request, toolName, path, payloadDigest, digest, agentID, taskID string) bool {
	return request.State == interaction.StateResolved &&
		request.Purpose == PurposeFileWrite &&
		request.Resolution.Handler == ResolutionHandlerFileWrite &&
		request.Resolution.TargetID == digest &&
		request.Resolution.AgentID == agentID &&
		request.Resolution.TaskID == taskID &&
		request.Origin.AgentID == agentID &&
		request.Origin.TaskID == taskID &&
		request.Subject.Kind == "file_write" &&
		request.Subject.ID == path &&
		request.Subject.TaskID == taskID &&
		request.Subject.Digest == digest &&
		request.Metadata[metadataFileTool] == toolName &&
		request.Metadata[metadataFilePath] == path &&
		request.Metadata[metadataFilePayloadDigest] == payloadDigest &&
		request.Metadata[metadataFileDigest] == digest
}

// buildFileWritePrompt 构造用户可见的审批正文。Metadata 不进 UI 投影，因此
// 工具名、目标路径、操作摘要与短预览都必须放进 Prompt；长度按 TUI 6 行分页控制。
func buildFileWritePrompt(agentID, toolName, path string, args map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent %s 请求写入文件（strict 执行模式，需人工批准）：\n", agentID)
	fmt.Fprintf(&b, "工具: %s\n路径: %s\n", toolName, path)
	if toolName == "edit_file" {
		oldStr, _ := args["old_str"].(string)
		newStr, _ := args["new_str"].(string)
		fmt.Fprintf(&b, "摘要: 单次替换，旧文本 %d 字节 → 新文本 %d 字节\n", len(oldStr), len(newStr))
		b.WriteString("- 旧: " + previewSnippet(oldStr) + "\n")
		b.WriteString("+ 新: " + previewSnippet(newStr))
	} else {
		content, _ := args["content"].(string)
		fmt.Fprintf(&b, "摘要: 覆盖写入，共 %d 字节\n", len(content))
		b.WriteString("预览: " + previewSnippet(content))
	}
	return b.String()
}

// previewSnippet 取前 fileWritePreviewMaxLines 行并限制在
// fileWritePreviewMaxRunes 个字符内，截断时追加提示。
func previewSnippet(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	truncated := false
	if len(lines) > fileWritePreviewMaxLines {
		lines = lines[:fileWritePreviewMaxLines]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	runes := []rune(out)
	if len(runes) > fileWritePreviewMaxRunes {
		out = string(runes[:fileWritePreviewMaxRunes])
		truncated = true
	}
	if truncated {
		out += " ……（已截断）"
	}
	return out
}

// bestEffortInterruptFileWrite 在原 ctx 已取消后使用独立短超时上下文收尾，
// 避免留下永久 pending 的幽灵请求。版本冲突时重新读取一次；任何错误都只记录
// 日志，原调用保持 fail-closed。形态同 shell.bestEffortInterrupt。
func bestEffortInterruptFileWrite(interactions *interaction.Service, id, reason string) {
	if interactions == nil || id == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		request, err := interactions.Get(cleanupCtx, id)
		if err != nil || request.State.IsTerminal() {
			return
		}
		if _, err = interactions.Interrupt(cleanupCtx, id, request.Version, reason); err == nil {
			return
		}
		if !errors.Is(err, interaction.ErrVersionConflict) {
			log.Printf("[file-write] 收尾 Interaction %s 失败: %v", id, err)
			return
		}
	}
}
