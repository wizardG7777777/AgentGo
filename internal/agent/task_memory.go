// task_memory.go 是 V6 §3 Task Memory（CM2，internal/taskmem）的 Agent Loop
// 接线：processTask 入口加载/创建，每个 settled Turn 收口处滚动更新，
// 历史压缩前 / Attempt 结束前 / 任务终态强制 checkpoint，并经 ctx 载体把
// 有界渲染注入每轮 messages（同时登记进 Context Manifest）。
//
// 证据纪律（V6 §3）：TurnFacts 只来自结构化账本——Store 的 ToolCallRecord
// （工具名/参数/成功否/exit code）、file_written 的 content hash 重算
// （与 local_write 同口径 sha256）、task.Artifacts 增量、用户决定正文。
// 模型文本声称不进入 Task Memory。
//
// nil-safe 降级：Agent.TaskMemStore 为 nil（未装配）时整链路关闭；taskmem
// 目录不可写等 IO 失败记日志后降级（Manifest 标注 dropped:<原因>），
// 绝不阻断任务执行。
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskmem"
	"agentgo/internal/trace"
)

// taskMemGoalMaxRunes 是 Goal 段的存储上限（任务描述的截断长度）。
const taskMemGoalMaxRunes = 500

// taskMemRuntime 是 processTask 一个 attempt 的 Task Memory 运行态：
// 内存对象 + 持久化句柄 + 注入载体 + 增量收集游标。
type taskMemRuntime struct {
	store *taskmem.Store
	mem   *taskmem.TaskMemory

	// toolRecordsSeen 是已消费 ToolCallRecord 身份的多重集。QueryToolCalls
	// 会从按工具分组的 map 合并历史；相同 Timestamp 的记录顺序不稳定，不能
	// 用切片前缀当游标。多重集既忽略重排，也保留完全相同记录的重复次数。
	toolRecordsSeen map[string]int
	artifactsSeen   map[string]struct{}
}

// initTaskMemory 在 processTask 入口加载或创建 Task Memory（attempt 恢复
// = 加载既有；新建 = 初始化 Goal/Constraints 并立即落盘 + emit created）。
// Agent.TaskMemStore 为 nil 时返回 nil（特性关闭，零开销）。
func (a *Agent) initTaskMemory(task *model.Task) *taskMemRuntime {
	if a.TaskMemStore == nil || task == nil {
		return nil
	}
	rt := &taskMemRuntime{
		store:           a.TaskMemStore,
		toolRecordsSeen: make(map[string]int),
		artifactsSeen:   make(map[string]struct{}, len(task.Artifacts)),
	}
	for _, p := range task.Artifacts {
		rt.artifactsSeen[p] = struct{}{}
	}
	mem, created, err := a.TaskMemStore.LoadOrCreate(task.ID)
	if err != nil {
		// IO 降级：记日志后继续，Manifest 经 carrier.dropped 标注。
		log.Printf("[agent %s] 任务 %s Task Memory 加载失败，降级为不启用: %v", a.ID, task.ID, err)
		return rt
	}
	rt.mem = mem
	if created {
		mem.Goal = truncateTaskMemRunes(task.Description, taskMemGoalMaxRunes)
		mem.Constraints = taskMemInitialConstraints(task)
		if err := a.TaskMemStore.Save(mem); err != nil {
			log.Printf("[agent %s] 任务 %s Task Memory 创建落盘失败，降级为不启用: %v", a.ID, task.ID, err)
			rt.mem = nil
			return rt
		}
		trace.Emit(trace.Event{
			Kind:        trace.KindTaskMemoryCreated,
			TaskID:      task.ID,
			AgentID:     a.ID,
			Description: mem.SummaryJSON(),
		})
	} else {
		log.Printf("[agent %s] 任务 %s Task Memory 恢复：version=%d（retry=%d）", a.ID, task.ID, mem.Version, task.RetryCount)
	}
	// 账本基线：之前 attempt 已发生的工具调用不计入本轮新增。
	if recs, qerr := a.Store.QueryToolCalls(task.ID, ""); qerr == nil {
		rt.toolRecordsSeen = taskMemToolRecordMultiset(recs)
	}
	return rt
}

