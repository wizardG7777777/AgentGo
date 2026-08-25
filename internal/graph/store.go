package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 本文件实现 V6 §6 第 9–14 条的 GraphStore：内存中的有类型 GraphDocument
// 是活跃图的主读写对象，JSON 仅作对外契约与持久化格式。变更 API 按角色
// 分离，字段所有权由 API 形状强制（V6 §6-9）：Scheduler 只能经
// DefinitionPatch 写定义字段，类型上不存在写 status/executor/execution
// 的途径；运行面写入一律 CAS state_version。
//
// 持久化 = 每图一份 snapshot.json + append-only journal.jsonl（journal.go）；
// 启动恢复见 recover.go；任何落盘失败使该图进入 persistence-degraded
// （变更 fail-closed，读取仍可用）。

// ============================================================
// 错误类型
// ============================================================

var (
	// ErrGraphNotFound 图不在内存索引中。
	ErrGraphNotFound = errors.New("graph: 图不存在")
	// ErrGraphExists SubmitGraph 时同 graph_id 的图已存在。
	ErrGraphExists = errors.New("graph: 图已存在")
	// ErrNodeNotFound 变更目标节点不存在。
	ErrNodeNotFound = errors.New("graph: 节点不存在")
	// ErrStoreClosed Store 已 Close，拒绝一切变更。
	ErrStoreClosed = errors.New("graph: Store 已关闭")
	// ErrRevisionConflict 定义面 CAS 失败；errors.Is 匹配，
	// errors.As 取 *RevisionConflictError 携带的当前 revision。
	ErrRevisionConflict = errors.New("graph: revision 冲突")
	// ErrStateVersionConflict 运行面 CAS 失败；errors.Is 匹配，
	// errors.As 取 *StateVersionConflictError 携带的当前 state_version。
	ErrStateVersionConflict = errors.New("graph: state_version 冲突")
	// ErrInvalidTransition 图/节点状态机拒绝的迁移。
	ErrInvalidTransition = errors.New("graph: 非法状态迁移")
	// ErrDegraded 图处于 persistence-degraded，变更 fail-closed。
	ErrDegraded = errors.New("graph: 持久化降级")
	// ErrTransitionExists 同一 (graph_id, source_activation_id, transition_id)
	// 的边选择已生效过（V6 §6-17 幂等身份）；errors.Is 匹配。
	ErrTransitionExists = errors.New("graph: 边选择已生效")
)

// RevisionConflictError 携带定义面 CAS 冲突详情。
type RevisionConflictError struct {
	GraphID string
	Base    int64 // 调用方声明的 baseRevision
	Current int64 // 当前 revision
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("%v: 图 %s 的 baseRevision=%d，当前 revision=%d",
		ErrRevisionConflict, e.GraphID, e.Base, e.Current)
}

func (e *RevisionConflictError) Is(target error) bool { return target == ErrRevisionConflict }

// StateVersionConflictError 携带运行面 CAS 冲突详情。
type StateVersionConflictError struct {
	GraphID string
	NodeID  string // 图级变更为空串
	Base    int64  // 调用方声明的 baseStateVersion
	Current int64  // 当前 state_version
}

func (e *StateVersionConflictError) Error() string {
	if e.NodeID != "" {
		return fmt.Sprintf("%v: 图 %s 节点 %s 的 baseStateVersion=%d，当前 state_version=%d",
			ErrStateVersionConflict, e.GraphID, e.NodeID, e.Base, e.Current)
	}
	return fmt.Sprintf("%v: 图 %s 的 baseStateVersion=%d，当前 state_version=%d",
		ErrStateVersionConflict, e.GraphID, e.Base, e.Current)
}

func (e *StateVersionConflictError) Is(target error) bool { return target == ErrStateVersionConflict }

// DegradedError 包装导致降级的底层落盘错误。
type DegradedError struct {
	GraphID string
	Err     error
}

func (e *DegradedError) Error() string {
	return fmt.Sprintf("%v: 图 %s 的变更无法落盘: %v", ErrDegraded, e.GraphID, e.Err)
}

func (e *DegradedError) Unwrap() error        { return e.Err }
func (e *DegradedError) Is(target error) bool { return target == ErrDegraded }

// TransitionExistsError 携带重复生效的边选择记录详情。
type TransitionExistsError struct {
	GraphID string
	Record  TransitionRecord
}

func (e *TransitionExistsError) Error() string {
	return fmt.Sprintf("%v: 图 %s 的 (source_activation=%s, transition_id=%d) 不允许重复生效",
		ErrTransitionExists, e.GraphID, e.Record.SourceActivationID, e.Record.TransitionID)
}

func (e *TransitionExistsError) Is(target error) bool { return target == ErrTransitionExists }

// ============================================================
// TransitionRecord —— 已生效边选择的 durable 簿记（V6 §6-17）
// ============================================================

// TransitionRecord 是一条已生效的边选择记录：幂等身份 =
// (graph_id, source_activation_id, transition_id)，同一来源 activation 的
// 同一条边最多生效一次；恢复时重放已记录事实，不重新猜测路由决定。
// 它与 GraphDocument 契约分离，存放在 entry 级簿记中（snapshot 存全量、
// journal 逐条重放，压缩后不丢失）。
type TransitionRecord struct {
	SourceNodeID       string `json:"source_node_id"`
	SourceActivationID string `json:"source_activation_id"`
	TransitionID       int    `json:"transition_id"` // 源节点 next 下标
	TargetNodeID       string `json:"target_node_id"`
	// TargetInput 冻结源边写入目标 activation 的输入端口；readiness 只读取
	// durable record，不在恢复时重新解释可能已 patch 的定义。
	TargetInput string `json:"target_input,omitempty"`
	// ReplayInputs 冻结 recovery retry 的输入复用语义；恢复时不得从最新
	// Definition 重新猜测。
	ReplayInputs bool `json:"replay_inputs,omitempty"`
	// ReplayedInputs 是 recovery retry 在选择边时从被恢复 Activation 的
	// Execution.Input 冻结复制的绑定；与本 TransitionRecord 同条 durable。
	ReplayedInputs []InputBinding `json:"replayed_inputs,omitempty"`
	// TargetActivationID 在边选择 durable 时一并冻结。目标若已有在途
	// activation 则指向它；否则预留下一个单调 ID。恢复不再根据目标当前
	// status 猜测这条边是否已经创建过新 activation。
	TargetActivationID string `json:"target_activation_id,omitempty"`
	// Input 是这条实际生效转移绑定的源 activation 输出（数据流图持久化
	// InputRef，见 types.go EdgeInput）：与边选择同条 journal 记录落盘。
	// 历史 journal 记录无此字段（nil），消费方按"仅有来源标识"回落。
	Input *EdgeInput `json:"input,omitempty"`
}

// transitionPayload 是 transition journal 记录的 payload，与 TransitionRecord 同形。
type transitionPayload = TransitionRecord

// transitionKey 是 entry 级边选择簿记的键（graph_id 由 entry 本身确定）。
type transitionKey struct {
	sourceActivationID string
	transitionID       int
}

