package store

// 工具名落库清洗（SWE-002 第二层防线 / SWE-003 残余①）的单测：
//   - 畸形工具名（DSML 泄漏、控制字符、超长）替换为确定性 malformed: 占位，
//     同一垃圾名同占位、不同垃圾名可区分，告警按 store 实例去重一次；
//   - 合法名逐字节不动；
//   - IsWellFormedToolName / IsToolNameCharsetLegal 的判定矩阵。

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// dsmlGarbageToolName 复现事故形状：模型把 DSML 标记泄进 tool_call 名字段，
// 产生 200+ 字符、含换行与 < > | 标记字符的畸形「工具名」。
func dsmlGarbageToolName() string {
	return "run_shell>\n<｜DSML｜parameter name=\"command\" string=\"true\">" + strings.Repeat("x", 200)
}

func TestIsWellFormedToolNameMatrix(t *testing.T) {
	cases := []struct {
		name      string
		toolName  string
		wellFormed bool
	}{
		{"内置工具名", "run_shell", true},
		{"含冒号点线", "mcp__x.y:z-w", true},
		{"空串", "", false},
		{"数字开头", "3foo", false},
		{"下划线开头", "_foo", false},
		{"含换行", "run_shell\nxx", false},
		{"含空白", "run shell", false},
		{"含标记字符", "run_shell>|<x>", false},
		{"含 CJK", "工具", false},
		{"恰 64 rune", strings.Repeat("a", 64), true},
		{"超 64 rune", strings.Repeat("a", 65), false},
		{"DSML 垃圾名", dsmlGarbageToolName(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWellFormedToolName(tc.toolName); got != tc.wellFormed {
				t.Errorf("IsWellFormedToolName(%q) = %v，应为 %v", tc.toolName, got, tc.wellFormed)
			}
		})
	}
	// 字符形状判定不看长度：超长但形状合法仍 true（evidence kind 走截断分支）。
	if !IsToolNameCharsetLegal(strings.Repeat("a", 200)) {
		t.Error("IsToolNameCharsetLegal 对超长但形状合法的名字应为 true")
	}
	if IsToolNameCharsetLegal("") || IsToolNameCharsetLegal("1abc") {
		t.Error("IsToolNameCharsetLegal 对空串/非字母开头应为 false")
	}
}

func TestMalformedToolNamePlaceholderDeterministic(t *testing.T) {
	raw1 := dsmlGarbageToolName()
	raw2 := "edit_file>\n<｜DSML｜" + strings.Repeat("y", 150)
	p1, p1Again, p2 := MalformedToolNamePlaceholder(raw1), MalformedToolNamePlaceholder(raw1), MalformedToolNamePlaceholder(raw2)
	if p1 != p1Again {
		t.Errorf("同一垃圾名应得同一占位: %q vs %q", p1, p1Again)
	}
	if p1 == p2 {
		t.Errorf("不同垃圾名占位应可区分，均为 %q", p1)
	}
	if !strings.HasPrefix(p1, "malformed:") || len(p1) != len("malformed:")+12 {
		t.Errorf("占位形状应为 malformed:<12 hex>，实际 %q", p1)
	}
	if !IsWellFormedToolName(p1) {
		t.Errorf("占位自身必须合法（防二次命中清洗）: %q", p1)
	}
}

// TestAppendToolCallSanitizesMalformedToolName 验证落库清洗：垃圾名替换为
// 确定性占位、按 store 实例去重告警一次、账本索引键同步干净；合法名不变。
func TestAppendToolCallSanitizesMalformedToolName(t *testing.T) {
	s, _ := newTestStore(10, 100)
	task := publishTestTask(t, s, "清洗测试任务")

	var logBuf bytes.Buffer
	prevOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOutput)

	raw := dsmlGarbageToolName()
	rec := ToolCallRecord{CallID: "c-1", AgentID: "a-1", ToolName: raw, Success: true}
	if err := s.AppendToolCall(task.ID, rec); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	// 同一垃圾名再次落库：同占位，但不得二次告警。
	if err := s.AppendToolCall(task.ID, ToolCallRecord{CallID: "c-2", AgentID: "a-1", ToolName: raw, Success: false}); err != nil {
		t.Fatalf("AppendToolCall 第二次: %v", err)
	}

	calls, err := s.QueryToolCalls(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("应有 2 条调用记录，实际 %d", len(calls))
	}
	wantPlaceholder := MalformedToolNamePlaceholder(raw)
	for _, c := range calls {
		if c.ToolName != wantPlaceholder {
			t.Errorf("垃圾名应清洗为 %q，实际账本中 %q", wantPlaceholder, c.ToolName)
		}
	}
	// 二级索引键同步干净：按占位名可查询，按原始垃圾名查不到。
	if byName, _ := s.QueryToolCalls(task.ID, wantPlaceholder); len(byName) != 2 {
		t.Errorf("按占位名查询应得 2 条，实际 %d", len(byName))
	}
	if byRaw, _ := s.QueryToolCalls(task.ID, raw); len(byRaw) != 0 {
		t.Errorf("按原始垃圾名查询应为 0 条，实际 %d", len(byRaw))
	}

	warns := strings.Count(logBuf.String(), "畸形工具名")
	if warns != 1 {
		t.Errorf("同一垃圾名应只告警 1 次，实际 %d 次；日志：%s", warns, logBuf.String())
	}
	if strings.Contains(logBuf.String(), raw) {
		t.Error("告警日志不得包含原始垃圾名（防换行/控制字符污染日志）")
	}

	// 合法名逐字节不动，且不产生告警。
	logBuf.Reset()
	legal := ToolCallRecord{CallID: "c-3", AgentID: "a-1", ToolName: "run_shell", Args: map[string]any{"command": "ls"}, Success: true}
	if err := s.AppendToolCall(task.ID, legal); err != nil {
		t.Fatalf("AppendToolCall 合法名: %v", err)
	}
	byLegal, _ := s.QueryToolCalls(task.ID, "run_shell")
	if len(byLegal) != 1 || byLegal[0].ToolName != "run_shell" {
		t.Fatalf("合法名应逐字节保留: %+v", byLegal)
	}
	if logBuf.Len() != 0 {
		t.Errorf("合法名不应产生告警: %s", logBuf.String())
	}
}
