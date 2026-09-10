package graph

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type AgentTaskDispatch struct {
	GraphID            string
	SessionID          string
	RunID              string
	DefinitionRevision int64
	Node               AgentTaskNode
	ActivationID       string
	TaskID             string
	Inputs             FrozenDataflowInputs
}

type AgentTaskBoard interface {
	CheckAgentTask(context.Context, AgentTaskDispatch) error
	PublishAgentTask(context.Context, AgentTaskDispatch) error
	CancelAgentTask(context.Context, string, string) error
}

type CandidateCommitter interface {
	ValidateCandidate(ctx context.Context, graphID, runID, candidateRef string) error
	CommitCandidate(ctx context.Context, completionID, graphID, candidateRef string) (string, error)
}

type DataflowRuntime struct {
	suspended map[string]bool
	Store     *DataflowStore
	Board     AgentTaskBoard
	Delivery  CandidateCommitter
	mu        sync.Mutex
	// 泵锁只串行派发，不阻止其它 goroutine 持久化任务终态。
	steps map[string]*sync.Mutex
}

func (r *DataflowRuntime) SuspendSession(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.suspended == nil {
		r.suspended = map[string]bool{}
	}
	r.suspended[id] = true
}
func (r *DataflowRuntime) ResumeSession(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.suspended, id)
}
func (r *DataflowRuntime) sessionSuspended(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.suspended[id]
}
func (r *DataflowRuntime) CancelSession(ctx context.Context, id, reason string) error {
	states, err := r.Store.List(id)
	if err != nil {
		return err
	}
	for _, s := range states {
		if s.Terminal() {
			continue
		}
		if _, err := r.Cancel(ctx, s.Definition.GraphID, "session-cancel", reason); err != nil {
			return err
		}
	}
	return nil
}

func NewDataflowRuntime(store *DataflowStore, board AgentTaskBoard) *DataflowRuntime {
	return &DataflowRuntime{Store: store, Board: board, steps: map[string]*sync.Mutex{}}
}

func (r *DataflowRuntime) stepLock(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock := r.steps[id]
	if lock == nil {
		lock = &sync.Mutex{}
		r.steps[id] = lock
	}
	return lock
}

func (r *DataflowRuntime) Create(ctx context.Context, requestID string, def DataflowDefinition) (DataflowSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return DataflowSnapshot{}, err
	}
	if err := dataflowIdentity("request_id", requestID); err != nil {
		return DataflowSnapshot{}, err
	}
	if err := ValidateDataflowDefinition(def); err != nil {
		return DataflowSnapshot{}, err
	}
	if def.Revision != 1 {
		return DataflowSnapshot{}, fmt.Errorf("新图 revision 必须为 1")
	}
	digest, err := def.Digest()
	if err != nil {
		return DataflowSnapshot{}, err
	}
	return r.Store.transact(def.GraphID, func(s *DataflowSnapshot, exists bool) error {
		if exists {
			receipt, ok := s.Requests[requestID]
			if !ok || receipt.Digest != digest || receipt.Action != "create" {
				return fmt.Errorf("创建身份已被不同请求占用")
			}
			return nil
		}
		*s = DataflowSnapshot{Schema: DataflowSchema, Definition: def, Status: "pending", Executions: map[string]AgentTaskExecution{}, Results: map[string]AgentTaskResult{}, ExternalInputs: map[string]map[int64]DataflowInputValue{}, Requests: map[string]DataflowRequestReceipt{requestID: {Digest: digest, Revision: 1, Action: "create"}}, RemovedNodes: map[string]bool{}}
		return nil
	})
}

type DataflowChange struct {
	RequestID         string                       `json:"request_id"`
	ExpectedRevision  int64                        `json:"expected_revision"`
	Add               []AgentTaskNode              `json:"add,omitempty"`
	Update            []AgentTaskNode              `json:"update,omitempty"`
	Remove            []string                     `json:"remove,omitempty"`
	InputDeclarations map[string]DataflowInputSpec `json:"input_declarations,omitempty"`
}

