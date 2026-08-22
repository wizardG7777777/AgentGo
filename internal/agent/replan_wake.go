package agent

// replan_wake.go 实现「通用 replan 唤醒任务」的 agent 侧发布口（V6 C6b）。
//
// 机制与 request_replan 工具的非图路径（internal/tools/plan_control.go）
// 同款：任务进入异常终态（workspace 合并冲突 / runtime loop fuse）需要
// Scheduler 裁决后续编排时，不触碰任何服务端控制面状态，只向公告板发布
// 一条 __scheduler__ 唤醒任务（描述首行含幂等标记与请求者上下文），
// Scheduler 认领后自行裁决。原 Plan 控制面的 RequestReplan 登记通道已随
// C6b 删除，本 helper 是其唯一替代机制。

import (
	"fmt"
	"log"
	"strings"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/runcontract"
	"agentgo/internal/taskcontract"
)

// replanRequestMarker 是通用 replan 唤醒任务描述中的幂等标记；查重按该
// 子串匹配。幂等键为 <taskID>/replan：同一任务的重复唤醒只保留一个唤醒
// 任务。与 internal/tools/plan_control.go 的 replanRequestMarker 同款契约。
func replanRequestMarker(taskID string) string {
	return "[replan-request: " + taskID + "/replan]"
}

// publishReplanWakeTask 发布一条 __scheduler__ replan 唤醒任务：
//
//   - 图任务（task.GraphID 非空）跳过——终态由 graph-terminal-feed 回填
//     引擎按边条件路由，无需唤醒 Scheduler；
//   - 幂等：发布前 ScanAll 查重，同一任务已有未处理（非终态）的同类唤醒
//     任务（描述含幂等标记）时跳过发布，不重复唤醒；
//   - 唤醒任务刻意不携带 GraphID/NodeID/ActivationID：它是 Scheduler 的
//     控制面输入而非图节点任务，带图身份会被 graph-terminal-feed 当作
//     节点终态回填引擎；
//   - 失败语义：返回 error 由调用方决定如何呈现（fuse / 合并冲突路径只记
//     日志，不推翻主流程；结构化 blocked 收尾路径额外 emit error 事件）。
func (a *Agent) publishReplanWakeTask(task *model.Task, taskID, reasonCode, detail string) error {
	if a.Store == nil || task == nil || task.GraphID != "" {
		return nil
	}
	marker := replanRequestMarker(taskID)

	// 幂等查重：同一任务的未处理同类唤醒只保留一个。
	// MemoryTaskStore.ScanAll 永不返回错误；其它实现扫描失败时退化为直接
	// 发布（多一个唤醒任务无害，Scheduler 裁决天然幂等）。
	if tasks, err := a.Store.ScanAll(); err == nil {
		for _, t := range tasks {
			if t == nil || t.EventType != "__scheduler__" || model.IsTerminal(t.Status) {
				continue
			}
			if strings.Contains(t.Description, marker) {
				log.Printf("[agent %s] 任务 %s 已有待处理的 replan 唤醒任务（reason=%s），跳过重复发布", a.ID, taskID, reasonCode)
				return nil
			}
		}
	}

	// 审计事实由唤醒任务自身的 task_published 事件承担（描述含幂等标记、
	// reason_code 与 detail），C6c 起不再单独 emit replan 时代事件。
	description := fmt.Sprintf(
		"%s\n任务 %s（event_type=%s）请求 replan。\nreason_code=%s\n详情：%s\n"+
			"处理指引：读取公告板当前任务状态，裁决是否需要补充/调整后续任务编排；判断无需调整时直接结束本任务。",
		marker, taskID, task.EventType, reasonCode, detail)
	wake := &model.Task{
		Description:    description,
		EventType:      "__scheduler__",
		EventSource:    "replan-request",
		ParentTaskID:   task.ID,
		MaxConcurrency: 1, // 同一时刻只允许一个 Scheduler 处理同一请求
	}
	if err := taskcontract.Inherit(task, wake, loopcontract.WorkCoordination); err != nil {
		return fmt.Errorf("继承 replan RunContract: %w", err)
	}
	if wake.RunContract != nil {
		wake.RunPhase = runcontract.PhaseRecovery
	}
	if err := a.Store.PublishTask(wake); err != nil {
		log.Printf("[agent %s] 任务 %s 发布 replan 唤醒任务失败（任务本身终态不受影响）: %v", a.ID, taskID, err)
		return err
	}
	log.Printf("[agent %s] 任务 %s 已发布 replan 唤醒任务 %s（reason=%s），交 Scheduler 裁决后续编排", a.ID, taskID, wake.ID, reasonCode)
	return nil
}
