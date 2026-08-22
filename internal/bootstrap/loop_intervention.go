package bootstrap

// 本文件是 L4 LoopInterventionRequested 到 L5 Scheduler coordination 的
// 唯一生产桥。它刻意位于 TaskOutcome terminal feed 之后：只有 source Task 的
// Outcome delivery 已被 Graph Runtime durable 消费（或 durable fallback）后，
// 才按 source task 定向物化一个脱图的 Scheduler wake。wake 自身形成 durable
// TaskOutcome 后，才确认原 intervention；PublishTask 成功绝不等价于 Ack。

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"agentgo/internal/intervention"
	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
	"agentgo/internal/model"
	"agentgo/internal/outcomestore"
	"agentgo/internal/reactor"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/trace"
)

const (
	loopInterventionWakeSource = model.TaskEventSourceLoopIntervention
	loopInterventionConsumer   = "scheduler-coordination/v1"
)

type taskOutcomeDeliveryReader interface {
	GetByRef(outcomeRef string) (outcomestore.Record, bool, error)
	PendingDeliveries() ([]outcomestore.Record, error)
}

type loopInterventionStore interface {
	PendingInterventionsForTask(taskID string) ([]loopcontract.LoopInterventionRequested, error)
	AckIntervention(taskID string, ack loopstore.InterventionAck) error
}

// loopInterventionHandler 只创建协调任务，不调用 Graph Runtime，不修改 Graph
// revision/state。Graph command 与非 Graph command 都由 Scheduler 自己裁决。
type loopInterventionHandler struct {
	tasks store.TaskStore
}

func (h loopInterventionHandler) HandleLoopIntervention(
	ctx context.Context, command loopcontract.LoopInterventionRequested,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if h.tasks == nil {
		return "", fmt.Errorf("LoopIntervention handler 缺少 TaskStore")
	}
	if err := command.Validate(); err != nil {
		return "", err
	}
	source, err := h.tasks.GetTask(command.TaskID)
	if err != nil {
		return "", fmt.Errorf("读取 intervention source Task %s: %w", command.TaskID, err)
	}
	if err := validateLoopInterventionSource(source, command); err != nil {
		return "", err
	}
	wake, err := buildLoopInterventionWake(source, command)
	if err != nil {
		return "", err
	}
	if existing, getErr := h.tasks.GetTask(wake.ID); getErr == nil {
		if err := validateExistingLoopInterventionWake(existing, wake); err != nil {
			return "", err
		}
		return intervention.WakeDecisionRef(command.CommandID), nil
	} else if !errors.Is(getErr, store.ErrTaskNotFound) {
		return "", fmt.Errorf("查询 intervention wake %s: %w", wake.ID, getErr)
	}
	if err := h.tasks.PublishTask(wake); err != nil {
		if !errors.Is(err, store.ErrTaskAlreadyExists) {
			return "", fmt.Errorf("发布 intervention wake %s: %w", wake.ID, err)
		}
		// 与另一投递者竞争时重新读取并校验，不能把任意同 ID Task 当作成功。
		existing, getErr := h.tasks.GetTask(wake.ID)
		if getErr != nil {
			return "", fmt.Errorf("重读并发 intervention wake %s: %w", wake.ID, getErr)
		}
		if err := validateExistingLoopInterventionWake(existing, wake); err != nil {
			return "", err
		}
	}
	return intervention.WakeDecisionRef(command.CommandID), nil
}

func validateLoopInterventionSource(source *model.Task, command loopcontract.LoopInterventionRequested) error {
	if source == nil || source.ID != command.TaskID || source.RunID != command.RunID ||
		source.GraphID != command.GraphID || source.NodeID != command.NodeID ||
		source.ActivationID != command.ActivationID || source.AttemptID != command.AttemptID {
		return fmt.Errorf("LoopIntervention %s 与 source Task lineage 不一致", command.CommandID)
	}
	if !model.IsTerminal(source.Status) {
		return fmt.Errorf("LoopIntervention source Task %s 尚未终态", source.ID)
	}
	if strings.TrimSpace(source.OutcomeRef) == "" {
		return fmt.Errorf("LoopIntervention source Task %s 缺少 durable outcome_ref", source.ID)
	}
	return nil
}

