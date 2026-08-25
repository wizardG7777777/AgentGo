package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// 结构化提交的自述终态（submit_task_result 的 status 参数）。
// failed / cancelled 只由系统路径产生，不接受 agent 自报。
const (
	// SubmitStatusCompleted 是缺省终态：任务以 completed 收尾（空串等价）。
	SubmitStatusCompleted = "completed"
	// SubmitStatusBlocked 是显式结构化 blocked：agent 自报无法完成，任务经
	// store 进入 blocked 终态（cause=agent_reported_blocked），永远不满足
	// 下游依赖；非图任务在终态落盘后由收尾路径发布 replan 唤醒任务。
	SubmitStatusBlocked = "blocked"

	// StructuredResultStorageKey 是 Task.Results 中保存自定义结构化结果的
	// 内部 carrier 键。Task.Results 的持久化契约仍是 map[string]string，
	// 因此 submit_task_result 的 result JSON object 先以 canonical JSON
	// 存在本键下；Graph terminal feed 再解码并把其字段类型保真地展开到
	// TerminalFact.Result 顶层。用户提交的 result 不得占用本键。
	StructuredResultStorageKey = "__agentgo_structured_result_v1"
	FulfillmentStorageKey      = "__agentgo_fulfillment_v1"
)

// StructuredSubmission 是 submit_task_result 工具写入的一次结构化提交。
// 字段全部由 LLM 在工具参数中自述；框架只做校验与渲染，不改写内容。
type StructuredSubmission struct {
	TaskID          string
	Summary         string   // 一两句话的结果概括；随依赖结果传递给下游任务
	ChecksPerformed []string // 已执行的验证（如 go build / go test ./...）
	Evidence        []string // 支撑证据（文件路径、命令输出要点等）
	RemainingRisks  []string // 残余风险
	BlockedReason   string   // 无法完成时的阻塞原因；非空时工具层会登记高优 ReplanRequest
	RequestReplan   bool     // 提交者请求 Scheduler 重新评估当前 Plan
	// Status 是自述终态：""/completed 走既有 completed 收尾；blocked 走结构化
	// blocked 收尾（agent finalization 分支先落 blocked 终态、再发布 replan
	// 唤醒任务）。Status=blocked 时 BlockedReason 必填（工具层校验）。
	// 不参与 Format 渲染——终态是路由事实，不是结果正文。
	Status string
	// Event 是本结果对应的事件名（可选）：非空时由 agent 在 SubmitResult 前写入
	// task.Results["event"]，驱动 V6 Graph 事件形态转移条件（when.event）。
	// 它不参与 Format 渲染——事件是图路由事实，不是结果正文。
	Event string
	// Verdict 是本结果对应的验收结论（可选，如 pass/fail/fixable）：非空时由
	// agent 在 SubmitResult 前写入 task.Results["verdict"]，驱动 V6 Graph
	// acceptance 节点的路径形态转移条件（$.verdict eq ...）。
	// 与 Event 一样不参与 Format 渲染。
	Verdict string
	// CitedEvidence 是验收结论引用的证据清单（可选，逗号分隔的不透明稳定
	// EvidenceRef）：非空时由 agent 在 SubmitResult 前写入
	// task.Results["cited_evidence"]，由 V6 Graph acceptance 节点做谱系核验
	// ——引用必须属于该 acceptance activation 的上游 Input 谱系或本任务自身
	// 证据，越谱系引用会使 verdict 不被采信（disputed）。与 Verdict 一样
	// 不参与 Format 渲染（核验通道，不是结果正文）。
	CitedEvidence string
	// ResultJSON 是 result 参数经边界校验和 JSON 规范化后的 object 文本。
	// 空串表示调用方没有提交自定义结构化字段；"{}" 表示显式提交空 object。
	// 它不参与 Format 渲染，而是在 finalization 时写入
	// Results[StructuredResultStorageKey]，供 Graph 条件与数据流消费。
	ResultJSON      string
	FulfillmentJSON string
}

// DecodeStructuredResult 解码 Task.Results 内部 carrier 中的 JSON object。
// 编解码协议集中在 agent 包，避免 Graph bridge 复制内部键或把 object 错当
// 普通字符串。输入必须是 object；null、数组和标量一律拒绝。
func DecodeStructuredResult(raw string) (map[string]any, error) {
	var result map[string]any
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("解码结构化结果: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("结构化结果必须是 JSON object")
	}
	return result, nil
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
