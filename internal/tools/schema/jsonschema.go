// Package schema 提供工具参数 JSON Schema 的链式构建器。
//
// 设计目的：消除工具注册时大量的 map[string]any 字面量样板代码。
// 未来扩展：可在 schema 包下新增 xmlschema.go、protoschema.go 等同级文件，
// 提供其他描述格式的构建器。各构建器互不依赖。
//
// 示例用法：
//
//	params := schema.Object().
//	    String("path", "文件路径", true).
//	    Int("offset", "起始行号（1-based），可选", false).
//	    Int("limit", "读取行数上限，可选", false).
//	    Build()
package schema

// JSONSchemaBuilder 构造一个 JSON Schema object 描述。
// 通过链式调用累加属性，最后通过 Build() 输出 map[string]any。
// 不是线程安全的——应在工具注册期一次性构建。
type JSONSchemaBuilder struct {
	properties map[string]any
	required   []string
}

// Object 启动一个新的 object schema 构建。
func Object() *JSONSchemaBuilder {
	return &JSONSchemaBuilder{
		properties: make(map[string]any),
	}
}

// String 添加一个 string 类型的属性。
func (b *JSONSchemaBuilder) String(name, description string, required bool) *JSONSchemaBuilder {
	b.properties[name] = map[string]any{
		"type":        "string",
		"description": description,
	}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// Int 添加一个 integer 类型的属性。
func (b *JSONSchemaBuilder) Int(name, description string, required bool) *JSONSchemaBuilder {
	b.properties[name] = map[string]any{
		"type":        "integer",
		"description": description,
	}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// Bool 添加一个 boolean 类型的属性。
func (b *JSONSchemaBuilder) Bool(name, description string, required bool) *JSONSchemaBuilder {
	b.properties[name] = map[string]any{
		"type":        "boolean",
		"description": description,
	}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// StringArray 添加一个 string 数组类型的属性。
func (b *JSONSchemaBuilder) StringArray(name, description string, required bool) *JSONSchemaBuilder {
	b.properties[name] = map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": description,
	}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// Enum 添加一个 string 类型的枚举属性，值必须在 values 列表中。
func (b *JSONSchemaBuilder) Enum(name, description string, values []string, required bool) *JSONSchemaBuilder {
	enumValues := make([]any, len(values))
	for i, v := range values {
		enumValues[i] = v
	}
	b.properties[name] = map[string]any{
		"type":        "string",
		"enum":        enumValues,
		"description": description,
	}
	if required {
		b.required = append(b.required, name)
	}
	return b
}

// Build 生成最终的 schema map。
func (b *JSONSchemaBuilder) Build() map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": b.properties,
	}
	if len(b.required) > 0 {
		req := make([]any, len(b.required))
		for i, r := range b.required {
			req[i] = r
		}
		out["required"] = req
	}
	return out
}
