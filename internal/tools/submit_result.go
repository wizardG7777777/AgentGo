package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentgo/internal/agent"
	"agentgo/internal/checkstore"
	"agentgo/internal/fulfillment"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

const (
	// structuredResultMaxBytes 与 Graph 小结果内联上限对齐：结构化路由事实
	// 应保持紧凑；大内容必须走 artifact / evidence 引用，不得塞入控制字段。
	structuredResultMaxBytes  = graph.InputInlineMaxBytes
	structuredResultMaxKeys   = 64
	structuredResultMaxDepth  = 16
	structuredResultKeyMaxLen = 64
)

var structuredResultReservedKeys = map[string]struct{}{
	"status":                         {},
	"event":                          {},
	"verdict":                        {},
	"cited_evidence":                 {},
	agent.StructuredResultStorageKey: {},
}

var submitTaskResultAllowedArgs = map[string]struct{}{
	"summary": {}, "checks_performed": {}, "evidence": {},
	"remaining_risks": {}, "status": {}, "blocked_reason": {},
	"request_replan": {}, "event": {}, "verdict": {},
	"cited_evidence": {}, "result": {},
}

// submitTaskResult 是 submit_task_result 工具的实现：普通执行节点的结构化提交通道。
//
// 与"自然完成"（本轮不调工具、输出纯文本）相比，它把 摘要/自定义 JSON Result/
// 已做检查/证据/残余风险/阻塞原因/自述终态 结构化，由 agent 的 finalization
// 短路分支渲染并持久化为权威结果：
//   - 先跑与自然完成同源的 ExpectedArtifacts 合约校验；缺失时返回错误且不标记
//     finalized——LLM 在本轮 ReAct 循环内补写文件后可重新调用；
//   - 校验通过后写入 agent.SubmitState 并 MarkTaskFinalized，下一轮 loop 顶部短路
//     退出（Transition.Cause=submit_task_result）；同一响应中排在本工具之后的
//     工具调用会被 finalizing fence 跳过（见 llm_executor），提交因此是
//     「唯一终态提交者」——已 finalized 后重复调用本工具直接返回错误；
//   - status=blocked（需同时填 blocked_reason）时自述 blocked 终态：agent 收尾
//     分支先落 blocked 终态（cause=agent_reported_blocked）、再为**非图任务**
//     发布 replan 唤醒任务，工具层不再附带发布；
//   - status 缺省（completed）且 blocked_reason 非空或 request_replan=true 时，
//     对非图任务额外发布一份 __scheduler__ 唤醒任务（与 request_replan 工具
//     同机制，幂等键 <taskID>/replan），让 Scheduler 在任务终态后重新决策；
//     图任务由 graph-terminal-feed 终态回填驱动边路由，跳过不登记。
//   - 终态契约 v2（schema v2 图任务，注入 OutletChecker 时）：event 参数
//     废弃、verdict 仅限 acceptance（均为参数级错误，不计入两击）；终态落盘
//     前经 CheckActivationOutlet 预求值出边——无匹配时首击拒绝（不
//     finalizing，可修正重交），第二击节点 failed + no-outlet 唤醒 Scheduler，
//     任务置 failed 并返回不可重试错误。
//
// 拒绝对象：不属于 Graph 的 scheduler 任务（指引用 report_done）。Graph
// controller 的 EventType 同样是 __scheduler__，但必须通过本工具提交 event /
// 自定义 result 等结构化节点结果；acceptance 则提交 verdict，不能被角色名
// 一并拒绝。
func (g PlanControlGroup) submitTaskResult(ctx context.Context, args map[string]any) (string, error) {
	taskID := g.Holder.Get()
	if taskID == "" {
		return "", fmt.Errorf("无法获取当前任务上下文")
	}
	// Task.Results 同时以 agent_id 保存权威正文，并以固定键保存 Graph 协议
	// 字段。两者同名时无论采用哪种覆盖顺序都会破坏一份事实，必须在进入
	// finalizing 前拒绝。
	if _, reserved := structuredResultReservedKeys[g.AgentID]; reserved {
		return "", fmt.Errorf("当前 agent_id %q 与 submit_task_result 的系统结果键冲突，无法安全提交结构化终态", g.AgentID)
	}
	task, err := g.Store.GetTask(taskID)
	if err != nil {
		return "", fmt.Errorf("读取当前任务失败: %w", err)
	}
	// 非图 scheduler 才有 report_done 专用汇报通道。Graph controller 必须
	// 使用本通道，否则 Results["event"] 无法进入 Runtime 的边条件求值。
	if task.EventType == "__scheduler__" && task.GraphID == "" {
		return "", fmt.Errorf("submit_task_result 仅面向执行节点（含 Graph controller）；非图 scheduler 任务请使用 report_done")
	}
	// 唯一终态提交者：已 finalized（本任务已成功提交过一次）后拒绝重复提交，
	// 不改变任何既有状态。经窄接口探测——旧装配的 notifier 不实现 IsFinalized
	// 时退化为不检查（重复 Put 以最新一次为准的旧行为）。
	if checker, ok := g.FinalizationNotifier.(interface{ IsFinalized() bool }); ok && checker.IsFinalized() {
		return "", fmt.Errorf("任务已提交结构化结果并进入收尾（finalizing）：submit_task_result 每次任务只能成功提交一次，本次重复调用被拒绝")
	}
	if err := validateSubmitTaskResultArgs(args); err != nil {
		return "", err
	}

	// 终态契约 v2：属于 schema v2 图的任务在提交期接受追加约束（event 废弃、
	// verdict 仅限 acceptance、终态落盘前出路匹配检查）。图不存在 / v1 图 /
	// 未注入 OutletChecker 时按 v1 语义处理（行为与引入前逐字节一致）。
	v2Graph := task.GraphID != "" && g.OutletChecker != nil &&
		(graph.OutletSchemaIsV2OrLater(g.OutletChecker.GraphSchema(task.GraphID)))

	summary, _ := args["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		return "", fmt.Errorf("summary 不能为空：请用一两句话概括任务结果")
	}
	checksRaw, _ := args["checks_performed"].(string)
	evidenceRaw, _ := args["evidence"].(string)
	risksRaw, _ := args["remaining_risks"].(string)
	blockedReason, _ := args["blocked_reason"].(string)
	requestReplan, _ := args["request_replan"].(bool)
	eventName, _ := args["event"].(string)
	eventName = strings.TrimSpace(eventName)
	if v2Graph && eventName != "" {
		return "", fmt.Errorf("event 参数在终态契约 v2 已废弃：v2 图任务的业务路由一律改用 result 数据字段 + 出边 path 条件，请把路由信息放进 result（参数级错误，不计入出路检查两击）")
	}
	if task.GraphID != "" && eventName != "" && !graph.IsValidEventName(eventName) {
		return "", fmt.Errorf("event %q 不属于 Graph 事件词表（仅允许 ready/completed/fixable/failed/blocked/pass/approved/rejected/timeout/always）", eventName)
	}
	verdict, _ := args["verdict"].(string)
	verdict = strings.TrimSpace(verdict)
	if verdict != "" {
		switch verdict {
		case "pass", "fixable", "failed":
		default:
			return "", fmt.Errorf("verdict 只接受 pass / fixable / failed，实际值 %q", verdict)
		}
		if eventName != "" {
			return "", fmt.Errorf("verdict 与 event 互斥：acceptance 业务结论只能通过 $.verdict 路由，不得同时提交 event")
		}
		if v2Graph && task.GraphNodeKind != string(graph.KindAcceptance) {
			return "", fmt.Errorf("verdict 仅允许 acceptance 节点任务提交（当前节点类型 %q）：v2 图普通节点的业务路由改用 result 数据字段 + path 条件（参数级错误，不计入出路检查两击）", task.GraphNodeKind)
		}
	}
	// cited_evidence：Graph acceptance 节点验收任务引用的证据清单（逗号分隔
	// 的不透明稳定 EvidenceRef 或 typed CheckRef），经 StructuredSubmission 写入
	// Results["cited_evidence"]，由 Graph Runtime 做谱系核验（引用必须属于
	// 该 activation 的上游 Input 谱系或本任务自身证据）。提交时做轻量形态
	// 校验（逐项非空）——谱系核验在图侧进行。
	citedEvidence, _ := args["cited_evidence"].(string)
	if trimmed := strings.TrimSpace(citedEvidence); trimmed != "" {
		for _, ref := range strings.Split(trimmed, ",") {
			if strings.TrimSpace(ref) == "" {
				return "", fmt.Errorf("cited_evidence 含空白引用项：应为逗号分隔的不透明稳定 EvidenceRef/CheckRef 清单")
			}
		}
		citedEvidence = trimmed
	}

	// result 是 Agent 可供 Graph 路由和下游数据流消费的自定义 JSON object。
	// 专用 event/verdict/cited_evidence/status 键必须走各自参数，防止绕过其
	// 词表、谱系和终态校验；其余字段在 Graph 终态回填时类型保真地展开到
	// Result 顶层，因此可直接被 $.coverage / $.metrics.score 等条件读取。
	resultJSON, err := normalizeStructuredResult(args, g.AgentID)
	if err != nil {
		return "", err
	}

	// status 自述终态：缺省 completed；blocked 必须附 blocked_reason；
	// failed/cancelled 由系统路径产生，不接受 agent 自报。
	status, _ := args["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = agent.SubmitStatusCompleted
	}
	switch status {
	case agent.SubmitStatusCompleted:
	case agent.SubmitStatusBlocked:
		if strings.TrimSpace(blockedReason) == "" {
			return "", fmt.Errorf("status=blocked 时必须填写 blocked_reason 说明阻塞原因")
		}
		if verdict != "" || eventName != "" {
			return "", fmt.Errorf("status=blocked 时不得填写 verdict/event；blocked 是任务终态，由 Runtime 的 blocked 兜底边处理")
		}
	default:
		return "", fmt.Errorf("status 只接受 completed / blocked（failed、cancelled 由系统路径产生，不接受自报），实际值 %q", status)
	}
	if status == agent.SubmitStatusBlocked && g.Checkpoints != nil && task.ProgressContract != nil &&
		task.ProgressContract.Policy.KnowledgeCheckpointAfterTurns > 0 {
		checkpoint, ok, checkpointErr := g.Checkpoints.LoadCheckpoint(task.ID)
		if checkpointErr != nil {
			return "", fmt.Errorf("读取 blocked Observation checkpoint: %w", checkpointErr)
		}
		if ok && checkpoint != nil && checkpoint.AttemptID == task.AttemptID &&
			checkpoint.KnowledgeTurnsSinceObservation > 0 && checkpoint.ObservationAttemptID != task.AttemptID {
			return "", fmt.Errorf("reason_code=observation_checkpoint_required [observation-checkpoint-required action=blocked_submit] blocked 前必须先冻结当前 Attempt 的 ObservationDelta")
		}
	}

	// ExpectedArtifacts 合约校验（与自然完成路径同源，含磁盘兜底）。缺失时
	// 返回错误并保持未 finalized——本轮 ReAct 循环继续，LLM 补写后可再次调用。
	check := agent.CheckExpectedArtifactsWithDisk(g.Store, task.ID, g.ArtifactResolver)
	if len(check.Missing) > 0 {
		return "", fmt.Errorf("submit_task_result 被拒绝：%s", agent.BuildArtifactFailureReason(check))
	}
	if err := g.recordRecoveredArtifacts(task.ID, check.Recovered); err != nil {
		return "", fmt.Errorf("submit_task_result 被拒绝：磁盘恢复的预期产物未能写入 durable artifact ledger: %w", err)
	}
	fulfillmentJSON := ""
	if status == agent.SubmitStatusCompleted && task.FulfillmentContract != nil {
		record, fulfillmentErr := g.buildFulfillment(task)
		if fulfillmentErr != nil {
			return "", fmt.Errorf("reason_code=contract_fulfillment_missing：submit_task_result 被拒绝：%w；请先完成真实文件修改并在最后一次修改后调用 run_check", fulfillmentErr)
		}
		encoded, _ := json.Marshal(record)
		fulfillmentJSON = string(encoded)
	}

	// 终态契约 v2 提交期出路检查（终态落盘前）：用该 activation 冻结定义的
	// 出边对「status 镜像事件 + result 数据」预求值。首击拒绝时不 finalizing、
	// 不产生任何终态写入（agent 可修正重交）；第二击升级时 runtime 已把节点
	// 置 failed 并发布 no-outlet 唤醒任务，此处同步把任务置 failed 并返回
	// 不可重试的终态错误。v1 图与非图任务不检查（v1 无匹配仍由回填时
	// fail-closed）。
	if v2Graph {
		evalResult := buildOutletEvalResult(resultJSON, verdict, citedEvidence, status)
		if err := g.OutletChecker.CheckActivationOutlet(task.GraphID, task.NodeID, task.ActivationID, status, evalResult); err != nil {
			var outletErr *graph.OutletError
			if errors.As(err, &outletErr) && outletErr.Escalated {
				reason := fmt.Sprintf("终态契约 v2 两击升级：节点 %s（activation %s）两次提交均无匹配出路（contract_no_outlet），已升级 Scheduler 裁决", task.NodeID, task.ActivationID)
				if failErr := g.Store.FailTask(g.AgentID, taskID, reason); failErr != nil {
					// 任务已被外部置终态（取消/看门狗）时忽略：终态唯一即可。
					log.Printf("[tools] 任务 %s 两击升级后 FailTask 未生效（可能已被外部置终态）: %v", taskID, failErr)
				}
			}
			return "", err
		}
	}

	g.SubmitState.Put(&agent.StructuredSubmission{
		TaskID:          task.ID,
		Summary:         summary,
		ChecksPerformed: splitList(checksRaw),
		Evidence:        splitList(evidenceRaw),
		RemainingRisks:  splitList(risksRaw),
		BlockedReason:   blockedReason,
		RequestReplan:   requestReplan,
		Status:          status,
		Event:           eventName,
		Verdict:         verdict,
		CitedEvidence:   citedEvidence,
		ResultJSON:      resultJSON,
		FulfillmentJSON: fulfillmentJSON,
	})
	g.FinalizationNotifier.MarkTaskFinalized()

	// 审计：结构化提交被接受。同一响应中排在其后的工具调用将被 finalizing
	// fence 跳过（tool_call_skipped）；终态提交本身由 agent 收尾路径
	// 以 task_result_committed 记录。
	trace.Emit(trace.Event{
		Kind:    trace.KindTaskFinalizing,
		TaskID:  task.ID,
		RunID:   string(task.RunID),
		AgentID: g.AgentID,
		Transition: &trace.Transition{
			PrevStatus: string(model.TaskStatusProcessing),
			NewStatus:  status,
		},
	})

	// status=blocked：终态提交与 replan 唤醒由 agent 收尾路径按「终态先
	// durable、再发布唤醒」的同一收尾事务完成，工具层不再附带发布唤醒任务。
	if status == agent.SubmitStatusBlocked {
		return "结构化结果已提交（status=blocked）：系统将把任务以 blocked 终态收尾（结果摘要与阻塞原因随终态保留），并在终态落盘后唤醒 Scheduler 重新规划。请停止调用其他工具，直接结束本轮。", nil
	}

	// 阻塞/重规划诉求随提交登记；图任务由 graph-terminal-feed 终态回填驱动
	// 边路由，无需唤醒任务。提交本身已生效（finalized），登记失败只降级为
	// 提示，不推翻提交。
	replanNote := ""
	if blockedReason != "" || requestReplan {
		if task.GraphID == "" {
			reasonCode := "submit_request_replan"
			detail := summary
			urgency := "normal"
			if blockedReason != "" {
				reasonCode = "submit_blocked"
				urgency = "high"
				detail = "blocked_reason: " + blockedReason + "\nsummary: " + summary
			}
			note, replanErr := g.requestGenericReplan(ctx, task, map[string]any{
				"reason_code": reasonCode, "urgency": urgency, "detail": detail,
			})
			if replanErr != nil {
				replanNote = fmt.Sprintf("；注意：replan 唤醒任务发布失败: %v", replanErr)
			} else {
				replanNote = "；" + note
			}
		}
	}

	return "结构化结果已提交：系统将以本次提交作为任务权威结果收尾（渲染文本随依赖结果传递给下游任务）。请停止调用其他工具，直接结束本轮。" + replanNote, nil
}

