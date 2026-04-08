package schema

import "testing"

func TestObject_Empty(t *testing.T) {
	out := Object().Build()
	if out["type"] != "object" {
		t.Errorf("type 应为 object，实际: %v", out["type"])
	}
	props, ok := out["properties"].(map[string]any)
	if !ok || len(props) != 0 {
		t.Errorf("properties 应为空 map，实际: %v", out["properties"])
	}
	if _, has := out["required"]; has {
		t.Errorf("空 schema 不应有 required 字段")
	}
}

func TestObject_StringRequired(t *testing.T) {
	out := Object().
		String("path", "文件路径", true).
		String("encoding", "编码", false).
		Build()

	props := out["properties"].(map[string]any)
	if len(props) != 2 {
		t.Fatalf("应有 2 个属性，实际 %d", len(props))
	}
	pathDef := props["path"].(map[string]any)
	if pathDef["type"] != "string" || pathDef["description"] != "文件路径" {
		t.Errorf("path 字段定义错误: %v", pathDef)
	}

	required := out["required"].([]any)
	if len(required) != 1 || required[0] != "path" {
		t.Errorf("required 应只含 path，实际: %v", required)
	}
}

func TestObject_IntAndBool(t *testing.T) {
	out := Object().
		Int("offset", "起始", false).
		Bool("recursive", "是否递归", true).
		Build()
	props := out["properties"].(map[string]any)
	if props["offset"].(map[string]any)["type"] != "integer" {
		t.Errorf("offset 类型应为 integer")
	}
	if props["recursive"].(map[string]any)["type"] != "boolean" {
		t.Errorf("recursive 类型应为 boolean")
	}
	required := out["required"].([]any)
	if len(required) != 1 || required[0] != "recursive" {
		t.Errorf("required 应只含 recursive，实际: %v", required)
	}
}

func TestObject_Enum(t *testing.T) {
	out := Object().
		Enum("mode", "提取模式", []string{"auto", "article", "full"}, false).
		Build()
	props := out["properties"].(map[string]any)
	mode := props["mode"].(map[string]any)
	if mode["type"] != "string" {
		t.Errorf("enum 字段类型应为 string")
	}
	enums := mode["enum"].([]any)
	if len(enums) != 3 || enums[0] != "auto" || enums[1] != "article" || enums[2] != "full" {
		t.Errorf("enum 值不正确: %v", enums)
	}
}

func TestObject_ChainOrder(t *testing.T) {
	// 验证 required 列表保留添加顺序
	out := Object().
		String("a", "", true).
		String("b", "", false).
		String("c", "", true).
		String("d", "", true).
		Build()
	required := out["required"].([]any)
	if len(required) != 3 || required[0] != "a" || required[1] != "c" || required[2] != "d" {
		t.Errorf("required 顺序不正确: %v", required)
	}
}
