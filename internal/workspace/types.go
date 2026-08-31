// Package workspace 实现按任务的写时复制（copy-on-write）执行隔离。
//
// 设计（2026-07-26 架构讨论定稿，取代 2026-04-08 删除的 git worktree 方案）：
//
//   - 隔离触发：节点声明式。Scheduler 在 publish_task 时设置
//     model.NodeCapability.Isolation.Mode = "workspace"。
//   - 执行语义：认领隔离任务的 Runner 运行在 overlay 视图中——
//     读穿透主根（workspace 未命中则读主根实时内容），写落任务专属
//     workspace（<projectRoot>/.agentgo/workspaces/<taskID>/）。
//     edit_file 对已有文件先 copy-on-write（从主根复制基线并记录基线
//     SHA256），write_file 的新文件直接落 workspace。
//   - 合并语义：任务成功终态由控制面（不经 LLM）把 dirty set 合并回主根：
//     基线 hash == 主根当前 hash → fast-forward 直接覆盖；
//     不一致 → 行级三路合并（Myers diff），干净则写入合并结果；
//     有冲突 → MergeResult.Conflicted=true，由执行面终止任务为 failed 并
//     自动 RequestReplan，Scheduler 裁决兜底。
//   - shell 残余风险（有意接受）：run_shell 在可丢弃完整项目快照中
//     运行，dirty set 在每次调用前覆盖；但命令写主根绝对路径仍不可完全阻止。
//
// 本文件放类型契约与导出方法（导出签名已冻结，B/C 线针对其编码）；
// 实现主体见 manager.go（生命周期与合并）、manifest.go（基线清单持久化）、
// merge3.go（行级三路合并，纯函数 Merge3）。
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"agentgo/internal/pathutil"
	"agentgo/internal/roster"
	"agentgo/internal/trace"
)

// DirName 是 projectRoot 下存放全部任务 workspace 的目录名。
const DirName = ".agentgo/workspaces"

// ManifestFileName 是 workspace 根下记录基线清单的文件名。
const ManifestFileName = ".workspace-manifest.json"

// DeliveryWorkspaceID 把逻辑 DeliveryID 映射为跨平台安全的目录名。Delivery
// identity 含冒号（语义上有用），但 Windows NTFS 不允许它作为文件名；不能
// 直接拿逻辑 ID 拼路径。
func DeliveryWorkspaceID(deliveryID string) string {
	sum := sha256.Sum256([]byte(deliveryID))
	return "delivery-" + hex.EncodeToString(sum[:16])
}

// ---------------------------------------------------------------------------
// 合并结果类型
// ---------------------------------------------------------------------------

// FileOutcome 是单文件合并结果。
type FileOutcome string

const (
	// OutcomeFastForward：主根自基线以来未变，直接覆盖。
	OutcomeFastForward FileOutcome = "fast_forward"
	// OutcomeAutoMerged：双方都变了但行区间不相交，三路合并干净。
	OutcomeAutoMerged FileOutcome = "auto_merged"
	// OutcomeNewFile：workspace 新建文件，主根无同名文件，直接落盘。
	OutcomeNewFile FileOutcome = "new_file"
	// OutcomeIdentical：双方内容一致（含同时新建同内容），无需写入。
	OutcomeIdentical FileOutcome = "identical"
	// OutcomeConflict：无法自动解决（行区间相交 / 删除-vs-修改 /
	// 双方新建不同内容）。
	OutcomeConflict FileOutcome = "conflict"
)

// ConflictRegion 描述一个三路合并冲突区域（行号基于基线文件，1-based）。
type ConflictRegion struct {
	BaseStart int    `json:"base_start"`
	BaseEnd   int    `json:"base_end"`
	Main      string `json:"main"`      // 主根版本的冲突区文本
	Workspace string `json:"workspace"` // workspace 版本的冲突区文本
}

// FileReport 是单文件的合并报告。
type FileReport struct {
	// Path 是主根下的绝对路径（合并目标位置）。
	Path      string           `json:"path"`
	Outcome   FileOutcome      `json:"outcome"`
	Detail    string           `json:"detail,omitempty"`
	Conflicts []ConflictRegion `json:"conflicts,omitempty"`
}

// MergeResult 是一次 MergeTask 的完整结果。
type MergeResult struct {
	Reports []FileReport `json:"reports"`
	// Conflicted 为 true 表示至少一个文件 OutcomeConflict；
	// 此时主根保证未被部分写入（冲突文件不落地，无冲突文件已落地——
	// 调用方按任务失败处理并保留 workspace 供排查）。
	Conflicted bool `json:"conflicted"`
}

