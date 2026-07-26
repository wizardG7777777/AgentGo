package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

func defaultPlanBudget() model.PlanBudget {
	return model.PlanBudget{
		MaxPlanRevisions:  32,
		MaxTasksCreated:   128,
		MaxActiveTasks:    32,
		MaxAcceptanceRuns: 16,
		MaxWallTime:       24 * time.Hour,
		MaxTokens:         4_000_000,
	}
}

type planTaskBackend struct{ store store.TaskStore }

type planAcceptanceVerifier struct {
	store       store.TaskStore
	projectRoot string
}

func (v planAcceptanceVerifier) VerifyAcceptance(ctx context.Context, _ *model.Plan, run model.AcceptanceRun, result model.AcceptanceResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, taskID := range run.TargetTaskIDs {
		task, err := v.store.GetTask(taskID)
		if err != nil {
			return fmt.Errorf("target task %s is unavailable: %w", taskID, err)
		}
		if !model.IsTerminal(task.Status) {
			return fmt.Errorf("target task %s is still %s", taskID, task.Status)
		}
		if result.Verdict == model.AcceptanceVerdictPass && task.Status != model.TaskStatusCompleted {
			return fmt.Errorf("target task %s is %s, not completed", taskID, task.Status)
		}
		for _, depID := range task.Dependencies {
			dep, depErr := v.store.GetTask(depID)
			if depErr != nil {
				return fmt.Errorf("dependency %s of target %s is unavailable", depID, taskID)
			}
			if result.Verdict == model.AcceptanceVerdictPass && dep.Status != model.TaskStatusCompleted {
				return fmt.Errorf("dependency %s of target %s is %s, not completed", depID, taskID, dep.Status)
			}
			if result.Verdict != model.AcceptanceVerdictPass && !model.IsTerminal(dep.Status) {
				return fmt.Errorf("dependency %s of target %s is not terminal", depID, taskID)
			}
		}
		if result.Verdict == model.AcceptanceVerdictPass {
			actual := make(map[string]bool, len(task.Artifacts))
			for _, path := range task.Artifacts {
				actual[canonicalArtifactKey(v.projectRoot, path)] = true
			}
			for _, expected := range task.ExpectedArtifacts {
				clean := filepath.Clean(expected)
				if !actual[canonicalArtifactKey(v.projectRoot, expected)] {
					return fmt.Errorf("target %s lacks expected artifact %s", taskID, expected)
				}
				path, err := safeEvidencePath(v.projectRoot, clean)
				if err != nil {
					return fmt.Errorf("expected artifact %s is outside the project root: %w", expected, err)
				}
				if _, err := os.Stat(path); err != nil {
					return fmt.Errorf("expected artifact %s is not present: %w", expected, err)
				}
			}
		}
	}

	for _, evidence := range result.Evidence {
		if evidence.TaskID != "" {
			task, err := v.store.GetTask(evidence.TaskID)
			if err != nil {
				return fmt.Errorf("task status evidence %s references unavailable task %s: %w", evidence.ID, evidence.TaskID, err)
			}
			if evidence.Output != string(task.Status) {
				return fmt.Errorf("task status evidence %s claims %q for task %s, actual status is %q; output must exactly equal the bare status word, descriptive text is rejected",
					evidence.ID, evidence.Output, evidence.TaskID, task.Status)
			}
		}
		if evidence.FilePath != "" {
			path, err := safeEvidencePath(v.projectRoot, evidence.FilePath)
			if err != nil {
				return err
			}
			digest, err := hashFile(path)
			if err != nil {
				return fmt.Errorf("read evidence file %s: %w", evidence.FilePath, err)
			}
			if !strings.EqualFold(digest, evidence.FileHash) {
				return fmt.Errorf("evidence file hash mismatch for %s", evidence.FilePath)
			}
		}
		if evidence.Command != "" {
			found := false
			candidateTaskIDs := append([]string{run.RunnerTaskID}, run.TargetTaskIDs...)
			for _, taskID := range candidateTaskIDs {
				if taskID == "" {
					continue
				}
				records, _ := v.store.QueryToolCalls(taskID, "run_shell")
				for _, rec := range records {
					command, _ := rec.Args["command"].(string)
					if rec.Success && command == evidence.Command && !rec.Timestamp.Before(run.CreatedAt) &&
						rec.ExitCode != nil && evidence.ExitCode != nil && *rec.ExitCode == *evidence.ExitCode {
						fromProjectRoot, err := runShellWorkingDirMatchesProjectRoot(v.projectRoot, rec.Args)
						if err != nil || !fromProjectRoot {
							continue
						}
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				return fmt.Errorf("no successful run_shell fact for command %q; the command must verbatim match a run_shell call executed after this run was created, with working_dir at the project root, by the acceptance runner or a target task, and exit_code must equal that call", evidence.Command)
			}
		}
	}
	return nil
}

// canonicalArtifactKey 把 artifact 路径规整为项目根下的绝对路径用于比对。
// 历史数据中 task.Artifacts 可能混有两种登记形态（相对项目根 / 绝对路径），
// expected_artifacts 则按合约是相对路径——两侧先统一成同一形态再比较，
// 避免登记侧与校验侧路径形态不一致导致验收永远失败（2026-07-21 验收马拉松事故）。
func canonicalArtifactKey(projectRoot, p string) string {
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		if rootAbs, err := filepath.Abs(projectRoot); err == nil {
			clean = filepath.Join(rootAbs, clean)
		}
	}
	return clean
}

func runShellWorkingDirMatchesProjectRoot(projectRoot string, args map[string]any) (bool, error) {	rootAbs, err := filepath.Abs(projectRoot)
	if err != nil {
		return false, fmt.Errorf("resolve absolute project root: %w", err)
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return false, fmt.Errorf("resolve project root: %w", err)
	}

	workingDir := projectRoot
	if raw, exists := args["working_dir"]; exists {
		value, ok := raw.(string)
		if !ok {
			return false, fmt.Errorf("run_shell working_dir is not a string")
		}
		if value != "" {
			workingDir = value
		}
	}
	workingDirAbs, err := filepath.Abs(workingDir)
	if err != nil {
		return false, fmt.Errorf("resolve absolute run_shell working_dir: %w", err)
	}
	workingDirResolved, err := filepath.EvalSymlinks(workingDirAbs)
	if err != nil {
		return false, fmt.Errorf("resolve run_shell working_dir: %w", err)
	}
	return filepath.Clean(workingDirResolved) == filepath.Clean(rootResolved), nil
}

func safeEvidencePath(root, input string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	path := input
	if !filepath.IsAbs(path) {
		path = filepath.Join(rootAbs, path)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	pathResolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve evidence path %s: %w", input, err)
	}
	rel, err := filepath.Rel(rootResolved, pathResolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("evidence path escapes project root: %s", input)
	}
	return pathResolved, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type replanRequesterAdapter struct {
	coordinator *plan.Coordinator
	store       store.TaskStore
}

func (a replanRequesterAdapter) RequestReplanFromEvent(ev trace.Event, reasonCode, urgency, detail string) (string, error) {
	if a.coordinator == nil || a.store == nil {
		return "", fmt.Errorf("plan coordinator is unavailable")
	}
	task, err := a.store.GetTask(ev.TaskID)
	if err != nil || task == nil || task.PlanID == "" {
		return "", fmt.Errorf("task %s is not associated with a plan", ev.TaskID)
	}
	p, err := a.coordinator.Store().GetPlan(task.PlanID)
	if err != nil {
		return "", err
	}
	req, err := a.coordinator.RequestReplan(context.Background(), model.ReplanRequest{
		PlanID: task.PlanID, SourceTaskID: task.ID, SourceEvent: string(ev.Kind),
		ReasonCode: reasonCode, Detail: detail,
		ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
		Urgency:        model.ReplanUrgency(urgency),
		IdempotencyKey: replanEventIdempotencyKey(task.PlanID, reasonCode, ev),
	})
	if err != nil {
		return "", err
	}
	if req.ObservedStateVersion > p.ExecutionStateVersion {
		latest, _ := a.coordinator.Store().GetPlan(task.PlanID)
		trace.Emit(trace.Event{
			Kind: trace.KindReplanRequested, TaskID: task.ID, Reason: req.ReasonCode,
			Plan: planTraceContext(latest),
		})
	}
	return req.ID, nil
}

func replanEventIdempotencyKey(planID, reasonCode string, ev trace.Event) string {
	payload, err := json.Marshal(ev)
	if err != nil {
		payload = []byte(fmt.Sprintf("%s|%s|%s|%d|%d|%s|%s", ev.Kind, ev.TaskID,
			ev.AgentID, ev.Loop, ev.AttemptNo, ev.CallID, ev.Path))
	}
	sum := sha256.Sum256(append([]byte(planID+"\x00"+reasonCode+"\x00"), payload...))
	return "reactor-event:" + hex.EncodeToString(sum[:])
}

func (b planTaskBackend) PublishTask(ctx context.Context, spec plan.TaskSpec) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	task := &model.Task{
		PlanID:             spec.PlanID,
		Description:        spec.Description,
		EventType:          spec.EventType,
		EventSource:        spec.ParentTaskID,
		ParentTaskID:       spec.ParentTaskID,
		ReplyToAgentID:     spec.ReplyToAgentID,
		BatchID:            spec.BatchID,
		NodeRole:           spec.Role,
		Dependencies:       append([]string(nil), spec.Dependencies...),
		PlanMutationSource: "acceptance",
	}
	if spec.Role == model.PlanNodeRoleAcceptance {
		// A formal AcceptanceRun has exactly one bound runner identity. Even if a
		// custom verifier route has multiple replicas, only one may claim it.
		task.MaxConcurrency = 1
	}
	if spec.Metadata != nil {
		task.AcceptanceRunID = spec.Metadata["acceptance_run_id"]
	}
	if err := b.store.PublishTask(task); err != nil {
		return "", err
	}
	return task.ID, nil
}

// makeTaskPlanHooks 装配 Task→Plan 守卫。capCheck 是节点能力检查
// （capabilityRegistry.checker()，可为 nil——nil 时退化为纯 plan 守卫，
// 供不装配能力注册表的兼容测试路径使用）。
func makeTaskPlanHooks(coordinator *plan.Coordinator, capCheck CapabilityChecker) store.TaskPlanHooks {
	return store.TaskPlanHooks{
		Prepare: func(task *model.Task, parent *model.Task) error {
			return preparePlannedTask(coordinator, task, parent)
		},
		CanClaim: func(agentID string, task *model.Task) error {
			// 节点能力检查（双保险之一）：独立于下方 plan 守卫——能力 ⊆ 白名单
			// 是认领的硬前提，即使 plan 守卫缺省（coordinator=nil 的兼容装配）
			// 也不跳过；checker 对未声明 capability 的任务直接放行。
			if capCheck != nil {
				if err := capCheck(agentID, task); err != nil {
					return err
				}
			}
			if coordinator == nil || task == nil || task.PlanID == "" {
				return nil
			}
			p, err := coordinator.Store().GetPlan(task.PlanID)
			if err != nil {
				return err
			}
			if p.Status != model.PlanStatusRunning {
				// Finalization can be durably committed before the active controller
				// emits its one user-facing summary. After snapshot restore that
				// processing lease becomes pending, so allow exactly that persisted
				// controller identity to be reclaimed. Every worker and stale
				// controller remains frozen once the Plan is terminal.
				if model.IsPlanTerminal(p.Status) &&
					task.NodeRole == model.PlanNodeRoleController &&
					p.ActiveDecisionTaskID == task.ID {
					return nil
				}
				return fmt.Errorf("plan %s is %s", p.ID, p.Status)
			}
			if task.NodeRole == model.PlanNodeRoleController {
				if p.ActiveDecisionTaskID != task.ID {
					return fmt.Errorf("controller task %s is not active for plan %s", task.ID, p.ID)
				}
			} else {
				node, ok := p.Nodes[task.ID]
				if !ok {
					return fmt.Errorf("task %s is not registered in plan %s", task.ID, p.ID)
				}
				if node.RetiredRevision > 0 || !containsPlanNodeID(p.CurrentNodeIDs, task.ID) {
					return fmt.Errorf("task %s was retired from plan %s at revision %d", task.ID, p.ID, node.RetiredRevision)
				}
			}
			return nil
		},
		CanEvict: func(task *model.Task) bool {
			if coordinator == nil || task == nil || task.PlanID == "" {
				return true
			}
			p, err := coordinator.Store().GetPlan(task.PlanID)
			if err != nil {
				return false // preserve facts while authority is unavailable
			}
			if model.IsPlanTerminal(p.Status) {
				return true
			}
			if task.NodeRole == model.PlanNodeRoleController {
				return task.ID != p.ActiveDecisionTaskID
			}
			for _, currentID := range p.CurrentNodeIDs {
				if currentID == task.ID {
					return false
				}
			}
			return true
		},
		Mutated: func(m store.TaskMutation) error {
			return recordPlannedTaskMutation(coordinator, m)
		},
	}
}

func containsPlanNodeID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func preparePlannedTask(coordinator *plan.Coordinator, task, parent *model.Task) error {
	if coordinator == nil || task == nil || task.PlanID == "" {
		return nil
	}
	ctx := context.Background()
	if task.EventType == "__scheduler__" && task.PlanID == task.ID {
		created, err := coordinator.Create(ctx, plan.CreateInput{
			PlanID: task.PlanID, RootTaskID: task.ID, Budget: defaultPlanBudget(),
		})
		if errors.Is(err, plan.ErrPlanAlreadyExists) {
			created, err = coordinator.Store().GetPlan(task.PlanID)
		}
		if err != nil {
			return err
		}
		task.NodeRole = model.PlanNodeRoleController
		task.CreatedRevision = created.CurrentRevision
		return nil
	}

	current, err := coordinator.Store().GetPlan(task.PlanID)
	if err != nil {
		return err
	}
	if task.NodeRole == model.PlanNodeRoleController {
		switch task.PlanMutationSource {
		case "control":
			current, err = coordinator.ActivateController(ctx, task.PlanID, task.ID)
			if err != nil {
				return err
			}
		case "control-reserved":
			// Pause recovery publishes the next controller while the target Plan
			// is still non-runnable, then atomically resumes and activates this
			// exact pre-reserved identity.
			if current.Status != model.PlanStatusPausedAwaitingDecision && current.Status != model.PlanStatusBlocked {
				return fmt.Errorf("reserved controller requires paused plan %s, got %s", current.ID, current.Status)
			}
		default:
			return fmt.Errorf("planned controller activation is restricted to the control path")
		}
		task.CreatedRevision = current.CurrentRevision
		return nil
	}
	if task.PlanMutationSource != "scheduler" && task.PlanMutationSource != "acceptance" {
		parentID := ""
		if parent != nil {
			parentID = parent.ID
		}
		return fmt.Errorf("planned DAG mutation is restricted to Scheduler/Acceptance control paths (parent=%s)", parentID)
	}
	registerCtx := ctx
	if task.PlanMutationSource == "scheduler" {
		if parent == nil || parent.ID != current.ActiveDecisionTaskID ||
			parent.PlanID != current.ID || parent.NodeRole != model.PlanNodeRoleController || parent.EventType != "__scheduler__" {
			parentID := ""
			if parent != nil {
				parentID = parent.ID
			}
			return fmt.Errorf("%w: planned DAG mutation requires active controller %s (parent=%s)", plan.ErrControllerConflict, current.ActiveDecisionTaskID, parentID)
		}
		registerCtx = plan.WithControllerAuthority(ctx, parent.ID)
	}
	if current.ExecutionMode == model.ExecutionModeConverge && task.NodeRole == model.PlanNodeRoleInvestigation {
		return fmt.Errorf("plan is in CONVERGE mode; new investigation nodes are not allowed")
	}
	if task.NodeRole == "" {
		if task.PlanMutationSource == "acceptance" {
			task.NodeRole = model.PlanNodeRoleAcceptance
		} else if current.CurrentAcceptanceSpecRevision == 0 {
			task.NodeRole = model.PlanNodeRoleInvestigation
		} else {
			task.NodeRole = model.PlanNodeRoleImplementation
		}
	}

	for attempts := 0; attempts < 16; attempts++ {
		current, err = coordinator.Store().GetPlan(task.PlanID)
		if err != nil {
			return err
		}
		if task.PlanMutationSource == "scheduler" && current.ActiveDecisionTaskID != parent.ID {
			return fmt.Errorf("%w: controller=%s active=%s plan=%s", plan.ErrControllerConflict, parent.ID, current.ActiveDecisionTaskID, current.ID)
		}
		updated, registerErr := coordinator.RegisterTask(registerCtx, plan.RegisterTaskInput{
			PlanID: task.PlanID, ObservedRevision: current.CurrentRevision,
			Node: model.PlanNode{
				TaskID: task.ID, Title: plannedTaskTitle(task.Description), Status: task.Status,
				Role: task.NodeRole, Dependencies: append([]string(nil), task.Dependencies...),
				Supersedes: append([]string(nil), task.Supersedes...),
				// 节点能力投影：PlanNode 持久化进 plan-state.json，必须与 Task
				// 脱钩（store 侧 task 是独立克隆体），故按 plan 包惯例整体克隆
				// （与 Dependencies/Supersedes 的 append 克隆一致），不浅共享指针。
				Capability: cloneNodeCapability(task.Capability),
			},
		})
		if registerErr == nil {
			task.CreatedRevision = updated.CurrentRevision
			trace.Emit(trace.Event{
				Kind: trace.KindPlanRevisionChanged, TaskID: task.ID,
				Plan: planTraceContext(updated),
			})
			return nil
		}
		if !errors.Is(registerErr, plan.ErrRevisionConflict) {
			return registerErr
		}
	}
	return fmt.Errorf("register planned task %s: too many revision conflicts", task.ID)
}

func plannedTaskTitle(description string) string {
	title := strings.TrimSpace(description)
	if firstLine, _, ok := strings.Cut(title, "\n"); ok {
		title = strings.TrimSpace(firstLine)
	}
	runes := []rune(title)
	if len(runes) > 160 {
		title = string(runes[:160]) + "…"
	}
	return title
}

// cloneNodeCapability 克隆节点能力声明。Capability 只在发布时由控制面写入、
// 之后不可变，但 PlanNode 与 Task 分属两个持久化域（plan-state.json 与 store），
// 克隆可避免跨域共享指针——与上方 Dependencies/Supersedes 的克隆惯例一致。
func cloneNodeCapability(c *model.NodeCapability) *model.NodeCapability {
	if c == nil {
		return nil
	}
	return &model.NodeCapability{
		Tools: append([]string(nil), c.Tools...),
		Model: c.Model,
	}
}

// plannedMutationPrep 是 recordPlannedTaskMutation 的可批处理中间形态：
//   - immediate 非 nil：控制器终态等特例，需逐条立即执行（自身含读-改-写、
//     落盘与 trace，保持原同步语义，失败整体重试）；
//   - keyed 非 nil：普通节点变更，可并入 Coordinator.RecordTaskMutations
//     与其他变更共享一次落盘（C1 合并 fsync）；
//   - 两者皆 nil：无需处理（非计划任务 / 非终态控制器 / 守卫拒绝）。
type plannedMutationPrep struct {
	immediate func() error
	keyed     *plan.PlanTaskMutation
	wake      bool
	reason    string
}

// preparePlannedMutation 只做纯计算（读 task 快照，不落盘），把 store 变更
// 转换为可批量提交的 plan.PlanTaskMutation 或逐条立即执行的特例闭包。
func preparePlannedMutation(coordinator *plan.Coordinator, mutation store.TaskMutation) plannedMutationPrep {
	if coordinator == nil || mutation.Task == nil || mutation.Task.PlanID == "" {
		return plannedMutationPrep{}
	}
	task := mutation.Task
	if task.NodeRole == model.PlanNodeRoleController {
		if mutation.Kind != store.TaskMutationStatus || !model.IsTerminal(task.Status) {
			return plannedMutationPrep{}
		}
		// 控制器终态是罕见特例（每个控制器生命周期一次）：保留原同步路径——
		// 闭包内现读 Plan 再决定 MarkBlocked，失败整体重试，与原实现一致。
		return plannedMutationPrep{immediate: func() error {
			return handleTerminalControllerMutation(coordinator, task)
		}}
	}
	wake := mutation.Kind == store.TaskMutationStatus && model.IsTerminal(task.Status)
	reason := ""
	urgency := model.ReplanUrgencyNormal
	if wake {
		reason = "task_" + string(task.Status)
		if task.Status == model.TaskStatusFailed || task.Status == model.TaskStatusBlocked {
			urgency = model.ReplanUrgencyHigh
		}
	}
	summary := plannedTaskSummary(task)
	fingerprint := ""
	if task.Status == model.TaskStatusFailed || task.Status == model.TaskStatusBlocked {
		fingerprint = failureFingerprint(task.Error)
	}
	return plannedMutationPrep{
		keyed: &plan.PlanTaskMutation{
			PlanID: task.PlanID, TaskID: task.ID,
			Mutation: plan.TaskMutation{
				Kind: string(mutation.Kind), Status: task.Status, AcceptanceRunID: task.AcceptanceRunID, Summary: summary,
				FailureFingerprint: fingerprint, ArtifactRefs: append([]string(nil), task.Artifacts...),
				Wake: wake, ReasonCode: reason, SourceEvent: string(mutation.Kind), Urgency: urgency,
				OccurredAt: mutation.At,
			},
		},
		wake: wake, reason: reason,
	}
}

// handleTerminalControllerMutation 是原 recordPlannedTaskMutation 控制器分支的
// 原样抽取：活跃控制器在 Plan 仍 Running 时终态化 = 持久信号消费者消失，
// 把 Plan 标记为 Blocked 并发射 trace.KindPlanPaused。
func handleTerminalControllerMutation(coordinator *plan.Coordinator, task *model.Task) error {
	current, err := coordinator.Store().GetPlan(task.PlanID)
	if err != nil {
		return err
	}
	if current.ActiveDecisionTaskID != task.ID {
		return nil
	}
	// Normal controller completion is preceded by formal finalization or the
	// explicit completed_no_execution transition. A terminal controller on a
	// still-running Plan means the durable signal consumer disappeared.
	if model.IsPlanTerminal(current.Status) || current.Status != model.PlanStatusRunning {
		return nil
	}
	reason := fmt.Sprintf("controller_%s_before_plan_terminal:%s", task.Status, task.ID)
	blocked, err := coordinator.MarkBlocked(context.Background(), task.PlanID, reason)
	if errors.Is(err, plan.ErrPlanTerminal) {
		return nil
	}
	if err == nil {
		trace.Emit(trace.Event{Kind: trace.KindPlanPaused, TaskID: task.ID, Reason: reason, Plan: planTraceContext(blocked)})
	}
	return err
}

// emitPlanMutationWakeTrace 在变更已持久化后发射 KindReplanRequested。
// 批量提交后 GetPlan 读到的是整批之后的最新状态（版本可能更靠前），
// 仅为观测上下文，不影响 ReplanRequest 内已固化的 ObservedStateVersion。
func emitPlanMutationWakeTrace(coordinator *plan.Coordinator, prep plannedMutationPrep, version int64) {
	if !prep.wake || prep.keyed == nil {
		return
	}
	current, getErr := coordinator.Store().GetPlan(prep.keyed.PlanID)
	if getErr == nil {
		trace.Emit(trace.Event{
			Kind: trace.KindReplanRequested, TaskID: prep.keyed.TaskID, Reason: prep.reason,
			Plan: planTraceContext(current),
		})
	} else {
		trace.Emit(trace.Event{Kind: trace.KindReplanRequested, TaskID: prep.keyed.TaskID, Reason: prep.reason,
			Plan: &trace.PlanTraceContext{PlanID: prep.keyed.PlanID, ExecutionStateVersion: version}})
	}
}

// recordPlannedTaskMutation 保持既有同步语义（等价于批尺寸为 1 的
// applyPlannedMutationBatch）：无 batcher 的接线（store 单测、bootstrap 内
// 直接联调测试）经 makeTaskPlanHooks 走到这里，行为与 C1 改造前一致。
func recordPlannedTaskMutation(coordinator *plan.Coordinator, mutation store.TaskMutation) error {
	prep := preparePlannedMutation(coordinator, mutation)
	if prep.immediate != nil {
		return prep.immediate()
	}
	if prep.keyed == nil {
		return nil
	}
	versions, notified, errs := coordinator.RecordTaskMutations(context.Background(), []plan.PlanTaskMutation{*prep.keyed})
	if errs[0] != nil {
		return errs[0]
	}
	if notified[0] {
		emitPlanMutationWakeTrace(coordinator, prep, versions[0])
	}
	return nil
}

// applyPlannedMutationBatch 把一批 store 变更按 FIFO 顺序提交（C1 批落盘）：
// 连续的普通节点变更合并为一次 Coordinator.RecordTaskMutations（一次克隆 +
// 一次 fsync）；控制器终态特例冲断当前合并段、逐条立即执行——相对顺序与
// 逐条同步执行完全一致。每条最多 3 次尝试（对齐原 applyPlanMutationWithRetry）。
// 返回与 batch 对齐的错误切片（nil = 已落盘）。
func applyPlannedMutationBatch(coordinator *plan.Coordinator, batch []store.TaskMutation) []error {
	errs := make([]error, len(batch))
	type keyedItem struct {
		batchIdx int
		prep     plannedMutationPrep
	}
	var run []keyedItem
	// flush 提交当前合并段，失败子集最多再试 2 次（合计 3 次）。
	flush := func() {
		for attempt := 0; attempt < 3 && len(run) > 0; attempt++ {
			keyed := make([]plan.PlanTaskMutation, len(run))
			for j, it := range run {
				keyed[j] = *it.prep.keyed
			}
			versions, notified, cerrs := coordinator.RecordTaskMutations(context.Background(), keyed)
			var next []keyedItem
			for j, it := range run {
				if cerrs[j] == nil {
					if notified[j] {
						emitPlanMutationWakeTrace(coordinator, it.prep, versions[j])
					}
				} else {
					errs[it.batchIdx] = cerrs[j]
					next = append(next, it)
				}
			}
			run = next
		}
		run = nil
	}
	for i, m := range batch {
		prep := preparePlannedMutation(coordinator, m)
		switch {
		case prep.immediate != nil:
			flush()
			errs[i] = retryPlanMutationOp(prep.immediate)
		case prep.keyed != nil:
			run = append(run, keyedItem{batchIdx: i, prep: prep})
		}
	}
	flush()
	return errs
}

// retryPlanMutationOp 对齐 store 侧 applyPlanMutationWithRetry 的 3 次尝试策略。
func retryPlanMutationOp(op func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = op(); err == nil {
			return nil
		}
	}
	return err
}

func plannedTaskSummary(task *model.Task) string {
	if task == nil {
		return ""
	}
	summary := strings.TrimSpace(task.TransferNote)
	if summary == "" {
		summary = strings.TrimSpace(task.LastResponse)
	}
	if len([]rune(summary)) > 600 {
		summary = string([]rune(summary)[:600]) + "…"
	}
	return summary
}

func failureFingerprint(reason string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(reason), " "))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func planTraceContext(p *model.Plan) *trace.PlanTraceContext {
	if p == nil {
		return nil
	}
	return &trace.PlanTraceContext{
		PlanID: p.ID, PlanRevision: p.CurrentRevision,
		ExecutionStateVersion:  p.ExecutionStateVersion,
		AcceptanceSpecRevision: p.CurrentAcceptanceSpecRevision,
		GraphDigest:            p.CurrentGraphDigest,
	}
}