func (r *DataflowRuntime) Apply(ctx context.Context, id string, change DataflowChange) (DataflowSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return DataflowSnapshot{}, err
	}
	if err := dataflowIdentity("request_id", change.RequestID); err != nil {
		return DataflowSnapshot{}, err
	}
	digest, err := dataflowDigest(change)
	if err != nil {
		return DataflowSnapshot{}, err
	}
	state, err := r.Store.transact(id, func(s *DataflowSnapshot, exists bool) error {
		if !exists {
			return fmt.Errorf("图不存在")
		}
		if receipt, ok := s.Requests[change.RequestID]; ok {
			if receipt.Action != "update" || receipt.Digest != digest {
				return fmt.Errorf("request_id 已绑定其它内容")
			}
			return nil
		}
		if s.Terminal() || s.Status == "finalizing" {
			return fmt.Errorf("已结束或结算中的图不能扩展")
		}
		if s.Definition.Revision != change.ExpectedRevision {
			return fmt.Errorf("图版本冲突: expected=%d actual=%d", change.ExpectedRevision, s.Definition.Revision)
		}
		if len(change.Add)+len(change.Update)+len(change.Remove)+len(change.InputDeclarations) == 0 {
			return fmt.Errorf("图变更为空")
		}
		nodes := map[string]AgentTaskNode{}
		for _, node := range s.Definition.Nodes {
			nodes[node.NodeID] = node
		}
		changed := map[string]bool{}
		for _, node := range change.Add {
			if _, ok := nodes[node.NodeID]; ok || s.RemovedNodes[node.NodeID] || changed[node.NodeID] {
				return fmt.Errorf("node_id %s 已使用", node.NodeID)
			}
			nodes[node.NodeID] = node
			changed[node.NodeID] = true
		}
		for _, node := range change.Update {
			if _, ok := nodes[node.NodeID]; !ok || changed[node.NodeID] {
				return fmt.Errorf("更新节点 %s 不存在或重复", node.NodeID)
			}
			if exec := s.Executions[node.NodeID]; exec.ActivationID != "" {
				return fmt.Errorf("节点 %s 已激活；追加实例而不是改写", node.NodeID)
			}
			nodes[node.NodeID] = node
			changed[node.NodeID] = true
			delete(s.Executions, node.NodeID)
		}
		for _, nodeID := range change.Remove {
			if _, ok := nodes[nodeID]; !ok || changed[nodeID] {
				return fmt.Errorf("删除节点 %s 不存在或重复", nodeID)
			}
			if exec := s.Executions[nodeID]; exec.ActivationID != "" {
				return fmt.Errorf("不能删除已有执行事实的节点 %s", nodeID)
			}
			delete(nodes, nodeID)
			delete(s.Executions, nodeID)
			s.RemovedNodes[nodeID] = true
			changed[nodeID] = true
		}
		if s.Definition.Inputs == nil {
			s.Definition.Inputs = map[string]DataflowInputSpec{}
		}
		for name, spec := range change.InputDeclarations {
			if _, ok := s.Definition.Inputs[name]; ok {
				return fmt.Errorf("已声明图输入 %s 不可覆盖", name)
			}
			s.Definition.Inputs[name] = spec
		}
		s.Definition.Nodes = nil
		for _, nodeID := range sortedDataflowKeys(nodes) {
			s.Definition.Nodes = append(s.Definition.Nodes, nodes[nodeID])
		}
		s.Definition.Revision++
		if err := ValidateDataflowDefinition(s.Definition); err != nil {
			return err
		}
		s.Requests[change.RequestID] = DataflowRequestReceipt{Digest: digest, Revision: s.Definition.Revision, Action: "update"}
		return nil
	})
	if err != nil {
		return state, err
	}
	if state.Status == "open" {
		if err := r.Step(ctx, id); err != nil {
			return state, err
		}
		state, _, err = r.Store.Get(id)
	}
	return state, err
}

