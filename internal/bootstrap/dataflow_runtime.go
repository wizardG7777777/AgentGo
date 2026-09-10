package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/contextcontract"
	"agentgo/internal/delivery"
	"agentgo/internal/effect"
	"agentgo/internal/graph"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/outcomestore"
	"agentgo/internal/scheduler"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/terminaladapter"
	"agentgo/internal/tools"
	"agentgo/internal/trace"
	"agentgo/internal/workspace"
)

type agentTaskBoard struct {
	tasks      store.TaskStore
	routes     *scheduler.AgentRegistry
	workspaces *workspace.Manager
	cancel     *store.TaskCancelRegistry
}

func dataflowRoute(ref string) string {
	if ref == "default" {
		return ""
	}
	return ref
}
func (b *agentTaskBoard) CheckAgentTask(ctx context.Context, d graph.AgentTaskDispatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.routes == nil || b.workspaces == nil {
		return fmt.Errorf("路由或 workspace 执行组件未装配")
	}
	if !b.routes.CanRouteForPlan(model.GraphRouteScope(d.GraphID), dataflowRoute(d.Node.Execution.RouteRef), d.Node.Execution.Tools...) {
		return fmt.Errorf("没有满足工具需求的 route_ref=%s", d.Node.Execution.RouteRef)
	}
	return nil
}
func (b *agentTaskBoard) origin(runID string) (*model.Task, error) {
	all, err := b.tasks.ScanAll()
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if string(t.RunID) == runID && t.EventType == "__scheduler__" && t.GraphID == "" && t.InterventionGraphID == "" && t.FinalReportGraphID == "" && t.ParentTaskID == "" {
			return t, nil
		}
	}
	return nil, fmt.Errorf("Run %s 缺少原始用户任务", runID)
}
func (b *agentTaskBoard) PublishAgentTask(ctx context.Context, d graph.AgentTaskDispatch) error {
	if err := b.CheckAgentTask(ctx, d); err != nil {
		return err
	}
	if t, err := b.tasks.GetTask(d.TaskID); err == nil && t != nil {
		if t.GraphID != d.GraphID || t.NodeID != d.Node.NodeID || t.ActivationID != d.ActivationID {
			return fmt.Errorf("派发任务身份冲突")
		}
		return nil
	}
	parent, err := b.origin(d.RunID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(d.Inputs)
	if err != nil {
		return err
	}
	schema, err := json.Marshal(d.Node.ResultSchema)
	if err != nil {
		return err
	}
	task := &model.Task{ID: d.TaskID, Description: d.Node.Title + "\n" + d.Node.Objective + "\n结果必须满足 result_schema=" + string(schema), GraphID: d.GraphID, NodeID: d.Node.NodeID, ActivationID: d.ActivationID, MaxConcurrency: 1, EventType: dataflowRoute(d.Node.Execution.RouteRef), RouteScope: model.GraphRouteScope(d.GraphID), InputCandidateRef: d.Inputs.WorkspaceCandidateRef, Capability: &model.NodeCapability{Tools: d.Node.Execution.Tools, Model: d.Node.Execution.Model, Isolation: &model.IsolationSpec{Mode: model.IsolationModeWorkspace}}}
	if err = taskcontract.Inherit(parent, task, loopcontract.WorkInvestigation); err != nil {
		return err
	}
	task.ContextInputs = []model.TaskContextInput{{Kind: "upstream_result", SourceRef: "graph-inputs:" + d.ActivationID, Content: string(body)}}
	return b.tasks.PublishTask(task)
}
func (b *agentTaskBoard) CancelAgentTask(_ context.Context, taskID, reason string) error {
	if b.cancel != nil {
		b.cancel.Cancel(taskID)
	}
	t, err := b.tasks.GetTask(taskID)
	if err != nil {
		return err
	}
	if model.IsTerminal(t.Status) {
		return nil
	}
	return tools.GuardedCancel(context.Background(), b.tasks, taskID, "graph_cancel")
}

type dataflowBridge struct {
	runtime   *graph.DataflowRuntime
	board     *agentTaskBoard
	outcomes  *outcomestore.Store
	authority *dataflowOutcomeAuthority
	sessionID func() string
	restored  map[string]bool
}

func wireDataflowRuntime(cfg *config.Config, tasks store.TaskStore, outcomes *outcomestore.Store, checkpoints taskCheckpointReader, manager *workspace.Manager, journal *effect.Journal, deliveries *delivery.Store, sessionID func() string) (*graph.DataflowStore, *graph.DataflowRuntime, *dataflowBridge, error) {
	gs, err := graph.NewDataflowStore(filepath.Join(cfg.ProjectRoot, ".agentgo", "state", "graphs-v6"))
	if err != nil {
		return nil, nil, nil, err
	}
	b := &agentTaskBoard{tasks: tasks, workspaces: manager}
	rt := graph.NewDataflowRuntime(gs, b)
	bridge := &dataflowBridge{runtime: rt, board: b, outcomes: outcomes, sessionID: sessionID, restored: map[string]bool{}}
	states, err := gs.List("")
	if err != nil {
		return nil, nil, nil, err
	}
	for _, s := range states {
		bridge.restored[s.Definition.GraphID] = true
	}
	a := &dataflowOutcomeAuthority{graphs: gs, runtime: rt, outcomes: outcomes, checkpoints: checkpoints, freezeCandidate: func(task *model.Task) (string, error) {
		return manager.FreezeAgentTaskCandidate(task.ID, task.GraphID, string(task.RunID), task.InputCandidateRef)
	}}
	bridge.authority = a
	if err = store.SetTerminalOutcomeCoordinator(tasks, a); err != nil {
		return nil, nil, nil, err
	}
	rt.Delivery = &dataflowDelivery{manager: manager, journal: journal, graphs: gs, store: deliveries}
	return gs, rt, bridge, nil
}

func (b *dataflowBridge) Run(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	lastError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := b.pump(ctx); err != nil {
				if err.Error() != lastError {
					log.Printf("[dataflow] 调度/结算需要处理: %v", err)
					trace.Emit(trace.Event{Kind: trace.KindError, Error: err.Error()})
					lastError = err.Error()
				}
			} else {
				lastError = ""
			}
		}
	}
}

