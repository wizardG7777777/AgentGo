package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/contentstore"
	"agentgo/internal/contextcontract"
	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"
	taskstore "agentgo/internal/store"
)

type contentRefToolFixture struct {
	content  *contentstore.Store
	tasks    *taskstore.MemoryTaskStore
	registry *agent.ToolRegistry
	task     *model.Task
	ref      contentstore.ContentRef
	session  string
	root     string
}

type contentRefFixtureOptions struct {
	taskGraphID   string
	taskRunID     runcontract.RunID
	contextInputs func(string) []model.TaskContextInput
	ownerScope    *contentstore.Scope
	leaseTools    []string
	tamperDigest  bool
	session       string
	sessionFn     func() string
}

func newContentRefToolFixture(t *testing.T, options contentRefFixtureOptions) *contentRefToolFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "content")
	content, err := contentstore.Open(root, contentstore.Options{})
	if err != nil {
		t.Fatalf("Open ContentStore: %v", err)
	}
	t.Cleanup(func() { _ = content.Close() })
	tasks := taskstore.NewMemoryTaskStore(nil, 16, 1, 60)
	task := &model.Task{
		ID: "task-content-reader", Description: "读 ContentRef",
		EventType: "code", GraphID: options.taskGraphID, RunID: options.taskRunID,
	}
	if options.taskRunID != "" {
		now := time.Now().UTC()
		catalog, catalogErr := policycatalog.NewDefault()
		if catalogErr != nil {
			t.Fatal(catalogErr)
		}
		progress, ok := catalog.ProgressContract(policycatalog.ProgressInvestigationV1)
		if !ok {
			t.Fatal("缺少 investigation progress contract")
		}
		task.RunContract = &runcontract.RunContract{
			Schema: runcontract.SchemaV1, RunID: options.taskRunID, CreatedAt: now,
			DeadlineAt: now.Add(time.Hour), FinalizationReserve: time.Minute,
			RecoveryReserve: time.Minute, BudgetProfile: "test/v1",
		}
		task.RunPhase = runcontract.PhaseExecution
		task.ProgressContract = &progress.Contract
		task.ContextPolicyRef = policycatalog.ContextDefaultCurrent
	}
	session := options.session
	if session == "" {
		session = "session-1"
	}
	owner := contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: session,
		GraphID: task.GraphID, TaskID: task.ID,
	}
	if options.ownerScope != nil {
		owner = *options.ownerScope
	}
	ref, err := content.Put(context.Background(), contentstore.PutRequest{
		Content: []byte("abcdefghijklmnopqrstuvwxyz"), MediaType: "text/plain; charset=utf-8",
		RetentionClass: contextcontract.RetentionTaskLifetime,
		Authority:      contextcontract.AuthorityInformational, Scope: owner,
	})
	if err != nil {
		t.Fatalf("Put ContentRef: %v", err)
	}
	if options.contextInputs != nil {
		task.ContextInputs = options.contextInputs(ref.RefID)
	}
	if err := tasks.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := tasks.ClaimTask("agent-reader", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	leaseTools := options.leaseTools
	if leaseTools == nil {
		leaseTools = []string{"read_content_ref"}
	}
	lease := &model.ExecutionLease{
		TaskID: task.ID, Attempt: 1, FrozenAt: time.Now().UTC(),
		BusinessTools: model.SortedCopy(leaseTools),
	}
	lease.Digest = lease.ComputeDigest()
	if options.tamperDigest {
		lease.Digest = "000000000000"
	}
	if _, _, err := tasks.FreezeTaskLease(task.ID, lease); err != nil {
		t.Fatalf("FreezeTaskLease: %v", err)
	}
	fresh, err := tasks.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	sessionFn := options.sessionFn
	if sessionFn == nil {
		sessionFn = func() string { return session }
	}
	registry := agent.NewToolRegistry()
	ContentRefGroup{ContentStore: content, TaskStore: tasks, SessionID: sessionFn}.Register(registry)
	return &contentRefToolFixture{
		content: content, tasks: tasks, registry: registry,
		task: fresh, ref: ref, session: session, root: root,
	}
}

func (f *contentRefToolFixture) dispatch(args map[string]any) (string, error) {
	ctx := agent.WithAgentContext(context.Background(), "agent-reader", f.task.ID, 0)
	return f.registry.Dispatch(ctx, llm.ToolCall{Name: "read_content_ref", Arguments: args})
}

