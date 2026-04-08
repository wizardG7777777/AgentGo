package trace

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PromptDumper 把每次 LLM 调用的完整 messages 写入独立文件，供排查"幻觉"问题。
//
// 为什么单独一个文件而不是合并到 trace 文件：
//   - 完整 prompt + response 会让 trace 文件膨胀 10-50 倍
//   - prompt dump 是 opt-in 的（--dump-prompts），用户主动启用时才有
//   - 单独文件方便用 jq 或 vim 查看，不污染主 trace
//
// 文件命名：与对应任务的 trace 文件同时间戳，但后缀为 .prompts.jsonl
//
//	2026-04-08T04-17-06_321b561d.prompts.jsonl
//
// 每行一个 JSON 对象，含 phase=request 或 phase=response。
type PromptDumper struct {
	mu      sync.Mutex
	dir     string
	files   map[string]*os.File // taskID → 已打开文件
	enabled bool
}

// NewPromptDumper 创建一个 prompt dumper。enabled=false 时所有 Dump 调用都是 no-op。
// dir 应当与 Writer 的 dir 相同（trace 和 prompts 文件并排存放）。
func NewPromptDumper(dir string, enabled bool) (*PromptDumper, error) {
	if enabled {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("创建 prompt dump 目录失败: %w", err)
		}
	}
	return &PromptDumper{
		dir:     dir,
		files:   make(map[string]*os.File),
		enabled: enabled,
	}, nil
}

// Enabled 返回 dumper 是否处于启用状态。
func (p *PromptDumper) Enabled() bool { return p != nil && p.enabled }

// DumpRequest 写入一次 LLM 请求的完整 messages。
func (p *PromptDumper) DumpRequest(taskID string, loop int, ts time.Time, messages any, toolsCount int) {
	if p == nil || !p.enabled || taskID == "" {
		return
	}
	p.write(taskID, ts, map[string]any{
		"ts":          ts.Format(time.RFC3339Nano),
		"task_id":     taskID,
		"loop":        loop,
		"phase":       "request",
		"messages":    messages,
		"tools_count": toolsCount,
	})
}

// DumpResponse 写入一次 LLM 响应的完整内容。
func (p *PromptDumper) DumpResponse(taskID string, loop int, ts time.Time, content string, toolCalls any, promptTokens, completionTokens int) {
	if p == nil || !p.enabled || taskID == "" {
		return
	}
	p.write(taskID, ts, map[string]any{
		"ts":                ts.Format(time.RFC3339Nano),
		"task_id":           taskID,
		"loop":              loop,
		"phase":             "response",
		"content":           content,
		"tool_calls":        toolCalls,
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
	})
}

// Close 关闭所有文件句柄。
func (p *PromptDumper) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.files {
		f.Close()
	}
	p.files = make(map[string]*os.File)
}

// CloseTask 关闭一个任务的文件句柄。
func (p *PromptDumper) CloseTask(taskID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, ok := p.files[taskID]; ok {
		f.Close()
		delete(p.files, taskID)
	}
}

func (p *PromptDumper) write(taskID string, ts time.Time, payload map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	f, ok := p.files[taskID]
	if !ok {
		shortID := taskID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		filename := fmt.Sprintf("%s_%s.prompts.jsonl", ts.UTC().Format("2006-01-02T15-04-05"), shortID)
		path := filepath.Join(p.dir, filename)
		var err error
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("[trace] WARNING: failed to open prompt dump file for task %s: %v", taskID, err)
			return
		}
		p.files[taskID] = f
	}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[trace] WARNING: failed to marshal prompt dump (task=%s): %v", taskID, err)
		return
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("[trace] WARNING: failed to write prompt dump (task=%s): %v", taskID, err)
	}
}

// --- 包级默认 PromptDumper ---

var defaultDumper *PromptDumper

// SetDefaultDumper 设置包级默认 PromptDumper。
func SetDefaultDumper(d *PromptDumper) { defaultDumper = d }

// DefaultDumper 返回包级默认 PromptDumper。可能为 nil。
func DefaultDumper() *PromptDumper { return defaultDumper }

// DumpRequest 包级 helper。
func DumpRequest(taskID string, loop int, messages any, toolsCount int) {
	if defaultDumper != nil {
		defaultDumper.DumpRequest(taskID, loop, time.Now(), messages, toolsCount)
	}
}

// DumpResponse 包级 helper。
func DumpResponse(taskID string, loop int, content string, toolCalls any, promptTokens, completionTokens int) {
	if defaultDumper != nil {
		defaultDumper.DumpResponse(taskID, loop, time.Now(), content, toolCalls, promptTokens, completionTokens)
	}
}
