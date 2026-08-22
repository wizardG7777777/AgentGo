package graph

import (
	"fmt"
	"sort"
	"strings"
)

func validateGraphContractCoverage(contract GraphContract, body GraphDefinitionBody) []ValidationIssue {
	var issues []ValidationIssue
	if strings.TrimSpace(contract.RequestDigest) == "" {
		issues = append(issues, validationIssue("CONTRACT_REQUEST_DIGEST_REQUIRED", "contract.request_digest", true, "GraphContract 必须绑定 request_digest"))
	}
	if !contract.ExecutionClass.IsValid() {
		issues = append(issues, validationIssue("CONTRACT_EXECUTION_CLASS_INVALID", "contract.execution_class", true, fmt.Sprintf("execution_class=%q 非法", contract.ExecutionClass)))
	}
	if len(contract.Deliverables) == 0 {
		issues = append(issues, validationIssue("CONTRACT_DELIVERABLE_REQUIRED", "contract.deliverables", true, "GraphContract 至少声明一个交付物"))
	}
	if contract.ExecutionClass == ExecutionMutating && len(contract.RequiredEffects) == 0 {
		issues = append(issues, validationIssue("MUTATING_EFFECT_REQUIRED", "contract.required_effects", true, "mutating contract 必须声明 required effect"))
	}
	if (contract.ExecutionClass == ExecutionAnswer || contract.ExecutionClass == ExecutionReadOnly) && len(contract.RequiredEffects) > 0 {
		issues = append(issues, validationIssue("READONLY_EFFECT_FORBIDDEN", "contract.required_effects", true, "answer/read_only contract 不得要求副作用"))
	}

	requirements := map[string]map[string]struct{}{
		"deliverable": {}, "effect": {}, "artifact": {}, "check": {}, "evidence": {},
	}
	issues = append(issues, collectContractRequirementIDs("deliverable", "contract.deliverables", contract.Deliverables, requirements["deliverable"])...)
	issues = append(issues, collectContractRequirementIDs("artifact", "contract.required_artifacts", contract.RequiredArtifacts, requirements["artifact"])...)
	issues = append(issues, collectContractRequirementIDs("check", "contract.required_checks", contract.RequiredChecks, requirements["check"])...)
	issues = append(issues, collectContractRequirementIDs("evidence", "contract.success_evidence", contract.SuccessEvidence, requirements["evidence"])...)
	for i, effect := range contract.RequiredEffects {
		path := fmt.Sprintf("contract.required_effects[%d]", i)
		effect = strings.TrimSpace(effect)
		if effect == "" {
			issues = append(issues, validationIssue("CONTRACT_REQUIREMENT_ID_REQUIRED", path, true, "required effect 不能为空"))
			continue
		}
		if _, duplicate := requirements["effect"][effect]; duplicate {
			issues = append(issues, validationIssue("CONTRACT_REQUIREMENT_DUPLICATE", path, true, fmt.Sprintf("required effect %q 重复", effect)))
		}
		requirements["effect"][effect] = struct{}{}
	}

	owners := map[string]map[string]map[string]struct{}{
		"deliverable": {}, "effect": {}, "artifact": {}, "check": {}, "evidence": {},
	}
	for _, id := range sortedDefinitionNodeIDs(body) {
		node := body.Nodes[id]
		path := "nodes." + id + ".contract_bindings"
		issues = append(issues, collectNodeBindings(path+".deliverables", id, "deliverable", node.ContractBindings.Deliverables, requirements, owners)...)
		issues = append(issues, collectNodeBindings(path+".effects", id, "effect", node.ContractBindings.Effects, requirements, owners)...)
		issues = append(issues, collectNodeBindings(path+".artifacts", id, "artifact", node.ContractBindings.Artifacts, requirements, owners)...)
		issues = append(issues, collectNodeBindings(path+".checks", id, "check", node.ContractBindings.Checks, requirements, owners)...)
		issues = append(issues, collectNodeBindings(path+".success_evidence", id, "evidence", node.ContractBindings.SuccessEvidence, requirements, owners)...)

		if node.Kind == KindEnd && hasAnyContractBinding(node.ContractBindings) {
			issues = append(issues, validationIssue("END_CONTRACT_BINDING_FORBIDDEN", path, true, "end 不产生交付物/Effect/Artifact/Check/Evidence，不得承接 Contract binding"))
		}
		if (contract.ExecutionClass == ExecutionAnswer || contract.ExecutionClass == ExecutionReadOnly) && hasMutatingCapability(node.Capability) {
			issues = append(issues, validationIssue("READONLY_WRITE_CAPABILITY_FORBIDDEN", "nodes."+id+".capability.tools", true, "answer/read_only contract 的节点不得声明写入或副作用工具"))
		}
	}

	for _, category := range []string{"deliverable", "effect", "artifact", "check", "evidence"} {
		keys := make([]string, 0, len(requirements[category]))
		for key := range requirements[category] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			bound := owners[category][key]
			if len(bound) == 0 {
				issues = append(issues, validationIssue("CONTRACT_REQUIREMENT_UNBOUND", "contract", true, fmt.Sprintf("%s requirement %q 未绑定任何节点", category, key)))
				continue
			}
			if successReachableAvoiding(body, bound) {
				issues = append(issues, validationIssue("CONTRACT_REQUIREMENT_BYPASSED", "contract", true, fmt.Sprintf("存在绕过 %s requirement %q 的 success path", category, key)))
			}
			if category == "deliverable" || category == "artifact" || category == "evidence" {
				for nodeID := range bound {
					node := body.Nodes[nodeID]
					if node.Kind != KindEnd && node.OutputContract == nil {
						issues = append(issues, validationIssue("CONTRACT_OUTPUT_MAPPING_REQUIRED", "nodes."+nodeID+".output_contract", true, fmt.Sprintf("节点绑定 %s %q 但没有 typed OutputContract", category, key)))
					}
				}
			}
		}
	}

	if contract.RequiresAcceptance {
		acceptanceNodes := make(map[string]struct{})
		for id, node := range body.Nodes {
			if node.Kind == KindAcceptance {
				acceptanceNodes[id] = struct{}{}
			}
		}
		if len(acceptanceNodes) == 0 {
			issues = append(issues, validationIssue("CONTRACT_ACCEPTANCE_REQUIRED", "contract.requires_acceptance", true, "Contract 要求 acceptance，但 Definition 没有 acceptance 节点"))
		} else if successReachableAvoiding(body, acceptanceNodes) {
			issues = append(issues, validationIssue("CONTRACT_ACCEPTANCE_BYPASSED", "contract.requires_acceptance", true, "存在未经过 acceptance 的 success path"))
		}
	}
	return issues
}

