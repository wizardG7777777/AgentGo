// Package builtin 提供 Hook System 的内置 hook 实现。
// 阶段 1 范围（hookSystem.md §10.2）：
//   - PathBoundaryHook         — Pre，迁移自 pathutil.ValidatePath 散布调用（C6）
//   - ValidateExpectedHashHook — Pre，迁移自 LocalWriteGroup 内的 SHA256 校验段（C7）
//   - RequireReadBeforeWriteHook — Pre，新增（C8）
//
// 注：RecordArtifactHook 已于 v5 Phase 4 迁为 Reactor（internal/reactor/builtin，
// 订阅 KindFileWritten），本包不再持有该实现。
package builtin

import (
	"path/filepath"
	"strings"
)

// stringSliceFromArg 把 hook args 中可能是 []string、[]any 或 nil 的字段
// 安全转成 []string。
//
// LLM tool-call args 经过 JSON 反序列化后，数组字段通常拿到 []any（每项是 string）；
// 但 Go 内部直接调用时也可能传 []string。两种形态都要支持。其它形态返回 nil（视为空）。
//
// 由 ValidateExpectedHashHook（互斥判空）和 ValidateLineAnchorsHook（取出做哈希校验）
// 共用——参见 nextUpgrade_v4.md §7.7 互斥规则。
func stringSliceFromArg(v any) []string {
	if v == nil {
		return nil
	}
	if ss, ok := v.([]string); ok {
		return ss
	}
	if sa, ok := v.([]any); ok {
		out := make([]string, 0, len(sa))
		for _, item := range sa {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// normalizeArtifactPath 把绝对路径转换为相对项目根的相对路径。
//
// 实现来源：原 internal/tools/local_write.go 的同名函数，C5 迁移到 hook 包，
// 同时**修复**了 Windows 路径分隔符问题：用 filepath.ToSlash 把 \ 替换为 /，
// 让 task.Artifacts 在所有平台上都使用 / 分隔符。
//
// 行为契约：
//   - projectRoot 非空且路径在其内部 → 返回 / 风格相对路径（如 docs/foo.md）
//   - 路径在 projectRoot 之外（filepath.Rel 返回 ".." 前缀）→ 返回 / 风格 cleaned 路径
//   - projectRoot 为空 → 返回 / 风格 cleaned 路径
//
// projectRoot 可能是相对路径（setting.yaml 的 project_root: "."），而
// filepath.Rel 在 base 相对 / target 绝对时直接报错（Windows 与 POSIX 同），
// 此时先转成绝对路径再重试——否则 artifact 会被登记成绝对路径，与
// expected_artifacts 的相对路径字面比对永远失败（2026-07-21 验收马拉松事故）。
// 仅在直接 Rel 失败时才走 Abs 重试，保持词法相对可解场景的行为字节级不变。
//
// 设计理由：artifact 路径主要供 LLM 阅读和下游 worker 解析，跨平台一致比
// OS native 分隔符更重要。现由 EnforceExpectedArtifactsHook 使用；
// internal/reactor/builtin 的 record-artifact Reactor 持有同名同行为副本。
func normalizeArtifactPath(absPath, projectRoot string) string {
	cleaned := filepath.Clean(absPath)
	if projectRoot != "" {
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
