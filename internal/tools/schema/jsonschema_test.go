package schema

import (
	"testing"
)

func TestJSONSchemaBuilder_StringArray(t *testing.T) {
	b := Object().
		String("path", "文件路径", true).
		StringArray("line_anchors", "行哈希锚点列表", false).
		Build()

	props, ok := b["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties 不是 map[string]any")
	}

	// line_anchors 应存在
	la, ok := props["line_anchors"].(map[string]any)
	if !ok {
		t.Fatalf("line_anchors 属性缺失或类型错误")
	}

	if typ, _ := la["type"].(string); typ != "array" {
		t.Errorf("line_anchors type = %q, want array", typ)
	}

	items, ok := la["items"].(map[string]any)
	if !ok {
		t.Fatalf("line_anchors items 缺失")
	}
	if itemTyp, _ := items["type"].(string); itemTyp != "string" {
		t.Errorf("line_anchors items.type = %q, want string", itemTyp)
	}

	if desc, _ := la["description"].(string); desc != "行哈希锚点列表" {
		t.Errorf("line_anchors description = %q, want 行哈希锚点列表", desc)
	}

	// required 列表应只含 path，不含 line_anchors
	req, ok := b["required"].([]any)
	if !ok {
		t.Fatalf("required 缺失")
	}
	foundPath := false
	foundAnchors := false
	for _, r := range req {
		switch r {
		case "path":
			foundPath = true
		case "line_anchors":
			foundAnchors = true
		}
	}
	if !foundPath {
		t.Error("required 中应包含 path")
	}
	if foundAnchors {
		t.Error("required 中不应包含 line_anchors（它是可选的）")
	}
}

func TestJSONSchemaBuilder_StringArray_Required(t *testing.T) {
	b := Object().
		StringArray("tags", "标签列表", true).
		Build()

	req, ok := b["required"].([]any)
	if !ok {
		t.Fatalf("required 缺失")
	}
	found := false
	for _, r := range req {
		if r == "tags" {
			found = true
			break
		}
	}
	if !found {
		t.Error("required 中应包含 tags")
	}
}