// sortedTransitionRecords 把边选择集合输出为确定顺序的切片
// （source_activation_id → transition_id → target_node_id 升序）。
func sortedTransitionRecords(set map[transitionKey]TransitionRecord) []TransitionRecord {
	out := make([]TransitionRecord, 0, len(set))
	for _, rec := range set {
		out = append(out, cloneTransitionRecord(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.SourceActivationID != b.SourceActivationID {
			return a.SourceActivationID < b.SourceActivationID
		}
		if a.TransitionID != b.TransitionID {
			return a.TransitionID < b.TransitionID
		}
		return a.TargetNodeID < b.TargetNodeID
	})
	return out
}

func cloneTransitionRecord(in TransitionRecord) TransitionRecord {
	out := in
	out.ReplayedInputs = cloneInputBindings(in.ReplayedInputs)
	if in.Input != nil {
		input := *in.Input
		input.EvidenceRefs = append([]string(nil), in.Input.EvidenceRefs...)
		input.Result = append(json.RawMessage(nil), in.Input.Result...)
		input.Evidence = make([]EvidenceEntry, len(in.Input.Evidence))
		for i := range in.Input.Evidence {
			input.Evidence[i] = cloneEvidenceEntry(in.Input.Evidence[i])
		}
		out.Input = &input
	}
	return out
}

// activationResultRef 是 activation 级 Result 的稳定身份。graph_id 与
// activation_id 都已经过各自权威校验，引用只作不透明字符串比较/解引用。
func activationResultRef(graphID, activationID string) string {
	return "graph-result:" + graphID + ":" + activationID
}

func sortedActivationResults(set map[string]ActivationResult) []ActivationResult {
	refs := make([]string, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]ActivationResult, 0, len(refs))
	for _, ref := range refs {
		out = append(out, cloneActivationResult(set[ref]))
	}
	return out
}

func cloneActivationResult(in ActivationResult) ActivationResult {
	out := in
	out.Result = append(json.RawMessage(nil), in.Result...)
	out.Evidence = make([]EvidenceEntry, len(in.Evidence))
	for i := range in.Evidence {
		out.Evidence[i] = cloneEvidenceEntry(in.Evidence[i])
	}
	return out
}

func cloneEvidenceEntry(in EvidenceEntry) EvidenceEntry {
	out := in
	if in.Success != nil {
		value := *in.Success
		out.Success = &value
	}
	if in.ExitCode != nil {
		value := *in.ExitCode
		out.ExitCode = &value
	}
	return out
}

func sameActivationResult(a, b ActivationResult) bool {
	if a.Ref != b.Ref || a.NodeID != b.NodeID || a.ActivationID != b.ActivationID ||
		!bytes.Equal(a.Result, b.Result) || len(a.Evidence) != len(b.Evidence) {
		return false
	}
	for i := range a.Evidence {
		if !sameEvidenceEntry(a.Evidence[i], b.Evidence[i]) {
			return false
		}
	}
	return true
}

func sameEvidenceEntry(a, b EvidenceEntry) bool {
	if a.Ref != b.Ref || a.Kind != b.Kind || a.Summary != b.Summary ||
		a.CallID != b.CallID || a.ToolName != b.ToolName ||
		a.Command != b.Command || a.CommandTruncated != b.CommandTruncated ||
		a.Path != b.Path || a.PathTruncated != b.PathTruncated {
		return false
	}
	if (a.Success == nil) != (b.Success == nil) || (a.ExitCode == nil) != (b.ExitCode == nil) {
		return false
	}
	if a.Success != nil && *a.Success != *b.Success {
		return false
	}
	return a.ExitCode == nil || *a.ExitCode == *b.ExitCode
}

// ============================================================
// DefinitionPatch —— Scheduler 定义面的唯一变更入口
// ============================================================

// NodeDefUpsert 只承载节点的定义字段与扩展字段。upsert 已存在节点时整体
// 替换定义字段，status/executor/execution 运行字段原样保留——类型上不
// 存在经 DefinitionPatch 写运行字段的途径（V6 §6-9 的强制点：Scheduler
// 不能整图覆盖伪造 completed 或占用者身份）。
type NodeDefUpsert struct {
	ID                  string                     `json:"id"`
	Kind                NodeKind                   `json:"kind"`
	Task                *NodeTask                  `json:"task,omitempty"`
	Capability          *Capability                `json:"capability,omitempty"`
	Next                []Transition               `json:"next"`
	Wait                *WaitSpec                  `json:"wait,omitempty"`
	Tool                *ToolSpec                  `json:"tool,omitempty"`
	Subgraph            *SubgraphSpec              `json:"subgraph,omitempty"`
	EndOutcome          EndOutcome                 `json:"end_outcome,omitempty"`
	OutputContract      *NodeOutputContract        `json:"output_contract,omitempty"`
	ProgressContractRef string                     `json:"progress_contract_ref,omitempty"`
	ContextPolicyRef    string                     `json:"context_policy_ref,omitempty"`
	Metadata            map[string]string          `json:"metadata,omitempty"`
	Extensions          map[string]json.RawMessage `json:"extensions,omitempty"`
}

// DefinitionPatch 是一次定义面变更，按固定顺序应用：删除节点 → upsert
// 节点 → 改 root。应用后在候选副本上重跑语义校验链（validateSemantics +
// validateRuntimeState），任一失败整体不生效。
type DefinitionPatch struct {
	RemoveNodes []string        `json:"remove_nodes,omitempty"`
	UpsertNodes []NodeDefUpsert `json:"upsert_nodes,omitempty"`
	Root        *string         `json:"root,omitempty"`
}

// empty 报告 patch 是否不含任何操作（空 patch 拒绝，避免无意义 revision 膨胀）。
func (p *DefinitionPatch) empty() bool {
	return len(p.RemoveNodes) == 0 && len(p.UpsertNodes) == 0 && p.Root == nil
}

// ============================================================
// Store 与 entry
// ============================================================

// GraphSummary 是 List 返回的单图摘要。
type GraphSummary struct {
	GraphID      string
	Revision     int64
	StateVersion int64
	Status       GraphStatus
	Root         string
	NodeCount    int
	Digest       string
	Seq          int64  // 已落盘的 journal 最大 seq
	Degraded     bool   // 是否处于 persistence-degraded
	SessionID    string // 图的 session 归属（空串表示尚未归并的历史图）
}

// Store 是 GraphDocument 的内存主索引 + 持久化协调器。同一图的变更经
// entry 锁串行，不同图并行；普通读取全部来自内存深拷贝，不碰硬盘；
// 不存在「并发 Runner 各自读文件后覆盖整图」的任何路径（V6 §6-11）。
type Store struct {
	dir     string
	mu      sync.RWMutex // 护 entries 与 closed
	entries map[string]*entry
	closed  bool

	// OnDegraded 在图进入 persistence-degraded 时调用（挂告警用，V6 §6-13）。
	// 在 entry 锁外同步触发，回调内可安全调用 Store 的读取方法。
	OnDegraded func(graphID string, err error)
}

// entry 是单图的内存态：文档指针、journal writer、持久化游标与降级状态。
type entry struct {
	mu             sync.RWMutex
	doc            *GraphDocument // 已提交内存快照；替换指针即生效
	journal        journalSink
	seq            int64  // 已落盘的 journal 最大 seq
	digest         string // 已落盘状态的执行语义摘要
	chainDigest    string // 已落盘 journal 的链式完整性头（snapshot 会冻结）
	journalEntries int64  // 距上次 snapshot 的 journal 条目数（压缩阈值）
	journalBytes   int64  // 距上次 snapshot 的 journal 字节数（压缩阈值，近似）
	degraded       error  // 非 nil 即 persistence-degraded（记录首个失败）
	dir            string // <store.dir>/<graph_id>

	// Graph Runtime 的 entry 级簿记（不属 GraphDocument 契约；snapshot 存
	// 全量、journal 逐条重放，压缩截断后仍可重建，见 journal.go/recover.go）：
	transitions       map[transitionKey]TransitionRecord // 已生效边选择（V6 §6-17 幂等身份）
	activationResults map[string]ActivationResult        // result_ref → activation 级完整 Result
	activationSeq     map[string]int                     // node_id → 已分配的最大 activation 序号（V6 §6-16）
}

// NewStore 创建以 dir 为持久化根的 Store（布局 <dir>/<graph_id>/snapshot.json
// + journal.jsonl）。构造时不读盘；启动恢复显式调用 Recover。
func NewStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("graph: 持久化根目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("graph: 创建持久化根目录: %w", err)
	}
	return &Store{dir: dir, entries: make(map[string]*entry)}, nil
}

