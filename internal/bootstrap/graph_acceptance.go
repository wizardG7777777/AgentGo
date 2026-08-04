package bootstrap

// 本文件是 G1b Graph acceptance 服务端核验桥（graph.AcceptanceVerifier /
// graph.GraphChangeWaker 的活系统实现 + Bootstrap 装配点）：
//   - acceptanceVerifier：对验收 agent 经 Results["evidence"] 自报的 JSON
//     证据逐条服务端核验——command 对照 Effect Journal 该任务的 shell 账
//     （账载命令 digest 逐字比对 + exit code 相符）、file_hash 在 pathutil
//     边界内重算 sha256 比对、task_status 逐字命中状态词表；
//   - graphChangeWaker：核验 disputed/unverifiable 时按 C5d 既有 graph
//     change 机制发布 __scheduler__ 唤醒任务（幂等标记
//     [graph-change-request: <graphID>/<activationID>/change]，与
//     request_replan 图路径同一格式，跨路径查重共享）；
//   - wireGraphAcceptanceBridge：装配注入点。
//
// 保守纪律（V6 红线「不误判 valid」）：无证据 / 证据类型未知 / Journal 不
// 可用一律 unverifiable——引擎侧按 disputed 同等处理（节点 failed + 唤醒），
// 绝不按自报 verdict 放行。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/pathutil"
	"agentgo/internal/store"
)

// ============================================================
// acceptanceVerifier —— graph.AcceptanceVerifier 的活系统实现
// ============================================================

// acceptanceVerifier 以 Effect Journal（command 证据的权威执行账）与
// projectRoot（file_hash 证据的边界）为核验依据。
type acceptanceVerifier struct {
	journal     *effect.Journal // nil 时一律 unverifiable（不误判 valid）
	projectRoot string
}

// taskStatusWords 是 task_status 证据的合法状态词表（逐字命中才过）。
var taskStatusWords = map[string]struct{}{
	"completed": {}, "failed": {}, "blocked": {}, "cancelled": {},
	"pass": {}, "fail": {},
}

// VerifyAcceptance 实现 graph.AcceptanceVerifier：逐条核验证据，首条失败即
// 短路（disputed 的 Reason 含哪条证据为何失败）；全部通过才 valid。
// 本方法在 graph.Runtime 锁内同步调用，只做只读查询（账本 Query / 文件读 /
// hash 重算），不回调 Runtime。
func (v *acceptanceVerifier) VerifyAcceptance(taskID string, _ string, evidence []graph.EvidenceItem) (graph.VerifyOutcome, error) {
	if len(evidence) == 0 {
		return graph.VerifyOutcome{
			Status: graph.VerifyUnverifiable,
			Reason: "验收结论未携带可核验证据（Results[\"evidence\"] 缺失或为空数组）",
		}, nil
	}
	if v.journal == nil {
		return graph.VerifyOutcome{
			Status: graph.VerifyUnverifiable,
			Reason: "Effect Journal 不可用，无法核验 command 证据（保守不采信）",
		}, nil
	}
	var notes []string
	checked := 0
	for _, item := range evidence {
		var note string
		var err error
		switch item.Type {
		case graph.EvidenceTypeCommand:
			err = v.verifyCommand(taskID, item)
		case graph.EvidenceTypeFileHash:
			note, err = v.verifyFileHash(item)
		case graph.EvidenceTypeTaskStatus:
			err = verifyTaskStatus(item)
		default:
			return graph.VerifyOutcome{
				Status:  graph.VerifyUnverifiable,
				Reason:  fmt.Sprintf("判据 %q 的证据类型 %q 未知（合法类型：command / file_hash / task_status）", item.Criterion, item.Type),
				Checked: checked,
			}, nil
		}
		if err != nil {
			return graph.VerifyOutcome{Status: graph.VerifyDisputed, Reason: err.Error(), Checked: checked}, nil
		}
		checked++
		if note != "" {
			notes = append(notes, note)
		}
	}
	return graph.VerifyOutcome{
		Status:  graph.VerifyValid,
		Reason:  strings.Join(notes, "；"),
		Checked: checked,
	}, nil
}

