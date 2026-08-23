package graph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// validateMinimumDefinition 执行事务化 authoring 的最小合法图规则。它刻意
// 不进入 legacy ParseAndValidate：旧 v1/v2 execution 仍可恢复，新 Definition
// commit 才接受这些更强约束。
func validateMinimumDefinition(body GraphDefinitionBody) []ValidationIssue {
	var issues []ValidationIssue
	if len(body.Nodes) == 0 {
		return []ValidationIssue{validationIssue("NODE_REQUIRED", "nodes", true, "Definition 至少需要一个节点")}
	}

	root, rootExists := body.Nodes[body.Root]
	if rootExists {
		switch root.Kind {
		case KindEnd:
			issues = append(issues, validationIssue("ROOT_CANNOT_BE_END", "root", true, "新 Definition 的 root 不得为 end"))
		case KindJoin, KindAcceptance:
			issues = append(issues, validationIssue("ROOT_KIND_NOT_STARTABLE", "root", true, fmt.Sprintf("root kind=%s 需要上游输入，不能作为初始 activation", root.Kind)))
		case KindRouter:
			// GraphContract 尚无 typed initial-input 字段；在能够机械证明输入齐备前
			// router root fail-closed。后续新增输入契约后只放开有完整输入的情形。
			issues = append(issues, validationIssue("ROOT_ROUTER_INPUT_REQUIRED", "root", true, "router root 需要 GraphContract typed initial input，当前 Definition 未声明"))
		}
	}

	endCount, successEndCount, workCount := 0, 0, 0
	inbound := make(map[string]int)
	for _, id := range sortedDefinitionNodeIDs(body) {
		node := body.Nodes[id]
		path := "nodes." + id
		for _, edge := range node.Next {
			inbound[edge.To]++
		}
		if isWorkProducingKind(node.Kind) {
			workCount++
		}
		if node.Kind == KindSubgraph {
			issues = append(issues, validationIssue("SUBGRAPH_AUTHORING_DISABLED", path+".kind", true, "Scheduler 新 Definition 暂不 author subgraph；Runtime legacy 能力保留"))
		}
		if len(node.Extensions) > 0 {
			issues = append(issues, validationIssue("UNKNOWN_EXTENSION", path+".extensions", true, "新 Definition 不接受未注册 extension"))
		}
		if node.Kind == KindEnd {
			endCount++
			if !node.EndOutcome.IsValid() {
				issues = append(issues, validationIssue("END_OUTCOME_REQUIRED", path+".end_outcome", true, "end 必须声明 success/failed/blocked/cancelled outcome"))
			}
			if node.EndOutcome == DefinitionEndSuccess {
				successEndCount++
			}
		} else if node.EndOutcome != "" {
			issues = append(issues, validationIssue("END_OUTCOME_FORBIDDEN", path+".end_outcome", true, "只有 end 节点可以声明 end_outcome"))
		}

		if isTaskProducingDefinitionNode(node) {
			if node.Task == nil || strings.TrimSpace(node.Task.Description) == "" {
				issues = append(issues, validationIssue("TASK_DESCRIPTION_REQUIRED", path+".task.description", true, "task-producing 节点必须写明任务与输出语义"))
			}
			issues = append(issues, validateNodeOutputContract(path, node.OutputContract)...)
			if node.ProgressContractRef == "" {
				issues = append(issues, validationIssue("PROGRESS_CONTRACT_REF_REQUIRED", path+".progress_contract_ref", true, "task-producing 节点必须绑定 ProgressContractRef"))
			}
			if node.ContextPolicyRef == "" {
				issues = append(issues, validationIssue("CONTEXT_POLICY_REF_REQUIRED", path+".context_policy_ref", true, "task-producing 节点必须绑定 ContextPolicyRef"))
			}
			issues = append(issues, validateTaskOutletCoverage(id, node)...)
			if ControllerRole(node.Metadata[MetadataControllerRole]) == ControllerRoleLoopRecovery &&
				!outputContractHasRequiredString(node.OutputContract, "$.decision") {
				issues = append(issues, validationIssue("RECOVERY_DECISION_CONTRACT_REQUIRED",
					path+".output_contract.fields", true,
					"loop_recovery controller 必须声明 required string 字段 $.decision（retry|blocked）"))
			}
			if ControllerRole(node.Metadata[MetadataControllerRole]) == ControllerRoleLoopRecovery &&
				(node.Task == nil || len(node.Task.RequiredInputs) != 1 || node.Task.RequiredInputs[0] != "failure_context") {
				issues = append(issues, validationIssue("RECOVERY_FAILURE_CONTEXT_REQUIRED",
					path+".task.required_inputs", true,
					"loop_recovery controller 必须且只能声明 required input failure_context"))
			}
		}
	}

	if endCount == 0 {
		issues = append(issues, validationIssue("END_NODE_REQUIRED", "nodes", true, "Definition 至少需要一个 end"))
	}
	if successEndCount == 0 {
		issues = append(issues, validationIssue("SUCCESS_END_REQUIRED", "nodes", true, "Definition 至少需要一个 outcome=success 的 end"))
	}
	if workCount == 0 {
		issues = append(issues, validationIssue("WORK_NODE_REQUIRED", "nodes", true, "Definition 至少需要一个真正执行工作的 activation-capable 节点"))
	}
	for _, id := range sortedDefinitionNodeIDs(body) {
		node := body.Nodes[id]
		if node.Kind == KindEnd && node.EndOutcome == DefinitionEndSuccess && inbound[id] == 0 {
			issues = append(issues, validationIssue("SUCCESS_END_WITHOUT_RESULT", "nodes."+id, true, "success end 必须消费至少一条上游 ResultRef"))
		}
	}

	canReachEnd := nodesThatCanReachAnyEnd(body)
	for _, id := range sortedDefinitionNodeIDs(body) {
		if !canReachEnd[id] {
			issues = append(issues, validationIssue("NODE_CANNOT_REACH_END", "nodes."+id, true, fmt.Sprintf("节点 %s 在结构上无法到达任何 end", id)))
		}
	}
	issues = append(issues, validateCyclicSCCExits(body)...)
	if hasZeroWorkSuccessPath(body) {
		issues = append(issues, validationIssue("ZERO_WORK_SUCCESS_PATH", "root", true, "存在未经过任何 work-producing activation 的 success 路径"))
	}
	return issues
}

