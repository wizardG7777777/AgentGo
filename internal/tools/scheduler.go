package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agentgo/internal/agent"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
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
	UserOutput           io.Writer            // 用户可见普通输出（进度汇报等）；nil 时回退到 stdout
	// ResultOutput 是任务最终结果块（report_done 的 "=== 任务完成 ===" 块）的输出目标；
	// nil 时回退到 UserOutput。bootstrap 将其接到 output.KindResult 事件 writer，
	// 让结果分类在产生处完成，消费方不再做子串匹配。
	ResultOutput io.Writer
}

const (
	defaultTaskResultPageRunes = 4000
	maxTaskResultPageRunes     = 8000
)

type taskResultPage struct {
	TaskID        string `json:"task_id"`
	AgentID       string `json:"agent_id"`
	Status        string `json:"status"`
	Offset        int    `json:"offset"`
	LimitApplied  int    `json:"limit_applied"`
	NextOffset    int    `json:"next_offset"`
	Complete      bool   `json:"complete"`
	OriginalBytes int    `json:"original_bytes"`
	OriginalRunes int    `json:"original_runes"`
	SHA256        string `json:"sha256"`
	Content       string `json:"content"`
}

// Register 把 Scheduler 控制工具注册到 r。
// Store / Holder 缺失时跳过对应工具。
func (g SchedulerGroup) Register(r *agent.ToolRegistry) {
	if g.Store == nil {
		return
	}

	r.Register(
		"cancel_task",
		"取消一个尚未完成的任务（pending 或 processing 状态）；Graph controller 只能取消同一 Graph 内的任务",
		schema.Object().
			String("task_id", "要取消的任务 ID", true).
			String("reason", "取消原因（用于日志和审计）", false).
			Build(),
		g.cancelTask,
	)

	if g.Holder != nil {
		r.Register(
			"get_task_result",
			"按需分页读取当前 Graph 或 legacy request scope 内 board result_refs 指向的完整任务结果。只在 excerpt 不足以支持当前决策时调用；不要机械读取所有任务。",
			schema.Object().
				String("task_id", "result_refs 所属任务 ID", true).
				String("agent_id", "结果生产者 ID；任务只有一份结果时可省略", false).
				Int("offset", "Unicode rune 偏移，0-based；默认 0", false).
				Int("limit", "本页最多返回的 Unicode rune 数；默认 4000，服务端最大 8000", false).
				Build(),
			g.getTaskResult,
		)

		r.Register(
			"report_done",
			"向用户报告最终结果，表示当前请求处理完毕。"+
				"Graph task 不可调用；图到达 end 后由 graph_ended 终态通知统一汇报。"+
				"调用前会校验 SchedulerBatch；"+
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

func (g SchedulerGroup) getTaskResult(_ context.Context, args map[string]any) (string, error) {
	if g.Holder == nil {
		return "", fmt.Errorf("get_task_result requires scheduler task context")
	}
	currentID := strings.TrimSpace(g.Holder.Get())
	if currentID == "" {
		return "", fmt.Errorf("get_task_result requires a current scheduler task")
	}
	current, err := g.Store.GetTask(currentID)
	if err != nil {
		return "", fmt.Errorf("读取当前 scheduler 任务失败: %w", err)
	}
	if err := validateTaskResultCaller(current); err != nil {
		return "", err
	}

	taskID, _ := args["task_id"].(string)
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return "", fmt.Errorf("缺少 task_id 参数")
	}
	target, err := g.Store.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("读取任务结果失败 (id=%s): %w", taskID, err)
	}
	if err := g.authorizeTaskResultRead(current, target); err != nil {
		return "", err
	}
	if !model.IsTerminal(target.Status) {
		return "", fmt.Errorf("任务 %s 当前状态为 %s；Results 仅在终态后提供稳定分页读取", taskID, target.Status)
	}
	if len(target.Results) == 0 {
		return "", fmt.Errorf("任务 %s 当前没有可读取的 Results", taskID)
	}

	agentIDs := make([]string, 0, len(target.Results))
	for id := range target.Results {
		if id == agent.StructuredResultStorageKey {
			continue // Graph bridge 内部 carrier，不作为可分页的 Agent 正文暴露。
		}
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)
	if len(agentIDs) == 0 {
		return "", fmt.Errorf("任务 %s 当前没有可读取的 Agent 结果正文", taskID)
	}
	agentID, _ := args["agent_id"].(string)
	agentID = strings.TrimSpace(agentID)
	if agentID == agent.StructuredResultStorageKey {
		return "", fmt.Errorf("agent_id=%s 是系统内部结构化结果 carrier，不可直接读取", agentID)
	}
	if agentID == "" {
		if len(agentIDs) != 1 {
			return "", fmt.Errorf("任务 %s 有多份结果，请指定 agent_id（可选值: %s）", taskID, strings.Join(agentIDs, ", "))
		}
		agentID = agentIDs[0]
	}
	result, ok := target.Results[agentID]
	if !ok {
		return "", fmt.Errorf("任务 %s 没有 agent_id=%s 的结果（可选值: %s）", taskID, agentID, strings.Join(agentIDs, ", "))
	}

	offset := 0
	if value, present := toInt(args["offset"]); present {
		offset = value
	}
	if offset < 0 {
		return "", fmt.Errorf("offset 不能为负数: %d", offset)
	}
	limit := defaultTaskResultPageRunes
	if value, present := toInt(args["limit"]); present {
		if value <= 0 {
			return "", fmt.Errorf("limit 必须 > 0: %d", value)
		}
		limit = min(value, maxTaskResultPageRunes)
	}

	runes := []rune(result)
	if offset > len(runes) {
		return "", fmt.Errorf("offset %d 超出结果总 rune 数 %d", offset, len(runes))
	}
	next := min(offset+limit, len(runes))
	page := taskResultPage{
		TaskID: taskID, AgentID: agentID, Status: string(target.Status),
		Offset: offset, LimitApplied: limit, NextOffset: next, Complete: next == len(runes),
		OriginalBytes: len(result), OriginalRunes: utf8.RuneCountInString(result),
		SHA256: computeSHA256([]byte(result)), Content: string(runes[offset:next]),
	}
	data, err := json.Marshal(page)
	if err != nil {
		return "", fmt.Errorf("序列化任务结果失败: %w", err)
	}
	return string(data), nil
}

// authorizeTaskResultRead keeps Graph and legacy result scopes disjoint:
//   - a Scheduler Graph node may read only tasks with the exact same non-empty
//     GraphID;
//   - a legacy Scheduler root may read only tasks inside its own SchedulerBatch /
//     ParentTaskID lineage.
//
// Graph ownership must not be inferred from ParentTaskID or any routing label:
// GraphID is the durable execution-contract identity.
func (g SchedulerGroup) authorizeTaskResultRead(current, target *model.Task) error {
	if err := validateTaskResultCaller(current); err != nil {
		return err
	}
	if current.GraphID != "" {
		if target.GraphID != "" && target.GraphID == current.GraphID {
			return nil
		}
		return fmt.Errorf("get_task_result 被拒绝：任务 %s 不属于当前 Graph %s", target.ID, current.GraphID)
	}
	if current.FinalReportGraphID != "" {
		if target.GraphID == current.FinalReportGraphID {
			return nil
		}
		return fmt.Errorf("get_task_result 被拒绝：任务 %s 不属于 final-report Graph %s",
			target.ID, current.FinalReportGraphID)
	}
	if current.InterventionGraphID != "" {
		if target.GraphID == current.InterventionGraphID &&
			(current.InterventionNodeID == "" || target.NodeID == current.InterventionNodeID) &&
			(current.InterventionActivationID == "" || target.ActivationID == current.InterventionActivationID) {
			return nil
		}
		return fmt.Errorf("get_task_result 被拒绝：任务 %s 不属于 intervention scope %s/%s/%s",
			target.ID, current.InterventionGraphID, current.InterventionNodeID, current.InterventionActivationID)
	}
	if target.GraphID != "" {
		return fmt.Errorf("get_task_result 被拒绝：Graph 任务 %s 不属于当前 legacy Scheduler scope", target.ID)
	}
	tasks, err := g.Store.ScanAll()
	if err != nil {
		return fmt.Errorf("读取 legacy 任务可见范围失败: %w", err)
	}
	visible := store.LegacyRequestTaskIDs(tasks, current.ID)
	if _, ok := visible[target.ID]; ok {
		return nil
	}
	return fmt.Errorf("get_task_result 被拒绝：任务 %s 不属于当前 Scheduler batch/lineage", target.ID)
}

func validateTaskResultCaller(current *model.Task) error {
	if current == nil || current.EventType != "__scheduler__" || current.Status != model.TaskStatusProcessing {
		currentID := ""
		if current != nil {
			currentID = current.ID
		}
		return fmt.Errorf("get_task_result 被拒绝：调用方 %s 不是正在执行的 Scheduler 任务", currentID)
	}
	if err := validateFinalReportScope(current); err != nil {
		return fmt.Errorf("get_task_result 被拒绝：%w", err)
	}
	return nil
}

func validateFinalReportScope(task *model.Task) error {
	if task == nil {
		return nil
	}
	finalSignal := task.EventSource == "graph-ended" || strings.TrimSpace(task.FinalReportGraphID) != "" ||
		task.RunPhase == runcontract.PhaseFinalization
	if !finalSignal {
		return nil
	}
	if task.EventSource == "graph-ended" && strings.TrimSpace(task.FinalReportGraphID) == "" {
		return fmt.Errorf("graph-ended final-report 缺少 final_report_graph_id")
	}
	scope, err := model.ClassifyControlScope(task)
	if err != nil || scope != model.ControlScopeFinalReport {
		return fmt.Errorf("final_report_graph_id=%q 与 task scope 不一致: %v", task.FinalReportGraphID, err)
	}
	return nil
}

// cancelTask 是 cancel_task 工具的 Scheduler 包装层。Graph controller 只能
// 取消 exact same GraphID 的任务；通过授权后，状态转换仍全部委托 GuardedCancel。
// TUI /cancel 直接调用 GuardedCancel，不受 Scheduler Graph scope 约束。
func (g SchedulerGroup) cancelTask(ctx context.Context, args map[string]any) (string, error) {
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return "", fmt.Errorf("缺少 task_id 参数")
	}
	if g.Holder != nil {
		currentID := strings.TrimSpace(g.Holder.Get())
		if currentID != "" {
			current, err := g.Store.GetTask(currentID)
			if err != nil {
				return "", fmt.Errorf("读取当前 scheduler 任务失败: %w", err)
			}
			if current.GraphID != "" {
				target, err := g.Store.GetTask(taskID)
				if err != nil {
					return "", fmt.Errorf("取消任务失败 (id=%s): %w", taskID, err)
				}
				if target.GraphID != current.GraphID {
					return "", fmt.Errorf("cancel_task 被拒绝：任务 %s 不属于当前 Graph %s", taskID, current.GraphID)
				}
			}
		}
	}
	reason, _ := args["reason"].(string)
	if err := GuardedCancel(ctx, g.Store, taskID, "scheduler"); err != nil {
		return "", err
	}
	return fmt.Sprintf("任务已取消: id=%s, 原因: %s", taskID, reason), nil
}

