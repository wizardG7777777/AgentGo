package config

// EffectiveAgentTools 返回显式声明的工具副本，不再自动补入观察、决策或检查工具。
func EffectiveAgentTools(allowed []string) []string {
	if allowed == nil {
		return nil // nil 保持“兼容允许全部”语义；framework control tools 已在全集中。
	}
	out := append([]string(nil), allowed...)
	return out
}

func containsTool(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