func buildLoopInterventionWake(source *model.Task, command loopcontract.LoopInterventionRequested) (*model.Task, error) {
	wakeID := intervention.WakeTaskID(command.CommandID)
	if wakeID == "" {
		return nil, fmt.Errorf("LoopIntervention command_id 不能为空")
	}
	marker := "[loop-intervention: " + command.CommandID + "]"
	scope := "非 Graph Run"
	directive := "读取当前公告板与 Run 状态；原任务若停在未提交 GraphDraft，使用 create_graph_draft 开启新的 authoring transaction（request_ref 由 framework 绑定原始请求），不得把 checkpoint_ref 当 ContentRef，也不得恢复原 Attempt 空转；无法收敛时诚实结束为 blocked/failed。"
	if command.GraphID != "" {
		scope = fmt.Sprintf("Graph %s 节点 %s activation %s", command.GraphID, command.NodeID, command.ActivationID)
		directive = "先读取当前 Graph/Definition revision，再决定提交 GraphChangeProposal、以改变后的契约创建新 Activation，或诚实沿 blocked/failed 路径收口；不得直接重开相同 Attempt。"
	}
	description := fmt.Sprintf(
		"%s\n%s 的 L4 Loop 请求结构化介入。\nreason_code=%s checkpoint_ref=%s attempt_id=%s\n"+
			"no_progress: model_calls=%d tool_actions=%d attempts=%d；missing_milestones=%s；repeated_signals=%d。\n"+
			"处理指引：%s",
		marker, scope, command.ReasonCode, command.CheckpointRef, command.AttemptID,
		command.BudgetUsed.ModelCalls, command.BudgetUsed.ToolActions, command.BudgetUsed.Attempts,
		strings.Join(command.MissingMilestones, ","), len(command.RepeatedSignals), directive,
	)
	wake := &model.Task{
		ID: wakeID, Description: description,
		EventType: "__scheduler__", EventSource: loopInterventionWakeSource,
		ParentTaskID: command.TaskID, MaxConcurrency: 1, RunPhase: runcontract.PhaseRecovery,
	}
	if err := taskcontract.Inherit(source, wake, loopcontract.WorkCoordination); err != nil {
		return nil, fmt.Errorf("继承 intervention coordination 契约: %w", err)
	}
	// 这是 L5 控制面输入，不是 Graph activation Task。携带图身份会让
	// graph-terminal-feed 把它误当成节点终态回填。
	wake.GraphID = ""
	wake.NodeID = ""
	wake.ActivationID = ""
	wake.GraphNodeKind = ""
	wake.InterventionGraphID = command.GraphID
	wake.InterventionNodeID = command.NodeID
	wake.InterventionActivationID = command.ActivationID
	return wake, nil
}

func validateExistingLoopInterventionWake(existing, expected *model.Task) error {
	if existing == nil || expected == nil || existing.ID != expected.ID ||
		existing.EventType != expected.EventType || existing.EventSource != expected.EventSource ||
		existing.ParentTaskID != expected.ParentTaskID || existing.Description != expected.Description ||
		existing.RunID != expected.RunID || existing.RunPhase != expected.RunPhase ||
		existing.GraphID != "" || existing.NodeID != "" ||
		existing.ActivationID != "" || !reflect.DeepEqual(existing.RunContract, expected.RunContract) ||
		existing.InterventionGraphID != expected.InterventionGraphID ||
		existing.InterventionNodeID != expected.InterventionNodeID ||
		existing.InterventionActivationID != expected.InterventionActivationID ||
		!reflect.DeepEqual(existing.ProgressContract, expected.ProgressContract) ||
		existing.ContextPolicyRef != expected.ContextPolicyRef {
		return fmt.Errorf("deterministic intervention wake task_id=%s 已被不一致事实占用", expected.ID)
	}
	return nil
}

// loopInterventionBridge 订阅与 graph-terminal-feed 相同的终态事件，但 priority
// 更晚且同属 reliable FIFO lane。显式 Outcome pending 检查仍是最终顺序守卫，
// 不能只依赖注册顺序猜测 Graph settlement 已完成。
type loopInterventionBridge struct {
	tasks    store.TaskStore
	loops    loopInterventionStore
	outcomes taskOutcomeDeliveryReader
	pump     *intervention.Pump
}

func newLoopInterventionBridge(tasks store.TaskStore, loops loopInterventionStore,
	outcomes taskOutcomeDeliveryReader) (*loopInterventionBridge, error) {
	if tasks == nil || loops == nil || outcomes == nil {
		return nil, fmt.Errorf("LoopIntervention bridge 缺少 Task/Loop/Outcome Store")
	}
	pump := &intervention.Pump{
		Store: loops, Handler: loopInterventionHandler{tasks: tasks},
		Consumer: loopInterventionConsumer,
	}
	return &loopInterventionBridge{tasks: tasks, loops: loops, outcomes: outcomes, pump: pump}, nil
}

func (b *loopInterventionBridge) Name() string { return "loop-intervention-bridge" }

func (b *loopInterventionBridge) Subscribe() []trace.EventKind {
	return []trace.EventKind{
		trace.KindTaskCompleted, trace.KindTaskFailed, trace.KindTaskBlocked, trace.KindTaskCancelled,
	}
}