// taskMemInitialConstraints 从任务契约提取约束（capability 覆盖与预期产物）。
func taskMemInitialConstraints(task *model.Task) []string {
	var out []string
	if task.GraphID != "" {
		out = append(out, "本任务是 agentTask：最终纯文本可以正常结束；也可用 submit_task_result 提交结构化结果；输入中的候选版本是本次工作基线。")
	}
	if task.Capability != nil {
		if len(task.Capability.Tools) > 0 {
			out = append(out, "工具子集: "+strings.Join(task.Capability.Tools, ","))
		}
		if task.Capability.Model != "" {
			out = append(out, "模型覆盖: "+task.Capability.Model)
		}
		if task.Capability.Isolation != nil && task.Capability.Isolation.Mode != "" {
			out = append(out, "执行隔离: "+string(task.Capability.Isolation.Mode))
		}
	}
	if len(task.ExpectedArtifacts) > 0 {
		out = append(out, "预期产物: "+strings.Join(task.ExpectedArtifacts, ","))
	}
	return out
}

// recordAttemptEnd 把本 attempt 的终止原因写入 Task Memory（有界截断，
// 去重防抖），重试接手时随渲染注入——模型由此看到「上一次是怎么死的」，
// 不再把重试误读为全新问答（2026-08-20 SWE-001 预防 3）。落盘失败降级
// 为进程内继续，不阻断失败处理主路径。
func (rt *taskMemRuntime) recordAttemptEnd(a *Agent, taskID, cause string) {
	if rt == nil || rt.mem == nil {
		return
	}
	if !taskmem.ApplyAttemptEnd(rt.mem, cause) {
		return
	}
	if err := rt.store.Save(rt.mem); err != nil {
		log.Printf("[agent %s] 任务 %s attempt 终止原因写入 Task Memory 失败（继续内存态）: %v", a.ID, taskID, err)
	}
}

// applySettledTurn 是一个 settled Turn 的收口：收集本轮结构化事实 →
// ApplyTurn → 仅实质变化时落盘 + 刷新注入 + emit task_memory_updated。
// 重复读取 / 无新增证据的轮次 ApplyTurn 返回 false，什么都不发生。
func (rt *taskMemRuntime) applySettledTurn(a *Agent, taskID string, result ExecuteResult, loop int) {
	if rt == nil || rt.mem == nil {
		return
	}
	facts := rt.collectTurnFacts(a, taskID, result)
	if !taskmem.ApplyTurn(rt.mem, facts) {
		return
	}
	if err := rt.store.Save(rt.mem); err != nil {
		// 落盘失败：降级为进程内继续（内存仍是最新的），记日志不阻断。
		log.Printf("[agent %s] 任务 %s Task Memory 落盘失败（继续内存态）: %v", a.ID, taskID, err)
	}
	trace.Emit(trace.Event{
		Kind:        trace.KindTaskMemoryUpdated,
		TaskID:      taskID,
		AgentID:     a.ID,
		Loop:        loop,
		Description: rt.mem.SummaryJSON(),
	})
}

func executeResultCalledTool(result ExecuteResult, name string) bool {
	for _, call := range result.ToolCalls {
		if call.Name == name {
			return true
		}
	}
	return false
}

// checkpoint 强制落盘并 emit task_memory_checkpointed（历史压缩前 /
// Attempt 结束前 / 任务终态）。无实质变化时也执行——checkpoint 是
// 持久化保证，不是版本递增。
func (rt *taskMemRuntime) checkpoint(a *Agent, taskID string, loop int, reason string) {
	if rt == nil || rt.mem == nil {
		return
	}
	if err := rt.store.Save(rt.mem); err != nil {
		log.Printf("[agent %s] 任务 %s Task Memory checkpoint 落盘失败: %v", a.ID, taskID, err)
		return
	}
	trace.Emit(trace.Event{
		Kind:        trace.KindTaskMemoryCheckpointed,
		TaskID:      taskID,
		AgentID:     a.ID,
		Loop:        loop,
		Reason:      reason,
		Description: rt.mem.SummaryJSON(),
	})
}

// recordBlockedReason 在 blocked 终态迁移前把结构化 blocked_reason
// 写入 Task Memory。终态事件到达后 Session promotion 会等待
// finalize 的 sealed checkpoint，因此此处只记录实质变化，不提前封存。
func (rt *taskMemRuntime) recordBlockedReason(a *Agent, taskID, reason string) {
	if rt == nil || rt.mem == nil || !taskmem.ApplyBlockedReason(rt.mem, reason) {
		return
	}
	if err := rt.store.Save(rt.mem); err != nil {
		log.Printf("[agent %s] 任务 %s blocked_reason 写入 Task Memory 失败: %v", a.ID, taskID, err)
		return
	}
	trace.Emit(trace.Event{
		Kind:        trace.KindTaskMemoryUpdated,
		TaskID:      taskID,
		AgentID:     a.ID,
		Loop:        -1,
		Reason:      "agent_reported_blocked",
		Description: rt.mem.SummaryJSON(),
	})
}

