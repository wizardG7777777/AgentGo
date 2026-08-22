package contentstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentgo/internal/contextcontract"
)

// Options 控制 Store 的可测试时钟。生产应使用零值。
type Options struct {
	Now func() time.Time
}

// Store 是 content-addressed blob + per-ref metadata 后端。不持有常驻文件
// 句柄；每次 IO 都在返回前关闭，避免 Windows 上阻塞 Session/TempDir 清理。
type Store struct {
	mu     sync.Mutex
	root   string
	now    func() time.Time
	closed bool
}

// Open 创建或打开 root 下的 Content Store。已有 metadata/blob 通过 lazy load
// 恢复；孤儿临时文件不会成为可解析 Ref。
func Open(root string, options Options) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("contentstore: root 不能为空")
	}
	root = filepath.Clean(root)
	for _, dir := range []string{filepath.Join(root, "blobs"), filepath.Join(root, "refs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("contentstore: 创建目录 %s: %w", dir, err)
		}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Store{root: root, now: now}, nil
}

func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Close 幂等关闭 Store。实例没有常驻句柄；该方法是生命周期 fence，保证返回
// 后没有 Store 自己持有的 Windows 文件句柄。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Put 原子写入正文与 metadata。相同正文跨 scope/ref 共享一个 blob；相同
// 正文+元数据返回同一 Ref。never_persist 必须留在调用方内存，不得进入本 Store。
func (s *Store) Put(ctx context.Context, request PutRequest) (ContentRef, error) {
	if s == nil {
		return ContentRef{}, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContentRef{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ContentRef{}, ErrClosed
	}
	now := s.now().UTC()
	if err := validatePutRequest(request, now); err != nil {
		return ContentRef{}, err
	}
	contentDigest := contextcontract.DigestBytes(request.Content)
	refID, err := contentRefIdentity(contentDigest, request.MediaType, request.RetentionClass,
		request.Authority, request.Scope, request.ExpiresAt)
	if err != nil {
		return ContentRef{}, err
	}
	refPath, err := s.refPath(refID)
	if err != nil {
		return ContentRef{}, err
	}
	if record, found, loadErr := s.loadRecordPath(refPath); loadErr != nil {
		return ContentRef{}, unavailable(refID, UnavailableMetadataCorrupt, true, loadErr)
	} else if found {
		refreshed, refreshErr := s.refreshExpiryLocked(record, now)
		if refreshErr != nil {
			return ContentRef{}, refreshErr
		}
		if refreshed.Availability != AvailabilityAvailable {
			return ContentRef{}, unavailable(refID, availabilityReason(refreshed.Availability), true, nil)
		}
		if !sameRefIdentity(refreshed.Ref, request, contentDigest) {
			return ContentRef{}, unavailable(refID, UnavailableMetadataCorrupt, true,
				fmt.Errorf("同 RefID 对应不一致元数据"))
		}
		blobPath := s.blobPath(contentDigest)
		if err := ensureBlob(blobPath, request.Content, contentDigest); err != nil {
			return ContentRef{}, s.markDegradedLocked(refreshed, UnavailableDegraded, err)
		}
		return refreshed.Ref, nil
	}

	ref := ContentRef{
		Schema: ContentRefSchemaV1, RefID: refID, ContentDigest: contentDigest,
		MediaType: request.MediaType, SizeBytes: int64(len(request.Content)),
		RetentionClass: request.RetentionClass, Authority: request.Authority,
		Scope: request.Scope, CreatedAt: now, ExpiresAt: request.ExpiresAt.UTC(),
	}
	ref.MetadataDigest, err = metadataDigest(ref)
	if err != nil {
		return ContentRef{}, err
	}
	if err := ref.Validate(); err != nil {
		return ContentRef{}, err
	}
	if err := ensureBlob(s.blobPath(contentDigest), request.Content, contentDigest); err != nil {
		return ContentRef{}, fmt.Errorf("contentstore: 写入 blob: %w", err)
	}
	record := diskRecord{
		Schema: DiskRecordSchemaV1, Ref: ref, Availability: AvailabilityAvailable,
		UpdatedAt: now,
	}
	if err := s.persistRecordPath(refPath, record); err != nil {
		// blob 可能成为孤儿，但没有 metadata 就永远不可 Resolve；这是安全的
		// fail-closed 崩溃窗口，后续 GC 可以清理。
		return ContentRef{}, err
	}
	return ref, nil
}

// Resolve 先检查 metadata/scope，再在锁外调用显式授权器，最后重新读取
// metadata 并校验正文 digest，避免授权期间 Ref 过期或被降级。
func (s *Store) Resolve(ctx context.Context, request ResolveRequest, authorize AuthorizeFunc) (ResolvedContent, error) {
	if s == nil {
		return ResolvedContent{}, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Ref.Validate(); err != nil {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "invalid_ref", Cause: err}
	}
	if err := validateToken("lease_ref", request.LeaseRef); err != nil {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "missing_lease", Cause: err}
	}
	if err := request.RequesterScope.Validate(); err != nil {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "invalid_requester_scope", Cause: err}
	}
	if request.MaxBytes <= 0 {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "read_budget_missing"}
	}
	if request.Ref.SizeBytes > request.MaxBytes {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "read_budget_exceeded"}
	}
	if authorize == nil {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "authorizer_missing"}
	}

	record, err := s.availableRecord(request.Ref.RefID)
	if err != nil {
		return ResolvedContent{}, err
	}
	if record.Ref.MetadataDigest != request.Ref.MetadataDigest {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "ref_metadata_mismatch"}
	}
	if !record.Ref.Scope.Allows(request.RequesterScope) {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "scope_mismatch"}
	}
	if err := authorize(ctx, AuthorizationRequest{
		Ref: record.Ref, LeaseRef: request.LeaseRef, RequesterScope: request.RequesterScope,
		MaxBytes: request.MaxBytes, Offset: 0, Limit: record.Ref.SizeBytes,
	}); err != nil {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "authorizer_rejected", Cause: err}
	}
	if err := ctx.Err(); err != nil {
		return ResolvedContent{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ResolvedContent{}, ErrClosed
	}
	reloaded, err := s.loadRequiredRecordLocked(request.Ref.RefID)
	if err != nil {
		return ResolvedContent{}, err
	}
	reloaded, err = s.refreshExpiryLocked(reloaded, s.now().UTC())
	if err != nil {
		return ResolvedContent{}, err
	}
	if reloaded.Availability != AvailabilityAvailable {
		return ResolvedContent{}, unavailable(request.Ref.RefID,
			availabilityReason(reloaded.Availability), true, nil)
	}
	if reloaded.Ref.MetadataDigest != record.Ref.MetadataDigest {
		return ResolvedContent{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "ref_changed_after_authorization"}
	}
	content, reason, readErr := readBlobFile(s.blobPath(reloaded.Ref.ContentDigest), reloaded.Ref)
	if readErr != nil {
		return ResolvedContent{}, s.markDegradedLocked(reloaded, reason, readErr)
	}
	return ResolvedContent{Ref: reloaded.Ref, Content: append([]byte(nil), content...)}, nil
}

