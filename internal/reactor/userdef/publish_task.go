package userdef

import (
	"fmt"

	"agentgo/internal/model"
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
	task := &model.Task{
		Description:  desc,
		EventType:    r.eventType,
		Priority:     r.priority,
		Dependencies: deps,
	}
	if err := r.store.PublishTask(task); err != nil {
		return fmt.Errorf("publish_task[%s]: store.PublishTask: %w", r.name, err)
	}
	return nil
}
