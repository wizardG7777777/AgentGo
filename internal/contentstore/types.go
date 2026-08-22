package contentstore

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"agentgo/internal/contextcontract"
)

const (
	ContentRefSchemaV1 = "agentgo.content-ref/v1"
	DiskRecordSchemaV1 = "agentgo.content-record/v1"
	// MaxResolveRangeBytes 是单次 streaming range resolve 的绝对上限。
	// 调用方可以在此之下再收紧 policy budget，但不能扩大。
	MaxResolveRangeBytes int64 = 64 << 10
)

// ScopeKind 是 ContentRef 的机械隔离层级。
type ScopeKind string

const (
	ScopeSession ScopeKind = "session"
	ScopeGraph   ScopeKind = "graph"
	ScopeTask    ScopeKind = "task"
)

func (k ScopeKind) Valid() bool {
	switch k {
	case ScopeSession, ScopeGraph, ScopeTask:
		return true
	default:
		return false
	}
}

// Scope 保存 Content 所属 Session/Graph/Task。SessionID 始终必填；Graph/Task
// 层按 Kind 逐级收紧。普通非 Graph Task 的 GraphID 可以为空。
type Scope struct {
	Kind      ScopeKind `json:"kind"`
	SessionID string    `json:"session_id"`
	GraphID   string    `json:"graph_id,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
}

func (s Scope) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("content scope kind=%q 无效", s.Kind)
	}
	if err := validateToken("session_id", s.SessionID); err != nil {
		return err
	}
	switch s.Kind {
	case ScopeSession:
		if s.GraphID != "" || s.TaskID != "" {
			return fmt.Errorf("session scope 不得携带 graph_id/task_id")
		}
	case ScopeGraph:
		if err := validateToken("graph_id", s.GraphID); err != nil {
			return err
		}
		if s.TaskID != "" {
			return fmt.Errorf("graph scope 不得携带 task_id")
		}
	case ScopeTask:
		if err := validateToken("task_id", s.TaskID); err != nil {
			return err
		}
		if s.GraphID != "" {
			if err := validateToken("graph_id", s.GraphID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Allows 报告 requester 是否落在 owner 的可见范围内。它只做 scope 机械交集，
// 不能代替 ExecutionLease 的授权回调。
func (owner Scope) Allows(requester Scope) bool {
	if owner.Validate() != nil || requester.Validate() != nil || owner.SessionID != requester.SessionID {
		return false
	}
	switch owner.Kind {
	case ScopeSession:
		return true
	case ScopeGraph:
		return requester.GraphID == owner.GraphID && requester.Kind != ScopeSession
	case ScopeTask:
		if requester.Kind != ScopeTask || requester.TaskID != owner.TaskID {
			return false
		}
		return owner.GraphID == "" || requester.GraphID == owner.GraphID
	default:
		return false
	}
}

// ContentRef 是大型正文的稳定、不透明引用。MetadataDigest 保护除自身之外的
// 全部元数据；ContentDigest 是正文完整 SHA256。
type ContentRef struct {
	Schema         string                         `json:"schema"`
	RefID          string                         `json:"ref_id"`
	ContentDigest  string                         `json:"content_digest"`
	MediaType      string                         `json:"media_type"`
	SizeBytes      int64                          `json:"size_bytes"`
	RetentionClass contextcontract.RetentionClass `json:"retention_class"`
	Authority      contextcontract.Authority      `json:"authority"`
	Scope          Scope                          `json:"scope"`
	CreatedAt      time.Time                      `json:"created_at"`
	ExpiresAt      time.Time                      `json:"expires_at,omitempty"`
	MetadataDigest string                         `json:"metadata_digest"`
}

func (r ContentRef) Validate() error {
	if r.Schema != ContentRefSchemaV1 {
		return fmt.Errorf("content ref schema=%q，无效", r.Schema)
	}
	if err := validateRefID(r.RefID); err != nil {
		return err
	}
	if !contextcontract.ValidDigest(r.ContentDigest) {
		return fmt.Errorf("content ref %s content_digest 无效", r.RefID)
	}
	if err := validateMediaType(r.MediaType); err != nil {
		return err
	}
	if r.SizeBytes < 0 {
		return fmt.Errorf("content ref %s size_bytes 不能为负", r.RefID)
	}
	if r.SizeBytes == math.MaxInt64 {
		return fmt.Errorf("content ref %s size_bytes 超出安全读取范围", r.RefID)
	}
	if !r.RetentionClass.Valid() {
		return fmt.Errorf("content ref %s retention_class=%q 无效", r.RefID, r.RetentionClass)
	}
	if r.RetentionClass == contextcontract.RetentionNeverPersist {
		return fmt.Errorf("content ref %s 不得使用 never_persist", r.RefID)
	}
	if !r.Authority.Valid() {
		return fmt.Errorf("content ref %s authority=%q 无效", r.RefID, r.Authority)
	}
	if err := r.Scope.Validate(); err != nil {
		return fmt.Errorf("content ref %s: %w", r.RefID, err)
	}
	if r.CreatedAt.IsZero() {
		return fmt.Errorf("content ref %s 缺少 created_at", r.RefID)
	}
	if !r.ExpiresAt.IsZero() && r.ExpiresAt.Before(r.CreatedAt) {
		return fmt.Errorf("content ref %s expires_at 早于 created_at", r.RefID)
	}
	if !contextcontract.ValidDigest(r.MetadataDigest) {
		return fmt.Errorf("content ref %s metadata_digest 无效", r.RefID)
	}
	wantRefID, err := contentRefIdentity(r.ContentDigest, r.MediaType, r.RetentionClass,
		r.Authority, r.Scope, r.ExpiresAt)
	if err != nil {
		return err
	}
	if wantRefID != r.RefID {
		return fmt.Errorf("content ref %s 与内容/作用域身份不一致", r.RefID)
	}
	want, err := metadataDigest(r)
	if err != nil {
		return err
	}
	if want != r.MetadataDigest {
		return fmt.Errorf("content ref %s metadata_digest 与元数据不一致", r.RefID)
	}
	return nil
}

// PutRequest 是一次 content-addressed 写入。Content 在调用返回后不被 Store 持有。
type PutRequest struct {
	Content        []byte
	MediaType      string
	RetentionClass contextcontract.RetentionClass
	Authority      contextcontract.Authority
	Scope          Scope
	ExpiresAt      time.Time
}

// AuthorizationRequest 是 Resolve 交给外部授权器的完整机械事实。
type AuthorizationRequest struct {
	Ref            ContentRef
	LeaseRef       string
	RequesterScope Scope
	MaxBytes       int64
	// Offset/Limit 绑定本次实际请求的 range。旧 Resolve 的
	// Offset=0、Limit=Ref.SizeBytes；Range Resolve 按请求值填写。
	Offset int64
	Limit  int64
}

// AuthorizeFunc 必须显式确认当前 Lease 可以读取 Ref。nil 永远拒绝。
type AuthorizeFunc func(context.Context, AuthorizationRequest) error

// ResolveRequest 把引用、Lease 与调用方 scope 绑定为一次读取请求。
type ResolveRequest struct {
	Ref            ContentRef
	LeaseRef       string
	RequesterScope Scope
	// MaxBytes 是本次 Lease/Context policy 允许解引用的硬上限，必须 > 0。
	MaxBytes int64
}

// ResolvedContent 是一次通过授权、scope 与 digest 校验后的正文副本。
type ResolvedContent struct {
	Ref     ContentRef
	Content []byte
}

// ResolveRangeRequest 把不透明 Ref、冻结 Lease、requester scope 与
// 有界 byte range 绑定为一次授权读。MaxBytes 是调用方 policy
// 上限，必须 <= MaxResolveRangeBytes，Limit 还必须 <= MaxBytes。
type ResolveRangeRequest struct {
	Ref            ContentRef
	LeaseRef       string
	RequesterScope Scope
	Offset         int64
	Limit          int64
	MaxBytes       int64
}

// ResolvedRange 是一页通过 scope、Lease、expiry、TOCTOU 与全文
// digest 核验的内容。Content 最多 MaxResolveRangeBytes，不持有文件句柄。
type ResolvedRange struct {
	Ref        ContentRef
	Offset     int64
	Content    []byte
	NextOffset int64
	EOF        bool
}

// Availability 是 Inspect 的非正文状态。
type Availability string

const (
	AvailabilityAvailable Availability = "available"
	AvailabilityExpired   Availability = "expired"
	AvailabilityDegraded  Availability = "degraded"
)

func (a Availability) Valid() bool {
	switch a {
	case AvailabilityAvailable, AvailabilityExpired, AvailabilityDegraded:
		return true
	default:
		return false
	}
}

// Status 是 ContentRef 的有界查询结果。expired/degraded 都不可 Resolve；元数据
// 仍保留供历史 Snapshot 审计。
type Status struct {
	Ref          ContentRef   `json:"ref"`
	Availability Availability `json:"availability"`
	Reason       string       `json:"reason,omitempty"`
	UpdatedAt    time.Time    `json:"updated_at"`
	Degraded     bool         `json:"degraded"`
}

func validateToken(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s 不能为空", label)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s=%q 含首尾空白", label, value)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s=%q 含控制字符", label, value)
		}
	}
	if len([]rune(value)) > 512 {
		return fmt.Errorf("%s 超过 512 rune", label)
	}
	return nil
}

func validateMediaType(value string) error {
	if err := validateToken("media_type", value); err != nil {
		return err
	}
	if !strings.Contains(value, "/") {
		return fmt.Errorf("media_type=%q 缺少 type/subtype", value)
	}
	if len([]rune(value)) > 256 {
		return fmt.Errorf("media_type 超过 256 rune")
	}
	return nil
}