func (r *DataflowRuntime) ProvideInput(ctx context.Context, id, requestID, port string, version, expectedRevision int64, value any, candidateRef string) (DataflowSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return DataflowSnapshot{}, err
	}
	if err := dataflowIdentity("request_id", requestID); err != nil {
		return DataflowSnapshot{}, err
	}
	digest, err := dataflowDigest(struct {
		Port             string
		Version          int64
		ExpectedRevision int64
		Value            any
		Candidate        string
	}{port, version, expectedRevision, value, candidateRef})
	if err != nil {
		return DataflowSnapshot{}, err
	}
	state, err := r.Store.transact(id, func(s *DataflowSnapshot, exists bool) error {
		if !exists {
			return fmt.Errorf("图不存在")
		}
		if old, ok := s.Requests[requestID]; ok {
			if old.Digest != digest || old.Action != "provide_input" {
				return fmt.Errorf("输入请求身份冲突")
			}
			return nil
		}
		if s.Terminal() || s.Status == "finalizing" {
			return fmt.Errorf("图已结束或正在结算")
		}
		if s.Definition.Revision != expectedRevision {
			return fmt.Errorf("输入绑定图版本冲突")
		}
		spec, ok := s.Definition.Inputs[port]
		if !ok || version < 1 {
			return fmt.Errorf("图输入未声明或版本非法")
		}
		if err := validateDataflowValue(spec.Schema, value); err != nil {
			return err
		}
		if s.ExternalInputs[port] == nil {
			s.ExternalInputs[port] = map[int64]DataflowInputValue{}
		}
		if _, ok := s.ExternalInputs[port][version]; ok {
			return fmt.Errorf("图输入版本不可覆盖")
		}
		ref, _ := dataflowDigest(struct {
			Graph   string
			Port    string
			Version int64
			Digest  string
		}{id, port, version, digest})
		s.ExternalInputs[port][version] = DataflowInputValue{Ref: "graph-input:" + strings.TrimPrefix(ref, "sha256:"), Value: value, CandidateRef: candidateRef}
		s.Requests[requestID] = DataflowRequestReceipt{Digest: digest, Revision: s.Definition.Revision, Action: "provide_input"}
		s.enqueuePlanning("input_available", "", port)
		return nil
	})
	if err == nil && state.Status == "open" {
		err = r.Step(ctx, id)
		if err == nil {
			state, _, err = r.Store.Get(id)
		}
	}
	return state, err
}

func (r *DataflowRuntime) Start(ctx context.Context, id, requestID string, revision int64) (DataflowSnapshot, error) {
	if r.Board == nil {
		return DataflowSnapshot{}, fmt.Errorf("AgentTask 执行面未装配")
	}
	if err := dataflowIdentity("request_id", requestID); err != nil {
		return DataflowSnapshot{}, err
	}
	digest, _ := dataflowDigest(struct {
		ID       string
		Revision int64
	}{id, revision})
	state, err := r.Store.transact(id, func(s *DataflowSnapshot, exists bool) error {
		if !exists {
			return fmt.Errorf("图不存在")
		}
		if old, ok := s.Requests[requestID]; ok {
			if old.Digest != digest || old.Action != "start" {
				return fmt.Errorf("启动请求身份冲突")
			}
			return nil
		}
		if s.Status != "pending" || s.Definition.Revision != revision {
			return fmt.Errorf("图状态或版本不允许启动")
		}
		ready := false
		for _, node := range s.Definition.Nodes {
			inputs, waiting, err := ResolveDataflowInputs(s.Definition, node.NodeID, s.Results, s.ExternalInputs, s.Executions)
			if err != nil {
				return err
			}
			if waiting == "" {
				dispatch := dataflowDispatch(*s, node, inputs)
				if r.Board.CheckAgentTask(ctx, dispatch) == nil {
					ready = true
					break
				}
			}
		}
		if !ready {
			return fmt.Errorf("图没有输入及执行能力均就绪的入口")
		}
		s.Status = "open"
		s.Requests[requestID] = DataflowRequestReceipt{Digest: digest, Revision: revision, Action: "start"}
		return nil
	})
	if err != nil {
		return state, err
	}
	if err = r.Step(ctx, id); err != nil {
		return state, err
	}
	state, _, err = r.Store.Get(id)
	return state, err
}

func dataflowDispatch(state DataflowSnapshot, node AgentTaskNode, inputs FrozenDataflowInputs) AgentTaskDispatch {
	digest, _ := dataflowDigest([]string{state.Definition.SessionID, state.Definition.GraphID, node.NodeID})
	taskID := "agent-task-" + strings.TrimPrefix(digest, "sha256:")[:32]
	return AgentTaskDispatch{GraphID: state.Definition.GraphID, SessionID: state.Definition.SessionID, RunID: state.Definition.RunID, DefinitionRevision: state.Definition.Revision, Node: node, TaskID: taskID, ActivationID: node.NodeID + "@1", Inputs: inputs}
}

