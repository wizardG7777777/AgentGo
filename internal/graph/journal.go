package graph

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"
)

// 每图持久化布局：<dir>/<graph_id>/snapshot.json + journal.jsonl。
const (
	snapshotFileName = "snapshot.json"
	journalFileName  = "journal.jsonl"
)

// journal 压缩阈值：距上次 snapshot 的条目数超 256 或字节超 1 MiB 时，
// 写新 snapshot 后截断 journal（V6 §6-12「变更日志达到阈值后再压缩」）。
// 包级变量而非常量，测试可调小以快速触发压缩。
var (
	journalCompactMaxEntries = int64(256)
	journalCompactMaxBytes   = int64(1 << 20)
)

// journal 条目类型。
const (
	journalKindSubmit      = "submit"
	journalKindPatch       = "patch"
	journalKindNodeStatus  = "node_status"
	journalKindExecutor    = "executor"
	journalKindExecution   = "execution"
	journalKindGraphStatus = "graph_status"
	// journalKindExecutionStatus 是 Graph Runtime 的原子「execution + 节点状态」
	// 变更（SetExecutionAndStatus）：activation 创建、任务发布成功、节点终态
	// 与挂起都必须是单条 durable 记录，拆成两条会留下崩溃窗口。
	journalKindExecutionStatus = "execution_status"
	// journalKindTransition 是边选择生效记录（RecordTransition，V6 §6-17）。
	journalKindTransition = "transition"
	// journalKindActivationResult 是 activation 级不可变完整 Result。它先于
	// 节点终态落盘，使随后任一 TransitionRecord.ResultRef 都必可解引用。
	journalKindActivationResult = "activation_result"

	// journalIntegrityVersion 是新写 journal 的链式完整性格式。旧记录没有
	// 该字段，恢复时只作为 legacy 前缀兼容；一旦进入此版本，后续记录不得
	// 再降级为无摘要格式。
	journalIntegrityVersion = 1
)

// journalEntry 是一条 append-only 变更日志。Digest 记录应用本条后的执行
// 语义摘要，恢复时逐条重算对照（含日志尾，V6 §6-13）。
type journalEntry struct {
	Seq              int64           `json:"seq"`
	Kind             string          `json:"kind"`
	Revision         int64           `json:"revision"`
	StateVersion     int64           `json:"state_version"`
	Digest           string          `json:"digest"`
	At               time.Time       `json:"at"`
	Payload          json.RawMessage `json:"payload"`
	IntegrityVersion int             `json:"integrity_version,omitempty"`
	PreviousDigest   string          `json:"previous_digest,omitempty"`
	EntryDigest      string          `json:"entry_digest,omitempty"`
}

// 各 kind 的类型化 payload。
type submitPayload struct {
	Doc *GraphDocument `json:"doc"`
}

type patchPayload struct {
	Patch DefinitionPatch `json:"patch"`
}

type nodeStatusPayload struct {
	NodeID string     `json:"node_id"`
	To     NodeStatus `json:"to"`
}

type executorPayload struct {
	NodeID   string   `json:"node_id"`
	Executor Executor `json:"executor"`
}

type executionPayload struct {
	NodeID    string    `json:"node_id"`
	Execution Execution `json:"execution"`
}

type graphStatusPayload struct {
	To GraphStatus `json:"to"`
}

// executionStatusPayload 是 execution_status 记录的 payload：execution 与
// 目标节点状态原子生效。
type executionStatusPayload struct {
	NodeID    string     `json:"node_id"`
	Execution Execution  `json:"execution"`
	To        NodeStatus `json:"to"`
}

type activationResultPayload = ActivationResult

// transitionPayload 是 transition 记录的 payload，与 TransitionRecord 同形
// （定义见 store.go）。

