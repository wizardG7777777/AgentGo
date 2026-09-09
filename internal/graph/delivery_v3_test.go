package graph

import "testing"

func TestV3MutatingCapabilityAllowsGenericShell(t *testing.T) {
	doc := &GraphDocument{Schema: SchemaV3, Nodes: map[string]Node{
		"work": {Kind: KindAgent, ProgressContractRef: "progress:code-change/v6",
			Capability: &Capability{Isolation: IsolationWorkspace, Tools: []string{"apply_change", "run_shell"}}},
	}}
	if err := validateCapabilityShape(doc); err != nil {
		t.Fatalf("隔离工作区中的通用 Shell 应允许：%v", err)
	}
	doc.Nodes["work"] = Node{Kind: KindAgent, ProgressContractRef: "progress:code-change/v6",
		Capability: &Capability{Isolation: IsolationWorkspace, Tools: []string{"apply_change"}}}
	if err := validateCapabilityShape(doc); err != nil {
		t.Fatalf("Graph v3 mutating 受约束工具集被拒绝: %v", err)
	}
}

func TestV3DeliveryRequirementOnlyForMutatingProducer(t *testing.T) {
	doc := &GraphDocument{Schema: SchemaV3, Nodes: map[string]Node{
		"read": {Kind: KindAgent, ProgressContractRef: "progress:investigation/v2"},
	}}
	if doc.RequiresDelivery() {
		t.Fatal("read-only v3 图不得伪造 Delivery Transaction")
	}
	doc.Nodes["work"] = Node{Kind: KindAgent, ProgressContractRef: "progress:code-change/v6"}
	if !doc.RequiresDelivery() {
		t.Fatal("mutating v3 图必须要求 Delivery Transaction")
	}
}

func TestV3AcceptanceUsesProducerDeliveryWorkspace(t *testing.T) {
	spec := TaskSpec{DeliveryID: "delivery:one"}
	bindDeliveryWorkspace(Node{Kind: KindAcceptance}, &spec)
	if spec.Isolation != IsolationWorkspace {
		t.Fatalf("acceptance 必须在候选 Delivery workspace 中验证: %+v", spec)
	}
	work := TaskSpec{DeliveryID: "delivery:one"}
	bindDeliveryWorkspace(Node{Kind: KindAgent}, &work)
	if work.Isolation != "" {
		t.Fatalf("producer 的 isolation 应由冻结 capability 决定: %+v", work)
	}
}
