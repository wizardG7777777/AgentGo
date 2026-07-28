package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/trace"
)

// judgeMetrics 构造判据测试用指标。
func judgeMetrics() *RunMetrics {
	return &RunMetrics{
		TerminalStatus:    "completed",
		AcceptanceRounds:  2,
		AcceptanceVerdict: "pass",
		Replans:           1,
		EventCounts:       map[string]int{"workspace_merged": 2},
	}
}

func TestJudge_TaskCompleted(t *testing.T) {
	res := RunJudges([]JudgeSpec{{Type: "task_completed"}}, t.TempDir(), judgeMetrics())
	if !res[0].Passed {
		t.Fatalf("completed 应通过: %s", res[0].Detail)
	}
	m := judgeMetrics()
	m.TerminalStatus = "timeout"
	res = RunJudges([]JudgeSpec{{Type: "task_completed"}}, t.TempDir(), m)
	if res[0].Passed || !strings.Contains(res[0].Detail, "timeout") {
		t.Fatalf("timeout 应失败并带终态: %+v", res[0])
	}
}

func TestJudge_FileJudges(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2-agentA\nline3\n"
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])

	m := judgeMetrics()
	res := RunJudges([]JudgeSpec{
		{Type: "file_exists", Path: "a.txt"},
		{Type: "file_exists", Path: "不存在.txt"},
		{Type: "file_contains", Path: "a.txt", Pattern: "line2-agentA"},
		{Type: "file_contains", Path: "a.txt", Pattern: "line9"},
		{Type: "file_contains", Path: "a.txt", Pattern: `line\d`, Regex: true},
		{Type: "file_hash", Path: "a.txt", SHA256: hash},
		{Type: "file_hash", Path: "a.txt", SHA256: "deadbeef"},
		{Type: "file_min_bytes", Path: "a.txt", Min: f64(10)},
		{Type: "file_min_bytes", Path: "a.txt", Min: f64(99999)},
	}, dir, m)

	want := []bool{true, false, true, false, true, true, false, true, false}
	for i, w := range want {
		if res[i].Passed != w {
			t.Errorf("判据 %d（%s）= %v，期望 %v: %s", i, res[i].Spec.Type, res[i].Passed, w, res[i].Detail)
		}
	}
}