func outputContractHasRequiredString(contract *NodeOutputContract, path string) bool {
	if contract == nil {
		return false
	}
	for _, field := range contract.Fields {
		if field.Path == path && field.Type == "string" && field.Required {
			return true
		}
	}
	return false
}

func validateNodeOutputContract(path string, contract *NodeOutputContract) []ValidationIssue {
	if contract == nil {
		return []ValidationIssue{validationIssue("OUTPUT_CONTRACT_REQUIRED", path+".output_contract", true, "task-producing 节点必须声明 typed OutputContract")}
	}
	var issues []ValidationIssue
	if !contract.SummaryRequired && len(contract.Fields) == 0 {
		issues = append(issues, validationIssue("OUTPUT_CONTRACT_EMPTY", path+".output_contract", true, "OutputContract 必须要求 summary 或至少一个 result 字段"))
	}
	seen := make(map[string]struct{}, len(contract.Fields))
	allowedTypes := map[string]struct{}{"any": {}, "string": {}, "number": {}, "integer": {}, "boolean": {}, "object": {}, "array": {}}
	for i, field := range contract.Fields {
		fieldPath := fmt.Sprintf("%s.output_contract.fields[%d]", path, i)
		if !strings.HasPrefix(field.Path, "$.") || len(field.Path) <= 2 {
			issues = append(issues, validationIssue("OUTPUT_FIELD_PATH_INVALID", fieldPath+".path", true, "输出字段 path 必须为 $.field 形态"))
		}
		if _, duplicate := seen[field.Path]; duplicate {
			issues = append(issues, validationIssue("OUTPUT_FIELD_DUPLICATE", fieldPath+".path", true, fmt.Sprintf("输出字段 %s 重复", field.Path)))
		}
		seen[field.Path] = struct{}{}
		if _, ok := allowedTypes[field.Type]; !ok {
			issues = append(issues, validationIssue("OUTPUT_FIELD_TYPE_INVALID", fieldPath+".type", true, fmt.Sprintf("输出字段 type=%q 不受支持", field.Type)))
		}
	}
	return issues
}

