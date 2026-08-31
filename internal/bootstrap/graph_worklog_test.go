// graph_worklog_test.go 覆盖上游工作记录的聚合渲染（2026-08-21 上游摘要）：
// 工具统计降序与有界、shell 退出码统计、文件清单去重与有界、无记录信号、
// provider 与 TaskStore 的接线。
package bootstrap

import (
	"strings"
	"testing"
	"time"

	"agentgo/internal/checkstore"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func recExit(name string, code int) store.ToolCallRecord {
	c := code
	return store.ToolCallRecord{ToolName: name, Success: code == 0, ExitCode: &c}
}

func TestRenderGraphWorkLogAggregates(t *testing.T) {
	recs := []store.ToolCallRecord{
		{ToolName: "read_file", Success: true},
		{ToolName: "read_file", Success: true},
		{ToolName: "read_file", Success: true},
		{ToolName: "edit_file", Success: true, Args: map[string]any{"path": "src/flask/ctx.py"}},
		{ToolName: "edit_file", Success: true, Args: map[string]any{"path": "src/flask/ctx.py"}}, // 重复路径去重
		{ToolName: "write_file", Success: true, Args: map[string]any{"path": "notes.md"}},
		recExit("run_shell", 0),
		recExit("run_shell", 1),
	}
	got := renderGraphWorkLog(recs)

	if !strings.Contains(got, "read_file×3") || !strings.Contains(got, "edit_file×2") {
		t.Errorf("工具统计应聚合计数: %q", got)
	}
	if !strings.Contains(got, "run_shell×2(ok=1 fail=1)") {
		t.Errorf("工作记录必须保留工具成功/失败计数: %q", got)
	}
	// 降序：read_file(3) 在 edit_file(2) 前
	if strings.Index(got, "read_file×3") > strings.Index(got, "edit_file×2") {
		t.Errorf("工具统计应按次数降序: %q", got)
	}
	if !strings.Contains(got, "exit≠0: 1") {
		t.Errorf("run_shell 应统计非零退出码: %q", got)
	}
	if !strings.Contains(got, "编辑文件: src/flask/ctx.py") || strings.Count(got, "ctx.py") != 1 {
		t.Errorf("编辑文件应去重: %q", got)
	}
	if !strings.Contains(got, "写入文件: notes.md") {
		t.Errorf("写入文件应列出: %q", got)
	}
}

func TestRenderGraphWorkLogIncludesTypedChecksAndSkipsFailedWrites(t *testing.T) {
	recs := []store.ToolCallRecord{
		{ToolName: "edit_file", Success: false, Args: map[string]any{"path": "bad.go"}},
		{ToolName: "write_file", Success: true, Args: map[string]any{"path": "good.go"}},
		{ToolName: "run_check", Success: true},
	}
	checks := []checkstore.Record{{CheckRef: "check:1", CheckID: "verification", Kind: "test",
		Status: checkstore.StatusFailed, ExitCode: 1, WorkspaceRevisionRef: "workspace:sha256:x"}}
	got := renderGraphWorkLogWithChecks(recs, checks)
	if strings.Contains(got, "编辑文件: bad.go") || !strings.Contains(got, "写入文件: good.go") {
		t.Fatalf("失败写入不得冒充文件变更: %q", got)
	}
	if !strings.Contains(got, "检查记录: latest_by_check_id=[verification/test status=failed exit=1") ||
		!strings.Contains(got, "superseded=0") {
		t.Fatalf("typed CheckRecord 未进入工作记录: %q", got)
	}
}

func TestRenderGraphWorkLogSupersededFailureDoesNotMasqueradeAsCurrent(t *testing.T) {
	base := time.Now().UTC()
	checks := []checkstore.Record{
		{CheckID: "verification", Kind: "test", Status: checkstore.StatusFailed,
			ExitCode: 1, WorkspaceRevisionRef: "workspace:empty", SettledAt: base},
		{CheckID: "verification", Kind: "verification", Status: checkstore.StatusPass,
			ExitCode: 0, WorkspaceRevisionRef: "workspace:sha256:candidate", SettledAt: base.Add(time.Second)},
	}
	got := renderGraphWorkLogWithChecks(nil, checks)
	if !strings.Contains(got, "latest_by_check_id=[verification/verification status=pass exit=0 workspace=workspace:sha256:candidate]") ||
		!strings.Contains(got, "superseded=1") || strings.Contains(got, "failed=1") {
		t.Fatalf("被后续 pass 覆盖的历史失败不得冒充当前结论: %q", got)
	}
}

func TestRenderGraphWorkLogEmptySignals(t *testing.T) {
	if got := renderGraphWorkLog(nil); got != "（无调用记录）" {
		t.Fatalf("无记录应给出强信号文本: %q", got)
	}
}

func TestRenderGraphWorkLogBounds(t *testing.T) {
	// 10 类工具：超出 8 类上限应标注剩余类别数
	recs := make([]store.ToolCallRecord, 0, 30)
	for _, name := range []string{"t01", "t02", "t03", "t04", "t05", "t06", "t07", "t08", "t09", "t10"} {
		recs = append(recs, store.ToolCallRecord{ToolName: name, Success: true})
	}
	got := renderGraphWorkLog(recs)
	if !strings.Contains(got, "(+2 类工具)") {
		t.Errorf("超上限应标注剩余类别: %q", got)
	}

	// 12 个写入文件：超出 10 条上限应标注 (+2 more)
	recs = recs[:0]
	for i := 0; i < 12; i++ {
		recs = append(recs, store.ToolCallRecord{
			ToolName: "write_file", Success: true,
			Args: map[string]any{"path": strings.Repeat("x", 1) + strings.Repeat("f", 2) + string(rune('a'+i)) + ".py"},
		})
	}
	got = renderGraphWorkLog(recs)
	if !strings.Contains(got, "(+2 more)") {
		t.Errorf("文件清单超上限应标注 (+N more): %q", got)
	}
}

func TestGraphWorkLogProviderWithStore(t *testing.T) {
	ch := make(chan model.Event, 16)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	provider := newGraphWorkLogProvider(s)

	if got := provider(""); got != "" {
		t.Errorf("空 taskID 应返回空串: %q", got)
	}
	if got := provider("task-absent"); got != "（无调用记录）" {
		t.Errorf("无记录任务应返回强信号: %q", got)
	}
	if err := s.PublishTask(&model.Task{ID: "task-1", Description: "来源任务", EventType: "work"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendToolCall("task-1", store.ToolCallRecord{ToolName: "read_file", Success: true}); err != nil {
		t.Fatal(err)
	}
	if got := provider("task-1"); !strings.Contains(got, "read_file×1") {
		t.Errorf("provider 应聚合账本记录: %q", got)
	}
	// 畸形工具名经账本清洗后，摘要吃到的是占位名（SWE-002 链路一致性）
	if err := s.AppendToolCall("task-1", store.ToolCallRecord{ToolName: "grep>\n<｜DSML｜x", Success: false}); err != nil {
		t.Fatal(err)
	}
	got := provider("task-1")
	if strings.Contains(got, "DSML") || !strings.Contains(got, "malformed:") {
		t.Errorf("畸形名应已清洗为占位: %q", got)
	}
}
