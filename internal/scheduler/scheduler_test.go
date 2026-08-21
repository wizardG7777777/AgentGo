package scheduler

import (
	"strings"
	"testing"
)

// TestSchedulerSystemPrompt_AgentCapabilitiesFieldDescription verifies that the
// "你能看见什么" section describes the resources.agent_capabilities field structure.
// Validates: Requirements 9.1
func TestSchedulerSystemPrompt_AgentCapabilitiesFieldDescription(t *testing.T) {
	prompt := schedulerSystemPrompt

	requiredPhrases := []string{
		"agent_capabilities",
		"agent_type",
		"capabilities",
		"description",
	}
	for _, phrase := range requiredPhrases {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("schedulerSystemPrompt should contain %q in the agent_capabilities field description", phrase)
		}
	}

	// Verify it appears in the "你能看见什么" context section
	sectionStart := strings.Index(prompt, "你能看见什么")
	if sectionStart == -1 {
		t.Fatal("schedulerSystemPrompt should contain '你能看见什么' section")
	}
	sectionText := prompt[sectionStart:]
	if !strings.Contains(sectionText, "agent_capabilities") {
		t.Error("agent_capabilities should be described in the '你能看见什么' section")
	}
}

func TestSchedulerSystemPromptUsesRegisteredLocalReadToolNames(t *testing.T) {
	if strings.Contains(schedulerSystemPrompt, "list_files") {
		t.Fatal("schedulerSystemPrompt references unregistered list_files instead of list_dir")
	}
	if !strings.Contains(schedulerSystemPrompt, "list_dir") {
		t.Fatal("schedulerSystemPrompt must expose the registered list_dir tool")
	}
}

func TestSchedulerSystemPrompt_ModeContractUsesOnlyCurrentAxes(t *testing.T) {
	for _, phrase := range []string{
		`exec_mode`,
		`topo_mode`,
		`两轴正交`,
		`Graph 中显式使用 approval 节点`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少当前模式契约 %q", phrase)
		}
	}
	for _, stale := range []string{
		`- mode：`,
		`mode / exec_mode / topo_mode`,
		`三轴`,
		`规划门控轴`,
		`Immediate 模式`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍包含过期模式契约 %q", stale)
		}
	}
}

func TestSchedulerSystemPrompt_ReadGraphBeforePatch(t *testing.T) {
	for _, phrase := range []string{
		`read_graph：按 graph_id 读取权威 GraphDocument 与当前 revision`,
		`patch_graph：以 base_revision CAS`,
		`修改前必须 read_graph 获取权威 revision`,
		`冲突时再次 read_graph`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 read_graph CAS 指引 %q", phrase)
		}
	}
}

// TestSchedulerSystemPrompt_GraphFirstForDurableWork locks the unified-Graph
// lifecycle (v7): every new user request must form a Graph — there is no
// parallel direct-answer path; topology choices are dependency-first.
func TestSchedulerSystemPrompt_GraphFirstForDurableWork(t *testing.T) {
	for _, phrase := range []string{
		`每个新用户请求都必须形成 Graph`,
		`没有与 Graph 并列的"直接回答"路径`,
		`不得在建图前完成主体工作`,
		`依赖优先于并行`,
		`最小 Graph（一个工作节点 + end）`,
		`legacy/恢复兼容`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少统一 Graph 生命周期指引 %q", phrase)
		}
	}

	for _, stale := range []string{
		`默认假设：能自己干就自己干`,
		`只有 C 类才委派`,
		`单个任务直发不值得建图`,
		`直接回答路径（不建图）`,
		`一次原子只读查询`,
		`工具调用上限是一次`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍含已删除的 D 路径旧指引 %q", stale)
		}
	}
}

