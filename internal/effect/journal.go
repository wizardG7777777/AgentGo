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
//   - 权威失败收口：Replay 遇到坏行、孤儿转换、重复/非法转换
//     立即拒绝打开；运行期 write/fsync 失败立即 poison，后续权威读写
//     全部失败，不得在部分日志上继续执行。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// maxJournalRecordBytes 是单行账本的绝对上限。账本只允许摘要，
// 不应承载正文；上限同时保证 replay Scanner 不会被无界行撑爆。
const maxJournalRecordBytes = 1024 * 1024

// 账本行操作类型。
const (
	opPrepare = "prepare"
	opSettle  = "settle"
	opUnknown = "unknown"
)

var (
	// ErrJournalClosed 是 journal 关闭后所有写方法的返回错误。
	ErrJournalClosed = errors.New("effect journal 已关闭")
	// ErrJournalPoisoned 表示某次 append 只完成了部分 write 或 fsync
	// 失败，账本的 durable 边界已无法证明，只能关闭并人工核验。
	ErrJournalPoisoned = errors.New("effect journal 已 poison")
	// ErrJournalRequired 是生产装配在副作用组未注入 Journal 时的
	// fail-closed 诊断。legacy/隔离单测可不启用账本，生产必须在启动期调用
	// RequireJournal 验证。
	ErrJournalRequired = errors.New("副作用执行组未装配 effect journal")
)

// PoisonError 保留首次导致账本不可信的操作和原因。
type PoisonError struct {
	Operation string
	Cause     error
}

func (e *PoisonError) Error() string {
	if e == nil {
		return ErrJournalPoisoned.Error()
	}
	return fmt.Sprintf("%s: operation=%s: %v", ErrJournalPoisoned, e.Operation, e.Cause)
}

func (e *PoisonError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *PoisonError) Is(target error) bool { return target == ErrJournalPoisoned }

