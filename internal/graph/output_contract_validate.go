package graph

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// ValidateNodeOutput mechanically validates a completed TaskOutcome against
// the activation-frozen typed contract. Failed/blocked never call this helper.
func ValidateNodeOutput(contract *NodeOutputContract, summary string, result map[string]any) error {
	if contract == nil {
		return nil // legacy Runtime node
	}
	if contract.SummaryRequired && strings.TrimSpace(summary) == "" {
		return fmt.Errorf("typed output contract: summary_required=true，但 summary 为空")
	}
	for _, field := range contract.Fields {
		value, exists := valueAtPath(result, field.Path)
		if !exists {
			if field.Required {
				return fmt.Errorf("typed output contract: 缺少 required 字段 %s", field.Path)
			}
			continue
		}
		if !outputValueMatchesType(value, field.Type) {
			return fmt.Errorf("typed output contract: 字段 %s 类型不匹配，期望 %s，实际 %T", field.Path, field.Type, value)
		}
	}
	return nil
}

func outputValueMatchesType(value any, want string) bool {
	switch want {
	case "any":
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		if value == nil {
			return false
		}
		kind := reflect.ValueOf(value).Kind()
		return kind == reflect.Map || kind == reflect.Struct
	case "array":
		if value == nil {
			return false
		}
		kind := reflect.ValueOf(value).Kind()
		return kind == reflect.Array || kind == reflect.Slice
	case "number":
		_, ok := numericValue(value)
		return ok
	case "integer":
		number, ok := numericValue(value)
		return ok && !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number
	default:
		return false
	}
}

func numericValue(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float32:
		return float64(number), true
	case float64:
		return number, true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
