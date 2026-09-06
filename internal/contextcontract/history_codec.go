package contextcontract

import (
	"encoding/json"
	"fmt"
)

const HistorySchema = "agentgo.model-history/v1"

type History struct {
	Schema  string         `json:"schema"`
	Entries []HistoryEntry `json:"entries"`
}

func EncodeHistory(entries []HistoryEntry) ([]byte, error) {
	return json.Marshal(History{Schema: HistorySchema, Entries: entries})
}
func DecodeHistory(raw []byte) ([]HistoryEntry, error) {
	var h History
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("拒绝旧格式或损坏模型历史: %w", err)
	}
	if h.Schema != HistorySchema {
		return nil, fmt.Errorf("拒绝模型历史契约 %q", h.Schema)
	}
	return h.Entries, nil
}