func TestSchedulerSystemPrompt_StaticRoutingTemplatesShelved(t *testing.T) {
	for _, phrase := range []string{
		`静态路由纪律`,
		`resources.specialized_agents / agent_capabilities 中实际列出的静态 route`,
		`controller→__scheduler__、agent→""、acceptance→acceptance.verify`,
		`metadata.route 显式覆盖优先`,
		`route 在 graph:<id> owner scope 下 ready 且 capability 覆盖节点 tools`,
		`不得提交一张注定无人认领的图`,
		`不主动使用 subgraph`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少静态路由纪律 %q", phrase)
		}
	}

	// agent_templates 动态组队机制 2026-08-20 起默认搁置（v7.6）：provision
	// 教程必须从正文绝迹，否则 scheduler 会尝试调用未注册的工具。
	for _, stale := range []string{
		`provision_agent_team`,
		`list_agent_templates`,
		`AgentTemplate 动态组队纪律`,
		`先检查 agent_templates 并按需 provision`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍含已搁置的模板组队指引 %q", stale)
		}
	}
}

func TestSchedulerSystemPrompt_GraphFanInAndConditionsAreExplicit(t *testing.T) {
	for _, phrase := range []string{
		`无 flow generation/correlation token 的单赋值安全基线`,
		`所有非 barrier 节点最多一条静态入边`,
		`条件分支必须各自保留后续与 end`,
		`禁止共享下游普通节点形成 OR mux`,
		`when 缺省即无条件`,
		`blocked/failed 等终态到达时照样选中`,
		`join 是 Runtime 内建 barrier`,
		`不会把上游 event 提升`,
		`成功汇合时自身终态事件固定回落为 completed`,
		`join → summarize`,
		`不得写 ready/pass`,
		`禁止让错误终态通过无条件边误入成功分支`,
		`task.required_inputs 列出必须齐备的端口名`,
		`每个 target_input 只能有一条生产边`,
		`并行 AND 使用不同端口`,
		`互斥候选不得共享端口`,
		`target_input 只允许指向 join / acceptance`,
		`root 的首次 activation 由 Runtime 隐式创建`,
		`activation="new" 回边`,
		`"next":[{"to":"done","when":{"event":"completed"}}]`,
		`上游失败应在上游节点自己的 next 直接以 event=failed/blocked 绕过 router 到 repair`,
		`router 激活后以自身 completed 终态求值 next`,
		`$.status eq "failed"`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 Graph fan-in/条件边界 %q", phrase)
		}
	}
	for _, stale := range []string{
		`普通 fan-in 是 first-arrival/OR`,
		`互斥条件分支写入同一个端口`,
		`互斥候选边共享端口`,
		`"route":{"kind":"router","task":{"title":"按调查结论分流"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"gap_fix","when":{"path":"$.coverage","operator":"eq","value":"gap"}},{"to":"done","when":{"path":"$.coverage","operator":"eq","value":"ok"}},{"to":"repair","when":{"event":"failed"}}]}`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍包含不安全的 OR mux 指引 %q", stale)
		}
	}
}

func TestSchedulerSystemPrompt_AcceptanceUsesVerdictOnly(t *testing.T) {
	for _, phrase := range []string{
		`task.title 必须非空`,
		`task.description 必须非空并逐项写明验收标准`,
		`verdict 只允许 pass / fixable / failed`,
		`completed 业务结论只读取 $.verdict`,
		`completed 结果必须省略 event`,
		`acceptance 出边禁止无条件、always、completed、pass/fixable 事件条件`,
		`Runtime failed/blocked 兜底事件`,
		`status=blocked 与 blocked_reason`,
		`disputed 是 Runtime 核验状态，不是 verifier 可提交的 verdict`,
		`"path":"$.verdict","operator":"eq","value":"pass"`,
		`"path":"$.verdict","operator":"eq","value":"fixable"`,
		`"path":"$.verdict","operator":"eq","value":"failed"`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 acceptance verdict-only 契约 %q", phrase)
		}
	}
	for _, stale := range []string{
		`验收 agent 以 submit_task_result 的 verdict/event 回报`,
		`verdict（及 event）提交结论`,
		`"to":"done","when":{"event":"pass"}`,
		`"to":"impl","when":{"event":"fixable"}`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍包含 acceptance 事件 verdict 契约 %q", stale)
		}
	}
}

func TestSchedulerPromptVersionTerminalContractV2(t *testing.T) {
	const want = "embedded:v8.2-upstream-intervention"
	if schedulerPromptVersion != want {
		t.Fatalf("schedulerPromptVersion = %q，期望 %q", schedulerPromptVersion, want)
	}
}

