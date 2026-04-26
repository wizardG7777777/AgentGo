package builtin

// stringSliceFromArg 把 hook args 中可能是 []string、[]any 或 nil 的字段
// 安全转成 []string。
//
// LLM tool-call args 经过 JSON 反序列化后，数组字段通常拿到 []any（每项是 string）；
// 但 Go 内部直接调用时也可能传 []string。两种形态都要支持。其它形态返回 nil（视为空）。
//
// 由 ValidateExpectedHashHook（互斥判空）和 ValidateLineAnchorsHook（取出做哈希校验）
// 共用——参见 nextUpgrade_v4.md §7.7 互斥规则。
func stringSliceFromArg(v any) []string {
	if v == nil {
		return nil
	}
	if ss, ok := v.([]string); ok {
		return ss
	}
	if sa, ok := v.([]any); ok {
		out := make([]string, 0, len(sa))
		for _, item := range sa {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
