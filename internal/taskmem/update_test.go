package taskmem

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestApplyTurn_FileWrittenConfirmed：file_written 事实更新文件版本（confirmed
// 证据链：结构化 FileWrittenFact 输入），同路径覆盖更新且空 hash 不覆盖精确 hash。
func TestApplyTurn_FileWrittenConfirmed(t *testing.T) {
	m := New("task-1")

	if !ApplyTurn(m, TurnFacts{FilesWritten: []FileWrittenFact{{Path: "a.go", Hash: "hash-1"}}}) {
		t.Fatal("首次 file_written 应有实质变化")
	}
	if m.Version != 2 {
		t.Fatalf("Version = %d, want 2", m.Version)
	}
	if len(m.Files) != 1 || m.Files[0].Path != "a.go" || m.Files[0].Hash != "hash-1" {
		t.Fatalf("Files = %+v, want a.go/hash-1", m.Files)
	}

	// edit_file 后续写入：空 hash 保留旧 hash，UpdatedAt 刷新。
	before := m.Files[0].UpdatedAt
	time.Sleep(time.Millisecond)
	if !ApplyTurn(m, TurnFacts{FilesWritten: []FileWrittenFact{{Path: "a.go"}}}) {
		t.Fatal("同路径再次写入应有实质变化")
	}
	if len(m.Files) != 1 {
		t.Fatalf("同路径应覆盖更新而非追加, Files = %+v", m.Files)
	}
	if m.Files[0].Hash != "hash-1" {
		t.Errorf("空 hash 不应覆盖精确 hash, got %q", m.Files[0].Hash)
	}
	if !m.Files[0].UpdatedAt.After(before) {
		t.Error("UpdatedAt 应刷新")
	}
}

// TestApplyTurn_ShellNonZeroFailure：run_shell 工具成功但退出码非零 → 失败尝试。
func TestApplyTurn_ShellNonZeroFailure(t *testing.T) {
	m := New("task-1")
	exit := 2
	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{
		{Name: "run_shell", Target: "go test ./...", Success: true, ExitCode: &exit},
	}})
	if len(m.Failures) != 1 || !strings.Contains(m.Failures[0], "exit=2") {
		t.Fatalf("Failures = %+v, want 含 exit=2 的一条", m.Failures)
	}
	if len(m.Actions) != 0 {
		t.Errorf("失败命令不应进 Actions, got %+v", m.Actions)
	}

	// 同命令 exit=0 → 成功动作，带 exit 标注。
	zero := 0
	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{
		{Name: "run_shell", Target: "go test ./...", Success: true, ExitCode: &zero},
	}})
	if len(m.Actions) != 1 || !strings.Contains(m.Actions[0].Caption, "exit=0") {
		t.Fatalf("Actions = %+v, want 含 exit=0 的一条", m.Actions)
	}
	if m.Actions[0].Evidence.Kind != EvidenceShell {
		t.Errorf("shell 动作证据 Kind = %q, want %q", m.Actions[0].Evidence.Kind, EvidenceShell)
	}
}