// v8.2（2026-08-21）上游零产出介入裁决节：触发通道、升序档位与反机械重开
// 纪律必须出现在正文中。
func TestSchedulerSystemPrompt_V82UpstreamIntervention(t *testing.T) {
	for _, phrase := range []string{
		"节点介入裁决",
		"上游零产出介入",
		"上游返工（首选）",
		"修正后返工",
		"controller 亲自补位",
		"降级 / 诚实失败",
		"不得机械重开同一节点而不补充新信息",
		"超时告警介入",
		"[watchdog-alert: <taskID>]",
		"send_message steer 收敛",
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 v8.2 介入裁决短语 %q", phrase)
		}
	}
}

// v8.1（2026-08-21）第四轮 SWE 复测后的 doctrine 补强：失败路径默认带返工、
// 建图前禁止亲自多轮调查、大图骨架先行。三条新纪律必须出现在正文中。
func TestSchedulerSystemPrompt_V81Doctrine(t *testing.T) {
	for _, phrase := range []string{
		"失败路径也是拓扑",
		"activation=\"new\" 回边",
		"failed → end 意味着放弃",
		"不包括亲自调查仓库",
		"调查节点开头的最小图",
		"骨架先行",
		"patch_graph 逐次扩展",
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 v8.1 doctrine 短语 %q", phrase)
		}
	}
}

// Graph 终态契约 v2（2026-08-20，docs/design/graph-terminal-contract-v2.md）：
// agent/controller 禁止 event、出边仅系统事件、业务分支走 path 条件 + 输出契约。
func TestSchedulerSystemPrompt_TerminalContractV2(t *testing.T) {
	for _, phrase := range []string{
		`"schema":"agentgo.graph/v2"`,
		`禁止提交 event 参数`,
		`仅允许系统事件 completed/failed/blocked/always`,
		`业务分支的唯一合法通道`,
		`输出契约`,
		`<output-contract>`,
		`无匹配出路会被拒绝并要求执行者重交`,
		`[graph-change-request: .../no-outlet]`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少终态契约 v2 指引 %q", phrase)
		}
	}
	for _, stale := range []string{
		`完成时报 event=ready`,
		`写入专用 Results["event"]`,
		`事件名仅允许 ready/completed/fixable/failed/blocked/pass/approved/rejected/timeout/always`,
		`"schema":"agentgo.graph/v1"`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍含 v1 自由事件指引 %q", stale)
		}
	}
}

func TestSchedulerSystemPrompt_AcceptanceUsesClosedToolsAndGraphCausality(t *testing.T) {
	for _, phrase := range []string{
		`只读闭集`,
		`无消息/发任务/用户交互/request_replan`,
		`Evidence 只证明实现者或 checker 的工具调用事实`,
		`不证明节点内“最后一次写入之后”的先后关系`,
		`implement → checker → acceptance`,
		`Graph 因果边而非证据时间戳负责证明先后`,
		`当前工具名闭集不包含 MCP`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 verifier 闭集/因果新鲜度契约 %q", phrase)
		}
	}
	if strings.Contains(schedulerSystemPrompt, `实现者在**最后一次写入之后**执行判据要求的命令`) {
		t.Error("schedulerSystemPrompt 仍宣称 Evidence 能证明实现节点内的最后写入顺序")
	}
}

func TestSchedulerSystemPrompt_StructuredRoutesAndMechanicalFuse(t *testing.T) {
	for _, phrase := range []string{
		`业务路由字段必须放进 result object`,
		`result={"coverage":"gap"}`,
		`不能把 coverage 只写在 summary 中`,
		`无任何匹配出路则整张图 failed`,
		`EvidenceRef 是系统按调用或内容身份签发的不透明稳定引用`,
		`合法的跨任务回边和长目标不设 activation 次数上限`,
		`同步机械级联`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少结构化路由/机械保险丝指引 %q", phrase)
		}
	}
	for _, stale := range []string{
		`activation 次数达到 32`,
		`emergency fuse（32 次）`,
		`ev:<taskID>:<seq>`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍含过期数据流契约 %q", stale)
		}
	}
}

