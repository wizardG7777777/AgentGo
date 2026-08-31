package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentgo/internal/delivery"
	"agentgo/internal/model"
	"agentgo/internal/pathutil"
	"agentgo/internal/store"
)

// FreezeCandidate 将 delivery workspace 的 dirty set 冻结为稳定只读身份。
// 它不提升任何文件；manifest 条目按路径排序，并把候选文件内容摘要纳入
// digest，因而 map 遍历顺序或展示文本都不会改变 CandidateRef。
func (m *Manager) FreezeCandidate(deliveryID, workspaceID, workspaceRevisionRef string) (delivery.Candidate, error) {
	if !strings.HasPrefix(deliveryID, "delivery:") || strings.TrimSpace(workspaceRevisionRef) == "" {
		return delivery.Candidate{}, fmt.Errorf("冻结 candidate 缺少合法 delivery_id/workspace_revision_ref")
	}
	root, err := m.workspaceRoot(workspaceID)
	if err != nil {
		return delivery.Candidate{}, err
	}
	owner, err := loadOwner(root)
	if err != nil {
		return delivery.Candidate{}, fmt.Errorf("读取 candidate workspace owner: %w", err)
	}
	if owner.Kind != OwnerDelivery || owner.DeliveryID != deliveryID {
		return delivery.Candidate{}, fmt.Errorf("冻结 candidate 的 workspace owner 与 delivery_id 不一致")
	}
	mf, err := loadManifest(filepath.Join(root, ManifestFileName))
	if err != nil {
		return delivery.Candidate{}, err
	}
	entries := mf.snapshot()
	if len(entries) == 0 {
		return delivery.Candidate{}, fmt.Errorf("冻结 candidate 拒绝空 dirty set")
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	type frozenFile struct {
		Path string        `json:"path"`
		Base manifestEntry `json:"base"`
		SHA  string        `json:"sha256"`
	}
	files := make([]frozenFile, 0, len(paths))
	for _, rel := range paths {
		data, readErr := os.ReadFile(filepath.Join(root, rel))
		if readErr != nil {
			return delivery.Candidate{}, fmt.Errorf("冻结 candidate 读取 %s: %w", rel, readErr)
		}
		sum := sha256.Sum256(data)
		files = append(files, frozenFile{Path: rel, Base: entries[rel], SHA: hex.EncodeToString(sum[:])})
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return delivery.Candidate{}, fmt.Errorf("编码 candidate manifest: %w", err)
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	return delivery.Candidate{
		Ref: deliveryID + "/candidate/" + digest[7:23], WorkspaceRevisionRef: workspaceRevisionRef,
		PatchDigest: digest, ManifestDigest: digest,
	}, nil
}

// ResolveWorkspaceRevision 实现 checkstore.WorkspaceRevisionResolver。Graph v3
// repair/acceptance 换 TaskID 但共享 Delivery dirty set，版本必须由
// candidate 实际内容而非当前 Task 的局部工具历史决定。
func (m *Manager) ResolveWorkspaceRevision(task *model.Task, taskStore store.TaskStore) (
	string, []string, bool, error,
) {
	if m == nil || task == nil || strings.TrimSpace(task.DeliveryID) == "" {
		return "", nil, false, nil
	}
	workspaceID := DeliveryWorkspaceID(task.DeliveryID)
	root, err := m.workspaceRoot(workspaceID)
	if err != nil {
		return "", nil, true, err
	}
	mf, err := loadManifest(filepath.Join(root, ManifestFileName))
	if err != nil {
		return "", nil, true, err
	}
	if len(mf.snapshot()) == 0 {
		return "workspace:empty", nil, true, nil
	}
	candidate, err := m.FreezeCandidate(task.DeliveryID, workspaceID, "workspace:pending")
	if err != nil {
		return "", nil, true, err
	}
	ref := "workspace:" + candidate.PatchDigest
	tasks, err := taskStore.ScanAll()
	if err != nil {
		return "", nil, true, err
	}
	seen := make(map[string]struct{})
	var effectRefs []string
	for _, related := range tasks {
		if related == nil || related.DeliveryID != task.DeliveryID {
			continue
		}
		records, queryErr := taskStore.QueryToolCalls(related.ID, "")
		if queryErr != nil {
			return "", nil, true, queryErr
		}
		for _, record := range records {
			if !record.Success || (record.ToolName != "write_file" && record.ToolName != "edit_file") ||
				strings.TrimSpace(record.CallID) == "" {
				continue
			}
			value := "tool-call:" + record.CallID
			if _, duplicate := seen[value]; duplicate {
				continue
			}
			seen[value] = struct{}{}
			effectRefs = append(effectRefs, value)
		}
	}
	sort.Strings(effectRefs)
	return ref, effectRefs, true, nil
}

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
	canonical, err := pathutil.ValidatePath(absMainPath, m.projectRoot)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(m.projectRoot, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// resolveRead 实现 View.ReadPath：只有 manifest 已登记的 dirty
// 业务文件才返回 workspace 副本，否则读穿透主根。不得以
// “物理文件存在”为命中依据，否则 owner/manifest/shell snapshot
// 会泄漏进业务路径命名空间。
func (v *View) resolveRead(absMainPath string) string {
	rel, ok := v.mgr.relPath(absMainPath)
	if !ok {
		return absMainPath
	}
	if _, hit := v.mf.get(rel); hit {
		return filepath.Join(v.root, rel)
	}
	return absMainPath
}

// resolveWrite 实现 View.WritePath 的 copy-on-write：
// workspace 副本不存在时——主根文件存在则复制其内容进 workspace（父目录
// MkdirAll）、同步落基线原始副本供三路合并，并在 manifest 记录基线
// SHA256；主根不存在则 manifest 记为新建（baseline 为空串）。
func (v *View) resolveWrite(absMainPath string) (string, error) {
	if v.mgr.ActiveView(v.taskID) != v {
		return "", fmt.Errorf("%w: %s", ErrViewNotFound, v.taskID)
	}
	if info, err := os.Stat(v.root); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrWorkspaceUnavailable, v.taskID)
	}
	rel, ok := v.mgr.relPath(absMainPath)
	if !ok {
		return "", fmt.Errorf("路径 %q 不在主根 %q 内，无法隔离写入", absMainPath, v.mgr.projectRoot)
	}
	if reservedWorkspaceRel(rel) {
		return "", fmt.Errorf("workspace_internal_path_forbidden: 业务路径 %q 与 workspace 控制面保留名冲突", rel)
	}
	wsPath := filepath.Join(v.root, rel)

	// 串行化检查-复制序列，避免并发 COW 同文件的竞态。
	v.mu.Lock()
	defer v.mu.Unlock()

	// 已有 manifest 副本直接返回写入位置。manifest.set 在
	// 工具 handler 获得路径前同步持久化，未登记的物理文件
	// 只能是控制元数据或崩溃留下的未执行基线副本，绝不得当业务文件复用。
	if _, hit := v.mf.get(rel); hit {
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

func reservedWorkspaceRel(rel string) bool {
	rel = filepath.Clean(rel)
	first := rel
	if index := strings.IndexRune(rel, filepath.Separator); index >= 0 {
		first = rel[:index]
	}
	return first == ownerFileName || first == ManifestFileName || first == baselineDirName ||
		first == shellRootDirName || strings.HasPrefix(first, ".workspace-shell-build-")
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