// graphDir 返回图的持久化目录。逻辑 graph_id 先经完整校验，再把 Windows
// 不可表示的分段（如含 ':' 或设备名 CON）可逆编码；普通分段保持原目录名。
// "/" 仍映射为嵌套目录（subgraph 物化子图住在父图目录下）。
func (s *Store) graphDir(graphID string) (string, error) {
	dir, err := graphStoragePath(s.dir, graphID)
	if err != nil {
		return "", fmt.Errorf("graph: graph_id %q 不能映射为持久化目录: %w", graphID, err)
	}
	return dir, nil
}

// ============================================================
// 读取（全部走内存，不碰硬盘）
// ============================================================

// Get 返回图的深拷贝（调用方改写不影响 Store 内的共享内存）。
func (s *Store) Get(graphID string) (*GraphDocument, bool) {
	e, ok := s.lookup(graphID)
	if !ok {
		return nil, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	cp, err := cloneDoc(e.doc)
	if err != nil {
		return nil, false // 内存对象不含不可序列化内容，理论不可达
	}
	return cp, true
}

// List 返回全部图的摘要（按 graph_id 排序，结果确定）。
func (s *Store) List() []GraphSummary {
	s.mu.RLock()
	entries := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	s.mu.RUnlock()
	out := make([]GraphSummary, 0, len(entries))
	for _, e := range entries {
		e.mu.RLock()
		out = append(out, GraphSummary{
			GraphID:      e.doc.GraphID,
			Revision:     e.doc.Revision,
			StateVersion: e.doc.StateVersion,
			Status:       e.doc.Status,
			Root:         e.doc.Root,
			NodeCount:    len(e.doc.Nodes),
			Digest:       e.digest,
			Seq:          e.seq,
			Degraded:     e.degraded != nil,
			SessionID:    e.doc.SessionID,
		})
		e.mu.RUnlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GraphID < out[j].GraphID })
	return out
}

// Digest 返回图当前已落盘状态的执行语义摘要。
func (s *Store) Digest(graphID string) (string, bool) {
	e, ok := s.lookup(graphID)
	if !ok {
		return "", false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.digest, true
}

// Degraded 查询图是否处于 persistence-degraded；是则返回首个落盘错误。
func (s *Store) Degraded(graphID string) (error, bool) {
	e, ok := s.lookup(graphID)
	if !ok {
		return nil, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.degraded, e.degraded != nil
}

func (s *Store) lookup(graphID string) (*entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[graphID]
	return e, ok
}

// lookupForMutation 供变更路径使用：Store 关闭后拒绝一切变更。
func (s *Store) lookupForMutation(graphID string) (*entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrStoreClosed
	}
	e, ok := s.entries[graphID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrGraphNotFound, graphID)
	}
	return e, nil
}

// ============================================================
// 变更 API（按角色分离）
// ============================================================

// SubmitGraph 新建一张图。doc 先序列化重走完整 ParseAndValidate（含
// 「新图必须 pending、节点运行面必须为空」约束），revision 归一为 1、
// state_version 归 0；
// submit 全量写入 journal 并 fsync 成功后才入内存索引——关键事实先
// durable 再对外确认（V6 §6-12）。落盘失败时图不入索引，直接返回错误。
func (s *Store) SubmitGraph(doc *GraphDocument) error {
	if doc != nil && (doc.DefinitionDigestVersion != "" || doc.DefinitionDigest != "" ||
		doc.ContractDigest != "" || doc.SourceProposalID != "") {
		return fmt.Errorf("graph: legacy SubmitGraph 不得伪造 Authoring Definition 绑定；请走 Draft/Commit/Start adapter")
	}
	return s.submitGraph(doc, true)
}

// createExecution 从 AuthoringStore 已提交的 immutable Definition 创建一张
// 尚未激活的 GraphExecution。与 legacy SubmitGraph 的区别：保留 Definition
// revision，并强制 durable definition/contract 绑定；本方法只写 submit 事实，
// 不把图置 running、不创建 root Activation。
func (s *Store) createExecution(doc *GraphDocument) error {
	if doc == nil {
		return fmt.Errorf("graph: 创建 Execution 的文档为 nil")
	}
	if doc.Revision <= 0 || strings.TrimSpace(doc.DefinitionDigestVersion) == "" ||
		strings.TrimSpace(doc.DefinitionDigest) == "" || strings.TrimSpace(doc.ContractDigest) == "" ||
		strings.TrimSpace(doc.SourceProposalID) == "" {
		return fmt.Errorf("graph: Authoring Execution 必须绑定正 revision、definition digest/version、contract digest 与 source proposal")
	}
	return s.submitGraph(doc, false)
}

func (s *Store) submitGraph(doc *GraphDocument, normalizeRevision bool) error {
	if doc == nil {
		return fmt.Errorf("graph: 提交的文档为 nil")
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("graph: 序列化提交文档: %w", err)
	}
	parsed, err := ParseAndValidate(data)
	if err != nil {
		return err
	}
	// legacy 提交从 revision=1 开始；Authoring Execution 保留 immutable
	// Definition revision。两条路径的运行 state_version 都从 0 开始。
	if normalizeRevision {
		parsed.Revision = 1
	}
	parsed.StateVersion = 0

	dir, err := s.graphDir(parsed.GraphID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrStoreClosed
	}
	if _, ok := s.entries[parsed.GraphID]; ok {
		return fmt.Errorf("%w: %s", ErrGraphExists, parsed.GraphID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("graph: 创建图目录: %w", err)
	}
	// O_EXCL 防止覆盖磁盘上已存在的同名 journal（未 Recover 的残留）。
	jw, err := openJournal(filepath.Join(dir, journalFileName), true)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("graph: 图 %s 的 journal 已存在于磁盘（是否遗漏启动 Recover？）: %w", parsed.GraphID, err)
		}
		return fmt.Errorf("graph: 创建 journal: %w", err)
	}
	line, digest, chainDigest, err := buildJournalLine(1, journalKindSubmit, parsed, submitPayload{Doc: parsed}, "")
	if err != nil {
		_ = jw.close()
		return err
	}
	if err := jw.append(line); err != nil {
		_ = jw.close()
		return fmt.Errorf("graph: 提交事实落盘失败: %w", err)
	}
	s.entries[parsed.GraphID] = &entry{
		doc:               parsed,
		journal:           jw,
		seq:               1,
		digest:            digest,
		chainDigest:       chainDigest,
		journalEntries:    1,
		journalBytes:      int64(len(line)) + 1,
		dir:               dir,
		transitions:       make(map[transitionKey]TransitionRecord),
		activationResults: make(map[string]ActivationResult),
		activationSeq:     activationSeqFromDoc(parsed),
	}
	return nil
}