// ResolveRange 执行一次授权后的 streaming range resolve。它不把整个
// blob 读入内存：仅用固定 buffer 流式计算全文 digest，并保留最多
// MaxBytes 的目标 range。授权回调在 Store 锁外执行；回调后会重读
// metadata/expiry 并比对 MetadataDigest，关闭授权期间的 TOCTOU 窗口。
func (s *Store) ResolveRange(ctx context.Context, request ResolveRangeRequest, authorize AuthorizeFunc) (ResolvedRange, error) {
	if s == nil {
		return ResolvedRange{}, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ResolvedRange{}, err
	}
	if err := request.Ref.Validate(); err != nil {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "invalid_ref", Cause: err}
	}
	if err := validateToken("lease_ref", request.LeaseRef); err != nil {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "missing_lease", Cause: err}
	}
	if err := request.RequesterScope.Validate(); err != nil {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "invalid_requester_scope", Cause: err}
	}
	if request.Offset < 0 {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "range_offset_invalid"}
	}
	if request.Limit <= 0 {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "range_limit_missing"}
	}
	if request.MaxBytes <= 0 || request.MaxBytes > MaxResolveRangeBytes {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "range_budget_invalid"}
	}
	if request.Limit > request.MaxBytes || request.Limit > MaxResolveRangeBytes {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "range_budget_exceeded"}
	}
	if request.Offset > request.Ref.SizeBytes {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "range_offset_out_of_bounds"}
	}
	if authorize == nil {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "authorizer_missing"}
	}

	record, err := s.availableRecord(request.Ref.RefID)
	if err != nil {
		return ResolvedRange{}, err
	}
	if record.Ref.MetadataDigest != request.Ref.MetadataDigest {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "ref_metadata_mismatch"}
	}
	if !record.Ref.Scope.Allows(request.RequesterScope) {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "scope_mismatch"}
	}
	if err := authorize(ctx, AuthorizationRequest{
		Ref: record.Ref, LeaseRef: request.LeaseRef, RequesterScope: request.RequesterScope,
		MaxBytes: request.MaxBytes, Offset: request.Offset, Limit: request.Limit,
	}); err != nil {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "authorizer_rejected", Cause: err}
	}
	if err := ctx.Err(); err != nil {
		return ResolvedRange{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ResolvedRange{}, ErrClosed
	}
	reloaded, err := s.loadRequiredRecordLocked(request.Ref.RefID)
	if err != nil {
		return ResolvedRange{}, err
	}
	reloaded, err = s.refreshExpiryLocked(reloaded, s.now().UTC())
	if err != nil {
		return ResolvedRange{}, err
	}
	if reloaded.Availability != AvailabilityAvailable {
		return ResolvedRange{}, unavailable(request.Ref.RefID,
			availabilityReason(reloaded.Availability), true, nil)
	}
	if reloaded.Ref.MetadataDigest != record.Ref.MetadataDigest {
		return ResolvedRange{}, &AccessDeniedError{RefID: request.Ref.RefID, Reason: "ref_changed_after_authorization"}
	}
	content, nextOffset, eof, reason, readErr := readBlobRangeFile(ctx,
		s.blobPath(reloaded.Ref.ContentDigest), reloaded.Ref, request.Offset, request.Limit)
	if readErr != nil {
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return ResolvedRange{}, readErr
		}
		return ResolvedRange{}, s.markDegradedLocked(reloaded, reason, readErr)
	}
	return ResolvedRange{
		Ref: reloaded.Ref, Offset: request.Offset, Content: content,
		NextOffset: nextOffset, EOF: eof,
	}, nil
}

