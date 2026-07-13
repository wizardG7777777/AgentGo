package userdef

import (
	"fmt"
	"strings"

	"agentgo/internal/reactor"
	"agentgo/internal/trace"
)

// ReplanRequester 是 request_replan Reactor 唯一可见的控制面接口。
//
// 实现负责根据原始事件解析 PlanID、版本和幂等键并持久化请求。用户 YAML 只能提供
// reasonCode / urgency / detail，不能控制任何权威路由或版本字段。
type ReplanRequester interface {
	RequestReplanFromEvent(ev trace.Event, reasonCode, urgency, detail string) (string, error)
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
	requester  ReplanRequester
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
	if deps.ReplanRequester == nil {
		return nil, fmt.Errorf("request_replan requires Deps.ReplanRequester, got nil")
	}

	return &requestReplanReactor{
		name:       name,
		onKind:     kind,
		when:       when,
		reasonCode: action.ReasonCode,
		urgency:    action.Urgency,
		detail:     action.Detail,
		requester:  deps.ReplanRequester,
	}, nil
}

func (r *requestReplanReactor) Name() string                 { return r.name }
func (r *requestReplanReactor) Subscribe() []trace.EventKind { return []trace.EventKind{r.onKind} }
func (r *requestReplanReactor) IsSync() bool                 { return false }
func (r *requestReplanReactor) Priority() int                { return 500 }

func (r *requestReplanReactor) Run(ev trace.Event) error {
	if !r.when.eval(ev) {
		return nil
	}
	if r.requester == nil {
		return fmt.Errorf("request_replan[%s]: requester not configured", r.name)
	}
	if _, err := r.requester.RequestReplanFromEvent(
		ev,
		r.reasonCode,
		r.urgency,
		r.detail,
	); err != nil {
		return fmt.Errorf("request_replan[%s]: %w", r.name, err)
	}
	return nil
}

var _ reactor.Reactor = (*requestReplanReactor)(nil)