// ConflictedPaths 返回所有冲突文件的主根绝对路径。
func (r *MergeResult) ConflictedPaths() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, rep := range r.Reports {
		if rep.Outcome == OutcomeConflict {
			out = append(out, rep.Path)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// View：单任务的 overlay 视图（执行期工具层使用）
// ---------------------------------------------------------------------------

// View 是某任务的 overlay 视图。其实现满足 tools.PathOverlayer 的结构性
// 接口（ReadPath/WritePath），由工具在 pathutil.ValidatePath 之后调用。
// 生命周期：Materialize 创建 → 任务执行期使用 → MergeTask 后 Cleanup。
type View struct {
	taskID string
	root   string // workspace 根绝对路径
	mgr    *Manager
	mf     *manifest  // dirty set 清单（基线 SHA256 + 新建标记），随 COW 即时持久化
	mu     sync.Mutex // 串行化 copy-on-write 的检查-复制序列
	// shellMu/shellReady 保护可丢弃的完整项目快照。稀疏 COW
	// 只能服务文件工具；Shell 需要真实目录树才能运行构建/测试。
	shellMu    sync.Mutex
	shellReady bool
}

// TaskID 返回视图所属任务。
func (v *View) TaskID() string { return v.taskID }

// Root 返回稀疏 workspace 根绝对路径；Shell 必须调用
// PrepareShellRoot 获取完整项目快照，不得直接用本根作 cwd。
func (v *View) Root() string { return v.root }

// PrepareShellRoot 返回一个完整、可执行的项目快照，并在每次
// Shell 调用前把 manifest dirty set 同步进去。该目录不进入
// candidate/merge，进程崩溃后也可安全重建。
func (v *View) PrepareShellRoot() (string, error) { return v.prepareShellRoot() }

// ReadPath 把主根绝对路径解析为实际读取位置：
// workspace 中已有副本（先前写过）则返回副本路径，否则原样返回主根路径。
func (v *View) ReadPath(absMainPath string) string {
	return v.resolveRead(absMainPath)
}

// WritePath 把主根绝对路径解析为 workspace 内的写入位置，并做
// copy-on-write：主根已存在该文件且 workspace 尚无副本时，复制基线进
// workspace 并在 manifest 记录基线 SHA256；主根不存在则记为新建文件。
// 返回的 workspace 路径的父目录由实现负责创建。
func (v *View) WritePath(absMainPath string) (string, error) {
	return v.resolveWrite(absMainPath)
}

// ---------------------------------------------------------------------------
// Swapper：按 Runner 共享的可切换 WorkdirProvider（认领时换入/恢复）
// ---------------------------------------------------------------------------

// Swapper 同时满足 tools.WorkdirProvider（Get）与 tools.PathOverlayer
// （ReadPath/WritePath）两个结构性接口。Get 恒返回主根——路径边界校验
// 永远面对主根；无活动 View 时 ReadPath/WritePath 为 passthrough（零开销）。
// 每个 Runner 持有独立 Swapper；认领隔离任务时 Agent 调 Activate 换入，
// defer 调返回的 restore 恢复。
type Swapper struct {
	main string
	mu   sync.RWMutex
	view *View
}

// NewSwapper 构造以 projectRoot 为主根的 Swapper。与 NewManager 同理，
// 构造期归一为绝对路径（相对根会让 View 的 relPath 对绝对目标路径报错）。
func NewSwapper(projectRoot string) *Swapper {
	return &Swapper{main: absRoot(projectRoot)}
}

// Get 实现 tools.WorkdirProvider：恒返回主根（校验边界不受隔离影响）。
func (s *Swapper) Get() string { return s.main }

// MainRoot 返回主根绝对路径。
func (s *Swapper) MainRoot() string { return s.main }

// ReadPath 实现 tools.PathOverlayer。
func (s *Swapper) ReadPath(absMainPath string) string {
	s.mu.RLock()
	v := s.view
	s.mu.RUnlock()
	if v == nil {
		return absMainPath
	}
	return v.ReadPath(absMainPath)
}

// WritePath 实现 tools.PathOverlayer。
func (s *Swapper) WritePath(absMainPath string) (string, error) {
	s.mu.RLock()
	v := s.view
	s.mu.RUnlock()
	if v == nil {
		return absMainPath, nil
	}
	return v.WritePath(absMainPath)
}

// Activate 换入任务视图，返回幂等的 restore（恢复为无视图状态）。
// Agent 在认领 Capability.Isolation 非 nil 的任务时调用。
func (s *Swapper) Activate(v *View) (restore func()) {
	s.mu.Lock()
	s.view = v
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.view = nil
			s.mu.Unlock()
		})
	}
}

