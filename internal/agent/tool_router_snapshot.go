package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"agentgo/internal/llm"
)

// ToolRouterSnapshot 把一次 Invocation 模型可见的工具定义与随后真实 dispatch 的
// Registry 绑定为同一不可变视图。Registry 在启动/Attempt 边界构造后只读；Defs
// 使用值拷贝，调用方不能改写 registry 内部切片。
type ToolRouterSnapshot struct {
	ID       string
	Registry *ToolRegistry
	Defs     []llm.ToolDef
	Phase    string
	MaxCalls int
}

// FreezeToolRouterSnapshot 在一次 Execute 开始时捕获工具视图。nil registry 是
// 装配错误，不能降级为“模型无工具但 dispatch 仍有工具”。
func FreezeToolRouterSnapshot(registry *ToolRegistry) (ToolRouterSnapshot, error) {
	return FreezeToolRouterSnapshotWithPolicy(registry, "default", 16)
}

func FreezeToolRouterSnapshotWithPolicy(registry *ToolRegistry, phase string, maxCalls int) (ToolRouterSnapshot, error) {
	if registry == nil {
		return ToolRouterSnapshot{}, fmt.Errorf("ToolRouterSnapshot 缺少 ToolRegistry")
	}
	defs := registry.Defs()
	if expected, singleton := mechanicalSingletonTool(phase); singleton {
		if len(defs) != 1 || defs[0].Name != expected {
			return ToolRouterSnapshot{}, fmt.Errorf(
				"auto-singleton phase=%s 工具面必须精确为 [%s]，实际=%v",
				phase, expected, registry.Names())
		}
	}
	if maxCalls <= 0 {
		return ToolRouterSnapshot{}, fmt.Errorf("ToolRouterSnapshot max_calls=%d 非法", maxCalls)
	}
	encoded, err := json.Marshal(struct {
		Phase    string        `json:"phase"`
		MaxCalls int           `json:"max_calls"`
		Defs     []llm.ToolDef `json:"defs"`
	}{Phase: phase, MaxCalls: maxCalls, Defs: defs})
	if err != nil {
		return ToolRouterSnapshot{}, fmt.Errorf("序列化 model-visible tool specs 失败: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return ToolRouterSnapshot{
		ID:       "trs-" + hex.EncodeToString(digest[:6]),
		Registry: registry,
		Defs:     defs,
		Phase:    phase,
		MaxCalls: maxCalls,
	}, nil
}