// finalize 在 processTask 退出时（defer）按任务最终状态收口 Task Memory：
// 终态（completed/failed/cancelled/blocked）置 Sealed 封存并 checkpoint；
// 重试回滚（pending）只 checkpoint 不封存——下一 attempt 加载后继续滚动。
// 注意：本 defer 先于 panic 恢复 defer 执行，panic 路径任务仍为 processing，
// 按 attempt_end 落盘（不封存），与「崩溃恢复后可继续」语义一致。
func (rt *taskMemRuntime) finalize(a *Agent, taskID string) {
	if rt == nil || rt.mem == nil {
		return
	}
	status := model.TaskStatus("")
	if cur, err := a.Store.GetTask(taskID); err == nil && cur != nil {
		status = cur.Status
	}
	switch status {
	case model.TaskStatusCompleted, model.TaskStatusFailed,
		model.TaskStatusCancelled, model.TaskStatusBlocked:
		rt.mem.Sealed = true
		rt.mem.Phase = "终态:" + string(status)
		rt.checkpoint(a, taskID, -1, "terminal:"+string(status))
	default:
		rt.checkpoint(a, taskID, -1, "attempt_end")
	}
}

// collectTurnFacts 从结构化账本收集一个 settled Turn 的 TurnFacts：
// ToolCallRecord 增量（成功否/exit code/参数目标）、apply_change 的 content
// hash 重算、task.Artifacts 增量、request_user_input 的用户决定正文。
// 错误串经 callID 与本轮 ToolResults 精确连接。旧账目没有 callID 时，只在
// tool name + args 对本轮调用形成唯一一对一匹配时保守回退。
func (rt *taskMemRuntime) collectTurnFacts(a *Agent, taskID string, result ExecuteResult) taskmem.TurnFacts {
	var facts taskmem.TurnFacts

	if recs, err := a.Store.QueryToolCalls(taskID, ""); err == nil {
		delta := rt.takeUnseenToolRecords(recs)
		contentByRecord := matchTaskMemToolRecordContent(delta, result)
		for i, rec := range delta {
			tf := taskmem.ToolCallFact{
				Name:          rec.ToolName,
				Target:        summarizeTaskMemTarget(rec.ToolName, rec.Args),
				Success:       rec.Success,
				ExitCode:      rec.ExitCode,
				ExitCodeScope: string(rec.ExitCodeScope),
			}
			if content, matched := contentByRecord[i]; matched {
				if !rec.Success {
					tf.Err = truncateTaskMemRunes(strings.TrimSpace(content), 160)
				} else if rec.ToolName == "request_user_input" {
					facts.UserDecision = truncateTaskMemRunes(strings.TrimSpace(content), 200)
				}
			}
			facts.ToolCalls = append(facts.ToolCalls, tf)

			// file_written 证据：apply_change 的 hash 对 content 参数重算
			//（与 local_write 的 computeSHA256 同口径）；apply_change 只记路径。
			if rec.Success && (rec.ToolName == "apply_change") {
				if p, _ := rec.Args["path"].(string); p != "" {
					fw := taskmem.FileWrittenFact{Path: p}
					if rec.ToolName == "apply_change" {
						if c, _ := rec.Args["content"].(string); c != "" {
							sum := sha256.Sum256([]byte(c))
							fw.Hash = hex.EncodeToString(sum[:])
						}
					}
					facts.FilesWritten = append(facts.FilesWritten, fw)
				}
			}
		}
	}

	// artifact 增量：record-artifact Reactor 登记的产物（含 shell 补登）。
	if cur, err := a.Store.GetTask(taskID); err == nil && cur != nil {
		for _, p := range cur.Artifacts {
			if _, seen := rt.artifactsSeen[p]; seen {
				continue
			}
			rt.artifactsSeen[p] = struct{}{}
			facts.NewArtifacts = append(facts.NewArtifacts, p)
		}
	}
	return facts
}

// takeUnseenToolRecords returns the append-only ledger delta without assuming
// QueryToolCalls preserves a stable order for equal timestamps.
func (rt *taskMemRuntime) takeUnseenToolRecords(records []store.ToolCallRecord) []store.ToolCallRecord {
	current := make(map[string]int, len(records))
	delta := make([]store.ToolCallRecord, 0)
	for _, record := range records {
		identity := taskMemToolRecordIdentity(record)
		occurrence := current[identity]
		current[identity] = occurrence + 1
		if occurrence >= rt.toolRecordsSeen[identity] {
			delta = append(delta, record)
		}
	}
	rt.toolRecordsSeen = current
	return delta
}

