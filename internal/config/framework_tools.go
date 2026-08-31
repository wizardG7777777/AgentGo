package config

// EffectiveAgentTools 把用户声明的业务工具与框架自动控制能力合成 Runner
// 的真实注册全集。Doctor、capability registry 与 runner 必须使用同一结果。
func EffectiveAgentTools(allowed []string) []string {
	if allowed == nil {
		return nil // nil 保持“兼容允许全部”语义；framework control tools 已在全集中。
	}
	out := append([]string(nil), allowed...)
	hasRunShell := false
	for _, name := range out {
		if name == "run_shell" {
			hasRunShell = true
		}
		if name == "record_observation_delta" {
			// 继续扫描 run_shell，以便派生 run_check。
		}
	}
	if !containsTool(out, "record_observation_delta") {
		out = append(out, "record_observation_delta")
	}
	if !containsTool(out, "submit_change_decision") {
		out = append(out, "submit_change_decision")
	}
	if hasRunShell && !containsTool(out, "run_check") {
		out = append(out, "run_check")
	}
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
