package builtin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"agentgo/internal/hook"
)

// ValidateExpectedHashHook 在 write_file / edit_file 调用之前校验 args
// 中的 expected_hash 是否与目标文件当前内容的 SHA256 一致。
//
// 这是 LocalWriteGroup.writeFile / editFile 中乐观并发控制段的 C7 迁移。
// 原 inline 实现已删除，整套语义在本 hook 中重建。
//
// 决策 B1（接受微小 TOCTOU 窗口，hookSystem.md §10.1）：
// 迁移之前 hash 校验在 Roster.TryClaim 之后、os.WriteFile 之前发生（锁内）。
// 迁移之后校验移到 PreCall hook，发生在 Roster 锁外，引入一个微秒级的
// TOCTOU 窗口：从 hook 校验 hash → 工具拿 Roster 锁 → 工具写入之间，
// 文件可能被外部进程修改。
//
// 在单进程 AgentGo 内此风险微秒级（所有写都走 Roster 锁），8-16 个代理
// 环境下也极少出现小时间窗口内多代理改同一文件。如未来发现真实复现，
// 可以退回 inline 实现，hook 系统设计不需要变。
//
// 行为契约（与原 inline 实现完全等价）：
//   - args["expected_hash"] 不存在或为空字符串 → Continue（跳过校验）
//   - args["path"] 缺失或非字符串 → Continue（让其他校验报错）
//   - 文件不存在（os.IsNotExist）→ Continue（允许新建，与原行为一致）
//   - 其他 ReadFile 错误 → Abort（权限拒绝等异常情况）
//   - 计算的 SHA256 != expected_hash → Abort，错误消息包含期望和实际 hash
//   - hash 一致 → Continue
//
// Phase: PreCall, Priority: 20（位于 PathBoundary=10 之后，先做路径
// 校验再做内容校验是合理的顺序）。
type ValidateExpectedHashHook struct{}

// NewValidateExpectedHashHook 是 ValidateExpectedHashHook 的构造函数。
// 本 hook 无任何外部依赖，构造函数仅为 API 一致性而存在。
func NewValidateExpectedHashHook() *ValidateExpectedHashHook {
	return &ValidateExpectedHashHook{}
}

// Name 返回 hook 唯一标识。
func (h *ValidateExpectedHashHook) Name() string { return "validate-expected-hash" }

// Phase 返回 PhasePreCall。
func (h *ValidateExpectedHashHook) Phase() hook.ToolHookPhase { return hook.PhasePreCall }

// Priority 返回 20。
func (h *ValidateExpectedHashHook) Priority() int { return 20 }

// Matches 仅匹配 write_file 和 edit_file。
func (h *ValidateExpectedHashHook) Matches(toolName string) bool {
	return toolName == "write_file" || toolName == "edit_file"
}

// Run 执行 hash 校验逻辑。
func (h *ValidateExpectedHashHook) Run(hctx hook.ToolHookContext) hook.ToolHookDecision {
	expectedHash, _ := hctx.Args["expected_hash"].(string)
	if expectedHash == "" {
		// 没提供 hash → 跳过校验，与原 inline 行为一致
		return hook.ToolHookDecision{Action: hook.Continue}
	}
	path, ok := hctx.Args["path"].(string)
	if !ok || path == "" {
		// path 缺失 → 让下游 hook（PathBoundary）或工具自报错
		return hook.ToolHookDecision{Action: hook.Continue}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在 → 允许新建，与原 inline 行为一致
			return hook.ToolHookDecision{Action: hook.Continue}
		}
		return hook.ToolHookDecision{
			Action:      hook.Abort,
			HookName:    h.Name(),
			AbortReason: fmt.Sprintf("hash 校验前读取文件失败: %v", err),
		}
	}

	current := sha256Hex(data)
	if current != expectedHash {
		return hook.ToolHookDecision{
			Action:   hook.Abort,
			HookName: h.Name(),
			AbortReason: fmt.Sprintf(
				"写入冲突：文件 %s 的内容已被其他代理修改（期望哈希 %s，当前哈希 %s）。请重新调用 read_file 获取最新内容后再试",
				path, expectedHash, current,
			),
		}
	}
	return hook.ToolHookDecision{Action: hook.Continue}
}

// sha256Hex 计算字节切片的 SHA256 摘要并返回十六进制字符串。
// 与 internal/tools 包的 computeSHA256 行为完全一致 —— 这里独立实现是为了
// 避免 hook/builtin 包反向依赖 tools 包，保持依赖方向单一（hook 只依赖 store）。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
