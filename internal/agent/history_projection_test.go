package agent

import (
	"reflect"
	"strings"
	"testing"

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