func TestSchedulerSystemPrompt_GraphControllerScopeBoundaries(t *testing.T) {
	for _, phrase := range []string{
		`Graph controller 的控制面被硬绑定到当前 Graph`,
		`禁止 submit_graph 新图`,
		`publish_task 脱图任务`,
		`report_done 提前收尾`,
		`cancel_task 也只能取消 exact same GraphID`,
		`最终用 submit_task_result 结算当前 controller 节点`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 Graph controller 作用域边界 %q", phrase)
		}
	}
}

// TestSchedulerSystemPrompt_NoDirectAnswerPath v7 起 D（直接回答）路径删除：
// 闲聊、状态回答与简单查询同样走最小 Graph，直接路径不得以例外形式复活。
func TestSchedulerSystemPrompt_NoDirectAnswerPath(t *testing.T) {
	for _, phrase := range []string{
		`没有与 Graph 并列的"直接回答"路径`,
		`闲聊、状态回答和简单查询也使用最小 Graph`,
		`能力不可用 + 替代方案`,
		`新用户请求中 **pending_downstream_tasks** 不存在或为空，绝不构成绕过 Graph 的许可`,
		`只有 trigger 明确是 graph_ended`,
		`Graph 的最终自然语言汇报只发生在 graph_ended 终态唤醒`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少统一 Graph 路径声明 %q", phrase)
		}
	}
	for _, stale := range []string{
		`直接回答路径（不建图）`,
		`一次原子只读查询`,
		`工具调用上限是一次`,
		`立即停止扩展直接路径`,
		`用户请求依赖不可用工具时，直接以自然语言说明情况`,
		`tasks 中没有 failed → 直接答"正常"`,
		`如果 **pending_downstream_tasks** 不存在或为空，且没有其它在途工作，用自然语言汇报最终结果收尾`,
	} {
		if strings.Contains(schedulerSystemPrompt, stale) {
			t.Errorf("schedulerSystemPrompt 仍含已删除的直接路径指引 %q", stale)
		}
	}
}

func TestSchedulerSystemPrompt_SoloUsesControllerGraph(t *testing.T) {
	for _, phrase := range []string{
		`仍调用 submit_graph`,
		`controller 节点（缺省路由 __scheduler__）`,
		`不得使用无人认领的 agent/acceptance 节点`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 solo Graph 指引 %q", phrase)
		}
	}
}

func TestSchedulerSystemPrompt_ExecModesConstrainGraphPlanning(t *testing.T) {
	for _, phrase := range []string{
		`readonly：write_file / edit_file / run_shell 会被 Gate 硬拒绝`,
		`不得提交依赖这三个工具的节点`,
		`/mode exec normal`,
		`strict：写文件与 run_shell 会进入精确绑定的授权 Interaction`,
		`不得用 Graph approval 节点伪造或绕过`,
		`yolo：只改变灰名单 Shell 的自动放行策略`,
	} {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少 exec mode 的 Graph 规划边界 %q", phrase)
		}
	}
}

func TestSchedulerSystemPrompt_TaskResultRefsAreReadOnDemand(t *testing.T) {
	required := []string{
		"result_refs",
		"original_bytes/original_runes",
		"sha256",
		"progress.retained_tail",
		"get_task_result",
		"excerpt 足以支持当前决策",
		"不得机械地遍历或读完所有 result_refs",
	}
	for _, phrase := range required {
		if !strings.Contains(schedulerSystemPrompt, phrase) {
			t.Errorf("schedulerSystemPrompt should contain task-result guidance %q", phrase)
		}
	}
}

