package userdef

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"agentgo/internal/trace"
)

// 模板变量替换：${event.x.y} → 从 trace.Event 取值，纯字符串拼接语义。
//
// 字段路径白名单：v5 首版只支持 trace.Event 已有字段的固定子集。
// 未知路径在启动期校验时即报错（避免运行期"模板替换出空串"的隐式 bug）。
//
// 设计原则：
//   - 不用 reflect：路径集小且稳定，switch 表既安全又快
//   - 数字字段调用 .render() 时转 base-10 字符串
//   - 未来加新字段：在此 switch 加一行 + LoadFromFile 启动期 unit test 自动覆盖
var varRefRE = regexp.MustCompile(`\$\{([a-zA-Z][a-zA-Z0-9_.]*)\}`)

// resolveField 把 "event.x.y" 路径映射到 trace.Event 字段值（统一 string 形态）。
// 返回 (value, ok)；ok=false 表示路径未识别（启动期应当拒绝）。
func resolveField(ev trace.Event, path string) (string, bool) {
	switch path {
	case "event.kind":
		return string(ev.Kind), true
	case "event.task.id":
		return ev.TaskID, true
	case "event.task.description":
		return ev.Description, true
	case "event.task.error":
		return ev.Error, true
	case "event.task.reason":
		return ev.Reason, true
	case "event.task.retry_count":
		if ev.Transition != nil {
			return strconv.Itoa(ev.Transition.RetryCount), true
		}
		return strconv.Itoa(ev.AttemptNo), true
	case "event.task.prev_status":
		if ev.Transition != nil {
			return ev.Transition.PrevStatus, true
		}
		return "", true
	case "event.task.new_status":
		if ev.Transition != nil {
			return ev.Transition.NewStatus, true
		}
		return "", true
	case "event.task.cancel_source":
		if ev.Transition != nil {
			return ev.Transition.CancelSource, true
		}
		return "", true
	case "event.cause":
		if ev.Transition != nil {
			return ev.Transition.Cause, true
		}
		return "", true
	case "event.task.priority":
		return ev.Priority, true
	case "event.task.event_type":
		return ev.EventType, true
	case "event.task.depth":
		return strconv.Itoa(ev.Depth), true
	case "event.agent.id":
		return ev.AgentID, true
	case "event.agent.prev_state":
		if ev.Transition != nil {
			return ev.Transition.PrevState, true
		}
		return "", true
	case "event.agent.new_state":
		if ev.Transition != nil {
			return ev.Transition.NewState, true
		}
		return "", true
	case "event.loop":
		return strconv.Itoa(ev.Loop), true
	case "event.tool":
		return ev.Tool, true
	case "event.path":
		return ev.Path, true
	case "event.error":
		return ev.Error, true
	case "event.reason":
		return ev.Reason, true
	case "event.attempt_no":
		return strconv.Itoa(ev.AttemptNo), true
	case "event.output_len":
		return strconv.Itoa(ev.OutputLen), true
	case "event.loops_used":
		return strconv.Itoa(ev.LoopsUsed), true
	case "event.shell.command":
		if ev.ShellExec != nil {
			return ev.ShellExec.Command, true
		}
		if ev.ShellTimeout != nil {
			return ev.ShellTimeout.Command, true
		}
		return "", true
	case "event.shell.exit_code":
		if ev.ShellExec != nil {
			return strconv.Itoa(ev.ShellExec.ExitCode), true
		}
		return "", true
	case "event.shell.outcome":
		if ev.ShellExec != nil {
			return ev.ShellExec.Outcome, true
		}
		return "", true
	case "event.shell.duration_ms":
		if ev.ShellExec != nil {
			return strconv.FormatInt(ev.ShellExec.DurationMS, 10), true
		}
		return "", true
	case "event.shell.decision":
		if ev.ShellTimeout != nil {
			return ev.ShellTimeout.Decision, true
		}
		return "", true
	}
	return "", false
}

// validatePaths 扫描字符串中所有 ${...} 引用，确认全部命中白名单。
// 启动期 loader 调用此函数：未知路径立即报错，避免运行期沉默失败。
func validatePaths(s string) error {
	matches := varRefRE.FindAllStringSubmatch(s, -1)
	for _, m := range matches {
		path := m[1]
		var dummy trace.Event
		if _, ok := resolveField(dummy, path); !ok {
			return fmt.Errorf("unknown variable reference %q (in %q)", path, s)
		}
	}
	return nil
}

// renderTemplate 把字符串中的 ${event.x.y} 替换为 trace.Event 中的值。
// 启动期已经过 validatePaths，此函数对未知路径替换为空串（防御性兜底，理论不会触发）。
func renderTemplate(tpl string, ev trace.Event) string {
	return varRefRE.ReplaceAllStringFunc(tpl, func(match string) string {
		// match 形如 "${event.x.y}"，剥外壳得到路径
		path := match[2 : len(match)-1]
		val, ok := resolveField(ev, path)
		if !ok {
			return ""
		}
		return val
	})
}

// promptTemplate 是 PromptSpec 启动期加载后的可渲染产物。
//
// content 是 prompt 文件正文（启动期一次读入）。args 在 render 时按 trace.Event 替换。
// args 与 prompt 内部的 ${event.x} 都允许出现——args 是"先把 event 字段绑到命名变量"
// 的语法糖，prompt 内既可写 ${event.task.id} 也可写 ${task_id}（如果 args 中定义了）。
type promptTemplate struct {
	content string
	args    map[string]string // value 是含 ${event.x} 的字符串
}

// render 按 ev 渲染最终字符串。先解 args（${event.x} → 值），再用 args 命名变量
// 替换 content 中的 ${name} 引用，最后再扫一遍 content 中残留的 ${event.x}。
func (p *promptTemplate) render(ev trace.Event) string {
	if p == nil {
		return ""
	}
	resolvedArgs := make(map[string]string, len(p.args))
	for name, valTpl := range p.args {
		resolvedArgs[name] = renderTemplate(valTpl, ev)
	}
	out := varRefRE.ReplaceAllStringFunc(p.content, func(match string) string {
		path := match[2 : len(match)-1]
		// 优先 args 命名变量
		if v, ok := resolvedArgs[path]; ok {
			return v
		}
		// 退到 event 直引
		if v, ok := resolveField(ev, path); ok {
			return v
		}
		return ""
	})
	return out
}

// validatePromptTemplate 启动期校验 content 与 args 中所有 ${...} 引用：
//   - args 的 value 中只能含 ${event.x}（不能引用 args 自身）
//   - content 中的 ${name} 必须命中 args key 或 event 字段路径
func validatePromptTemplate(content string, args map[string]string) error {
	for name, val := range args {
		if !isIdentifier(name) {
			return fmt.Errorf("invalid arg name %q (must be [a-zA-Z][a-zA-Z0-9_]*)", name)
		}
		if err := validatePaths(val); err != nil {
			return fmt.Errorf("arg %q: %w", name, err)
		}
	}
	matches := varRefRE.FindAllStringSubmatch(content, -1)
	for _, m := range matches {
		path := m[1]
		if _, ok := args[path]; ok {
			continue
		}
		var dummy trace.Event
		if _, ok := resolveField(dummy, path); !ok {
			return fmt.Errorf("prompt content references unknown %q (not in args, not in event fields)", path)
		}
	}
	return nil
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || isLetter(r)) {
				return false
			}
			continue
		}
		if !(r == '_' || isLetter(r) || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// trimQuotes 去掉字符串字面量两端的单/双引号（when 条件解析用）。
func trimQuotes(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1], true
		}
	}
	return s, false
}