func (b *dataflowBridge) pump(ctx context.Context) error {
	pending, pendingErr := b.outcomes.PendingDeliveries()
	if pendingErr != nil {
		return pendingErr
	}
	for _, record := range pending {
		if record.Outcome.GraphID != "" {
			continue
		}
		task, err := b.board.tasks.GetTask(record.Outcome.TaskID)
		if err == nil && task != nil && model.IsTerminal(task.Status) && task.OutcomeRef == record.OutcomeRef {
			if err := b.outcomes.AckDelivery(record.OutcomeRef); err != nil {
				return err
			}
		}
	}
	states, err := b.runtime.Store.List(b.sessionID())
	if err != nil {
		return err
	}
	for _, s := range states {
		id := s.Definition.GraphID
		if b.restored[id] {
			continue
		}
		for nodeID, e := range s.Executions {
			if e.OutcomeRef != "" || e.TaskID == "" {
				continue
			}
			t, err := b.board.tasks.GetTask(e.TaskID)
			if err != nil || t == nil || !model.IsTerminal(t.Status) {
				continue
			}
			if t.OutcomeRef == "" {
				return fmt.Errorf("任务 %s 已终态但没有 OutcomeRef", t.ID)
			}
			record, ok, err := b.outcomes.GetByRef(t.OutcomeRef)
			if err != nil || !ok {
				return fmt.Errorf("任务终态引用不可读取: %v", err)
			}
			fact, err := terminaladapter.ToAgentTaskTerminal(ctx, record, terminaladapter.Dependencies{})
			if err != nil {
				return err
			}
			if _, err = b.runtime.RecordTerminal(ctx, id, nodeID, fact); err != nil {
				return err
			}
			if err = b.outcomes.AckDelivery(record.OutcomeRef); err != nil {
				return err
			}
		}
		if s.Status == "open" {
			if err := b.runtime.Step(ctx, id); err != nil {
				return err
			}
		}
		current, _, err := b.runtime.Store.Get(id)
		if err != nil {
			return err
		}
		if err = b.plan(ctx, current); err != nil {
			return err
		}
	}
	return nil
}