// TestApplyTurn_ModelClaimWithoutEvidence：无证据输入即无 Fact——模型文本声称
// （"文件已改"、"测试通过"）不产生任何 Fact；只有 UserDecision（用户证据）
// 自动成为 confirmed Fact。
func TestApplyTurn_ModelClaimWithoutEvidence(t *testing.T) {
	m := New("task-1")
	// 空 TurnFacts（模型只输出了文本声称，无任何结构化事实）。
	if ApplyTurn(m, TurnFacts{}) {
		t.Fatal("空 TurnFacts 不应有实质变化")
	}
	if len(m.Facts) != 0 {
		t.Fatalf("无证据输入不应落 Facts, got %+v", m.Facts)
	}
	if m.Version != 1 {
		t.Errorf("无变化不应调版本, Version = %d, want 1", m.Version)
	}

	// 用户决定是唯一自动 confirmed 的 Fact 生产方。
	if !ApplyTurn(m, TurnFacts{UserDecision: "用方案 B 重写"}) {
		t.Fatal("用户决定应有实质变化")
	}
	if len(m.Facts) != 1 || !m.Facts[0].Confirmed || !strings.Contains(m.Facts[0].Text, "用方案 B 重写") {
		t.Fatalf("Facts = %+v, want 一条 confirmed 用户决定", m.Facts)
	}
	if m.Facts[0].Evidence[0].Kind != EvidenceUser {
		t.Errorf("用户决定证据 Kind = %q, want %q", m.Facts[0].Evidence[0].Kind, EvidenceUser)
	}
	// 同一决定重复输入去重。
	ApplyTurn(m, TurnFacts{UserDecision: "用方案 B 重写"})
	if len(m.Facts) != 1 {
		t.Errorf("重复用户决定应去重, Facts = %+v", m.Facts)
	}
}

// TestApplyTurn_RepeatedReadNoNewAction：重复读取同一目标不产生新 Action；
// 无实质变化的轮次不调版本。
func TestApplyTurn_RepeatedReadNoNewAction(t *testing.T) {
	m := New("task-1")
	read := ToolCallFact{Name: "read_file", Target: "a.go", Success: true}

	if !ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{read}}) {
		t.Fatal("首次读取应有实质变化")
	}
	if len(m.Actions) != 1 {
		t.Fatalf("Actions = %+v, want 1 条", m.Actions)
	}
	v := m.Version

	if ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{read}}) {
		t.Error("重复读取同一目标不应有实质变化")
	}
	if len(m.Actions) != 1 || m.Version != v {
		t.Errorf("重复读取后 Actions=%d Version=%d, want 1/%d", len(m.Actions), m.Version, v)
	}

	// 不同目标是新动作。
	if !ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{{Name: "read_file", Target: "b.go", Success: true}}}) {
		t.Error("读取新目标应有实质变化")
	}
	if len(m.Actions) != 2 {
		t.Errorf("Actions = %d, want 2", len(m.Actions))
	}
}

// TestApplyTurn_NoChangeNoVersionNoSave：无新增证据（重复读取 + 重复 artifact）
// 的轮次整体无变化。
func TestApplyTurn_NoChangeNoVersionNoSave(t *testing.T) {
	m := New("task-1")
	ApplyTurn(m, TurnFacts{NewArtifacts: []string{"out/report.md"}})
	v := m.Version
	if ApplyTurn(m, TurnFacts{NewArtifacts: []string{"out/report.md"}}) {
		t.Error("重复 artifact 登记不应有实质变化")
	}
	if m.Version != v {
		t.Errorf("Version = %d, want %d（未变化）", m.Version, v)
	}
}

// TestApplyTurn_BlockerSupersede：Gate 拒绝/Roster 占用进 Blockers；同工具
// 同目标成功后阻塞被 supersede 清除。
func TestApplyTurn_BlockerSupersede(t *testing.T) {
	m := New("task-1")
	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{
		{Name: "write_file", Target: "x.go", Success: false, Err: "[拒绝] 原因码=exec_mode_readonly retryable=false 说明=readonly 模式"},
	}})
	if len(m.Blockers) != 1 || !strings.Contains(m.Blockers[0], "write_file x.go") {
		t.Fatalf("Blockers = %+v, want 一条 write_file x.go", m.Blockers)
	}
	if len(m.Failures) != 1 {
		t.Fatalf("失败同时应进 Failures, got %+v", m.Failures)
	}

	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{
		{Name: "write_file", Target: "x.go", Success: true},
	}})
	if len(m.Blockers) != 0 {
		t.Errorf("同目标成功后阻塞应被清除, Blockers = %+v", m.Blockers)
	}
}