// GuardedCancel 是 cancel_task 工具与 TUI /cancel 共用的取消路径（D2 抽取）。
// 先尝试 pending→cancelled，失败时尝试 processing→cancelled，取消来源记为
// source（"scheduler" / "user"）。
//
// C6b 起旧 Plan 归属守卫（controller 租约 / membership 复查 / 外部调用方拦截）
// 已随其整包删除；Scheduler Graph scope 在 cancelTask 包装层单独校验，TUI
// 等直接调用方仍走不带 scope 的两段式转换。
//
// 错误消息保持「取消任务失败 (id=...): ...」措辞，调用方不应再包装。
func GuardedCancel(_ context.Context, s store.TaskStore, targetTaskID, source string) error {
	if _, err := s.GetTask(targetTaskID); err != nil {
		return fmt.Errorf("取消任务失败 (id=%s): %w", targetTaskID, err)
	}
	err := store.TransitionStateWithCancelSource(s, targetTaskID, model.TaskStatusPending, model.TaskStatusCancelled, source)
	if err != nil {
		err = store.TransitionStateWithCancelSource(s, targetTaskID, model.TaskStatusProcessing, model.TaskStatusCancelled, source)
	}
	if err != nil {
		return fmt.Errorf("取消任务失败 (id=%s): %w", targetTaskID, err)
	}
	return nil
}

