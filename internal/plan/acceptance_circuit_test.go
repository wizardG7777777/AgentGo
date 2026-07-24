package plan

import (
	"strings"
	"context"
	"errors"
	"testing"

	"agentgo/internal/model"
)

// 同类外部事实核验连续失败达到熔断阈值后，EnsureAcceptanceRun 拒绝创建新 run，
// Plan 挂起并发出高优信号，等待用户决策而不是机械重试。
func TestAcceptanceCircuitOpensAfterRepeatedIdenticalExternalFailures(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	verifier := &rejectingVerifier{}
	c.SetAcceptanceVerifier(verifier)
	p := createTestPlan(t, c, "p-accept-circuit", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}

	submitFailingResult := func() {
		t.Helper()
		run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID})
		if err != nil {
			t.Fatalf("EnsureAcceptanceRun: %v", err)
		}
		result, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
			RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
			CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictPass}},
		})
		if !errors.Is(err, ErrAcceptanceConstraint) || result.Verdict != model.AcceptanceVerdictFail {
			t.Fatalf("submit result=%+v err=%v, want external verification failure", result, err)
		}
		if result.FailureFingerprint == "" {
			t.Fatal("external verification failure did not record a FailureFingerprint")
		}
	}

	submitFailingResult()
	submitFailingResult()

	if _, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID}); !errors.Is(err, ErrAcceptanceCircuitOpen) {
		t.Fatalf("expected ErrAcceptanceCircuitOpen, got %v", err)
	}
	p, _ = c.Store().GetPlan(p.ID)
	if p.Status != model.PlanStatusPausedAwaitingDecision || p.PauseReason != "acceptance_circuit_open" {
		t.Fatalf("circuit pause = status %s reason %q, want paused/acceptance_circuit_open", p.Status, p.PauseReason)
	}
	signal, ok, err := c.TrySignal(p.ID)
	if err != nil || !ok || !containsString(signal.Reasons, "acceptance_circuit_open") {
		t.Fatalf("circuit signal=%+v ok=%v err=%v", signal, ok, err)
	}
}

// 硬约束失败（如 command_exit 缺命令证据、内建图事实不满足）同样纳入熔断：
// 同因连续失败达到阈值后 EnsureAcceptanceRun 拒绝创建新 run，Plan 挂起交用户
// 决策（2026-07-21 事故：command evidence target mismatch 同因连错 3 次无熔断）。
func TestAcceptanceCircuitOpensAfterRepeatedIdenticalHardConstraintFailures(t *testing.T) {
	c := NewCoordinator(NewMemoryStore(), nil)
	p := createTestPlan(t, c, "p-hardc-circuit", model.PlanBudget{})
	p = registerNode(t, c, p.ID, 0, "work")
	if _, err := c.DefineAcceptanceSpec(context.Background(), p.ID, testSpec(p.ID)); err != nil {
		t.Fatal(err)
	}

	submitFailingResult := func() {
		t.Helper()
		run, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID})
		if err != nil {
			t.Fatalf("EnsureAcceptanceRun: %v", err)
		}
		// 不提供任何 command 证据——command_exit 标准必然触发硬约束失败。
		result, _, err := c.SubmitAcceptanceResult(context.Background(), model.AcceptanceResult{
			RunID: run.ID, PlanID: p.ID, Verdict: model.AcceptanceVerdictPass,
			CriterionResults: []model.CriterionResult{{CriterionID: "tests", Verdict: model.AcceptanceVerdictPass}},
		})
		if !errors.Is(err, ErrAcceptanceConstraint) || result.Verdict != model.AcceptanceVerdictFail {
			t.Fatalf("submit result=%+v err=%v, want hard constraint failure", result, err)
		}
		if !strings.HasPrefix(result.FailureFingerprint, hardConstraintFingerprintPrefix) {
			t.Fatalf("hard constraint failure fingerprint = %q, want %q prefix",
				result.FailureFingerprint, hardConstraintFingerprintPrefix)
		}
	}

	submitFailingResult()
	submitFailingResult()

	if _, _, err := c.EnsureAcceptanceRun(context.Background(), EnsureAcceptanceRunInput{PlanID: p.ID}); !errors.Is(err, ErrAcceptanceCircuitOpen) {
		t.Fatalf("expected ErrAcceptanceCircuitOpen, got %v", err)
	}
	p, _ = c.Store().GetPlan(p.ID)
	if p.Status != model.PlanStatusPausedAwaitingDecision || p.PauseReason != "acceptance_circuit_open" {
		t.Fatalf("circuit pause = status %s reason %q, want paused/acceptance_circuit_open", p.Status, p.PauseReason)
	}
}
