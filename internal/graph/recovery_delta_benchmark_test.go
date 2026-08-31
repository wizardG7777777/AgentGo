package graph

import "testing"

func BenchmarkDecodeRecoveryDeltaV2(b *testing.B) {
	result := map[string]any{"recovery_delta": map[string]any{
		"schema":                       RecoveryDeltaSchemaV2,
		"source_checkpoint_ref":        "checkpoint:source",
		"source_observation_delta_ref": "observation:source",
		"failure_fingerprint":          "failure:source",
		"changed_dimensions":           []any{"strategy", "context"},
		"strategy":                     "复用冻结事实并从目标文件开始",
		"first_action": map[string]any{
			"tool": "read_file", "path": "src/flask/app.py",
		},
		"expected_milestone": "形成最小修改并进入检查",
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := decodeRecoveryDelta(result); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeRecoveryDeltaV3(b *testing.B) {
	result := map[string]any{"recovery_delta": map[string]any{
		"schema": RecoveryDeltaSchemaV3, "source_checkpoint_ref": "checkpoint:source",
		"source_observation_delta_ref": "observation:source", "failure_fingerprint": "failure:source",
		"changed_dimensions": []any{"strategy", "context"},
		"strategy":           "从冻结目标读集进入 mutation handoff",
		"first_action": map[string]any{
			"tool": "read_file", "path": "src/flask/app.py",
		},
		"expected_milestone": "形成最小修改并进入检查",
	}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := decodeRecoveryDelta(result); err != nil {
			b.Fatal(err)
		}
	}
}
