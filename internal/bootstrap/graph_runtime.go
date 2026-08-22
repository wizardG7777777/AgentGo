package bootstrap

// 本文件是 V6 Graph 运行桥接（C5a）：把 internal/graph 的 Runtime 引擎接进
// 活系统。三件东西：
//   - graphBoard：graph.TaskBoard → 真实任务公告板（store.TaskStore）的桥，
//     图任务携带 GraphID/NodeID/ActivationID 身份（与普通兼容任务区分）；
//   - graphFeedReactor：订阅四种任务终态事件，把图任务的终态事实回填
//     Runtime.OnTaskTerminal 驱动转移求值；
//   - wireGraphRuntime / resumeNonTerminalGraphs：Bootstrap 装配点
//     （持久化恢复、Reactor 注册、非终态图恢复执行）与 System 字段的来源。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/config"
	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/outcomestore"
	"agentgo/internal/policycatalog"
	"agentgo/internal/reactor"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/trace"
)

// ============================================================
// graphBoard —— graph.TaskBoard 的公告板实现
// ============================================================

// graphBoard 把 Graph Runtime 的节点任务发布桥到真实任务公告板。
//
// 幂等纪律（graph.TaskBoard 接口契约）：PublishGraphTask 以
// (GraphID, ActivationID) 为幂等键——进程在「任务已发布、task_id 尚未
// durable」的崩溃窗口后，ResumeGraph 会用同一 activation 补发，本实现
// 必须去重而不是制造重复任务。去重依据是公告板中现存任务的图身份
// （经 Session 快照跨重启保留），进程内另加一层索引做快路径。
type graphBoard struct {
	store    store.TaskStore
	policies *policycatalog.Catalog
	// recoveryQuarantine 是 Effect Journal 启动裁决仍为 unknown 的旧
	// task_id → 原因。Graph journal 可能保留 execution.task_id，但 Session
	// Task 快照已丢失；此时不得把 lookup miss 当作安全补发。
	recoveryQuarantine map[string]string
	// effectJournal 是 TaskStore miss 时的最后一道恢复闸。只要候选 TaskID
	// 在 durable Effect Journal 中有任何历史（包括 settled），就不能把整
	// 个任务当作“从未执行”重发；settled 只证明某个副作用发生，不证明任务完成。
	effectJournal    *effect.Journal
	outcomeAuthority *graphTaskOutcomeAuthority

	mu sync.Mutex
	// byActivation 是 (graphID \x00 activationID) → taskID 的进程内索引，
	// 仅作快路径；miss 时仍需扫公告板（覆盖重启后索引为空的恢复路径）。
	byActivation map[string]string

	// cleanupMu serializes idempotent Graph terminal cleanup across the live
	// Runtime path and any explicit recovery call using this board instance.
	cleanupMu sync.Mutex
}

var _ graph.GraphTaskTerminator = (*graphBoard)(nil)

func newGraphBoard(s store.TaskStore, quarantine ...map[string]string) *graphBoard {
	return newGraphBoardWithEffects(s, nil, quarantine...)
}

func newGraphBoardWithEffects(s store.TaskStore, journal *effect.Journal, quarantine ...map[string]string) *graphBoard {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		panic("初始化 Graph Task PolicyCatalog 失败: " + err.Error())
	}
	return newGraphBoardWithPolicies(s, journal, catalog, quarantine...)
}

func newGraphBoardWithPolicies(s store.TaskStore, journal *effect.Journal, policies *policycatalog.Catalog,
	quarantine ...map[string]string) *graphBoard {
	var q map[string]string
	if len(quarantine) > 0 {
		q = quarantine[0]
	}
	return &graphBoard{store: s, policies: policies, recoveryQuarantine: q, effectJournal: journal, byActivation: make(map[string]string)}
}

func graphActivationKey(graphID, activationID string) string {
	return graphID + "\x00" + activationID
}

