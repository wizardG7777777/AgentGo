package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/model"
)

const (
	toolResultExternalizeThresholdBytes = 16 << 10
	// preview 会再进入 JSON string，引号/反斜杠/控制字符最坏可数倍
	// 膨胀。4 KiB 的首尾总量可使 encoded envelope 稳定低于 L2 48 KiB
	// tool-result fragment cap；完整内容始终可通过 ContentRef 分页读取。
	toolResultInlinePreviewBytes = 4 << 10
)

type toolResultReferenceEnvelope struct {
	Schema        string `json:"schema"`
	Tool          string `json:"tool"`
	RefID         string `json:"ref_id"`
	OriginalBytes int    `json:"original_bytes"`
	SHA256        string `json:"sha256"`
	PreviewHead   string `json:"preview_head,omitempty"`
	PreviewTail   string `json:"preview_tail,omitempty"`
	Instruction   string `json:"instruction"`
}

func (r ContextRuntime) externalizeToolResult(ctx context.Context, task *model.Task, call llm.ToolCall, result string) (string, error) {
	if len([]byte(result)) <= toolResultExternalizeThresholdBytes || r.Content == nil || task == nil {
		return result, nil
	}
	expiresAt := time.Time{}
	if task.RunContract != nil {
		expiresAt = task.RunContract.DeadlineAt
	}
	ref, err := r.Content.Put(ctx, contentstore.PutRequest{
		Content: []byte(result), MediaType: "text/plain; charset=utf-8",
		RetentionClass: contextcontract.RetentionTaskLifetime,
		Authority:      contextcontract.AuthorityInformational,
		Scope: contentstore.Scope{
			Kind: contentstore.ScopeTask, SessionID: r.sessionScope(task),
			GraphID: task.GraphID, TaskID: task.ID,
		},
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return "", fmt.Errorf("持久化完整 ToolResult 失败 tool=%s: %w", call.Name, err)
	}
	bytes := []byte(result)
	half := toolResultInlinePreviewBytes / 2
	head := utf8SafePrefix(bytes, half)
	tail := utf8SafeSuffix(bytes, half)
	envelope := toolResultReferenceEnvelope{
		Schema: "agentgo.tool-result-ref/v1", Tool: call.Name,
		RefID: ref.RefID, OriginalBytes: len(bytes), SHA256: ref.ContentDigest,
		PreviewHead: string(head), PreviewTail: string(tail),
		Instruction: "完整结果已持久化；需要中间区段时使用 read_content_ref(ref_id, offset, limit) 分页读取",
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("编码 ToolResult reference envelope: %w", err)
	}
	return string(encoded), nil
}

func utf8SafePrefix(input []byte, limit int) []byte {
	if len(input) <= limit {
		return input
	}
	end := limit
	for end > 0 && (input[end]&0xc0) == 0x80 {
		end--
	}
	return input[:end]
}

func utf8SafeSuffix(input []byte, limit int) []byte {
	if len(input) <= limit {
		return input
	}
	start := len(input) - limit
	for start < len(input) && (input[start]&0xc0) == 0x80 {
		start++
	}
	return input[start:]
}
