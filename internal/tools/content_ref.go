package tools

// content_ref.go 是 L3 ContentRef 的显式解引用工具面。
//
// ContentRef 本身不授权。read_content_ref 每一页都从 agent context
// 取当前 TaskID，重读 TaskStore 中的冻结 ExecutionLease，与当前
// Session/Graph/Task scope 一起交给 ContentStore 机械校验。模型在
// Prompt/History 中看到 ref_id 不能扩大权限。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"agentgo/internal/agent"
	"agentgo/internal/contentstore"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/store"
)

const (
	contentRefToolDefaultLimit int64 = 32 << 10
	contentRefToolMaxLimit     int64 = 64 << 10
)

// ContentRefGroup 注册 read_content_ref。ContentStore/TaskStore 是生产必填依赖；
// SessionID 可选，无真实 Session 时与 L2 一样退化为 Run/Task scope。
// Register 始终注册 schema，未装配时调用 fail-closed，便于
// known-tools 并集和 config doctor 对账。
type ContentRefGroup struct {
	ContentStore *contentstore.Store
	TaskStore    store.TaskStore
	SessionID    func() string
}

func (g ContentRefGroup) Register(r *agent.ToolRegistry) {
	params := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"ref_id": map[string]any{
				"type": "string", "description": "ContentRef 的不透明 ref_id",
			},
			"offset": map[string]any{
				"type": "integer", "minimum": 0,
				"description": "起始 byte offset，默认 0",
			},
			"limit": map[string]any{
				"type": "integer", "minimum": 1, "maximum": contentRefToolMaxLimit,
				"description": fmt.Sprintf("本页最大 bytes，默认 %d，硬上限 %d",
					contentRefToolDefaultLimit, contentRefToolMaxLimit),
			},
		},
		"required": []any{"ref_id"},
	}
	r.Register("read_content_ref",
		"按 byte range 读取当前任务有权访问的 ContentRef。Ref 不授权；每页都会重新校验冻结 ExecutionLease 与 Session/Graph/Task scope。",
		params, g.readContentRef)
}

type contentRefToolResult struct {
	Content    string `json:"content"`
	NextOffset int64  `json:"next_offset"`
	EOF        bool   `json:"eof"`
	Digest     string `json:"digest"`
	Encoding   string `json:"encoding"`
}

func (g ContentRefGroup) readContentRef(ctx context.Context, args map[string]any) (string, error) {
	if g.ContentStore == nil {
		return "", fmt.Errorf("read_content_ref: ContentStore 未装配")
	}
	if g.TaskStore == nil {
		return "", fmt.Errorf("read_content_ref: TaskStore 未装配")
	}
	for key := range args {
		switch key {
		case "ref_id", "offset", "limit":
		default:
			return "", fmt.Errorf("read_content_ref: 不接受参数 %q", key)
		}
	}
	refID, ok := args["ref_id"].(string)
	if !ok || refID == "" {
		return "", fmt.Errorf("read_content_ref: 缺少 ref_id")
	}
	offset, err := contentRefRangeInt(args["offset"], 0, "offset")
	if err != nil {
		return "", err
	}
	limit, err := contentRefRangeInt(args["limit"], contentRefToolDefaultLimit, "limit")
	if err != nil {
		return "", err
	}
	if offset < 0 {
		return "", fmt.Errorf("read_content_ref: offset 不能为负数")
	}
	if limit <= 0 || limit > contentRefToolMaxLimit {
		return "", fmt.Errorf("read_content_ref: limit 必须在 1..%d", contentRefToolMaxLimit)
	}

	taskID := agent.TaskIDFromContext(ctx)
	if taskID == "" {
		return "", fmt.Errorf("read_content_ref: agent context 缺少 TaskID")
	}
	task, err := g.TaskStore.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("read_content_ref: 读取当前 Task: %w", err)
	}
	if task == nil || task.ID != taskID {
		return "", fmt.Errorf("read_content_ref: 当前 Task 身份不一致")
	}
	sessionID := g.contentRefSessionScope(task)
	requester := contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: sessionID,
		GraphID: task.GraphID, TaskID: task.ID,
	}
	if err := requester.Validate(); err != nil {
		return "", fmt.Errorf("read_content_ref: requester scope 无效: %w", err)
	}
	leaseRef, err := contentRefLeaseRef(task)
	if err != nil {
		return "", err
	}

	// Inspect 只取 metadata/availability；其 blob 完整性校验是固定 buffer
	// streaming verify，不会把整份正文读入内存。
	status, err := g.ContentStore.Inspect(refID)
	if err != nil {
		return "", err
	}
	if status.Availability != contentstore.AvailabilityAvailable {
		return "", contentRefUnavailable(status)
	}
	expectedGraphID := task.GraphID
	expectedRunID := task.RunID
	page, err := g.ContentStore.ResolveRange(ctx, contentstore.ResolveRangeRequest{
		Ref: status.Ref, LeaseRef: leaseRef, RequesterScope: requester,
		Offset: offset, Limit: limit, MaxBytes: contentRefToolMaxLimit,
	}, func(authCtx context.Context, request contentstore.AuthorizationRequest) error {
		return g.authorizeContentRef(authCtx, request, taskID, expectedGraphID, expectedRunID,
			sessionID, refID, offset, limit)
	})
	if err != nil {
		return "", err
	}

	encoding := "utf-8"
	content := string(page.Content)
	if !utf8.Valid(page.Content) {
		encoding = "base64"
		content = base64.StdEncoding.EncodeToString(page.Content)
	}
	payload, err := json.Marshal(contentRefToolResult{
		Content: content, NextOffset: page.NextOffset, EOF: page.EOF,
		Digest: page.Ref.ContentDigest, Encoding: encoding,
	})
	if err != nil {
		return "", fmt.Errorf("read_content_ref: 编码结果: %w", err)
	}
	return string(payload), nil
}

