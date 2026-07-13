package tools

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/agent"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/plan"
	"agentgo/internal/store"
	"agentgo/internal/tools/schema"
)

// SchedulerGroup 注册 scheduler 一等代理专属的两个工具：
//   - cancel_task：取消一个未完成的任务
//   - report_done：向用户报告最终结果（含事实校对 + 提前调用拦截 + reactLoop 终止信号）
//
// 与 MetaGroup 的区别：
//   - MetaGroup 的 publish_task / send_message 是 worker / explorer / scheduler 共用的
//   - SchedulerGroup 的 cancel_task / report_done 仅 scheduler 注册（worker 不应该取消任务，
//     也没有 user 概念可以汇报）
//
// Phase 3 引入。从 internal/scheduler/scheduler.go 的 toolCancelTask / toolReportDone
// 迁移而来，行为字节级一致 + 加入 hook 系统接入（通过 NewLLMExecutor 自动获得）。
//
// Phase 3.1 新增 FinalizationNotifier 字段（原 DoneNotifier），让 reportDone 能通知 scheduler agent
// "终止当前 reactLoop"，避免幻觉心跳循环。
type SchedulerGroup struct {
	Store                store.TaskStore
	Holder               TaskHolder           // 提供"当前 scheduler task 的 ID"，report_done 用于读 SchedulerBatch
	MBRegistry           *mailbox.Registry    // 当前未使用，留作未来扩展（例如 report_done 时通知其他代理）
	FinalizationNotifier FinalizationNotifier // 可选；非 nil 时 reportDone 成功后调 MarkTaskFinalized()
	ProjectRoot          string               // 项目根目录，供 probe_directory 做路径校验
	UserOutput           io.Writer            // 用户可见内容的输出目标；nil 时回退到 stdout
	PlanCoordinator      *plan.Coordinator
}

// Register 把 cancel_task / report_done 注册到 r。
// Store / Holder 缺失时跳过对应工具。
func (g SchedulerGroup) Register(r *agent.ToolRegistry) {
	if g.Store == nil {
		return
	}

	r.Register(
		"cancel_task",
		"取消一个尚未完成的任务（pending 或 processing 状态）",
		schema.Object().
			String("task_id", "要取消的任务 ID", true).
			String("reason", "取消原因（用于日志和审计）", false).
			Build(),
		g.cancelTask,
	)

	if g.Holder != nil {
		r.Register(
			"report_done",
			"向用户报告最终结果，表示当前请求处理完毕。"+
				"调用前会校验 SchedulerBatch；若已进入 DAG 或直接执行过写入/命令，还会要求 Plan 已正式终结；"+
				"调用后会清空 SchedulerBatch 并打印事实校对块（task.Artifacts）。",
			schema.Object().
				String("summary", "给用户的最终汇总报告", true).
				Build(),
			g.reportDone,
		)

		r.Register(
			"report_progress",
			"向用户汇报中间进度，不终止当前 reactLoop。"+
				"当 board snapshot 显示还有 pending_downstream_tasks 时使用，"+
				"让用户知道系统正在工作，降低焦虑感。"+
				"调用后 reactLoop 会继续，等下游任务完成后再汇报最终结果。",
			schema.Object().
				String("summary", "给用户的进度汇报摘要", true).
				Build(),
			g.reportProgress,
		)
	}

	r.Register(
		"probe_directory",
		"探测指定目录的完整结构，返回树状目录（含文件大小）、文件类型分布和统计综述。"+
			"比 list_dir 更强大但输出更多 token，用于任务规划前了解工作区全貌。",
		schema.Object().
			String("path", "要探测的目录路径（相对项目根），默认 '.'", false).
			Int("depth", "递归深度，默认 3，最大 10", false).
			Build(),
		g.probeDirectory,
	)
}

