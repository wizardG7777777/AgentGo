package plan

// digest_capability_test.go 验证节点能力（NodeCapability）纳入 graph digest 的口径：
// 能力变化必须改变 digest（旧验收随之失效），而书写顺序 / 空值形态不得造成抖动。

import (
	"testing"

	"agentgo/internal/model"
)

func digestTestPlan(cap *model.NodeCapability) *model.Plan {
	return &model.Plan{
		ID:            "p1",
		CurrentNodeIDs: []string{"n1"},
		Nodes: map[string]model.PlanNode{
			"n1": {TaskID: "n1", Title: "t", Role: model.PlanNodeRoleImplementation, Capability: cap},
		},
	}
}

func TestComputeGraphDigest_CapabilityChangesDigest(t *testing.T) {
	without := ComputeGraphDigest(digestTestPlan(nil))
	withTools := ComputeGraphDigest(digestTestPlan(&model.NodeCapability{Tools: []string{"read_file"}}))
	withModel := ComputeGraphDigest(digestTestPlan(&model.NodeCapability{Model: "deepseek-r1"}))
	withOtherTools := ComputeGraphDigest(digestTestPlan(&model.NodeCapability{Tools: []string{"write_file"}}))

	if without == withTools {
		t.Error("新增 tools 能力声明必须改变 graph digest（旧验收应失效）")
	}
	if without == withModel {
		t.Error("新增 model 覆盖必须改变 graph digest")
	}
	if withTools == withOtherTools {
		t.Error("不同 tools 子集必须产生不同 digest")
	}
}

func TestComputeGraphDigest_CapabilityNormalization(t *testing.T) {
	base := ComputeGraphDigest(digestTestPlan(&model.NodeCapability{Tools: []string{"read_file", "web_fetch"}}))
	// 书写顺序不同 + 重复项：语义同一集合，digest 必须一致（防抖动误失效）。
	shuffled := ComputeGraphDigest(digestTestPlan(&model.NodeCapability{Tools: []string{"web_fetch", "read_file", "web_fetch"}}))
	if base != shuffled {
		t.Error("同一工具集合的不同书写顺序不得改变 digest")
	}
	// 空 non-nil 与 nil 归一：两者 digest 一致（兼容零值）。
	empty := ComputeGraphDigest(digestTestPlan(&model.NodeCapability{}))
	if empty != ComputeGraphDigest(digestTestPlan(nil)) {
		t.Error("空 Capability 与 nil 必须归一为同一 digest")
	}
}

func TestComputeWorkGraphDigest_CapabilityIncluded(t *testing.T) {
	without := ComputeWorkGraphDigest(digestTestPlan(nil))
	withCap := ComputeWorkGraphDigest(digestTestPlan(&model.NodeCapability{Tools: []string{"read_file"}}))
	if without == withCap {
		t.Error("work 口径同样纳入 capability（节点执行边界是工作图语义的一部分）")
	}
}