// graphTaskID 为每个 (graph_id, activation_id) 预留稳定的 Task ID。稳定身份
// 不只是进程内去重：若进程死在「任务已发布并产生 Effect、Graph journal 尚未
// 写回 task_id」的窗口，重启后的 Effect quarantine 仍能从 activation 推导出
// 原 TaskID，避免把 lookup miss 当成可安全补发。格式保持 UUID 形状，便于既有
// trace / CLI 的 task_id 前缀检索继续获得足够熵。
func graphTaskID(graphID, activationID string) string {
	sum := sha256.Sum256([]byte(graphActivationKey(graphID, activationID)))
	h := fmt.Sprintf("%x", sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// PublishGraphTask 实现 graph.TaskBoard：组装带图身份的 model.Task 发布到
// 公告板，返回生成的 task.ID。spec.Tools/Model/Isolation 任一非空时挂
// model.NodeCapability，沿用现有 per-node 能力机制（QueryAvailable 过滤 +
// 认领 fail-closed 双保险）。
//
// 由 graph.Runtime 在 rt.mu 锁内同步调用，同一 Runtime 不存在并发补发。
func (b *graphBoard) PublishGraphTask(spec graph.TaskSpec) (string, error) {
	if spec.NodeKind == "" {
		return "", fmt.Errorf("发布图任务 %s/%s 缺少冻结 node_kind", spec.GraphID, spec.ActivationID)
	}
	key := graphActivationKey(spec.GraphID, spec.ActivationID)
	reservedID := graphTaskID(spec.GraphID, spec.ActivationID)
	b.mu.Lock()
	indexedID := b.byActivation[key]
	b.mu.Unlock()
	if indexedID != "" {
		if task, err := b.store.GetTask(indexedID); err == nil && task != nil &&
			task.GraphID == spec.GraphID && task.ActivationID == spec.ActivationID {
			if err := validateExistingGraphTaskKind(task, spec); err != nil {
				return "", err
			}
			return task.ID, nil
		}
	}

	// 快路径未命中：扫公告板找同 activation 的现存任务。覆盖重启恢复路径——
	// 崩溃前已发布的任务经 Session 快照带回图身份，而进程内索引为空。
	// （MemoryTaskStore.ScanAll 永不返回错误；其它实现扫描失败时退化为直接
	// 发布，与 v5 兼容性任务的发布语义一致。）
	if tasks, err := b.store.ScanAll(); err == nil {
		for _, t := range tasks {
			if t != nil && t.GraphID == spec.GraphID && t.ActivationID == spec.ActivationID {
				if err := validateExistingGraphTaskKind(t, spec); err != nil {
					return "", err
				}
				b.mu.Lock()
				b.byActivation[key] = t.ID
				b.mu.Unlock()
				return t.ID, nil
			}
		}
	}
	// 真实 Task 不存在时才看 Effect 历史；已有 Session Task 是更完整的权威
	// 事实，不能仅因它曾产生 settled Effect 就误伤。Task 缺失 + 任意 Effect
	// 则 fail-closed，避免从头重放已发生的 Shell/消息/文件写等副作用。
	if taskID, reason := b.missingTaskEffectFence(reservedID); reason != "" {
		return "", fmt.Errorf("图任务 %s/%s 缺失但已有 durable Effect 历史（task_id=%s），拒绝整任务重放：%s",
			spec.GraphID, spec.ActivationID, taskID, reason)
	}

	task := &model.Task{
		ID:            reservedID,
		RunID:         spec.RunID,
		Description:   graphTaskDescription(spec),
		ContextInputs: graphTaskContextInputs(spec),
		EventType:     spec.Route,
		// 一次 Graph activation 对应一个确定性 Task/ExecutionLease，只允许
		// 一个 Runner 执行。否则会继承公告板默认并发度，让多个 Team Agent
		// 重复认领同一节点，首个提交后其余执行者只能收到 lease revoked。
		MaxConcurrency:               1,
		GraphID:                      spec.GraphID,
		NodeID:                       spec.NodeID,
		ActivationID:                 spec.ActivationID,
		GraphNodeKind:                string(spec.NodeKind),
		GraphDefinitionDigestVersion: spec.DefinitionDigestVersion,
		RouteScope:                   model.GraphRouteScope(spec.GraphID),
	}
	if spec.RunContract != nil {
		run := *spec.RunContract
		task.RunContract = &run
	}
	if spec.ProgressContractRef != "" {
		if b.policies == nil {
			return "", fmt.Errorf("发布图任务被拒绝：Progress PolicyCatalog 未装配")
		}
		profile, ok := b.policies.ProgressContract(spec.ProgressContractRef)
		if !ok {
			return "", fmt.Errorf("发布图任务被拒绝：未知 ProgressContractRef %q", spec.ProgressContractRef)
		}
		task.ProgressContract = &profile.Contract
	}
	if spec.ContextPolicyRef != "" {
		if b.policies == nil || !b.policies.HasContextPolicy(spec.ContextPolicyRef) {
			return "", fmt.Errorf("发布图任务被拒绝：未知 ContextPolicyRef %q", spec.ContextPolicyRef)
		}
		task.ContextPolicyRef = spec.ContextPolicyRef
	}
	// 真正 legacy Graph 的运行 binding 全空，不能仅因桥接层写入 execution
	// phase 就被中央四件套闸门误判成半绑定新 Task。任一新 binding 出现时才
	// 显式冻结 execution；此时缺失的其它字段仍由 Store fail-closed。
	if task.RunID != "" || task.RunContract != nil || task.ProgressContract != nil || task.ContextPolicyRef != "" {
		task.RunPhase = runcontract.PhaseExecution
	}
	if len(spec.Tools) > 0 || spec.Model != "" || spec.Isolation != "" {
		task.Capability = &model.NodeCapability{Tools: spec.Tools, Model: spec.Model}
		if spec.Isolation != "" {
			task.Capability.Isolation = &model.IsolationSpec{Mode: spec.Isolation}
		}
	}
	if err := b.store.PublishTask(task); err != nil {
		return "", fmt.Errorf("发布图任务（图 %s 节点 %s activation %s）失败: %w",
			spec.GraphID, spec.NodeID, spec.ActivationID, err)
	}
	b.mu.Lock()
	b.byActivation[key] = task.ID
	b.mu.Unlock()
	return task.ID, nil
}

// validateExistingGraphTaskKind 校验同 activation 的已发布 Task 没有被另一种
// Graph 角色复用。旧快照的 GraphNodeKind 为空时保留兼容：ExecutionLease 会
// 按最小权限只授予 submit_task_result；非空但不同则恢复 fail-closed。
func validateExistingGraphTaskKind(task *model.Task, spec graph.TaskSpec) error {
	if task == nil || task.GraphNodeKind == "" {
		return nil
	}
	if task.GraphNodeKind != string(spec.NodeKind) {
		return fmt.Errorf("图任务 %s/%s 的已持久化 node_kind=%s，与当前冻结定义=%s 不一致",
			spec.GraphID, spec.ActivationID, task.GraphNodeKind, spec.NodeKind)
	}
	return nil
}

// LookupGraphTask 实现 graph.TaskBoard 的恢复核对面。Graph Runtime 不能
// 只相信 GraphDocument.execution.task_id：Session 恢复可能缺失该任务，
// 也可能任务已先到终态但 graph-terminal-feed 尚未来得及回填。这里以
// (graph_id, activation_id) 扫描公告板的当前权威事实并返回结构化快照。
func (b *graphBoard) LookupGraphTask(graphID, activationID, expectedTaskID string) (graph.GraphTaskSnapshot, bool, error) {
	tasks, err := b.store.ScanAll()
	if err != nil {
		return graph.GraphTaskSnapshot{}, false, err
	}
	for _, task := range tasks {
		if task == nil || task.GraphID != graphID || task.ActivationID != activationID {
			continue
		}
		b.mu.Lock()
		b.byActivation[graphActivationKey(graphID, activationID)] = task.ID
		b.mu.Unlock()
		snapshot := graph.GraphTaskSnapshot{TaskID: task.ID, NodeKind: graph.NodeKind(task.GraphNodeKind)}
		if terminal, ok := graphTerminalStatusOf(task.Status); ok {
			if b.outcomeAuthority != nil {
				fact, typed, outcomeErr := b.outcomeAuthority.FactForTask(context.Background(), task)
				if outcomeErr != nil {
					return graph.GraphTaskSnapshot{}, false, outcomeErr
				}
				if typed {
					snapshot.TerminalStatus = fact.Status
					snapshot.OutcomeRef = task.OutcomeRef
					snapshot.Result = fact.Result
					snapshot.Evidence = fact.Evidence
					return snapshot, true, nil
				}
			}
			snapshot.TerminalStatus = terminal
			snapshot.Result = graphTaskResult(task)
			snapshot.Evidence = assembleTaskEvidence(b.store, task)
		}
		return snapshot, true, nil
	}
	if b.outcomeAuthority != nil {
		fact, found, outcomeErr := b.outcomeAuthority.FactByActivation(context.Background(), graphID, activationID, expectedTaskID)
		if outcomeErr != nil {
			return graph.GraphTaskSnapshot{}, false, outcomeErr
		}
		if found {
			outcomeRef, _ := fact.Result["_task_outcome_ref"].(string)
			return graph.GraphTaskSnapshot{
				TaskID: fact.TaskID, TerminalStatus: fact.Status,
				OutcomeRef: outcomeRef,
				Result:     fact.Result, Evidence: fact.Evidence,
			}, true, nil
		}
	}
	// Store miss 后同时检查 durable execution.task_id（旧随机 ID）与由
	// graph/activation 推导的确定性 ID。任一 ID 有 Effect 历史，都只能合成
	// blocked 交人工/Scheduler 核验，不能把某条 settled Effect 当成整任务完成。
	candidates := []string{expectedTaskID, graphTaskID(graphID, activationID)}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		if taskID, reason := b.missingTaskEffectFence(candidate); reason != "" {
			return graph.GraphTaskSnapshot{
				TaskID:         taskID,
				TerminalStatus: graph.NodeBlocked,
				Result: map[string]any{
					"status": "blocked",
					"error":  reason,
					"event":  graph.EventBlocked,
				},
			}, true, nil
		}
	}
	return graph.GraphTaskSnapshot{}, false, nil
}

// missingTaskEffectFence 仅供已经确认 TaskStore miss 的路径调用。返回非空
// reason 表示候选 TaskID 有 durable 副作用事实，整任务重放不安全。
func (b *graphBoard) missingTaskEffectFence(taskID string) (string, string) {
	if taskID == "" {
		return "", ""
	}
	if reason := b.recoveryQuarantine[taskID]; reason != "" {
		return taskID, reason
	}
	if b.effectJournal == nil {
		return "", ""
	}
	effects, err := b.effectJournal.QueryStrict(taskID)
	if err != nil {
		return taskID, "effect_authority_unavailable: " + err.Error()
	}
	if len(effects) == 0 {
		return "", ""
	}
	first := effects[0]
	reason := fmt.Sprintf("effect_history_recovery_quarantine: Task 事实缺失但账本已有 %d 条 Effect（首条 id=%s kind=%s status=%s policy=%s）；既有副作用可能已发生，禁止自动整任务重放，需核验后 replan",
		len(effects), first.ID, first.Kind, first.Status, first.Policy)
	return taskID, reason
}

// graphTaskDescription 只组装 L1 objective/control contract。Graph Result 与
// Evidence 通过 graphTaskContextInputs 进入 L2 upstream section，禁止再把
// 数据流正文拼进 Description/user_task。
func graphTaskDescription(spec graph.TaskSpec) string {
	desc := spec.Title
	if spec.Description != "" {
		desc = spec.Title + "\n\n" + spec.Description
	}
	if len(spec.MissingEvidence) > 0 {
		desc += "\n\n## 当前证据缺口（Runtime 已核对）\n强制处置：本次验收必须以 blocked 结算并逐项说明缺口，不得提交 pass。"
		for _, requirement := range spec.MissingEvidence {
			desc += fmt.Sprintf("\n- 输入端口 %s 缺少 kind=%s", requirement.Input, requirement.Kind)
		}
	}
	if len(spec.RequiredEvidence) > 0 {
		desc += "\n\n## 验收证据契约（随本 activation 冻结）"
		for _, requirement := range spec.RequiredEvidence {
			desc += fmt.Sprintf("\n- 输入端口 %s 必须提供 kind=%s 的可解引用 Evidence", requirement.Input, requirement.Kind)
		}
	}
	if typed := renderTypedOutputContract(spec.TypedOutputContract); typed != "" {
		desc += "\n\n" + typed
	}
	if strings.TrimSpace(spec.OutputContract) != "" {
		desc += "\n\n" + spec.OutputContract
	}
	return desc
}

// graphTaskContextInputs 把每个冻结 InputBinding 拆成独立 Result/Evidence 数据
// 端口。每个端口都有稳定 source_ref；L2 可以分别外置、预算和审计，L1 Prompt
// 不再承担数据搬运。
func graphTaskContextInputs(spec graph.TaskSpec) []model.TaskContextInput {
	inputs := make([]model.TaskContextInput, 0, len(spec.Inputs)*2)
	for _, input := range spec.Inputs {
		resultPayload := struct {
			SourceNodeID       string          `json:"source_node_id"`
			SourceActivationID string          `json:"source_activation_id"`
			TargetInput        string          `json:"target_input,omitempty"`
			Summary            string          `json:"summary,omitempty"`
			WorkLog            string          `json:"work_log,omitempty"`
			ResultRef          string          `json:"result_ref,omitempty"`
			Result             json.RawMessage `json:"result,omitempty"`
			Truncated          bool            `json:"truncated,omitempty"`
		}{
			SourceNodeID: input.SourceNodeID, SourceActivationID: input.SourceActivationID,
			TargetInput: input.TargetInput, Summary: input.Summary, WorkLog: input.WorkLog,
			ResultRef: input.ResultRef, Result: append(json.RawMessage(nil), input.Result...), Truncated: input.Truncated,
		}
		if raw, err := json.Marshal(resultPayload); err == nil {
			inputs = append(inputs, model.TaskContextInput{
				Kind: model.TaskContextUpstreamResult,
				SourceRef: fmt.Sprintf("graph:%s/activation:%s/result:%s", spec.GraphID,
					input.SourceActivationID, contextInputPort(input.TargetInput)),
				Content: "<upstream-result authority=\"graph-dataflow\">\n" + string(raw) + "\n</upstream-result>",
			})
		}
		if len(input.Evidence) > 0 || len(input.EvidenceRefs) > 0 {
			knownEvidence := make(map[string]struct{}, len(input.Evidence))
			for _, entry := range input.Evidence {
				if entry.Ref != "" {
					knownEvidence[entry.Ref] = struct{}{}
				}
			}
			unresolved := make([]string, 0)
			for _, ref := range input.EvidenceRefs {
				if _, ok := knownEvidence[ref]; !ok {
					unresolved = append(unresolved, ref)
				}
			}
			evidencePayload := struct {
				SourceNodeID       string                `json:"source_node_id"`
				SourceActivationID string                `json:"source_activation_id"`
				TargetInput        string                `json:"target_input,omitempty"`
				Evidence           []graph.EvidenceEntry `json:"evidence,omitempty"`
				EvidenceRefs       []string              `json:"evidence_refs,omitempty"`
				UnresolvedRefs     []string              `json:"unresolved_evidence_refs,omitempty"`
			}{
				SourceNodeID: input.SourceNodeID, SourceActivationID: input.SourceActivationID,
				TargetInput: input.TargetInput, Evidence: append([]graph.EvidenceEntry(nil), input.Evidence...),
				EvidenceRefs:   append([]string(nil), input.EvidenceRefs...),
				UnresolvedRefs: unresolved,
			}
			if raw, err := json.Marshal(evidencePayload); err == nil {
				inputs = append(inputs, model.TaskContextInput{
					Kind: model.TaskContextUpstreamEvidence,
					SourceRef: fmt.Sprintf("graph:%s/activation:%s/evidence:%s", spec.GraphID,
						input.SourceActivationID, contextInputPort(input.TargetInput)),
					Content: "<upstream-evidence authority=\"graph-dataflow\">\n" + string(raw) + "\n</upstream-evidence>",
				})
			}
		}
	}
	return inputs
}

func contextInputPort(port string) string {
	port = strings.TrimSpace(port)
	if port == "" {
		return "default"
	}
	return port
}

func renderTypedOutputContract(contract *graph.NodeOutputContract) string {
	if contract == nil {
		return ""
	}
	raw, err := json.Marshal(contract)
	if err != nil {
		return "<typed-output-contract>\n编码失败；禁止提交 completed，改为 blocked 并报告 output_contract_error。\n</typed-output-contract>"
	}
	return "<typed-output-contract>\n" + string(raw) +
		"\nsummary_required 约束 TaskOutcome.summary；fields 中 required=true 的 path 必须出现在结构化 result object，type 必须匹配。" +
		"\n</typed-output-contract>"
}

// ============================================================
// graphFeedReactor —— 任务终态 → Graph Runtime 的回填器
// ============================================================

// graphTerminalSink 是 graphFeedReactor 对 Graph Runtime 的最小依赖
// （*graph.Runtime 满足）；单测用 fake 注入。
type graphTerminalSink interface {
	OnTaskTerminal(f graph.TerminalFact) error
	// FailTerminalWriteback 是 OnTaskTerminal 失败的回落（SWE-002）：把该
	// activation 节点显式置 failed 并唤醒 Scheduler 裁决，保证终态事实
	// 永远有处置路径。
	FailTerminalWriteback(f graph.TerminalFact, cause error) error
}

// graphFeedReactor 把任务终态事件回填给 Graph Runtime：取终态任务的图身份
// （GraphID 为空 = 非图任务，直接忽略），组装 graph.TerminalFact 调
// OnTaskTerminal 驱动转移求值。它是 Graph 控制面而非观测旁路，必须走
// Registry 的可靠 async 通道：普通 async 背压会丢事件，导致
// 任务已终态但 Graph 永久不推进。Runtime 的 durable 幂等结算负责
// 重复/迟到事件。
type graphFeedReactor struct {
	store            store.TaskStore
	sink             graphTerminalSink
	outcomeAuthority *graphTaskOutcomeAuthority
}

// TerminateGraphTasks implements graph.GraphTaskTerminator. It is invoked by
// Runtime after the Graph terminal status is durable and before graph_ended,
// keeping Task state transitions in the Graph main control flow rather than a
// Reactor response. A parallel branch can settle the Graph while siblings are
// pending/processing; leaving them live would execute outside the settled
// contract or become no_compatible_route debris after Team teardown.
func (b *graphBoard) TerminateGraphTasks(graphID string) error {
	graphID = strings.TrimSpace(graphID)
	if graphID == "" || b == nil || b.store == nil {
		return nil
	}
	b.cleanupMu.Lock()
	defer b.cleanupMu.Unlock()
	return b.terminateGraphTasksLocked(graphID)
}

// terminateGraphTasksLocked is shared by the live Runtime path and startup recovery.
// The Graph Store terminal status is the durable authority; trace delivery is
// deliberately not required for correctness because a process can die between
// the GraphStore terminal commit and this TaskStore cleanup. Caller holds
// b.cleanupMu.
func (b *graphBoard) terminateGraphTasksLocked(graphID string) error {
	tasks, err := b.store.ScanAll()
	if err != nil {
		return fmt.Errorf("scan tasks for terminal graph %s: %w", graphID, err)
	}
	var errs []error
	for _, task := range tasks {
		if task == nil || task.GraphID != graphID || model.IsTerminal(task.Status) {
			continue
		}
		reason := fmt.Sprintf("Graph %s 已终态，取消仍在途节点 %s/%s", graphID, task.NodeID, task.ActivationID)
		from, changed, err := cancelLiveGraphTask(b.store, task.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("cancel graph task %s: %w", task.ID, err))
			continue
		}
		if changed && from == model.TaskStatusPending {
			// Pending tasks have no executing Agent to emit the cancellation fact.
			event := trace.Event{
				Timestamp: time.Now(),
				Kind:      trace.KindTaskCancelled, TaskID: task.ID, RunID: string(task.RunID), GraphID: graphID,
				NodeID: task.NodeID, ActivationID: task.ActivationID, Reason: reason,
				Transition: &trace.Transition{
					PrevStatus: string(from), NewStatus: string(model.TaskStatusCancelled),
					Cause: "graph_terminal_cleanup", CancelSource: "graph_terminal",
				},
			}
			// Keep trace.Event as the single Reactor input authority. graphFeed
			// recognizes graph_terminal below and skips the already-settled Graph;
			// every other lifecycle subscriber still receives this cancellation.
			trace.Emit(event)
			if writer := trace.Default(); writer != nil {
				writer.CloseTask(task.ID)
			}
		}
	}
	return errors.Join(errs...)
}