// Inspect 返回有界 metadata 状态，不授权也不读取正文。它会把到期或 blob
// 缺失/损坏原子标成 expired/degraded，使历史 Snapshot 查询不误报 available。
func (s *Store) Inspect(refID string) (Status, error) {
	if s == nil {
		return Status{}, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Status{}, ErrClosed
	}
	record, err := s.loadRequiredRecordLocked(refID)
	if err != nil {
		return Status{}, err
	}
	record, err = s.refreshExpiryLocked(record, s.now().UTC())
	if err != nil {
		return Status{}, err
	}
	if record.Availability == AvailabilityAvailable {
		if _, _, _, reason, verifyErr := readBlobRangeFile(context.Background(),
			s.blobPath(record.Ref.ContentDigest), record.Ref, 0, 0); verifyErr != nil {
			if markErr := s.markDegradedLocked(record, reason, verifyErr); markErr != nil {
				var unavailableErr *UnavailableError
				if errors.As(markErr, &unavailableErr) {
					record.Availability = AvailabilityDegraded
					record.Reason = string(reason)
					record.UpdatedAt = s.now().UTC()
				} else {
					return Status{}, markErr
				}
			}
		}
	}
	return statusOf(record), nil
}

// Expire 显式结束一个 Ref 的生命周期。metadata 先原子标 expired，再在确认没有
// 其它 live Ref 引用相同 digest 后删除 blob；metadata 永久保留用于审计。
func (s *Store) Expire(ctx context.Context, refID, reason string) error {
	if s == nil {
		return ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	record, err := s.loadRequiredRecordLocked(refID)
	if err != nil {
		return err
	}
	_, err = s.expireRecordLocked(record, s.now().UTC(), reason)
	return err
}

// ExpireScope 显式终结完全匹配 scope（及可选 retention）的全部 Ref。
func (s *Store) ExpireScope(ctx context.Context, scope Scope, retention contextcontract.RetentionClass, reason string) (int, error) {
	if s == nil {
		return 0, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if retention != "" && !retention.Valid() {
		return 0, fmt.Errorf("retention_class=%q 无效", retention)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	paths, err := s.refPathsLocked()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		record, found, loadErr := s.loadRecordPath(path)
		if loadErr != nil {
			return count, unavailable("", UnavailableMetadataCorrupt, true, loadErr)
		}
		if !found || record.Ref.Scope != scope || (retention != "" && record.Ref.RetentionClass != retention) {
			continue
		}
		if record.Availability == AvailabilityExpired {
			continue
		}
		if _, err := s.expireRecordLocked(record, s.now().UTC(), reason); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func validatePutRequest(request PutRequest, now time.Time) error {
	if err := validateMediaType(request.MediaType); err != nil {
		return err
	}
	if !request.RetentionClass.Valid() {
		return fmt.Errorf("retention_class=%q 无效", request.RetentionClass)
	}
	if request.RetentionClass == contextcontract.RetentionNeverPersist {
		return fmt.Errorf("never_persist 内容不得写入 Content Store")
	}
	if !request.Authority.Valid() {
		return fmt.Errorf("authority=%q 无效", request.Authority)
	}
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if request.RetentionClass == contextcontract.RetentionEphemeralRequest && request.ExpiresAt.IsZero() {
		return fmt.Errorf("ephemeral_request 必须声明 expires_at")
	}
	if !request.ExpiresAt.IsZero() && !request.ExpiresAt.After(now) {
		return fmt.Errorf("expires_at 必须晚于当前时间")
	}
	return nil
}

func sameRefIdentity(ref ContentRef, request PutRequest, contentDigest string) bool {
	return ref.ContentDigest == contentDigest && ref.MediaType == request.MediaType &&
		ref.SizeBytes == int64(len(request.Content)) && ref.RetentionClass == request.RetentionClass &&
		ref.Authority == request.Authority && ref.Scope == request.Scope &&
		ref.ExpiresAt.Equal(request.ExpiresAt.UTC())
}

func (s *Store) availableRecord(refID string) (diskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return diskRecord{}, ErrClosed
	}
	record, err := s.loadRequiredRecordLocked(refID)
	if err != nil {
		return diskRecord{}, err
	}
	record, err = s.refreshExpiryLocked(record, s.now().UTC())
	if err != nil {
		return diskRecord{}, err
	}
	if record.Availability != AvailabilityAvailable {
		return diskRecord{}, unavailable(refID, availabilityReason(record.Availability), true, nil)
	}
	return record, nil
}

func (s *Store) refreshExpiryLocked(record diskRecord, now time.Time) (diskRecord, error) {
	if record.Availability == AvailabilityAvailable && !record.Ref.ExpiresAt.IsZero() &&
		!now.Before(record.Ref.ExpiresAt) {
		return s.expireRecordLocked(record, now, "retention_expired")
	}
	return record, nil
}

func (s *Store) expireRecordLocked(record diskRecord, now time.Time, reason string) (diskRecord, error) {
	if record.Availability == AvailabilityExpired {
		return record, nil
	}
	record.Availability = AvailabilityExpired
	record.Reason = boundedReason(reason)
	if record.Reason == "" {
		record.Reason = "expired"
	}
	record.UpdatedAt = now.UTC()
	path, _ := s.refPath(record.Ref.RefID)
	if err := s.persistRecordPath(path, record); err != nil {
		return diskRecord{}, err
	}
	if err := s.garbageCollectBlobLocked(record.Ref.ContentDigest, now); err != nil {
		return record, fmt.Errorf("contentstore: metadata 已过期但 blob GC 失败: %w", err)
	}
	return record, nil
}

func (s *Store) markDegradedLocked(record diskRecord, reason UnavailableReason, cause error) error {
	record.Availability = AvailabilityDegraded
	record.Reason = string(reason)
	record.UpdatedAt = s.now().UTC()
	path, _ := s.refPath(record.Ref.RefID)
	if err := s.persistRecordPath(path, record); err != nil {
		return fmt.Errorf("contentstore: 标记 degraded 失败: %w", err)
	}
	return unavailable(record.Ref.RefID, reason, true, cause)
}

func (s *Store) garbageCollectBlobLocked(digest string, now time.Time) error {
	paths, err := s.refPathsLocked()
	if err != nil {
		return err
	}
	for _, path := range paths {
		record, found, loadErr := s.loadRecordPath(path)
		if loadErr != nil {
			// 无法证明损坏 metadata 不再引用本 blob 时保守保留。
			return loadErr
		}
		if !found || record.Ref.ContentDigest != digest {
			continue
		}
		live := record.Availability == AvailabilityAvailable &&
			(record.Ref.ExpiresAt.IsZero() || now.Before(record.Ref.ExpiresAt))
		if live {
			return nil
		}
	}
	path := s.blobPath(digest)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) loadRequiredRecordLocked(refID string) (diskRecord, error) {
	path, err := s.refPath(refID)
	if err != nil {
		return diskRecord{}, &AccessDeniedError{RefID: refID, Reason: "invalid_ref_id", Cause: err}
	}
	record, found, loadErr := s.loadRecordPath(path)
	if loadErr != nil {
		return diskRecord{}, unavailable(refID, UnavailableMetadataCorrupt, true, loadErr)
	}
	if !found {
		return diskRecord{}, unavailable(refID, UnavailableMissingRef, true, nil)
	}
	return record, nil
}

func (s *Store) loadRecordPath(path string) (diskRecord, bool, error) {
	data, err := readMetadataFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return diskRecord{}, false, nil
	}
	if err != nil {
		return diskRecord{}, false, err
	}
	record, err := decodeRecord(data)
	if err != nil {
		return diskRecord{}, false, err
	}
	return record, true, nil
}

func (s *Store) persistRecordPath(path string, record diskRecord) error {
	data, err := encodeRecord(record)
	if err != nil {
		return fmt.Errorf("contentstore: 编码 metadata: %w", err)
	}
	if err := writeAtomicBytes(path, data, 0o600); err != nil {
		return fmt.Errorf("contentstore: 写 metadata: %w", err)
	}
	return nil
}

func (s *Store) refPath(refID string) (string, error) {
	if err := validateRefID(refID); err != nil {
		return "", err
	}
	digest := digestPart(refID)
	return shardedPath(s.root, "refs", digest, ".json"), nil
}

func (s *Store) blobPath(digest string) string {
	return shardedPath(s.root, "blobs", digest, ".blob")
}

func (s *Store) refPathsLocked() ([]string, error) {
	root := filepath.Join(s.root, "refs")
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func statusOf(record diskRecord) Status {
	return Status{
		Ref: record.Ref, Availability: record.Availability,
		Reason: record.Reason, UpdatedAt: record.UpdatedAt,
		Degraded: record.Availability != AvailabilityAvailable,
	}
}

func availabilityReason(availability Availability) UnavailableReason {
	switch availability {
	case AvailabilityExpired:
		return UnavailableExpired
	case AvailabilityDegraded:
		return UnavailableDegraded
	default:
		return UnavailableDegraded
	}
}

func unavailable(refID string, reason UnavailableReason, degraded bool, cause error) error {
	return &UnavailableError{RefID: refID, Reason: reason, Degraded: degraded, Cause: cause}
}

func boundedReason(reason string) string {
	reason = strings.TrimSpace(reason)
	runes := []rune(reason)
	if len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return reason
}
