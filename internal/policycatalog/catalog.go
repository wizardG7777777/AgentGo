package policycatalog

import (
	"fmt"
	"sort"

	"agentgo/internal/contextcontract"
	"agentgo/internal/loopcontract"
)

const (
	OutputReplayMaxEvents         = 4096
	OutputReplayMaxBytes          = 8 << 20
	OutputRecentRecords           = 128
	OutputDefaultSubscriberBuffer = 256
	ContextDefaultCurrent         = "context:default/v12"
	ReplayOpenAICompatibleCurrent = "provider-replay:openai-compatible/v6"

	ProgressCodeChangeV1 = "progress:code-change/v1"
	ProgressCodeChangeV2 = "progress:code-change/v2"
	ProgressCodeChangeV3 = "progress:code-change/v3"
	ProgressCodeChangeV4 = "progress:code-change/v4"
	ProgressCodeChangeV5 = "progress:code-change/v5"
	ProgressCodeChangeV6 = "progress:code-change/v6"
	ProgressCodeChangeV7 = "progress:code-change/v7"
	// ProgressCodeChangeV8 只升级 Observation Control Invocation wire：业务
	// Progress policy 与 v7 相同，历史 v7 仍按 exact/none 恢复。
	ProgressCodeChangeV8 = "progress:code-change/v8"
	// ProgressCodeChangeV9 扩大 Observation Control Invocation 的冻结输出预算，
	// 解决兼容 provider 在 2048 completion tokens 内反复截断的问题。业务
	// Progress policy、Observation schema 与 v8 相同；历史 v8 不静默迁移。
	ProgressCodeChangeV9 = "progress:code-change/v9"
	// ProgressCodeChangeV10 面向 simple-task/v2 的 Explorer handoff：已有上游
	// 调查时缩短首次 decision checkpoint，并在第一次无决策前进后交 v5 repair。
	// Observation wire 与 v9 相同；历史 v9 不迁移。
	ProgressCodeChangeV10 = "progress:code-change/v10"
	// ProgressCodeChangeV11 缩短已有 Explorer handoff 后的首次 decision
	// checkpoint；真实 mutation/check 仍按原强信号继续，历史 v10 不迁移。
	ProgressCodeChangeV11 = "progress:code-change/v11"
	// ProgressCodeChangeV12 对已有 Explorer handoff 只给一个 decision turn；
	// Observation wire 与 v11 相同，历史 v11 不迁移。
	ProgressCodeChangeV12 = "progress:code-change/v12"
	// ProgressCodeChangeCurrent 是所有新 Task/Graph authoring 的唯一选择。
	ProgressCodeChangeCurrent = "progress:code-change/v13"

	ProgressInvestigationV1 = "progress:investigation/v1"
	ProgressInvestigationV2 = "progress:investigation/v2"
	ProgressInvestigationV3 = "progress:investigation/v3"
	// ProgressInvestigationV4 为 simple-task/v4 的首失败与三段 boundary evidence 留出
	// 额外有界读取；v3 六轮与旧 output contract 按历史定义恢复。
	ProgressInvestigationV4 = "progress:investigation/v4"
	// ProgressInvestigationV5 在不增加 Run 总时长的前提下，为 exact handoff
	// 预留 execution 尾窗，并把完整调查 turn 上限收紧到 8。
	ProgressInvestigationV5 = "progress:investigation/v5"
	// ProgressInvestigationV6 把更多 execution window 留给 Worker/Repair；
	// v5 八轮/四分钟语义按历史引用恢复。
	ProgressInvestigationV6 = "progress:investigation/v6"
	// ProgressInvestigationV7 保留 v6 六轮，只把 downstream reserve 提高到
	// 十分钟，为失败 check 后的最小修正留出窗口。
	ProgressInvestigationV7      = "progress:investigation/v7"
	ProgressVerificationV1       = "progress:verification/v1"
	ProgressVerificationV2       = "progress:verification/v2"
	ProgressVerificationV3       = "progress:verification/v3"
	ProgressCoordinationV1       = "progress:coordination/v1"
	ProgressCoordinationV2       = "progress:coordination/v2"
	ProgressFinalReportCurrent   = "progress:final-report/v2"
	ProgressFinalReportV1        = "progress:final-report/v1"
	ProgressInvestigationCurrent = "progress:investigation/v8"
	ProgressVerificationCurrent  = "progress:verification/v4"
	ProgressCoordinationCurrent  = "progress:coordination/v3"
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
	output.Policy.FragmentRules = make(map[contextcontract.FragmentKind]contextcontract.FragmentRuleSpec, len(input.Policy.FragmentRules))
	for kind, rule := range input.Policy.FragmentRules {
		rule.AllowedDispositions = append([]contextcontract.Disposition(nil), rule.AllowedDispositions...)
		output.Policy.FragmentRules[kind] = rule
	}
	output.Policy.AtomicGroupRules = make(map[contextcontract.AtomicGroupKind]contextcontract.AtomicGroupRuleSpec, len(input.Policy.AtomicGroupRules))
	for kind, rule := range input.Policy.AtomicGroupRules {
		rule.TransformIDs = append([]string(nil), rule.TransformIDs...)
		output.Policy.AtomicGroupRules[kind] = rule
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