func (b *loopInterventionBridge) IsSync() bool        { return false }
func (b *loopInterventionBridge) ReliableAsync() bool { return true }
func (b *loopInterventionBridge) Priority() int       { return 110 }

func (b *loopInterventionBridge) Run(ev trace.Event) error {
	return b.RunWithContext(context.Background(), ev)
}

func (b *loopInterventionBridge) RunWithContext(ctx context.Context, ev trace.Event) error {
	if b == nil || strings.TrimSpace(ev.TaskID) == "" {
		return nil
	}
	task, err := b.tasks.GetTask(ev.TaskID)
	if err != nil {
		// Session freeze/switch 可让旧 session 的 reliable event 迟到。任务已
		// 不在当前公告板时直接忽略，绝不把历史 command 投递到新 Session。
		if errors.Is(err, store.ErrTaskNotFound) {
			return nil
		}
		return err
	}
	if task == nil || !model.IsTerminal(task.Status) {
		return nil
	}
	ready, err := b.outcomeDelivered(task)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("Task %s 的 TaskOutcome 尚未完成 terminal delivery，拒绝抢跑 intervention", task.ID)
	}

	var errs []error
	// 若本 Task 本身是某条 coordination wake，先以它的 durable OutcomeRef
	// 确认原 command；PublishTask 成功从不走这条 Ack 路径。
	if task.EventSource == loopInterventionWakeSource {
		if err := b.ackWakeOutcome(ctx, task); err != nil {
			errs = append(errs, err)
		}
		if err := b.absorbNestedWakeInterventions(ctx, task); err != nil {
			errs = append(errs, err)
		}
		// coordination wake 是单层人工/框架裁决边界。它自己的 no-progress
		// command 以该 wake 的 durable Outcome 吸收，禁止递归创建 wake 链。
		return errors.Join(errs...)
	}
	// 同一 terminal Task 也可能刚产生新的 intervention；只定向读取该 Task，
	// 绝不全 Store Drain。
	if _, err := b.pump.EnsureTask(ctx, task.ID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (b *loopInterventionBridge) absorbNestedWakeInterventions(ctx context.Context, wake *model.Task) error {
	commands, err := b.loops.PendingInterventionsForTask(wake.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, command := range commands {
		if err := b.pump.Ack(ctx, command.TaskID, command.CommandID, wake.OutcomeRef); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (b *loopInterventionBridge) outcomeDelivered(task *model.Task) (bool, error) {
	ref := strings.TrimSpace(task.OutcomeRef)
	if ref == "" {
		return false, nil
	}
	record, ok, err := b.outcomes.GetByRef(ref)
	if err != nil {
		return false, err
	}
	if !ok || record.Outcome.TaskID != task.ID || record.Outcome.RunID != task.RunID {
		return false, fmt.Errorf("Task %s 的 outcome_ref=%s 不可解引用或 lineage 不一致", task.ID, ref)
	}
	pending, err := b.outcomes.PendingDeliveries()
	if err != nil {
		return false, err
	}
	for _, candidate := range pending {
		if candidate.OutcomeRef == ref {
			return false, nil
		}
	}
	return true, nil
}

func (b *loopInterventionBridge) ackWakeOutcome(ctx context.Context, wake *model.Task) error {
	commands, err := b.loops.PendingInterventionsForTask(wake.ParentTaskID)
	if err != nil {
		return err
	}
	for _, command := range commands {
		if intervention.WakeTaskID(command.CommandID) != wake.ID {
			continue
		}
		if wake.RunID != command.RunID || wake.EventType != "__scheduler__" ||
			wake.GraphID != "" || wake.NodeID != "" || wake.ActivationID != "" {
			return fmt.Errorf("intervention wake %s 与 command %s lineage 不一致", wake.ID, command.CommandID)
		}
		return b.pump.Ack(ctx, command.TaskID, command.CommandID, wake.OutcomeRef)
	}
	return nil // 已 ack 的重复 terminal event 幂等忽略。
}

// wireLoopInterventionBridge 必须在 graph-terminal-feed 注册后、Reactor
// dispatcher 启用前调用。priority 仍保证即使注册顺序变化也先做 Graph feed。
func wireLoopInterventionBridge(tasks store.TaskStore, loops loopInterventionStore,
	outcomes *outcomestore.Store, registry *reactor.Registry) (*loopInterventionBridge, error) {
	if registry == nil {
		return nil, fmt.Errorf("LoopIntervention bridge 缺少 Reactor Registry")
	}
	bridge, err := newLoopInterventionBridge(tasks, loops, outcomes)
	if err != nil {
		return nil, err
	}
	if err := registry.Register(bridge); err != nil {
		return nil, fmt.Errorf("注册 LoopIntervention bridge: %w", err)
	}
	return bridge, nil
}