// TestSchedulerSystemPrompt_CapabilitiesRoutingGuidance verifies that the prompt
// contains capabilities-based routing guidance in the "路由指引" section.
// Validates: Requirements 9.2, 9.3
func TestSchedulerSystemPrompt_CapabilitiesRoutingGuidance(t *testing.T) {
	prompt := schedulerSystemPrompt

	// Check for capabilities-based routing section
	if !strings.Contains(prompt, "基于 capabilities 的路由决策") {
		t.Error("schedulerSystemPrompt should contain capabilities-based routing guidance section")
	}

	// R9.2: guidance to prefer agents whose capabilities match the task
	if !strings.Contains(prompt, "优先匹配能力") {
		t.Error("schedulerSystemPrompt should contain guidance to prefer capability-matching agents")
	}
	if !strings.Contains(prompt, "capabilities 当前列出真实工具名") || !strings.Contains(prompt, "run_shell") {
		t.Error("schedulerSystemPrompt should route against actual registered tool names")
	}
	if !strings.Contains(prompt, "同时包含 submit_task_result") {
		t.Error("schedulerSystemPrompt should preserve the acceptance-node verdict submission capability boundary")
	}

	// R9.3: guidance to avoid routing to agents lacking required capabilities
	if !strings.Contains(prompt, "避免能力不足的路由") {
		t.Error("schedulerSystemPrompt should contain guidance to avoid routing to capability-lacking agents")
	}
	if !strings.Contains(prompt, "避免将任务路由到该代理类型") {
		t.Error("schedulerSystemPrompt should instruct to avoid routing to agents without required capabilities")
	}
}

// TestSchedulerSystemPrompt_OnlyRouteToExistingAgentTypes verifies that the prompt
// contains constraints about only routing to existing agent types.
// Validates: Requirements 10.1, 10.2, 10.3, 10.4
func TestSchedulerSystemPrompt_OnlyRouteToExistingAgentTypes(t *testing.T) {
	prompt := schedulerSystemPrompt

	// R10.1: instruct to only choose from agent_capabilities and specialized_agents
	if !strings.Contains(prompt, "仅路由到已存在的代理类型") {
		t.Error("schedulerSystemPrompt should contain existing-agent-type-only constraint section")
	}
	if !strings.Contains(prompt, "仅从已知代理类型中选择") {
		t.Error("schedulerSystemPrompt should instruct to only select from known agent types")
	}

	// R10.2: instruct to check event_type before publishing
	if !strings.Contains(prompt, "发布前检查") {
		t.Error("schedulerSystemPrompt should instruct to check event_type before publishing")
	}

	// R10.3: explain the missing capability directly instead of publishing an
	// unclaimable task.
	if !strings.Contains(prompt, "自然语言向用户说明无法完成的原因") {
		t.Error("schedulerSystemPrompt should explain missing capabilities directly")
	}
	if !strings.Contains(prompt, "无匹配时不发布") {
		t.Error("schedulerSystemPrompt should instruct not to publish when no matching agent type exists")
	}

	// R10.4: include example about non-existent event_type
	if !strings.Contains(prompt, "示例") {
		t.Error("schedulerSystemPrompt should include an example about non-existent event_type routing")
	}
	// The example should mention that a non-existent event_type should not be used
	if !strings.Contains(prompt, "specialized_agents") {
		t.Error("schedulerSystemPrompt should reference specialized_agents in the routing constraint")
	}
}

// TestSchedulerSystemPrompt_UnavailableToolsGuidance verifies that the
// schedulerSystemPrompt contains "unavailable_tools" guidance in the
// "你能看见什么" section, instructing the Scheduler to avoid assigning tasks
// that depend on unavailable tools and to suggest alternatives directly.
// Validates: Requirements 4.4
func TestSchedulerSystemPrompt_UnavailableToolsGuidance(t *testing.T) {
	prompt := schedulerSystemPrompt

	// The prompt must mention unavailable_tools
	if !strings.Contains(prompt, "unavailable_tools") {
		t.Fatal("schedulerSystemPrompt should contain 'unavailable_tools'")
	}

	// Verify it appears in the "你能看见什么" context section
	sectionStart := strings.Index(prompt, "你能看见什么")
	if sectionStart == -1 {
		t.Fatal("schedulerSystemPrompt should contain '你能看见什么' section")
	}
	sectionText := prompt[sectionStart:]
	if !strings.Contains(sectionText, "unavailable_tools") {
		t.Error("unavailable_tools should be described in the '你能看见什么' section")
	}

	// Verify guidance to avoid assigning tasks depending on unavailable tools
	if !strings.Contains(sectionText, "web_search") {
		t.Error("unavailable_tools guidance should mention web_search as an example")
	}
	if !strings.Contains(sectionText, "web_fetch") {
		t.Error("unavailable_tools guidance should mention web_fetch as an example")
	}

	// 能力不足也必须进入统一 Graph，由最小 controller 形成说明结果。
	if !strings.Contains(sectionText, "仍提交最小 controller → end") || !strings.Contains(sectionText, "替代方案") {
		t.Error("unavailable_tools guidance should keep capability explanation inside the unified Graph path")
	}
}