func validateTaskOutletCoverage(id string, node GraphDefinitionNode) []ValidationIssue {
	coveredCompleted, coveredFailed, coveredBlocked := false, false, false
	verdicts := make(map[string]bool)
	for _, edge := range node.Next {
		if edge.When == nil || edge.When.Event == EventAlways {
			coveredCompleted, coveredFailed, coveredBlocked = true, true, true
			continue
		}
		if edge.When.Path != "" {
			coveredCompleted = true
			if node.Kind == KindAcceptance && edge.When.Path == "$.verdict" && edge.When.Operator == OpEq {
				var verdict string
				if json.Unmarshal(edge.When.Value, &verdict) == nil {
					verdicts[verdict] = true
				}
			}
			continue
		}
		switch edge.When.Event {
		case EventCompleted:
			coveredCompleted = true
		case EventFailed:
			coveredFailed = true
		case EventBlocked:
			coveredBlocked = true
		}
	}
	path := "nodes." + id + ".next"
	var issues []ValidationIssue
	if !coveredCompleted {
		issues = append(issues, validationIssue("COMPLETED_OUTLET_REQUIRED", path, true, "task-producing 节点缺少 completed/result 出口"))
	}
	if !coveredFailed {
		issues = append(issues, validationIssue("FAILED_OUTLET_REQUIRED", path, true, "task-producing 节点缺少 Runtime failed 出口"))
	}
	if !coveredBlocked {
		issues = append(issues, validationIssue("BLOCKED_OUTLET_REQUIRED", path, true, "task-producing 节点缺少 Runtime blocked 出口"))
	}
	if node.Kind == KindAcceptance {
		for _, verdict := range []string{"pass", "fixable", "failed"} {
			if !verdicts[verdict] {
				issues = append(issues, validationIssue("ACCEPTANCE_VERDICT_OUTLET_REQUIRED", path, true, fmt.Sprintf("acceptance 缺少 $.verdict eq %s 出口", verdict)))
			}
		}
	}
	return issues
}

func validateDefinitionPolicies(body GraphDefinitionBody, resolver DefinitionPolicyResolver) []ValidationIssue {
	var issues []ValidationIssue
	needsAuthority := false
	for _, id := range sortedDefinitionNodeIDs(body) {
		node := body.Nodes[id]
		if !isTaskProducingDefinitionNode(node) {
			continue
		}
		if node.ProgressContractRef != "" {
			needsAuthority = true
			if resolver != nil && !resolver.HasProgressContract(node.ProgressContractRef) {
				issues = append(issues, validationIssue("PROGRESS_CONTRACT_REF_UNKNOWN", "nodes."+id+".progress_contract_ref", true, fmt.Sprintf("未知 ProgressContractRef %q", node.ProgressContractRef)))
			}
		}
		if node.ContextPolicyRef != "" {
			needsAuthority = true
			if resolver != nil && !resolver.HasContextPolicy(node.ContextPolicyRef) {
				issues = append(issues, validationIssue("CONTEXT_POLICY_REF_UNKNOWN", "nodes."+id+".context_policy_ref", true, fmt.Sprintf("未知 ContextPolicyRef %q", node.ContextPolicyRef)))
			}
		}
	}
	if needsAuthority && resolver == nil {
		issues = append(issues, validationIssue("POLICY_AUTHORITY_UNAVAILABLE", "nodes", false, "无法核验 ProgressContractRef/ContextPolicyRef，按 fail-closed 拒绝"))
	}
	return issues
}