func (g PlanControlGroup) buildFulfillment(task *model.Task) (fulfillment.Record, error) {
	if task == nil || task.FulfillmentContract == nil {
		return fulfillment.Record{}, nil
	}
	if g.Checks == nil {
		return fulfillment.Record{}, fmt.Errorf("CheckStore 未装配")
	}
	workspaceRef, effectRefs, err := checkstore.WorkspaceRevision(task, g.Store, g.Workspaces)
	if err != nil {
		return fulfillment.Record{}, err
	}
	record := fulfillment.Record{
		Schema: fulfillment.SchemaV1, WorkspaceRevisionRef: workspaceRef,
		EffectRefs: append([]string(nil), effectRefs...),
	}
	for _, checkID := range task.FulfillmentContract.RequiredCheckIDs {
		check, ok, checkErr := g.Checks.Latest(task.ID, task.AttemptID, checkID)
		if checkErr != nil {
			return fulfillment.Record{}, checkErr
		}
		if !ok {
			return fulfillment.Record{}, fmt.Errorf("缺少 required check %q", checkID)
		}
		if check.Status != checkstore.StatusPass {
			return fulfillment.Record{}, fmt.Errorf("required check %q 未通过", checkID)
		}
		if check.WorkspaceRevisionRef != workspaceRef {
			return fulfillment.Record{}, fmt.Errorf("required check %q 已 stale：check=%s current=%s",
				checkID, check.WorkspaceRevisionRef, workspaceRef)
		}
		record.CheckRefs = append(record.CheckRefs, check.CheckRef)
		record.SatisfiedRequirementIDs = append(record.SatisfiedRequirementIDs, checkID)
	}
	if task.FulfillmentContract.RequireWorkspaceChange {
		record.SatisfiedRequirementIDs = append(record.SatisfiedRequirementIDs, "workspace-change")
	}
	if err := record.Validate(task.FulfillmentContract); err != nil {
		return fulfillment.Record{}, err
	}
	return record, nil
}