func TestJudge_FileHashTrimTrailingBlank(t *testing.T) {
	dir := t.TempDir()
	normalized := "line1\nline2-agentA\nline3\n"
	sum := sha256.Sum256([]byte(normalized))
	hash := hex.EncodeToString(sum[:])
	// 盘上文件多一个尾部换行（全量首跑的真实翻车形态）
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(normalized+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := judgeMetrics()

	strict := RunJudges([]JudgeSpec{{Type: "file_hash", Path: "a.txt", SHA256: hash}}, dir, m)
	if strict[0].Passed {
		t.Errorf("未开归一时尾部多换行应不匹配")
	}
	relaxed := RunJudges([]JudgeSpec{{Type: "file_hash", Path: "a.txt", SHA256: hash, TrimTrailingBlank: true}}, dir, m)
	if !relaxed[0].Passed {
		t.Errorf("开归一后应匹配: %s", relaxed[0].Detail)
	}

	// 行级内容的实质差异不得被归一放过
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\nline2-WRONG\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tampered := RunJudges([]JudgeSpec{{Type: "file_hash", Path: "a.txt", SHA256: hash, TrimTrailingBlank: true}}, dir, m)
	if tampered[0].Passed {
		t.Errorf("内容被篡改时归一不得放行")
	}
}

func TestJudge_AcceptancePass(t *testing.T) {
	m := judgeMetrics()
	res := RunJudges([]JudgeSpec{{Type: "acceptance_pass"}}, t.TempDir(), m)
	if !res[0].Passed {
		t.Fatalf("verdict=pass 应通过: %s", res[0].Detail)
	}
	m.AcceptanceVerdict = "fail"
	res = RunJudges([]JudgeSpec{{Type: "acceptance_pass"}}, t.TempDir(), m)
	if res[0].Passed || !strings.Contains(res[0].Detail, "fail") {
		t.Fatalf("verdict=fail 应失败: %+v", res[0])
	}
	m.AcceptanceRounds = 0
	res = RunJudges([]JudgeSpec{{Type: "acceptance_pass"}}, t.TempDir(), m)
	if res[0].Passed || !strings.Contains(res[0].Detail, "未发生") {
		t.Fatalf("无验收 run 应失败并说明: %+v", res[0])
	}
}

func TestJudge_EventCountAndAbsent(t *testing.T) {
	m := judgeMetrics()
	res := RunJudges([]JudgeSpec{
		{Type: "event_count", Kind: "workspace_merged", Min: f64(2)},
		{Type: "event_count", Kind: "workspace_merged", Min: f64(3)},
		{Type: "event_absent", Kind: "workspace_merge_conflict"},
		{Type: "event_absent", Kind: "workspace_merged"},
	}, t.TempDir(), m)
	want := []bool{true, false, true, false}
	for i, w := range want {
		if res[i].Passed != w {
			t.Errorf("判据 %d = %v，期望 %v: %s", i, res[i].Passed, w, res[i].Detail)
		}
	}
}

func TestJudge_MetricBounds(t *testing.T) {
	m := judgeMetrics()
	m.PromptTokens, m.CompletionTokens = 100, 50
	res := RunJudges([]JudgeSpec{
		{Type: "metric_bounds", Metric: "total_tokens", Max: f64(200)},
		{Type: "metric_bounds", Metric: "total_tokens", Max: f64(120)},
		{Type: "metric_bounds", Metric: "replans", Min: f64(1)},
		{Type: "metric_bounds", Metric: "不存在的指标", Max: f64(1)},
	}, t.TempDir(), m)
	want := []bool{true, false, true, false}
	for i, w := range want {
		if res[i].Passed != w {
			t.Errorf("判据 %d = %v，期望 %v: %s", i, res[i].Passed, w, res[i].Detail)
		}
	}
}

// f64 是 float64 指针的字面量助手。
func f64(v float64) *float64 { return &v }

func TestHarvestEvents(t *testing.T) {
	events := []trace.Event{
		{Kind: trace.KindTaskPublished, EventType: "__scheduler__"},
		{Kind: trace.KindTaskPublished, EventType: ""},
		{Kind: trace.KindTaskPublished, EventType: ""},
		{Kind: trace.KindLLMCallStart, AgentID: "worker-1", TaskID: "t1", Loop: 3},
		{Kind: trace.KindLLMCallStart, AgentID: "worker-1", TaskID: "t1", Loop: 7},
		{Kind: trace.KindLLMCallStart, AgentID: "worker-2", TaskID: "t2", Loop: 5},
		{Kind: trace.KindTaskCompleted, LoopsUsed: 7}, // 发射端当前不填，不应计入
		{Kind: trace.KindTaskFailed},
		{Kind: trace.KindError},
		{Kind: trace.KindReplanRequested},
		{Kind: trace.KindReplanRequested},
		{Kind: trace.KindAcceptanceCompleted, Acceptance: &trace.AcceptanceTraceContext{Verdict: "fail"}},
		{Kind: trace.KindAcceptanceCompleted, Acceptance: &trace.AcceptanceTraceContext{Verdict: "pass"}},
		{Kind: trace.KindLLMCallEnd, PromptTokens: 10, CompletionTokens: 5},
		{Kind: trace.KindLLMCallEnd, PromptTokens: 20, CompletionTokens: 7},
	}
	m := &RunMetrics{}
	HarvestEvents(events, m)
	if m.Subtasks != 2 {
		t.Errorf("Subtasks = %d，期望 2（根任务不计入）", m.Subtasks)
	}
	if m.Loops != 12 {
		t.Errorf("Loops = %d，期望 12（7+5，按 agent/task 最大 loop 求和）", m.Loops)
	}
	if m.Replans != 2 {
		t.Errorf("Replans = %d，期望 2", m.Replans)
	}
	if m.AcceptanceRounds != 2 || m.AcceptanceVerdict != "pass" {
		t.Errorf("验收 = %d 轮 %q，期望 2 轮 pass（取最后一次）", m.AcceptanceRounds, m.AcceptanceVerdict)
	}
	if m.Errors != 2 {
		t.Errorf("Errors = %d，期望 2", m.Errors)
	}
	if m.LLMCalls != 2 {
		t.Errorf("LLMCalls = %d，期望 2", m.LLMCalls)
	}
	if m.EventCounts["replan_requested"] != 2 {
		t.Errorf("EventCounts[replan_requested] = %d，期望 2", m.EventCounts["replan_requested"])
	}

	// token 兜底：snapshot 缺失时从 llm_call_end 求和
	SnapshotTokenFallback(events, m)
	if m.PromptTokens != 30 || m.CompletionTokens != 12 {
		t.Errorf("token 兜底 = %d+%d，期望 30+12", m.PromptTokens, m.CompletionTokens)
	}
}

func TestCollectTraceEvents(t *testing.T) {
	root := t.TempDir()
	logsDir := filepath.Join(root, ".agentgo", "sessions", "sess-1", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"ts":"2026-07-28T01:00:00Z","kind":"task_completed","task_id":"t1","loops_used":3}` + "\n"
	if err := os.WriteFile(filepath.Join(logsDir, "x_t1.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	events := CollectTraceEvents(root)
	if len(events) != 1 || events[0].Kind != trace.KindTaskCompleted || events[0].LoopsUsed != 3 {
		t.Fatalf("收割结果不符: %+v", events)
	}
}
