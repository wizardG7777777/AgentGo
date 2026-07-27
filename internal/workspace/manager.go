package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// baselineDirName 是 workspace 根下保存基线原始副本的目录名。
// 三路合并需要 base 原文，而 manifest 只记录基线哈希，因此 copy-on-write
// 时把基线内容同步落一份到该目录。目录内文件不在 manifest 映射里，
// 天然不属于 dirty set，合并与孤儿清扫均不感知它。
const baselineDirName = ".workspace-baseline"

// conflictTextLimit 是冲突报告中每侧冲突区文本的最大长度（4KB）。
const conflictTextLimit = 4 * 1024

// workspaceRoot 返回任务 workspace 的绝对路径，并拒绝越界 taskID
// （空、绝对路径、含路径分隔符），保证拼接结果恒在 workspaces 根下。
func (m *Manager) workspaceRoot(taskID string) (string, error) {
	if taskID == "" || taskID == "." || taskID == ".." ||
		filepath.IsAbs(taskID) || strings.ContainsAny(taskID, `/\`) {
		return "", fmt.Errorf("非法 taskID %q：不得为空、绝对路径或含路径分隔符", taskID)
	}
	return filepath.Join(m.projectRoot, DirName, taskID), nil
}

// relPath 把主根绝对路径换算为相对主根的路径；主根外路径返回 ok=false。
func (m *Manager) relPath(absMainPath string) (string, bool) {
	rel, err := filepath.Rel(m.projectRoot, absMainPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// resolveRead 实现 View.ReadPath：manifest 已登记或 workspace 下文件
// 存在则返回 workspace 副本路径，否则原样返回主根路径（读穿透）。
func (v *View) resolveRead(absMainPath string) string {
	rel, ok := v.mgr.relPath(absMainPath)
	if !ok {
		return absMainPath
	}
	if _, hit := v.mf.get(rel); hit {
		return filepath.Join(v.root, rel)
	}
	wsPath := filepath.Join(v.root, rel)
	if _, err := os.Stat(wsPath); err == nil {
		return wsPath
	}
	return absMainPath
}

// resolveWrite 实现 View.WritePath 的 copy-on-write：
// workspace 副本不存在时——主根文件存在则复制其内容进 workspace（父目录
// MkdirAll）、同步落基线原始副本供三路合并，并在 manifest 记录基线
// SHA256；主根不存在则 manifest 记为新建（baseline 为空串）。
func (v *View) resolveWrite(absMainPath string) (string, error) {
	rel, ok := v.mgr.relPath(absMainPath)
	if !ok {
		return "", fmt.Errorf("路径 %q 不在主根 %q 内，无法隔离写入", absMainPath, v.mgr.projectRoot)
	}
	wsPath := filepath.Join(v.root, rel)

	// 串行化检查-复制序列，避免并发 COW 同文件的竞态。
	v.mu.Lock()
	defer v.mu.Unlock()

	// 已有副本（manifest 登记，或文件已存在——manifest 同步持久化，
	// 后者仅覆盖持久化前崩溃的极窄窗口）→ 直接返回写入位置。
	if _, hit := v.mf.get(rel); hit {
		return wsPath, nil
	}
	if _, err := os.Stat(wsPath); err == nil {
		return wsPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(wsPath), 0o755); err != nil {
		return "", fmt.Errorf("创建 workspace 目录失败：%w", err)
	}
	data, err := os.ReadFile(absMainPath)
	switch {
	case err == nil:
		// 主根存在 → 复制基线进 workspace，并落基线原始副本。
		if err := os.WriteFile(wsPath, data, 0o644); err != nil {
			return "", fmt.Errorf("复制基线进 workspace 失败：%w", err)
		}
		baseCopy := filepath.Join(v.root, baselineDirName, rel)
		if err := os.MkdirAll(filepath.Dir(baseCopy), 0o755); err != nil {
			return "", fmt.Errorf("创建基线副本目录失败：%w", err)
		}
		if err := os.WriteFile(baseCopy, data, 0o644); err != nil {
			return "", fmt.Errorf("保存基线副本失败：%w", err)
		}
		sum := sha256.Sum256(data)
		if err := v.mf.set(rel, manifestEntry{BaselineSHA256: hex.EncodeToString(sum[:])}); err != nil {
			return "", err
		}
		return wsPath, nil
	case os.IsNotExist(err):
		// 主根不存在 → 记为新建文件（基线为空串）。
		if err := v.mf.set(rel, manifestEntry{New: true}); err != nil {
			return "", err
		}
		return wsPath, nil
	default:
		return "", fmt.Errorf("读取主根文件失败：%w", err)
	}
}

// mergeOne 合并单个文件：roster 非 nil 时对主根绝对路径先 TryClaim、
// 处理完 Release；随后按 FileOutcome 规则落盘。返回的报告 Path 已填
// 主根绝对路径。
func (m *Manager) mergeOne(agentID, rel string, e manifestEntry, wsRoot string) FileReport {
	mainPath := filepath.Join(m.projectRoot, rel)
	wsPath := filepath.Join(wsRoot, rel)
	rep := FileReport{Path: mainPath}

	if m.roster != nil {
		ok, err := m.roster.TryClaim(agentID, mainPath)
		if err != nil {
			rep.Outcome = OutcomeConflict
			rep.Detail = fmt.Sprintf("roster 声明失败：%v", err)
			return rep
		}
		if !ok {
			occupiedBy, _, _ := m.roster.IsOccupied(mainPath)
			rep.Outcome = OutcomeConflict
			rep.Detail = fmt.Sprintf("主根文件被 %s 占用，合并跳过", occupiedBy)
			return rep
		}
		defer func() { _ = m.roster.Release(agentID, mainPath) }()
	}

	wsData, err := os.ReadFile(wsPath)
	if os.IsNotExist(err) {
		// 登记了但副本不存在（WritePath 后未真正写入）→ 无变更。
		rep.Outcome = OutcomeIdentical
		rep.Detail = "workspace 副本不存在，无变更"
		return rep
	}
	if err != nil {
		rep.Outcome = OutcomeConflict
		rep.Detail = fmt.Sprintf("读取 workspace 副本失败：%v", err)
		return rep
	}

	mainData, mainErr := os.ReadFile(mainPath)
	mainExists := mainErr == nil
	if mainErr != nil && !os.IsNotExist(mainErr) {
		rep.Outcome = OutcomeConflict
		rep.Detail = fmt.Sprintf("读取主根文件失败：%v", mainErr)
		return rep
	}

	if e.New {
		// workspace 新建文件。
		switch {
		case !mainExists:
			if err := writeMainFile(mainPath, wsData); err != nil {
				rep.Outcome = OutcomeConflict
				rep.Detail = fmt.Sprintf("写入主根失败：%v", err)
				return rep
			}
			rep.Outcome = OutcomeNewFile
			rep.Detail = "新建文件落盘主根"
		case bytes.Equal(mainData, wsData):
			rep.Outcome = OutcomeIdentical
			rep.Detail = "双方新建内容一致"
		default:
			rep.Outcome = OutcomeConflict
			rep.Detail = "双方新建同名文件且内容不一致"
		}
		return rep
	}

	// 有基线的文件。
	if !mainExists {
		rep.Outcome = OutcomeConflict
		rep.Detail = "主根文件自基线以来被删除（删除-vs-修改）"
		return rep
	}
	if bytes.Equal(mainData, wsData) {
		rep.Outcome = OutcomeIdentical
		rep.Detail = "内容与主根一致，无需写入"
		return rep
	}
	sum := sha256.Sum256(mainData)
	if hex.EncodeToString(sum[:]) == e.BaselineSHA256 {
		// 主根自基线以来未变 → fast-forward 覆盖。
		if err := writeMainFile(mainPath, wsData); err != nil {
			rep.Outcome = OutcomeConflict
			rep.Detail = fmt.Sprintf("写入主根失败：%v", err)
			return rep
		}
		rep.Outcome = OutcomeFastForward
		rep.Detail = "主根自基线以来未变，直接覆盖"
		return rep
	}
	// 双方都变了 → 行级三路合并。base 原文取自基线副本。
	baseData, err := os.ReadFile(filepath.Join(wsRoot, baselineDirName, rel))
	if err != nil {
		rep.Outcome = OutcomeConflict
		rep.Detail = "基线副本缺失，无法三路合并"
		return rep
	}
	merged, conflicts, ok := Merge3(baseData, mainData, wsData)
	if !ok {
		rep.Outcome = OutcomeConflict
		rep.Detail = fmt.Sprintf("三路合并存在 %d 处行冲突", len(conflicts))
		rep.Conflicts = truncateConflicts(conflicts)
		return rep
	}
	if err := writeMainFile(mainPath, merged); err != nil {
		rep.Outcome = OutcomeConflict
		rep.Detail = fmt.Sprintf("写入合并结果失败：%v", err)
		return rep
	}
	rep.Outcome = OutcomeAutoMerged
	rep.Detail = "双侧变更区间不相交，自动合并"
	return rep
}

// writeMainFile 把内容写入主根（父目录按需创建）。
func writeMainFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// truncateConflicts 把每侧冲突区文本截断到 4KB，避免报告无限膨胀。
func truncateConflicts(in []ConflictRegion) []ConflictRegion {
	out := make([]ConflictRegion, len(in))
	for i, c := range in {
		c.Main = truncateText(c.Main)
		c.Workspace = truncateText(c.Workspace)
		out[i] = c
	}
	return out
}

func truncateText(s string) string {
	if len(s) <= conflictTextLimit {
		return s
	}
	return s[:conflictTextLimit] + "…(截断)"
}