func (g ContentRefGroup) authorizeContentRef(ctx context.Context, request contentstore.AuthorizationRequest,
	taskID, graphID string, runID runcontract.RunID, sessionID, refID string, offset, limit int64,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 先重算 Session scope，再重读 Task/Lease，使 TaskStore 快照成为
	// authorizer 最后一个权威读。
	if current := g.contentRefSessionScope(&model.Task{ID: taskID, RunID: runID}); current != sessionID {
		return fmt.Errorf("授权期间 Session 已变更")
	}
	fresh, err := g.TaskStore.GetTask(taskID)
	if err != nil {
		return fmt.Errorf("重读 Task: %w", err)
	}
	if fresh == nil || fresh.ID != taskID || fresh.GraphID != graphID || fresh.RunID != runID ||
		fresh.Status != model.TaskStatusProcessing {
		return fmt.Errorf("Task 身份/状态已变更")
	}
	leaseRef, err := contentRefLeaseRef(fresh)
	if err != nil {
		return err
	}
	wantScope := contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: sessionID, GraphID: graphID, TaskID: taskID,
	}
	if request.Ref.RefID != refID || request.LeaseRef != leaseRef || request.RequesterScope != wantScope ||
		request.Offset != offset || request.Limit != limit || request.MaxBytes != contentRefToolMaxLimit {
		return fmt.Errorf("ContentRef 授权事实与当前 Task/Lease/range 不一致")
	}
	return nil
}

// contentRefSessionScope 与 agent.ContextRuntime.sessionScope 使用同一机械规则：
// 真实 Session 优先；无 Session 的新 Run 使用 sessionless-run；legacy
// Task 使用 task identity。初次 requester 和 authorizer 重读都调用本函数，
// 避免空 Session provider 造成 L2 写入 scope 与 L3 解引用 scope 分叉。
func (g ContentRefGroup) contentRefSessionScope(task *model.Task) string {
	if g.SessionID != nil {
		if sessionID := strings.TrimSpace(g.SessionID()); sessionID != "" {
			return sessionID
		}
	}
	if task != nil && task.RunID != "" {
		return "sessionless-run:" + string(task.RunID)
	}
	if task != nil && task.ID != "" {
		return "legacy-task:" + task.ID
	}
	return "legacy-unknown"
}

func contentRefLeaseRef(task *model.Task) (string, error) {
	if task == nil || task.Status != model.TaskStatusProcessing {
		return "", fmt.Errorf("read_content_ref: Task 不在 processing")
	}
	lease := task.Lease
	if lease == nil || lease.Revoked {
		return "", fmt.Errorf("read_content_ref: 当前 Task 缺少有效冻结 ExecutionLease")
	}
	if lease.TaskID != task.ID || lease.Attempt <= 0 || lease.FrozenAt.IsZero() || lease.Digest == "" {
		return "", fmt.Errorf("read_content_ref: ExecutionLease 身份字段不完整")
	}
	if !slices.Equal(lease.BusinessTools, model.SortedCopy(lease.BusinessTools)) ||
		!slices.Equal(lease.ControlTools, model.SortedCopy(lease.ControlTools)) {
		return "", fmt.Errorf("read_content_ref: ExecutionLease 工具面未 canonicalize")
	}
	if lease.ComputeDigest() != lease.Digest {
		return "", fmt.Errorf("read_content_ref: ExecutionLease digest 失配")
	}
	if !slices.Contains(lease.ToolUnion(), "read_content_ref") {
		return "", fmt.Errorf("read_content_ref: 冻结 ExecutionLease 未授予该工具")
	}
	return "execution-lease:" + lease.Digest, nil
}

func contentRefRangeInt(value any, fallback int64, label string) (int64, error) {
	if value == nil {
		return fallback, nil
	}
	switch number := value.(type) {
	case int:
		return int64(number), nil
	case int64:
		return number, nil
	case float64:
		const maxSafeJSONInteger = float64(1<<53 - 1)
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number ||
			number < -maxSafeJSONInteger || number > maxSafeJSONInteger {
			return 0, fmt.Errorf("read_content_ref: %s 必须是安全整数", label)
		}
		return int64(number), nil
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			return 0, fmt.Errorf("read_content_ref: %s 必须是安全整数", label)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("read_content_ref: %s 必须是 integer", label)
	}
}

func contentRefUnavailable(status contentstore.Status) error {
	reason := contentstore.UnavailableDegraded
	switch status.Availability {
	case contentstore.AvailabilityExpired:
		reason = contentstore.UnavailableExpired
	case contentstore.AvailabilityDegraded:
		if status.Reason != "" {
			reason = contentstore.UnavailableReason(status.Reason)
		}
	}
	return &contentstore.UnavailableError{
		RefID: status.Ref.RefID, Reason: reason, Degraded: true,
	}
}
