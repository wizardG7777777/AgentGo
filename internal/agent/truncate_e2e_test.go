package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// TestE2E_TruncateFiresOnContextLimit 端到端验证 §11.7.4 truncate 接通：
//
// 用真实的 processTask 主循环（不绕开 ReAct loop）+ fake executor 注入大 PromptTokens
// 的历史，驱动 ContextLimit 触发，最后从临时 trace 目录读回 JSONL 断言至少一条
// KindHistoryTruncated 事件——这是在单测层面能做到的最贴近"实际跑任务后 grep trace"
// 的闭环验证，关闭"S7 装配漏接"漏洞窗口（CLAUDE.md "Shipping conventions" 第 1 条）。
//
// 与 token_truncate_test.go 中纯函数级断言互补：
//   - 函数级测试断言 TruncateHistory 本身行为正确（输入超限 → 输出在限）
//   - 本端到端测试断言 processTask 主循环**真的会调** TruncateHistory（接通点不被回退）
//
// 故未来若有人误删 agent.go 中的 TruncateHistory 调用点：函数级测试仍然全绿（函数没变），
// 但本测试会因 trace 文件中找不到 KindHistoryTruncated 事件而红——抓住装配漏接。
func TestE2E_TruncateFiresOnContextLimit(t *testing.T) {
	s, r, _ := setup()

	// Step 1: 临时 trace 目录 + Writer，捕获本测试期间所有 trace.Emit 调用
	tmpDir := t.TempDir()
	w, err := trace.NewWriter(tmpDir, 100)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// 包级 defaultWriter 是全局共享变量；测试期间替换、结束后恢复
	originalDefault := trace.Default()
	trace.SetDefault(w)
	t.Cleanup(func() { trace.SetDefault(originalDefault) })

	// Step 2: 发布一个普通任务
	task := &model.Task{Description: "e2e truncate test", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}

	// Step 3: fake executor——精心调配数值让"真截断"可达（成功路径，predict 真的下降），
	// 而不是仅仅触发 ErrContextLimitTooSmall 退化路径（before == after）：
	//   - 第 1 轮：返回 PromptTokens=700 作为锚（assistant content 小，几乎不占额外 tokens）
	//   - 第 2~24 轮：返回 PromptTokens=0（不影响锚），每条 ~30 tokens（小内容）
	//   - 第 25 轮：返回 ToolCalled=false 终止
	//
	// 触发数学：predict = 700 + (loops_after_anchor) × 30。当 loops 累积到 ~10 时
	// predict ≈ 1000 = ContextLimit，loop 11 onwards 必然触发 truncate；history 长度足够
	// 让 protectedHead(1) + protectedTail(6) 之间还有可删的中间 entries，从而 predict 真的下降。
	const contextLimit = 1000
	const fakeModel = "fake-model"
	const maxLoops = 25
	loops := 0
	executor := func(ctx context.Context, task *model.Task, depResults map[string]string, history []HistoryEntry) (ExecuteResult, error) {
		loops++
		if loops >= maxLoops {
			return ExecuteResult{Output: "done", ToolCalled: false}, nil
		}
		// 第 1 轮（loops==1）当锚：PromptTokens=700
		// 后续轮：PromptTokens=0（让锚保持在 history 头部，让"after-anchor 累积"生效）
		var anchor int
		if loops == 1 {
			anchor = 700
		}
		return ExecuteResult{
			Output:           "fake tool output",
			ToolCalled:       true,
			AssistantContent: "x", // 1 char, ~0 tokens
			ToolCalls: []llm.ToolCall{
				{ID: "call-x", Name: "ft", Arguments: map[string]any{"k": "v"}},
			},
			ToolResults: []ToolResult{
				{ToolCallID: "call-x", Content: strings.Repeat("r", 60)}, // ~20 tokens
			},
			PromptTokens:     anchor,
			CompletionTokens: 10,
		}, nil
	}

	ag := NewAgent("agent-trunc-e2e", "code", s, r, executor, maxLoops+5)
	ag.PollInterval = 10 * time.Millisecond
	ag.Model = fakeModel
	ag.ContextLimit = contextLimit // 触发 truncate 的关键开关

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go ag.Run(ctx)

	// Step 4: 等待任务完成
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("等待任务完成超时")
		default:
		}
		got, gerr := s.GetTask(task.ID)
		if gerr != nil {
			t.Fatalf("GetTask: %v", gerr)
		}
		if got.Status == model.TaskStatusCompleted || got.Status == model.TaskStatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Flush trace writer 文件句柄，让磁盘内容可读
	if err := w.Close(); err != nil {
		t.Fatalf("trace.Writer.Close: %v", err)
	}

	// Step 5: 读取 tmpDir 下所有 .jsonl 文件，断言至少一条 history_truncated 事件
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", tmpDir, err)
	}
	truncatedSeen := false
	realShrinkSeen := false // 至少有一条事件 Before > After（成功路径，非 ErrContextLimitTooSmall 退化）
	totalEvents := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, ferr := os.Open(filepath.Join(tmpDir, e.Name()))
		if ferr != nil {
			t.Fatalf("Open: %v", ferr)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			totalEvents++
			var ev trace.Event
			if jerr := json.Unmarshal(scanner.Bytes(), &ev); jerr != nil {
				continue
			}
			if ev.Kind != trace.KindHistoryTruncated {
				continue
			}
			truncatedSeen = true
			if ev.KeptEntries <= 0 {
				t.Errorf("history_truncated.kept_entries 应当 > 0，实际 %d", ev.KeptEntries)
			}
			if ev.Strategy == "" {
				t.Errorf("history_truncated.strategy 不应为空")
			}
			if ev.PromptTokensBefore > ev.PromptTokensAfter {
				realShrinkSeen = true
			}
		}
		_ = f.Close()
	}

	if totalEvents == 0 {
		t.Fatalf("trace 目录 %s 下没有任何事件——可能 SetDefault 没生效", tmpDir)
	}

	if !truncatedSeen {
		t.Fatalf("trace 文件中未找到 KindHistoryTruncated 事件——这意味着 processTask 主循环没有调用 TruncateHistory，§11.7.4 装配漏接已回退！")
	}

	if !realShrinkSeen {
		// 所有 truncate 事件都是 Before==After（即都走 ErrContextLimitTooSmall 退化路径）。
		// 测试场景应当让真截断可达；不可达说明数值参数失调或 truncate 实现退化。
		t.Errorf("所有 history_truncated 事件 Before==After——预期至少一条事件 Before>After（成功路径），" +
			"否则说明截断函数总是落入 ErrContextLimitTooSmall 退化分支或 truncate 实际未减少 history 大小")
	}
}