// ActiveView 返回当前活动视图（nil = 未隔离）。run_shell 用它切换
// 默认工作目录到 workspace 根。
func (s *Swapper) ActiveView() *View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.view
}

// ---------------------------------------------------------------------------
// Manager：workspace 生命周期与合并（控制面使用）
// ---------------------------------------------------------------------------

// Manager 管理全部任务 workspace。构造后生命周期与进程一致；
// 合并回主根的写入经 roster 逐文件 TryClaim（roster 可为 nil = 跳过声明，
// 仅测试用）。
type Manager struct {
	projectRoot string
	roster      roster.Roster

	mu     sync.Mutex
	view   map[string]*View // workspaceID -> 已物化视图
	owners map[string]Owner
	leases map[string]int // workspaceID -> 当前正在使用该视图的 Activation 数
}

// NewManager 构造 Manager。projectRoot 为主根路径——构造期归一为绝对路径
// 保存：真实配置允许相对根（如 project_root: "."），而工具层经
// pathutil.ValidatePath 交给 relPath/resolveWrite 的目标路径恒为绝对路径，
// 相对根会让 filepath.Rel(相对, 绝对) 直接报错（2026-07-27 真实运行事故：
// 隔离任务全部「路径不在主根内」失败，单测因 t.TempDir() 恒绝对而漏检）。
func NewManager(projectRoot string, r roster.Roster) *Manager {
	return &Manager{projectRoot: absRoot(projectRoot), roster: r, view: make(map[string]*View),
		owners: make(map[string]Owner), leases: make(map[string]int)}
}

// absRoot 把根路径归一为绝对路径；Abs 失败（极罕见）时退化为 Clean 后的原值。
func absRoot(root string) string {
	if canonical, err := pathutil.CanonicalizeRoot(root); err == nil {
		return canonical
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	return abs
}

// ProjectRoot 返回主根绝对路径。
func (m *Manager) ProjectRoot() string { return m.projectRoot }

// Materialize 为任务创建（或复用，幂等——任务重试时复用既有 workspace）
// workspace 目录与视图，并登记为活动视图。
func (m *Manager) Materialize(taskID string) (*View, error) {
	return m.MaterializeOwned(taskID, TaskOwner(taskID))
}

// MaterializeOwned 以显式 owner 物化 workspace。Graph v3 必须使用
// DeliveryOwner；物理目录名不再承担逻辑 TaskID 语义。
func (m *Manager) MaterializeOwned(workspaceID string, owner Owner) (*View, error) {
	wsRoot, err := m.workspaceRoot(workspaceID)
	if err != nil {
		return nil, err
	}
	if err := owner.validate(workspaceID); err != nil {
		return nil, err
	}
	m.mu.Lock()
	// 幂等：已有活动视图直接返回（任务重试复用既有 workspace 与 dirty set）。
	if v, ok := m.view[workspaceID]; ok {
		if !sameOwner(m.owners[workspaceID], owner) {
			m.mu.Unlock()
			return nil, fmt.Errorf("workspace %s owner 冲突", workspaceID)
		}
		m.mu.Unlock()
		return v, nil
	}
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("创建 workspace 目录失败：%w", err)
	}
	if err := persistOwner(wsRoot, workspaceID, owner); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	// 重试复用时从磁盘恢复既有 manifest（幂等加载）。
	mf, err := loadManifest(filepath.Join(wsRoot, ManifestFileName))
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	v := &View{taskID: workspaceID, root: wsRoot, mgr: m, mf: mf}
	m.view[workspaceID] = v
	m.owners[workspaceID] = owner
	m.mu.Unlock()
	// 锁外 Emit：避免同步 Reactor 回调 Manager 造成死锁。
	trace.Emit(trace.Event{
		Kind: trace.KindWorkspaceMaterialized, TaskID: workspaceID,
		RunID: owner.RunID, GraphID: owner.GraphID, Path: wsRoot,
	})
	return v, nil
}

