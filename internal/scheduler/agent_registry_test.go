package scheduler

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

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