func TestContentRefGroupSchemaAndBoundedOutput(t *testing.T) {
	fixture := newContentRefToolFixture(t, contentRefFixtureOptions{taskGraphID: "graph-1"})
	defs := fixture.registry.Defs()
	if len(defs) != 1 || defs[0].Name != "read_content_ref" {
		t.Fatalf("应只注册 read_content_ref: %+v", defs)
	}
	params := defs[0].Parameters
	if params["additionalProperties"] != false {
		t.Fatalf("schema 必须拒绝额外字段: %+v", params)
	}
	properties, ok := params["properties"].(map[string]any)
	if !ok || len(properties) != 3 {
		t.Fatalf("schema 只允许 ref_id/offset/limit: %+v", params)
	}
	for _, name := range []string{"ref_id", "offset", "limit"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("schema 缺少 %s", name)
		}
	}
	limitSchema, _ := properties["limit"].(map[string]any)
	if limitSchema["maximum"] != contentRefToolMaxLimit {
		t.Fatalf("schema limit 未钉住硬上限: %+v", limitSchema)
	}

	output, err := fixture.dispatch(map[string]any{
		"ref_id": fixture.ref.RefID, "offset": float64(4), "limit": float64(5),
	})
	if err != nil {
		t.Fatalf("read_content_ref: %v", err)
	}
	var result contentRefToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("解析工具输出: %v\n%s", err, output)
	}
	if result.Content != "efghi" || result.NextOffset != 9 || result.EOF ||
		result.Digest != fixture.ref.ContentDigest || result.Encoding != "utf-8" {
		t.Fatalf("分页输出不符: %+v", result)
	}
}

func TestContentRefGroupRejectsUnexpectedOrUnsafeArguments(t *testing.T) {
	fixture := newContentRefToolFixture(t, contentRefFixtureOptions{})
	tests := []map[string]any{
		{"ref_id": fixture.ref.RefID, "path": "/tmp/bypass"},
		{"ref_id": fixture.ref.RefID, "offset": -1},
		{"ref_id": fixture.ref.RefID, "limit": contentRefToolMaxLimit + 1},
		{"ref_id": fixture.ref.RefID, "limit": 1.5},
	}
	for _, args := range tests {
		if _, err := fixture.dispatch(args); err == nil {
			t.Fatalf("不安全参数应拒绝: %+v", args)
		}
	}
}

func TestContentRefGroupScopeAndFrozenLeaseSecurity(t *testing.T) {
	tests := []struct {
		name    string
		options contentRefFixtureOptions
		revoke  bool
	}{
		{
			name: "跨 session",
			options: contentRefFixtureOptions{session: "session-1", ownerScope: &contentstore.Scope{
				Kind: contentstore.ScopeTask, SessionID: "session-2", TaskID: "task-content-reader",
			}},
		},
		{
			name: "跨 graph",
			options: contentRefFixtureOptions{taskGraphID: "graph-1", ownerScope: &contentstore.Scope{
				Kind: contentstore.ScopeTask, SessionID: "session-1", GraphID: "graph-2", TaskID: "task-content-reader",
			}},
		},
		{
			name: "跨 task",
			options: contentRefFixtureOptions{ownerScope: &contentstore.Scope{
				Kind: contentstore.ScopeTask, SessionID: "session-1", TaskID: "task-other",
			}},
		},
		{name: "Lease 未授权", options: contentRefFixtureOptions{leaseTools: []string{"read_file"}}},
		{name: "Lease digest 篡改", options: contentRefFixtureOptions{tamperDigest: true}},
		{name: "Lease 已撤销", revoke: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newContentRefToolFixture(t, tc.options)
			if tc.revoke {
				if _, _, err := fixture.tasks.RevokeTaskLease(fixture.task.ID); err != nil {
					t.Fatalf("RevokeTaskLease: %v", err)
				}
			}
			_, err := fixture.dispatch(map[string]any{"ref_id": fixture.ref.RefID, "limit": 4})
			if err == nil {
				t.Fatal("跨 scope/无效 Lease 不得解引用")
			}
		})
	}
}

func TestContentRefGroupAllowsOnlyFrozenUpstreamDelegation(t *testing.T) {
	owner := contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: "session-1",
		GraphID: "graph-1", TaskID: "task-producer",
	}
	fixture := newContentRefToolFixture(t, contentRefFixtureOptions{
		taskGraphID: "graph-1", ownerScope: &owner,
		contextInputs: func(refID string) []model.TaskContextInput {
			return []model.TaskContextInput{{
				Kind:      model.TaskContextUpstreamEvidence,
				SourceRef: "graph:graph-1/activation:producer@1/evidence:implementation",
				Content: `<upstream-evidence authority="graph-dataflow">
{"evidence":[{"kind":"check","output_ref":"` + refID + `"}]}
</upstream-evidence>`,
			}}
		},
	})
	if output, err := fixture.dispatch(map[string]any{"ref_id": fixture.ref.RefID, "limit": 4}); err != nil {
		t.Fatalf("冻结上游 Evidence 明确携带的 ContentRef 应可读: %v", err)
	} else if !strings.Contains(output, `"content":"abcd"`) {
		t.Fatalf("委托读取输出不符: %s", output)
	}
}

