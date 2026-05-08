package userdef

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/trace"
)

// MailboxSender 是 send_message sink 需要的最小接口。
// 真实路径由 *mailbox.Registry 实现。
type MailboxSender interface {
	Send(msg mailbox.Message) error
}

// TraceEmitter 是 emit_trace sink 需要的最小接口。
// 缺省走包级 trace.Emit；测试可注入 fake 收集事件。
type TraceEmitter interface {
	Emit(ev trace.Event)
}

type defaultTraceEmitter struct{}

func (defaultTraceEmitter) Emit(ev trace.Event) { trace.Emit(ev) }

// invokeLLMReactor 是 invoke_llm 动词的 reactor.Reactor 实现。
type invokeLLMReactor struct {
	name       string
	onKind     trace.EventKind
	when       *whenCond
	prompt     *promptTemplate
	llm        LLMCompleter
	output     outputSink
	llmTimeout time.Duration
}

func (r *invokeLLMReactor) Name() string                 { return r.name }
func (r *invokeLLMReactor) Subscribe() []trace.EventKind { return []trace.EventKind{r.onKind} }
func (r *invokeLLMReactor) IsSync() bool                 { return false }
func (r *invokeLLMReactor) Priority() int                { return 500 }

// Run 执行：when 过滤 → 渲染 prompt → 调 LLM → 投递输出。
//
// 错误语义：when 不命中返回 nil；其余环节失败返回 error，由 Registry 记日志（async 不阻塞主流程）。
func (r *invokeLLMReactor) Run(ev trace.Event) error {
	if !r.when.eval(ev) {
		return nil
	}
	prompt := r.prompt.render(ev)
	if prompt == "" {
		return fmt.Errorf("invoke_llm[%s]: rendered prompt is empty", r.name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.llmTimeout)
	defer cancel()

	out, err := r.llm.Complete(ctx, prompt)
	if err != nil {
		return fmt.Errorf("invoke_llm[%s]: LLM call: %w", r.name, err)
	}
	if out == "" {
		return fmt.Errorf("invoke_llm[%s]: LLM returned empty output", r.name)
	}

	if err := r.output.dispatch(ev, out); err != nil {
		return fmt.Errorf("invoke_llm[%s]: output dispatch: %w", r.name, err)
	}
	return nil
}

// outputSink 是三种输出去向的统一接口。
type outputSink interface {
	dispatch(ev trace.Event, llmOutput string) error
}

// ── write_file sink ───────────────────────────────────────────────────

type writeFileSinkImpl struct {
	pathTpl     string // 含 ${event.x} 模板
	baseDir     string // 相对路径基准：优先 ProjectRoot，否则 reactors.yaml 所在目录
	projectRoot string // 渲染后路径 confinement 检查的根
}

func (s *writeFileSinkImpl) dispatch(ev trace.Event, llmOutput string) error {
	rendered := renderTemplate(s.pathTpl, ev)
	if rendered == "" {
		return fmt.Errorf("write_file: rendered path is empty (template=%q)", s.pathTpl)
	}
	if !filepath.IsAbs(rendered) && s.baseDir != "" {
		rendered = filepath.Join(s.baseDir, rendered)
	}
	abs, err := filepath.Abs(rendered)
	if err != nil {
		return fmt.Errorf("write_file: resolve path %q: %w", rendered, err)
	}
	if s.projectRoot != "" {
		if err := ensurePathUnderRoot(abs, s.projectRoot); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("write_file: mkdir: %w", err)
	}
	if s.projectRoot != "" {
		// 目录创建后再检查一次，覆盖 parent 中已有符号链接或并发替换的情况。
		if err := ensurePathUnderRoot(abs, s.projectRoot); err != nil {
			return err
		}
	}
	if err := os.WriteFile(abs, []byte(llmOutput), 0o644); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	return nil
}

func ensurePathUnderRoot(absPath, projectRoot string) error {
	canonRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return fmt.Errorf("write_file: canonicalize project root: %w", err)
	}
	existingParent, canonParent, err := canonicalExistingParent(filepath.Dir(absPath))
	if err != nil {
		return err
	}
	relParent, err := filepath.Rel(canonRoot, canonParent)
	if err != nil || strings.HasPrefix(relParent, "..") || filepath.IsAbs(relParent) {
		return fmt.Errorf("write_file: path %q is outside project root %q", absPath, projectRoot)
	}
	remaining, err := filepath.Rel(existingParent, absPath)
	if err != nil {
		return fmt.Errorf("write_file: resolve relative path: %w", err)
	}
	relPath, err := filepath.Rel(canonRoot, filepath.Join(canonParent, remaining))
	if err != nil || strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
		return fmt.Errorf("write_file: path %q is outside project root %q", absPath, projectRoot)
	}
	return nil
}

func canonicalExistingParent(parent string) (string, string, error) {
	for {
		if _, err := os.Stat(parent); err == nil {
			canon, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return "", "", fmt.Errorf("write_file: canonicalize path: %w", err)
			}
			return parent, canon, nil
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("write_file: stat path: %w", err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", "", fmt.Errorf("write_file: no existing parent for %q", parent)
		}
		parent = next
	}
}

// ── send_message sink ─────────────────────────────────────────────────

type sendMessageSinkImpl struct {
	toTpl    string
	msgType  string
	priority string
	sender   MailboxSender
}

func (s *sendMessageSinkImpl) dispatch(ev trace.Event, llmOutput string) error {
	if s.sender == nil {
		return fmt.Errorf("send_message: mailbox not configured")
	}
	to := renderTemplate(s.toTpl, ev)
	if to == "" {
		return fmt.Errorf("send_message: rendered 'to' is empty")
	}
	msg := mailbox.Message{
		From:     "reactor:" + s.msgType,
		To:       to,
		Content:  llmOutput,
		Summary:  truncateForSummary(llmOutput, 120),
		Type:     s.msgType,
		Priority: s.priority,
		SentAt:   time.Now(),
	}
	return s.sender.Send(msg)
}

func truncateForSummary(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ── emit_trace sink ───────────────────────────────────────────────────

type emitTraceSinkImpl struct {
	kind    trace.EventKind
	emitter TraceEmitter
}

func (s *emitTraceSinkImpl) dispatch(ev trace.Event, llmOutput string) error {
	out := trace.Event{
		Kind:        s.kind,
		TaskID:      ev.TaskID,
		AgentID:     ev.AgentID,
		Description: llmOutput,
	}
	s.emitter.Emit(out)
	return nil
}