// cancelLiveGraphTask closes a pending/processing Task despite a concurrent
// ClaimTask or RetryRollback between ScanAll/GetTask and the state CAS. Every
// failed CAS is followed by an authoritative re-read: terminal means another
// actor already settled it, a changed live state is retried, and an unchanged
// state returns the real Store error instead of spinning on a persistent
// failure.
func cancelLiveGraphTask(tasks store.TaskStore, taskID string) (from model.TaskStatus, changed bool, err error) {
	for {
		current, err := tasks.GetTask(taskID)
		if err != nil {
			return "", false, err
		}
		if current == nil {
			return "", false, store.ErrTaskNotFound
		}
		if model.IsTerminal(current.Status) {
			return current.Status, false, nil
		}
		from = current.Status
		if from != model.TaskStatusPending && from != model.TaskStatusProcessing {
			return from, false, fmt.Errorf("unexpected live status %s", from)
		}
		transitionErr := store.TransitionStateWithCancelSource(tasks, taskID, from, model.TaskStatusCancelled, "graph_terminal")
		if transitionErr == nil {
			return from, true, nil
		}
		latest, getErr := tasks.GetTask(taskID)
		if getErr != nil {
			return from, false, errors.Join(transitionErr, getErr)
		}
		if latest != nil && model.IsTerminal(latest.Status) {
			return latest.Status, false, nil
		}
		if latest == nil || latest.Status == from {
			return from, false, transitionErr
		}
		// pending <-> processing raced the CAS; retry from the new authority.
	}
}