// verifyCommand 核验 command 证据：Effect Journal 该任务的 shell 账中须存在
// 同命令（规范化=去首尾空白后逐字一致——账本按脱敏纪律只存命令 sha256
// 前 12）、从 project root 执行且 exit code 相符（expect_exit 缺省 0）。
// Target 只证明命令文本；ArgsDigest 还覆盖 run_shell 实际 working_dir，必须
// 同时比对，否则在其它目录运行的同名命令会被误当作验收证据。
// 查不到账或任一字段不符 → disputed（命令必须本次任务真实执行过）。
func (v *acceptanceVerifier) verifyCommand(taskID string, item graph.EvidenceItem) error {
	command := strings.TrimSpace(item.Value)
	if command == "" {
		return fmt.Errorf("判据 %q 的 command 证据命令串为空", item.Criterion)
	}
	expectExit := 0
	if item.ExpectExit != nil {
		expectExit = *item.ExpectExit
	}
	wantTarget := "cmd:" + acceptanceDigest12([]byte(command))
	wantArgs := acceptanceDigest12([]byte(command + "\n" + v.projectRoot))
	for _, e := range v.journal.Query(taskID) {
		if e.Kind != effect.KindShell || e.Status != effect.StatusSettled ||
			e.Target != wantTarget || e.ArgsDigest != wantArgs {
			continue
		}
		code, ok := parseExitCode(e.ResultSummary)
		if !ok {
			continue
		}
		if code == expectExit {
			return nil
		}
	}
	return fmt.Errorf("判据 %q 的 command 证据未通过：该任务的 shell 账中不存在从 project root 执行的同命令且 exit=%d 的记录（命令必须本次任务真实执行过）",
		item.Criterion, expectExit)
}

