package builtin

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"agentgo/internal/hook"
	"agentgo/internal/tools/hashline"
)

// mismatch 记录单行哈希失配信息。
type mismatch struct {
	line         int
	expectedHash string
	actualHash   string
}

// ValidateLineAnchorsHook 在 write_file / edit_file 调用之前校验 args 中的
// line_anchors 是否与目标文件当前行的内容哈希一致。
//
// 这是 §7 Hashline 行哈希增强的核心校验层。与 ValidateExpectedHashHook 互斥：
// 提供 line_anchors 时 expected_hash 被忽略。
//
// Phase: PreCall, Priority: 25（位于 ValidateExpectedHash=20 与
// RequireReadBeforeWrite=30 之间）。
type ValidateLineAnchorsHook struct{}

// NewValidateLineAnchorsHook 构造函数。
func NewValidateLineAnchorsHook() *ValidateLineAnchorsHook {
	return &ValidateLineAnchorsHook{}
}

// Name 返回 hook 唯一标识。
func (h *ValidateLineAnchorsHook) Name() string { return "validate-line-anchors" }

// Phase 返回 PhasePreCall。
func (h *ValidateLineAnchorsHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 25。
func (h *ValidateLineAnchorsHook) Priority() int { return 25 }

// Matches 仅匹配 write_file 和 edit_file。
func (h *ValidateLineAnchorsHook) Matches(toolName string) bool {
	return toolName == "write_file" || toolName == "edit_file"
}

// Run 执行行哈希校验逻辑。
func (h *ValidateLineAnchorsHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	anchors := stringSliceFromArg(hctx.Args["line_anchors"])
	if len(anchors) == 0 {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	path, ok := hctx.Args["path"].(string)
	if !ok || path == "" {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return hook.ToolHookDecision{Action: hook.Continue}
		}
		return hook.ToolHookDecision{
			Action:      hook.Abort,
			HookName:    h.Name(),
			AbortReason: fmt.Sprintf("行哈希校验前读取文件失败: %v", err),
		}
	}

	lines := strings.Split(string(data), "\n")
	// 如果文件以 \n 结尾，strings.Split 会产生一个尾部空字符串，
	// 这在行号计算中不应算作额外一行。但在校验时我们保留它，
	// 因为行号 1-based 对应的是 Split 后的索引 0..n-1。

	var parseErrors []string
	var mismatches []mismatch

	for _, a := range anchors {
		ref, err := hashline.ParseLineRef(a)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("  - %q: %v", a, err))
			continue
		}
		if ref.Line < 1 || ref.Line > len(lines) {
			mismatches = append(mismatches, mismatch{
				line:         ref.Line,
				expectedHash: ref.Hash,
				actualHash:   "(越界)",
			})
			continue
		}
		actual := hashline.ComputeLineHash(ref.Line, lines[ref.Line-1])
		if actual != ref.Hash {
			mismatches = append(mismatches, mismatch{
				line:         ref.Line,
				expectedHash: ref.Hash,
				actualHash:   actual,
			})
		}
	}

	if len(parseErrors) == 0 && len(mismatches) == 0 {
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	// 构造错误消息
	reason := buildMismatchReason(path, lines, parseErrors, mismatches)
	return hook.ToolHookDecision{
		Action:      hook.Abort,
		HookName:    h.Name(),
		AbortReason: reason,
	}
}

// buildMismatchReason 构造哈希失配的格式化错误消息。
// 含 ±2 行上下文 + >>> 高亮 + 当前重算哈希。
func buildMismatchReason(path string, lines []string, parseErrors []string, mismatches []mismatch) string {
	var sb strings.Builder

	// 头部
	if len(mismatches) > 0 {
		if len(mismatches) == 1 {
			sb.WriteString("行哈希校验失败：1 行自读取以来已改变。")
		} else {
			sb.WriteString(fmt.Sprintf("行哈希校验失败：%d 行自读取以来已改变。", len(mismatches)))
		}
		sb.WriteString("请用下方更新后的 LINE#HASH 引用重试（>>> 标记失配行）。\n")
	}
	if len(parseErrors) > 0 {
		if len(mismatches) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("另外有 %d 个锚点无法解析：\n", len(parseErrors)))
		for _, pe := range parseErrors {
			sb.WriteString(pe)
			sb.WriteString("\n")
		}
	}

	// 构建需要展示的行号集合（失配行 ±2 上下文）
	if len(mismatches) > 0 {
		showSet := make(map[int]bool)
		for _, m := range mismatches {
			for l := max(1, m.line-2); l <= min(len(lines), m.line+2); l++ {
				showSet[l] = true
			}
		}
		showLines := make([]int, 0, len(showSet))
		for l := range showSet {
			showLines = append(showLines, l)
		}
		sort.Ints(showLines)

		mismatchSet := make(map[int]string) // line -> actualHash
		for _, m := range mismatches {
			mismatchSet[m.line] = m.actualHash
		}

		sb.WriteString("\n")
		prev := 0
		for _, l := range showLines {
			if prev > 0 && l > prev+1 {
				sb.WriteString("    ...\n")
			}
			content := lines[l-1]
			actualHash := hashline.ComputeLineHash(l, content)
			if expected, isMismatch := mismatchSet[l]; isMismatch {
				sb.WriteString(fmt.Sprintf(">>> %d#%s|%s     ← 期望 %s，实际 %s\n", l, actualHash, content, expected, actualHash))
			} else {
				sb.WriteString(fmt.Sprintf("    %d#%s|%s\n", l, actualHash, content))
			}
			prev = l
		}
		sb.WriteString("\n提示：复用最新 read_file / edit_file 输出里的 LINE#HASH 引用；不要凭记忆构造哈希。")
	}

	return sb.String()
}