func (r *DataflowRuntime) Step(ctx context.Context, id string) error {
	lock := r.stepLock(id)
	lock.Lock()
	defer lock.Unlock()
	if r.Board == nil {
		return fmt.Errorf("执行面未装配")
	}
	state, ok, err := r.Store.Get(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("图不存在")
	}
	if state.Status != "open" {
		return nil
	}
	if r.sessionSuspended(state.Definition.SessionID) {
		return nil
	}
	for _, node := range state.Definition.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, _, err := r.Store.Get(id)
		if err != nil {
			return err
		}
		if current.Status != "open" {
			return nil
		}
		found := false
		for _, latest := range current.Definition.Nodes {
			if latest.NodeID == node.NodeID {
				node = latest
				found = true
				break
			}
		}
		if !found {
			continue
		}
		exec := current.Executions[node.NodeID]
		if exec.ActivationID != "" && exec.Status != "dispatching" {
			continue
		}
		inputs, waiting, inputErr := ResolveDataflowInputs(current.Definition, node.NodeID, current.Results, current.ExternalInputs, current.Executions)
		if inputErr != nil {
			waiting = "invalid_input:" + inputErr.Error()
		}
		dispatch := dataflowDispatch(current, node, inputs)
		if waiting == "" {
			if err := r.Board.CheckAgentTask(ctx, dispatch); err != nil {
				waiting = "waiting_executor:" + err.Error()
			}
		}
		if waiting != "" {
			_, err = r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
				if s.Status != "open" {
					return nil
				}
				if s.Definition.Revision != current.Definition.Revision {
					return fmt.Errorf("派发期间图版本变化，请重新扫描")
				}
				old := s.Executions[node.NodeID]
				if old.ActivationID != "" {
					return nil
				}
				s.Executions[node.NodeID] = AgentTaskExecution{NodeID: node.NodeID, Status: "waiting", WaitingReason: waiting}
				if inputErr != nil || strings.HasPrefix(waiting, "waiting_executor:") {
					s.enqueuePlanning("execution_blocked", node.NodeID, waiting)
				}
				return nil
			})
			if err != nil {
				return err
			}
			continue
		}
		reserved := false
		_, err = r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
			if s.Status != "open" {
				return nil
			}
			if s.Definition.Revision != current.Definition.Revision {
				return fmt.Errorf("派发期间图版本变化，请重新扫描")
			}
			old := s.Executions[node.NodeID]
			if old.ActivationID != "" {
				if old.Status == "dispatching" {
					dispatch.Node = old.Definition
					dispatch.Inputs = old.Inputs
					reserved = true
				}
				return nil
			}
			s.Executions[node.NodeID] = AgentTaskExecution{NodeID: node.NodeID, ActivationID: dispatch.ActivationID, TaskID: dispatch.TaskID, Status: "dispatching", Definition: node, Inputs: inputs}
			reserved = true
			return nil
		})
		if err != nil {
			return err
		}
		if !reserved {
			continue
		}
		if err := r.Board.PublishAgentTask(ctx, dispatch); err != nil {
			_, saveErr := r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
				old := s.Executions[node.NodeID]
				old.Error = err.Error()
				s.Executions[node.NodeID] = old
				s.enqueuePlanning("dispatch_failed", node.NodeID, err.Error())
				return nil
			})
			if saveErr != nil {
				return saveErr
			}
			return err
		}
		_, err = r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
			old := s.Executions[node.NodeID]
			if old.Status == "dispatching" {
				old.Status = "running"
				old.Error = ""
				s.Executions[node.NodeID] = old
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	_, err = r.Store.transact(id, func(s *DataflowSnapshot, _ bool) error {
		if s.Status != "open" {
			return nil
		}
		active := false
		for _, e := range s.Executions {
			if e.Status == "running" || e.Status == "dispatching" {
				active = true
			}
		}
		activity := "running"
		if !active {
			activity = "waiting_planning"
		}
		if s.Activity != activity {
			s.Activity = activity
			if !active {
				s.enqueuePlanning("planning_needed", "", "当前没有执行中的任务，请根据结果扩展或完成图")
			}
		}

		return nil
	})
	return err
}