func taskMemToolRecordMultiset(records []store.ToolCallRecord) map[string]int {
	seen := make(map[string]int, len(records))
	for _, record := range records {
		seen[taskMemToolRecordIdentity(record)]++
	}
	return seen
}

// taskMemToolRecordIdentity is deliberately independent of slice position.
// CallID alone is not sufficient because some compatible providers reuse IDs
// across responses, so the durable record fields participate as well.
func taskMemToolRecordIdentity(record store.ToolCallRecord) string {
	payload := struct {
		Timestamp string         `json:"timestamp"`
		CallID    string         `json:"call_id"`
		AgentID   string         `json:"agent_id"`
		ToolName  string         `json:"tool_name"`
		Args      map[string]any `json:"args"`
		Success   bool           `json:"success"`
		ExitCode  *int           `json:"exit_code"`
	}{
		Timestamp: record.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		CallID:    record.CallID, AgentID: record.AgentID, ToolName: record.ToolName,
		Args: record.Args, Success: record.Success, ExitCode: record.ExitCode,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		// Tool arguments originate as JSON, so this is only a defensive fallback.
		data = []byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%#v\x00%t\x00%v",
			payload.Timestamp, payload.CallID, payload.AgentID, payload.ToolName,
			payload.Args, payload.Success, payload.ExitCode))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// matchTaskMemToolRecordContent joins durable records to this turn's result.
// New records use CallID. Legacy records (CallID empty) are matched only when
// their tool-name/args signature is unique on both sides after exact matches.
func matchTaskMemToolRecordContent(records []store.ToolCallRecord, result ExecuteResult) map[int]string {
	matched := make(map[int]string)
	resultCount := make(map[string]int, len(result.ToolResults))
	resultContent := make(map[string]string, len(result.ToolResults))
	for _, toolResult := range result.ToolResults {
		resultCount[toolResult.ToolCallID]++
		resultContent[toolResult.ToolCallID] = toolResult.Content
	}
	callCount := make(map[string]int, len(result.ToolCalls))
	callByID := make(map[string]llm.ToolCall, len(result.ToolCalls))
	for _, call := range result.ToolCalls {
		callCount[call.ID]++
		callByID[call.ID] = call
	}

	reservedCallIDs := make(map[string]struct{})
	for i, record := range records {
		if record.CallID == "" {
			continue
		}
		reservedCallIDs[record.CallID] = struct{}{}
		call, ok := callByID[record.CallID]
		if !ok || callCount[record.CallID] != 1 || resultCount[record.CallID] != 1 || call.Name != record.ToolName {
			continue
		}
		matched[i] = resultContent[record.CallID]
	}

	legacyRecords := make(map[string][]int)
	for i, record := range records {
		if record.CallID == "" {
			signature := taskMemToolCallSignature(record.ToolName, record.Args)
			legacyRecords[signature] = append(legacyRecords[signature], i)
		}
	}
	legacyCalls := make(map[string][]llm.ToolCall)
	for _, call := range result.ToolCalls {
		if _, reserved := reservedCallIDs[call.ID]; reserved {
			continue
		}
		signature := taskMemToolCallSignature(call.Name, call.Arguments)
		legacyCalls[signature] = append(legacyCalls[signature], call)
	}
	for signature, indices := range legacyRecords {
		calls := legacyCalls[signature]
		if len(indices) != 1 || len(calls) != 1 {
			continue
		}
		call := calls[0]
		if callCount[call.ID] != 1 || resultCount[call.ID] != 1 {
			continue
		}
		matched[indices[0]] = resultContent[call.ID]
	}
	return matched
}

func taskMemToolCallSignature(name string, args map[string]any) string {
	data, err := json.Marshal(struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}{Name: name, Args: args})
	if err != nil {
		return name + "\x00" + fmt.Sprintf("%#v", args)
	}
	return string(data)
}

// summarizeTaskMemTarget 生成工具调用的目标摘要（有界）：按工具语义挑选
// 最具定位价值的参数。
func summarizeTaskMemTarget(toolName string, args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	for _, key := range []string{"path", "command", "to", "question", "url", "pattern", "query", "description", "task_id"} {
		if v, ok := args[key].(string); ok && v != "" {
			return truncateTaskMemRunes(strings.TrimSpace(v), 80)
		}
	}
	return ""
}

// truncateTaskMemRunes 按 rune 截断（与 taskmem 包内截断同手法）。
func truncateTaskMemRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

// insertTaskMemMessage 把 Task Memory 渲染插入 messages：紧随 user 首条
// （任务描述）之后——Task Memory 是「当前工作状态」，装配优先级仅次于
// 任务契约，高于历史流（V6 §3「优先注入完整但有界的当前 Task Memory」）。