// graphEndWakeReactor 把顶层图终态转成一条独立的 Scheduler 唤醒任务。
// graph_ended 本身只是 trace 事实，不会进入 Scheduler 的 EventCh；若没有
// 这座桥，图可以在后台正确收官，但用户永远收不到明确结果。
//
// 本 Reactor 刻意同步：Graph Runtime 已 durable 写入终态后才 Emit，当前
// 调用只做内存 Store 查询/发布；同步完成可消除「图已终态、唤醒尚未入板」
// 的进程内竞态。启动恢复仍会用 reconcileTerminalGraphWakes 补崩溃窗口。
type graphEndWakeReactor struct {
	tasks  store.TaskStore
	graphs *graph.Store
	mu     sync.Mutex
}

const graphEndEventSource = "graph-ended"

func newGraphEndWakeReactor(tasks store.TaskStore, graphs *graph.Store) *graphEndWakeReactor {
	return &graphEndWakeReactor{tasks: tasks, graphs: graphs}
}

func (r *graphEndWakeReactor) Name() string { return "graph-ended-scheduler-wake" }

func (r *graphEndWakeReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{trace.KindGraphEnded}
}

func (r *graphEndWakeReactor) IsSync() bool { return true }

func (r *graphEndWakeReactor) Priority() int { return 200 }

func (r *graphEndWakeReactor) Run(ev trace.Event) error {
	if strings.TrimSpace(ev.GraphID) == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wakeLocked(ev.GraphID, ev.Reason)
}

