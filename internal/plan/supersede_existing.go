package plan

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"agentgo/internal/model"
)

type SupersedeExistingInput struct {
	PlanID             string
	ObservedRevision   int64
	RetireTaskIDs      []string
	ReplacementTaskIDs []string
	Reason             string
}

// SupersedeExisting retires current nodes and links already-published current
// replacement Tasks. It is the normal integration path when Scheduler first
// publishes replacement Tasks through TaskStore and then records semantics.
func (c *Coordinator) SupersedeExisting(ctx context.Context, in SupersedeExistingInput) (*model.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	retired := sortedUniqueStrings(in.RetireTaskIDs)
	replacements := sortedUniqueStrings(in.ReplacementTaskIDs)
	if len(retired) == 0 || len(replacements) == 0 {
		return nil, fmt.Errorf("supersede requires retired and replacement task ids")
	}
	if id, overlaps := firstStringSetOverlap(retired, replacements); overlaps {
		return nil, fmt.Errorf("supersede retire and replacement sets overlap at %s", id)
	}
	var postErr error
	var notify bool
	err := c.store.update(func(state *persistentState) error {
		rec, ok := state.Plans[in.PlanID]
		if !ok {
			return ErrPlanNotFound
		}
		p := &rec.Plan
		if err := ensureControllerAuthority(ctx, p); err != nil {
			return err
		}
		if err := ensurePlanMutable(p); err != nil {
			return err
		}
		if p.CurrentRevision != in.ObservedRevision {
			return fmt.Errorf("%w: observed=%d current=%d", ErrRevisionConflict, in.ObservedRevision, p.CurrentRevision)
		}
		current := make(map[string]bool, len(p.CurrentNodeIDs))
		for _, id := range p.CurrentNodeIDs {
			current[id] = true
		}
		for _, id := range append(append([]string(nil), retired...), replacements...) {
			if !current[id] {
				return fmt.Errorf("%w: %s is not current", ErrNodeNotFound, id)
			}
		}
		if reason := budgetReason(p, budgetDelta{revisions: 1}); reason != "" {
			now := time.Now().UTC()
			pausePlan(p, reason, "supersede rejected by budget", now)
			p.ExecutionStateVersion++
			_, _, _ = appendRequest(rec, model.ReplanRequest{
				PlanID: p.ID, SourceEvent: "budget", ReasonCode: "budget_exhausted",
				ObservedRevision: p.CurrentRevision, ObservedStateVersion: p.ExecutionStateVersion,
				Urgency: model.ReplanUrgencyHigh,
			})
			notify = true
			postErr = fmt.Errorf("%w: %s", ErrBudgetExceeded, reason)
			return nil
		}
		nextRevision := p.CurrentRevision + 1
		for _, id := range replacements {
			node := p.Nodes[id]
			node.Supersedes = sortedUniqueStrings(append(node.Supersedes, retired...))
			p.Nodes[id] = node
		}
		for _, id := range retired {
			p.Nodes[id] = compactRetiredNode(p.Nodes[id], nextRevision, replacements, in.Reason)
			delete(current, id)
		}
		// 依赖边改写：剩余当前节点里指向退休节点的依赖全部重定向到替代
		// 节点集合。没有这一步，退休节点仍被引用，validateCurrentGraph 以
		// ErrDependencyNotFound 回滚整个 supersede——acceptance 角色节点
		// 同样在此覆盖（2026-07-27 真实运行：验收节点引用被退休节点，
		// 替代失败、plan 楔死）。
		redirectSupersededDeps(p, current, retired, replacements)
		p.CurrentNodeIDs = p.CurrentNodeIDs[:0]
		for id := range current {
			p.CurrentNodeIDs = append(p.CurrentNodeIDs, id)
		}
		sort.Strings(p.CurrentNodeIDs)
		if err := validateCurrentGraph(p); err != nil {
			return err
		}
		p.CurrentRevision = nextRevision
		p.Usage.PlanRevisions++
		p.CurrentGraphDigest = ComputeGraphDigest(p)
		now := time.Now().UTC()
		addedWarning, warningErr := appendSoftBudgetRequest(rec, p, now)
		if warningErr != nil {
			return warningErr
		}
		if addedWarning {
			notify = true
		}
		p.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	if notify {
		c.notify(in.PlanID)
	}
	p, getErr := c.store.GetPlan(in.PlanID)
	if postErr != nil {
		return p, postErr
	}
	return p, getErr
}

func firstStringSetOverlap(left, right []string) (string, bool) {
	seen := make(map[string]struct{}, len(left))
	for _, value := range left {
		seen[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := seen[value]; ok {
			return value, true
		}
	}
	return "", false
}

// redirectSupersededDeps 把 current 中每个剩余节点的依赖边里指向 retired
// 的条目重定向到 replacements（多个替代节点时展开为对全部替代节点的依赖，
// 结果排序去重）。节点角色不做区分——acceptance 角色节点同样改写。
// ApplySupersede 与 SupersedeExisting 共用。
func redirectSupersededDeps(p *model.Plan, current map[string]bool, retired, replacements []string) {
	retiredSet := make(map[string]bool, len(retired))
	for _, id := range retired {
		retiredSet[id] = true
	}
	for id := range current {
		node := p.Nodes[id]
		changed := false
		redirected := make([]string, 0, len(node.Dependencies))
		for _, dep := range node.Dependencies {
			if retiredSet[dep] {
				redirected = append(redirected, replacements...)
				changed = true
			} else {
				redirected = append(redirected, dep)
			}
		}
		if changed {
			node.Dependencies = sortedUniqueStrings(redirected)
			p.Nodes[id] = node
		}
	}
}

// compactRetiredNode keeps cold history intentionally small. The latest graph
// remains fully described by CurrentNodeIDs; retired nodes retain identity,
// title, terminal fact and a bounded summary only.
func compactRetiredNode(node model.PlanNode, retiredRevision int64, replacements []string, reason string) model.PlanNode {
	node.RetiredRevision = retiredRevision
	node.SupersededBy = strings.Join(replacements, ",")
	if reason != "" {
		node.Summary = strings.TrimSpace(node.Summary + "\nSuperseded: " + reason)
	}
	runes := []rune(node.Summary)
	if len(runes) > 600 {
		node.Summary = string(runes[:600]) + "…"
	}
	titleRunes := []rune(strings.TrimSpace(node.Title))
	if len(titleRunes) > 160 {
		node.Title = string(titleRunes[:160]) + "…"
	}
	node.Dependencies = nil
	node.Supersedes = nil
	node.ArtifactRefs = nil
	node.FailureFingerprint = ""
	node.TraceRef = ""
	return node
}