// record 是 effects.jsonl 的单行格式。
// op=prepare 载完整 Effect；op=settle/unknown 只载 ID + 结果/原因，
// Replay 时回写索引中的既有条目；prepare 缺失的孤儿
// settle/unknown 是权威链断裂，必须 fail-closed。
type record struct {
	Op            string    `json:"op"`
	Effect        *Effect   `json:"effect,omitempty"`
	ID            string    `json:"id,omitempty"`
	ResultSummary string    `json:"result_summary,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	At            time.Time `json:"at,omitempty"`
}

// journalFile 是 append 边界的最小句柄接口。真实实现是 *os.File；
// 窄接口使短写和 fsync 失败可在不依赖平台故障的情况下验证。
type journalFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

// Journal 是 Effect Journal 的内存索引 + 追加写句柄。
type Journal struct {
	mu     sync.Mutex
	file   journalFile
	path   string
	closed bool
	poison error
	index  map[string]*Effect // id → 最新状态
	order  []string           // id 的 prepare 顺序（Query 稳定序）
	maxSeq map[string]int     // taskID → 已分配最大 seq（重启续号）
}

// OpenJournal 打开（或创建）指定目录下的 effects.jsonl 并立即重放重建索引。
// dir 不存在时自动创建。返回的 journal 可立即 Prepare/Settle/MarkUnknown；
// 句柄由调用方 Close（Windows 纪律：先 Close 再让目录清理）。
func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建 effect journal 目录失败 %s: %w", dir, err)
	}
	path := filepath.Join(dir, journalFileName)
	// O_APPEND 双保险：即使绕过 Mutex 的并发写也不会互相覆盖。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开 effect journal 失败 %s: %w", path, err)
	}
	// OpenFile 的 perm 只影响新建文件；旧版可能留下 0644，打开时
	// 同步收紧为 0600，避免副作用目标摘要被同机其他用户读取。
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("收紧 effect journal 权限失败 %s: %w", path, err)
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

// RequireJournal 是生产装配的显式 fail-closed 校验点。工具组为了
// legacy/隔离单测仍可以不启用账本，但生产 bootstrap 必须在允许
// 新执行前通过此函数证明 Journal 存在且未 poison。
func RequireJournal(j *Journal) error {
	if j == nil {
		return NewAuthorityError(AuthorityPhaseRead, "", false, ErrJournalRequired)
	}
	if err := j.Health(); err != nil {
		return NewAuthorityError(AuthorityPhaseRead, "", false, err)
	}
	return nil
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
	if err := j.writeHealthLocked(); err != nil {
		return err
	}
	if e == nil {
		return fmt.Errorf("effect 为 nil，无法 Prepare")
	}
	if err := validatePrepareIntent(e); err != nil {
		return err
	}
	seq := j.maxSeq[e.TaskID] + 1
	cp := *e
	cp.ID = fmt.Sprintf("%s-%d", cp.TaskID, seq)
	cp.Status = StatusPrepared
	cp.PreparedAt = time.Now().UTC()
	cp.ResultSummary = ""
	cp.UnknownReason = ""
	cp.SettledAt = time.Time{}
	if err := j.appendLine(&record{Op: opPrepare, Effect: &cp}); err != nil {
		return err
	}
	j.maxSeq[cp.TaskID] = seq
	j.index[cp.ID] = &cp
	j.order = append(j.order, cp.ID)
	*e = cp
	emitEffectEvent(trace.KindEffectPrepared, &cp, "", "")
	return nil
}

// Settle 记录副作用执行结果（status → settled）。id 必须是 Prepare 分配
// 的 ID；对已 settled 的条目重复 Settle 返回错误（唯一结果记录者）。
// unknown → settled 合法（恢复裁决「已核验」路径）。
func (j *Journal) Settle(id, resultSummary string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.writeHealthLocked(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("effect settle 缺少 id")
	}
	e := j.index[id]
	if e == nil {
		return fmt.Errorf("未找到 effect %s（prepare 行缺失）", id)
	}
	if e.Status != StatusPrepared && e.Status != StatusUnknown {
		return fmt.Errorf("effect %s 非法转换 %s -> settled", id, e.Status)
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
	if err := j.writeHealthLocked(); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("effect unknown 缺少 id")
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("effect %s unknown 缺少 reason", id)
	}
	e := j.index[id]
	if e == nil {
		return fmt.Errorf("未找到 effect %s（prepare 行缺失）", id)
	}
	if e.Status != StatusPrepared {
		return fmt.Errorf("effect %s 非法转换 %s -> unknown", id, e.Status)
	}
	if err := j.appendLine(&record{Op: opUnknown, ID: id, Reason: reason, At: time.Now().UTC()}); err != nil {
		return err
	}
	e.Status = StatusUnknown
	e.UnknownReason = reason
	emitEffectEvent(trace.KindEffectUnknown, e, "", reason)
	return nil
}

// Query 是旧调用方的非权威投影。Deprecated：新的恢复/执行控制流
// 必须调用 QueryStrict，以便 poison 不会被误投影成“零条 Effect”。
// 兼容口在 poison 时返回 nil，只允许 UI/诊断类过渡消费。
func (j *Journal) Query(taskID string) []Effect {
	out, err := j.QueryStrict(taskID)
	if err != nil {
		return nil
	}
	return out
}

// QueryStrict 返回某任务的全部 Effect（按 prepare 顺序，值拷贝）；
// Journal poison 时返回 typed 错误，调用方必须 fail-closed。
func (j *Journal) QueryStrict(taskID string) ([]Effect, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poison != nil {
		return nil, j.poison
	}
	var out []Effect
	for _, id := range j.order {
		e := j.index[id]
		if e.TaskID == taskID {
			out = append(out, *e)
		}
	}
	return out, nil
}

// QueryByStatus 是旧调用方的非权威投影。Deprecated：新控制流使用
// QueryByStatusStrict。
func (j *Journal) QueryByStatus(status Status) []Effect {
	out, err := j.QueryByStatusStrict(status)
	if err != nil {
		return nil
	}
	return out
}

// QueryByStatusStrict 返回全账本的指定状态投影，poison 时失败。
func (j *Journal) QueryByStatusStrict(status Status) ([]Effect, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poison != nil {
		return nil, j.poison
	}
	var out []Effect
	for _, id := range j.order {
		e := j.index[id]
		if e.Status == status {
			out = append(out, *e)
		}
	}
	return out, nil
}

// Health 返回 Journal 当前 durable authority 健康状态。关闭后
// 内存索引仍可诊断读取，但已不具备生产写权威，因此 Health
// 返回 ErrJournalClosed；poison 优先保留首个 durable 根因。
func (j *Journal) Health() error {
	if j == nil {
		return ErrJournalRequired
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.poison != nil {
		return j.poison
	}
	if j.closed {
		return ErrJournalClosed
	}
	return nil
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

// writeHealthLocked 在每个权威写入口先检查不可逆的运行状态。
// 调用方必须持有 mu。
func (j *Journal) writeHealthLocked() error {
	if j.closed {
		return ErrJournalClosed
	}
	if j.poison != nil {
		return j.poison
	}
	return nil
}

// poisonLocked 只保留第一个 durable 失败；后续读写稳定返同一
// 根因，避免在已不可信的账本上覆盖首个定界证据。
func (j *Journal) poisonLocked(operation string, cause error) error {
	if j.poison == nil {
		j.poison = &PoisonError{Operation: operation, Cause: cause}
	}
	return j.poison
}

// appendLine 写一行并立刻 fsync。调用方必须持有 mu。
func (j *Journal) appendLine(rec *record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 effect record 失败: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxJournalRecordBytes {
		return fmt.Errorf("effect record 超过单行上限: %d > %d", len(data), maxJournalRecordBytes)
	}
	n, writeErr := j.file.Write(data)
	if writeErr != nil || n != len(data) {
		cause := writeErr
		if cause == nil {
			cause = io.ErrShortWrite
		} else if n != len(data) {
			cause = fmt.Errorf("短写 %d/%d: %w", n, len(data), cause)
		}
		return j.poisonLocked("write", cause)
	}
	if err := j.file.Sync(); err != nil {
		return j.poisonLocked("fsync", err)
	}
	return nil
}

// replay 从头到尾严格读取账本，重建 id 索引与 per-task 最大
// seq。任何坏行、未知字段、孤儿转换、重复/非法转换都使 OpenJournal
// 失败，不允许在不完整的 durable authority 上继续。读取使用独立
// 只读句柄（写句柄是 O_APPEND|O_WRONLY），读完立即关闭。
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
	scanner.Buffer(make([]byte, 64*1024), maxJournalRecordBytes)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			return fmt.Errorf("effect journal 第 %d 行为空，拒绝重放", lineNum)
		}
		var rec record
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&rec); err != nil {
			return fmt.Errorf("effect journal 第 %d 行 JSON 无效: %w", lineNum, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return fmt.Errorf("effect journal 第 %d 行包含多个 JSON 值", lineNum)
			}
			return fmt.Errorf("effect journal 第 %d 行尾部无效: %w", lineNum, err)
		}
		if err := j.applyRecord(&rec); err != nil {
			return fmt.Errorf("effect journal 第 %d 行转换无效: %w", lineNum, err)
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("扫描 effect journal 失败: %w", err)
	}
	return nil
}

// applyRecord 验证并应用一条账本行到内存索引（replay 专用）。
// 调用方不持锁——replay 发生在 OpenJournal 对外发布前。
func (j *Journal) applyRecord(rec *record) error {
	if rec == nil {
		return fmt.Errorf("账本行为 nil")
	}
	switch rec.Op {
	case opPrepare:
		if rec.Effect == nil {
			return fmt.Errorf("prepare 缺少 effect")
		}
		if rec.ID != "" || rec.ResultSummary != "" || rec.Reason != "" || !rec.At.IsZero() {
			return fmt.Errorf("prepare 携带非法转换字段")
		}
		if err := validatePreparedEffect(rec.Effect); err != nil {
			return err
		}
		e := *rec.Effect
		if _, exists := j.index[e.ID]; exists {
			return fmt.Errorf("重复 prepare: id=%s", e.ID)
		}
		seq := seqOfID(e.TaskID, e.ID)
		if seq <= 0 {
			return fmt.Errorf("prepare id %q 不符合 <taskID>-<seq>", e.ID)
		}
		wantSeq := j.maxSeq[e.TaskID] + 1
		if seq != wantSeq {
			return fmt.Errorf("task %s prepare seq 不连续: got=%d want=%d", e.TaskID, seq, wantSeq)
		}
		j.order = append(j.order, e.ID)
		j.index[e.ID] = &e
		j.maxSeq[e.TaskID] = seq
	case opSettle:
		if rec.Effect != nil || rec.ID == "" || rec.Reason != "" || rec.At.IsZero() {
			return fmt.Errorf("settle 字段不完整或携带非法字段")
		}
		e := j.index[rec.ID]
		if e == nil {
			return fmt.Errorf("孤儿 settle: id=%s", rec.ID)
		}
		if e.Status != StatusPrepared && e.Status != StatusUnknown {
			return fmt.Errorf("effect %s 非法转换 %s -> settled", rec.ID, e.Status)
		}
		e.Status = StatusSettled
		e.ResultSummary = rec.ResultSummary
		e.SettledAt = rec.At
	case opUnknown:
		if rec.Effect != nil || rec.ID == "" || strings.TrimSpace(rec.Reason) == "" ||
			rec.ResultSummary != "" || rec.At.IsZero() {
			return fmt.Errorf("unknown 字段不完整或携带非法字段")
		}
		e := j.index[rec.ID]
		if e == nil {
			return fmt.Errorf("孤儿 unknown: id=%s", rec.ID)
		}
		if e.Status != StatusPrepared {
			return fmt.Errorf("effect %s 非法转换 %s -> unknown", rec.ID, e.Status)
		}
		e.Status = StatusUnknown
		e.UnknownReason = rec.Reason
	default:
		return fmt.Errorf("未知账本操作 %q", rec.Op)
	}
	return nil
}

// validatePrepareIntent 验证调用方在 Prepare 前只提供意图字段。
// ID/状态/时间和结果都由 Journal 权威分配，调用方不得预填。
func validatePrepareIntent(e *Effect) error {
	if e == nil {
		return fmt.Errorf("effect 为 nil")
	}
	if strings.TrimSpace(e.TaskID) == "" {
		return fmt.Errorf("effect 缺少 TaskID，无法分配幂等身份")
	}
	if strings.ContainsAny(e.TaskID, "\r\n") {
		return fmt.Errorf("effect TaskID 包含非法换行")
	}
	if !validKind(e.Kind) {
		return fmt.Errorf("effect kind 无效: %q", e.Kind)
	}
	if strings.TrimSpace(e.Target) == "" {
		return fmt.Errorf("effect 缺少 Target")
	}
	if strings.TrimSpace(e.ArgsDigest) == "" {
		return fmt.Errorf("effect 缺少 ArgsDigest")
	}
	if !validPolicy(e.Policy) {
		return fmt.Errorf("effect replay policy 无效: %q", e.Policy)
	}
	if e.ID != "" || e.Status != "" || e.ResultSummary != "" || e.UnknownReason != "" ||
		!e.PreparedAt.IsZero() || !e.SettledAt.IsZero() {
		return fmt.Errorf("effect Prepare 意图携带 Journal 权威生命周期字段")
	}
	return nil
}

func validatePreparedEffect(e *Effect) error {
	if e == nil {
		return fmt.Errorf("prepare effect 为 nil")
	}
	intent := *e
	intent.ID = ""
	intent.Status = ""
	intent.PreparedAt = time.Time{}
	if err := validatePrepareIntent(&intent); err != nil {
		return err
	}
	if e.ID == "" || e.Status != StatusPrepared || e.PreparedAt.IsZero() {
		return fmt.Errorf("prepare effect 缺少权威 id/status/prepared_at")
	}
	if e.ResultSummary != "" || e.UnknownReason != "" || !e.SettledAt.IsZero() {
		return fmt.Errorf("prepare effect 携带结算/unknown 字段")
	}
	return nil
}

func validKind(kind Kind) bool {
	switch kind {
	case KindFileWrite, KindFileEdit, KindShell, KindMessage, KindWorkspaceMerge:
		return true
	default:
		return false
	}
}

func validPolicy(policy ReplayPolicy) bool {
	switch policy {
	case PolicySafeReplay, PolicyVerifyFirst, PolicyManualOnly, PolicyNeverReplay:
		return true
	default:
		return false
	}
}

// seqOfID 从幂等身份 <taskID>-<seq> 解析序号；格式不符返回 0（不参与续号）。
func seqOfID(taskID, id string) int {
	prefix := taskID + "-"
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	suffix := id[len(prefix):]
	if suffix == "" || (len(suffix) > 1 && suffix[0] == '0') {
		return 0
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// emitEffectEvent 发送 effect_* trace 事件。payload 只载标识与摘要
// （effect_id / kind / policy / target / args_digest / result_summary），
// 不含完整参数/命令——与账本同一脱敏纪律。trace.Emit 内部降级
// （写失败仅 stderr WARNING），事件失败不阻断账本与副作用本身。
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
