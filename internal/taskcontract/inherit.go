// Package taskcontract 负责新 Task 的跨层运行契约继承。
// 发布方不得只复制 RunID 或只复制 deadline；Run/Context/Progress 必须作为
// 同一冻结 binding 建立。
package taskcontract

import (
	"fmt"
	"strings"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/model"
	"agentgo/internal/policycatalog"
	"agentgo/internal/runcontract"

	"github.com/google/uuid"
)

// Start 为没有父 Task 的显式入口建立完整 Run/Context/Progress binding。
func Start(child *model.Task, workClass loopcontract.WorkClass, budgetProfile string,
	window, finalizationReserve, recoveryReserve time.Duration) error {
	if child == nil {
		return fmt.Errorf("task contract start 的 child 不能为空")
	}
	if strings.TrimSpace(budgetProfile) == "" || window <= 0 {
		return fmt.Errorf("task contract start 缺少 budget profile/window")
	}
	now := time.Now().UTC()
	run := &runcontract.RunContract{
		Schema: runcontract.SchemaV1, RunID: runcontract.RunID("run-" + uuid.NewString()),
		CreatedAt: now, DeadlineAt: now.Add(window), BudgetProfile: budgetProfile,
		FinalizationReserve: finalizationReserve, RecoveryReserve: recoveryReserve,
	}
	if err := run.ValidateAt(now); err != nil {
		return err
	}
	parent := &model.Task{ID: "request-ingress", RunID: run.RunID, RunContract: run,
		ContextPolicyRef: policycatalog.ContextDefaultCurrent}
	return Inherit(parent, child, workClass)
}

// Inherit 从 parent 向 child 复制 RunContract，并按 child 的工作类别重新选择
// framework ProgressContract。无 Run 的 parent 是显式 legacy no-op；半绑定拒绝。
func Inherit(parent, child *model.Task, workClass loopcontract.WorkClass) error {
	if parent == nil || child == nil {
		return fmt.Errorf("task contract inherit 的 parent/child 不能为空")
	}
	if parent.RunID == "" && parent.RunContract == nil {
		return nil
	}
	if parent.RunID == "" || parent.RunContract == nil || parent.RunContract.RunID != parent.RunID {
		return fmt.Errorf("父任务 %s 的 RunID/RunContract binding 不完整", parent.ID)
	}
	catalog, err := policycatalog.NewDefault()
	if err != nil {
		return fmt.Errorf("初始化 framework PolicyCatalog: %w", err)
	}
	progressRef, err := progressRefFor(workClass)
	if err != nil {
		return err
	}
	profile, ok := catalog.ProgressContract(progressRef)
	if !ok {
		return fmt.Errorf("framework ProgressContract %s 缺失", progressRef)
	}
	contextRef := parent.ContextPolicyRef
	if contextRef == "" {
		contextRef = policycatalog.ContextDefaultCurrent
	}
	if !catalog.HasContextPolicy(contextRef) {
		return fmt.Errorf("父任务 %s 引用未知 ContextPolicy=%s", parent.ID, contextRef)
	}
	run := *parent.RunContract
	child.RunID = parent.RunID
	child.RunContract = &run
	if child.RunPhase == "" {
		child.RunPhase = runcontract.PhaseExecution
	}
	if !child.RunPhase.Valid() {
		return fmt.Errorf("子任务 RunPhase=%q 无效", child.RunPhase)
	}
	child.ContextPolicyRef = contextRef
	child.ProgressContract = &profile.Contract
	return nil
}

func progressRefFor(workClass loopcontract.WorkClass) (string, error) {
	switch workClass {
	case loopcontract.WorkCodeChange:
		return policycatalog.ProgressCodeChangeCurrent, nil
	case loopcontract.WorkInvestigation:
		return policycatalog.ProgressInvestigationV1, nil
	case loopcontract.WorkVerification:
		return policycatalog.ProgressVerificationV1, nil
	case loopcontract.WorkCoordination:
		return policycatalog.ProgressCoordinationV1, nil
	default:
		return "", fmt.Errorf("未知 Task work_class=%q", workClass)
	}
}