// buildJournalLine 构造一条 journal 记录并序列化为一行。Digest 继续表示
// Graph 定义语义摘要；EntryDigest 则覆盖本记录的身份、payload、时间与前一
// 条 EntryDigest，防止不进入 GraphDocument 的 Result/Transition 账本被合法
// JSON 位翻后静默重放。
func buildJournalLine(seq int64, kind string, doc *GraphDocument, payload any, previousDigest string) ([]byte, string, string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", fmt.Errorf("graph: 编码 journal payload: %w", err)
	}
	entry := journalEntry{
		Seq:              seq,
		Kind:             kind,
		Revision:         doc.Revision,
		StateVersion:     doc.StateVersion,
		Digest:           ComputeDigest(doc),
		At:               time.Now().UTC(),
		Payload:          raw,
		IntegrityVersion: journalIntegrityVersion,
		PreviousDigest:   previousDigest,
	}
	entry.EntryDigest = computeJournalEntryDigest(&entry)
	line, err := json.Marshal(entry)
	if err != nil {
		return nil, "", "", fmt.Errorf("graph: 编码 journal 记录: %w", err)
	}
	return line, entry.Digest, entry.EntryDigest, nil
}

// journalDigestInput 排除 EntryDigest 自身，避免循环；其余恢复语义与审计
// 字段全部纳入。Payload 保留 durable 原始字节，Result 的不可变性是字节级。
type journalDigestInput struct {
	Domain           string          `json:"domain"`
	IntegrityVersion int             `json:"integrity_version"`
	Seq              int64           `json:"seq"`
	Kind             string          `json:"kind"`
	Revision         int64           `json:"revision"`
	StateVersion     int64           `json:"state_version"`
	Digest           string          `json:"digest"`
	At               time.Time       `json:"at"`
	PreviousDigest   string          `json:"previous_digest"`
	Payload          json.RawMessage `json:"payload"`
}

func computeJournalEntryDigest(entry *journalEntry) string {
	if entry == nil {
		return ""
	}
	return hashCanonical(journalDigestInput{
		Domain:           "agentgo.graph.journal/v1",
		IntegrityVersion: entry.IntegrityVersion,
		Seq:              entry.Seq,
		Kind:             entry.Kind,
		Revision:         entry.Revision,
		StateVersion:     entry.StateVersion,
		Digest:           entry.Digest,
		At:               entry.At,
		PreviousDigest:   entry.PreviousDigest,
		Payload:          entry.Payload,
	})
}

// journalSink 抽象 journal 落盘（append+fsync / 压缩截断 / 关闭），
// 测试可注入失败实现制造 persistence-degraded。
type journalSink interface {
	append(line []byte) error
	reset() error
	close() error
}

// journalWriter 是 journalSink 的文件实现：每次 append 后 flush + 恰好一次
// fsync（项目硬约束：绝不在已经过一次 fsync 的路径里加第二次）。
type journalWriter struct {
	f    *os.File
	path string
}

// openJournal 打开（必要时创建）journal 文件。exclusive 用于 SubmitGraph：
// O_EXCL 防止覆盖磁盘上已存在的同名 journal（未 Recover 的残留）。
func openJournal(path string, exclusive bool) (*journalWriter, error) {
	flag := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if exclusive {
		flag |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return nil, err
	}
	return &journalWriter{f: f, path: path}, nil
}

// append 写一行并 fsync 一次。部分写失败会在文件尾留下坏行，恢复时
// 「坏行即停并截断」兜住（recover.go）。
func (w *journalWriter) append(line []byte) error {
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := w.f.Write(buf); err != nil {
		return fmt.Errorf("graph: 写 journal: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("graph: fsync journal: %w", err)
	}
	return nil
}

// reset 在压缩时把 journal 截断为空：关闭当前句柄后以 O_TRUNC 重建。
// 即使截断结果丢失也不影响正确性——残留条目 seq ≤ 新 snapshot.seq，
// 恢复时按「只放 seq > snapshot.seq」规则跳过。
func (w *journalWriter) reset() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("graph: 压缩前关闭 journal: %w", err)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("graph: 压缩截断 journal: %w", err)
	}
	w.f = f
	return nil
}

