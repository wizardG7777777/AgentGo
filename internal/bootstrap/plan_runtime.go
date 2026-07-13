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
				actual[filepath.Clean(path)] = true
			}
			for _, expected := range task.ExpectedArtifacts {
				clean := filepath.Clean(expected)
				if !actual[clean] {
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
				return fmt.Errorf("task status evidence %s claims %q for task %s, actual status is %q",
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
				return fmt.Errorf("no successful run_shell fact for command %q", evidence.Command)
			}
		}
	}
	return nil
}

func runShellWorkingDirMatchesProjectRoot(projectRoot string, args map[string]any) (bool, error) {
	rootAbs, err := filepath.Abs(projectRoot)
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
		NodeRole:           spec.Role,
		Dependencies:       append([]string(nil), spec.Dependencies...),
		PlanMutationSource: "acceptance",
	}
	if spec.Metadata != nil {
		task.AcceptanceRunID = spec.Metadata["acceptance_run_id"]
	}
	if err := b.store.PublishTask(task); err != nil {
		return "", err
	}
	return task.ID, nil
}

func makeTaskPlanHooks(coordinator *plan.Coordinator) store.TaskPlanHooks {
	return store.TaskPlanHooks{
		Prepare: func(task *model.Task, parent *model.Task) error {
			return preparePlannedTask(coordinator, task, parent)
		},
		CanClaim: func(task *model.Task) error {
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

func recordPlannedTaskMutation(coordinator *plan.Coordinator, mutation store.TaskMutation) error {
	if coordinator == nil || mutation.Task == nil || mutation.Task.PlanID == "" {
		return nil
	}
	task := mutation.Task
	if task.NodeRole == model.PlanNodeRoleController {
		if mutation.Kind != store.TaskMutationStatus || !model.IsTerminal(task.Status) {
			return nil
		}
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
	version, err := coordinator.RecordTaskMutation(context.Background(), task.PlanID, task.ID, plan.TaskMutation{
		Kind: string(mutation.Kind), Status: task.Status, AcceptanceRunID: task.AcceptanceRunID, Summary: summary,
		FailureFingerprint: fingerprint, ArtifactRefs: append([]string(nil), task.Artifacts...),
		Wake: wake, ReasonCode: reason, SourceEvent: string(mutation.Kind), Urgency: urgency,
		OccurredAt: mutation.At,
	})
	if err != nil {
		return err
	}
	if wake {
		current, getErr := coordinator.Store().GetPlan(task.PlanID)
		if getErr == nil {
			trace.Emit(trace.Event{
				Kind: trace.KindReplanRequested, TaskID: task.ID, Reason: reason,
				Plan: planTraceContext(current),
			})
		} else {
			trace.Emit(trace.Event{Kind: trace.KindReplanRequested, TaskID: task.ID, Reason: reason,
				Plan: &trace.PlanTraceContext{PlanID: task.PlanID, ExecutionStateVersion: version}})
		}
	}
	return nil
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