func (b *dataflowBridge) plan(ctx context.Context, s graph.DataflowSnapshot) error {
	if len(s.PlanningEvents) == 0 {
		return nil
	}
	all, err := b.board.tasks.ScanAll()
	if err != nil {
		return err
	}
	for _, task := range all {
		if task.EventType == "__scheduler__" && task.InterventionGraphID == s.Definition.GraphID && !model.IsTerminal(task.Status) {
			return nil
		}
	}
	last := s.PlanningEvents[len(s.PlanningEvents)-1].Sequence
	id := "graph-plan-" + graphPlanningKey(s.Definition.GraphID, last)
	if existing, err := b.board.tasks.GetTask(id); err == nil && existing != nil {
		return b.runtime.AcknowledgePlanning(s.Definition.GraphID, last)
	}
	parent, err := b.board.origin(s.Definition.RunID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	objective := "根据新增事实维护同一张 agentTask 图。需要后续工作时 apply_graph_change(update/add)，完成后 submit_task_result 结束本次规划；不等待子任务。全部目标已达到时 control_graph(complete)，选择真实 result_refs。不要重建图，不要调用旧控制节点工具。"
	if s.Terminal() {
		objective = "图已结束。根据确定的结果/交付回执向用户说明结论，通过 submit_task_result 提交最终答复。不得重启或修改已结束的图。"
	}
	task := &model.Task{ID: id, Description: objective, EventType: "__scheduler__", EventSource: "dataflow-planning", ParentTaskID: parent.ID, InterventionGraphID: s.Definition.GraphID, RouteScope: model.GraphRouteScope(s.Definition.GraphID), MaxConcurrency: 1}
	if s.Terminal() {
		task.InterventionGraphID = ""
		task.FinalReportGraphID = s.Definition.GraphID
	}
	if err = taskcontract.Inherit(parent, task, loopcontract.WorkCoordination); err != nil {
		return err
	}
	task.ContextInputs = []model.TaskContextInput{{Kind: "upstream_result", SourceRef: "dataflow-state:" + s.Definition.GraphID, Content: string(data)}}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = b.board.tasks.PublishTask(task); err != nil {
		return err
	}
	return b.runtime.AcknowledgePlanning(s.Definition.GraphID, last)
}

func graphPlanningKey(id string, seq int64) string {
	raw := fmt.Sprintf("%s:%d", id, seq)
	return contextcontract.ShortDigestText(raw)
}

type dataflowDelivery struct {
	store   *delivery.Store
	manager *workspace.Manager
	journal *effect.Journal
	graphs  *graph.DataflowStore
}

func (d *dataflowDelivery) ValidateCandidate(ctx context.Context, graphID, runID, ref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.manager.CheckCandidateCommit(ref, graphID, runID)
}

func (d *dataflowDelivery) CommitCandidate(ctx context.Context, completionID, graphID, candidateRef string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	state, ok, err := d.graphs.Get(graphID)
	if err != nil || !ok {
		return "", fmt.Errorf("图不存在: %v", err)
	}
	if _, _, err = d.manager.CandidateChanges(candidateRef, graphID, state.Definition.RunID); err != nil {
		return "", err
	}
	id := "dataflow-delivery-" + strings.TrimPrefix(completionID, "sha256:")
	for _, entry := range d.journal.Query(id) {
		if entry.TaskID == id {
			if entry.Status == effect.StatusSettled {
				if record, ok, err := d.store.Get("delivery:" + entry.ID); err != nil || !ok || record.Status != "committed" {
					return "", fmt.Errorf("交付回执未确认")
				}
				return "delivery:" + entry.ID, nil
			}
			return "", fmt.Errorf("已有未确认交付，禁止重放")
		}
	}
	e := effect.Effect{TaskID: id, Kind: effect.KindWorkspaceMerge, Target: candidateRef, ArgsDigest: contextcontract.ShortDigestText(candidateRef), Policy: effect.PolicyNeverReplay}
	if err = d.journal.Prepare(&e); err != nil {
		return "", err
	}
	if _, err = d.store.Prepare(delivery.CommitRecord{ID: "delivery:" + e.ID, RunID: state.Definition.RunID, GraphID: graphID, CompletionRef: completionID, CandidateRef: candidateRef, EffectRef: e.ID}); err != nil {
		return "", err
	}
	if err = d.manager.ApplyCandidate(candidateRef, graphID, state.Definition.RunID); err != nil {
		if markErr := d.journal.MarkUnknown(e.ID, err.Error()); markErr != nil {
			return "", markErr
		}
		_, _ = d.store.Finish("delivery:"+e.ID, false, err.Error())
		return "", err
	}
	if err = d.journal.Settle(e.ID, "候选已提交至项目根"); err != nil {
		return "", err
	}
	if _, err = d.store.Finish("delivery:"+e.ID, true, ""); err != nil {
		return "", err
	}
	return "delivery:" + e.ID, nil
}