func (w *journalWriter) close() error {
	return w.f.Close()
}

// ============================================================
// snapshot
// ============================================================

const (
	snapshotVersionLegacy = 1
	snapshotVersion       = 2
)

// snapshotFile 是 snapshot.json 的内容：完整 GraphDocument + 持久化游标。
// Transitions 与 ActivationSeq 是 Graph Runtime 的 entry 级簿记（不属于
// GraphDocument 契约）：journal 压缩截断后，边选择记录与 per 节点 activation
// 单调序号仍可从 snapshot 重建（V6 §6-16/17 的 durable 要求）。
type snapshotFile struct {
	Version      int            `json:"version"`
	GraphID      string         `json:"graph_id"`
	Seq          int64          `json:"seq"` // 截至此快照已落盘的 journal seq
	Revision     int64          `json:"revision"`
	StateVersion int64          `json:"state_version"`
	Digest       string         `json:"digest"`
	Doc          *GraphDocument `json:"doc"`
	// ChainDigest 是 seq 对应的最后一条 journal EntryDigest；IntegrityDigest
	// 覆盖本快照游标、完整 Doc 与排序后的全部 entry 级账本。Digest 仍只保留
	// 既有的定义语义含义，避免破坏读取 API。
	ChainDigest     string `json:"chain_digest,omitempty"`
	IntegrityDigest string `json:"integrity_digest,omitempty"`

	Transitions       []TransitionRecord `json:"transitions,omitempty"`        // 已生效边选择（排序后全量）
	ActivationResults []ActivationResult `json:"activation_results,omitempty"` // activation 级完整 Result（按 ref 排序）
	ActivationSeq     map[string]int     `json:"activation_seq,omitempty"`     // node_id → 已分配的最大 activation 序号
}

// compactLocked 压缩：写新 snapshot（含当前 seq）→ 截断 journal → 计数清零。
// 调用方持 e.mu；失败返回错误，由调用方标记 degraded。
func (e *entry) compactLocked() error {
	snap := &snapshotFile{
		Version:           snapshotVersion,
		GraphID:           e.doc.GraphID,
		Seq:               e.seq,
		Revision:          e.doc.Revision,
		StateVersion:      e.doc.StateVersion,
		Digest:            e.digest,
		Doc:               e.doc,
		ChainDigest:       e.chainDigest,
		Transitions:       sortedTransitionRecords(e.transitions),
		ActivationResults: sortedActivationResults(e.activationResults),
		ActivationSeq:     maps.Clone(e.activationSeq),
	}
	snap.IntegrityDigest = computeSnapshotIntegrityDigest(snap)
	if err := writeSnapshotAtomic(filepath.Join(e.dir, snapshotFileName), snap); err != nil {
		return err
	}
	if err := e.journal.reset(); err != nil {
		return err
	}
	e.journalEntries = 0
	e.journalBytes = 0
	return nil
}

// writeSnapshotAtomic 落盘 snapshot：同目录临时文件 → fsync → 原子替换 →
// 目录 fsync。与既有原子落盘惯例同一手法（写临时文件 → fsync → rename →
// （Go 在 Windows 的 os.Rename 走 MoveFileEx + MOVEFILE_REPLACE_EXISTING，
// 可覆盖已存在目标；该手法已在 Windows 生产验证）。
func writeSnapshotAtomic(path string, snap *snapshotFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("graph: 创建 snapshot 目录: %w", err)
	}
	// 不用 MarshalIndent：ActivationResult.Result / EdgeInput.Result 是
	// json.RawMessage，缩进器会改写其权威字节，导致 snapshot 压缩前后同一
	// stable ResultRef 解出不同内容。紧凑编码同时保持跨平台确定性。
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("graph: 编码 snapshot: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".graph-snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("graph: 创建 snapshot 临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("graph: chmod snapshot 临时文件: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("graph: 写 snapshot 临时文件: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("graph: fsync snapshot 临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("graph: 关闭 snapshot 临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("graph: 替换 snapshot: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
