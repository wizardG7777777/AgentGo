package contentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agentgo/internal/contextcontract"
)

const maxMetadataBytes = 64 << 10

type diskRecord struct {
	Schema       string       `json:"schema"`
	Ref          ContentRef   `json:"ref"`
	Availability Availability `json:"availability"`
	Reason       string       `json:"reason,omitempty"`
	UpdatedAt    time.Time    `json:"updated_at"`
	RecordDigest string       `json:"record_digest"`
}

func (r diskRecord) validate() error {
	if r.Schema != DiskRecordSchemaV1 {
		return fmt.Errorf("content record schema=%q，无效", r.Schema)
	}
	if err := r.Ref.Validate(); err != nil {
		return err
	}
	if !r.Availability.Valid() {
		return fmt.Errorf("content record %s availability=%q 无效", r.Ref.RefID, r.Availability)
	}
	if r.UpdatedAt.IsZero() {
		return fmt.Errorf("content record %s 缺少 updated_at", r.Ref.RefID)
	}
	if !contextcontract.ValidDigest(r.RecordDigest) {
		return fmt.Errorf("content record %s record_digest 无效", r.Ref.RefID)
	}
	want, err := computeRecordDigest(r)
	if err != nil {
		return err
	}
	if want != r.RecordDigest {
		return fmt.Errorf("content record %s record_digest 与元数据不一致", r.Ref.RefID)
	}
	return nil
}

func metadataDigest(ref ContentRef) (string, error) {
	canonical := ref
	canonical.MetadataDigest = ""
	return contextcontract.StableDigest("agentgo.content-ref-metadata/v1", canonical)
}

func contentRefIdentity(contentDigest, mediaType string, retention contextcontract.RetentionClass,
	authority contextcontract.Authority, scope Scope, expiresAt time.Time,
) (string, error) {
	digest, err := contextcontract.StableDigest("agentgo.content-ref-id/v1", struct {
		ContentDigest  string                         `json:"content_digest"`
		MediaType      string                         `json:"media_type"`
		RetentionClass contextcontract.RetentionClass `json:"retention_class"`
		Authority      contextcontract.Authority      `json:"authority"`
		Scope          Scope                          `json:"scope"`
		ExpiresAt      time.Time                      `json:"expires_at,omitempty"`
	}{
		ContentDigest: contentDigest, MediaType: mediaType,
		RetentionClass: retention, Authority: authority, Scope: scope,
		ExpiresAt: expiresAt.UTC(),
	})
	if err != nil {
		return "", err
	}
	return "content:" + digest, nil
}

func computeRecordDigest(record diskRecord) (string, error) {
	canonical := record
	canonical.RecordDigest = ""
	return contextcontract.StableDigest("agentgo.content-record/v1", canonical)
}

func validateRefID(refID string) error {
	digest, ok := strings.CutPrefix(refID, "content:")
	if !ok || !contextcontract.ValidDigest(digest) {
		return fmt.Errorf("content ref_id=%q 无效", refID)
	}
	return nil
}

func digestPart(refID string) string {
	return strings.TrimPrefix(refID, "content:")
}

func shardedPath(root, kind, digest, suffix string) string {
	return filepath.Join(root, kind, digest[:2], digest+suffix)
}

