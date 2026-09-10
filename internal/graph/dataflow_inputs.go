package graph

import "fmt"

type DataflowInputValue struct {
	Ref          string `json:"ref"`
	Value        any    `json:"value"`
	CandidateRef string `json:"candidate_ref,omitempty"`
}

type FrozenDataflowInputs struct {
	Values                map[string]DataflowInputValue `json:"values"`
	WorkspaceCandidateRef string                        `json:"workspace_candidate_ref,omitempty"`
}

// ResolveDataflowInputs 只解析数据，不执行模型/工具，也不修改结果来源。
// 缺输入返回明确 waiting reason；坏引用/跨图结果返回错误。
func ResolveDataflowInputs(def DataflowDefinition, nodeID string, results map[string]AgentTaskResult, external map[string]map[int64]DataflowInputValue, executions map[string]AgentTaskExecution) (FrozenDataflowInputs, string, error) {
	var node *AgentTaskNode
	for i := range def.Nodes {
		if def.Nodes[i].NodeID == nodeID {
			node = &def.Nodes[i]
			break
		}
	}
	out := FrozenDataflowInputs{Values: map[string]DataflowInputValue{}}
	if node == nil {
		return out, "", fmt.Errorf("节点 %s 不存在", nodeID)
	}
	for _, slot := range sortedDataflowKeys(node.Inputs) {
		source := node.Inputs[slot]
		var input DataflowInputValue
		switch source.Kind {
		case "node_result":
			result, ok := results[source.NodeID]
			if !ok {
				return out, "waiting_inputs:" + slot, nil
			}
			if result.Schema != AgentTaskResultSchema || result.GraphID != def.GraphID || result.RunID != def.RunID || result.NodeID != source.NodeID || result.Ref == "" {
				return out, "", fmt.Errorf("输入 %s 的结果身份或版本不匹配", slot)
			}
			input = DataflowInputValue{Ref: result.Ref, Value: result.Value, CandidateRef: result.CandidateRef}
		case "node_outcome":
			fact, ok := executions[source.NodeID]
			if !ok || fact.OutcomeRef == "" {
				return out, "waiting_outcome:" + slot, nil
			}
			if fact.NodeID != source.NodeID || !dataflowTaskTerminal(fact.Status) {
				return out, "", fmt.Errorf("输入终态身份不一致")
			}
			input = DataflowInputValue{Ref: fact.OutcomeRef, Value: map[string]any{"status": fact.Status, "error": fact.Error, "task_id": fact.TaskID, "node_id": fact.NodeID, "attempt_id": fact.AttemptID}}
		case "graph_input":
			var ok bool
			input, ok = external[source.Port][source.Version]
			if !ok {
				return out, "waiting_inputs:" + slot, nil
			}
			if input.Ref == "" {
				return out, "", fmt.Errorf("图输入 %s 缺少持久化引用", slot)
			}
			if err := validateDataflowValue(def.Inputs[source.Port].Schema, input.Value); err != nil {
				return out, "", fmt.Errorf("图输入 %s: %w", slot, err)
			}
		default:
			return out, "", fmt.Errorf("输入 %s 来源类型无效", slot)
		}
		value, err := selectDataflowValue(input.Value, source.Pointer)
		if err != nil {
			return out, "", fmt.Errorf("输入 %s: %w", slot, err)
		}
		input.Value = value
		out.Values[slot] = input
	}
	candidates := map[string]bool{}
	for _, input := range out.Values {
		if input.CandidateRef != "" {
			candidates[input.CandidateRef] = true
		}
	}
	if node.WorkspaceInput != "" {
		input := out.Values[node.WorkspaceInput]
		if input.CandidateRef == "" {
			return out, "", fmt.Errorf("workspace_input=%s 未提供候选版本", node.WorkspaceInput)
		}
		out.WorkspaceCandidateRef = input.CandidateRef
	} else if len(candidates) > 1 {
		return out, "", fmt.Errorf("多个候选版本必须显式声明 workspace_input")
	} else {
		for ref := range candidates {
			out.WorkspaceCandidateRef = ref
		}
	}
	copy, err := cloneDataflow(out)
	return copy, "", err
}
