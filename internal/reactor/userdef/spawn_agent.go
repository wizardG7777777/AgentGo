package userdef

import (
	"context"
	"fmt"
	"time"

	"agentgo/internal/spawn"
	"agentgo/internal/trace"
)

// spawnAgentReactor 是 spawn_agent 动词的 reactor.Reactor 实现。
//
// 同步性：Async（与其他用户 reactor 一致）；Priority 500。
// 一次触发只 spawn 一个 ad-hoc agent + 一个 initial_task。
type spawnAgentReactor struct {
	name       string
	onKind     trace.EventKind
	when       *whenCond
	baseKind   string
	override   spawn.RuntimeOverride
	desc       descRenderer    // S7：可能是 staticDesc 或 translatedDesc
	sysPromTpl *promptTemplate // override.system_prompt（可为 nil）
	lifecycle  string
	host       spawn.SpawnHost
}

func (r *spawnAgentReactor) Name() string                 { return r.name }
func (r *spawnAgentReactor) Subscribe() []trace.EventKind { return []trace.EventKind{r.onKind} }
func (r *spawnAgentReactor) IsSync() bool                 { return false }
func (r *spawnAgentReactor) Priority() int                { return 500 }

// Run 渲染 description / system_prompt（如有 override）→ 调 SpawnHost.Spawn。
//
// via_translator 路径会在此处发起 reactor 自带 LLM 调用（同步阻塞 reactor goroutine）。
// LLM 失败或超时会返回 error，由 Registry 记日志（async 不阻塞主流程）。
func (r *spawnAgentReactor) Run(ev trace.Event) error {
	if !r.when.eval(ev) {
		return nil
	}
	if r.lifecycle != "" && r.lifecycle != "one_shot" {
		return fmt.Errorf("spawn_agent[%s]: lifecycle %q not implemented (v5.x; only one_shot)", r.name, r.lifecycle)
	}
	descCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	desc, err := r.desc.render(descCtx, ev)
	if err != nil {
		return fmt.Errorf("spawn_agent[%s]: %w", r.name, err)
	}
	if desc == "" {
		return fmt.Errorf("spawn_agent[%s]: rendered description is empty", r.name)
	}

	override := r.override // 复制基础整数字段
	if r.sysPromTpl != nil {
		override.SystemPrompt = r.sysPromTpl.render(ev)
		override.SystemPromptSet = true
	}

	req := spawn.SpawnRequest{
		BaseKind:               r.baseKind,
		Override:               override,
		InitialTaskDescription: desc,
		Lifecycle:              r.lifecycle,
		SourceTaskID:           ev.TaskID,
		Depth:                  ev.Depth + 1,
	}
	if _, _, err := r.host.Spawn(context.Background(), req); err != nil {
		return fmt.Errorf("spawn_agent[%s]: %w", r.name, err)
	}
	return nil
}

// descRenderer 是 spawn_agent.initial_task.description 的渲染抽象（S7）。
//
// 两种实现：
//   - staticDesc：常规 PromptSpec.File，纯字符串模板替换
//   - translatedDesc：via_translator——translator_prompt 渲染后调 reactor LLM 二次加工
type descRenderer interface {
	render(ctx context.Context, ev trace.Event) (string, error)
}

// staticDesc 包装常规 promptTemplate，render 是纯模板替换（无错误）。
type staticDesc struct {
	tpl *promptTemplate
}

func (s *staticDesc) render(_ context.Context, ev trace.Event) (string, error) {
	return s.tpl.render(ev), nil
}

// translatedDesc 实现 §6.1.4 场景 4：description 由 reactor LLM 二次加工生成。
//
// 渲染步骤：
//  1. 用 trace.Event 渲染 translator_prompt（${event.x.y} 替换）
//  2. 把渲染后的 prompt 喂给 reactor 自带 LLMCompleter（无工具 / 无 history）
//  3. LLM 输出文本即为 spawned agent 的 initial_task.description
//
// translator 是 reactor 上下文的延伸——loader 已校验它不复用主 agent client（原则 5）。
type translatedDesc struct {
	translatorTpl *promptTemplate
	llm           LLMCompleter
	timeout       time.Duration
}

func (t *translatedDesc) render(ctx context.Context, ev trace.Event) (string, error) {
	p := t.translatorTpl.render(ev)
	if p == "" {
		return "", fmt.Errorf("via_translator: rendered translator prompt is empty")
	}
	rctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, err := t.llm.Complete(rctx, p)
	if err != nil {
		return "", fmt.Errorf("via_translator LLM: %w", err)
	}
	if out == "" {
		return "", fmt.Errorf("via_translator: LLM returned empty output")
	}
	return out, nil
}