// reportDone 是 report_done 工具的实现。包含四段逻辑：
//
//  1. **Graph 硬拒绝**：Graph task 不得直接向用户宣告整个请求完成；图到达 end
//     后由 graph_ended 终态通知统一汇报。
//
//  2. **硬性提前拦截**：从 holder 拿到当前 scheduler task ID，读 task.SchedulerBatch，
//     扫描每个子任务的状态。任一未到终态（completed/failed/cancelled）→ 拒绝调用，
//     返回 LLM 可读的错误消息（这与旧 Scheduler.toolReportDone 的硬拦截一致）。
//
//  3. **事实校对**：从 task.Artifacts 直接构造一段"实际写入文件清单"，与 LLM 的 summary
//     并列打印到 stdout。LLM 即使在 summary 里编造产物，用户也能从事实校对块看出矛盾。
//     这是修复历史问题"Scheduler report_done 不基于 task.Artifacts 真实清单"
//     的关键路径，从 internal/scheduler/scheduler.go::buildArtifactsReport 迁移而来。
//
//  4. **清空 batch**：调 store.ClearSchedulerBatch 让下一轮 reactLoop 看到干净状态。
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
	if err := validateFinalReportScope(currentTask); err != nil {
		return "", fmt.Errorf("report_done 被拒绝：%w", err)
	}
	if currentTask.GraphID != "" {
		return "", fmt.Errorf(
			"report_done 被拒绝：当前任务属于 Graph %s；Graph controller 请用 submit_task_result 提交当前节点结果（需要事件边时填写 event），再等待图到达 end 后由 graph_ended 终态通知统一汇报",
			currentTask.GraphID,
		)
	}
	batch := currentTask.SchedulerBatch

	// 硬拦截：batch 中还有未到终态的任务时拒绝 report_done，避免 Scheduler
	// 在下游仍在执行时提前向用户宣布完成。
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

	// 4. 输出到用户可见终端——结果块走 ResultOutput（KindResult 分类在产生处完成），
	//    未装配时回退 UserOutput（单 Writer 用法兼容）。
	resultOut := g.ResultOutput
	if resultOut == nil {
		resultOut = g.UserOutput
	}
	if resultOut != nil {
		fmt.Fprintf(resultOut, "\n=== 任务完成 ===\n%s\n%s================\n\n", displaySummary, artifactsReport)
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
// 但本函数只读 task.Artifacts（由 record-artifact Reactor 在文件写入事件后
// 硬连线追加），任何由 LLM 编造的文件都不会出现在这里；任何 LLM 没提
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
