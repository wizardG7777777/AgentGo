package policycatalog

import (
	"fmt"
	"sort"

	"agentgo/internal/contextcontract"
	"agentgo/internal/loopcontract"
)

const (
	ContextDefaultV1 = "context:default/v1"
	ContextDefaultV2 = "context:default/v2"
	ContextDefaultV3 = "context:default/v3"
	ContextDefaultV4 = "context:default/v4"
	ContextDefaultV5 = "context:default/v5"
	ContextDefaultV6 = "context:default/v6"
	ContextDefaultV7 = "context:default/v7"
	ContextDefaultV8 = "context:default/v8"
	// ContextDefaultCurrent 是所有新 Run/Invocation 的 framework 默认引用。
	// 历史任务必须继续使用其已冻结的具体版本，禁止在恢复时把 v1 偷换为该别名。
	ContextDefaultCurrent    = ContextDefaultV8
	ReplayOpenAICompatibleV1 = "provider-replay:openai-compatible/v1"
	ReplayOpenAICompatibleV2 = "provider-replay:openai-compatible/v2"
	ReplayOpenAICompatibleV3 = "provider-replay:openai-compatible/v3"

	ProgressCodeChangeV1    = "progress:code-change/v1"
	ProgressInvestigationV1 = "progress:investigation/v1"
	ProgressVerificationV1  = "progress:verification/v1"
	ProgressCoordinationV1  = "progress:coordination/v1"
)

// ContextProfile 把 L2 budget policy 与默认 provider replay policy 引用绑定。
type ContextProfile struct {
	Ref             string
	Digest          string
	Policy          contextcontract.ContextBudgetPolicy
	ReplayPolicyRef string
}

// ReplayProfile 是 provider replay policy 及其稳定 digest。
type ReplayProfile struct {
	Ref    string
	Digest string
	Policy contextcontract.ProviderReplayPolicy
}

// ProgressProfile 是 L4 CompiledProgressContract 模板及其稳定 digest。
type ProgressProfile struct {
	Ref      string
	Digest   string
	Contract loopcontract.CompiledProgressContract
}

// Catalog 是构造后只读的内存 catalog。所有 Lookup 返回深拷贝。
type Catalog struct {
	contexts map[string]ContextProfile
	replays  map[string]ReplayProfile
	progress map[string]ProgressProfile
}

