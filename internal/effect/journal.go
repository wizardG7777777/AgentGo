package effect

// journal.go 是 Effect Journal 的 append-only 持久化账本。
//
// 设计原则（参照 internal/store/persistence.go 的 ArtifactLog）：
//   - JSONL 追加日志：Prepare/Settle/MarkUnknown 各追加一行到
//     .agentgo/state/effects.jsonl；启动时从头到尾重放，重建
//     id → Effect 索引与 per-task 最大 seq。
//   - 零外部依赖：只用标准库。
//   - 单进程单写者：sync.Mutex 串行化并发 goroutine 的追加（Windows 上
//     单行 write 不保证原子，必须上锁）。
//   - 崩溃安全：每 append write-through 到 OS 后恰好一次 fsync——不做
//     group-commit（与 artifacts 的刻意差异：副作用频率低，且
//     prepared→settled 间隙正是崩溃窗口，必须逐条耐久；同时遵守
//     「同一路径不加第二次 fsync」的项目纪律）。
//   - 损坏行容错：Replay 遇解析失败的行告警跳过，不让一行坏数据拖垮整个账本。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentgo/internal/trace"
)

// journalFileName 是 Effect Journal 在 state 目录下的固定文件名。
const journalFileName = "effects.jsonl"

// 账本行操作类型。
const (
	opPrepare = "prepare"
	opSettle  = "settle"
	opUnknown = "unknown"
)

// ErrJournalClosed 是 journal 关闭后所有写方法的返回错误。
var ErrJournalClosed = fmt.Errorf("effect journal 已关闭")

