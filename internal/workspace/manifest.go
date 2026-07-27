package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// manifestEntry 记录 dirty set 中单个文件的基线信息。
// BaselineSHA256 是 copy-on-write 时刻主根文件内容的 SHA256（hex）；
// New=true 表示该文件为 workspace 新建（主根无基线，BaselineSHA256 恒为空串）。
type manifestEntry struct {
	BaselineSHA256 string `json:"baseline_sha256"`
	New            bool   `json:"new"`
}

// manifest 是 workspace 的 dirty set 清单：map[relPath]entry。
// 持久化为 workspace 根下的 .workspace-manifest.json——每次登记后整文件
// JSON 重写（一次 WriteFile，不加二次 fsync），任务重试复用 workspace 时
// 经 loadManifest 幂等恢复。并发安全。
type manifest struct {
	path    string // manifest 文件绝对路径
	mu      sync.Mutex
	entries map[string]manifestEntry
}

// loadManifest 从磁盘加载 manifest；文件不存在（或为空）时返回空清单。
// JSON 损坏返回错误——宁可使 Materialize/MergeTask 失败也不静默丢基线。
func loadManifest(path string) (*manifest, error) {
	mf := &manifest{path: path, entries: make(map[string]manifestEntry)}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return mf, nil
		}
		return nil, fmt.Errorf("读取 manifest 失败：%w", err)
	}
	if len(data) == 0 {
		return mf, nil
	}
	if err := json.Unmarshal(data, &mf.entries); err != nil {
		return nil, fmt.Errorf("解析 manifest 失败：%w", err)
	}
	if mf.entries == nil {
		mf.entries = make(map[string]manifestEntry)
	}
	return mf, nil
}

// get 查询条目（rel 为相对主根路径）。
func (mf *manifest) get(rel string) (manifestEntry, bool) {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	e, ok := mf.entries[rel]
	return e, ok
}

// set 登记/更新条目并立即持久化（整文件重写）。
func (mf *manifest) set(rel string, e manifestEntry) error {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	mf.entries[rel] = e
	return mf.persistLocked()
}

// snapshot 返回条目拷贝，调用方修改不影响内部状态。
func (mf *manifest) snapshot() map[string]manifestEntry {
	mf.mu.Lock()
	defer mf.mu.Unlock()
	out := make(map[string]manifestEntry, len(mf.entries))
	for k, v := range mf.entries {
		out[k] = v
	}
	return out
}

// persistLocked 整文件 JSON 重写（一次 WriteFile，无二次 fsync）。
// 调用方须已持有 mf.mu。Go 的 map JSON 序列化按键排序，输出确定。
func (mf *manifest) persistLocked() error {
	data, err := json.MarshalIndent(mf.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 manifest 失败：%w", err)
	}
	if err := os.WriteFile(mf.path, data, 0o644); err != nil {
		return fmt.Errorf("持久化 manifest 失败：%w", err)
	}
	return nil
}
