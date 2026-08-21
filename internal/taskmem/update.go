// update.go 实现 Task Memory 的滚动更新纪律（V6 §3）：
//   - 更新以替换/合并/supersede 为主，绝不把每轮摘要追加到尾部；
//   - 无实质变化不调版本、不落盘（ApplyTurn 返回 false）；
//   - 只消费结构化输入（TurnFacts），不调用 LLM；
//   - 各段预算在每次更新后强制执法（不是渲染时才裁剪）。
package taskmem

import (
	"fmt"
	"strings"
	"time"
)

// 文本截断上限（runes）：失败错误串 / 用户决定 / 动作描述。
const (
	errTextMaxRunes    = 160
	decisionMaxRunes   = 200
	actionCaptionRunes = 120
)

// ToolCallFact 是一次工具调用的结构化事实（来自 ToolCallRecord 账本，
// 不是模型自述）。
type ToolCallFact struct {
	Name     string // 工具名
	Target   string // 目标摘要（路径 / 命令 / 收件人等，调用方预截断）
	Success  bool   // 工具级成功（run_shell 的命令退出码见 ExitCode）
	Err      string // 失败错误串（预截断）
	ExitCode *int   // run_shell 的命令退出码；非零视为失败尝试
}

// FileWrittenFact 是一次已确认的文件写入效果（file_written）。
type FileWrittenFact struct {
	Path string
	Hash string // 内容 hash（write_file 可精确得出；edit_file 可为空）
}

// TurnFacts 是一个 settled Turn 的结构化输入：只携带 Tool/Effect/Artifact/
// 状态事实与用户决定，不携带模型文本声称。
type TurnFacts struct {
	ToolCalls    []ToolCallFact
	FilesWritten []FileWrittenFact
	NewArtifacts []string // 本轮新登记的 artifact 路径
	UserDecision string   // 本轮用户决定文本（如 request_user_input 的回答）
}

// readClassTools 是读取类工具集合：重复读取同一目标不产生新 Action
// （「重复读取或无新增证据的轮次不扩写」）。
var readClassTools = map[string]bool{
	"read_file": true, "list_dir": true, "grep_search": true, "glob_search": true,
	"web_search": true, "web_fetch": true, "probe_directory": true, "get_task_result": true,
}

// ApplyTurn 把一个 settled Turn 的结构化事实滚动合并进 Task Memory，
// 返回是否有实质变化。有变化时 Version 递增、UpdatedAt 刷新并执法各段预算；
// 无变化时不调版本（调用方据此决定不落盘、不发 updated 事件）。
//
// Sealed 的 Task Memory 拒绝更新（终态封存，等待 CM3 晋升）。
func ApplyTurn(m *TaskMemory, facts TurnFacts) bool {
	if m == nil || m.Sealed {
		return false
	}
	changed := false
	now := time.Now()

	// 1. 文件与产物版本：同路径覆盖更新（supersede 旧版本），edit_file 的
	//    空 hash 不覆盖已知的精确 hash。
	for _, fw := range facts.FilesWritten {
		if fw.Path == "" {
			continue
		}
		changed = upsertFile(m, fw.Path, fw.Hash, now) || changed
	}
	// 新登记 artifact 是产物版本证据；路径已在 Files 中（同轮 file_written
	// 已覆盖）时不重复计入。
	for _, p := range facts.NewArtifacts {
		if p == "" || findFile(m, p) >= 0 {
			continue
		}
		changed = upsertFile(m, p, "", now) || changed
	}

	// 2. 工具调用：成功进 Actions（读取类按目标去重），失败进 Failures 尾部。
	for _, tc := range facts.ToolCalls {
		if tc.Name == "" {
			continue
		}
		if isShellFailure(tc) || !tc.Success {
			appendFailure(m, failureCaption(tc), now)
			// Gate 拒绝 / Roster 占用是结构化阻塞信号；同工具同目标后来
			// 成功时按 supersede 清除（见下方 clearBlocker）。
			if isBlockingErr(tc.Err) {
				upsertBlocker(m, blockerCaption(tc), now)
			}
			changed = true
			continue
		}
		// 成功调用重置连续失败去重游标：之后重现的同形失败算新失败尝试。
		m.lastFailureCaption = ""
		// 成功：同工具同目标的现存阻塞视为已解除（supersede）。
		if clearBlocker(m, tc.Name, tc.Target) {
			changed = true
		}
		if readClassTools[tc.Name] {
			// 读取类：同 (工具, 目标) 已在 Actions 中时不产生新条目。
			if hasAction(m, tc.Name, tc.Target) {
				continue
			}
		}
		m.Actions = append(m.Actions, ActionRecord{
			Caption:  actionCaption(tc),
			Evidence: actionEvidence(tc),
			At:       now,
		})
		changed = true
	}

	// 3. 用户决定：唯一自动 confirmed 的 Fact 生产方（证据=用户输入本身）。
	if d := strings.TrimSpace(facts.UserDecision); d != "" {
		text := "用户决定: " + truncateRunes(d, decisionMaxRunes)
		if !hasFact(m, text) {
			m.Facts = append(m.Facts, Fact{
				Text:      text,
				Confirmed: true,
				Evidence:  []EvidenceRef{{Kind: EvidenceUser, Ref: "request_user_input"}},
				UpdatedAt: now,
			})
			changed = true
		}
	}

	if !changed {
		return false
	}
	enforceBudgets(m)
	m.Version++
	m.UpdatedAt = now
	return true
}