// cancelTask 是 cancel_task 工具的实现。先尝试 pending→cancelled，
// 失败时尝试 processing→cancelled。两种 transition 都失败则返回错误。
func (g SchedulerGroup) cancelTask(ctx context.Context, args map[string]any) (string, error) {
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return "", fmt.Errorf("缺少 task_id 参数")
	}
	reason, _ := args["reason"].(string)
	var currentPlan *model.Plan
	var currentControllerID string
	if g.PlanCoordinator != nil && g.Holder != nil {
		currentID := g.Holder.Get()
		currentTask, currentErr := g.Store.GetTask(currentID)
		if currentErr != nil {
			return "", fmt.Errorf("读取当前 scheduler 任务失败: %w", currentErr)
		}
		if currentTask.PlanID != "" {
			currentControllerID = currentTask.ID
			currentPlan, currentErr = g.PlanCoordinator.Store().GetPlan(currentTask.PlanID)
			if currentErr != nil {
				return "", fmt.Errorf("读取当前 Plan 失败: %w", currentErr)
			}
			if currentErr = validateActiveController(currentTask, currentPlan); currentErr != nil {
				return "", currentErr
			}
			if currentPlan.Status != model.PlanStatusRunning {
				return "", fmt.Errorf("cancel_task 被拒绝：Plan %s 当前为 %s", currentPlan.ID, currentPlan.Status)
			}
		}
	}
	target, targetErr := g.Store.GetTask(taskID)
	if targetErr != nil {
		return "", fmt.Errorf("读取待取消任务失败 (id=%s): %w", taskID, targetErr)
	}
	if currentPlan != nil && target.PlanID != currentPlan.ID {
		return "", fmt.Errorf("cancel_task 被拒绝：任务 %s 不属于当前 Plan %s", taskID, currentPlan.ID)
	}

	cancel := func() error {
		// Re-read membership inside the controller lease. A controller switch
		// cannot interleave between this check and the TaskStore transition.
		latest, latestErr := g.Store.GetTask(taskID)
		if latestErr != nil {
			return latestErr
		}
		if currentPlan != nil && latest.PlanID != currentPlan.ID {
			return fmt.Errorf("任务 %s 不属于当前 Plan %s", taskID, currentPlan.ID)
		}
		err := store.TransitionStateWithCancelSource(g.Store, taskID, model.TaskStatusPending, model.TaskStatusCancelled, "scheduler")
		if err != nil {
			err = store.TransitionStateWithCancelSource(g.Store, taskID, model.TaskStatusProcessing, model.TaskStatusCancelled, "scheduler")
		}
		return err
	}
	var err error
	if currentPlan != nil {
		err = g.PlanCoordinator.WithControllerLease(ctx, currentPlan.ID, currentControllerID, cancel)
	} else {
		err = cancel()
	}
	if err != nil {
		return "", fmt.Errorf("取消任务失败 (id=%s): %w", taskID, err)
	}
	return fmt.Sprintf("任务已取消: id=%s, 原因: %s", taskID, reason), nil
}