// PatchGraph 是 Scheduler 定义面的唯一变更入口（V6 §6-9）：只能改定义
// 字段（upsert 节点的 kind/task/capability/next/metadata/extensions、删除
// 节点、改 root）。baseRevision 与当前不符返回 *RevisionConflictError
// （携带当前 revision）。应用后在候选副本上重跑语义校验链，通过则
// revision+1、state_version+1、重算 digest，journal append + fsync 后生效。
func (s *Store) PatchGraph(graphID string, baseRevision int64, patch DefinitionPatch) (int64, error) {
	if patch.empty() {
		return 0, fmt.Errorf("graph: 空 patch（至少包含一项定义操作）")
	}
	var newRevision int64
	err := s.mutate(graphID, journalKindPatch, patchPayload{Patch: patch}, true, func(c *GraphDocument) error {
		if c.Revision != baseRevision {
			return &RevisionConflictError{GraphID: graphID, Base: baseRevision, Current: c.Revision}
		}
		if err := validatePatchRuntimeSafety(c, &patch); err != nil {
			return err
		}
		if err := applyPatch(c, &patch); err != nil {
			return err
		}
		if err := validateAuthoringSemantics(c); err != nil {
			return err
		}
		c.Revision++
		c.StateVersion++
		newRevision = c.Revision
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRevision, nil
}

// adoptDefinition 把已由 Authoring Compiler/Store commit 的新 immutable
// Definition revision 原子换入运行图；旧 Activation 继续读取 Execution.Definition，
// 只有未来 Activation 读取更新后的 Nodes。运行中的 root 不允许改变。
func (s *Store) adoptDefinition(graphID string, baseRevision int64, baseDigest string, definition *GraphDocument) error {
	if definition == nil {
		return fmt.Errorf("graph: adopt Definition 不能为空")
	}
	return s.mutate(graphID, journalKindDefinitionAdopted,
		definitionAdoptPayload{Definition: definition}, true, func(c *GraphDocument) error {
			if c.Revision != baseRevision {
				return &RevisionConflictError{GraphID: graphID, Base: baseRevision, Current: c.Revision}
			}
			if c.DefinitionDigest != baseDigest {
				return fmt.Errorf("graph: 当前 Execution definition digest=%s 与 Change base=%s 不一致", c.DefinitionDigest, baseDigest)
			}
			if c.Status.IsTerminal() {
				return fmt.Errorf("graph: 终态 Execution %s 不接受新 Definition", graphID)
			}
			if definition.GraphID != graphID || definition.Revision != baseRevision+1 || definition.Root != c.Root {
				return fmt.Errorf("graph: 新 Definition identity/revision/root 与运行中 Execution 不一致")
			}
			if definition.SessionID != c.SessionID || definition.RunID != c.RunID || !reflect.DeepEqual(definition.RunContract, c.RunContract) {
				return fmt.Errorf("graph: 新 Definition 不得改变 Session/Run identity")
			}
			for id, old := range c.Nodes {
				if _, kept := definition.Nodes[id]; !kept && old.Execution != nil {
					return fmt.Errorf("graph: 新 Definition 不能删除已有 activation 的节点 %q", id)
				}
			}
			if err := applyDefinitionAdoption(c, definition); err != nil {
				return err
			}
			if err := validateAuthoringSemantics(c); err != nil {
				return err
			}
			return nil
		})
}

func applyDefinitionAdoption(current, definition *GraphDocument) error {
	if current == nil || definition == nil {
		return fmt.Errorf("graph: Definition adoption 输入为空")
	}
	nodes := make(map[string]Node, len(definition.Nodes))
	for id, next := range definition.Nodes {
		if old, exists := current.Nodes[id]; exists {
			next.Status, next.Executor, next.Execution = old.Status, old.Executor, old.Execution
		} else {
			next.Status, next.Executor, next.Execution = NodeInactive, nil, nil
		}
		nodes[id] = next
	}
	current.Schema = definition.Schema
	current.Revision = definition.Revision
	current.Root = definition.Root
	current.Nodes = nodes
	current.DefinitionDigestVersion = definition.DefinitionDigestVersion
	current.DefinitionDigest = definition.DefinitionDigest
	current.ContractDigest = definition.ContractDigest
	current.SourceProposalID = definition.SourceProposalID
	current.StateVersion++
	return nil
}

// validatePatchRuntimeSafety 只拦截无法保留既有 activation 身份的操作：已有
// activation 的节点定义可以 upsert（Execution.Definition 冻结旧语义），
// 但图仍运行时不能从承载运行事实的 Nodes 表中物理删除，即使该 activation
// 已终态——join 与 durable transition 仍可能引用它。整图终态后才可清理。
func validatePatchRuntimeSafety(doc *GraphDocument, patch *DefinitionPatch) error {
	for _, id := range patch.RemoveNodes {
		node, ok := doc.Nodes[id]
		if !ok {
			continue // applyPatch 负责稳定的“不存在”诊断
		}
		if !doc.Status.IsTerminal() && node.Execution != nil {
			return fmt.Errorf("graph: patch 不能从非终态图删除已有 activation 的节点 %q（status=%s activation=%s）；其运行事实和 transition/join 引用必须保留",
				id, node.Status, activationIDOf(node))
		}
	}
	return nil
}

func activationIDOf(node Node) string {
	if node.Execution == nil {
		return ""
	}
	return node.Execution.ActivationID
}

// SetNodeStatus 是 Graph Runtime 写节点状态的入口：状态机校验 +
// state_version CAS；仅 state_version+1，revision 与 digest 不变。
func (s *Store) SetNodeStatus(graphID, nodeID string, to NodeStatus, stateVersion int64) error {
	return s.mutate(graphID, journalKindNodeStatus, nodeStatusPayload{NodeID: nodeID, To: to}, false,
		func(c *GraphDocument) error {
			if err := checkStateVersion(c, nodeID, stateVersion); err != nil {
				return err
			}
			n, ok := c.Nodes[nodeID]
			if !ok {
				return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
			}
			if !IsValidNodeStatusTransition(n.Status, to) {
				return fmt.Errorf("%w: 图 %s 节点 %s 不能从 %q 迁到 %q",
					ErrInvalidTransition, graphID, nodeID, n.Status, to)
			}
			n.Status = to
			c.Nodes[nodeID] = n
			c.StateVersion++
			return nil
		})
}

// SetExecutor 是调度/认领系统写节点 executor 的入口（认领事实）。
// 形状校验（type 仅 "agent"、agent_id 非空）与 validate.go 阶段 9 一致。
func (s *Store) SetExecutor(graphID, nodeID string, exec Executor, stateVersion int64) error {
	if exec.Type != ExecutorTypeAgent {
		return fmt.Errorf("graph: executor.type 仅允许 %q，实际为 %q", ExecutorTypeAgent, exec.Type)
	}
	if strings.TrimSpace(exec.AgentID) == "" {
		return fmt.Errorf("graph: executor.agent_id 不能为空")
	}
	return s.mutate(graphID, journalKindExecutor, executorPayload{NodeID: nodeID, Executor: exec}, false,
		func(c *GraphDocument) error {
			if err := checkStateVersion(c, nodeID, stateVersion); err != nil {
				return err
			}
			n, ok := c.Nodes[nodeID]
			if !ok {
				return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
			}
			ex := exec
			n.Executor = &ex
			c.Nodes[nodeID] = n
			c.StateVersion++
			return nil
		})
}

// SetExecution 是 Agent Loop / Harness 写执行事实的入口
// （phase/task_id/activation_id/result_ref/evidence_refs）。
func (s *Store) SetExecution(graphID, nodeID string, exec Execution, stateVersion int64) error {
	if strings.TrimSpace(exec.Phase) == "" {
		return fmt.Errorf("graph: execution.phase 不能为空")
	}
	if exec.ActivationID != "" {
		if owner, _, ok := parseActivationID(exec.ActivationID); !ok || owner != nodeID {
			return fmt.Errorf("graph: execution.activation_id %q 不属于节点 %q", exec.ActivationID, nodeID)
		}
	}
	return s.mutate(graphID, journalKindExecution, executionPayload{NodeID: nodeID, Execution: exec}, false,
		func(c *GraphDocument) error {
			if err := checkStateVersion(c, nodeID, stateVersion); err != nil {
				return err
			}
			n, ok := c.Nodes[nodeID]
			if !ok {
				return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
			}
			ex := exec
			n.Execution = &ex
			c.Nodes[nodeID] = n
			c.StateVersion++
			return nil
		})
}

// SetGraphStatus 是 Graph Runtime 写图状态的入口：图状态机校验 +
// state_version CAS；仅 state_version+1。
func (s *Store) SetGraphStatus(graphID string, to GraphStatus, stateVersion int64) error {
	if to == GraphBlocked {
		return fmt.Errorf("graph: blocked 必须通过 CommitGraphOutcome 原子提交，禁止只写 GraphStatus")
	}
	return s.mutate(graphID, journalKindGraphStatus, graphStatusPayload{To: to}, false,
		func(c *GraphDocument) error {
			if err := checkStateVersion(c, "", stateVersion); err != nil {
				return err
			}
			if !IsValidGraphStatusTransition(c.Status, to) {
				return fmt.Errorf("%w: 图 %s 不能从 %q 迁到 %q",
					ErrInvalidTransition, graphID, c.Status, to)
			}
			c.Status = to
			c.StateVersion++
			return nil
		})
}

// CommitGraphOutcome 将业务 outcome 与派生 GraphStatus 放在同一条 journal 记录
// 和同一次内存变更中。新 typed end 必须使用本入口；SetGraphStatus 只保留
// legacy/runtime 兼容。
func (s *Store) CommitGraphOutcome(graphID string, outcome GraphOutcomeRecord, stateVersion int64) error {
	status := outcome.Status()
	if status == "" || !outcome.Outcome.IsValid() {
		return fmt.Errorf("graph: 非法 EndOutcome %q", outcome.Outcome)
	}
	if outcome.CommittedAt.IsZero() {
		outcome.CommittedAt = time.Now().UTC()
	}
	return s.mutate(graphID, journalKindGraphOutcome, graphOutcomePayload{Outcome: outcome}, false, func(c *GraphDocument) error {
		if c.StateVersion != stateVersion {
			return &StateVersionConflictError{GraphID: graphID, Base: stateVersion, Current: c.StateVersion}
		}
		if c.Outcome != nil {
			if c.Outcome.Outcome == outcome.Outcome && c.Outcome.EndActivationID == outcome.EndActivationID {
				return nil
			}
			return fmt.Errorf("graph: 图 %s 已提交不同 outcome=%s", graphID, c.Outcome.Outcome)
		}
		if !IsValidGraphStatusTransition(c.Status, status) {
			return fmt.Errorf("%w: graph %s %s -> %s", ErrInvalidTransition, graphID, c.Status, status)
		}
		copy := outcome
		c.Outcome = &copy
		c.Status = status
		c.StateVersion++
		if err := validateRuntimeState(c); err != nil {
			return fmt.Errorf("graph: outcome 运行状态非法: %w", err)
		}
		return nil
	})
}

// SetExecutionAndStatus 是 Graph Runtime 写「execution + 节点状态」的原子入口
// （V6 §6-15/16）：activation 创建（ready）、任务发布成功（running）、节点
// 终态与挂起都必须单条 journal 记录生效——拆成 execution/status 两条会在
// 两者之间留下崩溃窗口（durable 说已激活/已终结但状态对不上）。因此执行
// activation_id 强制非空：Graph Runtime 的一切节点写入都携带 activation 上下文。
func (s *Store) SetExecutionAndStatus(graphID, nodeID string, exec Execution, to NodeStatus, stateVersion int64) error {
	if strings.TrimSpace(exec.Phase) == "" {
		return fmt.Errorf("graph: execution.phase 不能为空")
	}
	if strings.TrimSpace(exec.ActivationID) == "" {
		return fmt.Errorf("graph: execution.activation_id 不能为空（Graph Runtime 写入必须携带 activation 上下文）")
	}
	if owner, _, ok := parseActivationID(exec.ActivationID); !ok || owner != nodeID {
		return fmt.Errorf("graph: execution.activation_id %q 不属于节点 %q", exec.ActivationID, nodeID)
	}
	return s.mutate(graphID, journalKindExecutionStatus,
		executionStatusPayload{NodeID: nodeID, Execution: exec, To: to}, false,
		func(c *GraphDocument) error {
			if err := checkStateVersion(c, nodeID, stateVersion); err != nil {
				return err
			}
			n, ok := c.Nodes[nodeID]
			if !ok {
				return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, nodeID)
			}
			if !IsValidNodeStatusTransition(n.Status, to) {
				return fmt.Errorf("%w: 图 %s 节点 %s 不能从 %q 迁到 %q",
					ErrInvalidTransition, graphID, nodeID, n.Status, to)
			}
			ex := exec
			n.Execution = &ex
			n.Status = to
			c.Nodes[nodeID] = n
			c.StateVersion++
			return nil
		})
}

// RecordTransition 记录一条边选择生效事实（V6 §6-17，durable）。同一
// (graph_id, source_activation_id, transition_id) 已生效过则返回
// *TransitionExistsError（errors.Is 匹配 ErrTransitionExists）——调用方
// 应先 HasTransition 查询再决定是否激活目标，重复记录是防御性兜底。
func (s *Store) RecordTransition(graphID string, rec TransitionRecord, stateVersion int64) error {
	rec = cloneTransitionRecord(rec)
	return s.mutate(graphID, journalKindTransition, transitionPayload(rec), false,
		func(c *GraphDocument) error {
			if err := checkStateVersion(c, "", stateVersion); err != nil {
				return err
			}
			if err := validateTransitionRecord(graphID, c, rec, nil, false); err != nil {
				return err
			}
			c.StateVersion++
			return nil
		})
}

// HasTransition 查询某来源 activation 的指定边是否已生效。
func (s *Store) HasTransition(graphID, sourceActivationID string, transitionID int) (TransitionRecord, bool) {
	e, ok := s.lookup(graphID)
	if !ok {
		return TransitionRecord{}, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	rec, ok := e.transitions[transitionKey{sourceActivationID, transitionID}]
	return cloneTransitionRecord(rec), ok
}

// Transitions 返回图已生效的边选择记录（确定顺序，见 sortedTransitionRecords）。
func (s *Store) Transitions(graphID string) []TransitionRecord {
	e, ok := s.lookup(graphID)
	if !ok {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return sortedTransitionRecords(e.transitions)
}

// RecordActivationResult 在任一边选择前持久化 activation 的完整 Result 与
// 可解引用证据。身份是 (graph_id, activation_id) 的确定函数；重复写入完全
// 相同内容幂等，试图用同一 activation 改写结果则 fail-closed。
//
// 该 entry 级记录不改变 GraphDocument/state_version，但占用独立 journal seq，
// 并随 snapshot 压缩保留。
func (s *Store) RecordActivationResult(graphID string, rec ActivationResult) error {
	wantRef := activationResultRef(graphID, rec.ActivationID)
	if rec.Ref == "" {
		rec.Ref = wantRef
	}
	rec = cloneActivationResult(rec)
	return s.mutate(graphID, journalKindActivationResult, activationResultPayload(rec), false,
		func(c *GraphDocument) error {
			if err := validateActivationResultRecord(graphID, c, rec, true); err != nil {
				return err
			}
			return nil // entry 级簿记，不改变 GraphDocument/state_version
		})
}

func validateEvidenceEntryBounds(ev EvidenceEntry) error {
	if ev.ExitCodeScope != "" && ev.ExitCodeScope != "whole_command" &&
		ev.ExitCodeScope != "last_pipeline_command" {
		return fmt.Errorf("exit_code_scope=%q 无效", ev.ExitCodeScope)
	}
	checks := []struct {
		name  string
		value string
		max   int
	}{
		{"ref", ev.Ref, EvidenceIdentityMaxRunes},
		{"kind", ev.Kind, MaxIDLength},
		{"summary", ev.Summary, EvidenceSummaryMaxRunes},
		{"call_id", ev.CallID, EvidenceIdentityMaxRunes},
		{"tool_name", ev.ToolName, EvidenceIdentityMaxRunes},
		{"command", ev.Command, EvidenceCommandMaxRunes},
		{"path", ev.Path, EvidencePathMaxRunes},
	}
	for _, check := range checks {
		if utf8.RuneCountInString(check.value) > check.max {
			return fmt.Errorf("%s 超过 %d rune 上限", check.name, check.max)
		}
	}
	return nil
}

// validateActivationResultRecord 是 live 写入、journal 重放与 snapshot 恢复
// 共用的单记录校验。Result 是路由/数据流权威，必须为 JSON object；仅
// json.Valid 不足以阻止数组、标量或重复 object key 混入。
func validateActivationResultRecord(graphID string, doc *GraphDocument, rec ActivationResult, requireNode bool) error {
	if strings.TrimSpace(rec.NodeID) == "" || strings.TrimSpace(rec.ActivationID) == "" {
		return fmt.Errorf("graph: activation result 的 node_id/activation_id 均不能为空")
	}
	if owner, _, ok := parseActivationID(rec.ActivationID); !ok || owner != rec.NodeID {
		return fmt.Errorf("graph: activation result 的 activation_id %q 不属于节点 %q", rec.ActivationID, rec.NodeID)
	}
	if requireNode {
		if doc == nil {
			return fmt.Errorf("graph: activation result %s 校验时缺少 GraphDocument", rec.Ref)
		}
		if _, ok := doc.Nodes[rec.NodeID]; !ok {
			return fmt.Errorf("%w: 图 %s 节点 %s", ErrNodeNotFound, graphID, rec.NodeID)
		}
	}
	wantRef := activationResultRef(graphID, rec.ActivationID)
	if rec.Ref != wantRef {
		return fmt.Errorf("graph: activation result ref %q 与稳定引用 %q 不一致", rec.Ref, wantRef)
	}
	if err := validateJSONObject(rec.Result, MaxDocumentBytes, "activation result "+rec.Ref+" 的 result"); err != nil {
		return err
	}
	seenEvidence := make(map[string]EvidenceEntry, len(rec.Evidence))
	for i, ev := range rec.Evidence {
		if strings.TrimSpace(ev.Ref) == "" || strings.TrimSpace(ev.Kind) == "" {
			return fmt.Errorf("graph: activation result %s 的 evidence[%d] ref/kind 不能为空", rec.Ref, i)
		}
		if err := validateEvidenceEntryBounds(ev); err != nil {
			return fmt.Errorf("graph: activation result %s 的 evidence[%d] 非法: %w", rec.Ref, i, err)
		}
		if previous, exists := seenEvidence[ev.Ref]; exists && !sameEvidenceEntry(previous, ev) {
			return fmt.Errorf("graph: activation result %s 内 EvidenceRef %q 对应不一致的结构化证据", rec.Ref, ev.Ref)
		}
		seenEvidence[ev.Ref] = ev
	}
	return nil
}

func validateJSONObject(raw json.RawMessage, maxBytes int, label string) error {
	if len(raw) == 0 || !json.Valid(raw) {
		return fmt.Errorf("graph: %s 不是合法 JSON object", label)
	}
	if len(raw) > maxBytes {
		return fmt.Errorf("graph: %s 为 %d 字节，超过上限 %d", label, len(raw), maxBytes)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return fmt.Errorf("graph: %s 必须是 JSON object", label)
	}
	if path, key, duplicate := findDuplicateKey(raw); duplicate {
		return fmt.Errorf("graph: %s 含重复键 %q（path=%s）", label, key, path)
	}
	return nil
}

// validateActivationResultAgainstLedger 保证 ResultRef 与 EvidenceRef 都是
// 不可变身份。同一 ResultRef 的完全相同重放可幂等；内容冲突 fail-closed。
func validateActivationResultAgainstLedger(rec ActivationResult, ledger map[string]ActivationResult) error {
	evidenceByRef := make(map[string]EvidenceEntry)
	for ref, existing := range ledger {
		if ref == rec.Ref {
			if sameActivationResult(existing, rec) {
				return nil
			}
			return fmt.Errorf("graph: activation result %s 已存在且内容不一致，拒绝改写", rec.Ref)
		}
		for _, evidence := range existing.Evidence {
			if previous, ok := evidenceByRef[evidence.Ref]; ok && !sameEvidenceEntry(previous, evidence) {
				return fmt.Errorf("graph: ledger 中 EvidenceRef %q 已对应不一致的结构化证据", evidence.Ref)
			}
			evidenceByRef[evidence.Ref] = evidence
		}
	}
	for _, evidence := range rec.Evidence {
		if previous, ok := evidenceByRef[evidence.Ref]; ok && !sameEvidenceEntry(previous, evidence) {
			return fmt.Errorf("graph: EvidenceRef %q 已由其它 activation 绑定不同结构化证据", evidence.Ref)
		}
	}
	return nil
}

// validateTransitionRecord 校验 durable 边身份及其可选数据流绑定。allowLegacy
// 只允许旧记录省略 TargetActivationID/Input；一旦携带 ResultRef，始终要求它
// 等于来源 activation 的稳定引用且能解到同源不可变 Result。
func validateTransitionRecord(graphID string, doc *GraphDocument, rec TransitionRecord, results map[string]ActivationResult, allowLegacy bool) error {
	if strings.TrimSpace(rec.SourceNodeID) == "" || strings.TrimSpace(rec.SourceActivationID) == "" ||
		strings.TrimSpace(rec.TargetNodeID) == "" {
		return fmt.Errorf("graph: 边选择记录的 source_node_id/source_activation_id/target_node_id 均不能为空")
	}
	if rec.TransitionID < 0 {
		return fmt.Errorf("graph: 边选择记录的 transition_id 不能为负: %d", rec.TransitionID)
	}
	if owner, _, ok := parseActivationID(rec.SourceActivationID); !ok || owner != rec.SourceNodeID {
		return fmt.Errorf("graph: 边选择记录的 source_activation_id %q 不属于节点 %q", rec.SourceActivationID, rec.SourceNodeID)
	}
	if rec.TargetActivationID == "" {
		if !allowLegacy {
			return fmt.Errorf("graph: 边选择记录缺少 target_activation_id")
		}
	} else if owner, _, ok := parseActivationID(rec.TargetActivationID); !ok || owner != rec.TargetNodeID {
		return fmt.Errorf("graph: 边选择记录的 target_activation_id %q 不属于节点 %q", rec.TargetActivationID, rec.TargetNodeID)
	}
	if doc != nil {
		if _, ok := doc.Nodes[rec.SourceNodeID]; !ok {
			return fmt.Errorf("%w: 图 %s 来源节点 %s", ErrNodeNotFound, graphID, rec.SourceNodeID)
		}
		if _, ok := doc.Nodes[rec.TargetNodeID]; !ok {
			return fmt.Errorf("%w: 图 %s 目标节点 %s", ErrNodeNotFound, graphID, rec.TargetNodeID)
		}
	}
	if rec.TargetInput != "" && (len(rec.TargetInput) > MaxIDLength || !idCharset.MatchString(rec.TargetInput)) {
		return fmt.Errorf("graph: 边选择记录的 target_input %q 非法", rec.TargetInput)
	}
	if rec.ReplayInputs {
		if rec.TargetInput != "" {
			return fmt.Errorf("graph: replay_inputs 边不得同时声明 target_input")
		}
		if doc == nil {
			return fmt.Errorf("graph: replay_inputs 边缺少 GraphDocument authority")
		}
		source := doc.Nodes[rec.SourceNodeID]
		if source.Execution != nil && source.Execution.ActivationID == rec.SourceActivationID {
			source = nodeForExecution(source, *source.Execution)
		}
		if source.Kind != KindController {
			return fmt.Errorf("graph: replay_inputs 来源节点 %s 不是 controller", rec.SourceNodeID)
		}
		for i, binding := range rec.ReplayedInputs {
			owner, _, ok := parseActivationID(binding.SourceActivationID)
			if !ok || owner != binding.SourceNodeID {
				return fmt.Errorf("graph: replayed_inputs[%d] source activation %q 无效", i, binding.SourceActivationID)
			}
			if binding.ResultRef != "" && results != nil {
				stored, exists := results[binding.ResultRef]
				if !exists || stored.NodeID != binding.SourceNodeID || stored.ActivationID != binding.SourceActivationID {
					return fmt.Errorf("graph: replayed_inputs[%d] result_ref=%q 不可解引用或来源不一致", i, binding.ResultRef)
				}
			}
		}
	} else if len(rec.ReplayedInputs) > 0 {
		return fmt.Errorf("graph: 非 replay_inputs 边不得携带 replayed_inputs")
	}
	if rec.Input == nil {
		if allowLegacy {
			return nil
		}
		return fmt.Errorf("graph: 边选择记录缺少 durable Input/ResultRef")
	}
	input := rec.Input
	maxSummaryRunes := InputSummaryMaxRunes + utf8.RuneCountInString("…（已截断）")
	if utf8.RuneCountInString(input.Summary) > maxSummaryRunes {
		return fmt.Errorf("graph: 边选择记录的 input.summary 超过 %d rune 上限", maxSummaryRunes)
	}
	if input.Truncated && len(input.Result) > 0 {
		return fmt.Errorf("graph: 边选择记录的 input 同时声明 truncated 与内联 result")
	}
	if len(input.Result) > 0 {
		if err := validateJSONObject(input.Result, InputInlineMaxBytes, "边选择记录的 input.result"); err != nil {
			return err
		}
	}
	evidenceByRef := make(map[string]EvidenceEntry, len(input.Evidence))
	for i, evidence := range input.Evidence {
		if strings.TrimSpace(evidence.Ref) == "" || strings.TrimSpace(evidence.Kind) == "" {
			return fmt.Errorf("graph: 边选择记录的 input.evidence[%d] ref/kind 不能为空", i)
		}
		if err := validateEvidenceEntryBounds(evidence); err != nil {
			return fmt.Errorf("graph: 边选择记录的 input.evidence[%d] 非法: %w", i, err)
		}
		if previous, ok := evidenceByRef[evidence.Ref]; ok && !sameEvidenceEntry(previous, evidence) {
			return fmt.Errorf("graph: 边选择记录内 EvidenceRef %q 对应不一致的结构化证据", evidence.Ref)
		}
		evidenceByRef[evidence.Ref] = evidence
	}
	seenRefs := make(map[string]struct{}, len(input.EvidenceRefs))
	for _, ref := range input.EvidenceRefs {
		if strings.TrimSpace(ref) == "" || utf8.RuneCountInString(ref) > EvidenceIdentityMaxRunes {
			return fmt.Errorf("graph: 边选择记录含非法 EvidenceRef %q", ref)
		}
		if _, duplicate := seenRefs[ref]; duplicate {
			return fmt.Errorf("graph: 边选择记录重复引用 EvidenceRef %q", ref)
		}
		seenRefs[ref] = struct{}{}
		if _, ok := evidenceByRef[ref]; !ok && !allowLegacy {
			return fmt.Errorf("graph: 边选择记录的 EvidenceRef %q 缺少对应结构化证据", ref)
		}
	}
	if input.ResultRef == "" {
		if allowLegacy {
			return nil
		}
		return fmt.Errorf("graph: 边选择记录缺少 input.result_ref")
	}
	wantRef := activationResultRef(graphID, rec.SourceActivationID)
	if input.ResultRef != wantRef {
		return fmt.Errorf("graph: 边选择记录的 input.result_ref %q 与来源稳定引用 %q 不一致", input.ResultRef, wantRef)
	}
	if results == nil {
		return nil // live 路径稍后在 entry 锁内对实际 Result ledger 复核。
	}
	stored, ok := results[input.ResultRef]
	if !ok {
		return fmt.Errorf("graph: 边选择记录的 input.result_ref %q 不可解引用", input.ResultRef)
	}
	if stored.NodeID != rec.SourceNodeID || stored.ActivationID != rec.SourceActivationID {
		return fmt.Errorf("graph: 边选择记录的 input.result_ref %q 来源不一致", input.ResultRef)
	}
	if len(input.Result) > 0 && !bytes.Equal(input.Result, stored.Result) {
		return fmt.Errorf("graph: 边选择记录的内联 result 与 %q 的不可变 Result 不一致", input.ResultRef)
	}
	storedEvidence := make(map[string]EvidenceEntry, len(stored.Evidence))
	for _, evidence := range stored.Evidence {
		storedEvidence[evidence.Ref] = evidence
	}
	for ref, evidence := range evidenceByRef {
		if authoritative, ok := storedEvidence[ref]; !ok || !sameEvidenceEntry(authoritative, evidence) {
			return fmt.Errorf("graph: 边选择记录的 EvidenceRef %q 与来源 activation Result 谱系不一致", ref)
		}
	}
	return nil
}

func sameTransitionRecord(a, b TransitionRecord) bool {
	return reflect.DeepEqual(a, b)
}

// ResolveActivationResult 解引用一份 activation 级完整 Result。返回深拷贝，
// 调用方修改不会污染 Store；ref 不属于该 graph 或尚未落盘时 ok=false。
func (s *Store) ResolveActivationResult(graphID, ref string) (ActivationResult, bool) {
	e, ok := s.lookup(graphID)
	if !ok {
		return ActivationResult{}, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	rec, ok := e.activationResults[ref]
	if !ok {
		return ActivationResult{}, false
	}
	return cloneActivationResult(rec), true
}

// NextActivationID 返回节点下一个 activation_id（<nodeID>@<n>，n 为该节点
// 在图内的单调序号，V6 §6-16）。序号表由 execution/execution_status 事实
// （含 submit 文档携带的 execution）重建并随持久化一起走：snapshot 存全量、
// journal 重放取最大，进程重启后绝不重号。
func (s *Store) NextActivationID(graphID, nodeID string) (string, error) {
	e, ok := s.lookup(graphID)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrGraphNotFound, graphID)
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return fmt.Sprintf("%s@%d", nodeID, e.activationSeq[nodeID]+1), nil
}

// parseActivationID 解析 Graph Runtime 的 activation_id 形态 <nodeID>@<n>。
// 节点 ID 字符集不含 '@'（validate.go idCharset），末个 '@' 即分隔点；
// 不符合形态（无序号段、序号非正整数）时 ok=false。
func parseActivationID(id string) (nodeID string, n int, ok bool) {
	i := strings.LastIndexByte(id, '@')
	if i <= 0 || i == len(id)-1 {
		return "", 0, false
	}
	v, err := strconv.Atoi(id[i+1:])
	if err != nil || v < 1 {
		return "", 0, false
	}
	return id[:i], v, true
}

// noteActivation 把 <nodeID>@<n> 形式的 activation_id 登记进序号表（取最大）。
// activation_id 的属主节点与写入节点不一致时忽略（防御性）。
func noteActivation(seq map[string]int, nodeID, activationID string) {
	owner, n, ok := parseActivationID(activationID)
	if !ok || owner != nodeID {
		return
	}
	if n > seq[nodeID] {
		seq[nodeID] = n
	}
}

// activationSeqFromDoc 扫描文档各节点 execution.activation_id 重建序号表
// （submit 文档理论上不带 execution，此处防御性扫描）。
func activationSeqFromDoc(doc *GraphDocument) map[string]int {
	seq := make(map[string]int)
	for id, n := range doc.Nodes {
		if n.Execution != nil {
			noteActivation(seq, id, n.Execution.ActivationID)
		}
	}
	return seq
}

// ============================================================
// 变更统一纪律与辅助
// ============================================================

// mutate 定位 entry、串行化执行变更，并在首次进入 degraded 时触发
// OnDegraded 回调（entry 锁外）。
func (s *Store) mutate(graphID, kind string, payload any, revalidate bool, applyFn func(c *GraphDocument) error) error {
	e, err := s.lookupForMutation(graphID)
	if err != nil {
		return err
	}
	wasDegraded := e.isDegraded()
	e.mu.Lock()
	err = e.mutateLocked(kind, payload, revalidate, applyFn)
	e.mu.Unlock()
	if !wasDegraded && e.isDegraded() && s.OnDegraded != nil {
		s.OnDegraded(graphID, e.degradedCause())
	}
	return err
}

// mutateLocked 统一变更纪律（V6 §6-11/12）：锁内克隆候选 → apply → 可选
// 语义重校验 → journal append + fsync 成功 → 替换内存指针生效。
// 任一步失败内存不前进；落盘失败标记 degraded（后续变更 fail-closed）。
// entry 级簿记（边选择 / activation 序号）随 durable 成功在同一把锁内生效。
func (e *entry) mutateLocked(kind string, payload any, revalidate bool, applyFn func(c *GraphDocument) error) error {
	if e.degraded != nil {
		return &DegradedError{GraphID: e.doc.GraphID, Err: e.degraded}
	}
	if kind == journalKindActivationResult {
		rec := ActivationResult(payload.(activationResultPayload))
		if err := validateActivationResultAgainstLedger(rec, e.activationResults); err != nil {
			return err
		}
		if previous, exists := e.activationResults[rec.Ref]; exists && sameActivationResult(previous, rec) {
			return nil
		}
	}
	cand, err := cloneDoc(e.doc)
	if err != nil {
		return err
	}
	if err := applyFn(cand); err != nil {
		return err
	}
	// Execution 会把独立 ledger 的引用冻结进 GraphDocument；若只在重启时
	// 校验，live 进程可先观察/消费 dangling Input 或 Settlement。新写一律
	// strict，legacy inline 仅由 recovery 的显式兼容路径放行。
	switch kind {
	case journalKindExecution:
		p := payload.(executionPayload)
		if err := validateExecutionLedgerBindings(e.doc.GraphID, p.NodeID, p.Execution,
			e.transitions, e.activationResults, false); err != nil {
			return fmt.Errorf("graph: execution ledger 绑定非法: %w", err)
		}
	case journalKindExecutionStatus:
		p := payload.(executionStatusPayload)
		if err := validateExecutionLedgerBindings(e.doc.GraphID, p.NodeID, p.Execution,
			e.transitions, e.activationResults, false); err != nil {
			return fmt.Errorf("graph: execution_status ledger 绑定非法: %w", err)
		}
	}
	// 边选择幂等身份的前置校验（entry 级簿记，必须在 journal append 前拦截）
	if kind == journalKindTransition {
		rec := TransitionRecord(payload.(transitionPayload))
		if err := validateTransitionRecord(e.doc.GraphID, cand, rec, e.activationResults, false); err != nil {
			return err
		}
		key := transitionKey{rec.SourceActivationID, rec.TransitionID}
		if _, dup := e.transitions[key]; dup {
			return &TransitionExistsError{GraphID: e.doc.GraphID, Record: rec}
		}
	}
	if revalidate {
		if err := validateSemantics(cand); err != nil {
			return err
		}
		if err := validateRuntimeState(cand); err != nil {
			return err
		}
	}
	line, digest, chainDigest, err := buildJournalLine(e.seq+1, kind, cand, payload, e.chainDigest)
	if err != nil {
		return err
	}
	if err := e.journal.append(line); err != nil {
		e.markDegradedLocked(err)
		return &DegradedError{GraphID: e.doc.GraphID, Err: err}
	}
	e.doc = cand
	e.seq++
	e.digest = digest
	e.chainDigest = chainDigest
	e.journalEntries++
	e.journalBytes += int64(len(line)) + 1
	e.noteBookkeepingLocked(kind, payload)
	if e.journalEntries > journalCompactMaxEntries || e.journalBytes > journalCompactMaxBytes {
		if err := e.compactLocked(); err != nil {
			// 本次变更已 durable 且生效；压缩失败只降级后续变更。
			e.markDegradedLocked(err)
		}
	}
	return nil
}

// noteBookkeepingLocked 在变更 durable 生效后同步 entry 级簿记：
// transition 记录进边选择集合；execution/execution_status 记录推进
// activation 序号表（取 <nodeID>@<n> 的最大 n）。
func (e *entry) noteBookkeepingLocked(kind string, payload any) {
	switch kind {
	case journalKindTransition:
		rec := TransitionRecord(payload.(transitionPayload))
		e.transitions[transitionKey{rec.SourceActivationID, rec.TransitionID}] = cloneTransitionRecord(rec)
		noteActivation(e.activationSeq, rec.TargetNodeID, rec.TargetActivationID)
	case journalKindExecution:
		p := payload.(executionPayload)
		noteActivation(e.activationSeq, p.NodeID, p.Execution.ActivationID)
	case journalKindExecutionStatus:
		p := payload.(executionStatusPayload)
		noteActivation(e.activationSeq, p.NodeID, p.Execution.ActivationID)
	case journalKindActivationResult:
		rec := ActivationResult(payload.(activationResultPayload))
		e.activationResults[rec.Ref] = cloneActivationResult(rec)
		noteActivation(e.activationSeq, rec.NodeID, rec.ActivationID)
	}
}

// markDegradedLocked 记录首个落盘失败（degraded 为粘滞状态，首因保留）。
func (e *entry) markDegradedLocked(err error) {
	if e.degraded == nil {
		e.degraded = err
	}
}

func (e *entry) isDegraded() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.degraded != nil
}

func (e *entry) degradedCause() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.degraded
}

// checkStateVersion 校验运行面 CAS 前提。
func checkStateVersion(c *GraphDocument, nodeID string, base int64) error {
	if c.StateVersion != base {
		return &StateVersionConflictError{GraphID: c.GraphID, NodeID: nodeID, Base: base, Current: c.StateVersion}
	}
	return nil
}

// applyPatch 按固定顺序应用定义面操作：删除节点 → upsert 节点 → 改 root。
// 语义正确性（root 指向、引用、可达性、转移形态）由调用方随后的
// validateSemantics 统一保证。upsert 已存在节点时整体替换定义字段、
// 原样保留运行字段；新节点的运行字段从零开始（status=inactive）。
func applyPatch(doc *GraphDocument, patch *DefinitionPatch) error {
	for _, id := range patch.RemoveNodes {
		if id == "" {
			return fmt.Errorf("graph: patch 含空节点 ID 的删除操作")
		}
		if _, ok := doc.Nodes[id]; !ok {
			return fmt.Errorf("graph: patch 删除不存在的节点 %q", id)
		}
		delete(doc.Nodes, id)
	}
	for _, up := range patch.UpsertNodes {
		if up.ID == "" {
			return fmt.Errorf("graph: patch 含空 ID 的 upsert 操作")
		}
		n, ok := doc.Nodes[up.ID]
		if !ok {
			n = Node{Status: NodeInactive}
		}
		n.Kind = up.Kind
		n.Task = up.Task
		n.Capability = up.Capability
		n.Next = up.Next
		n.Wait = up.Wait
		n.Tool = up.Tool
		n.Subgraph = up.Subgraph
		n.EndOutcome = up.EndOutcome
		n.OutputContract = up.OutputContract
		n.ProgressContractRef = up.ProgressContractRef
		n.ContextPolicyRef = up.ContextPolicyRef
		n.Metadata = up.Metadata
		n.Extensions = up.Extensions
		doc.Nodes[up.ID] = n
	}
	if patch.Root != nil {
		doc.Root = *patch.Root
	}
	return nil
}

// cloneDoc 经 JSON 往返深拷贝文档（Extensions 的 RawMessage 一并复制）。
func cloneDoc(doc *GraphDocument) (*GraphDocument, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("graph: 克隆文档: %w", err)
	}
	var out GraphDocument
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("graph: 克隆文档: %w", err)
	}
	return &out, nil
}

// Close 关闭全部 journal writer 并拒绝后续变更；内存读取仍可用。幂等。
// Windows 硬约束：测试必须 t.Cleanup 先 Close 再让 TempDir 清理。
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	entries := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	s.mu.Unlock()
	var errs []error
	for _, e := range entries {
		e.mu.Lock()
		if err := e.journal.close(); err != nil {
			errs = append(errs, fmt.Errorf("graph: 关闭图 %s 的 journal: %w", e.doc.GraphID, err))
		}
		e.mu.Unlock()
	}
	return errors.Join(errs...)
}
