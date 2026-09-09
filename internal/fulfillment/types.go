package fulfillment

import (
	"fmt"
)

const (
	SchemaV1      = "agentgo.fulfillment/v1"
	SchemaV2      = "agentgo.fulfillment/v2"
	SchemaCurrent = SchemaV2
)

type Contract struct {
	RequireWorkspaceChange bool `json:"require_workspace_change,omitempty"`
}

type Record struct {
	Schema               string   `json:"schema"`
	WorkspaceRevisionRef string   `json:"workspace_revision_ref,omitempty"`
	EffectRefs           []string `json:"effect_refs,omitempty"`

	SatisfiedRequirementIDs []string `json:"satisfied_requirement_ids,omitempty"`
}

func (r Record) Validate(contract *Contract) error {
	if contract == nil {
		return nil
	}
	if r.Schema != SchemaCurrent {
		return fmt.Errorf("fulfillment schema=%q 无效", r.Schema)
	}
	if contract.RequireWorkspaceChange && (r.WorkspaceRevisionRef == "" || r.WorkspaceRevisionRef == "workspace:empty") {
		return fmt.Errorf("缺少 workspace change")
	}
	return nil
}