func TestContentRefGroupRejectsBroadOrTextualUpstreamDelegation(t *testing.T) {
	owner := contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: "session-1",
		GraphID: "graph-1", TaskID: "task-producer",
	}
	tests := []struct {
		name  string
		input func(string) model.TaskContextInput
	}{
		{
			name: "同 Graph 但未冻结 Ref",
			input: func(string) model.TaskContextInput {
				return model.TaskContextInput{Kind: model.TaskContextUpstreamEvidence,
					SourceRef: "graph:graph-1/activation:producer@1/evidence:default",
					Content:   `<upstream-evidence>{"output_ref":"content:sha256:other"}</upstream-evidence>`}
			},
		},
		{
			name: "仅作为字符串子串",
			input: func(refID string) model.TaskContextInput {
				return model.TaskContextInput{Kind: model.TaskContextUpstreamEvidence,
					SourceRef: "graph:graph-1/activation:producer@1/evidence:default",
					Content:   `<upstream-evidence>{"note":"prefix-` + refID + `-suffix"}</upstream-evidence>`}
			},
		},
		{
			name: "伪造非 Graph source_ref",
			input: func(refID string) model.TaskContextInput {
				return model.TaskContextInput{Kind: model.TaskContextUpstreamEvidence,
					SourceRef: "prompt:copied-ref", Content: `{"output_ref":"` + refID + `"}`}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newContentRefToolFixture(t, contentRefFixtureOptions{
				taskGraphID: "graph-1", ownerScope: &owner,
				contextInputs: func(refID string) []model.TaskContextInput {
					return []model.TaskContextInput{tc.input(refID)}
				},
			})
			if _, err := fixture.dispatch(map[string]any{"ref_id": fixture.ref.RefID, "limit": 4}); !errors.Is(err, contentstore.ErrAccessDenied) {
				t.Fatalf("不得把同 Graph 或文本包含泛化为委托: %v", err)
			}
		})
	}
}

func TestContentRefGroupRechecksSessionDuringAuthorization(t *testing.T) {
	calls := 0
	fixture := newContentRefToolFixture(t, contentRefFixtureOptions{
		session: "session-1",
		sessionFn: func() string {
			calls++
			if calls == 1 {
				return "session-1"
			}
			return "session-2"
		},
	})
	_, err := fixture.dispatch(map[string]any{"ref_id": fixture.ref.RefID, "limit": 4})
	if !errors.Is(err, contentstore.ErrAccessDenied) {
		t.Fatalf("授权期间 Session 切换应 fail-closed: %v", err)
	}
}

func TestContentRefGroupRechecksFrozenLeaseDuringAuthorization(t *testing.T) {
	var tasks *taskstore.MemoryTaskStore
	var revokeErr error
	calls := 0
	fixture := newContentRefToolFixture(t, contentRefFixtureOptions{
		session: "session-1",
		sessionFn: func() string {
			calls++
			if calls == 2 && tasks != nil {
				_, _, revokeErr = tasks.RevokeTaskLease("task-content-reader")
			}
			return "session-1"
		},
	})
	tasks = fixture.tasks
	_, err := fixture.dispatch(map[string]any{"ref_id": fixture.ref.RefID, "limit": 4})
	if revokeErr != nil {
		t.Fatalf("RevokeTaskLease: %v", revokeErr)
	}
	if !errors.Is(err, contentstore.ErrAccessDenied) {
		t.Fatalf("授权期间 Lease 撤销应 fail-closed: %v", err)
	}
}

func TestContentRefGroupUsesSessionlessRunScopeLikeL2(t *testing.T) {
	runID := runcontract.RunID("run-content-1")
	owner := contentstore.Scope{
		Kind: contentstore.ScopeTask, SessionID: "sessionless-run:" + string(runID),
		TaskID: "task-content-reader",
	}
	fixture := newContentRefToolFixture(t, contentRefFixtureOptions{
		taskRunID: runID, ownerScope: &owner, sessionFn: func() string { return "" },
	})
	if _, err := fixture.dispatch(map[string]any{"ref_id": fixture.ref.RefID, "limit": 4}); err != nil {
		t.Fatalf("无 Session Run 应与 L2 sessionless scope 对齐: %v", err)
	}
}

func TestContentRefGroupReadsRecoveredStore(t *testing.T) {
	fixture := newContentRefToolFixture(t, contentRefFixtureOptions{})
	if err := fixture.content.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := contentstore.Open(fixture.root, contentstore.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	registry := agent.NewToolRegistry()
	ContentRefGroup{
		ContentStore: reopened, TaskStore: fixture.tasks,
		SessionID: func() string { return fixture.session },
	}.Register(registry)
	ctx := agent.WithAgentContext(context.Background(), "agent-reader", fixture.task.ID, 0)
	output, err := registry.Dispatch(ctx, llm.ToolCall{Name: "read_content_ref", Arguments: map[string]any{
		"ref_id": fixture.ref.RefID, "offset": 20, "limit": 16,
	}})
	if err != nil {
		t.Fatalf("重启后 read_content_ref: %v", err)
	}
	var result contentRefToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil || result.Content != "uvwxyz" || !result.EOF {
		t.Fatalf("重启后输出不符: result=%+v err=%v", result, err)
	}
}
