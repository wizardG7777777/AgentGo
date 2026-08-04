package graph

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

// encodedGraphSegmentPrefix 不属于 graph_id 合法字符集，因此编码后的物理
// 目录与用户提供的原始 ID 不会冲突。只编码 Windows 不可表示或语义特殊的
// 段；普通 ID 继续保持可读的既有目录布局，并在所有平台使用同一映射。
const encodedGraphSegmentPrefix = "~"

// graphStoragePath 把逻辑 graph_id 映射为跨平台安全的物理目录。graph_id
// 仍是对外稳定身份；编码只属于持久化实现，不泄漏到 GraphDocument/Trace。
func graphStoragePath(root, graphID string) (string, error) {
	if err := validateGraphID(graphID); err != nil {
		return "", err
	}
	segments := strings.Split(graphID, "/")
	for i, seg := range segments {
		segments[i] = encodeGraphStorageSegment(seg)
	}
	return filepath.Join(root, filepath.Join(segments...)), nil
}

func encodeGraphStorageSegment(seg string) string {
	if isPortableGraphStorageSegment(seg) {
		return seg
	}
	return encodedGraphSegmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(seg))
}

func decodeGraphStorageSegment(seg string) (string, error) {
	if !strings.HasPrefix(seg, encodedGraphSegmentPrefix) {
		return seg, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(seg, encodedGraphSegmentPrefix))
	if err != nil || len(raw) == 0 {
		return "", fmt.Errorf("graph: 持久化目录段 %q 的编码无效", seg)
	}
	decoded := string(raw)
	if !graphIDSegmentCharset.MatchString(decoded) || decoded == "." || decoded == ".." {
		return "", fmt.Errorf("graph: 持久化目录段 %q 解码为非法 graph_id 分段 %q", seg, decoded)
	}
	return decoded, nil
}

// isPortableGraphStorageSegment 覆盖 Windows 最严格的目录名约束；在 Unix
// 也沿用同一判断，保证同一 graph_id 的磁盘布局可跨平台搬运。
func isPortableGraphStorageSegment(seg string) bool {
	if strings.ContainsRune(seg, ':') || strings.HasSuffix(seg, ".") {
		return false
	}
	upper := strings.ToUpper(seg)
	base := upper
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	if len(base) == 4 {
		prefix, digit := base[:3], base[3]
		if (prefix == "COM" || prefix == "LPT") && digit >= '1' && digit <= '9' {
			return false
		}
	}
	return true
}
