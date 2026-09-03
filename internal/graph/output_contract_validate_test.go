package graph

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateNodeOutput(t *testing.T) {
	contract := &NodeOutputContract{SummaryRequired: true, Fields: []OutputFieldContract{
		{Path: "$.changed", Type: "boolean", Required: true},
		{Path: "$.stats.count", Type: "integer", Required: true},
		{Path: "$.notes", Type: "array"},
	}}
	valid := map[string]any{"changed": true, "stats": map[string]any{"count": float64(2)}, "notes": []any{"ok"}}
	if err := ValidateNodeOutput(contract, "完成", valid); err != nil {
		t.Fatalf("合法 typed output 被拒绝: %v", err)
	}
	for name, test := range map[string]struct {
		summary string
		result  map[string]any
	}{
		"empty-summary": {summary: "", result: valid},
		"missing":       {summary: "完成", result: map[string]any{"changed": true}},
		"wrong-type":    {summary: "完成", result: map[string]any{"changed": "yes", "stats": map[string]any{"count": 2}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateNodeOutput(contract, test.summary, test.result); err == nil {
				t.Fatal("非法 typed output 必须拒绝")
			}
		})
	}
}

func TestValidateInvestigationBoundaryOutput(t *testing.T) {
	contract := &NodeOutputContract{
		SummaryRequired: true,
		Profile:         OutputContractProfileInvestigationBoundaryV1,
	}
	valid := func() map[string]any {
		return map[string]any{
			"hypothesis":           "公开 proxy 与内部保存路径共用公开属性，导致内部访问被误计数",
			"recommended_change":   "在状态所有者区分公开属性和内部背板",
			"rejected_alternative": "仅覆写叶子容器方法不能区分框架内部访问",
			"evidence_files":       []any{"src/public.go", "src/owner.go"},
			"evidence_ranges": []any{
				map[string]any{"path": "src/public.go", "symbol": "Proxy", "start_line": float64(10), "end_line": float64(20)},
				map[string]any{"path": "src/owner.go", "symbol": "Owner", "start_line": float64(30), "end_line": float64(50)},
			},
			"boundary_evidence": map[string]any{
				"public_entry":      map[string]any{"path": "src/public.go", "symbol": "Proxy", "start_line": float64(10), "end_line": float64(20)},
				"state_owner":       map[string]any{"path": "src/owner.go", "symbol": "Owner", "start_line": float64(30), "end_line": float64(50)},
				"internal_consumer": map[string]any{"path": "src/owner.go", "symbol": "Owner", "start_line": float64(30), "end_line": float64(50)},
			},
		}
	}
	if err := ValidateNodeOutput(contract, "边界调查完成", valid()); err != nil {
		t.Fatalf("合法 investigation boundary 被拒绝: %v", err)
	}
	v2Result := valid()
	v2Result["failure_observation"] = map[string]any{
		"path": "src/public.go", "symbol": "Proxy", "start_line": float64(10), "end_line": float64(20),
		"failure_kind": "assertion_failed",
	}
	contract.Profile = OutputContractProfileInvestigationBoundaryV2
	if err := ValidateNodeOutput(contract, "首失败与边界调查完成", v2Result); err != nil {
		t.Fatalf("合法 investigation boundary v2 被拒绝: %v", err)
	}
	v2Result["failure_observation"].(map[string]any)["failure_kind"] = " "
	if err := ValidateNodeOutput(contract, "首失败与边界调查完成", v2Result); err == nil {
		t.Fatal("v2 空 failure_kind 必须 fail-closed")
	}
	contract.Profile = OutputContractProfileInvestigationBoundaryV1

	for name, mutate := range map[string]func(map[string]any){
		"空反证": func(result map[string]any) { result["rejected_alternative"] = " " },
		"角色未引用范围": func(result map[string]any) {
			result["boundary_evidence"].(map[string]any)["internal_consumer"] =
				map[string]any{"path": "src/owner.go", "symbol": "Missing", "start_line": float64(30), "end_line": float64(50)}
		},
		"三个角色伪装为同一证据": func(result map[string]any) {
			one := map[string]any{"path": "src/public.go", "symbol": "Proxy", "start_line": float64(10), "end_line": float64(20)}
			result["boundary_evidence"] = map[string]any{
				"public_entry": one, "state_owner": one, "internal_consumer": one,
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := valid()
			mutate(result)
			if err := ValidateNodeOutput(contract, "边界调查完成", result); err == nil {
				t.Fatal("非法 investigation boundary 必须 fail-closed")
			}
		})
	}
}

func TestValidateInvestigationBoundaryEvidenceAgainstSource(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "src", "boundary.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("func PublicProxy() {}\ntype StateOwner struct{}\nfunc InternalConsumer() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := &NodeOutputContract{Profile: OutputContractProfileInvestigationBoundaryV1}
	result := map[string]any{"boundary_evidence": map[string]any{
		"public_entry": map[string]any{
			"path": "src/boundary.go", "symbol": "PublicProxy", "start_line": float64(1), "end_line": float64(1),
		},
		"state_owner": map[string]any{
			"path": "src/boundary.go", "symbol": "StateOwner", "start_line": float64(2), "end_line": float64(2),
		},
		"internal_consumer": map[string]any{
			"path": "src/boundary.go", "symbol": "InternalConsumer", "start_line": float64(3), "end_line": float64(3),
		},
	}}
	if err := ValidateNodeOutputEvidence(contract, root, result); err != nil {
		t.Fatalf("真实 boundary range 被拒绝: %v", err)
	}
	result["boundary_evidence"].(map[string]any)["public_entry"].(map[string]any)["symbol"] = "NotImplemented"
	if err := ValidateNodeOutputEvidence(contract, root, result); err == nil {
		t.Fatal("range 内不存在的候选 symbol 必须 fail-closed")
	}
}