// recordRecoveredArtifacts 把 expected_artifacts 磁盘兜底命中的文件
// 在进入 finalizing 前补登到 durable ledger。仅 stat 成功不足以
// 形成 Graph artifact Evidence；必须同步固化路径与当前内容身份。
// 多个文件中途失败可安全重试：Store 以 path+latest meta 幂等。
func (g PlanControlGroup) recordRecoveredArtifacts(taskID string, recovered []string) error {
	if len(recovered) == 0 {
		return nil
	}
	ledger, ok := g.Store.(interface {
		AppendArtifactWithMeta(taskID string, path string, meta model.ArtifactMeta) error
	})
	if !ok {
		return fmt.Errorf("TaskStore 不支持 AppendArtifactWithMeta")
	}
	if g.ArtifactResolver == nil {
		return fmt.Errorf("ArtifactResolver 未装配")
	}
	for _, expected := range recovered {
		physical := g.ArtifactResolver(taskID, expected)
		meta, err := recoveredArtifactMeta(physical)
		if err != nil {
			return fmt.Errorf("读取恢复产物 %s 失败: %w", expected, err)
		}
		path := filepath.ToSlash(filepath.Clean(expected))
		if err := ledger.AppendArtifactWithMeta(taskID, path, meta); err != nil {
			return fmt.Errorf("登记恢复产物 %s 失败: %w", path, err)
		}
	}
	return nil
}

