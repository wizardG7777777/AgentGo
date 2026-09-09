package contextruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
)

func TestDuplicateReadProjectionKeepsFileIdentityAndRawHistory(t *testing.T) {
	content, err := contentstore.Open(t.TempDir(), contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = content.Close() })
	var history []contextcontract.HistoryEntry
	for index, path := range []string{"a.go", "b.go", "a.go"} {
		id := string(rune('a' + index))
		history = append(history, contextcontract.HistoryEntry{
			TurnID:      id,
			ToolCalls:   []llm.ToolCall{{ID: id, Name: "read_file", Arguments: map[string]any{"path": path}}},
			ToolResults: []contextcontract.ToolResult{{ToolCallID: id, Content: "相同内容"}},
		})
	}
	before, _ := json.Marshal(history)
	projected, report, refs, err := ProjectHistory(context.Background(), history, contextcontract.ContextBudgetPolicy{}, 5,
		"attempt-1", content, contentstore.Scope{Kind: contentstore.ScopeTask, SessionID: "session-1", TaskID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.DeduplicatedFragments != 1 || len(refs) != 1 || len(projected) != 3 {
		t.Fatalf("应仅引用化同文件的旧重复读取：%+v refs=%d entries=%d", report, len(refs), len(projected))
	}
	if !strings.Contains(projected[0].ToolResults[0].Content, "agentgo.tool-result-ref/v1") || projected[1].ToolResults[0].Content != "相同内容" || projected[2].ToolResults[0].Content != "相同内容" {
		t.Fatalf("不同文件或最新读取被错误裁剪：%+v", projected)
	}
	after, _ := json.Marshal(history)
	if string(before) != string(after) {
		t.Fatal("历史投影修改了 Raw History")
	}
}

func TestHistoryRejectsRetiredObservationProjection(t *testing.T) {
	_, _, _, err := ProjectHistory(context.Background(), []contextcontract.HistoryEntry{{ContextProjection: "observation:retired"}},
		contextcontract.ContextBudgetPolicy{}, 5, "attempt", nil, contentstore.Scope{})
	if err == nil {
		t.Fatal("旧 Observation 锚点不能转换成新历史继续执行")
	}
}
