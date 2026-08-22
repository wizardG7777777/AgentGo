package contentstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/contextcontract"
)

type fakeClock struct{ current time.Time }

func (c *fakeClock) Now() time.Time              { return c.current }
func (c *fakeClock) Advance(delta time.Duration) { c.current = c.current.Add(delta) }

func openTestStore(t *testing.T, clock *fakeClock) *Store {
	t.Helper()
	options := Options{}
	if clock != nil {
		options.Now = clock.Now
	}
	store, err := Open(filepath.Join(t.TempDir(), "content"), options)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Windows 纪律：TempDir 清理前先关闭 Store 生命周期。Store 无常驻句柄，
	// 该清理仍钉住未来实现不能引入未关闭 writer。
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func taskScope(taskID string) Scope {
	return Scope{Kind: ScopeTask, SessionID: "session-1", GraphID: "graph-1", TaskID: taskID}
}

func putText(t *testing.T, store *Store, content string, scope Scope) ContentRef {
	t.Helper()
	ref, err := store.Put(context.Background(), PutRequest{
		Content: []byte(content), MediaType: "text/plain; charset=utf-8",
		RetentionClass: contextcontract.RetentionTaskLifetime,
		Authority:      contextcontract.AuthorityInformational, Scope: scope,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return ref
}

func allowAll(_ context.Context, _ AuthorizationRequest) error { return nil }

func TestPutResolveRequiresScopeLeaseAndExplicitAuthorization(t *testing.T) {
	store := openTestStore(t, nil)
	secret := "仅授权任务可读取的正文"
	ref := putText(t, store, secret, taskScope("task-1"))
	if err := ref.Validate(); err != nil {
		t.Fatalf("ContentRef.Validate: %v", err)
	}
	second := putText(t, store, secret, taskScope("task-1"))
	if second.RefID != ref.RefID || second.MetadataDigest != ref.MetadataDigest {
		t.Fatalf("幂等 Put 未复用 Ref: first=%+v second=%+v", ref, second)
	}

	base := ResolveRequest{Ref: ref, LeaseRef: "lease-1", RequesterScope: taskScope("task-1"), MaxBytes: 4096}
	if _, err := store.Resolve(context.Background(), base, nil); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("缺少 authorizer 应拒绝，实际 %v", err)
	}
	withoutLease := base
	withoutLease.LeaseRef = ""
	if _, err := store.Resolve(context.Background(), withoutLease, allowAll); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("缺少 lease 应拒绝，实际 %v", err)
	}
	withoutBudget := base
	withoutBudget.MaxBytes = 0
	if _, err := store.Resolve(context.Background(), withoutBudget, allowAll); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("缺少读取预算应拒绝，实际 %v", err)
	}
	tooSmall := base
	tooSmall.MaxBytes = 1
	if _, err := store.Resolve(context.Background(), tooSmall, allowAll); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("读取预算不足应拒绝，实际 %v", err)
	}

	authorizerCalls := 0
	wrongScope := base
	wrongScope.RequesterScope = taskScope("task-2")
	if _, err := store.Resolve(context.Background(), wrongScope, func(context.Context, AuthorizationRequest) error {
		authorizerCalls++
		return nil
	}); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("跨 Task scope 应拒绝，实际 %v", err)
	}
	if authorizerCalls != 0 {
		t.Fatal("机械 scope 拒绝后不应调用外部 authorizer")
	}

	rejected := errors.New("lease 已撤销")
	if _, err := store.Resolve(context.Background(), base, func(context.Context, AuthorizationRequest) error {
		return rejected
	}); !errors.Is(err, ErrAccessDenied) || !errors.Is(err, rejected) {
		t.Fatalf("授权器拒绝未保留 cause: %v", err)
	}

	resolved, err := store.Resolve(context.Background(), base, func(_ context.Context, request AuthorizationRequest) error {
		if request.LeaseRef != "lease-1" || request.Ref.RefID != ref.RefID {
			t.Fatalf("授权事实不完整: %+v", request)
		}
		// callback 必须在 Store 锁外运行；回调内 Inspect 不得死锁。
		_, inspectErr := store.Inspect(request.Ref.RefID)
		return inspectErr
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(resolved.Content) != secret {
		t.Fatalf("Resolve content=%q", resolved.Content)
	}
	resolved.Content[0] = 'X'
	again, err := store.Resolve(context.Background(), base, allowAll)
	if err != nil || string(again.Content) != secret {
		t.Fatalf("调用方修改返回值污染 Store: content=%q err=%v", again.Content, err)
	}

	metadataPath, _ := store.refPath(ref.RefID)
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte(secret)) {
		t.Fatalf("metadata 不得复制正文: %s", metadata)
	}
}