// recoveredArtifactMeta 流式计算大产物内容身份，避免 os.ReadFile
// 按文件大小无界分配内存。显式 Close 使 Windows TempDir/工作区
// 清理不会被本路径留下的句柄阻塞。
func recoveredArtifactMeta(path string) (model.ArtifactMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return model.ArtifactMeta{}, err
	}
	h := sha256.New()
	bytesCopied, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil {
		return model.ArtifactMeta{}, copyErr
	}
	if closeErr != nil {
		return model.ArtifactMeta{}, closeErr
	}
	return model.ArtifactMeta{SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: bytesCopied}, nil
}

// buildOutletEvalResult 构造与 graph-terminal-feed 同形态的终态 Result，
// 供终态契约 v2 提交期出路预求值：自定义 result 字段类型保真地展开到顶层；
// completed 终态另带 verdict / cited_evidence 协议键（blocked 落盘口径不带
// 这两个键，预求值保持一致）。resultJSON 已经 normalizeStructuredResult 校验，
// 解码失败只可能为空串，按无自定义字段处理。
func buildOutletEvalResult(resultJSON, verdict, citedEvidence, status string) map[string]any {
	result := map[string]any{}
	if resultJSON != "" {
		if structured, err := agent.DecodeStructuredResult(resultJSON); err == nil {
			for k, v := range structured {
				result[k] = v
			}
		}
	}
	if status == agent.SubmitStatusCompleted {
		if verdict != "" {
			result["verdict"] = verdict
		}
		if citedEvidence != "" {
			result["cited_evidence"] = citedEvidence
		}
	}
	return result
}

