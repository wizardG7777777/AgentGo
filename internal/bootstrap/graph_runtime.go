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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"agentgo/internal/config"
	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/store"
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
	store store.TaskStore
	// recoveryQuarantine 是 Effect Journal 启动裁决仍为 unknown 的旧
	// task_id → 原因。Graph journal 可能保留 execution.task_id，但 Session
	// Task 快照已丢失；此时不得把 lookup miss 当作安全补发。
	recoveryQuarantine map[string]string
	// effectJournal 是 TaskStore miss 时的最后一道恢复闸。只要候选 TaskID
	// 在 durable Effect Journal 中有任何历史（包括 settled），就不能把整
	// 个任务当作“从未执行”重发；settled 只证明某个副作用发生，不证明任务完成。
	effectJournal *effect.Journal

	mu sync.Mutex
	// byActivation 是 (graphID \x00 activationID) → taskID 的进程内索引，
	// 仅作快路径；miss 时仍需扫公告板（覆盖重启后索引为空的恢复路径）。
	byActivation map[string]string
}

func newGraphBoard(s store.TaskStore, quarantine ...map[string]string) *graphBoard {
	return newGraphBoardWithEffects(s, nil, quarantine...)
}

func newGraphBoardWithEffects(s store.TaskStore, journal *effect.Journal, quarantine ...map[string]string) *graphBoard {
	var q map[string]string
	if len(quarantine) > 0 {
		q = quarantine[0]
	}
	return &graphBoard{store: s, recoveryQuarantine: q, effectJournal: journal, byActivation: make(map[string]string)}
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
	key := graphActivationKey(spec.GraphID, spec.ActivationID)
	reservedID := graphTaskID(spec.GraphID, spec.ActivationID)
	b.mu.Lock()
	indexedID := b.byActivation[key]
	b.mu.Unlock()
	if indexedID != "" {
		if task, err := b.store.GetTask(indexedID); err == nil && task != nil &&
			task.GraphID == spec.GraphID && task.ActivationID == spec.ActivationID {
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
		ID:           reservedID,
		Description:  graphTaskDescription(spec),
		EventType:    spec.Route,
		GraphID:      spec.GraphID,
		NodeID:       spec.NodeID,
		ActivationID: spec.ActivationID,
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
		snapshot := graph.GraphTaskSnapshot{TaskID: task.ID}
		if terminal, ok := graphTerminalStatusOf(task.Status); ok {
			snapshot.TerminalStatus = terminal
			snapshot.Result = graphTaskResult(task)
		}
		return snapshot, true, nil
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
	effects := b.effectJournal.Query(taskID)
	if len(effects) == 0 {
		return "", ""
	}
	first := effects[0]
	reason := fmt.Sprintf("effect_history_recovery_quarantine: Task 事实缺失但账本已有 %d 条 Effect（首条 id=%s kind=%s status=%s policy=%s）；既有副作用可能已发生，禁止自动整任务重放，需核验后 replan",
		len(effects), first.ID, first.Kind, first.Status, first.Policy)
	return taskID, reason
}

// graphTaskDescription 组装图任务描述：节点标题为主，描述非空时换行追加。
func graphTaskDescription(spec graph.TaskSpec) string {
	if spec.Description == "" {
		return spec.Title
	}
	return spec.Title + "\n\n" + spec.Description
}

// ============================================================
// graphFeedReactor —— 任务终态 → Graph Runtime 的回填器
// ============================================================

// graphTerminalSink 是 graphFeedReactor 对 Graph Runtime 的最小依赖
// （*graph.Runtime 满足）；单测用 fake 注入。
type graphTerminalSink interface {
	OnTaskTerminal(f graph.TerminalFact) error
}

// graphFeedReactor 把任务终态事件回填给 Graph Runtime：取终态任务的图身份
// （GraphID 为空 = 非图任务，直接忽略），组装 graph.TerminalFact 调
// OnTaskTerminal 驱动转移求值。Async——回填在 Registry 的 worker goroutine
// 上执行，不阻塞 trace.Emit 调用方；错误经 async reactor 的 error 返回通道
// 仅记日志，绝不中断主流程。
type graphFeedReactor struct {
	store store.TaskStore
	sink  graphTerminalSink
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
		Description:    description,
		EventType:      "__scheduler__",
		EventSource:    graphEndEventSource,
		TimeoutSeconds: 86400,
		MaxConcurrency: 1,
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
func reconcileTerminalGraphWakes(graphs *graph.Store, tasks store.TaskStore) {
	waker := newGraphEndWakeReactor(tasks, graphs)
	for _, summary := range graphs.List() {
		if !summary.Status.IsTerminal() {
			continue
		}
		if err := waker.wakeLocked(summary.GraphID, ""); err != nil {
			log.Printf("[启动] WARNING: 补发图 %s 终态通知失败: %v", summary.GraphID, err)
		}
	}
}

func newGraphFeedReactor(s store.TaskStore, sink graphTerminalSink) *graphFeedReactor {
	return &graphFeedReactor{store: s, sink: sink}
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

// Priority 取 100 与 task-end-callback 同档：同为任务终态事实的消费/转发者
// （950 档是 anomaly 等纯观测器）。Async 的 priority 只决定投递顺序，
// 与其它终态消费者无执行先后依赖。
func (r *graphFeedReactor) Priority() int { return 100 }

func (r *graphFeedReactor) Run(ev trace.Event) error {
	if ev.TaskID == "" {
		return nil
	}
	task, err := r.store.GetTask(ev.TaskID)
	if err != nil || task == nil {
		return nil // 任务不可查（已淘汰或从未入库）：无从回填，静默忽略
	}
	if task.GraphID == "" {
		return nil // 非图任务：不回填
	}
	status, ok := graphTerminalStatusOf(task.Status)
	if !ok {
		log.Printf("[graph] DEBUG 任务 %s 的终态事件 %s 与当前状态 %q 不符，忽略",
			ev.TaskID, ev.Kind, task.Status)
		return nil
	}
	return r.sink.OnTaskTerminal(graph.TerminalFact{
		GraphID:      task.GraphID,
		NodeID:       task.NodeID,
		ActivationID: task.ActivationID,
		TaskID:       task.ID,
		Status:       status,
		Result:       graphTaskResult(task),
	})
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

// graphTaskResult 组装 TerminalFact.Result：task.Results 全量键值 + 权威
// 任务终态。status 键最后写入——以 task.Status 为准，防 Results 同名键
// 覆盖（Results 的键是认领者 ID，理论上可撞名）。
// Results 含 "event" 键时引擎优先采用它做事件形态转移求值（eventNameOf），
// 与 C5b 结构化结果通道的语义自然衔接。
func graphTaskResult(task *model.Task) map[string]any {
	result := make(map[string]any, len(task.Results)+1)
	for k, v := range task.Results {
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
// 调用时序约束：必须在 trace.SetDefaultDispatcher(reactorReg) 之前完成
// feed 注册（Bootstrap 主流程在 restoreRuntimeBeforeReactorActivation 挂
// dispatcher），否则挂载窗口内到达的任务终态事件无人回填。
func wireGraphRuntime(cfg *config.Config, taskStore store.TaskStore, reactorReg *reactor.Registry, effectJournal *effect.Journal, recoveryQuarantine ...map[string]string) (*graph.Store, *graph.Runtime, error) {
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
	rt := graph.NewRuntime(gs, newGraphBoardWithEffects(taskStore, effectJournal, recoveryQuarantine...))
	if err := reactorReg.Register(newGraphFeedReactor(taskStore, rt)); err != nil {
		return nil, nil, fmt.Errorf("注册 graph-terminal-feed Reactor 失败: %w", err)
	}
	if err := reactorReg.Register(newGraphEndWakeReactor(taskStore, gs)); err != nil {
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
	resumed := 0
	for _, sum := range sys.GraphStore.List() {
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
	reconcileTerminalGraphWakes(sys.GraphStore, sys.Store)
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
