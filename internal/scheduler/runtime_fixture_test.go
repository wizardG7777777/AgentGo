package scheduler

import (
	"agentgo/internal/testagent"
	"agentgo/internal/testmodel"
	"testing"

	"io"

	"agentgo/internal/agenttemplate"

	"agentgo/internal/config"
	"agentgo/internal/effect"
	"agentgo/internal/gate"
	"agentgo/internal/graph"
	"agentgo/internal/interaction"
	"agentgo/internal/llm"
	"agentgo/internal/mailbox"
	"agentgo/internal/memory"
	"agentgo/internal/model"
	"agentgo/internal/modes"
	"agentgo/internal/roster"
	"agentgo/internal/store"
)

func newTestScheduler(t *testing.T,
	s store.TaskStore,
	r roster.Roster,
	llmClient llm.Invoker,
	eventCh <-chan model.Event,
	cfg *config.Config,
	cancelReg *store.TaskCancelRegistry,
	mbRegistry *mailbox.Registry,
	interactions *interaction.Service,
	gateReg *gate.Registry,
	storeView store.StoreHookView,
	recordToolCall func(string, store.ToolCallRecord),
	agentRegistry *AgentRegistry,
	templateCatalog *agenttemplate.Catalog,
	templateProvisioner agenttemplate.Provisioner,
	memoryStore memory.Store,
	userOutput io.Writer,
	resultOutput io.Writer,
	modeStore *modes.Store,
	graphRuntime *graph.Runtime,
	graphStore *graph.Store,
	// effectJournal 是 V6 §4 H2b 共享副作用账本（internal/effect）：
	// scheduler 的写工具 / run_shell / send_message 经它记录
	// prepared/settled。nil 时不记账（单测直构场景）。
	effectJournal *effect.Journal,
	graphAuthoring ...GraphAuthoringDeps,
) *Bundle {
	t.Helper()
	if len(graphAuthoring) == 0 {
		graphAuthoring = []GraphAuthoringDeps{{ContextRuntime: testmodel.Runtime(t)}}
	} else if !graphAuthoring[0].ContextRuntime.Ready() {
		graphAuthoring[0].ContextRuntime = testmodel.Runtime(t)
	}
	b := New(s, r, llmClient, eventCh, cfg, cancelReg, mbRegistry, interactions, gateReg, storeView, recordToolCall, agentRegistry, templateCatalog, templateProvisioner, memoryStore, userOutput, resultOutput, modeStore, graphRuntime, graphStore, effectJournal, graphAuthoring...)
	b.SchedulerExec.Inner = testagent.Wrap(b.SchedulerExec.Inner)
	return b
}
