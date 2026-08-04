package bootstrap

// capability_registry.go 实现 per-node 能力配置（NodeCapability）的路由认领检查面。
//
// 核心不变式：任务声明的节点工具子集 ⊆ 认领 runner 的工具白名单。
// 本注册表是判定「⊆」的事实源，经 store.SetCapabilityChecker 注入后由
// 两个检查点共用（双保险）：
//   - QueryAvailable：按 agentID 过滤可认领任务——无能力认领的任务对该
//     agent 不可见；
//   - ClaimTask：落锁前叠加同一检查——即使 QueryAvailable 漏放，落锁时仍被拦下。
//
// 事实源选择（不另造第二份真相）：
//  1. byAgent / byKind ← buildAgentRuntime 产出的 rt.AllowedTools。
//     该值就是 runner.New 注册工具时的过滤依据（resolveToolGroups 的 allowlist），
//     即 runner 的「真实生效白名单」，比任何配置再解析都可靠。
//  2. spawn ad-hoc ← spawn.Manager.KindOf + byKind：spawn/types.go 明确
//     AllowedTools 不可 override、始终继承 base kind，KindOf 是 spawn 包官方
//     身份解析出口（bootstrap 已用它给 userdef 做 per-kind 路由）。
//  3. 动态 Team ← scheduler.AgentRegistry.RouteCapabilities：team 包
//     不外露 agentID→tools 查询，但 provision 注册 route 的 capabilities 与
//     runner 白名单同出 prep.tmpl.Tools（manager.go 同一行数据源）；route 语义
//     还是「所有 ready listener 的交集」，恰好匹配「任一 listener 都可能认领」
//     的保守判定。

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"agentgo/internal/model"
)

// CapabilityChecker 是 store.SetCapabilityChecker 注入点的函数形态
// （与 A 方 store 契约同签名）。
type CapabilityChecker func(agentID string, task *model.Task) error

// capabilityRegistry 维护 agentID / kind → 工具白名单的线程安全索引。
// 装配期（Bootstrap 单 goroutine）填充，认领期（多 runner 并发）只读。
type capabilityRegistry struct {
	mu      sync.RWMutex
	byAgent map[string][]string
	byKind  map[string][]string

	// kindOf 解析 spawn ad-hoc agent 的 base kind（spawn.Manager.KindOf）；nil 跳过。
	kindOf func(agentID string) string
	// routeCaps 查询某 eventType 全部 ready listener 的能力交集
	// （scheduler.AgentRegistry.RouteCapabilities）；nil 跳过。
	routeCaps func(eventType string) ([]string, bool)
}

func newCapabilityRegistry() *capabilityRegistry {
	return &capabilityRegistry{
		byAgent: make(map[string][]string),
		byKind:  make(map[string][]string),
	}
}

// registerAgent 登记一个静态 runner 的生效白名单（InstanceID → rt.AllowedTools）。
func (r *capabilityRegistry) registerAgent(agentID string, tools []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byAgent[agentID] = append([]string(nil), tools...)
}

// registerKind 登记静态 kind 的白名单。同一 kind 的多个 replica 白名单相同，
// 重复登记幂等。
func (r *capabilityRegistry) registerKind(kind string, tools []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKind[kind] = append([]string(nil), tools...)
}

// checker 返回可注入 store.SetCapabilityChecker 的检查函数。
func (r *capabilityRegistry) checker() CapabilityChecker {
	return func(agentID string, task *model.Task) error {
		return r.check(agentID, task)
	}
}

func (r *capabilityRegistry) check(agentID string, task *model.Task) error {
	// 只约束显式声明了 tools 子集的任务；无能力声明的任务认领行为完全不变。
	if task == nil || task.Capability == nil || len(task.Capability.Tools) == 0 {
		return nil
	}
	// scheduler 自身不在白名单 registry 内：topo=solo 时它亲自执行任务，
	// 其工具面由 scheduler 固定装配而非 kind 声明——跳过检查（solo 语义）。
	if agentID == "scheduler" {
		return nil
	}
	required := task.Capability.Tools

	r.mu.RLock()
	allowed, ok := r.byAgent[agentID]
	r.mu.RUnlock()
	if ok {
		return capabilitySubsetError(agentID, task, required, allowed)
	}

	// spawn ad-hoc：白名单继承 base kind。
	if r.kindOf != nil {
		if kind := r.kindOf(agentID); kind != "" {
			r.mu.RLock()
			kindTools, ok := r.byKind[kind]
			r.mu.RUnlock()
			if ok {
				return capabilitySubsetError(agentID, task, required, kindTools)
			}
		}
	}

	// 动态 Team（及其他已注册 route）：按任务路由目标的能力交集判定。
	// 认领者必然是该 route 的 listener（eventType 匹配由认领层保证），
	// 交集语义保证「任意 listener 赢认领都满足 ⊆」。team 事件类型按 Team
	// 唯一（team:<id>），不做归属 scope 过滤也不会跨 Team 串扰。
	if r.routeCaps != nil {
		if allowed, ok := r.routeCaps(task.EventType); ok {
			return capabilitySubsetError(agentID, task, required, allowed)
		}
	}

	// fail-closed：认领者身份无法解析出任何白名单事实源时拒绝。
	// 选拒绝而非放行的理由：能力声明是 Scheduler 对节点执行边界的显式收缩，
	// 放行未知身份等于把收缩承诺交给无法验证的执行者；且本分支只在任务
	// 显式声明 tools 子集时才会到达，爆炸半径限于新特性本身，不影响既有任务。
	return fmt.Errorf("节点能力检查失败: agent %q 无已注册的工具白名单（任务 %s 声明 tools=[%s]），无法验证认领资格，按 fail-closed 拒绝",
		agentID, task.ID, strings.Join(required, ", "))
}

// capabilitySubsetError 校验 required ⊆ allowed，违例时返回带缺工具清单的错误。
func capabilitySubsetError(agentID string, task *model.Task, required, allowed []string) error {
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	var missing []string
	for _, name := range required {
		if !set[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("节点能力越界: agent %q 的工具白名单缺少 [%s]，无法认领声明了 tools=[%s] 的任务 %s",
		agentID, strings.Join(missing, ", "), strings.Join(required, ", "), task.ID)
}
