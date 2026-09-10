package model

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/quick"
)

func TestLeaseV4DigestBindsModelAndHasNoObservationFields(t *testing.T) {
	if err := quick.Check(func(name string) bool {
		first := ExecutionLease{Schema: ExecutionLeaseSchemaCurrent, Model: name, BusinessTools: []string{"run_shell"}}
		second := first
		second.Model = name + "-changed"
		raw, err := json.Marshal(first)
		return err == nil && first.Schema == "agentgo.execution-lease/v4" && first.ComputeDigest() != second.ComputeDigest() && !strings.Contains(string(raw), "observation")
	}, nil); err != nil {
		t.Fatalf("租约摘要或字段不符合新契约：%v", err)
	}
}