// isShellFailure 判定 run_shell 的命令级失败（工具成功但退出码非零）。
func isShellFailure(tc ToolCallFact) bool {
	return tc.Name == "run_shell" && tc.Success && tc.ExitCode != nil && *tc.ExitCode != 0
}

// isBlockingErr 识别结构化阻塞信号：新 Gate/Suggestion 路径的
// "[拒绝] 原因码=..."、未迁移 Gate 的旧 "[hook 拒绝]"，以及 Roster
// 占用（「占用」是项目固定中文错误串）。
func isBlockingErr(err string) bool {
	return strings.Contains(err, "[拒绝]") ||
		strings.Contains(err, "[hook 拒绝]") ||
		strings.Contains(err, "占用")
}

// ApplyBlockedReason 把 submit_task_result(status=blocked) 中的权威
// blocked_reason 写入 Task Memory。它是终态迁移事实，不是模型推断；
// 调用方在 Store 落 blocked 终态之前执行，使随后的 sealed checkpoint
// 与 Session promotion 能看到真实阻塞原因。
func ApplyBlockedReason(m *TaskMemory, reason string) bool {
	if m == nil || m.Sealed {
		return false
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false
	}
	caption := "agent_reported_blocked — " + truncateRunes(reason, errTextMaxRunes)
	for _, existing := range m.Blockers {
		if existing == caption {
			return false
		}
	}
	m.Blockers = append(m.Blockers, caption)
	enforceBudgets(m)
	m.Version++
	m.UpdatedAt = time.Now()
	return true
}

// ApplyAttemptEnd 把一次 attempt 的终止原因记入 Failures 尾部（2026-08-20
// SWE-001：重试接手时让模型看到「上一次是怎么死的」，避免把重试误读为
// 全新对话）。caption 有界截断；与 ApplyTurn 同款去重游标去抖。仅实质
// 变化时递增版本。
func ApplyAttemptEnd(m *TaskMemory, cause string) bool {
	if m == nil || m.Sealed {
		return false
	}
	cause = strings.TrimSpace(cause)
	if cause == "" {
		return false
	}
	before := len(m.Failures)
	appendFailure(m, "attempt 终止: "+truncateRunes(cause, errTextMaxRunes), time.Now())
	if len(m.Failures) == before {
		return false
	}
	enforceBudgets(m)
	m.Version++
	m.UpdatedAt = time.Now()
	return true
}

// findFile 返回路径在 Files 中的下标，未找到返回 -1。
func findFile(m *TaskMemory, path string) int {
	for i := range m.Files {
		if m.Files[i].Path == path {
			return i
		}
	}
	return -1
}

// upsertFile 同路径覆盖更新文件版本（替换语义）；hash 为空时保留旧值。
func upsertFile(m *TaskMemory, path, hash string, now time.Time) bool {
	if i := findFile(m, path); i >= 0 {
		if hash != "" {
			m.Files[i].Hash = hash
		}
		m.Files[i].UpdatedAt = now
		return true
	}
	m.Files = append(m.Files, FileVersion{Path: path, Hash: hash, UpdatedAt: now})
	return true
}

// hasAction 判定读取类动作是否已存在同 (工具, 目标) 条目。
func hasAction(m *TaskMemory, name, target string) bool {
	prefix := name + " " + target
	for i := range m.Actions {
		if m.Actions[i].Caption == prefix || strings.HasPrefix(m.Actions[i].Caption, prefix+" (") {
			return true
		}
	}
	return false
}

// hasFact 按文本去重（同一用户决定不重复入账）。
func hasFact(m *TaskMemory, text string) bool {
	for i := range m.Facts {
		if m.Facts[i].Text == text {
			return true
		}
	}
	return false
}

// actionCaption 生成动作描述（有界）。
func actionCaption(tc ToolCallFact) string {
	base := tc.Name
	if tc.Target != "" {
		base += " " + tc.Target
	}
	if tc.Name == "run_shell" && tc.ExitCode != nil {
		base += fmt.Sprintf(" (exit=%d)", *tc.ExitCode)
	}
	return truncateRunes(base, actionCaptionRunes)
}

// actionEvidence 为动作生成证据引用（shell 与工具结果区分 Kind）。
func actionEvidence(tc ToolCallFact) EvidenceRef {
	kind := EvidenceToolResult
	if tc.Name == "run_shell" {
		kind = EvidenceShell
	}
	ref := tc.Name
	if tc.Target != "" {
		ref += " " + truncateRunes(tc.Target, 80)
	}
	return EvidenceRef{Kind: kind, Ref: ref}
}

