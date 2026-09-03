// Package observationcontract owns the single model-visible Observation schema.
// Tool registration, runtime authority projection, and provider probes must all
// use this builder so their wire contract cannot drift.
package observationcontract

import (
	"sort"

	"agentgo/internal/taskmem"
)

// SchemaProfile freezes the authority visible to one control invocation.
type SchemaProfile struct {
	EvidenceRefs            []string
	OpenCandidateRefs       []string
	PostPredecessorEvidence []string
}

// Parameters builds a fresh JSON Schema map. Empty authority is represented by
// maxItems=0 and never by the invalid JSON Schema keyword enum: [].
func Parameters(profile SchemaProfile) map[string]any {
	evidence := sortedUnique(profile.EvidenceRefs)
	candidates := sortedUnique(profile.OpenCandidateRefs)
	post := sortedUnique(profile.PostPredecessorEvidence)
	factEvidenceItems := stringAuthority(evidence)
	resolvedEvidenceItems := stringAuthority(post)
	candidate := stringAuthority(candidates)
	factSchema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "maxLength": taskmem.MaxObservationTextRunes,
				"description": "有界的工作 claim，不得写 reasoning；framework 仅核对 evidence 归属，按 inferred 保存"},
			"evidence_refs": map[string]any{"type": "array", "minItems": 1, "maxItems": 8,
				"items": factEvidenceItems, "description": "只能从当前 Invocation 的 settled evidence authority 中选择"},
		}, "required": []any{"text", "evidence_refs"},
	}
	resolvedSchema := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"candidate_ref": candidate,
			"evidence_refs": map[string]any{"type": "array", "minItems": 1, "maxItems": 8,
				"items": resolvedEvidenceItems, "description": "只能选择晚于 predecessor 的 settled evidence"},
		}, "required": []any{"candidate_ref", "evidence_refs"},
	}
	nextAction := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"decision": map[string]any{"type": "string", "enum": []any{
				taskmem.ObservationNextMutate, taskmem.ObservationNextContinue,
				taskmem.ObservationNextNeedContext, taskmem.ObservationNextVerify,
				taskmem.ObservationNextBlocked,
			}},
			"mutation": map[string]any{"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"tool": map[string]any{"type": "string", "enum": []any{"edit_file", "write_file"}},
					"path": map[string]any{"type": "string"},
				}, "required": []any{"tool", "path"}},
		}, "required": []any{"decision"},
		"allOf": []any{map[string]any{
			"if":   map[string]any{"properties": map[string]any{"decision": map[string]any{"const": taskmem.ObservationNextMutate}}},
			"then": map[string]any{"required": []any{"mutation"}},
		}},
	}
	params := map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"phase": map[string]any{"type": "string", "enum": []any{
				taskmem.ObservationPhaseInvestigate, taskmem.ObservationPhaseImplement,
				taskmem.ObservationPhaseVerify, taskmem.ObservationPhaseFinalize,
				taskmem.ObservationPhaseBlocked,
			}},
			"facts":               map[string]any{"type": "array", "maxItems": taskmem.MaxObservationFacts, "items": factSchema},
			"resolved_candidates": map[string]any{"type": "array", "maxItems": taskmem.MaxObservationNext, "items": resolvedSchema},
			"next_candidates": map[string]any{"type": "array", "maxItems": taskmem.MaxObservationNext,
				"items": map[string]any{"type": "string"}},
			"next_action": nextAction,
		}, "required": []any{"phase", "facts", "resolved_candidates", "next_candidates", "next_action"},
	}
	properties := params["properties"].(map[string]any)
	if len(evidence) == 0 {
		properties["facts"].(map[string]any)["maxItems"] = 0
	}
	if len(candidates) == 0 || len(post) == 0 {
		properties["resolved_candidates"].(map[string]any)["maxItems"] = 0
	}
	return params
}

func stringAuthority(values []string) map[string]any {
	out := map[string]any{"type": "string"}
	if len(values) > 0 {
		enum := make([]any, len(values))
		for i, value := range values {
			enum[i] = value
		}
		out["enum"] = enum
	}
	return out
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
