package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestTurnLedgerRoundTripAndSessionBoundary(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir(), SessionConfig{})
	if err != nil {
		t.Fatalf("创建 SessionManager 失败: %v", err)
	}
	firstID := sm.Current().ID
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	first := TurnRecord{
		ID: "turn-1", AgentID: "worker-1", TaskID: "task-1", Loop: 1,
		Text: "第一行\n第二行", Reasoning: "原始思考第一步\n原始思考第二步", Status: "completed",
		ToolCalls: []string{"read_file", "write_file"},
		StartedAt: at, CompletedAt: at.Add(time.Second),
	}
	if err := sm.AppendTurn(firstID, first); err != nil {
		t.Fatalf("追加首轮失败: %v", err)
	}

	secondSession, err := sm.CreateNew()
	if err != nil {
		t.Fatalf("创建第二个 Session 失败: %v", err)
	}
	failed := TurnRecord{
		ID: "turn-2", AgentID: "scheduler-1", TaskID: "task-2", Loop: 2,
		Text: "切换后仍写回原 Session", Status: "failed", Error: "模型中断",
		StartedAt: at.Add(2 * time.Second), CompletedAt: at.Add(3 * time.Second),
	}
	if err := sm.AppendTurn(firstID, failed); err != nil {
		t.Fatalf("切换后按显式 Session ID 追加失败: %v", err)
	}

	got, err := sm.LoadTurns(firstID)
	if err != nil {
		t.Fatalf("读取首个 Session 轮次失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("首个 Session 应保存两轮，实际为 %d: %+v", len(got), got)
	}
	if got[0].SessionID != firstID || got[1].SessionID != firstID {
		t.Fatalf("轮次归属错误: %+v", got)
	}
	if got[0].Text != first.Text || got[0].Reasoning != first.Reasoning ||
		!reflect.DeepEqual(got[0].ToolCalls, first.ToolCalls) {
		t.Fatalf("正文、思维链或工具名未原样恢复: %+v", got[0])
	}
	secondTurns, err := sm.LoadTurns(secondSession.ID)
	if err != nil {
		t.Fatalf("读取第二个 Session 失败: %v", err)
	}
	if len(secondTurns) != 0 {
		t.Fatalf("第二个 Session 不应混入旧边界轮次: %+v", secondTurns)
	}
}

func TestLoadTurnsSkipsCorruptedLines(t *testing.T) {
	sm, err := NewSessionManager(t.TempDir(), SessionConfig{})
	if err != nil {
		t.Fatalf("创建 SessionManager 失败: %v", err)
	}
	sessionID := sm.Current().ID
	if err := sm.AppendTurn(sessionID, TurnRecord{
		ID: "valid", AgentID: "worker-1", Loop: 1, Status: "completed",
	}); err != nil {
		t.Fatalf("写入有效轮次失败: %v", err)
	}
	path := filepath.Join(sm.baseDir, "sess-"+sessionID, turnLedgerFile)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开轮次账本失败: %v", err)
	}
	if _, err := f.WriteString("{broken-json\n"); err != nil {
		_ = f.Close()
		t.Fatalf("写入损坏行失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭轮次账本失败: %v", err)
	}

	got, err := sm.LoadTurns(sessionID)
	if err != nil {
		t.Fatalf("损坏行不应阻断恢复: %v", err)
	}
	if len(got) != 1 || got[0].ID != "valid" {
		t.Fatalf("有效轮次未恢复: %+v", got)
	}
}