func (r *graphEndWakeReactor) wakeLocked(graphID, reason string) error {
	if r.tasks == nil || r.graphs == nil {
		return nil
	}
	doc, ok := r.graphs.Get(graphID)
	if !ok || !doc.Status.IsTerminal() {
		return nil
	}
	if graphIsMaterializedChild(r.graphs, graphID) {
		return nil // 子图终态由 Runtime 回填父节点；只在顶层图收官时回复用户
	}
	marker := graphEndWakeMarker(doc)
	tasks, err := r.tasks.ScanAll()
	if err != nil {
		return fmt.Errorf("检查图终态唤醒任务失败: %w", err)
	}
	for _, task := range tasks {
		if task != nil && task.EventSource == graphEndEventSource && strings.Contains(task.Description, marker) {
			return nil
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("序列化图 %s 终态快照失败: %w", graphID, err)
	}
	detail := truncateRunes(string(raw), 6000)
	description := fmt.Sprintf(
		"%s\n顶层图 %s 已到终态 %s（revision=%d state_version=%d）。\n原因：%s\n终态快照：%s\n处理指引：这是完成通知，不要重新执行图内工作。先用 read_graph 核对当前权威状态；然后基于节点结果向用户给出明确、耐久的最终回复。图失败时说明失败点与可恢复条件，图成功时说明实际完成结果。",
		marker, doc.GraphID, doc.Status, doc.Revision, doc.StateVersion, graphEndReason(doc.Status, reason), detail)
	wake := &model.Task{
		Description:      description,
		EventType:        "__scheduler__",
		EventSource:      graphEndEventSource,
		ExpectedDuration: 24 * time.Hour,
		MaxConcurrency:   1,
	}
	parent := &model.Task{RunID: doc.RunID, RunContract: doc.RunContract}
	if err := taskcontract.Inherit(parent, wake, loopcontract.WorkCoordination); err != nil {
		return fmt.Errorf("图 %s 终态唤醒继承 RunContract: %w", graphID, err)
	}
	if wake.RunContract != nil {
		wake.RunPhase = runcontract.PhaseFinalization
	}
	if err := r.tasks.PublishTask(wake); err != nil {
		return fmt.Errorf("发布图 %s 终态 Scheduler 唤醒任务失败: %w", graphID, err)
	}
	return nil
}

func graphEndWakeMarker(doc *graph.GraphDocument) string {
	return fmt.Sprintf("[graph-ended: %s/%d]", doc.GraphID, doc.StateVersion)
}

func graphEndReason(status graph.GraphStatus, reason string) string {
	if strings.TrimSpace(reason) == "" {
		if status != graph.GraphCompleted {
			return "恢复快照未保留具体原因；请从失败节点与 trace 核对"
		}
		return "无（正常收官）"
	}
	return reason
}

// graphIsMaterializedChild 不依赖 graph_id 的斜杠形状判断子图；用户图 ID
// 本身可能含合法分段，权威事实是某个 subgraph activation 的 ChildGraphID。
func graphIsMaterializedChild(graphs *graph.Store, graphID string) bool {
	for _, summary := range graphs.List() {
		if summary.GraphID == graphID {
			continue
		}
		doc, ok := graphs.Get(summary.GraphID)
		if !ok {
			continue
		}
		for _, node := range doc.Nodes {
			if node.Execution != nil && node.Execution.ChildGraphID == graphID {
				return true
			}
		}
	}
	return false
}

// reconcileTerminalGraphWakes 覆盖「图终态已 durable、唤醒任务尚未来得及
// 进入 Session 快照就崩溃」的窗口。公告板恢复完成后调用，marker 查重使
// 已处理/在途的终态通知不会重复发布。
// owned 非空时只为集合内的图补发（会话模式：不给旧会话的图唤醒新会话的
// Scheduler——2026-08 起启动永远是全新会话，旧图已全量停驻）；nil = 全量
// （无 Session 模式，行为同今）。
func reconcileTerminalGraphWakes(graphs *graph.Store, tasks store.TaskStore, owned map[string]struct{}) {
	waker := newGraphEndWakeReactor(tasks, graphs)
	for _, summary := range graphs.List() {
		if !summary.Status.IsTerminal() {
			continue
		}
		if owned != nil {
			if _, ok := owned[summary.GraphID]; !ok {
				continue // 非当前 session 的图：不唤醒新会话的 Scheduler
			}
		}
		if err := waker.wakeLocked(summary.GraphID, ""); err != nil {
			log.Printf("[启动] WARNING: 补发图 %s 终态通知失败: %v", summary.GraphID, err)
		}
	}
}

// terminalGraphClosure returns terminal Graphs plus every materialized
// descendant referenced by their durable node executions. Descendants remain
// in the closure even if a repair write later fails: a terminal ancestor is
// enough authority to forbid their Tasks from resuming.
func terminalGraphClosure(graphs *graph.Store) map[string]struct{} {
	closure := make(map[string]struct{})
	if graphs == nil {
		return closure
	}
	queue := make([]string, 0)
	for _, summary := range graphs.List() {
		if summary.Status.IsTerminal() {
			closure[summary.GraphID] = struct{}{}
			queue = append(queue, summary.GraphID)
		}
	}
	for len(queue) > 0 {
		graphID := queue[0]
		queue = queue[1:]
		doc, ok := graphs.Get(graphID)
		if !ok {
			continue
		}
		for _, node := range doc.Nodes {
			if node.Execution == nil {
				continue
			}
			childID := strings.TrimSpace(node.Execution.ChildGraphID)
			if childID == "" {
				continue
			}
			if _, seen := closure[childID]; seen {
				continue
			}
			closure[childID] = struct{}{}
			queue = append(queue, childID)
		}
	}
	return closure
}

// reconcileTerminalGraphTasks closes the crash window between the durable
// terminal Graph status and the main-flow TaskBoard cleanup that precedes
// graph_ended. It also closes Tasks in live descendants of a terminal ancestor,
// even if repairing that descendant's Graph journal failed. Session restore
// intentionally requeues old processing Tasks as pending, so this must run
// before Runners start claiming work.
func reconcileTerminalGraphTasks(graphs *graph.Store, tasks store.TaskStore) {
	if graphs == nil || tasks == nil {
		return
	}
	board := newGraphBoard(tasks)
	graphIDs := terminalGraphClosure(graphs)
	ordered := make([]string, 0, len(graphIDs))
	for graphID := range graphIDs {
		ordered = append(ordered, graphID)
	}
	sort.Strings(ordered)
	for _, graphID := range ordered {
		err := board.TerminateGraphTasks(graphID)
		if err != nil {
			log.Printf("[启动] WARNING: 清理终态图树 %s 的遗留任务失败: %v", graphID, err)
		}
	}
}

// reconcileTerminalGraphTrees repairs snapshots produced before recursive
// subgraph teardown existed (or a crash between ancestor settlement steps): a
// terminal ancestor is authoritative, so no materialized descendant may remain
// runnable. CancelGraphTree keeps the ancestor's outcome, durably cancels live
// descendant nodes, and emits graph_ended for each child that newly terminates.
func reconcileTerminalGraphTrees(graphs *graph.Store, runtime *graph.Runtime) {
	if graphs == nil || runtime == nil {
		return
	}
	for _, summary := range graphs.List() {
		if !summary.Status.IsTerminal() {
			continue
		}
		reason := fmt.Sprintf("startup recovery: ancestor Graph %s is already terminal (%s)", summary.GraphID, summary.Status)
		if err := runtime.CancelGraphTree(summary.GraphID, reason); err != nil {
			log.Printf("[启动] WARNING: 清理终态图 %s 的遗留子图失败: %v", summary.GraphID, err)
		}
	}
}

func newGraphFeedReactor(s store.TaskStore, sink graphTerminalSink, authority ...*graphTaskOutcomeAuthority) *graphFeedReactor {
	var outcomeAuthority *graphTaskOutcomeAuthority
	if len(authority) > 0 {
		outcomeAuthority = authority[0]
	}
	return &graphFeedReactor{store: s, sink: sink, outcomeAuthority: outcomeAuthority}
}

func (r *graphFeedReactor) Name() string { return "graph-terminal-feed" }

func (r *graphFeedReactor) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindTaskCompleted,
		trace.KindTaskFailed,
		trace.KindTaskBlocked,
		trace.KindTaskCancelled,
	}
}

func (r *graphFeedReactor) IsSync() bool { return false }

// ReliableAsync keeps durable Graph progression off the task-finalization
// goroutine without exposing it to Registry's lossy observational backpressure.
func (r *graphFeedReactor) ReliableAsync() bool { return true }

// Priority 取 100 与 task-end-callback 同档：同为任务终态事实的可靠
// 消费/转发者（950 档是 anomaly 等纯观测器）。
func (r *graphFeedReactor) Priority() int { return 100 }

func (r *graphFeedReactor) Run(ev trace.Event) error {
	if ev.TaskID == "" {
		return nil
	}
	task, err := r.store.GetTask(ev.TaskID)
	if err != nil || task == nil {
		if r.outcomeAuthority == nil {
			return nil
		}
		fact, found, outcomeErr := r.outcomeAuthority.FactByTaskID(context.Background(), ev.TaskID)
		if outcomeErr != nil {
			return outcomeErr
		}
		if !found {
			_, ackErr := r.outcomeAuthority.AckNonGraphTask(ev.TaskID)
			return ackErr
		}
		return r.deliverTypedOutcome(nil, fact)
	}
	if task.GraphID == "" {
		if r.outcomeAuthority != nil {
			_, err := r.outcomeAuthority.AckNonGraphTask(task.ID)
			return err
		}
		return nil // legacy 非图任务：不回填
	}
	if ev.Kind == trace.KindTaskCancelled && ev.Transition != nil &&
		ev.Transition.CancelSource == "graph_terminal" {
		if r.outcomeAuthority != nil {
			_, err := r.outcomeAuthority.AckTask(task.ID)
			return err
		}
		return nil // legacy：Graph 已终态，不再回填 Runtime。
	}
	status, ok := graphTerminalStatusOf(task.Status)
	if !ok {
		log.Printf("[graph] DEBUG 任务 %s 的终态事件 %s 与当前状态 %q 不符，忽略",
			ev.TaskID, ev.Kind, task.Status)
		return nil
	}
	if r.outcomeAuthority != nil {
		fact, typed, outcomeErr := r.outcomeAuthority.FactForTask(context.Background(), task)
		if outcomeErr != nil {
			failure := graph.TerminalFact{
				GraphID: task.GraphID, NodeID: task.NodeID, ActivationID: task.ActivationID,
				TaskID: task.ID, Status: graph.NodeFailed,
				Result: map[string]any{"status": "failed", "reason_code": "task_outcome_authority_failure", "reason": outcomeErr.Error()},
			}
			return r.failTerminalWriteback(task, failure, outcomeErr, "")
		}
		if typed {
			return r.deliverTypedOutcome(task, fact)
		}
	}
	fact := graph.TerminalFact{
		GraphID:      task.GraphID,
		NodeID:       task.NodeID,
		ActivationID: task.ActivationID,
		TaskID:       task.ID,
		Status:       status,
		Result:       graphTaskResult(task),
		Evidence:     assembleTaskEvidence(r.store, task),
	}
	return r.deliverLegacy(task, fact)
}

func (r *graphFeedReactor) deliverTypedOutcome(task *model.Task, fact graph.TerminalFact) error {
	outcomeRef, _ := fact.Result["_task_outcome_ref"].(string)
	if err := r.sink.OnTaskTerminal(fact); err != nil {
		return r.failTerminalWriteback(task, fact, err, outcomeRef)
	}
	return r.outcomeAuthority.AckRef(outcomeRef)
}

func (r *graphFeedReactor) deliverLegacy(task *model.Task, fact graph.TerminalFact) error {
	if err := r.sink.OnTaskTerminal(fact); err != nil {
		return r.failTerminalWriteback(task, fact, err, "")
	}
	return nil
}