// Acquire 在视图换入 Runner 前取得活动租约。Cleanup 与 Acquire 使用同一把
// 锁，因此任何清扫都不能越过正在执行的 Agent Activation。
func (m *Manager) Acquire(workspaceID string) (func(), error) {
	m.mu.Lock()
	view, ok := m.view[workspaceID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrViewNotFound, workspaceID)
	}
	if info, err := os.Stat(view.root); err != nil || !info.IsDir() {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrWorkspaceUnavailable, workspaceID)
	}
	m.leases[workspaceID]++
	m.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			becameIdle := false
			if m.leases[workspaceID] <= 1 {
				delete(m.leases, workspaceID)
				becameIdle = true
			} else {
				m.leases[workspaceID]--
			}
			m.mu.Unlock()
			if becameIdle {
				m.discardShellSnapshotIfIdle(workspaceID)
			}
		})
	}, nil
}

// discardShellSnapshotIfIdle 只清理可丢弃的完整项目快照，
// 保留 owner/manifest/dirty/baseline。重检 lease 关闭 release→新 Acquire
// 窗口；Manager 锁与 View.shellMu 的顺序为唯一顺序。
func (m *Manager) discardShellSnapshotIfIdle(workspaceID string) {
	m.mu.Lock()
	if m.leases[workspaceID] > 0 {
		m.mu.Unlock()
		return
	}
	view := m.view[workspaceID]
	if view == nil {
		m.mu.Unlock()
		return
	}
	// 持有 Manager 锁阻止新 Acquire；discardShellRoot 内部串行化
	// 可能尚在收尾的 PrepareShellRoot。
	err := view.discardShellRoot()
	m.mu.Unlock()
	if err != nil {
		owner, _ := m.Owner(workspaceID)
		trace.Emit(trace.Event{Kind: trace.KindWorkspaceCleanupRejected, TaskID: workspaceID,
			RunID: owner.RunID, GraphID: owner.GraphID, Path: filepath.Join(view.root, shellRootDirName),
			Reason: "shell_snapshot_cleanup_failed", Description: err.Error()})
	}
}

func (m *Manager) InUse(workspaceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leases[workspaceID] > 0
}

func (m *Manager) Owner(workspaceID string) (Owner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[workspaceID]
	return owner, ok
}

// ActiveView 返回任务的活动视图（nil = 无）。
func (m *Manager) ActiveView(taskID string) *View {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.view[taskID]
}

// ResolveForTask 把主根绝对路径解析为该任务当前的物理位置：
// 任务有活动 workspace 且文件已在其中则返回 workspace 副本路径，
// 否则返回主根路径。record-artifact / ArtifactMeta 重算用它定位真实文件。
func (m *Manager) ResolveForTask(taskID, absMainPath string) string {
	if v := m.ActiveView(taskID); v != nil {
		return v.ReadPath(absMainPath)
	}
	return absMainPath
}

// MergeTask 把任务 workspace 的 dirty set 合并回主根（控制面操作，不经 LLM）。
// 逐文件：roster TryClaim(agentID, 主根路径) → 按 FileOutcome 规则落盘 → Release。
// 返回的 MergeResult.Conflicted=true 时冲突文件不落地。
// agentID 用于 roster 声明与 trace 归因。
func (m *Manager) MergeTask(ctx context.Context, taskID, agentID string) (*MergeResult, error) {
	wsRoot, err := m.workspaceRoot(taskID)
	if err != nil {
		return nil, err
	}
	// manifest 以磁盘为准（同步持久化保证与活动视图一致；视图注销后仍可合并）。
	mf, err := loadManifest(filepath.Join(wsRoot, ManifestFileName))
	if err != nil {
		return nil, err
	}
	entries := mf.snapshot()

	// 按 relPath 排序，保证合并顺序确定。
	rels := make([]string, 0, len(entries))
	for rel := range entries {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	result := &MergeResult{}
	owner, _ := m.Owner(taskID)
	counts := make(map[FileOutcome]int)
	for _, rel := range rels {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("合并中断：%w", err)
		}
		rep := m.mergeOne(agentID, rel, entries[rel], wsRoot)
		result.Reports = append(result.Reports, rep)
		counts[rep.Outcome]++
		if rep.Outcome == OutcomeConflict {
			result.Conflicted = true
			trace.Emit(trace.Event{
				Kind:        trace.KindWorkspaceMergeConflict,
				TaskID:      taskID,
				RunID:       owner.RunID,
				GraphID:     owner.GraphID,
				AgentID:     agentID,
				Path:        rep.Path,
				Description: fmt.Sprintf("合并冲突（%d 个冲突区域）：%s", len(rep.Conflicts), rep.Detail),
			})
		}
	}
	trace.Emit(trace.Event{
		Kind:    trace.KindWorkspaceMerged,
		TaskID:  taskID,
		RunID:   owner.RunID,
		GraphID: owner.GraphID,
		AgentID: agentID,
		Description: fmt.Sprintf(
			"合并完成：fast_forward=%d auto_merged=%d new_file=%d identical=%d conflict=%d",
			counts[OutcomeFastForward], counts[OutcomeAutoMerged],
			counts[OutcomeNewFile], counts[OutcomeIdentical], counts[OutcomeConflict]),
	})
	return result, nil
}

