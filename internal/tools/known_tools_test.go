package tools

import (
	"sort"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/agenttemplate"
	"agentgo/internal/interaction"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// registerAllGroupsFully 用"能让每个 Group 注册其完整工具集"的最小依赖构造
// 全部 ToolGroup 并注册到 r（F7）。各 Group 的 nil-skip 规则对应关系：
//   - LocalReadGroup / LocalWriteGroup / ShellGroup：无 skip 规则，全量注册
//   - WebGroup：Provider 非 nil 才注册（fakeSearchProvider）
//   - MetaGroup：publish_task 需 Store、send_message 需 MBRegistry
//   - PlanControlGroup：Store / Holder 非 nil 才注册（request_replan 恒注册）；
//     submit_task_result 另需 FinalizationNotifier + SubmitState 提交通道注入
//   - SchedulerGroup：Store 非 nil 注册 cancel_task/probe_directory，
//     Holder 非 nil 才补 get_task_result/report_done/report_progress
//   - AgentTemplateGroup：list 需 Catalog，provision 另需
//     Provisioner / Store / Holder
//   - GraphControlGroup：无 skip 规则，无条件注册（nil 依赖调用时报错）
func registerAllGroupsFully(t *testing.T, r *agent.ToolRegistry) {
	t.Helper()

	taskStore := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	catalog, err := agenttemplate.Load(agenttemplate.LoadOptions{
		DefaultModel: "test-model", ValidateTools: ValidateToolNames,
	})
	if err != nil {
		t.Fatalf("加载内置 agent template 目录: %v", err)
	}

	RegisterGroups(r,
		LocalReadGroup{Workdir: &DefaultWorkdir{ProjectRoot: t.TempDir()}},
		ContentRefGroup{},
		LocalWriteGroup{
			LocalReadGroup: LocalReadGroup{Workdir: &DefaultWorkdir{ProjectRoot: t.TempDir()}},
			Roster:         &recordingRoster{},
			AgentID:        "agent-1",
		},
		WebGroup{Provider: &fakeSearchProvider{}},
		ShellGroup{Workdir: &DefaultWorkdir{ProjectRoot: t.TempDir()}, AgentID: "agent-1"},
		MetaGroup{
			Store: newFakeStore(), MBRegistry: mailbox.NewRegistry(8),
			Interactions: interaction.NewService(nil), AgentID: "agent-1",
		},
		PlanControlGroup{
			Store: taskStore, Holder: &fakeHolder{id: "controller"},
			// submit_task_result 需提交通道注入才注册；全量并集守护按完整依赖装配。
			FinalizationNotifier: &fakeFinalizationNotifier{},
			SubmitState:          agent.NewSubmitState(),
		},
		SchedulerGroup{
			Store: newFakeStore(), Holder: &fakeHolder{id: "sched"}, ProjectRoot: t.TempDir(),
		},
		AgentTemplateGroup{
			Catalog: catalog, Provisioner: &recordingTemplateProvisioner{},
			Store: taskStore, Holder: &fakeHolder{id: "controller"},
		},
		// GraphControlGroup 无条件注册两个工具（nil 依赖在调用时报明确中文
		// 错误），全量并集守护按零依赖装配即可。
		GraphControlGroup{},
		GraphAuthoringGroup{},
	)
}

// TestAllToolNames_UnionMatchesAllGroups 是 F7 的全量守护：AllToolNames 是手工
// 维护的镜像，必须与"全部 ToolGroup 以完整依赖注册后的工具并集"严格相等——
// 双向校验：新增工具忘记收录（registered ∖ known）与删除工具留下陈旧条目
// （known ∖ registered）都必须失败。
func TestAllToolNames_UnionMatchesAllGroups(t *testing.T) {
	r := agent.NewToolRegistry()
	registerAllGroupsFully(t, r)

	registered := map[string]bool{}
	for _, d := range r.Defs() {
		registered[d.Name] = true
	}
	known := map[string]bool{}
	for _, n := range AllToolNames {
		known[n] = true
	}

	var missing, stale []string
	for name := range registered {
		if !known[name] {
			missing = append(missing, name)
		}
	}
	for _, name := range AllToolNames {
		if !registered[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("已注册但未收录进 AllToolNames 的工具（请补充镜像）: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("AllToolNames 中无任何 ToolGroup 注册的陈旧条目（请删除）: %v", stale)
	}
	if len(registered) != len(AllToolNames) {
		t.Errorf("注册并集大小=%d, AllToolNames 大小=%d", len(registered), len(AllToolNames))
	}
}

// TestAllToolNames_NoDuplicates 校验 AllToolNames 内部无重复条目——重复会让
// 上面的集合相等校验在"镜像多一条、实际少一条"时误判通过。
func TestAllToolNames_NoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range AllToolNames {
		if seen[n] {
			t.Errorf("AllToolNames 存在重复条目 %q", n)
		}
		seen[n] = true
	}
}