func validateSubmitTaskResultArgs(args map[string]any) error {
	unknown := make([]string, 0)
	for key := range args {
		if _, ok := submitTaskResultAllowedArgs[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("submit_task_result 含未知参数 %s：请只使用工具 schema 声明的字段", strings.Join(unknown, ", "))
}

// normalizeStructuredResult 校验 result object 的形状、键、深度和体积，并
// 返回无空白、稳定键序的 JSON 文本。不存在 result 时返回空串；显式空 object
// 返回 "{}"，两者语义可审计地区分。
func normalizeStructuredResult(args map[string]any, agentID string) (string, error) {
	raw, present := args["result"]
	if !present {
		return "", nil
	}
	result, ok := raw.(map[string]any)
	if !ok || result == nil {
		return "", fmt.Errorf("result 必须是 JSON object；数组、字符串、数字、布尔或 null 均不接受")
	}
	if len(result) > structuredResultMaxKeys {
		return "", fmt.Errorf("result 顶层字段数 %d 超过上限 %d", len(result), structuredResultMaxKeys)
	}
	if agentID == agent.StructuredResultStorageKey {
		return "", fmt.Errorf("当前 agent_id 与系统结构化结果保留键冲突，无法安全提交 result")
	}
	if err := validateStructuredResultValue(result, 1, agentID, true); err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("result 不是可序列化的 JSON object: %w", err)
	}
	if len(data) > structuredResultMaxBytes {
		return "", fmt.Errorf("result 规范化后大小 %d 字节超过上限 %d 字节；大内容请写入 artifact 并在 evidence 中引用", len(data), structuredResultMaxBytes)
	}

	// 再解码一次，拒绝 MarshalJSON 等自定义类型生成的非 object 载荷，并把
	// int/json.Number 等 Go 测试输入归一为 Graph 条件求值使用的 JSON 类型。
	normalized, err := agent.DecodeStructuredResult(string(data))
	if err != nil {
		return "", fmt.Errorf("result 规范化失败: %w", err)
	}
	data, err = json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("result 规范化失败: %w", err)
	}
	return string(data), nil
}