// failureCaption 生成失败尝试条目（有界）。
func failureCaption(tc ToolCallFact) string {
	var sb strings.Builder
	if tc.Name == "run_shell" && tc.Success && tc.ExitCode != nil {
		fmt.Fprintf(&sb, "run_shell 命令失败 (exit=%d)", *tc.ExitCode)
	} else {
		sb.WriteString(tc.Name + " 调用失败")
	}
	if tc.Target != "" {
		sb.WriteString(": " + tc.Target)
	}
	if tc.Err != "" {
		sb.WriteString(" — " + truncateRunes(tc.Err, errTextMaxRunes))
	}
	return truncateRunes(sb.String(), actionCaptionRunes+errTextMaxRunes)
}

// appendFailure 追加失败尝试。与上一条失败完全相同时去重（同一失败的
// 连续重试不刷屏）；成功动作会重置去重游标——间隔重现的同形失败仍计入。
func appendFailure(m *TaskMemory, caption string, now time.Time) {
	if caption == m.lastFailureCaption {
		return
	}
	m.Failures = append(m.Failures, caption)
	m.lastFailureCaption = caption
}

// blockerCaption 生成阻塞条目。格式 "<tool> <target> — <err>"：前缀
// "<tool> <target>" 同时充当 supersede 键（见 clearBlocker）。
func blockerCaption(tc ToolCallFact) string {
	text := tc.Name
	if tc.Target != "" {
		text += " " + tc.Target
	}
	if tc.Err != "" {
		text += " — " + truncateRunes(tc.Err, errTextMaxRunes)
	}
	return truncateRunes(text, actionCaptionRunes+errTextMaxRunes)
}

// upsertBlocker 登记阻塞：同 (工具, 目标) 的旧条目被替换（supersede）。
func upsertBlocker(m *TaskMemory, caption string, now time.Time) {
	prefix := blockerKey(caption)
	for i := range m.Blockers {
		if blockerKey(m.Blockers[i]) == prefix {
			m.Blockers[i] = caption
			return
		}
	}
	m.Blockers = append(m.Blockers, caption)
}

// clearBlocker 在同工具同目标成功后清除对应阻塞（supersede 语义），
// 返回是否有条目被清除。
func clearBlocker(m *TaskMemory, name, target string) bool {
	key := name
	if target != "" {
		key += " " + target
	}
	kept := m.Blockers[:0]
	cleared := false
	for _, b := range m.Blockers {
		if blockerKey(b) == key {
			cleared = true
			continue
		}
		kept = append(kept, b)
	}
	m.Blockers = kept
	return cleared
}

// blockerKey 提取阻塞条目的 supersede 键（" — " 前的 "<tool> <target>"）。
func blockerKey(caption string) string {
	key, _, _ := strings.Cut(caption, " — ")
	return key
}

// enforceBudgets 执法各段硬上限。淘汰策略「confirmed+最近优先，
// inferred+最旧先汰」——不是尾部追加：
//   - Actions / Failures / Blockers / NextCandidates：保留最近 N 条（头部淘汰）；
//   - Facts：先汰最旧的 inferred，再汰最旧的 confirmed（列表序即时间序）；
//   - Files：汰 UpdatedAt 最旧的条目。
func enforceBudgets(m *TaskMemory) {
	if len(m.Actions) > MaxActions {
		m.Actions = append([]ActionRecord(nil), m.Actions[len(m.Actions)-MaxActions:]...)
	}
	if len(m.Failures) > MaxFailures {
		m.Failures = append([]string(nil), m.Failures[len(m.Failures)-MaxFailures:]...)
	}
	if len(m.Blockers) > MaxBlockers {
		m.Blockers = append([]string(nil), m.Blockers[len(m.Blockers)-MaxBlockers:]...)
	}
	if len(m.NextCandidates) > MaxNextCandidates {
		m.NextCandidates = append([]string(nil), m.NextCandidates[len(m.NextCandidates)-MaxNextCandidates:]...)
	}
	for len(m.Facts) > MaxFacts {
		idx := oldestFactIndex(m.Facts, false)
		if idx < 0 {
			idx = oldestFactIndex(m.Facts, true)
		}
		if idx < 0 {
			break
		}
		m.Facts = append(m.Facts[:idx], m.Facts[idx+1:]...)
	}
	for len(m.Files) > MaxFiles {
		oldest := 0
		for i := range m.Files {
			if m.Files[i].UpdatedAt.Before(m.Files[oldest].UpdatedAt) {
				oldest = i
			}
		}
		m.Files = append(m.Files[:oldest], m.Files[oldest+1:]...)
	}
}

// oldestFactIndex 返回最旧一条 inferred（wantConfirmed=false）或 confirmed
// （wantConfirmed=true）Fact 的下标；无匹配返回 -1。列表序即插入序，
// 线性扫描取 UpdatedAt 最小者以容忍乱序恢复。
func oldestFactIndex(facts []Fact, wantConfirmed bool) int {
	idx := -1
	for i := range facts {
		if facts[i].Confirmed != wantConfirmed {
			continue
		}
		if idx < 0 || facts[i].UpdatedAt.Before(facts[idx].UpdatedAt) {
			idx = i
		}
	}
	return idx
}

// truncateRunes 按 rune 截断并追加省略标记。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
