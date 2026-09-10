package tools

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"agentgo/internal/model"
)

func (g EvidenceGroup) readGraphEvidence(task *model.Task, graphID, ref string, offset, limit int64) (string, error) {
	if g.Graphs == nil {
		return "", fmt.Errorf("Graph 证据存储未注入")
	}
	bound := task.GraphID
	if bound == "" {
		bound = task.InterventionGraphID
	}
	if bound == "" {
		bound = task.FinalReportGraphID
	}
	if graphID == "" {
		graphID = bound
	}
	if bound != "" && graphID != bound {
		return "", fmt.Errorf("证据图超出当前任务范围")
	}
	doc, ok, getErr := g.Graphs.Get(graphID)
	if getErr != nil {
		return "", getErr
	}
	if !ok {
		return "", fmt.Errorf("证据所属图不存在")
	}
	if task.RunID == "" || doc.Definition.RunID != string(task.RunID) {
		return "", fmt.Errorf("证据不属于当前 Run")
	}
	if g.SessionID != nil && doc.Definition.SessionID != g.SessionID() {
		return "", fmt.Errorf("证据不属于当前 Session")
	}
	var value any
	for _, result := range doc.Results {
		if result.Ref == ref {
			value = result
			break
		}
		for _, e := range result.Evidence {
			if e.Ref == ref {
				value = e
				break
			}
		}
		if value != nil {
			break
		}
	}
	if value == nil {
		return "", fmt.Errorf("当前图没有该结果或证据引用")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if offset > int64(len(raw)) {
		return "", fmt.Errorf("证据分页起点越界")
	}
	end := min(int64(len(raw)), offset+limit)
	part := raw[offset:end]
	encoding, body := "utf-8", string(part)
	if !utf8.Valid(part) {
		encoding, body = "base64", base64.StdEncoding.EncodeToString(part)
	}
	digest := sha256.Sum256(raw)
	result := contentRefToolResult{Content: body, NextOffset: end, EOF: end == int64(len(raw)), Digest: hex.EncodeToString(digest[:]), Encoding: encoding}
	out, err := json.Marshal(result)
	return string(out), err
}