// ================================================================
// ⚠️  2026-04-20 回归锁（预期红态 —— 请勿删除断言！）
// ================================================================
//
// 本节下列测试中的 DoesNotClaimSingleTaskPerLoop 当前**故意失败**，用于锁定
// P0-1 "Scheduler publish_task 完全串行发布" 缺陷：在修复完成前它应保持红灯。
// 如果 CI 报此测试失败，**不是回归**，这是提醒 bug 还没修。修复路径：
// 改写 scheduler.go 第 243 行附近的"publish_task 是单次调用工具，一次只能发布
// 一个任务"陈述，明确说"每次调用创建一个任务；同一 reactLoop 内可按顺序多次
// 调用，独立 Task 随后并行执行"，再补一个**纯独立无依赖**任务批量登记的示例（与现有"3 探索 + 1 汇总"
// 的依赖聚合示例形成对照）。
//
// ContainsParallelIndependentExecutionGuidance 当前是绿的（现有 prompt 已覆盖），
// 作为回归锁防止修改时误删并行执行指引。
//
// ❌ 错误处理：删除断言 / 改 Skip / 弱化误导句子列表 —— 这样会掩盖 bug 信号
// ✅ 正确处理：修 scheduler.go 中的 schedulerSystemPrompt，此处自动变绿
//
// 背景（bug 现象）：2026-04-20 并发测试中 scheduler 把 3 个完全独立的子任务按 loop
// 0/1/2 串行发布（每 loop 只 publish 一个并等完），wall-clock 从预期 ~30s 拖到
// 14.5 min，所有并发场景事实上无法被测试触发。根因是 prompt 中"一次只能发布
// 一个任务"这句权威陈述误导了 LLM。工具调用本身按模型顺序登记，但同轮创建的
// 独立 Task 会由多个 Runner 并行执行。
//
// 该问题已修复；历史记录见 docs/archived/。
// ================================================================

// TestSchedulerSystemPrompt_DoesNotClaimSingleTaskPerLoop 断言 prompt 不再声称
// publish_task 每次只能发一个任务。该陈述与基础设施能力矛盾，会诱导 LLM 把
// 独立任务串行化发布。
func TestSchedulerSystemPrompt_DoesNotClaimSingleTaskPerLoop(t *testing.T) {
	prompt := schedulerSystemPrompt
	misleading := []string{
		"一次只能发布一个任务",
		"不支持“一次规划多个任务”",
		"不支持\"一次规划多个任务\"",
	}
	for _, phrase := range misleading {
		if strings.Contains(prompt, phrase) {
			t.Errorf("prompt 含误导性陈述 %q —— 该陈述会阻止同轮登记多个独立 Task，"+
				"会诱导 LLM 把独立任务串行化。见 2026-04-20 历史问题记录 P0-1", phrase)
		}
	}
}

// TestSchedulerSystemPrompt_ContainsParallelIndependentExecutionGuidance 断言 prompt
// 明确指引"同轮按顺序登记无依赖 Task，登记后由 Runner 并行执行"。
func TestSchedulerSystemPrompt_ContainsParallelIndependentExecutionGuidance(t *testing.T) {
	prompt := schedulerSystemPrompt
	// 必须同时出现以下两类关键词，才算覆盖"同轮登记、独立执行"这一场景：
	//   - 关系描述："无依赖" / "相互独立" / "独立任务"
	//   - 模式描述："同一轮" / "同一 reactLoop"
	hasIndependence := strings.Contains(prompt, "无依赖") ||
		strings.Contains(prompt, "相互独立") ||
		strings.Contains(prompt, "独立任务")
	hasParallelism := strings.Contains(prompt, "同一轮") ||
		strings.Contains(prompt, "同一个 reactLoop") ||
		strings.Contains(prompt, "同一 reactLoop")

	if !hasIndependence || !hasParallelism {
		t.Errorf("prompt 缺少同轮登记并并行执行独立任务的明确指引（独立关键词=%v, 同轮关键词=%v）—— "+
			"2026-04-20 测试暴露 scheduler 把独立任务串行化，需在 prompt 中加入"+
			"明确的'无依赖任务同轮登记、随后并行执行'示例。见历史问题记录 P0-1",
			hasIndependence, hasParallelism)
	}
}