// Cleanup 删除任务 workspace 目录并注销活动视图。仅应在合并成功
// （或任务终态确认不再需要重试）后调用；失败/取消的孤儿目录交给
// Watchdog 经 ListOrphans 清扫。
func (m *Manager) Cleanup(taskID string) error {
	wsRoot, err := m.workspaceRoot(taskID)
	if err != nil {
		return err
	}
	// 防卫：解析后的目录必须直接位于 workspaces 根下，绝不删除根外路径。
	if filepath.Dir(wsRoot) != filepath.Join(m.projectRoot, DirName) {
		return fmt.Errorf("拒绝删除 workspace 根外路径：%s", wsRoot)
	}
	m.mu.Lock()
	owner := m.owners[taskID]
	if active := m.leases[taskID]; active > 0 {
		m.mu.Unlock()
		trace.Emit(trace.Event{Kind: trace.KindWorkspaceCleanupRejected, TaskID: taskID,
			RunID: owner.RunID, GraphID: owner.GraphID, Path: wsRoot,
			Reason: "active_lease", Description: fmt.Sprintf("leases=%d", active)})
		return fmt.Errorf("%w: %s leases=%d", ErrWorkspaceInUse, taskID, active)
	}
	if err := os.RemoveAll(wsRoot); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("删除 workspace 目录失败：%w", err)
	}
	delete(m.view, taskID)
	delete(m.owners, taskID)
	delete(m.leases, taskID)
	m.mu.Unlock()
	trace.Emit(trace.Event{
		Kind: trace.KindWorkspaceCleaned, TaskID: taskID,
		RunID: owner.RunID, GraphID: owner.GraphID, Path: wsRoot,
	})
	return nil
}

// ListWorkspaces 返回物理目录及其持久化 owner。旧目录没有 owner 文件时只
// 能按 legacy Task workspace 解释；delivery-* 缺 owner 一律报错并保留现场。
func (m *Manager) ListWorkspaces() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(m.projectRoot, DirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 workspace 根目录失败：%w", err)
	}
	out := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		workspaceID := entry.Name()
		root, rootErr := m.workspaceRoot(workspaceID)
		if rootErr != nil {
			return nil, rootErr
		}
		owner, ownerErr := loadOwner(root)
		if os.IsNotExist(ownerErr) {
			if strings.HasPrefix(workspaceID, "delivery-") {
				return nil, fmt.Errorf("delivery workspace %s 缺少 owner metadata，拒绝猜测清理", workspaceID)
			}
			out = append(out, Record{WorkspaceID: workspaceID, Owner: TaskOwner(workspaceID), Legacy: true})
			continue
		}
		if ownerErr != nil {
			return nil, ownerErr
		}
		if err := owner.validate(workspaceID); err != nil {
			return nil, err
		}
		out = append(out, Record{WorkspaceID: workspaceID, Owner: owner})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkspaceID < out[j].WorkspaceID })
	return out, nil
}

// ListOrphans 返回 workspace 根下所有任务目录的 taskID（不做存活判断，
// 由调用方结合 TaskStore 判定终态/失踪后调 Cleanup）。
// workspace 根不存在时返回 nil, nil。
func (m *Manager) ListOrphans() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.projectRoot, DirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 workspace 根目录失败：%w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

// ErrViewNotFound 表示任务没有活动 workspace（如合并已完成）。
var ErrViewNotFound = fmt.Errorf("任务无活动 workspace")

var ErrWorkspaceInUse = fmt.Errorf("workspace 正在被活动 Activation 使用")

var ErrWorkspaceUnavailable = fmt.Errorf("workspace 物理目录不存在或已失效")
