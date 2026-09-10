package graph

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// 结果使用明确的 JSON Schema 子集；未知关键词拒绝，不假装已经校验。
func validateDataflowValueSchema(schema map[string]any) error {
	if schema == nil {
		return fmt.Errorf("缺少数据 schema")
	}
	for key := range schema {
		switch key {
		case "type", "description", "properties", "required", "additionalProperties", "items", "enum":
		default:
			return fmt.Errorf("不支持的 schema 关键词 %q", key)
		}
	}
	typ, ok := schema["type"].(string)
	if !ok {
		return fmt.Errorf("schema.type 必须是字符串")
	}
	switch typ {
	case "object", "array", "string", "number", "integer", "boolean", "null":
	default:
		return fmt.Errorf("不支持的数据类型 %q", typ)
	}
	if v, ok := schema["description"]; ok {
		if _, ok := v.(string); !ok {
			return fmt.Errorf("schema.description 必须是字符串")
		}
	}
	if v, ok := schema["additionalProperties"]; ok {
		if typ != "object" {
			return fmt.Errorf("非 object 不可声明 additionalProperties")
		}
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("additionalProperties 必须是 boolean")
		}
	}
	props := map[string]any{}
	if v, ok := schema["properties"]; ok {
		if typ != "object" {
			return fmt.Errorf("非 object 不可声明 properties")
		}
		var valid bool
		props, valid = v.(map[string]any)
		if !valid {
			return fmt.Errorf("properties 必须是 object")
		}
		for _, k := range sortedDataflowKeys(props) {
			sub, ok := props[k].(map[string]any)
			if !ok {
				return fmt.Errorf("属性 %s 缺少 schema", k)
			}
			if err := validateDataflowValueSchema(sub); err != nil {
				return fmt.Errorf("属性 %s: %w", k, err)
			}
		}
	}
	if v, ok := schema["required"]; ok {
		if typ != "object" {
			return fmt.Errorf("非 object 不可声明 required")
		}
		keys, err := dataflowStringList(v)
		if err != nil {
			return fmt.Errorf("required: %w", err)
		}
		seen := map[string]bool{}
		for _, k := range keys {
			if _, ok := props[k]; !ok || seen[k] {
				return fmt.Errorf("required 属性 %q 未声明或重复", k)
			}
			seen[k] = true
		}
	}
	if v, ok := schema["items"]; ok {
		if typ != "array" {
			return fmt.Errorf("非 array 不可声明 items")
		}
		sub, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("items 必须是 schema")
		}
		if err := validateDataflowValueSchema(sub); err != nil {
			return err
		}
	}
	if typ == "array" && schema["items"] == nil {
		return fmt.Errorf("array 必须声明 items")
	}
	if v, ok := schema["enum"]; ok {
		values, ok := v.([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("enum 必须是非空数组")
		}
		for _, value := range values {
			without := map[string]any{}
			for k, v := range schema {
				if k != "enum" {
					without[k] = v
				}
			}
			if err := validateDataflowValue(without, value); err != nil {
				return fmt.Errorf("enum 值不满足类型: %w", err)
			}
		}
	}
	return nil
}

func dataflowStringList(value any) ([]string, error) {
	if values, ok := value.([]string); ok {
		return values, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("必须是字符串数组")
	}
	var out []string
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("必须是字符串数组")
		}
		out = append(out, s)
	}
	return out, nil
}

func validateDataflowValue(schema map[string]any, value any) error {
	typ, _ := schema["type"].(string)
	wrong := func() error { return fmt.Errorf("值类型不满足 %s", typ) }
	switch typ {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return wrong()
		}
		props, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"]; ok {
			keys, err := dataflowStringList(required)
			if err != nil {
				return err
			}
			for _, k := range keys {
				if _, ok := object[k]; !ok {
					return fmt.Errorf("缺少结果字段 %s", k)
				}
			}
		}
		for _, k := range sortedDataflowKeys(object) {
			sub, exists := props[k]
			if !exists {
				if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
					return fmt.Errorf("未声明结果字段 %s", k)
				}
				continue
			}
			s, ok := sub.(map[string]any)
			if !ok {
				return fmt.Errorf("属性 schema 非法")
			}
			if err := validateDataflowValue(s, object[k]); err != nil {
				return fmt.Errorf("字段 %s: %w", k, err)
			}
		}
	case "array":
		values, ok := value.([]any)
		if !ok {
			return wrong()
		}
		sub, ok := schema["items"].(map[string]any)
		if !ok {
			return fmt.Errorf("array 缺少 items")
		}
		for i, v := range values {
			if err := validateDataflowValue(sub, v); err != nil {
				return fmt.Errorf("元素 %d: %w", i, err)
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return wrong()
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return wrong()
		}
	case "null":
		if value != nil {
			return wrong()
		}
	case "integer", "number":
		var number float64
		switch n := value.(type) {
		case json.Number:
			var err error
			number, err = n.Float64()
			if err != nil {
				return wrong()
			}
		case float64:
			number = n
		case int:
			number = float64(n)
		case int64:
			number = float64(n)
		default:
			return wrong()
		}
		if math.IsNaN(number) || math.IsInf(number, 0) || (typ == "integer" && math.Trunc(number) != number) {
			return wrong()
		}
	default:
		return fmt.Errorf("未声明值类型")
	}
	if options, ok := schema["enum"].([]any); ok {
		found := false
		for _, option := range options {
			a, ea := json.Marshal(option)
			b, eb := json.Marshal(value)
			if ea == nil && eb == nil && string(a) == string(b) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("值不在 enum 中")
		}
	}
	return nil
}

func dataflowPointerTokens(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("字段选择必须使用 JSON Pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	for i, p := range parts {
		var b strings.Builder
		for j := 0; j < len(p); j++ {
			if p[j] != '~' {
				b.WriteByte(p[j])
				continue
			}
			j++
			if j == len(p) || (p[j] != '0' && p[j] != '1') {
				return nil, fmt.Errorf("JSON Pointer 转义非法")
			}
			if p[j] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
		}
		parts[i] = b.String()
	}
	return parts, nil
}

func selectDataflowValue(value any, pointer string) (any, error) {
	parts, err := dataflowPointerTokens(pointer)
	if err != nil {
		return nil, err
	}
	for _, key := range parts {
		switch v := value.(type) {
		case map[string]any:
			var found bool
			value, found = v[key]
			if !found {
				return nil, fmt.Errorf("输入字段 %q 不存在", key)
			}
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(v) || strconv.Itoa(i) != key {
				return nil, fmt.Errorf("输入数组下标非法 %q", key)
			}
			value = v[i]
		default:
			return nil, fmt.Errorf("输入字段选择经过非容器值 %s", reflect.TypeOf(value))
		}
	}
	return value, nil
}
