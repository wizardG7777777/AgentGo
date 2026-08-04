package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGraphStoragePathEncodesNonPortableSegments(t *testing.T) {
	root := t.TempDir()
	dir, err := graphStoragePath(root, "team:alpha/CON/trailing.")
	if err != nil {
		t.Fatalf("graphStoragePath: %v", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if !strings.HasPrefix(seg, encodedGraphSegmentPrefix) {
			t.Fatalf("非跨平台分段应编码，实际目录段=%q（完整=%q）", seg, rel)
		}
		if strings.ContainsRune(seg, ':') || strings.EqualFold(seg, "CON") || strings.HasSuffix(seg, ".") {
			t.Fatalf("编码后仍含 Windows 非法目录名: %q", seg)
		}
	}
}

func TestStoreRecoversEncodedGraphID(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	raw := strings.Replace(tinyDocJSON, `"graph_id": "g1"`, `"graph_id": "team:alpha"`, 1)
	mustSubmit(t, s, raw)
	physical, err := graphStoragePath(dir, "team:alpha")
	if err != nil {
		t.Fatalf("graphStoragePath: %v", err)
	}
	if st, err := os.Stat(filepath.Join(physical, journalFileName)); err != nil || st.IsDir() {
		t.Fatalf("编码目录内应存在 journal: stat=%v err=%v", st, err)
	}

	ns := reopenStore(t, s)
	if doc, ok := ns.Get("team:alpha"); !ok || doc.GraphID != "team:alpha" {
		t.Fatalf("Recover 应还原逻辑 graph_id，ok=%v doc=%+v", ok, doc)
	}
}