func TestContentAddressedBlobDedupAndReferenceAwareExpiry(t *testing.T) {
	store := openTestStore(t, nil)
	content := "两个 Task 共用的大正文"
	first := putText(t, store, content, taskScope("task-1"))
	second := putText(t, store, content, taskScope("task-2"))
	if first.RefID == second.RefID {
		t.Fatal("不同 scope 必须产生不同 RefID")
	}
	if first.ContentDigest != second.ContentDigest {
		t.Fatal("相同正文必须共享 ContentDigest")
	}
	if got := countFiles(t, filepath.Join(store.root, "blobs"), ".blob"); got != 1 {
		t.Fatalf("blob 数=%d，want=1", got)
	}

	if err := store.Expire(context.Background(), first.RefID, "task_terminal"); err != nil {
		t.Fatalf("Expire first: %v", err)
	}
	if _, err := os.Stat(store.blobPath(first.ContentDigest)); err != nil {
		t.Fatalf("仍有 live Ref 时 blob 不得删除: %v", err)
	}
	resolved, err := store.Resolve(context.Background(), ResolveRequest{
		Ref: second, LeaseRef: "lease-2", RequesterScope: taskScope("task-2"), MaxBytes: 4096,
	}, allowAll)
	if err != nil || string(resolved.Content) != content {
		t.Fatalf("第二个 Ref 应继续可用: content=%q err=%v", resolved.Content, err)
	}

	if err := store.Expire(context.Background(), second.RefID, "task_terminal"); err != nil {
		t.Fatalf("Expire second: %v", err)
	}
	if _, err := os.Stat(store.blobPath(first.ContentDigest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("最后 live Ref 过期后 blob 应删除，实际 %v", err)
	}
	for _, refID := range []string{first.RefID, second.RefID} {
		status, err := store.Inspect(refID)
		if err != nil || status.Availability != AvailabilityExpired || !status.Degraded {
			t.Fatalf("过期 metadata 应保留 degraded 状态: status=%+v err=%v", status, err)
		}
	}
}

func TestTimedExpiryIsTypedAndSurvivesReopen(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)}
	root := filepath.Join(t.TempDir(), "content")
	store, err := Open(root, Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), PutRequest{
		Content: []byte("短期 provider replay"), MediaType: "application/json",
		RetentionClass: contextcontract.RetentionEphemeralRequest,
		Authority:      contextcontract.AuthorityInformational, Scope: taskScope("task-1"),
		ExpiresAt: clock.current.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	status, err := store.Inspect(ref.RefID)
	if err != nil {
		t.Fatalf("Inspect expired: %v", err)
	}
	if status.Availability != AvailabilityExpired || !status.Degraded || status.Reason != "retention_expired" {
		t.Fatalf("expired status=%+v", status)
	}
	_, err = store.Resolve(context.Background(), ResolveRequest{
		Ref: ref, LeaseRef: "lease-1", RequesterScope: taskScope("task-1"), MaxBytes: 4096,
	}, allowAll)
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) || unavailableErr.Reason != UnavailableExpired || !unavailableErr.Degraded {
		t.Fatalf("过期 Resolve 应返回 typed unavailable: %v", err)
	}
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	reopened, err := Open(root, Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	recovered, err := reopened.Inspect(ref.RefID)
	if err != nil || recovered.Availability != AvailabilityExpired || !recovered.Degraded {
		t.Fatalf("重启后未恢复 expired metadata: status=%+v err=%v", recovered, err)
	}
}

