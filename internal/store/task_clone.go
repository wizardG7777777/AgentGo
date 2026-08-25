package store

import (
	"agentgo/internal/fulfillment"
	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
)

// cloneTask returns a deep-enough immutable snapshot for all mutable fields on
// model.Task. Store read APIs must never expose their internal Task pointers.
func cloneTask(src *model.Task) *model.Task {
	if src == nil {
		return nil
	}
	dst := *src
	dst.RunContract = cloneRunContract(src.RunContract)
	dst.ProgressContract = cloneProgressContract(src.ProgressContract)
	dst.Dependencies = cloneStrings(src.Dependencies)
	dst.ContextInputs = cloneTaskContextInputs(src.ContextInputs)
	dst.FulfillmentContract = cloneFulfillmentContract(src.FulfillmentContract)
	dst.Agents = cloneStrings(src.Agents)
	dst.RetryReasons = cloneStrings(src.RetryReasons)
	dst.LastHistory = append([]byte(nil), src.LastHistory...)
	dst.Artifacts = cloneStrings(src.Artifacts)
	dst.ExpectedArtifacts = cloneStrings(src.ExpectedArtifacts)
	dst.SchedulerBatch = cloneStrings(src.SchedulerBatch)
	dst.Results = cloneStringMap(src.Results)
	if src.Capability != nil {
		dst.Capability = &model.NodeCapability{
			Tools: cloneStrings(src.Capability.Tools),
			Model: src.Capability.Model,
		}
		// IsolationSpec 仅一个字符串字段，值拷贝即可——克隆丢失隔离声明会让
		// 读路径（ScanAll/GetTask 克隆体）上的节点静默退化为非隔离执行。
		if src.Capability.Isolation != nil {
			iso := *src.Capability.Isolation
			dst.Capability.Isolation = &iso
		}
	}
	if src.ArtifactMeta != nil {
		dst.ArtifactMeta = make(map[string]model.ArtifactMeta, len(src.ArtifactMeta))
		for k, v := range src.ArtifactMeta {
			dst.ArtifactMeta[k] = v
		}
	}
	if src.ReadSet != nil {
		dst.ReadSet = make(map[string]model.ReadInfo, len(src.ReadSet))
		for k, v := range src.ReadSet {
			dst.ReadSet[k] = v
		}
	}
	dst.Lease = cloneLease(src.Lease)
	return &dst
}

func cloneFulfillmentContract(src *fulfillment.Contract) *fulfillment.Contract {
	if src == nil {
		return nil
	}
	copy := *src
	copy.RequiredCheckIDs = cloneStrings(src.RequiredCheckIDs)
	return &copy
}

func cloneTaskContextInputs(src []model.TaskContextInput) []model.TaskContextInput {
	if src == nil {
		return nil
	}
	return append([]model.TaskContextInput(nil), src...)
}

// cloneLease 深拷贝执行租约（V6 §4 H1）。克隆丢失会让读路径上的任务静默
// 退化为「未冻结」，重认领时重复 emit frozen 事件并丢失 Revoked 状态。
func cloneLease(src *model.ExecutionLease) *model.ExecutionLease {
	if src == nil {
		return nil
	}
	dst := *src
	dst.BusinessTools = cloneStrings(src.BusinessTools)
	dst.ControlTools = cloneStrings(src.ControlTools)
	return &dst
}

func cloneStrings(src []string) []string {
	if src == nil {
		return nil
	}
	return append([]string(nil), src...)
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneRunContract(src *runcontract.RunContract) *runcontract.RunContract {
	if src == nil {
		return nil
	}
	dst := *src
	return &dst
}

func cloneProgressContract(src *loopcontract.CompiledProgressContract) *loopcontract.CompiledProgressContract {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Deliverables = append([]loopcontract.DeliverableRule(nil), src.Deliverables...)
	dst.VerificationTargets = append([]loopcontract.VerificationRule(nil), src.VerificationTargets...)
	dst.AcceptedSignals = append([]loopcontract.ProgressSignalRule(nil), src.AcceptedSignals...)
	return &dst
}

func cloneToolArgs(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = cloneToolArgValue(value)
	}
	return dst
}

func cloneToolArgValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneToolArgs(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneToolArgValue(item)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case map[string]string:
		return cloneStringMap(typed)
	case []map[string]any:
		out := make([]map[string]any, len(typed))
		for i, item := range typed {
			out[i] = cloneToolArgs(item)
		}
		return out
	default:
		// Tool arguments originate from JSON. Scalar JSON values are immutable;
		// the explicit aggregate cases above cover the mutable forms.
		return value
	}
}

func cloneIntPointer(src *int) *int {
	if src == nil {
		return nil
	}
	value := *src
	return &value
}