func validateStructuredResultValue(value any, depth int, agentID string, topLevel bool) error {
	if depth > structuredResultMaxDepth {
		return fmt.Errorf("result JSON 嵌套深度超过上限 %d", structuredResultMaxDepth)
	}
	switch v := value.(type) {
	case map[string]any:
		if len(v) > structuredResultMaxKeys {
			return fmt.Errorf("result object 字段数 %d 超过单层上限 %d", len(v), structuredResultMaxKeys)
		}
		for key, child := range v {
			if !isStructuredResultKey(key) {
				return fmt.Errorf("result 字段名 %q 非法：必须是最长 %d 字节的 ASCII 标识符（字母或下划线开头，仅含字母、数字、下划线）", key, structuredResultKeyMaxLen)
			}
			if topLevel {
				if _, reserved := structuredResultReservedKeys[key]; reserved {
					return fmt.Errorf("result 顶层字段 %q 是系统保留键；请使用 submit_task_result 的同名专用参数", key)
				}
				if key == agentID {
					return fmt.Errorf("result 顶层字段 %q 与当前 agent_id 冲突，会覆盖权威结果正文", key)
				}
			}
			if err := validateStructuredResultValue(child, depth+1, agentID, false); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := validateStructuredResultValue(child, depth+1, agentID, false); err != nil {
				return err
			}
		}
	case nil, string, bool, float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		// json.Marshal 在下一步负责拒绝 NaN、Inf 和越界数字。
	default:
		return fmt.Errorf("result 包含 JSON 不支持的值类型 %T", value)
	}
	return nil
}

func isStructuredResultKey(key string) bool {
	if key == "" || len(key) > structuredResultKeyMaxLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if i == 0 {
			if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				return false
			}
			continue
		}
		if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