// verifyFileHash 核验 file_hash 证据：pathutil 边界内重算文件 sha256。
// value 为「路径」时只核验文件存在并可读，返回实际 hash 供记录（调用方
// 并入 valid 的 Reason）；value 为「路径=hash」时重算比对，一致才过
// （hash 取最后一个 "=" 之后——路径本身可含 "="；期望值支持完整 sha256
// hex 或项目惯例的前 12 位 digest）。
func (v *acceptanceVerifier) verifyFileHash(item graph.EvidenceItem) (string, error) {
	raw := strings.TrimSpace(item.Value)
	if raw == "" {
		return "", fmt.Errorf("判据 %q 的 file_hash 证据路径为空", item.Criterion)
	}
	path, expectHash := raw, ""
	if i := strings.LastIndex(raw, "="); i >= 0 {
		path, expectHash = strings.TrimSpace(raw[:i]), strings.ToLower(strings.TrimSpace(raw[i+1:]))
	}
	abs, err := pathutil.ValidatePath(path, v.projectRoot)
	if err != nil {
		return "", fmt.Errorf("判据 %q 的 file_hash 证据路径越界或非法: %v", item.Criterion, err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("判据 %q 的 file_hash 证据文件不可读（%s）: %v", item.Criterion, path, err)
	}
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if expectHash == "" {
		return fmt.Sprintf("%s sha256=%s", path, actual[:12]), nil
	}
	if expectHash != actual && expectHash != actual[:12] {
		return "", fmt.Errorf("判据 %q 的 file_hash 证据未通过：%s 重算 sha256=%s 与自报 %s 不一致",
			item.Criterion, path, actual[:12], expectHash)
	}
	return "", nil
}

// verifyTaskStatus 核验 task_status 证据：value（去首尾空白）逐字命中
// 状态词表才过；非法词 disputed（如 "success" / "passed" 这类近义词不放行）。
func verifyTaskStatus(item graph.EvidenceItem) error {
	word := strings.TrimSpace(item.Value)
	if _, ok := taskStatusWords[word]; !ok {
		return fmt.Errorf("判据 %q 的 task_status 证据状态词 %q 非法（合法词：completed/failed/blocked/cancelled/pass/fail）",
			item.Criterion, word)
	}
	return nil
}

// acceptanceDigest12 返回 data 的 sha256 hex 前 12（与 run_shell 落账的
// Target "cmd:<digest>" 同一口径——internal/tools.digest12 的等价物，
// 两处算法必须保持一致：sha256 全量 hex 的前 12 字符）。
func acceptanceDigest12(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

// parseExitCode 从 shell 账的 ResultSummary（"exit_code=N outcome=..."）
// 解析退出码。
func parseExitCode(resultSummary string) (int, bool) {
	var code int
	if _, err := fmt.Sscanf(resultSummary, "exit_code=%d", &code); err != nil {
		return 0, false
	}
	return code, true
}

// ============================================================
// graphChangeWaker —— graph.GraphChangeWaker 的公告板实现
// ============================================================

// graphChangeWaker 按 C5d 既有 graph change 机制发布 __scheduler__ 唤醒任务。
type graphChangeWaker struct {
	store store.TaskStore
}

// graphChangeWakeMarker 是唤醒任务描述中的幂等标记（与 tools/plan_control.go
// 的 graphChangeMarker 同一格式：<graphID>/<activationID>/change）——两条
// 触发路径（request_replan 图任务 / acceptance 核验不通过）共享查重。
func graphChangeWakeMarker(graphID, activationID string) string {
	return "[graph-change-request: " + graphID + "/" + activationID + "/change]"
}

// WakeGraphChange 实现 graph.GraphChangeWaker：同一 activation 已有未处理
// （非终态）的同类唤醒任务时幂等返回，不重复发布。唤醒任务刻意不携带
// GraphID/NodeID/ActivationID：它是 Scheduler 的控制面输入而非图节点任务，
// 带图身份会被 graph-terminal-feed 当作节点终态回填引擎。
func (w graphChangeWaker) WakeGraphChange(spec graph.GraphChangeWakeSpec) error {
	marker := graphChangeWakeMarker(spec.GraphID, spec.ActivationID)
	// 幂等查重（MemoryTaskStore.ScanAll 永不返回错误；其它实现扫描失败时
	// 退化为直接发布——多一个唤醒任务无害，Scheduler 裁决天然幂等）。
	if tasks, err := w.store.ScanAll(); err == nil {
		for _, t := range tasks {
			if t == nil || t.EventType != "__scheduler__" || model.IsTerminal(t.Status) {
				continue
			}
			if strings.Contains(t.Description, marker) {
				return nil
			}
		}
	}
	description := fmt.Sprintf(
		"%s\n图 %s 的验收节点 %s（activation %s）服务端核验未通过（%s），自报 verdict 未被采信，节点已置 failed。\n原因：%s\n来源任务：%s\n处理指引：读取该图当前状态（当前 revision），用 patch_graph（base_revision CAS）裁决是否修改图定义（如调整验收判据、修复节点或改路由）；冲突时重新读取最新 revision 再改；判断无需修改时直接结束本任务。",
		marker, spec.GraphID, spec.NodeID, spec.ActivationID, spec.Reason, spec.Detail, spec.TaskID)
	wake := &model.Task{
		Description:    description,
		EventType:      "__scheduler__",
		EventSource:    "graph-change-request",
		ParentTaskID:   spec.TaskID,
		MaxConcurrency: 1, // 同一时刻只允许一个 Scheduler 处理同一请求
	}
	if err := w.store.PublishTask(wake); err != nil {
		return fmt.Errorf("发布 graph change 唤醒任务失败: %w", err)
	}
	return nil
}

// ============================================================
// Bootstrap 装配点
// ============================================================

// wireGraphAcceptanceBridge 装配 G1b acceptance 服务端核验桥：核验器 +
// graph change 唤醒器注入 graph.Runtime。journal 为 nil（OpenJournal 失败
// 降级）时照常装配——核验器运行时按 unverifiable 保守处理（不误判 valid），
// 与 Effect Journal「落账失败只降级」的纪律一致。
//
// 调用时序约束：与 graph approval/tool 桥同批（resumeNonTerminalGraphs 之前）——
// 恢复路径不触发 OnTaskTerminal，但启动后第一批验收任务终态必须已注入。
func wireGraphAcceptanceBridge(projectRoot string, journal *effect.Journal, taskStore store.TaskStore, rt *graph.Runtime) {
	rt.SetAcceptanceVerifier(&acceptanceVerifier{journal: journal, projectRoot: projectRoot})
	rt.SetChangeWaker(graphChangeWaker{store: taskStore})
}