// TestApplyTurn_FailureDedup：连续同形失败去重；间隔重现仍计为新失败。
func TestApplyTurn_FailureDedup(t *testing.T) {
	m := New("task-1")
	fail := ToolCallFact{Name: "web_fetch", Target: "http://x", Success: false, Err: "超时"}
	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{fail}})
	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{fail}})
	if len(m.Failures) != 1 {
		t.Fatalf("连续同形失败应去重, Failures = %+v", m.Failures)
	}
	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{{Name: "read_file", Target: "a", Success: true}}})
	ApplyTurn(m, TurnFacts{ToolCalls: []ToolCallFact{fail}})
	if len(m.Failures) != 2 {
		t.Errorf("间隔重现的同形失败应计为新失败, Failures = %+v", m.Failures)
	}
}

// TestEnforceBudgets_FactsEviction：Facts 超限时 inferred+最旧先汰，
// confirmed+最近保留；段不上溢。
func TestEnforceBudgets_FactsEviction(t *testing.T) {
	m := New("task-1")
	base := time.Now()
	// 填满：20 confirmed（较旧）+ 15 inferred（最新）= 35 > 30。
	for i := 0; i < 20; i++ {
		m.Facts = append(m.Facts, Fact{Text: fmt.Sprintf("confirmed-%02d", i), Confirmed: true, UpdatedAt: base.Add(time.Duration(i) * time.Second)})
	}
	for i := 0; i < 15; i++ {
		m.Facts = append(m.Facts, Fact{Text: fmt.Sprintf("inferred-%02d", i), Confirmed: false, UpdatedAt: base.Add(time.Duration(100+i) * time.Second)})
	}
	// 触发一次实质变化以执法预算。
	ApplyTurn(m, TurnFacts{UserDecision: "触发预算"})
	if len(m.Facts) > MaxFacts {
		t.Fatalf("Facts 超上限: %d > %d", len(m.Facts), MaxFacts)
	}
	inferredLeft, confirmedLeft := 0, 0
	for _, f := range m.Facts {
		if f.Confirmed {
			confirmedLeft++
		} else {
			inferredLeft++
		}
	}
	// 35+1=36 → 淘汰 6 条：全部应来自 inferred 最旧侧。
	if inferredLeft != 15-6 {
		t.Errorf("inferred 剩 %d 条, want 9（最旧先汰）", inferredLeft)
	}
	if confirmedLeft != 21 {
		t.Errorf("confirmed 剩 %d 条, want 21（最近优先保留）", confirmedLeft)
	}
	// 最旧的 inferred-00 应已被淘汰。
	for _, f := range m.Facts {
		if f.Text == "inferred-00" {
			t.Error("最旧 inferred 应被淘汰")
		}
	}
}

