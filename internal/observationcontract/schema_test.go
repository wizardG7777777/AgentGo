package observationcontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParametersNeverEmitsEmptyEnum(t *testing.T) {
	for _, profile := range []SchemaProfile{
		{},
		{EvidenceRefs: []string{"tool-call:a"}},
		{OpenCandidateRefs: []string{"candidate:sha256:a"}},
		{EvidenceRefs: []string{"tool-call:a"}, OpenCandidateRefs: []string{"candidate:sha256:a"}, PostPredecessorEvidence: []string{"tool-call:b"}},
	} {
		data, err := json.Marshal(Parameters(profile))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"enum":[]`) {
			t.Fatalf("schema 含非法空 enum: %s", data)
		}
	}
}

func TestParametersUsesZeroContainerLimitsForMissingAuthority(t *testing.T) {
	properties := Parameters(SchemaProfile{})["properties"].(map[string]any)
	if properties["facts"].(map[string]any)["maxItems"] != 0 ||
		properties["resolved_candidates"].(map[string]any)["maxItems"] != 0 {
		t.Fatalf("空 authority 未冻结 maxItems=0: %#v", properties)
	}
}
