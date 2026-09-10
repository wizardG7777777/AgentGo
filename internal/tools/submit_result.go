package tools

import (
	"agentgo/internal/agent"
	"agentgo/internal/executionfacts"
	"agentgo/internal/fulfillment"
	"agentgo/internal/model"
	"agentgo/internal/trace"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (g PlanControlGroup) submitTaskResult(ctx context.Context, args map[string]any) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if g.Store == nil || g.Holder == nil || g.SubmitState == nil || g.FinalizationNotifier == nil {
		return "", fmt.Errorf("任务结果通道未装配")
	}
	finalized, ok := g.FinalizationNotifier.(interface{ IsFinalized() bool })
	if !ok {
		return "", fmt.Errorf("缺少 finalizing 状态权威")
	}
	if finalized.IsFinalized() {
		return "", fmt.Errorf("本任务已提交结果")
	}
	var in struct {
		Summary       string         `json:"summary"`
		Result        map[string]any `json:"result"`
		Status        string         `json:"status"`
		BlockedReason string         `json:"blocked_reason"`
		Checks        string         `json:"checks_performed"`
		Evidence      string         `json:"evidence"`
		Risks         string         `json:"remaining_risks"`
	}
	if err := decodeNativeGraphArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Summary) == "" {
		return "", fmt.Errorf("缺少 summary")
	}
	if in.Status == "" {
		in.Status = "completed"
	}
	if in.Status != "completed" && in.Status != "blocked" {
		return "", fmt.Errorf("非法提交状态")
	}
	if in.Status == "blocked" && strings.TrimSpace(in.BlockedReason) == "" {
		return "", fmt.Errorf("blocked 必须说明原因")
	}
	task, err := g.Store.GetTask(g.Holder.Get())
	if err != nil || task == nil || task.Status != model.TaskStatusProcessing {
		return "", fmt.Errorf("当前任务不是 processing")
	}
	if in.Result == nil {
		in.Result = map[string]any{"summary": in.Summary}
	}
	if task.GraphID != "" && in.Status == "completed" {
		if g.ResultValidator == nil {
			return "", fmt.Errorf("agentTask 结果校验未装配")
		}
		if err := g.ResultValidator.ValidateAgentTaskResult(task.GraphID, task.NodeID, in.Result); err != nil {
			return "", err
		}
	}
	raw, err := json.Marshal(in.Result)
	if err != nil || len(raw) > 1<<20 {
		return "", fmt.Errorf("结果不可编码或超过1MiB")
	}
	check := agent.CheckExpectedArtifactsWithDisk(g.Store, task.ID, g.ArtifactResolver)
	if len(check.Missing) > 0 {
		return "", fmt.Errorf("缺少预期产物: %s", agent.BuildArtifactFailureReason(check))
	}
	if err := g.recordRecoveredArtifacts(task.ID, check.Recovered); err != nil {
		return "", err
	}
	fulfillmentJSON := ""
	if in.Status == "completed" && task.FulfillmentContract != nil {
		record, err := g.buildFulfillment(task)
		if err != nil {
			return "", fmt.Errorf("contract_fulfillment_missing: %w", err)
		}
		body, err := json.Marshal(record)
		if err != nil {
			return "", err
		}
		fulfillmentJSON = string(body)
	}
	g.SubmitState.Put(&agent.StructuredSubmission{TaskID: task.ID, Summary: in.Summary, Status: in.Status, BlockedReason: in.BlockedReason, ResultJSON: string(raw), ChecksPerformed: splitList(in.Checks), Evidence: splitList(in.Evidence), RemainingRisks: splitList(in.Risks), FulfillmentJSON: fulfillmentJSON})
	g.FinalizationNotifier.MarkTaskFinalized()
	trace.Emit(trace.Event{Kind: trace.KindTaskFinalizing, TaskID: task.ID, RunID: string(task.RunID), AgentID: g.AgentID})
	return "任务结果已提交；正在结算，停止调用后续工具。", nil
}

func (g PlanControlGroup) buildFulfillment(task *model.Task) (fulfillment.Record, error) {
	if task == nil || task.FulfillmentContract == nil {
		return fulfillment.Record{}, nil
	}
	workspaceRef, effectRefs, err := executionfacts.WorkspaceRevision(task, g.Store, g.Workspaces)
	if err != nil {
		return fulfillment.Record{}, err
	}
	record := fulfillment.Record{
		Schema: fulfillment.SchemaCurrent, WorkspaceRevisionRef: workspaceRef,
		EffectRefs: append([]string(nil), effectRefs...),
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