// NewDefault 构造并完整校验内置 v1 catalog。任何默认 policy 自相矛盾都在
// 装配前返回错误，不能降级成缺失 authority。
func NewDefault() (*Catalog, error) {
	replayProfiles, err := defaultReplayProfiles()
	if err != nil {
		return nil, err
	}
	contextProfiles, err := defaultContextProfiles()
	if err != nil {
		return nil, err
	}
	progressProfiles, err := defaultProgressProfiles()
	if err != nil {
		return nil, err
	}
	catalog := &Catalog{
		contexts: make(map[string]ContextProfile, len(contextProfiles)),
		replays:  make(map[string]ReplayProfile, len(replayProfiles)),
		progress: make(map[string]ProgressProfile, len(progressProfiles)),
	}
	for _, profile := range replayProfiles {
		if _, duplicate := catalog.replays[profile.Ref]; duplicate {
			return nil, fmt.Errorf("policy catalog: 重复 replay ref=%s", profile.Ref)
		}
		catalog.replays[profile.Ref] = profile
	}
	for _, profile := range contextProfiles {
		if _, duplicate := catalog.contexts[profile.Ref]; duplicate {
			return nil, fmt.Errorf("policy catalog: 重复 context ref=%s", profile.Ref)
		}
		catalog.contexts[profile.Ref] = profile
	}
	for _, profile := range progressProfiles {
		if _, duplicate := catalog.progress[profile.Ref]; duplicate {
			return nil, fmt.Errorf("policy catalog: 重复 progress ref=%s", profile.Ref)
		}
		catalog.progress[profile.Ref] = profile
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return catalog, nil
}

// Validate 对 catalog 的引用闭合和稳定 digest 做全量校验。
func (c *Catalog) Validate() error {
	if c == nil {
		return fmt.Errorf("policy catalog 为空")
	}
	if len(c.contexts) == 0 || len(c.replays) == 0 || len(c.progress) == 0 {
		return fmt.Errorf("policy catalog 缺少 context/replay/progress authority")
	}
	for _, ref := range sortedKeys(c.contexts) {
		profile := c.contexts[ref]
		if profile.Ref != ref || profile.Policy.PolicyID != ref {
			return fmt.Errorf("context profile %s ref/policy_id 不一致", ref)
		}
		if err := profile.Policy.Validate(); err != nil {
			return fmt.Errorf("context profile %s 无效: %w", ref, err)
		}
		digest, err := profile.Policy.ComputeDigest()
		if err != nil || digest != profile.Digest {
			return fmt.Errorf("context profile %s digest 不一致", ref)
		}
		if _, ok := c.replays[profile.ReplayPolicyRef]; !ok {
			return fmt.Errorf("context profile %s 引用未知 replay policy=%s", ref, profile.ReplayPolicyRef)
		}
	}
	for _, ref := range sortedKeys(c.replays) {
		profile := c.replays[ref]
		if profile.Ref != ref || profile.Policy.PolicyID != ref {
			return fmt.Errorf("replay profile %s ref/policy_id 不一致", ref)
		}
		if err := profile.Policy.Validate(); err != nil {
			return fmt.Errorf("replay profile %s 无效: %w", ref, err)
		}
		digest, err := profile.Policy.ComputeDigest()
		if err != nil || digest != profile.Digest {
			return fmt.Errorf("replay profile %s digest 不一致", ref)
		}
	}
	for _, ref := range sortedKeys(c.progress) {
		profile := c.progress[ref]
		if profile.Ref != ref || profile.Contract.Ref.ContractID != ref {
			return fmt.Errorf("progress profile %s ref/contract_id 不一致", ref)
		}
		if err := profile.Contract.Validate(); err != nil {
			return fmt.Errorf("progress profile %s 无效: %w", ref, err)
		}
		digest, err := ProgressContractDigest(profile.Contract)
		if err != nil || digest != profile.Digest || profile.Contract.Ref.ContractDigest != "sha256:"+digest {
			return fmt.Errorf("progress profile %s digest 不一致", ref)
		}
	}
	return nil
}

// HasProgressContract 实现 graph.DefinitionPolicyResolver。
func (c *Catalog) HasProgressContract(ref string) bool {
	if c == nil {
		return false
	}
	_, ok := c.progress[ref]
	return ok
}

// HasContextPolicy 实现 graph.DefinitionPolicyResolver。
func (c *Catalog) HasContextPolicy(ref string) bool {
	if c == nil {
		return false
	}
	_, ok := c.contexts[ref]
	return ok
}

func (c *Catalog) ContextPolicy(ref string) (ContextProfile, bool) {
	if c == nil {
		return ContextProfile{}, false
	}
	profile, ok := c.contexts[ref]
	if !ok {
		return ContextProfile{}, false
	}
	return cloneContextProfile(profile), true
}

func (c *Catalog) ProviderReplayPolicy(ref string) (ReplayProfile, bool) {
	if c == nil {
		return ReplayProfile{}, false
	}
	profile, ok := c.replays[ref]
	if !ok {
		return ReplayProfile{}, false
	}
	return cloneReplayProfile(profile), true
}

func (c *Catalog) ProgressContract(ref string) (ProgressProfile, bool) {
	if c == nil {
		return ProgressProfile{}, false
	}
	profile, ok := c.progress[ref]
	if !ok {
		return ProgressProfile{}, false
	}
	return cloneProgressProfile(profile), true
}

func (c *Catalog) ContextRefs() []string {
	if c == nil {
		return nil
	}
	return sortedKeys(c.contexts)
}

func (c *Catalog) ReplayRefs() []string {
	if c == nil {
		return nil
	}
	return sortedKeys(c.replays)
}

func (c *Catalog) ProgressRefs() []string {
	if c == nil {
		return nil
	}
	return sortedKeys(c.progress)
}

func sortedKeys[T any](items map[string]T) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneContextProfile(input ContextProfile) ContextProfile {
	output := input
	if input.Policy.ModelContextWindow != nil {
		value := *input.Policy.ModelContextWindow
		output.Policy.ModelContextWindow = &value
	}
	if input.Policy.ProtocolOverheadReserve != nil {
		value := *input.Policy.ProtocolOverheadReserve
		output.Policy.ProtocolOverheadReserve = &value
	}
	output.Policy.FragmentRules = make(map[contextcontract.FragmentKind]contextcontract.FragmentBudgetRule, len(input.Policy.FragmentRules))
	for kind, rule := range input.Policy.FragmentRules {
		rule.AllowedDispositions = append([]contextcontract.Disposition(nil), rule.AllowedDispositions...)
		output.Policy.FragmentRules[kind] = rule
	}
	output.Policy.AtomicGroupRules = make(map[contextcontract.AtomicGroupKind]contextcontract.AtomicGroupBudgetRule, len(input.Policy.AtomicGroupRules))
	for kind, rule := range input.Policy.AtomicGroupRules {
		rule.TransformIDs = append([]string(nil), rule.TransformIDs...)
		output.Policy.AtomicGroupRules[kind] = rule
	}
	output.Policy.SectionBudgets = make(map[contextcontract.ContextSection]contextcontract.Budget, len(input.Policy.SectionBudgets))
	for section, budget := range input.Policy.SectionBudgets {
		output.Policy.SectionBudgets[section] = budget
	}
	return output
}

func cloneReplayProfile(input ReplayProfile) ReplayProfile {
	output := input
	output.Policy.Fields = make(map[string]contextcontract.ReplayRequirement, len(input.Policy.Fields))
	for field, requirement := range input.Policy.Fields {
		output.Policy.Fields[field] = requirement
	}
	output.Policy.GroupTransforms = append([]contextcontract.ReplayTransform(nil), input.Policy.GroupTransforms...)
	return output
}

func cloneProgressProfile(input ProgressProfile) ProgressProfile {
	output := input
	output.Contract.Deliverables = append([]loopcontract.DeliverableRule(nil), input.Contract.Deliverables...)
	output.Contract.VerificationTargets = append([]loopcontract.VerificationRule(nil), input.Contract.VerificationTargets...)
	output.Contract.AcceptedSignals = append([]loopcontract.ProgressSignalRule(nil), input.Contract.AcceptedSignals...)
	return output
}
