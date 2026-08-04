package userdef

import (
	"fmt"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// requestReplanWakeEventType 是 replan 唤醒任务的路由：直接进 Scheduler 队列。
const requestReplanWakeEventType = "__scheduler__"

// replanWakeStore 是 request_replan 动作的最小存储依赖：发布唤醒任务 +
// 幂等扫描 + 源任务图身份识别。*store.MemoryTaskStore 天然满足本接口。
type replanWakeStore interface {
	PublishTask(task *model.Task) error
	ScanAll() ([]*model.Task, error)
	GetTask(taskID string) (*model.Task, error)
}

// UnmarshalYAML 对 request_replan 子结构启用严格字段校验。顶层 loader 目前为了兼容
// 历史 YAML 不全局启用 KnownFields，因此这里必须局部拒绝 plan_id / revision /
// idempotency_key 等越权字段，不能静默忽略。
func (a *RequestReplanAction) UnmarshalYAML(unmarshal func(any) error) error {
	var fields map[string]any
	if err := unmarshal(&fields); err != nil {
		return err
	}
	for field := range fields {
		switch field {
		case "reason_code", "urgency", "detail":
		default:
			return fmt.Errorf(
				"request_replan: unknown field %q (allowed: reason_code, urgency, detail)",
				field,
			)
		}
	}

	type rawAction RequestReplanAction
	var raw rawAction
	if err := unmarshal(&raw); err != nil {
		return err
	}
	*a = RequestReplanAction(raw)
	return nil
}

type requestReplanReactor struct {
	name       string
	onKind     trace.EventKind
	when       *whenCond
	reasonCode string
	urgency    string
	detail     string
	store      replanWakeStore
}

func buildRequestReplan(
	name string,
	kind trace.EventKind,
	when *whenCond,
	action *RequestReplanAction,
	deps Deps,
) (*requestReplanReactor, error) {
	if action == nil {
		return nil, fmt.Errorf("request_replan action is nil")
	}
	if strings.TrimSpace(action.ReasonCode) == "" {
		return nil, fmt.Errorf("request_replan: 'reason_code' is required")
	}
	switch action.Urgency {
	case "normal", "high":
		// valid
	default:
		return nil, fmt.Errorf(
			"request_replan: urgency must be one of normal/high (got %q)",
			action.Urgency,
		)
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("request_replan requires Deps.Store, got nil")
	}
	wakeStore, ok := deps.Store.(replanWakeStore)
	if !ok {
		return nil, fmt.Errorf("request_replan requires Deps.Store to support PublishTask/ScanAll/GetTask")
	}

	return &requestReplanReactor{
		name:       name,
		onKind:     kind,
		when:       when,
		reasonCode: action.ReasonCode,
		urgency:    action.Urgency,
		detail:     action.Detail,
		store:      wakeStore,
	}, nil
}

func (r *requestReplanReactor) Name() string                 { return r.name }
func (r *requestReplanReactor) Subscribe() []trace.EventKind { return []trace.EventKind{r.onKind} }
func (r *requestReplanReactor) IsSync() bool                 { return false }
func (r *requestReplanReactor) Priority() int                { return 500 }

// Run 发布「通用 replan 唤醒任务」给 Scheduler（C6b 起 Plan 控制面已删除，
// 本动作是用户 Reactor 请求重新编排的唯一通道）：
//
//   - 幂等键 <taskID>/replan 由本实现按源任务生成：发布前 ScanAll 发现同一
//     标记的未终态 __scheduler__ 唤醒任务则幂等返回，不重复发布；
//   - 唤醒任务刻意不带 GraphID/NodeID/ActivationID——不属于任何图，避免被
//     graph-terminal-feed 当作节点终态回填；
//   - 图任务（GraphID 非空）已有 request_replan 工具的 graph change 专用通道，
//     本动作对图任务仅留 trace 提示（择简：不发布、不报错），信号不丢。
func (r *requestReplanReactor) Run(ev trace.Event) error {
	if !r.when.eval(ev) {
		return nil
	}
	if ev.TaskID == "" {
		return fmt.Errorf("request_replan[%s]: event has no source task id", r.name)
	}

	// 图身份识别：GetTask 失败（任务已淘汰等）按非图任务处理，请求不丢。
	if src, err := r.store.GetTask(ev.TaskID); err == nil && src != nil && src.GraphID != "" {
		trace.Emit(trace.Event{
			Kind:   trace.KindError,
			TaskID: ev.TaskID,
			Error: fmt.Sprintf(
				"request_replan[%s]: 图任务（graph=%s）请使用 request_replan 工具（graph change 通道），通用 replan 唤醒任务不发布",
				r.name, src.GraphID),
		})
		return nil
	}

	marker := fmt.Sprintf("[replan-request: %s/replan]", ev.TaskID)

	// 幂等：同一请求者已有未终态的唤醒任务则不再重复发布。
	// 审计事实由唤醒任务自身的 task_published 事件承担，C6c 起不再单独
	// emit replan 时代事件。
	tasks, err := r.store.ScanAll()
	if err != nil {
		return fmt.Errorf("request_replan[%s]: ScanAll: %w", r.name, err)
	}
	for _, t := range tasks {
		if t == nil || t.EventType != requestReplanWakeEventType || model.IsTerminal(t.Status) {
			continue
		}
		if strings.Contains(t.Description, marker) {
			return nil
		}
	}

	detail := strings.TrimSpace(r.detail)
	if detail == "" {
		detail = "（无）"
	}
	wake := &model.Task{
		Description: fmt.Sprintf(
			"%s\n任务 %s 请求重新评估后续编排（reason_code=%s, urgency=%s, detail=%s）。处理指引：裁决是否补充/调整后续编排，无需调整则直接结束本任务。",
			marker, ev.TaskID, r.reasonCode, r.urgency, detail,
		),
		EventType:      requestReplanWakeEventType,
		EventSource:    "replan-request",
		ParentTaskID:   ev.TaskID,
		MaxConcurrency: 1,
	}
	if err := r.store.PublishTask(wake); err != nil {
		return fmt.Errorf("request_replan[%s]: publish wake task: %w", r.name, err)
	}
	return nil
}

var _ reactor.Reactor = (*requestReplanReactor)(nil)
