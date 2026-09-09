package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyChangeCreateReplaceAndVersionConflict(t *testing.T) {
	g, _, root := newWriteGroup(t, nil)
	ctx := context.Background()
	path := filepath.Join(root, "new", "file.txt")
	if _, err := g.applyChange(ctx, map[string]any{"path": path, "operation": "create", "content": "初始内容"}); err != nil {
		t.Fatal(err)
	}
	firstHash := computeSHA256([]byte("初始内容"))
	if _, err := g.applyChange(ctx, map[string]any{"path": path, "operation": "replace", "old_str": "初始", "new_str": "修改", "expected_hash": firstHash}); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"path": path, "operation": "create", "content": "覆盖"},
		{"path": path, "operation": "write", "content": "覆盖", "expected_hash": firstHash},
		{"path": path, "content": "混用", "old_str": "修改", "new_str": "覆盖"},
		{"path": path, "old_str": "修改", "new_str": "覆盖", "line_anchors": []string{"1#AA"}},
		{"path": path, "content": "覆盖", "expected_hash": 123},
		{"path": path, "content": "覆盖", "operation": true},
		{"path": path, "content": "覆盖", "old_str": nil},
		{"path": path, "content": "覆盖", "line_anchors": nil},
	} {
		if _, err := g.applyChange(ctx, args); err == nil {
			t.Fatalf("冲突请求应拒绝：%v", args)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "修改内容" {
		t.Fatalf("拒绝后文件被改变：%q %v", data, err)
	}
	if files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".agentgo-change-*")); err != nil || len(files) != 0 {
		t.Fatalf("临时变更文件未清理：%v %v", files, err)
	}
}
