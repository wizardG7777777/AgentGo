package graph

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"

	"agentgo/internal/pathutil"
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
	if contract.Profile == OutputContractProfileInvestigationBoundaryV1 ||
		contract.Profile == OutputContractProfileInvestigationBoundaryV2 {
		if err := validateInvestigationBoundaryOutput(contract.Profile, result); err != nil {
			return err
		}
	}
	return nil
}

// ValidateNodeOutputEvidence 把需要源码真实性的跨字段 profile 与当前 ProjectRoot
// 对账。普通/历史 OutputContract 不读取文件。investigation-boundary/v1/v2 只接受
// range 内逐字存在的 symbol，防止模型把尚未实现的候选方法伪装成已读证据。
func ValidateNodeOutputEvidence(contract *NodeOutputContract, projectRoot string, result map[string]any) error {
	if contract == nil || (contract.Profile != OutputContractProfileInvestigationBoundaryV1 &&
		contract.Profile != OutputContractProfileInvestigationBoundaryV2) {
		return nil
	}
	if strings.TrimSpace(projectRoot) == "" {
		return fmt.Errorf("typed output contract: investigation boundary 缺少 ProjectRoot authority")
	}
	fieldPaths := []string{
		"$.boundary_evidence.public_entry",
		"$.boundary_evidence.state_owner",
		"$.boundary_evidence.internal_consumer",
	}
	if contract.Profile == OutputContractProfileInvestigationBoundaryV2 {
		fieldPaths = append(fieldPaths, "$.failure_observation")
	}
	for _, fieldPath := range fieldPaths {
		value, ok := valueAtPath(result, fieldPath)
		entry, objectOK := value.(map[string]any)
		if !ok || !objectOK {
			return fmt.Errorf("typed output contract: %s 必须是对象", fieldPath)
		}
		relativePath, _ := entry["path"].(string)
		symbol, _ := entry["symbol"].(string)
		start, startOK := numericValue(entry["start_line"])
		end, endOK := numericValue(entry["end_line"])
		if !startOK || !endOK {
			return fmt.Errorf("typed output contract: %s 行范围非法", fieldPath)
		}
		resolved, err := pathutil.ValidatePath(relativePath, projectRoot)
		if err != nil {
			return fmt.Errorf("typed output contract: %s.path 非法: %w", fieldPath, err)
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return fmt.Errorf("typed output contract: 读取 %s.path 失败: %w", fieldPath, err)
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		startIndex, endIndex := int(start)-1, int(end)
		if startIndex < 0 || endIndex <= startIndex || endIndex > len(lines) {
			return fmt.Errorf("typed output contract: %s range=%d-%d 超出文件行数 %d", fieldPath, int(start), int(end), len(lines))
		}
		if !strings.Contains(strings.Join(lines[startIndex:endIndex], "\n"), strings.TrimSpace(symbol)) {
			return fmt.Errorf("typed output contract: %s.symbol=%q 未出现在声明 range 的真实源码", fieldPath, symbol)
		}
	}
	return nil
}

func validateInvestigationBoundaryOutput(profile string, result map[string]any) error {
	for _, path := range []string{"$.hypothesis", "$.recommended_change", "$.rejected_alternative"} {
		value, ok := valueAtPath(result, path)
		text, stringOK := value.(string)
		if !ok || !stringOK || strings.TrimSpace(text) == "" {
			return fmt.Errorf("typed output contract: %s 必须是非空字符串", path)
		}
	}

	filesValue, ok := valueAtPath(result, "$.evidence_files")
	if !ok {
		return fmt.Errorf("typed output contract: 缺少 $.evidence_files")
	}
	files := outputStringSet(filesValue)
	if len(files) == 0 {
		return fmt.Errorf("typed output contract: $.evidence_files 必须非空")
	}
	rangesValue, ok := valueAtPath(result, "$.evidence_ranges")
	if !ok {
		return fmt.Errorf("typed output contract: 缺少 $.evidence_ranges")
	}
	ranges := outputObjectSlice(rangesValue)
	if len(ranges) == 0 {
		return fmt.Errorf("typed output contract: $.evidence_ranges 必须非空")
	}

	distinct := make(map[string]struct{}, 3)
	for _, role := range []string{"public_entry", "state_owner", "internal_consumer"} {
		path := "$.boundary_evidence." + role
		value, exists := valueAtPath(result, path)
		entry, objectOK := value.(map[string]any)
		if !exists || !objectOK {
			return fmt.Errorf("typed output contract: %s 必须是对象", path)
		}
		file, _ := entry["path"].(string)
		symbol, _ := entry["symbol"].(string)
		start, startOK := numericValue(entry["start_line"])
		end, endOK := numericValue(entry["end_line"])
		file, symbol = strings.TrimSpace(file), strings.TrimSpace(symbol)
		if file == "" || symbol == "" || !startOK || !endOK || start < 1 || end < start || math.Trunc(start) != start || math.Trunc(end) != end {
			return fmt.Errorf("typed output contract: %s 必须给出非空 path/symbol 与合法 1-based start_line/end_line", path)
		}
		if _, exists := files[file]; !exists {
			return fmt.Errorf("typed output contract: %s.path=%q 不在 evidence_files", path, file)
		}
		matched := false
		for _, evidenceRange := range ranges {
			rangePath, _ := evidenceRange["path"].(string)
			rangeSymbol, _ := evidenceRange["symbol"].(string)
			rangeStart, rangeStartOK := numericValue(evidenceRange["start_line"])
			rangeEnd, rangeEndOK := numericValue(evidenceRange["end_line"])
			if rangeStartOK && rangeEndOK && strings.TrimSpace(rangePath) == file &&
				strings.TrimSpace(rangeSymbol) == symbol && rangeStart == start && rangeEnd == end {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("typed output contract: %s 必须逐字引用 evidence_ranges 中同一 path/symbol/range", path)
		}
		distinct[file+"\x00"+symbol] = struct{}{}
	}
	if len(distinct) < 2 {
		return fmt.Errorf("typed output contract: boundary_evidence 三个角色至少必须引用两个不同 path/symbol")
	}
	if profile == OutputContractProfileInvestigationBoundaryV2 {
		value, exists := valueAtPath(result, "$.failure_observation")
		entry, objectOK := value.(map[string]any)
		if !exists || !objectOK {
			return fmt.Errorf("typed output contract: $.failure_observation 必须是对象")
		}
		failureKind, _ := entry["failure_kind"].(string)
		if strings.TrimSpace(failureKind) == "" {
			return fmt.Errorf("typed output contract: $.failure_observation.failure_kind 必须非空")
		}
		if err := validateBoundaryEntryReference("$.failure_observation", entry, files, ranges); err != nil {
			return err
		}
	}
	return nil
}

func validateBoundaryEntryReference(fieldPath string, entry map[string]any, files map[string]struct{}, ranges []map[string]any) error {
	file, _ := entry["path"].(string)
	symbol, _ := entry["symbol"].(string)
	start, startOK := numericValue(entry["start_line"])
	end, endOK := numericValue(entry["end_line"])
	file, symbol = strings.TrimSpace(file), strings.TrimSpace(symbol)
	if file == "" || symbol == "" || !startOK || !endOK || start < 1 || end < start || math.Trunc(start) != start || math.Trunc(end) != end {
		return fmt.Errorf("typed output contract: %s 必须给出非空 path/symbol 与合法 1-based start_line/end_line", fieldPath)
	}
	if _, exists := files[file]; !exists {
		return fmt.Errorf("typed output contract: %s.path=%q 不在 evidence_files", fieldPath, file)
	}
	for _, evidenceRange := range ranges {
		rangePath, _ := evidenceRange["path"].(string)
		rangeSymbol, _ := evidenceRange["symbol"].(string)
		rangeStart, rangeStartOK := numericValue(evidenceRange["start_line"])
		rangeEnd, rangeEndOK := numericValue(evidenceRange["end_line"])
		if rangeStartOK && rangeEndOK && strings.TrimSpace(rangePath) == file &&
			strings.TrimSpace(rangeSymbol) == symbol && rangeStart == start && rangeEnd == end {
			return nil
		}
	}
	return fmt.Errorf("typed output contract: %s 必须逐字引用 evidence_ranges 中同一 path/symbol/range", fieldPath)
}

func outputStringSet(value any) map[string]struct{} {
	out := make(map[string]struct{})
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out[strings.TrimSpace(text)] = struct{}{}
			}
		}
	case []string:
		for _, item := range values {
			if strings.TrimSpace(item) != "" {
				out[strings.TrimSpace(item)] = struct{}{}
			}
		}
	}
	return out
}

func outputObjectSlice(value any) []map[string]any {
	switch values := value.(type) {
	case []any:
		out := make([]map[string]any, 0, len(values))
		for _, item := range values {
			if object, ok := item.(map[string]any); ok {
				out = append(out, object)
			}
		}
		return out
	case []map[string]any:
		return values
	default:
		return nil
	}
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