func writeAtomicBytes(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("contentstore: 创建目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".content-*.tmp")
	if err != nil {
		return fmt.Errorf("contentstore: 创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("contentstore: chmod 临时文件: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("contentstore: 写临时文件: %w", err)
	}
	// 文件内容在本路径只 fsync 一次；目录项在 rename 后另做目录 fsync。
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("contentstore: fsync 临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("contentstore: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("contentstore: 原子替换 %s: %w", path, err)
	}
	syncDirectory(dir)
	return nil
}

func syncDirectory(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	handle, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = handle.Sync()
	_ = handle.Close()
}

func ensureBlob(path string, content []byte, digest string) error {
	if _, err := os.Stat(path); err == nil {
		return verifyBlobFile(path, int64(len(content)), digest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("contentstore: stat blob: %w", err)
	}
	if err := writeAtomicBytes(path, content, 0o600); err != nil {
		// 另一进程可能刚写入同一 content-addressed blob。只在目标已存在且
		// 内容身份完全一致时把 rename 冲突视作幂等成功。
		if _, statErr := os.Stat(path); statErr == nil {
			return verifyBlobFile(path, int64(len(content)), digest)
		}
		return err
	}
	return verifyBlobFile(path, int64(len(content)), digest)
}

func verifyBlobFile(path string, size int64, digest string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	written, copyErr := io.Copy(h, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != size {
		return fmt.Errorf("blob size=%d，want=%d", written, size)
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("blob digest 不一致")
	}
	return nil
}

func readBlobFile(path string, ref ContentRef) ([]byte, UnavailableReason, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, UnavailableMissingBlob, err
	}
	if err != nil {
		return nil, UnavailableDegraded, err
	}
	// 最多读取声明尺寸 + 1 字节，损坏文件不会造成无界分配。
	content, readErr := io.ReadAll(io.LimitReader(file, ref.SizeBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, UnavailableDegraded, readErr
	}
	if closeErr != nil {
		return nil, UnavailableDegraded, closeErr
	}
	if int64(len(content)) != ref.SizeBytes {
		return nil, UnavailableSizeMismatch, fmt.Errorf("blob size=%d，want=%d", len(content), ref.SizeBytes)
	}
	if contextcontract.DigestBytes(content) != ref.ContentDigest {
		return nil, UnavailableDigestMismatch, fmt.Errorf("blob digest 不一致")
	}
	return content, "", nil
}

// readBlobRangeFile 以固定大小 buffer 流式扫描整个 blob，同时只保留
// [offset, offset+limit) 的有界页。扫描全文是为了验证 ContentDigest；
// 不会把整个 blob 读入内存。limit=0 只做 streaming verify，供 Inspect
// 复用。返回前会关闭句柄，遵守 Windows 清理纪律。
func readBlobRangeFile(ctx context.Context, path string, ref ContentRef, offset, limit int64) (
	content []byte, nextOffset int64, eof bool, reason UnavailableReason, err error,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if offset < 0 || limit < 0 || offset > ref.SizeBytes {
		return nil, 0, false, UnavailableSizeMismatch,
			fmt.Errorf("blob range 无效: offset=%d limit=%d size=%d", offset, limit, ref.SizeBytes)
	}
	file, openErr := os.Open(path)
	if errors.Is(openErr, os.ErrNotExist) {
		return nil, 0, false, UnavailableMissingBlob, openErr
	}
	if openErr != nil {
		return nil, 0, false, UnavailableDegraded, openErr
	}
	closeWith := func(current error) error {
		if closeErr := file.Close(); current == nil && closeErr != nil {
			return closeErr
		}
		return current
	}
	before, statErr := file.Stat()
	if statErr != nil {
		return nil, 0, false, UnavailableDegraded, closeWith(statErr)
	}
	if !before.Mode().IsRegular() {
		return nil, 0, false, UnavailableDegraded,
			closeWith(fmt.Errorf("blob 不是普通文件"))
	}

	remaining := ref.SizeBytes - offset
	want := limit
	if want > remaining {
		want = remaining
	}
	// 调用方在入口已把 limit 限制在 64 KiB；这里仍以 want
	// 精确预分配，不根据 blob 全文尺寸分配。
	page := make([]byte, 0, int(want))
	rangeEnd := offset + want
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, false, "", closeWith(ctxErr)
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			chunkStart := total
			chunkEnd := total + int64(n)
			if _, hashErr := hash.Write(buffer[:n]); hashErr != nil {
				return nil, 0, false, UnavailableDegraded, closeWith(hashErr)
			}
			if chunkEnd > offset && chunkStart < rangeEnd {
				from := maxInt64(offset, chunkStart) - chunkStart
				to := minInt64(rangeEnd, chunkEnd) - chunkStart
				page = append(page, buffer[int(from):int(to)]...)
			}
			total = chunkEnd
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, 0, false, UnavailableDegraded, closeWith(readErr)
		}
	}
	after, statErr := file.Stat()
	if statErr != nil {
		return nil, 0, false, UnavailableDegraded, closeWith(statErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return nil, 0, false, UnavailableDegraded, closeErr
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) {
		return nil, 0, false, UnavailableChangedDuringRead,
			fmt.Errorf("blob 在 streaming read 期间发生变更")
	}
	if total != ref.SizeBytes {
		return nil, 0, false, UnavailableSizeMismatch,
			fmt.Errorf("blob size=%d，want=%d", total, ref.SizeBytes)
	}
	if hex.EncodeToString(hash.Sum(nil)) != ref.ContentDigest {
		return nil, 0, false, UnavailableDigestMismatch, fmt.Errorf("blob digest 不一致")
	}
	next := offset + int64(len(page))
	return page, next, next >= ref.SizeBytes, "", nil
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func readMetadataFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxMetadataBytes {
		return nil, fmt.Errorf("metadata 超过 %d bytes", maxMetadataBytes)
	}
	return data, nil
}

func encodeRecord(record diskRecord) ([]byte, error) {
	digest, err := computeRecordDigest(record)
	if err != nil {
		return nil, err
	}
	record.RecordDigest = digest
	return json.MarshalIndent(record, "", "  ")
}

func decodeRecord(data []byte) (diskRecord, error) {
	var record diskRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return diskRecord{}, err
	}
	if err := record.validate(); err != nil {
		return diskRecord{}, err
	}
	return record, nil
}