func TestCorruptBlobBecomesDurableDegraded(t *testing.T) {
	store := openTestStore(t, nil)
	ref := putText(t, store, "原始正文", taskScope("task-1"))
	if err := os.WriteFile(store.blobPath(ref.ContentDigest), []byte("篡改正文"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := store.Inspect(ref.RefID)
	if err != nil {
		t.Fatalf("Inspect degraded: %v", err)
	}
	if status.Availability != AvailabilityDegraded || !status.Degraded {
		t.Fatalf("损坏 blob 未标 degraded: %+v", status)
	}
	if status.Reason != string(UnavailableDigestMismatch) {
		t.Fatalf("损坏原因=%q，want=%q", status.Reason, UnavailableDigestMismatch)
	}
	_, err = store.Resolve(context.Background(), ResolveRequest{
		Ref: ref, LeaseRef: "lease-1", RequesterScope: taskScope("task-1"), MaxBytes: 4096,
	}, allowAll)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("degraded Ref 不得 Resolve: %v", err)
	}
}

func TestNeverPersistAndInvalidEphemeralAreRejected(t *testing.T) {
	store := openTestStore(t, nil)
	base := PutRequest{
		Content: []byte("x"), MediaType: "text/plain",
		Authority: contextcontract.AuthorityInformational, Scope: taskScope("task-1"),
	}
	base.RetentionClass = contextcontract.RetentionNeverPersist
	if _, err := store.Put(context.Background(), base); err == nil {
		t.Fatal("never_persist 不得写入 Store")
	}
	base.RetentionClass = contextcontract.RetentionEphemeralRequest
	if _, err := store.Put(context.Background(), base); err == nil {
		t.Fatal("ephemeral_request 缺 expires_at 必须拒绝")
	}
}

func TestResolveRangeStreamsBoundedPagesWithFullDigestVerification(t *testing.T) {
	store := openTestStore(t, nil)
	content := strings.Repeat("0123456789", 20_000) // 200 KiB，超过单页硬上限。
	ref := putText(t, store, content, taskScope("task-1"))

	authorizerCalls := 0
	resolve := func(offset, limit int64) ResolvedRange {
		t.Helper()
		page, err := store.ResolveRange(context.Background(), ResolveRangeRequest{
			Ref: ref, LeaseRef: "execution-lease:abc123", RequesterScope: taskScope("task-1"),
			Offset: offset, Limit: limit, MaxBytes: MaxResolveRangeBytes,
		}, func(_ context.Context, request AuthorizationRequest) error {
			authorizerCalls++
			if request.Ref.RefID != ref.RefID || request.Offset != offset || request.Limit != limit ||
				request.MaxBytes != MaxResolveRangeBytes || request.LeaseRef != "execution-lease:abc123" {
				t.Fatalf("授权 range 事实不完整: %+v", request)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ResolveRange(%d,%d): %v", offset, limit, err)
		}
		return page
	}

	first := resolve(0, 4096)
	if string(first.Content) != content[:4096] || first.NextOffset != 4096 || first.EOF {
		t.Fatalf("首页边界不符: len=%d next=%d eof=%t", len(first.Content), first.NextOffset, first.EOF)
	}
	lastOffset := int64(len(content) - 11)
	last := resolve(lastOffset, 4096)
	if string(last.Content) != content[lastOffset:] || last.NextOffset != int64(len(content)) || !last.EOF {
		t.Fatalf("末页边界不符: len=%d next=%d eof=%t", len(last.Content), last.NextOffset, last.EOF)
	}
	empty := resolve(int64(len(content)), 1)
	if len(empty.Content) != 0 || empty.NextOffset != int64(len(content)) || !empty.EOF {
		t.Fatalf("EOF 空页不符: %+v", empty)
	}
	if authorizerCalls != 3 {
		t.Fatalf("每页必须独立授权，实际 %d", authorizerCalls)
	}
}

func TestResolveRangeRejectsBudgetAndScopeBeforeAuthorization(t *testing.T) {
	store := openTestStore(t, nil)
	ref := putText(t, store, "bounded", taskScope("task-1"))
	base := ResolveRangeRequest{
		Ref: ref, LeaseRef: "execution-lease:abc123", RequesterScope: taskScope("task-1"),
		Offset: 0, Limit: 4, MaxBytes: 4,
	}
	authorizerCalls := 0
	authorize := func(context.Context, AuthorizationRequest) error {
		authorizerCalls++
		return nil
	}
	tooLarge := base
	tooLarge.Limit = MaxResolveRangeBytes + 1
	tooLarge.MaxBytes = MaxResolveRangeBytes
	if _, err := store.ResolveRange(context.Background(), tooLarge, authorize); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("超过绝对上限应拒绝: %v", err)
	}
	wrongScope := base
	wrongScope.RequesterScope = taskScope("task-2")
	if _, err := store.ResolveRange(context.Background(), wrongScope, authorize); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("跨 Task range resolve 应拒绝: %v", err)
	}
	outOfBounds := base
	outOfBounds.Offset = ref.SizeBytes + 1
	if _, err := store.ResolveRange(context.Background(), outOfBounds, authorize); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("超出 blob 边界应拒绝: %v", err)
	}
	if authorizerCalls != 0 {
		t.Fatalf("机械边界拒绝后不得调用 authorizer: %d", authorizerCalls)
	}
}