func (r *graphFeedReactor) failTerminalWriteback(task *model.Task, fact graph.TerminalFact, cause error, outcomeRef string) error {
	if fbErr := r.sink.FailTerminalWriteback(fact, cause); fbErr != nil {
		if task == nil {
			return errors.Join(cause, fbErr)
		}
		// 终态事实不许无处置（SWE-002 第三层防线）：回落把节点显式置 failed
		// 并经 GraphChangeWakeSpec 发布幂等 writeback-failed 唤醒任务，交
		// Scheduler 裁决。刻意不做盲重试——确定性数据错误（如证据越界拒写）
		// 重试必然复发；瞬时 IO 失败经「节点 failed + 转移求值/Scheduler
		// 重激活」是更诚实的恢复路径。
		log.Printf("[graph] ERROR 任务 %s（图 %s 节点 %s activation %s）终态回填失败: %v；回落处置返回错误: %v",
			task.ID, task.GraphID, task.NodeID, task.ActivationID, cause, fbErr)
		return errors.Join(cause, fbErr)
	}
	if task != nil {
		log.Printf("[graph] WARN 任务 %s（图 %s 节点 %s activation %s）终态回填失败: %v；已交回落处置",
			task.ID, task.GraphID, task.NodeID, task.ActivationID, cause)
	}
	if outcomeRef != "" {
		return r.outcomeAuthority.AckRef(outcomeRef)
	}
	return nil
}

// ============================================================
// 终态证据组装（数据流 EvidenceEntry）
// ============================================================

// evidenceMaxEntries 是单次终态随 TerminalFact 携带的证据条目上限；
// 超限时追加一条截断标记条目，不让超大调用历史无界进入图 journal。
const evidenceMaxEntries = 64

// assembleTaskEvidence 把终态任务的可观察事实组装为数据流证据条目：
// ToolCallRecord 调用历史在前，声明的 artifact 引用在后。Ref 是由 TaskID +
// CallID/调用内容或 artifact 内容身份生成的稳定引用，绝不使用查询序数；查询
// 重排、恢复重放不会让验收契约漂移。AgentGo 只对自身可观察的调用身份/种类/
// 退出码负责，不记录工具输出正文（正文经 Result 与 artifact 文件传递）。
func assembleTaskEvidence(s store.TaskStore, task *model.Task) []graph.EvidenceEntry {
	if s == nil || task == nil {
		return nil
	}
	calls, err := s.QueryToolCalls(task.ID, "")
	if err != nil {
		log.Printf("[graph] WARN 读取任务 %s Evidence 调用账失败: %v", task.ID, err)
		return nil
	}
	return assembleTaskEvidenceFromCalls(task, calls)
}

func assembleTaskEvidenceFromCalls(task *model.Task, calls []store.ToolCallRecord) []graph.EvidenceEntry {
	if task == nil {
		return nil
	}
	var out []graph.EvidenceEntry
	seen := make(map[string]struct{})
	truncatedTotal := 0
	for _, call := range calls {
		ref := evidenceCallRef(task.ID, call)
		if _, duplicate := seen[ref]; duplicate {
			continue // 完全相同的 durable 调用事实是同一份内容寻址证据。
		}
		seen[ref] = struct{}{}
		if len(out) >= evidenceMaxEntries {
			truncatedTotal++
			continue
		}
		out = append(out, evidenceCallEntry(ref, call))
	}
	for _, artifact := range task.Artifacts {
		if strings.TrimSpace(artifact) == "" {
			continue
		}
		ref := evidenceArtifactRef(task, artifact)
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		if len(out) >= evidenceMaxEntries {
			truncatedTotal++
			continue
		}
		path, pathTruncated := boundedEvidenceValue(artifact, graph.EvidencePathMaxRunes)
		out = append(out, graph.EvidenceEntry{
			Ref: ref, Kind: "artifact", Summary: evidenceArtifactSummary(task, artifact),
			Path: path, PathTruncated: pathTruncated,
		})
	}
	if truncatedTotal > 0 {
		out = append(out, graph.EvidenceEntry{
			Ref:     stableEvidenceRef(task.ID, "truncated", fmt.Sprintf("%d", truncatedTotal)),
			Kind:    "truncated",
			Summary: fmt.Sprintf("其余 %d 条证据从略（超过单任务证据上限 %d）", truncatedTotal, evidenceMaxEntries),
		})
	}
	return out
}

func evidenceCallEntry(ref string, call store.ToolCallRecord) graph.EvidenceEntry {
	success := call.Success
	entry := graph.EvidenceEntry{
		Ref: ref, Kind: evidenceKindOf(call.ToolName), Summary: evidenceCallSummary(call),
		CallID: call.CallID, ToolName: evidenceToolNameOf(call.ToolName), Success: &success,
	}
	arg := func(key string) string {
		value, _ := call.Args[key].(string)
		return value
	}
	switch call.ToolName {
	case "run_shell":
		entry.Command, entry.CommandTruncated = boundedEvidenceValue(arg("command"), graph.EvidenceCommandMaxRunes)
		if call.ExitCode != nil {
			exit := *call.ExitCode
			entry.ExitCode = &exit
		}
	case "write_file", "edit_file", "read_file":
		entry.Path, entry.PathTruncated = boundedEvidenceValue(arg("path"), graph.EvidencePathMaxRunes)
	}
	return entry
}

// boundedEvidenceValue 保留字段总量硬边界，超限时最后一个 rune 用省略号替换，
// 同时由调用方写入显式 truncated 标志。空白仅裁首尾，不改变中间命令/路径。
func boundedEvidenceValue(value string, maxRunes int) (string, bool) {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes), false
	}
	if maxRunes <= 0 {
		return "", true
	}
	if maxRunes == 1 {
		return "…", true
	}
	return string(runes[:maxRunes-1]) + "…", true
}

// evidenceCallRef 把协议 CallID 与调用的 durable 内容一起纳入身份。部分兼容
// provider 会复用 CallID，因此不能只取 CallID；旧快照 CallID 为空时内容哈希
// 仍然稳定。Timestamp 不参与身份，避免导入/精度归一导致引用漂移。
func evidenceCallRef(taskID string, call store.ToolCallRecord) string {
	payload := struct {
		CallID   string         `json:"call_id,omitempty"`
		AgentID  string         `json:"agent_id,omitempty"`
		ToolName string         `json:"tool_name"`
		Args     map[string]any `json:"args,omitempty"`
		Success  bool           `json:"success"`
		ExitCode *int           `json:"exit_code,omitempty"`
	}{
		CallID: call.CallID, AgentID: call.AgentID, ToolName: call.ToolName,
		Args: call.Args, Success: call.Success, ExitCode: call.ExitCode,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Args 正常来自 JSON 工具参数，不应到这里；即使某个测试桩塞入不可编码
		// 值，也用不含 Args 的稳定审计字段退化，绝不退回查询序数。
		encoded = []byte(strings.Join([]string{
			call.CallID, call.AgentID, call.ToolName,
			fmt.Sprintf("success=%v", call.Success), evidenceCallSummary(call),
		}, "\x00"))
	}
	return stableEvidenceRef(taskID, "call", string(encoded))
}

func evidenceArtifactRef(task *model.Task, artifact string) string {
	// Artifacts 已由写入边界归一化；这里使用 durable 原串，避免跨平台恢复时
	// filepath.Clean 把分隔符改写后造成既有 EvidenceRef 漂移。
	identity := artifact
	if meta, ok := task.ArtifactMeta[artifact]; ok {
		identity += fmt.Sprintf("\x00%s\x00%d", meta.SHA256, meta.Bytes)
	}
	return stableEvidenceRef(task.ID, "artifact", identity)
}

func evidenceArtifactSummary(task *model.Task, artifact string) string {
	summary := "产物文件: " + artifact
	if meta, ok := task.ArtifactMeta[artifact]; ok {
		if meta.SHA256 != "" {
			summary += fmt.Sprintf("（sha256=%s bytes=%d）", meta.SHA256, meta.Bytes)
		}
	}
	bounded, _ := boundedEvidenceValue(summary, graph.EvidenceSummaryMaxRunes)
	return bounded
}