// TestSchedulerSystemPrompt_PreservesUserOriginalConstraints 是 2026-04-23
// 随机测试暴露的 P2 "Scheduler 改写子任务 description 时丢失用户原始约束"
// 回归锁。
//
// 现象：用户 prompt 含明确否定约束"**不用撰写文字报告**"，但 scheduler 把
// 用户的顶层任务拆解为子任务时，子任务 description 变成"总结 / 撰写..."
// —— 负约束丢失，explorer/worker 按默认理解继续生成报告。
//
// 根因：schedulerSystemPrompt 未显式要求"拆分 / 改写子任务 description 时
// 必须保留用户的原始约束（尤其是否定性约束：不要、禁止、避免、不用 等）"。
// LLM 倾向于"润色"用户的话，但润色过程常丢失否定词。
//
// 本测试在修复前 🔴 RED：断言 schedulerSystemPrompt 含"保留用户原始约束 /
// 否定约束"相关规则。
//
// 修复方向：prompt 加一段明确的规则，类似：
//
//	"在将用户请求拆分为子任务时，必须**逐字保留**用户的否定性约束（如
//	'不要/禁止/避免/不用'等词）到子任务 description 中。不得以'更清晰的
//	表述'为由丢弃或弱化这些约束。"
func TestSchedulerSystemPrompt_PreservesUserOriginalConstraints(t *testing.T) {
	prompt := schedulerSystemPrompt

	// 一组：至少一个"保留原始意图/约束"的规则字样
	preserveSignals := []string{
		"保留用户",
		"保留原始",
		"原始约束",
		"逐字保留",
		"不得弱化",
		"不得改写用户",
		"用户的否定",
		"否定性约束",
		"否定约束",
	}
	// 二组：至少一个"否定性约束"例子，证明 prompt 作者意识到这类约束特殊
	negationExampleSignals := []string{
		"不要",
		"禁止",
		"避免",
		"不用",
		"don't",
		"avoid",
	}

	hasPreserveRule := false
	for _, k := range preserveSignals {
		if strings.Contains(prompt, k) {
			hasPreserveRule = true
			break
		}
	}
	// 第二组只要 prompt 里有任一否定词作为例子即可；目前 prompt 的"避免"
	// 是用在"避免能力不足的路由"语境下，不是"约束传递"语境。所以我们不
	// 靠第二组独立红，只用第一组作为主断言。
	hasAnyNegationExample := false
	for _, k := range negationExampleSignals {
		if strings.Contains(prompt, k) {
			hasAnyNegationExample = true
			break
		}
	}
	_ = hasAnyNegationExample // 仅作信息性 grep，不强断言

	if !hasPreserveRule {
		t.Errorf("schedulerSystemPrompt 缺少 `保留用户原始约束` 的规则段（期望含 %v 之一）—— "+
			"2026-04-23 随机测试暴露 scheduler 把用户 `不用撰写文字报告` 改写成 `总结...`，"+
			"否定约束丢失。见 2026-04-23 历史问题记录 P2",
			preserveSignals)
	}
}

// TestSchedulerSystemPrompt_NodeCapabilityGuidance 断言 prompt 含 publish_task
// tools/model 节点能力声明小节，且讲清"子集 ⊆ 现存路由白名单"硬约束（越界
// 任务无人认领、等 claim_starvation 告警修复）与裁剪后的收尾通道
// （submit_task_result 或纯文本）。
func TestSchedulerSystemPrompt_NodeCapabilityGuidance(t *testing.T) {
	prompt := schedulerSystemPrompt
	required := []string{
		"节点能力声明",
		"claim_starvation",
		"submit_task_result",
	}
	for _, phrase := range required {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("schedulerSystemPrompt 缺少节点能力声明指引 %q", phrase)
		}
	}
}
