package scheduler

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

func TestAgentRegistryRuntimeRoutesAreExactAndRemovable(t *testing.T) {
	reg := NewAgentRegistry()
	if reg.CanRoute("") {
		t.Fatal("empty registry must not invent a default route")
	}
	if err := reg.RegisterRoute("static:writer", "", "", 1, "writer", []string{"read_file", "write_file"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterRoute("team:verify", "team:verify", "plan-a", 1, "verifier", []string{"read_file", "submit_acceptance_result"}); err != nil {
		t.Fatal(err)
	}
	if !reg.CanRoute("") || !reg.CanRoute("", "write_file") {
		t.Fatal("default static route should be ready with its own capabilities")
	}
	if !reg.CanRoute("team:verify", "read_file", "submit_acceptance_result") {
		t.Fatal("verifier route should satisfy its complete capability set")
	}
	if reg.CanRoute("team:verify", "run_shell") {
		t.Fatal("route must not claim a capability it does not have")
	}
	if !reg.CanRouteForPlan("plan-a", "team:verify", "submit_acceptance_result") {
		t.Fatal("dynamic Team route should be ready for its owning Plan")
	}
	if reg.CanRouteForPlan("plan-b", "team:verify") || reg.CanRouteForPlan("", "team:verify") {
		t.Fatal("dynamic Team route must not be routable by another or unmanaged Plan")
	}
	if !reg.CanRouteForPlan("plan-a", "", "write_file") || !reg.CanRouteForPlan("plan-b", "", "write_file") {
		t.Fatal("static route should remain global across Plans")
	}
	if !reg.UnregisterRoute("team:verify") || reg.CanRoute("team:verify") {
		t.Fatal("unregistered dynamic route must stop being routable")
	}
}

func TestAgentRegistryDoesNotUnionCapabilitiesAcrossSameRoute(t *testing.T) {
	reg := NewAgentRegistry()
	if err := reg.RegisterRoute("kind:submitter", "verify", "", 1, "submitter", []string{"submit_acceptance_result"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterRoute("kind:shell", "verify", "", 1, "shell", []string{"run_shell"}); err != nil {
		t.Fatal(err)
	}
	if reg.CanRoute("verify", "submit_acceptance_result", "run_shell") {
		t.Fatal("capabilities from incompatible agents sharing a route must not be unioned")
	}
}

func TestAgentRegistryRequiresEveryListenerOnSharedRouteToBeCapable(t *testing.T) {
	reg := NewAgentRegistry()
	if err := reg.RegisterRoute("kind:verifier", "verify", "", 1, "verifier", []string{"submit_acceptance_result", "run_shell"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterRoute("kind:readonly", "verify", "", 1, "readonly", []string{"read_file"}); err != nil {
		t.Fatal(err)
	}
	if !reg.CanRoute("verify") {
		t.Fatal("shared route with ready listeners should remain routable for unprivileged tasks")
	}
	if reg.CanRoute("verify", "submit_acceptance_result", "run_shell") {
		t.Fatal("a weaker listener could claim the privileged task")
	}
	entries := reg.Specialized()
	if len(entries) != 1 || len(entries[0].Capabilities) != 0 {
		t.Fatalf("shared route snapshot must expose only guaranteed tools: %+v", entries)
	}
	if tools, ok := reg.RouteCapabilities("verify"); !ok || len(tools) != 0 {
		t.Fatalf("shared route guaranteed tools=%v ok=%v, want empty ready route", tools, ok)
	}
	if _, ok := reg.RouteCapabilities("missing"); ok {
		t.Fatal("missing route reported capabilities")
	}
}

func TestAgentRegistryPlanScopedViewsKeepStaticRoutesGlobalAndTeamsPrivate(t *testing.T) {
	reg := NewAgentRegistry()
	if err := reg.RegisterRoute("static:research", "research", "", 1, "global research", []string{"read_file"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterRoute("team:a", "team:a", "plan-a", 1, "Plan A team", []string{"read_file"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterRoute("team:b", "team:b", "plan-b", 1, "Plan B team", []string{"run_shell"}); err != nil {
		t.Fatal(err)
	}

	if got := reg.Specialized(); len(got) != 3 {
		t.Fatalf("global diagnostic view len=%d, want all 3 routes: %+v", len(got), got)
	}
	view := reg.SpecializedForPlan("plan-a")
	if len(view) != 2 || view[0].EventType != "research" || view[1].EventType != "team:a" {
		t.Fatalf("Plan A view must contain global static + own Team only: %+v", view)
	}
	if _, ok := reg.RouteCapabilitiesForPlan("plan-a", "team:b"); ok {
		t.Fatal("Plan B Team capabilities leaked into Plan A view")
	}
	if caps, ok := reg.RouteCapabilitiesForPlan("plan-b", "research"); !ok || !containsAll(caps, []string{"read_file"}) {
		t.Fatalf("global static capabilities unavailable to Plan B: caps=%v ok=%v", caps, ok)
	}
}

// Feature: agent-capability-declaration, Property 4: registry capabilities round-trip
// **Validates: Requirements 4.2, 4.3**
//
// 使用 rapid 生成随机 Register 调用序列，验证 Specialized() 返回结果中
// 每个 EventType 唯一、Capabilities 为最后一次注册值、Count 为累加值。
func TestProperty_AgentRegistryCapabilities(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		reg := NewAgentRegistry()

		// 生成随机 Register 调用序列
		numCalls := rapid.IntRange(1, 20).Draw(t, "numCalls")

		// 用有限的 EventType 池增加重复注册的概率
		eventTypes := make([]string, 0)
		numTypes := rapid.IntRange(1, 5).Draw(t, "numTypes")
		for i := range numTypes {
			eventTypes = append(eventTypes, rapid.StringMatching(`[a-z]{1,10}`).Draw(t, fmt.Sprintf("eventType_%d", i)))
		}

		// 记录每个 EventType 的期望状态
		type expected struct {
			lastCaps   []string // 最后一次注册的 Capabilities
			totalCount int      // 累加的 Count
			lastRole   string   // 最后一次非空 Role
		}
		expectedMap := make(map[string]*expected)

		for i := range numCalls {
			et := rapid.SampledFrom(eventTypes).Draw(t, fmt.Sprintf("call_%d_et", i))
			count := rapid.IntRange(1, 10).Draw(t, fmt.Sprintf("call_%d_count", i))

			// 随机生成 Capabilities: nil / empty / non-empty
			var caps []string
			capsChoice := rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("call_%d_capsChoice", i))
			switch capsChoice {
			case 0:
				caps = nil
			case 1:
				caps = []string{}
			case 2:
				n := rapid.IntRange(1, 4).Draw(t, fmt.Sprintf("call_%d_capsLen", i))
				caps = make([]string, n)
				for j := range n {
					caps[j] = rapid.StringMatching(`[a-z_]{1,10}`).Draw(t, fmt.Sprintf("call_%d_cap_%d", i, j))
				}
			}

			// 随机生成 Role: empty / non-empty
			role := ""
			if rapid.Bool().Draw(t, fmt.Sprintf("call_%d_hasRole", i)) {
				role = rapid.StringMatching(`[a-zA-Z ]{1,20}`).Draw(t, fmt.Sprintf("call_%d_role", i))
			}

			reg.Register(SpecializedAgent{
				EventType:    et,
				Count:        count,
				Capabilities: caps,
				Role:         role,
			})

			// 更新期望状态
			exp, exists := expectedMap[et]
			if !exists {
				exp = &expected{}
				expectedMap[et] = exp
			}
			exp.totalCount += count
			if caps != nil {
				exp.lastCaps = caps
			}
			if role != "" {
				exp.lastRole = role
			}
		}

		// 验证 Specialized() 输出
		result := reg.Specialized()

		// 1. 每个 EventType 恰好出现一次
		seen := make(map[string]bool)
		for _, sa := range result {
			if seen[sa.EventType] {
				t.Fatalf("duplicate EventType %q in Specialized() output", sa.EventType)
			}
			seen[sa.EventType] = true
		}

		// 2. 结果数量等于不同 EventType 的数量
		if len(result) != len(expectedMap) {
			t.Fatalf("Specialized() returned %d entries, want %d", len(result), len(expectedMap))
		}

		// 3. 验证每个 EventType 的 Capabilities、Count、Role
		for _, sa := range result {
			exp, ok := expectedMap[sa.EventType]
			if !ok {
				t.Fatalf("unexpected EventType %q in Specialized() output", sa.EventType)
			}

			// Count 为累加值
			if sa.Count != exp.totalCount {
				t.Errorf("EventType %q: Count = %d, want %d", sa.EventType, sa.Count, exp.totalCount)
			}

			// Capabilities 为最后一次注册值（如果所有注册都是 nil，则保持初始零值 nil）
			if exp.lastCaps == nil {
				if sa.Capabilities != nil {
					t.Errorf("EventType %q: Capabilities = %v, want nil", sa.EventType, sa.Capabilities)
				}
			} else {
				if sa.Capabilities == nil {
					t.Errorf("EventType %q: Capabilities = nil, want %v", sa.EventType, exp.lastCaps)
				} else if len(sa.Capabilities) != len(exp.lastCaps) {
					t.Errorf("EventType %q: Capabilities len = %d, want %d", sa.EventType, len(sa.Capabilities), len(exp.lastCaps))
				} else {
					for j, c := range sa.Capabilities {
						if c != exp.lastCaps[j] {
							t.Errorf("EventType %q: Capabilities[%d] = %q, want %q", sa.EventType, j, c, exp.lastCaps[j])
						}
					}
				}
			}

			// Role 为最后一次非空注册值
			if sa.Role != exp.lastRole {
				t.Errorf("EventType %q: Role = %q, want %q", sa.EventType, sa.Role, exp.lastRole)
			}
		}
	})
}
