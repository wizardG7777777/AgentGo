package fulfillment

import (
	"fmt"
	"slices"
	"strings"
)

const SchemaV1 = "agentgo.fulfillment/v1"

type Contract struct {
	RequireWorkspaceChange bool     `json:"require_workspace_change,omitempty"`
	RequiredCheckIDs       []string `json:"required_check_ids,omitempty"`
}

type Record struct {
	Schema                  string   `json:"schema"`
	WorkspaceRevisionRef    string   `json:"workspace_revision_ref,omitempty"`
	EffectRefs              []string `json:"effect_refs,omitempty"`
	CheckRefs               []string `json:"check_refs,omitempty"`
	SatisfiedRequirementIDs []string `json:"satisfied_requirement_ids,omitempty"`
}

func (r Record) Validate(contract *Contract) error {
	if contract == nil {
		return nil
	}
	if r.Schema != SchemaV1 {
		return fmt.Errorf("fulfillment schema=%q 无效", r.Schema)
	}
	if contract.RequireWorkspaceChange && (r.WorkspaceRevisionRef == "" || r.WorkspaceRevisionRef == "workspace:empty") {
		return fmt.Errorf("缺少 workspace change")
	}
	for _, id := range contract.RequiredCheckIDs {
		if !slices.Contains(r.SatisfiedRequirementIDs, strings.TrimSpace(id)) {
			return fmt.Errorf("缺少 required check %q", id)
		}
	}
	return nil
}
