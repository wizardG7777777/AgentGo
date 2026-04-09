package scheduler

import (
	"encoding/json"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// boardSnapshot 是 scheduler agent 在每轮 reactLoop 看到的全局任务板 JSON 结构。
// 包含模式、触发事件、任务列表、资源（worker 可用数）四段。
//
// 从 internal/scheduler/scheduler.go 的私有 boardSnapshot 类型迁移而来，
// 保持字段顺序与 JSON tag 完全一致以确保 LLM 看到的 schema 不变。
type boardSnapshot struct {
	Mode      string         `json:"mode"`
	Trigger   triggerInfo    `json:"trigger"`
	Tasks     []taskSnapshot `json:"tasks"`
	Resources resourceInfo   `json:"resources"`
}

type resourceInfo struct {
	WorkerCount      int `json:"worker_count"`
	BusyWorkers      int `json:"busy_workers"`
	AvailableWorkers int `json:"available_workers"`
}

type triggerInfo struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id,omitempty"`
	Text   string `json:"text,omitempty"`
}

type taskSnapshot struct {
	ID            string            `json:"id"`
	Description   string            `json:"description"`
	Status        string            `json:"status"`
	EventType     string            `json:"event_type,omitempty"`
	Results       map[string]string `json:"results,omitempty"`
	Error         string            `json:"error,omitempty"`
	Dependencies  []string          `json:"dependencies,omitempty"`
	PartialOutput string            `json:"partial_output,omitempty"`
	Artifacts     []string          `json:"artifacts,omitempty"`
	LastResponse  string            `json:"last_response,omitempty"`
}

// BuildBoardJSON 构造 scheduler agent 在每轮 reactLoop 注入到 history 的 board 快照 JSON。
//
// 参数：
//   - s: TaskStore，用于 ScanAll
//   - cfg: 配置（读取 WorkerCount 计算可用 worker 数）
//   - mode: "immediate" 或 "plan"，由调用方决定（旧 scheduler 通过 Scheduler.GetMode 拿）
//   - trigger: 当前 reactLoop 的触发事件（用户输入、task completed 等）
//
// 设计原则：
//   - 自包含 helper 函数，不依赖 *Scheduler 或 *agent.Agent，方便单测
//   - 字段顺序与 JSON tag 与旧 scheduler.go::buildBoardJSON 完全一致
//   - 已完成任务展开 Results；失败/重试中任务展开 LastResponse；processing 任务展开 PartialOutput
//
// 从 internal/scheduler/scheduler.go::Scheduler.buildBoardJSON 迁移而来。
func BuildBoardJSON(s store.TaskStore, cfg *config.Config, mode string, trigger model.Event) string {
	tasks, _ := s.ScanAll()

	ti := triggerInfo{Type: string(trigger.Type), TaskID: trigger.TaskID}
	if trigger.Payload != nil {
		ti.Text = trigger.Payload["text"]
	}

	var taskSnaps []taskSnapshot
	for _, t := range tasks {
		snap := taskSnapshot{
			ID:          t.ID,
			Description: t.Description,
			Status:      string(t.Status),
			EventType:   t.EventType,
		}
		if model.IsTerminal(t.Status) && len(t.Results) > 0 {
			snap.Results = t.Results
		}
		if t.Error != "" {
			snap.Error = t.Error
		}
		if len(t.Dependencies) > 0 {
			snap.Dependencies = t.Dependencies
		}
		if t.Status == model.TaskStatusProcessing && t.PartialOutput != "" {
			snap.PartialOutput = t.PartialOutput
		}
		if len(t.Artifacts) > 0 {
			snap.Artifacts = t.Artifacts
		}
		// 失败/重试中的任务展开 LastResponse；已 completed 的任务用 Results
		if t.LastResponse != "" && t.Status != model.TaskStatusCompleted {
			snap.LastResponse = t.LastResponse
		}
		taskSnaps = append(taskSnaps, snap)
	}

	busyWorkers := 0
	for _, t := range tasks {
		if t.Status == model.TaskStatusProcessing && t.EventType == "" {
			busyWorkers += len(t.Agents)
		}
	}
	workerCount := cfg.WorkerCount
	if workerCount <= 0 {
		workerCount = 1
	}
	available := workerCount - busyWorkers
	if available < 0 {
		available = 0
	}

	bs := boardSnapshot{
		Mode:    mode,
		Trigger: ti,
		Tasks:   taskSnaps,
		Resources: resourceInfo{
			WorkerCount:      workerCount,
			BusyWorkers:      busyWorkers,
			AvailableWorkers: available,
		},
	}
	data, _ := json.MarshalIndent(bs, "", "  ")
	return string(data)
}
