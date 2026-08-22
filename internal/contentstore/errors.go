package contentstore

import (
	"errors"
	"fmt"
)

var (
	ErrClosed       = errors.New("contentstore: Store 已关闭")
	ErrUnavailable  = errors.New("contentstore: ContentRef 不可用")
	ErrAccessDenied = errors.New("contentstore: ContentRef 访问被拒绝")
)

// UnavailableReason 是 Resolve/Inspect 不可用的封闭原因。
type UnavailableReason string

const (
	UnavailableMissingRef        UnavailableReason = "missing_ref"
	UnavailableExpired           UnavailableReason = "expired"
	UnavailableDegraded          UnavailableReason = "degraded"
	UnavailableMissingBlob       UnavailableReason = "missing_blob"
	UnavailableSizeMismatch      UnavailableReason = "size_mismatch"
	UnavailableDigestMismatch    UnavailableReason = "digest_mismatch"
	UnavailableChangedDuringRead UnavailableReason = "changed_during_read"
	UnavailableMetadataCorrupt   UnavailableReason = "metadata_corrupt"
)

// UnavailableError 保留 Ref 身份与 degraded 事实，不把缺失正文解释为空内容。
type UnavailableError struct {
	RefID    string
	Reason   UnavailableReason
	Degraded bool
	Cause    error
}

func (e *UnavailableError) Error() string {
	if e == nil {
		return ErrUnavailable.Error()
	}
	if e.Cause != nil {
		return fmt.Sprintf("%v: ref=%s reason=%s: %v", ErrUnavailable, e.RefID, e.Reason, e.Cause)
	}
	return fmt.Sprintf("%v: ref=%s reason=%s", ErrUnavailable, e.RefID, e.Reason)
}

func (e *UnavailableError) Unwrap() error        { return e.Cause }
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// AccessDeniedError 区分 scope 拒绝、Lease 缺失与授权器拒绝。
type AccessDeniedError struct {
	RefID  string
	Reason string
	Cause  error
}

func (e *AccessDeniedError) Error() string {
	if e == nil {
		return ErrAccessDenied.Error()
	}
	if e.Cause != nil {
		return fmt.Sprintf("%v: ref=%s reason=%s: %v", ErrAccessDenied, e.RefID, e.Reason, e.Cause)
	}
	return fmt.Sprintf("%v: ref=%s reason=%s", ErrAccessDenied, e.RefID, e.Reason)
}

func (e *AccessDeniedError) Unwrap() error        { return e.Cause }
func (e *AccessDeniedError) Is(target error) bool { return target == ErrAccessDenied }