// TestEnforceBudgets_SectionsBounded：各段硬上限（Actions/Failures/Files/
// Blockers/Next）不上溢；Files 最久未更新先汰。
func TestEnforceBudgets_SectionsBounded(t *testing.T) {
	m := New("task-1")
	base := time.Now()
	for i := 0; i < MaxFiles+5; i++ {
		m.Files = append(m.Files, FileVersion{Path: fmt.Sprintf("f%02d.go", i), UpdatedAt: base.Add(time.Duration(i) * time.Second)})
	}
	for i := 0; i < MaxBlockers+3; i++ {
		m.Blockers = append(m.Blockers, fmt.Sprintf("blocker-%02d", i))
	}
	for i := 0; i < MaxNextCandidates+3; i++ {
		m.NextCandidates = append(m.NextCandidates, fmt.Sprintf("next-%02d", i))
	}
	// 通过工具调用灌入 Actions 与 Failures（走公开入口）。
	facts := TurnFacts{}
	for i := 0; i < MaxActions+10; i++ {
		facts.ToolCalls = append(facts.ToolCalls, ToolCallFact{Name: "write_file", Target: fmt.Sprintf("w%02d.go", i), Success: true})
	}
	for i := 0; i < MaxFailures+5; i++ {
		facts.ToolCalls = append(facts.ToolCalls, ToolCallFact{Name: "web_fetch", Target: fmt.Sprintf("u%02d", i), Success: false, Err: "e"})
	}
	ApplyTurn(m, facts)

	if len(m.Actions) != MaxActions {
		t.Errorf("Actions = %d, want %d", len(m.Actions), MaxActions)
	}
	if len(m.Failures) != MaxFailures {
		t.Errorf("Failures = %d, want %d", len(m.Failures), MaxFailures)
	}
	if len(m.Files) != MaxFiles {
		t.Errorf("Files = %d, want %d", len(m.Files), MaxFiles)
	}
	if len(m.Blockers) != MaxBlockers {
		t.Errorf("Blockers = %d, want %d", len(m.Blockers), MaxBlockers)
	}
	if len(m.NextCandidates) != MaxNextCandidates {
		t.Errorf("NextCandidates = %d, want %d", len(m.NextCandidates), MaxNextCandidates)
	}
	// Actions 滚动保留最近：最早的 w00 应已淘汰，最新的 w29 应在。
	if m.Actions[0].Caption == "write_file w00.go" {
		t.Error("Actions 应淘汰最旧条目")
	}
	last := m.Actions[len(m.Actions)-1].Caption
	if !strings.Contains(last, "w29.go") {
		t.Errorf("Actions 最新条目 = %q, want 含 w29.go", last)
	}
	// Files 最久未更新（f00）先汰。
	for _, f := range m.Files {
		if f.Path == "f00.go" {
			t.Error("最久未更新的文件版本应被淘汰")
		}
	}
}

// TestApplyTurn_SealedRejectsUpdate：终态封存后拒绝滚动更新。
func TestApplyTurn_SealedRejectsUpdate(t *testing.T) {
	m := New("task-1")
	m.Sealed = true
	if ApplyTurn(m, TurnFacts{UserDecision: "迟到的事实"}) {
		t.Error("Sealed 后不应再更新")
	}
	if len(m.Facts) != 0 {
		t.Errorf("Sealed 后 Facts 不应增长, got %+v", m.Facts)
	}
}

// ApplyAttemptEnd（2026-08-20 SWE-001 预防 3）：attempt 终止原因有界入 Failures
// 尾部，重试接手可见；同形同因去重，Sealed 拒写。
func TestApplyAttemptEnd(t *testing.T) {
	m := New("t-attempt")
	v0 := m.Version
	if !ApplyAttemptEnd(m, "流式 tool call 参数解析失败（载荷 30124 字符）") {
		t.Fatal("首次写入应生效")
	}
	if m.Version != v0+1 || len(m.Failures) != 1 ||
		!strings.Contains(m.Failures[0], "attempt 终止") || !strings.Contains(m.Failures[0], "载荷 30124") {
		t.Fatalf("终止原因未按预期入账: %+v", m.Failures)
	}
	// 同形同因去重（不刷屏、不调版本）
	if ApplyAttemptEnd(m, "流式 tool call 参数解析失败（载荷 30124 字符）") {
		t.Fatal("相同终止原因应去重")
	}
	if len(m.Failures) != 1 || m.Version != v0+1 {
		t.Fatalf("去重后不应变化: %+v", m)
	}
	// 不同原因计入新条目
	if !ApplyAttemptEnd(m, "context deadline exceeded") || len(m.Failures) != 2 {
		t.Fatalf("新终止原因应入账: %+v", m.Failures)
	}
	// Sealed 拒写
	m.Sealed = true
	if ApplyAttemptEnd(m, "不应写入") {
		t.Fatal("Sealed 后应拒绝更新")
	}
	// 空串不入账
	m.Sealed = false
	if ApplyAttemptEnd(m, "   ") {
		t.Fatal("空白原因不应入账")
	}
}