func collectContractRequirementIDs(category, path string, items []ContractRequirement, dst map[string]struct{}) []ValidationIssue {
	var issues []ValidationIssue
	for i, item := range items {
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		id := strings.TrimSpace(item.ID)
		if id == "" || strings.TrimSpace(item.Kind) == "" {
			issues = append(issues, validationIssue("CONTRACT_REQUIREMENT_ID_REQUIRED", itemPath, true, fmt.Sprintf("%s requirement 必须有非空 id/kind", category)))
			continue
		}
		if _, duplicate := dst[id]; duplicate {
			issues = append(issues, validationIssue("CONTRACT_REQUIREMENT_DUPLICATE", itemPath+".id", true, fmt.Sprintf("%s requirement id=%q 重复", category, id)))
		}
		dst[id] = struct{}{}
	}
	return issues
}

func collectNodeBindings(path, nodeID, category string, values []string, requirements map[string]map[string]struct{}, owners map[string]map[string]map[string]struct{}) []ValidationIssue {
	var issues []ValidationIssue
	seen := make(map[string]struct{}, len(values))
	for i, raw := range values {
		value := strings.TrimSpace(raw)
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		if _, duplicate := seen[value]; duplicate {
			issues = append(issues, validationIssue("CONTRACT_BINDING_DUPLICATE", itemPath, true, fmt.Sprintf("binding %q 重复", value)))
			continue
		}
		seen[value] = struct{}{}
		if _, exists := requirements[category][value]; !exists {
			issues = append(issues, validationIssue("CONTRACT_BINDING_UNKNOWN", itemPath, true, fmt.Sprintf("binding %q 未在 GraphContract %s requirements 中声明", value, category)))
			continue
		}
		if owners[category][value] == nil {
			owners[category][value] = make(map[string]struct{})
		}
		owners[category][value][nodeID] = struct{}{}
	}
	return issues
}

func successReachableAvoiding(body GraphDefinitionBody, blocked map[string]struct{}) bool {
	if _, denied := blocked[body.Root]; denied {
		return false
	}
	queue := []string{body.Root}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		node, ok := body.Nodes[id]
		if !ok {
			continue
		}
		if node.Kind == KindEnd && node.EndOutcome == DefinitionEndSuccess {
			return true
		}
		for _, edge := range node.Next {
			if _, denied := blocked[edge.To]; denied {
				continue
			}
			queue = append(queue, edge.To)
		}
	}
	return false
}

func hasAnyContractBinding(binding GraphContractBindings) bool {
	return len(binding.Deliverables)+len(binding.Effects)+len(binding.Artifacts)+len(binding.Checks)+len(binding.SuccessEvidence) > 0
}

func hasMutatingCapability(capability *Capability) bool {
	if capability == nil {
		return false
	}
	mutating := map[string]struct{}{
		"write_file": {}, "edit_file": {}, "run_shell": {}, "send_message": {},
		"publish_task": {}, "request_user_input": {}, "request_replan": {},
	}
	for _, tool := range capability.Tools {
		if _, exists := mutating[tool]; exists {
			return true
		}
	}
	return false
}
