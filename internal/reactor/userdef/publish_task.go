package userdef

import (
	"errors"
	"fmt"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/taskcontract"
	"agentgo/internal/trace"
)

// PublishStore 是 publish_task reactor 的最小依赖接口（DI / 测试用）。
// 真实路径由 store.MemoryTaskStore 实现。
type PublishStore interface {
	PublishTask(task *model.Task) error
}

// publishTaskReactor 是 PublishTaskAction 对应的 reactor.Reactor 实现。
//
// 同步性：Async（用户 reactor 默认异步，避免用户配置错误把主流程拖死）。
// Priority：500（低于 builtin 的 950，但用户 reactor 之间无内部排序需求）。
//
// C6b 起 Plan 控制面已删除，publish_task 行为统一为直接发布——不再有
// 「Plan 内任务改道 ReplanRequest」的 plan_boundary 拦截。
type publishTaskReactor struct {
	name      string
	onKind    trace.EventKind
	when      *whenCond
	desc      *promptTemplate
	kind      string
	eventType string
	priority  int
	store     PublishStore

	// depTemplates 是 publish_task.dependencies 字段的字符串模板列表。
	// 每条可含 ${event.x} 引用（启动期已 validatePaths 校验）；运行时 render
	// 后非空值进入 Task.Dependencies。空字符串模板会被 silently 跳过。
	depTemplates []string
}

func (r *publishTaskReactor) Name() string                 { return r.name }
func (r *publishTaskReactor) Subscribe() []trace.EventKind { return []trace.EventKind{r.onKind} }
func (r *publishTaskReactor) IsSync() bool                 { return false }
func (r *publishTaskReactor) Priority() int                { return 500 }

// Run 在事件触发时执行。when 条件不满足直接 nil 返回（不算失败）。
//
// 错误语义：description 渲染后为空 / store.PublishTask 失败 → 返回 error；
// Registry 会记日志，但 Async 路径不阻塞主流程（Phase 4 设计）。
func (r *publishTaskReactor) Run(ev trace.Event) error {
	if !r.when.eval(ev) {
		return nil
	}
	desc := r.desc.render(ev)
	if desc == "" {
		return fmt.Errorf("publish_task[%s]: rendered description is empty", r.name)
	}
	var deps []string
	for _, tpl := range r.depTemplates {
		if v := renderTemplate(tpl, ev); v != "" {
			deps = append(deps, v)
		}
	}
	batchID := ev.BatchID
	if batchID == "" {
		batchID = ev.TaskID
	}
	task := &model.Task{
		Description:    desc,
		EventType:      r.eventType,
		Priority:       r.priority,
		Dependencies:   deps,
		EventSource:    ev.TaskID,
		ParentTaskID:   ev.TaskID,
		ReplyToAgentID: ev.AgentID,
		BatchID:        batchID,
	}
	var parent *model.Task
	reader, readable := r.store.(interface {
		GetTask(string) (*model.Task, error)
	})
	if readable && ev.TaskID != "" {
		var getErr error
		parent, getErr = reader.GetTask(ev.TaskID)
		if getErr != nil {
			if ev.RunID != "" || !errors.Is(getErr, store.ErrTaskNotFound) {
				return fmt.Errorf("publish_task[%s]: read parent RunContract: %w", r.name, getErr)
			}
			// 无 RunID 的旧 trace 允许 source 已被淘汰；保持全空 binding 的
			// 显式 legacy 兼容，不伪造新的 Run 身份。
			parent = nil
		}
	}
	if ev.RunID != "" {
		if ev.TaskID == "" || !readable || parent == nil {
			return fmt.Errorf("publish_task[%s]: 新 Run %s 缺少可解引用 source Task", r.name, ev.RunID)
		}
		if string(parent.RunID) != ev.RunID {
			return fmt.Errorf("publish_task[%s]: source Task RunID=%s 与事件 RunID=%s 不一致",
				r.name, parent.RunID, ev.RunID)
		}
	}
	if parent != nil {
		workClass := loopcontract.WorkInvestigation
		if parent.ProgressContract != nil {
			workClass = parent.ProgressContract.WorkClass
		}
		if err := taskcontract.Inherit(parent, task, workClass); err != nil {
			return fmt.Errorf("publish_task[%s]: inherit RunContract: %w", r.name, err)
		}
	}
	if err := r.store.PublishTask(task); err != nil {
		return fmt.Errorf("publish_task[%s]: store.PublishTask: %w", r.name, err)
	}
	return nil
}