func stableEvidenceRef(taskID, kind, identity string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + identity))
	return fmt.Sprintf("ev:%s:%s:%x", taskID, kind, sum[:16])
}

// evidenceKindMaxRunes 是证据 kind 保留自定义工具名的长度上限（与落库清洗
// 的合法名上限同口径；store 侧 kind 硬上限是 MaxIDLength=128）。
const evidenceKindMaxRunes = 64

// evidenceKindOf 把工具名归并为证据种类（shell/file_write/file_edit/read/web/
// artifact 之外保留原工具名，便于下游按种类粗筛）。
//
// fallthrough 不再原样透传（SWE-002 第一层防线）：字符形状非法（空、非字母
// 开头、含 [a-zA-Z0-9_.:-] 之外字符——如模型 DSML 泄漏产生的畸形「工具名」）
// 一律归一为 "unknown"；形状合法但超 64 rune 按 boundedEvidenceValue 截断。
// kind 受 store validateEvidenceEntryBounds 的 MaxIDLength=128 约束，原样透传
// 会让整条 activation result 被拒写、终态事实丢失。
func evidenceKindOf(toolName string) string {
	switch toolName {
	case "run_shell":
		return "shell"
	case "write_file":
		return "file_write"
	case "edit_file":
		return "file_edit"
	case "read_file":
		return "read"
	case "web_search", "web_fetch":
		return "web"
	}
	if !store.IsToolNameCharsetLegal(toolName) {
		return "unknown"
	}
	bounded, _ := boundedEvidenceValue(toolName, evidenceKindMaxRunes)
	return bounded
}

// evidenceToolNameOf 归一证据条目的 tool_name 字段（SWE-002 第一层防线）：
// 不合法（空、超 64 rune、含字符集外字符）替换为确定性占位
// malformed:<sha256(raw)前12hex>——同一垃圾名永远同一占位，不同垃圾名可区分；
// 合法但超 EvidenceIdentityMaxRunes 时按 boundedEvidenceValue 截断（当前合法
// 名 ≤ 64 rune，此分支是防御兜底）。原始垃圾名不进证据层——trace 的工具调用
// 事件仍保留原始 ToolCall 可对账。
func evidenceToolNameOf(raw string) string {
	if !store.IsWellFormedToolName(raw) {
		return store.MalformedToolNamePlaceholder(raw)
	}
	bounded, _ := boundedEvidenceValue(raw, graph.EvidenceIdentityMaxRunes)
	return bounded
}

// evidenceCallSummary 生成单条工具调用的有界摘要：shell 含命令与退出码，
// 文件类含路径，其余为工具名 + 成功标志。
func evidenceCallSummary(call store.ToolCallRecord) string {
	arg := func(key string) string {
		v, _ := call.Args[key].(string)
		return v
	}
	var s string
	switch call.ToolName {
	case "run_shell":
		exit := "?"
		if call.ExitCode != nil {
			exit = fmt.Sprintf("%d", *call.ExitCode)
		}
		s = fmt.Sprintf("命令: %s（exit=%s）", arg("command"), exit)
	case "write_file", "edit_file", "read_file":
		s = fmt.Sprintf("路径: %s", arg("path"))
	default:
		s = fmt.Sprintf("%s success=%v", call.ToolName, call.Success)
	}
	bounded, _ := boundedEvidenceValue(s, graph.EvidenceSummaryMaxRunes)
	return bounded
}

// graphTerminalStatusOf 把任务终态映射为图节点终态。任务 cancelled 映射为
// 节点 failed——TerminalFact 只接受 completed/failed/blocked，取消对图语义
// 等同失败（原状态保留在 Result["status"]，供条件求值区分）。
func graphTerminalStatusOf(s model.TaskStatus) (graph.NodeStatus, bool) {
	switch s {
	case model.TaskStatusCompleted:
		return graph.NodeCompleted, true
	case model.TaskStatusFailed, model.TaskStatusCancelled:
		return graph.NodeFailed, true
	case model.TaskStatusBlocked:
		return graph.NodeBlocked, true
	}
	return "", false
}

// graphTaskResult 组装 TerminalFact.Result：task.Results + 权威任务终态。
// status 键最后写入，以 task.Status 为准。结构化 carrier 只在 completed / blocked
// 两种合法产出终态展开；failed / cancelled 即使来自旧快照或竞争残留，也会
// 剔除 carrier 以及 event/verdict/cited_evidence，避免错误终态沿成功/custom
// path 边放行。completed 的 event 仍可优先驱动事件形态；blocked 仅保留其
// 合法自定义诊断字段并依赖 blocked 终态事件。
func graphTaskResult(task *model.Task) map[string]any {
	result := make(map[string]any, len(task.Results)+1)
	allowStructured := task.Status == model.TaskStatusCompleted || task.Status == model.TaskStatusBlocked
	allowSuccessProtocol := task.Status == model.TaskStatusCompleted
	// submit_task_result 的自定义 result object 以单一内部 carrier 保存，
	// 这里先类型保真地展开；随后再覆盖普通 Results 键，确保 event/verdict/
	// cited_evidence 与 Agent 权威正文永远不能被 carrier 篡改。工具边界已做
	// 严格校验；历史损坏 carrier 只告警并忽略，让 Graph 按缺字段规则失败。
	if raw, ok := task.Results[agent.StructuredResultStorageKey]; ok && allowStructured {
		structured, err := agent.DecodeStructuredResult(raw)
		if err != nil {
			log.Printf("[graph] WARN 任务 %s 的结构化 Result carrier 无法解码，已忽略: %v", task.ID, err)
		} else {
			for k, v := range structured {
				switch k {
				case "status", "event", "verdict", "cited_evidence", agent.StructuredResultStorageKey:
					continue // 即使历史载荷损坏，也不得绕过专用协议字段。
				}
				result[k] = v
			}
		}
	}
	for k, v := range task.Results {
		if k == agent.StructuredResultStorageKey {
			continue
		}
		if !allowSuccessProtocol {
			switch k {
			case "event", "verdict", "cited_evidence":
				continue
			}
		}
		result[k] = v
	}
	result["status"] = string(task.Status)
	return result
}

// ============================================================
// Bootstrap 装配点
// ============================================================

// wireGraphRuntime 装配 V6 Graph 运行桥接：持久化 Store（与 artifacts 同基，
// <project_root>/.agentgo/state/graphs）→ 启动恢复（告警逐条 log，不阻断）
// → OnDegraded 告警挂钩 → Runtime + 公告板桥 → 注册终态回填 Reactor。
// 返回的 Store/Runtime 由 System 持有（Shutdown 时 Close；C5b 图工具复用）。
//
// sessionIDProvider 提供当前活跃 session ID（Session 生命周期隔离）：注入
// Runtime 用于提交盖章；装配末尾把无归属历史图归并给当前 session（仅内存，
// 幂等）。可为 nil（无 Session 模式，归属恒空、行为同今）。
//
// 调用时序约束：必须在 trace.SetDefaultDispatcher(reactorReg) 之前完成
// feed 注册（Bootstrap 主流程在 restoreRuntimeBeforeReactorActivation 挂
// dispatcher），否则挂载窗口内到达的任务终态事件无人回填。
func wireGraphRuntime(cfg *config.Config, taskStore store.TaskStore, reactorReg *reactor.Registry,
	effectJournal *effect.Journal, sessionIDProvider func() string,
	recoveryQuarantine ...map[string]string) (*graph.Store, *graph.Runtime, error) {
	policies, err := policycatalog.NewDefault()
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Graph PolicyCatalog 失败: %w", err)
	}
	return wireGraphRuntimeWithPolicies(cfg, taskStore, reactorReg, effectJournal, policies,
		sessionIDProvider, recoveryQuarantine...)
}

