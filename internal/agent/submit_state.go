package agent

import (
	"strings"
	"sync"
)

// StructuredSubmission 是 submit_task_result 工具写入的一次结构化提交。
// 字段全部由 LLM 在工具参数中自述；框架只做校验与渲染，不改写内容。
type StructuredSubmission struct {
	TaskID          string
	Summary         string   // 一两句话的结果概括；同时作为下游可见的 TransferNote
	ChecksPerformed []string // 已执行的验证（如 go build / go test ./...）
	Evidence        []string // 支撑证据（文件路径、命令输出要点等）
	RemainingRisks  []string // 残余风险
	BlockedReason   string   // 无法完成时的阻塞原因；非空时工具层会登记高优 ReplanRequest
	RequestReplan   bool     // 提交者请求 Scheduler 重新评估当前 Plan
}

// Format 把结构化提交渲染为 markdown 结果块：summary 在前，
// 其余分节（已执行的检查 / 证据 / 残余风险 / 阻塞原因 / 重规划请求）随后，空节省略。
// 渲染文本在 finalization 短路分支替代 LLM 自由文本，成为 SubmitResult / LastResponse 的权威负载。
func (s *StructuredSubmission) Format() string {
	var sb strings.Builder
	sb.WriteString("## 任务结果摘要\n\n")
	sb.WriteString(s.Summary)
	writeSection := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		sb.WriteString("\n\n## " + title + "\n")
		for _, item := range items {
			sb.WriteString("\n- " + item)
		}
	}
	writeSection("已执行的检查", s.ChecksPerformed)
	writeSection("证据", s.Evidence)
	writeSection("残余风险", s.RemainingRisks)
	if s.BlockedReason != "" {
		sb.WriteString("\n\n## 阻塞原因\n\n" + s.BlockedReason)
	}
	if s.RequestReplan {
		sb.WriteString("\n\n## 重规划请求\n\n提交者请求 Scheduler 重新评估当前 Plan。")
	}
	return sb.String()
}

// SubmitState 线程安全地暂存"已校验、待 ReAct 循环消费的"结构化提交。
//
// 生命周期：
//   - submit_task_result 工具校验通过后 Put 一份提交，并标记任务 finalized
//   - agent.Run 在 finalization 短路分支 Take（即取即删），渲染后作为权威结果提交
//   - Take 未命中时短路分支退回 report_done 兼容路径（lastOutput），行为与旧版一致
type SubmitState struct {
	mu   sync.Mutex
	subs map[string]*StructuredSubmission
}

// NewSubmitState 创建一个空的 SubmitState。
func NewSubmitState() *SubmitState {
	return &SubmitState{subs: make(map[string]*StructuredSubmission)}
}

// Put 暂存一份结构化提交；同任务重复 Put 以最新一次为准（重试场景）。
func (s *SubmitState) Put(sub *StructuredSubmission) {
	if sub == nil || sub.TaskID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[sub.TaskID] = sub
}

// Take 取出并删除指定任务的结构化提交；没有暂存时返回 (nil, false)。
func (s *SubmitState) Take(taskID string) (*StructuredSubmission, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[taskID]
	if ok {
		delete(s.subs, taskID)
	}
	return sub, ok
}
