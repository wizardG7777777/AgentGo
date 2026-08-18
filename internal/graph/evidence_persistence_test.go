package graph

import (
	"strings"
	"testing"
)

// TestActivationResultStructuredEvidencePersists 验证 verifier 消费的结构化证据
// 字段随 activation Result journal 跨重启保真；Resolve 返回深拷贝，调用方不能
// 通过 Success/ExitCode 指针改写 Store 权威事实。
func TestActivationResultStructuredEvidencePersists(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mustSubmit(t, s, tinyDocJSON)
	success := false
	exit := 17
	record := ActivationResult{
		NodeID: "a", ActivationID: "a@1", Result: []byte(`{"status":"failed"}`),
		Evidence: []EvidenceEntry{{
			Ref: "ev:task-1:call:0123456789abcdef", Kind: "shell", Summary: "测试失败",
			CallID: "call-test", ToolName: "run_shell", Success: &success,
			Command: "go test ./...", ExitCode: &exit,
			Path: "artifacts/test.log",
		}},
	}
	if err := s.RecordActivationResult("g1", record); err != nil {
		t.Fatalf("RecordActivationResult: %v", err)
	}
	transition := TransitionRecord{
		SourceNodeID: "a", SourceActivationID: "a@1", TransitionID: 0,
		TargetNodeID: "b", TargetActivationID: "b@1",
		Input: &EdgeInput{
			ResultRef: activationResultRef("g1", "a@1"), Result: []byte(`{"status":"failed"}`),
			Evidence: record.Evidence, EvidenceRefs: []string{record.Evidence[0].Ref},
		},
	}
	if err := s.RecordTransition("g1", transition, 0); err != nil {
		t.Fatalf("RecordTransition: %v", err)
	}
	selected, ok := s.HasTransition("g1", "a@1", 0)
	if !ok || selected.Input == nil {
		t.Fatalf("HasTransition: ok=%v transition=%+v", ok, selected)
	}
	*selected.Input.Evidence[0].Success = true
	selected.Input.Result[0] = '['
	selected.Input.EvidenceRefs[0] = "tampered"
	selectedAgain, _ := s.HasTransition("g1", "a@1", 0)
	if selectedAgain.Input == nil || selectedAgain.Input.Evidence[0].Success == nil || *selectedAgain.Input.Evidence[0].Success ||
		string(selectedAgain.Input.Result) != `{"status":"failed"}` || selectedAgain.Input.EvidenceRefs[0] != record.Evidence[0].Ref {
		t.Fatalf("Transition/Input/Evidence 查询必须深拷贝: %+v", selectedAgain.Input)
	}
	oversized := record
	oversized.ActivationID = "a@2"
	oversized.Ref = ""
	oversized.Evidence = []EvidenceEntry{{
		Ref: "ev:task-1:call:oversized", Kind: "shell",
		Command: strings.Repeat("x", EvidenceCommandMaxRunes+1),
	}}
	if err := s.RecordActivationResult("g1", oversized); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("Store 必须拒绝越界结构化 Evidence 字段，err=%v", err)
	}
	ref := activationResultRef("g1", "a@1")
	before, ok := s.ResolveActivationResult("g1", ref)
	if !ok || len(before.Evidence) != 1 {
		t.Fatalf("ResolveActivationResult: ok=%v result=%+v", ok, before)
	}
	*before.Evidence[0].Success = true
	*before.Evidence[0].ExitCode = 0
	again, ok := s.ResolveActivationResult("g1", ref)
	if !ok || again.Evidence[0].Success == nil || *again.Evidence[0].Success ||
		again.Evidence[0].ExitCode == nil || *again.Evidence[0].ExitCode != 17 {
		t.Fatalf("Resolve 必须深拷贝结构化证据指针: %+v", again.Evidence)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ns, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore(restart): %v", err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if err := ns.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	recovered, ok := ns.ResolveActivationResult("g1", ref)
	if !ok || len(recovered.Evidence) != 1 {
		t.Fatalf("恢复后 Result/Evidence 缺失: ok=%v result=%+v", ok, recovered)
	}
	evidence := recovered.Evidence[0]
	if evidence.CallID != "call-test" || evidence.ToolName != "run_shell" ||
		evidence.Success == nil || *evidence.Success || evidence.Command != "go test ./..." ||
		evidence.ExitCode == nil || *evidence.ExitCode != 17 || evidence.Path != "artifacts/test.log" {
		t.Fatalf("结构化 Evidence JSON 往返失真: %+v", evidence)
	}
	transitions := ns.Transitions("g1")
	if len(transitions) != 1 || transitions[0].Input == nil || len(transitions[0].Input.Evidence) != 1 ||
		transitions[0].Input.Evidence[0].ExitCode == nil || *transitions[0].Input.Evidence[0].ExitCode != 17 {
		t.Fatalf("Transition Input 的结构化 Evidence 恢复失真: %+v", transitions)
	}
}