func wireGraphRuntimeWithPolicies(cfg *config.Config, taskStore store.TaskStore, reactorReg *reactor.Registry,
	effectJournal *effect.Journal, policies *policycatalog.Catalog, sessionIDProvider func() string,
	recoveryQuarantine ...map[string]string) (*graph.Store, *graph.Runtime, error) {
	return wireGraphRuntimeWithOutcome(cfg, taskStore, reactorReg, effectJournal, policies,
		sessionIDProvider, nil, nil, recoveryQuarantine...)
}

func wireGraphRuntimeWithOutcome(cfg *config.Config, taskStore store.TaskStore, reactorReg *reactor.Registry,
	effectJournal *effect.Journal, policies *policycatalog.Catalog, sessionIDProvider func() string,
	outcomes *outcomestore.Store, checkpoints taskCheckpointReader,
	recoveryQuarantine ...map[string]string) (*graph.Store, *graph.Runtime, error) {
	dir := filepath.Join(cfg.ProjectRoot, ".agentgo", "state", "graphs")
	gs, err := graph.NewStore(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 Graph 持久化 Store 失败: %w", err)
	}
	if err := gs.Recover(); err != nil {
		// 恢复告警逐条 log，不阻断启动（其它图可能已成功恢复读写）。
		for _, w := range flattenJoinedErrors(err) {
			log.Printf("[启动] WARNING: graph 恢复告警: %v", w)
		}
	}
	gs.OnDegraded = func(graphID string, derr error) {
		log.Printf("[graph] ERROR: 图 %s 进入 persistence-degraded，变更 fail-closed: %v", graphID, derr)
		trace.Emit(trace.Event{
			Kind:    trace.KindError,
			GraphID: graphID,
			Error:   fmt.Sprintf("图持久化降级（变更 fail-closed）: %v", derr),
		})
	}
	board := newGraphBoardWithPolicies(taskStore, effectJournal, policies, recoveryQuarantine...)
	var outcomeAuthority *graphTaskOutcomeAuthority
	if outcomes != nil {
		outcomeAuthority = newGraphTaskOutcomeAuthority(gs, outcomes, checkpoints)
		board.outcomeAuthority = outcomeAuthority
		if err := store.SetTerminalOutcomeCoordinator(taskStore, outcomeAuthority); err != nil {
			_ = gs.Close()
			return nil, nil, fmt.Errorf("装配 TaskOutcome terminal hook: %w", err)
		}
	}
	rt := graph.NewRuntime(gs, board)
	rt.SetSessionIDProvider(sessionIDProvider)
	// 上游工作记录（2026-08-21 上游摘要）：转移结算时按来源 Task ID 聚合
	// ToolCallRecord 并随 EdgeInput 冻结；下游任务发布只读冻结文本。
	rt.SetWorkLogProvider(newGraphWorkLogProvider(taskStore))
	// 上面的 gs.Recover 只负责把历史图从磁盘读回内存。2026-08 起启动永远是
	// 全新 Session 且进入会话不自动续跑：会话模式下全部历史图（含无归属
	// 图，以及 --resume 会话自己的图）一次性停驻——吞终态事件、停 wait
	// timer、不被 resumeNonTerminalGraphs 恢复。旧图此后没有恢复入口
	// （僵尸停驻，随其会话归档退出），续走由用户提交新提示词重新规划。
	// 无 Session 模式（provider 取空串）时两函数本身空操作：全部图无归属、
	// 正常驱动，行为同今。
	currentSID := ""
	if sessionIDProvider != nil {
		currentSID = sessionIDProvider()
	}
	if suspended := rt.SuspendGraphsExceptSession(currentSID); len(suspended) > 0 {
		log.Printf("[启动] 已停驻 %d 张不属于当前 session 的历史图", len(suspended))
	}
	if currentSID != "" {
		if suspended := rt.SuspendGraphsForSession(currentSID); len(suspended) > 0 {
			log.Printf("[启动] 已停驻 %d 张当前 session 的历史图（进入会话不再自动续跑）", len(suspended))
		}
	}
	if err := reactorReg.Register(newGraphFeedReactor(taskStore, rt, outcomeAuthority)); err != nil {
		_ = gs.Close()
		return nil, nil, fmt.Errorf("注册 graph-terminal-feed Reactor 失败: %w", err)
	}
	if outcomes != nil {
		interventionStore, ok := checkpoints.(loopInterventionStore)
		if !ok {
			_ = gs.Close()
			return nil, nil, fmt.Errorf("装配 LoopIntervention bridge 失败: checkpoint authority %T 不支持 intervention outbox", checkpoints)
		}
		if _, err := wireLoopInterventionBridge(taskStore, interventionStore, outcomes, reactorReg); err != nil {
			_ = gs.Close()
			return nil, nil, fmt.Errorf("装配 LoopIntervention bridge 失败: %w", err)
		}
	}
	if err := reactorReg.Register(newGraphEndWakeReactor(taskStore, gs)); err != nil {
		_ = gs.Close()
		return nil, nil, fmt.Errorf("注册 graph-ended-scheduler-wake Reactor 失败: %w", err)
	}
	log.Printf("[启动] Graph Runtime 桥接完成（state=%s，已恢复图 %d 张）", dir, len(gs.List()))
	return gs, rt, nil
}

// resumeNonTerminalGraphs 对全部非终态图逐个 ResumeGraph（进程重启后恢复
// 执行）。单图失败只记 WARNING，不阻断其它图与系统启动。
//
// 调用时序约束（硬）：必须放在 restoreRuntimeBeforeReactorActivation 之后——
//  1. graphBoard 的幂等补发靠公告板中已恢复的旧任务去重，Session 快照
//     导入前 Resume 会把「崩溃前已发布」的任务误判缺失而重复发布；
//  2. 此时 dispatcher 已挂载，Resume 发出的 graph_* 事件属真实工作状态
//     变化（非恢复诊断），应正常扇出给观测面。
func resumeNonTerminalGraphs(sys *System) {
	if sys == nil || sys.GraphStore == nil || sys.GraphRuntime == nil {
		return
	}
	// A terminal status may have reached durable storage immediately before the
	// process died, while graph_ended cleanup never ran. Repair old/incomplete
	// descendant trees first, then close every Task owned by
	// the now-terminal tree. This all happens before any non-terminal Graph can
	// resume or any Runner can claim restored work.
	terminalClosure := terminalGraphClosure(sys.GraphStore)
	reconcileTerminalGraphTrees(sys.GraphStore, sys.GraphRuntime)
	reconcileTerminalGraphTasks(sys.GraphStore, sys.Store)

	// session 归属过滤（Session 生命周期隔离）：2026-08 起会话模式下启动
	// 不再恢复任何非终态图——历史图已在 wireGraphRuntime 全量停驻（进入
	// 会话不自动续跑），owned 集合仅为 reconcileTerminalGraphWakes 的
	// 过滤保留。仅无 Session 模式（owned==nil）保持「同今」的全量恢复。
	var owned map[string]struct{}
	if sid := currentSessionID(sys); sid != "" {
		owned = make(map[string]struct{})
		for _, id := range sys.GraphRuntime.GraphsForSession(sid) {
			owned[id] = struct{}{}
		}
	}

	if owned == nil {
		resumed := 0
		for _, sum := range sys.GraphStore.List() {
			if _, forbidden := terminalClosure[sum.GraphID]; forbidden {
				continue
			}
			if sum.Status.IsTerminal() {
				continue
			}
			if err := sys.GraphRuntime.ResumeGraph(sum.GraphID); err != nil {
				log.Printf("[启动] WARNING: 恢复图 %s 的执行失败: %v", sum.GraphID, err)
				continue
			}
			resumed++
		}
		if resumed > 0 {
			log.Printf("[启动] 已恢复 %d 张非终态图的执行", resumed)
		}
	}
	reconcileTerminalGraphWakes(sys.GraphStore, sys.Store, owned)
}

// flattenJoinedErrors 把 errors.Join 的多错误展开为扁平切片（单错误原样返回）。
func flattenJoinedErrors(err error) []error {
	if err == nil {
		return nil
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		return u.Unwrap()
	}
	return []error{err}
}