func TestResolveRangeRechecksExpiryAfterAuthorization(t *testing.T) {
	store := openTestStore(t, nil)
	ref := putText(t, store, "TOCTOU", taskScope("task-1"))
	_, err := store.ResolveRange(context.Background(), ResolveRangeRequest{
		Ref: ref, LeaseRef: "execution-lease:abc123", RequesterScope: taskScope("task-1"),
		Offset: 0, Limit: 4, MaxBytes: 4,
	}, func(ctx context.Context, _ AuthorizationRequest) error {
		// authorizer 在 Store 锁外执行；授权期间 Ref 被终结，
		// ResolveRange 必须重读 metadata，不能继续使用旧 available 快照。
		return store.Expire(ctx, ref.RefID, "authorization_race")
	})
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) || unavailableErr.Reason != UnavailableExpired {
		t.Fatalf("授权期间过期应 fail-closed: %v", err)
	}
}

func TestResolveRangeDigestMismatchBecomesDurableDegraded(t *testing.T) {
	store := openTestStore(t, nil)
	ref := putText(t, store, "original", taskScope("task-1"))
	if err := os.WriteFile(store.blobPath(ref.ContentDigest), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := store.ResolveRange(context.Background(), ResolveRangeRequest{
		Ref: ref, LeaseRef: "execution-lease:abc123", RequesterScope: taskScope("task-1"),
		Offset: 0, Limit: 4, MaxBytes: 4,
	}, allowAll)
	var unavailableErr *UnavailableError
	if !errors.As(err, &unavailableErr) || unavailableErr.Reason != UnavailableDigestMismatch {
		t.Fatalf("全文 digest 失配应返回 typed unavailable: %v", err)
	}
	status, inspectErr := store.Inspect(ref.RefID)
	if inspectErr != nil || status.Availability != AvailabilityDegraded ||
		status.Reason != string(UnavailableDigestMismatch) {
		t.Fatalf("损坏状态应 durable: status=%+v err=%v", status, inspectErr)
	}
}

func TestResolveRangeSurvivesStoreReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "content")
	store, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref := putText(t, store, "recovery-content", taskScope("task-1"))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	page, err := reopened.ResolveRange(context.Background(), ResolveRangeRequest{
		Ref: ref, LeaseRef: "execution-lease:abc123", RequesterScope: taskScope("task-1"),
		Offset: 9, Limit: 32, MaxBytes: 32,
	}, allowAll)
	if err != nil || string(page.Content) != "content" || !page.EOF {
		t.Fatalf("重启后 range resolve 失败: page=%+v err=%v", page, err)
	}
}

func TestCloseLeavesNoWindowsBlockingHandle(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "content")
	store, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = putText(t, store, "close-check", taskScope("task-1"))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close 应幂等: %v", err)
	}
	if _, err := store.Inspect("content:" + contextcontract.DigestBytes([]byte("x"))); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close 后 Inspect 应返回 ErrClosed: %v", err)
	}
	moved := filepath.Join(parent, "content-moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("Close 后目录应可重命名（Windows 句柄纪律）: %v", err)
	}
}

func countFiles(t *testing.T, root, suffix string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == suffix {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}
