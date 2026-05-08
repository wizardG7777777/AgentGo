// Package spawn 是 v5 Phase 5 (S5+S6) 的 ad-hoc agent 生命周期管理器。
//
// S5 提供：
//   - base_kind 继承 + 受限 override 合并（buildAdhocRuntime）
//   - 唯一 EventType 路由（adhoc:<spawnID>）
//   - lifecycle: one_shot 销毁链路（订阅 task 终态事件，发现匹配立即取消 runner ctx）
//
// S6 由 reactor.userdef.spawn_agent 调用 Manager.Spawn 落地动词语义。
//
// 设计依据：docs/activate/ReactiveSystem.md §6.1.4 场景 3。
package spawn

import (
	"context"

	"agentgo/internal/llm"
)

// LLMFactory 用 model 名构造 llm.Client。bootstrap 通常用 buildKindLLMClient
// 的闭包；测试可注入 fake。
type LLMFactory func(model string) llm.Client

// ReactorSpawnMaxDepth 是 spawn_agent reactor 级联硬上限。
//
// 不进 YAML，避免用户误把稳定性护栏调没；取 5 落在 ReactiveSystem.md §6.2.4
// 建议的 3-5 范围上沿，给合法拆分留出空间，同时阻断无限 spawn 链。
const ReactorSpawnMaxDepth = 5

// SpawnRequest 是一次 ad-hoc agent 创建请求。
//
// BaseKind 必须命中 cfg.Agents 中已声明的 kind；Override 中的字段按"零值=不覆盖"
// 语义合并到从 BaseKind 派生的 AgentRuntimeConfig 上。
//
// 不可被 override 的字段（Kind / EventType / InstanceID / AllowedTools / Profile）
// 由 buildAdhocRuntime 强制——这些一旦 override 会破坏路由或工具集闭合。
type SpawnRequest struct {
	BaseKind               string
	Override               RuntimeOverride
	InitialTaskDescription string
	Lifecycle              string // "one_shot" 是 v5 仅支持的值；空串等同 one_shot
	SourceTaskID           string // 触发 spawn 的上游任务，仅用于 trace/排障
	Depth                  int    // 本次 spawn 后的 reactor 深度；根事件触发 spawn 时为 1
}

// RuntimeOverride 描述 spawn_agent.override 中允许覆盖的字段子集。
//
// 零值语义：每个字段零值 = "不覆盖，沿用 base_kind 配置"。SystemPromptSet 单独
// 标记 system prompt 是否被显式覆盖（区分"override 了空串"与"未 override"）。
type RuntimeOverride struct {
	SystemPrompt                 string
	SystemPromptSet              bool
	Model                        string
	AgentMaxLoops                int
	TaskMaxRetries               int
	EnforceCompactTokenThreshold int
	ContextLimit                 int
}

// SpawnHost 是 reactor.userdef.spawn_agent 消费的 spawn 接口。
// internal/spawn.Manager 实现该接口；测试可注入 fake。
type SpawnHost interface {
	Spawn(ctx context.Context, req SpawnRequest) (spawnID string, taskID string, err error)
}
