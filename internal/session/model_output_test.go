package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentgo/internal/contextruntime"
	"agentgo/internal/llm"
)

func TestModelOutputPersistsCurrentResultAndRejectsOldLedger(t *testing.T) {
	manager, err := NewSessionManager(t.TempDir(), SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	id := manager.Current().ID
	result, err := llm.NewResult(llm.ResultData{Schema: llm.ResultSchema, Items: []llm.OutputItem{{Kind: llm.OutputItemMessage, Text: "完整输出"}}, FinishReason: llm.FinishReasonStop})
	if err != nil {
		t.Fatal(err)
	}
	record := contextruntime.OutputRecord{Schema: "agentgo.model-output/v1", Identity: llm.Identity{InvocationID: "invocation", SessionID: id, AgentID: "agent"}, Result: &result, Text: result.Content(), Status: "completed", StartedAt: time.Now(), CompletedAt: time.Now()}
	if err := manager.AppendModelOutput(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.LoadModelOutputs(id)
	if err != nil || len(loaded) != 1 || loaded[0].Result == nil || loaded[0].Result.Content() != "完整输出" {
		t.Fatalf("完整结果未持久化: %+v %v", loaded, err)
	}
	path := filepath.Join(manager.Current().Dir, turnLedgerFile)
	if err := os.WriteFile(path, []byte("{\"id\":\"旧轮次\",\"text\":\"旧正文\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.LoadModelOutputs(id); err == nil {
		t.Fatal("接受旧模型输出账本")
	}
}

func TestOldSessionSnapshotIsRejectedWithoutMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	raw := []byte(`{"version":5,"model_history_contract":"agentgo.model-history/v2","tasks":[]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(path); err == nil {
		t.Fatal("接受旧 Session 快照")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatal("拒绝旧快照时修改了磁盘内容")
	}
}
