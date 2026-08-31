package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agentgo/internal/contentstore"
	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
)

func projectionPolicy(t *testing.T) policycatalog.ContextProfile {
	t.Helper()
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, ok := catalog.ContextPolicy(policycatalog.ContextDefaultV3)
	if !ok {
		t.Fatal("缺少 Context v3")
	}
	return profile
}

func TestHistoryProjectionUsesCurrentSectionPressureNotRepeatedPromptSpend(t *testing.T) {
	profile := projectionPolicy(t)
	history := make([]HistoryEntry, 10)
	for i := range history {
		history[i] = HistoryEntry{TurnID: "small-turn", AssistantContent: strings.Repeat("x", 512)}
	}
	projected, report := projectHistoryForContext(history, profile.Policy)
	if report.Applied || len(projected) != len(history) {
		t.Fatalf("小 History 不应因外部累计 prompt spend 被压缩: report=%+v len=%d", report, len(projected))
	}
}

func TestHistoryProjectionDerivesBoundedReplayWithoutMutatingRawHistory(t *testing.T) {
	profile := projectionPolicy(t)
	history := make([]HistoryEntry, 12)
	for i := range history {
		history[i] = HistoryEntry{
			TurnID: "turn-" + string(rune('a'+i)), AssistantContent: strings.Repeat("界", 8<<10),
		}
	}
	original := append([]HistoryEntry(nil), history...)
	projected, report := projectHistoryForContext(history, profile.Policy)
	if !report.Applied || report.OmittedEntries == 0 || report.RetainedEntries < historyProjectionKeepRecent {
		t.Fatalf("大 History 未形成 pressure projection: %+v", report)
	}
	if len(projected) != report.RetainedEntries+1 || !strings.HasPrefix(projected[0].IncomingMail, historyProjectionMarker) {
		t.Fatalf("投影视图形状错误: report=%+v projected=%+v", report, projected[:1])
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatal("Replay projection 修改了 Raw History")
	}
	projectedAgain, reportAgain := projectHistoryForContext(history, profile.Policy)
	if reportAgain != report || projectedAgain[0].IncomingMail != projected[0].IncomingMail {
		t.Fatal("同一 Raw History 的投影不确定或发生递归摘要")
	}
}

func TestHistoryProjectionDirectiveSelectsAggressiveRecoveryView(t *testing.T) {
	profile := projectionPolicy(t)
	history := make([]HistoryEntry, 0, 10)
	for i := 0; i < 9; i++ {
		history = append(history, HistoryEntry{TurnID: "turn", AssistantContent: strings.Repeat("x", 12<<10)})
	}
	history = append(history, HistoryEntry{ContextProjection: "aggressive"})
	projected, report := projectHistoryForContext(history, profile.Policy)
	if !report.Applied || !report.Aggressive || !strings.Contains(projected[0].IncomingMail, `mode="context-recovery"`) {
		t.Fatalf("Context overflow 未切换 aggressive projection: report=%+v", report)
	}
	if history[len(history)-1].ContextProjection != "aggressive" {
		t.Fatal("投影消费时修改了恢复指令原始记录")
	}
}

func TestReplayV4ReferencesObservationCoveredHistoryAndDeduplicatesByPathDigest(t *testing.T) {
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := catalog.ContextPolicy(policycatalog.ContextDefaultV10)
	replay, _ := catalog.ProviderReplayPolicy(policycatalog.ReplayOpenAICompatibleV4)
	content, err := contentstore.Open(t.TempDir(), contentstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = content.Close() })
	turn := func(id, toolID, name, target, body string) HistoryEntry {
		args := map[string]any{"path": target}
		return HistoryEntry{
			TurnID: id, ToolCalled: true, AssistantContent: "investigate " + target,
			ToolCalls:   []llm.ToolCall{{ID: toolID, Name: name, Arguments: args}},
			ToolResults: []ToolResult{{ToolCallID: toolID, Content: body}},
		}
	}
	history := []HistoryEntry{
		turn("turn-1", "read-1", "read_file", `src\\a.go`, "same body"),
		turn("turn-2", "grep-1", "grep_search", "src", "unique evidence"),
		{ContextProjection: observationProjectionPrefix + "observation:sha256:checkpoint"},
		turn("turn-3", "read-2", "read_file", "src/a.go", "same body"),
	}
	original, _ := json.Marshal(history)
	projected, report, refs, err := projectHistoryForContextReplay(context.Background(), history,
		profile.Policy, replay.Policy.Version, "task/attempt-1", content, contentstore.Scope{
			Kind: contentstore.ScopeTask, SessionID: "session-1", TaskID: "task-1",
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 3 || report.ReferencedFragments != 4 || report.DeduplicatedFragments != 1 || len(refs) != 4 {
		t.Fatalf("Replay v4 投影计数错误: len=%d report=%+v refs=%d", len(projected), report, len(refs))
	}
	if !strings.Contains(projected[0].AssistantContent, `"reason":"observation_covered"`) ||
		!strings.Contains(projected[0].ToolResults[0].Content, "duplicate_content") ||
		!strings.Contains(projected[1].ToolResults[0].Content, "observation_covered") ||
		projected[2].ToolResults[0].Content != "same body" {
		t.Fatalf("Replay v4 引用/去重视图错误: %+v", projected)
	}
	after, _ := json.Marshal(history)
	if !reflect.DeepEqual(original, after) {
		t.Fatal("Replay v4 修改了 Raw History")
	}
}
