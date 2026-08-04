package bootstrap

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/ui"
)

// requestDescMaxRunes 是取消摘要里请求描述的最大字符数，超出部分以 "…" 收尾。
const requestDescMaxRunes = 20

// cancelLatestActiveRequest 取消"最新创建的一棵请求树"（Esc 取消的后端）。
//
// 一棵请求树 = 一个非终态的 EventType="__scheduler__" 任务（用户输入或
// replan/graph-change 唤醒经公告板发布的 scheduler 任务）加上它的全部下游。
// C6b 起 Plan 控制面已删除，请求树归组只依赖任务谱系事实：
//   - 根任务本身 + SchedulerBatch 显式跟踪的非终态子任务；
//   - ParentTaskID 谱系上的全部非终态后代（publish_task 发布时记录父边）；
//   - 级联：对已被取消的任务集合做 BFS，把 Dependencies 指向已取消任务的
//     非终态任务一并取消（语义与 watchdog 的级联取消一致，来源记为
//     "dependency_failure"）。
//
// 幂等：刚被取消的树整体进入终态，重复调用自然落到下一棵非终态请求树；
// 全部请求树都已终态时返回 ui.ErrNoActiveRequest。
//
// 返回一行中文摘要（请求描述截断 + 取消任务数），供 UI 消息流直接展示。
func cancelLatestActiveRequest(ctx context.Context, s store.TaskStore) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tasks, err := s.ScanAll()
	if err != nil {
		return "", fmt.Errorf("读取任务列表失败: %w", err)
	}
	byID := make(map[string]*model.Task, len(tasks))
	var roots []*model.Task
	for _, t := range tasks {
		if t == nil {
			continue
		}
		byID[t.ID] = t
		if t.EventType == "__scheduler__" && !model.IsTerminal(t.Status) {
			roots = append(roots, t)
		}
	}
	if len(roots) == 0 {
		return "", ui.ErrNoActiveRequest
	}
	// 栈语义：最新创建的一棵优先；CreatedAt 打平（粗时间粒度）时以 ID
	// 字典序兜底，保证多次调用结果确定。
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].CreatedAt.Equal(roots[j].CreatedAt) {
			return roots[i].ID > roots[j].ID
		}
		return roots[i].CreatedAt.After(roots[j].CreatedAt)
	})

	cancelled := make(map[string]bool)
	var queue []string
	cancelOne := func(taskID, source string) {
		if cancelled[taskID] {
			return
		}
		if err := cancelTaskTwoPhase(s, taskID, source); err != nil {
			// 与 ScanAll 快照之间的竞争（任务已自行终态）不算失败，仅记录。
			log.Printf("[请求取消] 取消任务 %s 失败: %v", taskID, err)
			return
		}
		cancelled[taskID] = true
		queue = append(queue, taskID)
	}
	// 级联取消：BFS 把 Dependencies 指向已取消任务的非终态任务一并取消
	// （参照 watchdog 的级联语义）。
	cascade := func() {
		for len(queue) > 0 {
			head := queue[0]
			queue = queue[1:]
			for _, t := range tasks {
				if t == nil || cancelled[t.ID] || model.IsTerminal(t.Status) {
					continue
				}
				for _, depID := range t.Dependencies {
					if depID == head {
						cancelOne(t.ID, "dependency_failure")
						break
					}
				}
			}
		}
	}

	root := roots[0]
	cancelOne(root.ID, "user")
	// SchedulerBatch 是 scheduler 的显式子任务跟踪，作为第一批取消候选；
	// cancelOne 内部按 cancelled 集合去重。
	for _, childID := range root.SchedulerBatch {
		child, ok := byID[childID]
		if !ok || model.IsTerminal(child.Status) {
			continue
		}
		cancelOne(childID, "user")
	}
	// ParentTaskID 谱系后代：覆盖 batch 跟踪之外经谱系挂到本请求树的任务
	// （含 replan/graph-change 唤醒任务的下游、worker 自拆的子任务）。
	for _, t := range tasks {
		if t == nil || cancelled[t.ID] || model.IsTerminal(t.Status) {
			continue
		}
		for ancestor := t.ParentTaskID; ancestor != ""; {
			if ancestor == root.ID {
				cancelOne(t.ID, "user")
				break
			}
			parent, ok := byID[ancestor]
			if !ok {
				break
			}
			ancestor = parent.ParentTaskID
		}
	}

	// 级联取消：BFS 把 Dependencies 指向已取消任务的非终态任务一并取消。
	cascade()

	desc := truncateRunes(root.Description, requestDescMaxRunes)
	if desc == "" {
		desc = root.ID
	}
	return fmt.Sprintf("已取消请求「%s」：共取消 %d 个任务", desc, len(cancelled)), nil
}

// cancelTaskTwoPhase 两段式取消：先试 pending→cancelled，失败后试
// processing→cancelled（与 GuardedCancel 的转换写法一致）。
func cancelTaskTwoPhase(s store.TaskStore, taskID, source string) error {
	err := store.TransitionStateWithCancelSource(s, taskID, model.TaskStatusPending, model.TaskStatusCancelled, source)
	if err != nil {
		err = store.TransitionStateWithCancelSource(s, taskID, model.TaskStatusProcessing, model.TaskStatusCancelled, source)
	}
	return err
}

// truncateRunes 把 s 截断到最多 n 个字符（rune），超长时以 "…" 收尾。
func truncateRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