func isTaskProducingDefinitionNode(node GraphDefinitionNode) bool {
	return node.Kind == KindController || node.Kind == KindAgent || node.Kind == KindAcceptance
}

func isWorkProducingKind(kind NodeKind) bool {
	switch kind {
	case KindController, KindAgent, KindTool, KindApproval, KindWaitEvent, KindAcceptance, KindSubgraph:
		return true
	}
	return false
}

func sortedDefinitionNodeIDs(body GraphDefinitionBody) []string {
	ids := make([]string, 0, len(body.Nodes))
	for id := range body.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func nodesThatCanReachAnyEnd(body GraphDefinitionBody) map[string]bool {
	reverse := make(map[string][]string, len(body.Nodes))
	queue := make([]string, 0)
	seen := make(map[string]bool, len(body.Nodes))
	for id, node := range body.Nodes {
		if node.Kind == KindEnd {
			seen[id] = true
			queue = append(queue, id)
		}
		for _, edge := range node.Next {
			reverse[edge.To] = append(reverse[edge.To], id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, source := range reverse[id] {
			if !seen[source] {
				seen[source] = true
				queue = append(queue, source)
			}
		}
	}
	return seen
}

func hasZeroWorkSuccessPath(body GraphDefinitionBody) bool {
	type state struct {
		id     string
		worked bool
	}
	root, ok := body.Nodes[body.Root]
	if !ok {
		return false
	}
	queue := []state{{id: body.Root, worked: isWorkProducingKind(root.Kind)}}
	seen := make(map[state]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		node, exists := body.Nodes[current.id]
		if !exists {
			continue
		}
		if node.Kind == KindEnd && node.EndOutcome == DefinitionEndSuccess && !current.worked {
			return true
		}
		for _, edge := range node.Next {
			target, exists := body.Nodes[edge.To]
			if exists {
				queue = append(queue, state{id: edge.To, worked: current.worked || isWorkProducingKind(target.Kind)})
			}
		}
	}
	return false
}

// validateCyclicSCCExits 使用 Tarjan 找强连通区域；含环区域必须至少有一条边
// 指向区域外。结合 NODE_CANNOT_REACH_END，既拒绝封闭死环，也保留有出口回边。
func validateCyclicSCCExits(body GraphDefinitionBody) []ValidationIssue {
	index := 0
	indices := make(map[string]int)
	low := make(map[string]int)
	onStack := make(map[string]bool)
	var stack []string
	var components [][]string
	var visit func(string)
	visit = func(id string) {
		indices[id], low[id] = index, index
		index++
		stack = append(stack, id)
		onStack[id] = true
		for _, edge := range body.Nodes[id].Next {
			if _, exists := body.Nodes[edge.To]; !exists {
				continue
			}
			if _, seen := indices[edge.To]; !seen {
				visit(edge.To)
				if low[edge.To] < low[id] {
					low[id] = low[edge.To]
				}
			} else if onStack[edge.To] && indices[edge.To] < low[id] {
				low[id] = indices[edge.To]
			}
		}
		if low[id] == indices[id] {
			var component []string
			for {
				last := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[last] = false
				component = append(component, last)
				if last == id {
					break
				}
			}
			components = append(components, component)
		}
	}
	for _, id := range sortedDefinitionNodeIDs(body) {
		if _, seen := indices[id]; !seen {
			visit(id)
		}
	}
	var issues []ValidationIssue
	for _, component := range components {
		members := make(map[string]bool, len(component))
		for _, id := range component {
			members[id] = true
		}
		cyclic := len(component) > 1
		hasExit := false
		for _, id := range component {
			for _, edge := range body.Nodes[id].Next {
				if edge.To == id {
					cyclic = true
				}
				if _, exists := body.Nodes[edge.To]; exists && !members[edge.To] {
					hasExit = true
				}
			}
		}
		if cyclic && !hasExit {
			sort.Strings(component)
			issues = append(issues, validationIssue("SCC_WITHOUT_EXIT", "nodes."+component[0], true, fmt.Sprintf("循环区域 %v 没有结构出口", component)))
		}
	}
	return issues
}