// reportDone 是 report_done 工具的实现。包含三段逻辑：
//
//  1. **硬性提前拦截**：从 holder 拿到当前 scheduler task ID，读 task.SchedulerBatch，
//     扫描每个子任务的状态。任一未到终态（completed/failed/cancelled）→ 拒绝调用，
//     返回 LLM 可读的错误消息（这与旧 Scheduler.toolReportDone 的硬拦截一致）。
//
//  2. **事实校对**：从 task.Artifacts 直接构造一段"实际写入文件清单"，与 LLM 的 summary
//     并列打印到 stdout。LLM 即使在 summary 里编造产物，用户也能从事实校对块看出矛盾。
//     这是修复历史问题"Scheduler report_done 不基于 task.Artifacts 真实清单"
//     的关键路径，从 internal/scheduler/scheduler.go::buildArtifactsReport 迁移而来。
//
//  3. **清空 batch**：调 store.ClearSchedulerBatch 让下一轮 reactLoop 看到干净状态。
func (g SchedulerGroup) reportDone(ctx context.Context, args map[string]any) (string, error) {
	summary, _ := args["summary"].(string)

	// 拿到当前 scheduler task ID（由 holder 闭包提供，scheduler agent 在 OnTaskStart 设置）
	currentID := g.Holder.Get()
	if currentID == "" {
		return "", fmt.Errorf("无法获取当前 scheduler 任务上下文")
	}

	currentTask, err := g.Store.GetTask(currentID)
	if err != nil {
		return "", fmt.Errorf("读取当前 scheduler 任务失败: %w", err)
	}
	batch := currentTask.SchedulerBatch

	// Check live work before changing the Plan terminal state. Otherwise an
	// empty compatibility Plan with a pending legacy batch could be closed and
	// only then have report_done rejected.
	var pendingTasks []string
	for _, id := range batch {
		task, err := g.Store.GetTask(id)
		if err != nil {
			// 任务被淘汰或不存在，跳过（不阻止 report_done）
			continue
		}
		if !model.IsTerminal(task.Status) {
			short := id
			if len(short) >= 8 {
				short = short[:8]
			}
			pendingTasks = append(pendingTasks, fmt.Sprintf("%s(%s)", short, task.Status))
		}
	}
	if len(pendingTasks) > 0 {
		return "", fmt.Errorf(
			"report_done 被拒绝：以下任务尚未完成: %s。请等待所有任务到达终态后再调用 report_done",
			strings.Join(pendingTasks, ", "),
		)
	}

	if g.PlanCoordinator != nil && currentTask.PlanID != "" {
		p, planErr := g.PlanCoordinator.Store().GetPlan(currentTask.PlanID)
		if planErr != nil {
			return "", fmt.Errorf("读取当前 Plan 失败: %w", planErr)
		}
		if authorityErr := validateActiveController(currentTask, p); authorityErr != nil {
			return "", authorityErr
		}
		if !model.IsPlanTerminal(p.Status) {
			if p.Status != model.PlanStatusRunning || reportNeedsFormalFinalization(g.Store, currentTask, p) {
				return "", fmt.Errorf("report_done 被拒绝：Plan %s 尚未依据最新正式验收进入终态（status=%s）", p.ID, p.Status)
			}
			authorityCtx := plan.WithControllerAuthority(ctx, currentTask.ID)
			if _, completeErr := g.PlanCoordinator.CompleteWithoutExecution(authorityCtx, p.ID); completeErr != nil {
				return "", fmt.Errorf("结束只读 Plan 失败: %w", completeErr)
			}
		}
	}

	// 2. 事实校对：构造 artifacts 报告
	artifactsReport := buildSchedulerArtifactsReport(g.Store, batch)

	// 3. 长内容自动落盘，TUI 只显示摘要
	displaySummary := summary
	reportPath := ""
	const maxTerminalLines = 30
	summaryLines := strings.Split(summary, "\n")
	if len(summaryLines) > maxTerminalLines {
		reportPath = filepath.Join(g.ProjectRoot, ".agentgo", "reports", fmt.Sprintf("report-%s.md", time.Now().Format("20060102-150405")))
		if err := os.MkdirAll(filepath.Dir(reportPath), 0755); err == nil {
			if err := os.WriteFile(reportPath, []byte(summary), 0644); err == nil {
				displaySummary = fmt.Sprintf(
					"📄 完整报告已保存至: %s\n\n📋 摘要（前 %d 行）:\n%s\n\n[... %d 行省略，见上方文件]",
					reportPath, maxTerminalLines,
					strings.Join(summaryLines[:maxTerminalLines], "\n"),
					len(summaryLines)-maxTerminalLines,
				)
			}
		}
	}

	// 4. 输出到用户可见终端
	if g.UserOutput != nil {
		fmt.Fprintf(g.UserOutput, "\n=== 任务完成 ===\n%s\n%s================\n\n", displaySummary, artifactsReport)
	} else {
		log.Printf("=== 任务完成 ===\n%s\n%s================", displaySummary, artifactsReport)
	}

	// 3. 清空 batch（让下一轮 reactLoop 看到干净状态）
	if err := g.Store.ClearSchedulerBatch(currentID); err != nil {
		// 清空失败仅记日志，不影响"已汇报"的语义
		if g.UserOutput != nil {
			fmt.Fprintf(g.UserOutput, "[scheduler-group] ClearSchedulerBatch 失败 (task=%s): %v\n", currentID, err)
		} else {
			log.Printf("[scheduler-group] ClearSchedulerBatch 失败 (task=%s): %v", currentID, err)
		}
	}

	// 4. 通知 scheduler agent "当前 reactLoop 已完成报告"，让下一轮 Execute 短路终止。
	//    这是修复"report_done 后 reactLoop 不终止 → LLM 幻觉心跳"的核心信号。
	//    FinalizationNotifier 为 nil 时跳过（向后兼容 + worker 测试场景）。
	if g.FinalizationNotifier != nil {
		g.FinalizationNotifier.MarkTaskFinalized()
	}

	return "已向用户报告完成", nil
}

