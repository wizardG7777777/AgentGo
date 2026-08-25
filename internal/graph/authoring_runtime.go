package graph

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"agentgo/internal/trace"
)

// StartDefinitionRequest 是 Authoring Definition→Runtime Execution 的完整 CAS
// 前提。调用方必须同时携带 definition 与 contract digest，不能只猜 revision。
type StartDefinitionRequest struct {
	StartID                    string
	GraphID                    string
	ExpectedDefinitionRevision int64
	ExpectedDefinitionDigest   string
	ExpectedContractDigest     string
	SessionID                  string
	OwnerTaskID                string
}

// StartDefinitionResult 返回 durable StartIntent 与 Runtime root identity。
type StartDefinitionResult struct {
	Intent           StartIntent `json:"intent"`
	ExecutionRef     string      `json:"execution_ref"`
	RootActivationID string      `json:"root_activation_id"`
}

// AuthoringRuntime 是 AuthoringStore 与既有 Graph Runtime 之间的 C3 事务
// adapter。它不暴露给 LLM；C4 工具只能调用本对象，不能绕过它直调 SubmitGraph。
type AuthoringRuntime struct {
	Authoring *AuthoringStore
	Runtime   *Runtime
}

// StartDefinition 保证顺序：BeginStart durable → CreateExecution/root activation
// → CompleteStart durable。任一崩溃窗口都可用同一请求幂等补完。
func (a *AuthoringRuntime) StartDefinition(ctx context.Context, req StartDefinitionRequest) (StartDefinitionResult, error) {
	if a == nil || a.Authoring == nil || a.Runtime == nil {
		return StartDefinitionResult{}, fmt.Errorf("graph authoring: Start adapter 依赖未注入")
	}
	if strings.TrimSpace(req.StartID) == "" || strings.TrimSpace(req.GraphID) == "" ||
		req.ExpectedDefinitionRevision <= 0 || strings.TrimSpace(req.ExpectedDefinitionDigest) == "" ||
		strings.TrimSpace(req.ExpectedContractDigest) == "" || strings.TrimSpace(req.OwnerTaskID) == "" {
		return StartDefinitionResult{}, fmt.Errorf("graph authoring: StartDefinition 请求缺少 start/graph/revision/digest/owner 前提")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	definition, ok := a.Authoring.GetDefinition(req.GraphID, req.ExpectedDefinitionRevision)
	if !ok {
		return StartDefinitionResult{}, fmt.Errorf("%w: %s@%d", ErrDefinitionNotFound, req.GraphID, req.ExpectedDefinitionRevision)
	}
	if definition.DefinitionDigest != req.ExpectedDefinitionDigest || definition.ContractDigest != req.ExpectedContractDigest {
		return StartDefinitionResult{}, fmt.Errorf("graph authoring: StartDefinition digest 与 Definition %s@%d 不一致", req.GraphID, req.ExpectedDefinitionRevision)
	}
	if definition.OwnerTaskID != strings.TrimSpace(req.OwnerTaskID) || definition.SessionID != req.SessionID {
		return StartDefinitionResult{}, fmt.Errorf("graph authoring: StartDefinition owner/session 与 Definition 不一致")
	}

	intent, err := a.Authoring.BeginStart(StartIntent{
		StartID: req.StartID, GraphID: req.GraphID,
		DefinitionRevision: definition.Revision, DefinitionDigest: definition.DefinitionDigest,
		ContractDigest: definition.ContractDigest, SessionID: definition.SessionID,
		OwnerTaskID: definition.OwnerTaskID, RootActivationID: definition.Body.Root + "@1",
	})
	if err != nil {
		return StartDefinitionResult{}, err
	}
	if intent.Status == StartStarted {
		return startResult(*intent), nil
	}
	if intent.Status == StartFailed {
		return startResult(*intent), fmt.Errorf("graph authoring: StartIntent %s 已失败（%s）: %s", intent.StartID, intent.FailureCode, intent.FailureReason)
	}
	if err := ctx.Err(); err != nil {
		return a.failStart(*intent, "caller_cancelled", err)
	}

	doc, err := graphExecutionDocument(*definition)
	if err != nil {
		return a.failStart(*intent, "definition_projection_failed", err)
	}
	rootActivationID, startErr := a.Runtime.startCommittedExecution(doc)
	if startErr != nil {
		// 并发调用可能已在 Runtime 成功并完成同一 Intent；先读 durable 权威，
		// 不能用迟到错误覆盖 started。
		if current, found := a.Authoring.GetStartIntent(intent.StartID); found && current.Status == StartStarted {
			return startResult(*current), nil
		}
		return a.failStart(*intent, "execution_start_failed", startErr)
	}
	if rootActivationID != "" {
		intent.RootActivationID = rootActivationID
	}
	completed, completeErr := a.Authoring.CompleteStart(intent.StartID, intent.IntentRevision, executionRefForGraph(intent.GraphID))
	if completeErr != nil {
		var conflict *AuthoringRevisionConflictError
		if errors.As(completeErr, &conflict) {
			if current, found := a.Authoring.GetStartIntent(intent.StartID); found && current.Status == StartStarted {
				return startResult(*current), nil
			}
		}
		return StartDefinitionResult{}, completeErr
	}
	completed.RootActivationID = intent.RootActivationID
	return startResult(*completed), nil
}

func (a *AuthoringRuntime) failStart(intent StartIntent, code string, cause error) (StartDefinitionResult, error) {
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	failed, err := a.Authoring.FailStart(intent.StartID, intent.IntentRevision, code, reason)
	if err != nil {
		if current, found := a.Authoring.GetStartIntent(intent.StartID); found {
			if current.Status == StartStarted {
				return startResult(*current), nil
			}
			if current.Status == StartFailed {
				return startResult(*current), cause
			}
		}
		return StartDefinitionResult{}, errors.Join(cause, err)
	}
	return startResult(*failed), cause
}

func startResult(intent StartIntent) StartDefinitionResult {
	return StartDefinitionResult{
		Intent: intent, ExecutionRef: intent.ExecutionRef,
		RootActivationID: intent.RootActivationID,
	}
}

func executionRefForGraph(graphID string) string { return "graph:" + graphID }

// graphExecutionDocument 显式执行 authoring→Runtime 类型转换。Definition 的
// L2/L4 policy refs 与 Contract bindings 暂不塞入 Runtime metadata；C3 只投影
// Runtime 已有的强类型字段，避免下层解析自由文本。
func graphExecutionDocument(definition GraphDefinition) (*GraphDocument, error) {
	nodes := make(map[string]Node, len(definition.Body.Nodes))
	for id, source := range definition.Body.Nodes {
		if source.Kind == KindEnd && !source.EndOutcome.IsValid() {
			return nil, fmt.Errorf("graph authoring: end 节点 %s 缺少合法 DefinitionEndOutcome", id)
		}
		if source.Kind != KindEnd && source.EndOutcome != "" {
			return nil, fmt.Errorf("graph authoring: 非 end 节点 %s 不得携带 DefinitionEndOutcome", id)
		}
		outcome, err := runtimeEndOutcome(source.EndOutcome)
		if err != nil {
			return nil, fmt.Errorf("graph authoring: 节点 %s: %w", id, err)
		}
		nodes[id] = Node{
			Kind: source.Kind, Task: source.Task, Capability: source.Capability,
			Next: source.Next, Wait: source.Wait, Tool: source.Tool, Subgraph: source.Subgraph,
			EndOutcome: outcome, Status: NodeInactive,
			OutputContract:      source.OutputContract,
			ProgressContractRef: source.ProgressContractRef,
			ContextPolicyRef:    source.ContextPolicyRef,
			Metadata:            source.Metadata, Extensions: source.Extensions,
		}
	}
	return &GraphDocument{
		Schema: definition.Body.Schema, GraphID: definition.GraphID,
		RunID: definition.Body.RunID, RunContract: definition.Body.RunContract,
		DefinitionDigestVersion: definition.DefinitionDigestVersion,
		DefinitionDigest:        definition.DefinitionDigest, ContractDigest: definition.ContractDigest,
		SourceProposalID: definition.SourceProposalID,
		Revision:         definition.Revision, StateVersion: 0, Root: definition.Body.Root,
		Status: GraphPending, SessionID: definition.SessionID, Nodes: nodes,
	}, nil
}

func runtimeEndOutcome(outcome DefinitionEndOutcome) (EndOutcome, error) {
	switch outcome {
	case "":
		return "", nil
	case DefinitionEndSuccess:
		return EndSuccess, nil
	case DefinitionEndFailed:
		return EndFailed, nil
	case DefinitionEndBlocked:
		return EndBlocked, nil
	case DefinitionEndCancelled:
		return EndCancelled, nil
	default:
		return "", fmt.Errorf("非法 DefinitionEndOutcome %q", outcome)
	}
}

// startCommittedExecution 幂等创建并启动与 immutable Definition 绑定的
// GraphExecution。调用方必须先 durable BeginStart；Runtime 本身不创建 Intent。
func (rt *Runtime) startCommittedExecution(doc *GraphDocument) (string, error) {
	if rt == nil || rt.store == nil {
		return "", fmt.Errorf("graph: Runtime/Store 未注入")
	}
	if doc == nil {
		return "", fmt.Errorf("graph: Definition Execution 文档为 nil")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.synchronousSteps = 0

	if existing, found := rt.store.Get(doc.GraphID); found {
		if err := validateExecutionDefinitionBinding(existing, doc); err != nil {
			return "", err
		}
		if !existing.Status.IsTerminal() {
			if err := rt.resumeGraphLocked(doc.GraphID); err != nil {
				return activationOf(existing.Nodes[existing.Root]), err
			}
		}
		current, _ := rt.store.Get(doc.GraphID)
		if current == nil {
			return "", fmt.Errorf("graph: 已存在 Execution %s 在启动对账后消失", doc.GraphID)
		}
		activationID := activationOf(current.Nodes[current.Root])
		if activationID == "" {
			return "", fmt.Errorf("graph: Execution %s 未形成 root Activation", doc.GraphID)
		}
		return activationID, nil
	}
	// 一个 Run 对应一个用户请求顶层 Graph。终态 Graph 后的恢复只能走
	// GraphChangeProposal/新用户 Run，不能用另一个 GraphID 重新执行业务。
	// subgraph 在父 activation 已冻结 ChildGraphID 后启动，按该 durable 事实豁免。
	if err := rt.validateSingleTopLevelGraphLocked(doc); err != nil {
		return "", err
	}

	if err := rt.store.createExecution(doc); err != nil {
		trace.Emit(trace.Event{Kind: trace.KindGraphSubmissionRejected, GraphID: doc.GraphID, Error: err.Error()})
		return "", err
	}
	trace.Emit(trace.Event{
		Kind: trace.KindGraphSubmitted, GraphID: doc.GraphID,
		Description: fmt.Sprintf("revision=%d definition_digest=%s", doc.Revision, truncateDigest(doc.DefinitionDigest)),
	})
	if err := rt.resumeGraphLocked(doc.GraphID); err != nil {
		current, _ := rt.store.Get(doc.GraphID)
		if current != nil {
			return activationOf(current.Nodes[current.Root]), err
		}
		return "", err
	}
	current, ok := rt.store.Get(doc.GraphID)
	if !ok {
		return "", fmt.Errorf("graph: Execution %s 创建后不可读", doc.GraphID)
	}
	activationID := activationOf(current.Nodes[current.Root])
	if activationID == "" {
		return "", fmt.Errorf("graph: Execution %s 启动后缺少 root Activation", doc.GraphID)
	}
	return activationID, nil
}

func (rt *Runtime) runtimeGraphIsChildLocked(graphID string) bool {
	for _, candidate := range rt.store.List() {
		doc, ok := rt.store.Get(candidate.GraphID)
		if !ok {
			continue
		}
		for _, childID := range materializedChildGraphIDs(doc) {
			if childID == graphID {
				return true
			}
		}
	}
	return false
}

// AdoptCommittedDefinition 将 AuthoringStore 已 commit 的新 revision 换入当前
// Execution。它不重复 root、不重解释旧 Result；在途 Activation 先补冻结快照。
func (rt *Runtime) AdoptCommittedDefinition(definition GraphDefinition) error {
	if rt == nil || rt.store == nil {
		return fmt.Errorf("graph: Runtime/Store 未注入")
	}
	doc, err := graphExecutionDocument(definition)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.adoptCommittedDefinitionLocked(definition, doc)
}

func (rt *Runtime) adoptCommittedDefinitionLocked(definition GraphDefinition, doc *GraphDocument) error {
	current, err := rt.graph(definition.GraphID)
	if err != nil {
		return err
	}
	if current.Revision == definition.Revision && current.DefinitionDigest == definition.DefinitionDigest {
		return nil
	}
	if definition.Revision != current.Revision+1 {
		return &RevisionConflictError{GraphID: definition.GraphID, Base: definition.Revision - 1, Current: current.Revision}
	}
	upserts := make([]NodeDefUpsert, 0, len(definition.Body.Nodes))
	for id := range definition.Body.Nodes {
		upserts = append(upserts, NodeDefUpsert{ID: id})
	}
	if err := rt.freezePatchedActivationsLocked(definition.GraphID, DefinitionPatch{UpsertNodes: upserts}); err != nil {
		return err
	}
	if err := rt.store.adoptDefinition(definition.GraphID, current.Revision, current.DefinitionDigest, doc); err != nil {
		return err
	}
	trace.Emit(trace.Event{
		Kind: trace.KindGraphRevisionCommitted, GraphID: definition.GraphID,
		Description: fmt.Sprintf("new_revision=%d definition_digest=%s source_change=%s",
			definition.Revision, truncateDigest(definition.DefinitionDigest), definition.SourceProposalID),
	})
	return nil
}

// CommitGraphChangeAndAdopt 在 Runtime 锁内完成 Authoring commit→Execution
// adoption，防止两次 durable 写之间被节点终态推进穿插。进程崩溃仍由启动
// ReconcileCommittedDefinitions 补第二段。
func (a *AuthoringRuntime) CommitGraphChangeAndAdopt(changeID string, expectedProposalRevision int64, reportID string) (*GraphDefinition, error) {
	if a == nil || a.Authoring == nil || a.Runtime == nil || a.Runtime.store == nil {
		return nil, fmt.Errorf("graph authoring: change commit adapter 依赖未注入")
	}
	a.Runtime.mu.Lock()
	defer a.Runtime.mu.Unlock()
	change, ok := a.Authoring.GetGraphChangeProposal(changeID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrGraphChangeNotFound, changeID)
	}
	current, err := a.Runtime.graph(change.GraphID)
	if err != nil {
		return nil, err
	}
	if current.Status.IsTerminal() {
		return nil, fmt.Errorf("graph: 终态 Execution %s 不接受 GraphChange commit", change.GraphID)
	}
	definition, err := a.Authoring.CommitGraphChange(changeID, expectedProposalRevision, reportID)
	if err != nil {
		return nil, err
	}
	doc, err := graphExecutionDocument(*definition)
	if err != nil {
		return nil, err
	}
	if err := a.Runtime.adoptCommittedDefinitionLocked(*definition, doc); err != nil {
		return nil, fmt.Errorf("GraphChange Definition 已 commit，Runtime adoption 待恢复: %w", err)
	}
	return definition, nil
}

// ReconcileCommittedDefinitions 修复「Authoring change commit 已 durable、Runtime
// adoption 尚未落盘」的跨 journal 崩溃窗口。只处理已有 Execution；尚未 start
// 的 pending Definition 保持 parked。每个 revision 按序幂等 adoption。
func (a *AuthoringRuntime) ReconcileCommittedDefinitions() error {
	if a == nil || a.Authoring == nil || a.Runtime == nil || a.Runtime.store == nil {
		return fmt.Errorf("graph authoring: reconcile 依赖未注入")
	}
	var errs []error
	for _, latest := range a.Authoring.ListLatestDefinitions() {
		execution, exists := a.Runtime.store.Get(latest.GraphID)
		if !exists {
			continue
		}
		if execution.Revision > latest.Revision {
			errs = append(errs, fmt.Errorf("graph %s Execution revision=%d 超过 Authoring latest=%d",
				latest.GraphID, execution.Revision, latest.Revision))
			continue
		}
		for revision := execution.Revision + 1; revision <= latest.Revision; revision++ {
			definition, ok := a.Authoring.GetDefinition(latest.GraphID, revision)
			if !ok {
				errs = append(errs, fmt.Errorf("graph %s 缺少待 adoption Definition revision=%d", latest.GraphID, revision))
				break
			}
			if err := a.Runtime.AdoptCommittedDefinition(*definition); err != nil {
				errs = append(errs, fmt.Errorf("graph %s adoption revision=%d: %w", latest.GraphID, revision, err))
				break
			}
		}
	}
	return errors.Join(errs...)
}

func validateExecutionDefinitionBinding(existing, expected *GraphDocument) error {
	if existing == nil || expected == nil {
		return fmt.Errorf("graph: Execution Definition 绑定不可为空")
	}
	if existing.GraphID != expected.GraphID || existing.Revision != expected.Revision ||
		existing.Schema != expected.Schema || existing.Root != expected.Root ||
		existing.DefinitionDigestVersion != expected.DefinitionDigestVersion ||
		existing.DefinitionDigest != expected.DefinitionDigest || existing.ContractDigest != expected.ContractDigest ||
		existing.SourceProposalID != expected.SourceProposalID || existing.RunID != expected.RunID ||
		existing.SessionID != expected.SessionID || !reflect.DeepEqual(existing.RunContract, expected.RunContract) {
		return fmt.Errorf("graph: 已存在 Execution %s 与待启动 immutable Definition 身份不一致", expected.GraphID)
	}
	return nil
}