// record 是 effects.jsonl 的单行格式。
// op=prepare 载完整 Effect；op=settle/unknown 只载 ID + 结果/原因，
// Replay 时回写索引中的既有条目（prepare 行缺失的孤儿 settle/unknown
// 告警跳过——账本的权威条目以 prepare 行为准）。
type record struct {
	Op            string    `json:"op"`
	Effect        *Effect   `json:"effect,omitempty"`
	ID            string    `json:"id,omitempty"`
	ResultSummary string    `json:"result_summary,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	At            time.Time `json:"at,omitempty"`
}

// Journal 是 Effect Journal 的内存索引 + 追加写句柄。
type Journal struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	closed bool
	index  map[string]*Effect // id → 最新状态
	order  []string           // id 的 prepare 顺序（Query 稳定序）
	maxSeq map[string]int     // taskID → 已分配最大 seq（重启续号）
}

// OpenJournal 打开（或创建）指定目录下的 effects.jsonl 并立即重放重建索引。
// dir 不存在时自动创建。返回的 journal 可立即 Prepare/Settle/MarkUnknown；
// 句柄由调用方 Close（Windows 纪律：先 Close 再让目录清理）。
func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 effect journal 目录失败 %s: %w", dir, err)
	}
	path := filepath.Join(dir, journalFileName)
	// O_APPEND 双保险：即使绕过 Mutex 的并发写也不会互相覆盖。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开 effect journal 失败 %s: %w", path, err)
	}
	j := &Journal{
		file:   f,
		path:   path,
		index:  make(map[string]*Effect),
		maxSeq: make(map[string]int),
	}
	if err := j.replay(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return j, nil
}

// Path 返回账本文件的物理路径，供调试与日志打印。
func (j *Journal) Path() string { return j.path }

// Prepare 在副作用执行前把意图落账（先落账再执行）。调用方填
// TaskID/Kind/Target/ArgsDigest/Policy（AgentID 可选）；ID/Status/PreparedAt
// 由 journal 分配：ID = <taskID>-<seq>，seq per-task 单调，进程重启后按
// 账本已落最大值续号，绝不重号。
// 成功时发出 effect_prepared trace 事件（Emit 自身降级，不阻断）。
func (j *Journal) Prepare(e *Effect) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrJournalClosed
	}
	if e.TaskID == "" {
		return fmt.Errorf("effect 缺少 TaskID，无法分配幂等身份")
	}
	seq := j.maxSeq[e.TaskID] + 1
	e.ID = fmt.Sprintf("%s-%d", e.TaskID, seq)
	e.Status = StatusPrepared
	e.PreparedAt = time.Now().UTC()
	if err := j.appendLine(&record{Op: opPrepare, Effect: e}); err != nil {
		return err
	}
	j.maxSeq[e.TaskID] = seq
	cp := *e
	j.index[e.ID] = &cp
	j.order = append(j.order, e.ID)
	emitEffectEvent(trace.KindEffectPrepared, &cp, "", "")
	return nil
}

// Settle 记录副作用执行结果（status → settled）。id 必须是 Prepare 分配
// 的 ID；对已 settled 的条目重复 Settle 返回错误（唯一结果记录者）。
// unknown → settled 合法（恢复裁决「已核验」路径）。
func (j *Journal) Settle(id, resultSummary string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrJournalClosed
	}
	e := j.index[id]
	if e == nil {
		return fmt.Errorf("未找到 effect %s（prepare 行缺失）", id)
	}
	if e.Status == StatusSettled {
		return fmt.Errorf("effect %s 已 settled，拒绝重复记录结果", id)
	}
	now := time.Now().UTC()
	if err := j.appendLine(&record{Op: opSettle, ID: id, ResultSummary: resultSummary, At: now}); err != nil {
		return err
	}
	e.Status = StatusSettled
	e.ResultSummary = resultSummary
	e.SettledAt = now
	emitEffectEvent(trace.KindEffectSettled, e, "", "")
	return nil
}

// MarkUnknown 把 Effect 标为 unknown（结果不可知）。reason 必传——unknown
// 账目必须说明「为什么不可知」，供恢复裁决与人工排查。
func (j *Journal) MarkUnknown(id, reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return ErrJournalClosed
	}
	e := j.index[id]
	if e == nil {
		return fmt.Errorf("未找到 effect %s（prepare 行缺失）", id)
	}
	if e.Status == StatusSettled {
		return fmt.Errorf("effect %s 已 settled，拒绝改标 unknown", id)
	}
	if e.Status == StatusUnknown {
		return nil // 幂等：重复标记不产生重复账本行
	}
	if err := j.appendLine(&record{Op: opUnknown, ID: id, Reason: reason, At: time.Now().UTC()}); err != nil {
		return err
	}
	e.Status = StatusUnknown
	e.UnknownReason = reason
	emitEffectEvent(trace.KindEffectUnknown, e, "", reason)
	return nil
}

// Query 返回某任务的全部 Effect（按 prepare 顺序，值拷贝）。
func (j *Journal) Query(taskID string) []Effect {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Effect
	for _, id := range j.order {
		e := j.index[id]
		if e.TaskID == taskID {
			out = append(out, *e)
		}
	}
	return out
}

// QueryByStatus 返回全账本中处于指定状态的 Effect（按 prepare 顺序）。
func (j *Journal) QueryByStatus(status Status) []Effect {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Effect
	for _, id := range j.order {
		e := j.index[id]
		if e.Status == status {
			out = append(out, *e)
		}
	}
	return out
}

// Close 关闭写句柄（幂等）。所有 append 都是 write-through + fsync，
// 关闭时无残余缓冲需要兜底同步。
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return j.file.Close()
}

// appendLine 写一行并立刻 fsync。调用方必须持有 mu。
func (j *Journal) appendLine(rec *record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 effect record 失败: %w", err)
	}
	if _, err := j.file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("写入 effect journal 失败: %w", err)
	}
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("fsync effect journal 失败: %w", err)
	}
	return nil
}

// replay 从头到尾读取账本，重建 id 索引与 per-task 最大 seq。
// 损坏行告警跳过（与 ArtifactLog 同一容错姿态）；读取走独立的只读句柄
//（写句柄是 O_APPEND|O_WRONLY），读完立即关闭。
func (j *Journal) replay() error {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("打开 effect journal 读取失败: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 单行最大 1 MB 余量
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			log.Printf("[EffectJournal] WARN 第 %d 行 JSON 解析失败，跳过: %v", lineNum, err)
			continue
		}
		j.applyRecord(&rec)
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("扫描 effect journal 失败: %w", err)
	}
	return nil
}

// applyRecord 应用一条账本行到内存索引（replay 专用；运行时路径在持锁
// 方法内直接更新）。调用方不持锁——replay 发生在 OpenJournal 内、对外发布前。
func (j *Journal) applyRecord(rec *record) {
	switch rec.Op {
	case opPrepare:
		if rec.Effect == nil || rec.Effect.ID == "" || rec.Effect.TaskID == "" {
			log.Printf("[EffectJournal] WARN prepare 行缺少 id/task_id，跳过")
			return
		}
		e := *rec.Effect
		if _, exists := j.index[e.ID]; !exists {
			j.order = append(j.order, e.ID)
		}
		j.index[e.ID] = &e
		if seq := seqOfID(e.TaskID, e.ID); seq > j.maxSeq[e.TaskID] {
			j.maxSeq[e.TaskID] = seq
		}
	case opSettle:
		e := j.index[rec.ID]
		if e == nil {
			log.Printf("[EffectJournal] WARN 孤儿 settle 行（prepare 缺失），跳过: id=%s", rec.ID)
			return
		}
		e.Status = StatusSettled
		e.ResultSummary = rec.ResultSummary
		e.SettledAt = rec.At
	case opUnknown:
		e := j.index[rec.ID]
		if e == nil {
			log.Printf("[EffectJournal] WARN 孤儿 unknown 行（prepare 缺失），跳过: id=%s", rec.ID)
			return
		}
		e.Status = StatusUnknown
		e.UnknownReason = rec.Reason
	default:
		log.Printf("[EffectJournal] WARN 未知账本操作 %q，跳过", rec.Op)
	}
}

// seqOfID 从幂等身份 <taskID>-<seq> 解析序号；格式不符返回 0（不参与续号）。
func seqOfID(taskID, id string) int {
	prefix := taskID + "-"
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	n, err := strconv.Atoi(id[len(prefix):])
	if err != nil {
		return 0
	}
	return n
}

// emitEffectEvent 发送 effect_* trace 事件。payload 只载标识与摘要
//（effect_id / kind / policy / target / args_digest / result_summary），
// 不含完整参数/命令——与账本同一脱敏纪律。trace.Emit 内部降级
//（写失败仅 stderr WARNING），事件失败不阻断账本与副作用本身。
func emitEffectEvent(kind trace.EventKind, e *Effect, decision, reason string) {
	trace.Emit(trace.Event{
		Kind:    kind,
		TaskID:  e.TaskID,
		AgentID: e.AgentID,
		Effect: &trace.EffectPayload{
			EffectID:      e.ID,
			Kind:          string(e.Kind),
			Policy:        string(e.Policy),
			Status:        string(e.Status),
			Target:        e.Target,
			ArgsDigest:    e.ArgsDigest,
			ResultSummary: e.ResultSummary,
			Decision:      decision,
			Reason:        reason,
		},
	})
}