func validateActiveController(task *model.Task, p *model.Plan) error {
	if task == nil || p == nil || task.NodeRole != model.PlanNodeRoleController ||
		task.EventType != "__scheduler__" || task.ID != p.ActiveDecisionTaskID {
		taskID := ""
		if task != nil {
			taskID = task.ID
		}
		planID := ""
		activeID := ""
		if p != nil {
			planID = p.ID
			activeID = p.ActiveDecisionTaskID
		}
		return fmt.Errorf("scheduler control operation requires active controller %s for plan %s (caller=%s)", activeID, planID, taskID)
	}
	return nil
}

func reportNeedsFormalFinalization(s store.TaskStore, task *model.Task, p *model.Plan) bool {
	if p != nil && len(p.CurrentNodeIDs) > 0 {
		return true
	}
	if task == nil {
		return false
	}
	if len(task.Artifacts) > 0 {
		return true
	}
	for _, toolName := range []string{"write_file", "edit_file", "run_shell"} {
		records, err := s.QueryToolCalls(task.ID, toolName)
		if err != nil {
			continue
		}
		for _, record := range records {
			if record.Success {
				return true
			}
		}
	}
	return false
}

// reportProgress 是 report_progress 工具的实现。
// 向用户输出中间进度摘要，但不调用 FinalizationNotifier，
// 让 reactLoop 继续执行，等下游任务完成后再汇报最终结果。
func (g SchedulerGroup) reportProgress(ctx context.Context, args map[string]any) (string, error) {
	summary, _ := args["summary"].(string)
	if summary == "" {
		return "", fmt.Errorf("summary 不能为空")
	}

	// 输出到用户可见终端
	if g.UserOutput != nil {
		fmt.Fprintf(g.UserOutput, "\n📊 任务进度更新\n%s\n\n", summary)
	} else {
		log.Printf("📊 任务进度更新\n%s", summary)
	}

	// 关键：不调用 FinalizationNotifier.MarkTaskFinalized()
	// 返回成功，让 reactLoop 继续下一轮
	return "进度已记录，继续处理中...", nil
}

// buildSchedulerArtifactsReport 扫描指定任务列表，从 task.Artifacts 构造一段
// "系统校验"文本块，附加到 report_done 输出末尾。
//
// 这是 LLM 自由发挥的硬约束兜底——LLM 生成的 summary 可能编造不存在的产物，
// 但本函数只读 task.Artifacts（由 RecordArtifactHook 在 write_file/edit_file
// 成功后硬连线追加），任何由 LLM 编造的文件都不会出现在这里；任何 LLM 没提
// 的真实文件也会被列出。
//
// 单个任务 GetTask 失败不影响整体输出，只在该行打印错误标记。
//
// 从 internal/scheduler/scheduler.go::buildArtifactsReport 迁移而来。
func buildSchedulerArtifactsReport(s store.TaskStore, taskIDs []string) string {
	if len(taskIDs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("=== 实际产出（系统校验，来自 task.Artifacts）===\n")
	for _, id := range taskIDs {
		shortID := id
		if len(shortID) >= 8 {
			shortID = shortID[:8]
		}
		task, err := s.GetTask(id)
		if err != nil || task == nil {
			fmt.Fprintf(&b, "任务 %s: <读取失败: %v>\n", shortID, err)
			continue
		}
		fmt.Fprintf(&b, "任务 %s [%s]:\n", shortID, task.Status)
		if len(task.Artifacts) == 0 {
			b.WriteString("  └─ 无文件产出\n")
		} else {
			for _, p := range task.Artifacts {
				fmt.Fprintf(&b, "  └─ %s\n", p)
			}
		}
	}
	return b.String()
}
