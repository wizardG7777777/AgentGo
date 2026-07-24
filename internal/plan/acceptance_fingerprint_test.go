package plan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agentgo/internal/model"
)

func TestExternalFactFailureFingerprintStableAcrossVolatileContent(t *testing.T) {
	// 同类缺陷：命令串、任务 ID、证据 ID 都不同，指纹必须相同。
	err1 := errors.New(`no successful run_shell fact for command "ls -la docs 2>&1"`)
	err2 := errors.New(`no successful run_shell fact for command "dir /b internal\\agent 2>nul"`)
	if fp1, fp2 := ExternalFactFailureFingerprint(err1), ExternalFactFailureFingerprint(err2); fp1 != fp2 {
		t.Fatalf("same defect category produced different fingerprints:\n%s\n%s", fp1, fp2)
	}

	err3 := errors.New(`task status evidence ev-task-0c7d10f8 claims "completed: long description" for task 0c7d10f8-a187-4945-bcb5-8bb24323f055, actual status is "completed"`)
	err4 := errors.New(`task status evidence ev-task-41b2b8b7 claims "done with details" for task 41b2b8b7-e3a7-484a-a164-f3f164a8a9cb, actual status is "failed"`)
	if fp3, fp4 := ExternalFactFailureFingerprint(err3), ExternalFactFailureFingerprint(err4); fp3 != fp4 {
		t.Fatalf("task_status defect produced different fingerprints:\n%s\n%s", fp3, fp4)
	}

	// 不同类缺陷：指纹必须不同。
	if fp1, fp3 := ExternalFactFailureFingerprint(err1), ExternalFactFailureFingerprint(err3); fp1 == fp3 {
		t.Fatalf("different defect categories produced the same fingerprint: %s", fp1)
	}
}

func TestExternalFactFailureFingerprintPrefixAndNil(t *testing.T) {
	if fp := ExternalFactFailureFingerprint(nil); fp != "" {
		t.Fatalf("nil error fingerprint = %q, want empty", fp)
	}
	fp := ExternalFactFailureFingerprint(errors.New("some failure"))
	if !strings.HasPrefix(fp, externalFactFingerprintPrefix) {
		t.Fatalf("fingerprint %q missing %q prefix", fp, externalFactFingerprintPrefix)
	}
}

func newCircuitPlan() *model.Plan {
	return &model.Plan{
		ID:                            "plan-1",
		CurrentAcceptanceSpecID:       "spec-1",
		CurrentAcceptanceSpecRevision: 1,
		CurrentGraphDigest:            "digest-a",
		AcceptanceRuns:                map[string]model.AcceptanceRun{},
		AcceptanceResults:             map[string]model.AcceptanceResult{},
	}
}

func addCircuitFailure(p *model.Plan, runID, resultID, fingerprint string, createdAt time.Time) {
	p.AcceptanceRuns[runID] = model.AcceptanceRun{
		ID: runID, PlanID: p.ID, SpecID: p.CurrentAcceptanceSpecID,
		SpecRevision: p.CurrentAcceptanceSpecRevision, TargetGraphDigest: p.CurrentGraphDigest,
		ResultID: resultID,
	}
	p.AcceptanceResults[resultID] = model.AcceptanceResult{
		ID: resultID, RunID: runID, PlanID: p.ID,
		Verdict: model.AcceptanceVerdictFail, FailureFingerprint: fingerprint,
		CreatedAt: createdAt,
	}
}

func TestLeadingExternalFactFailures(t *testing.T) {
	base := time.Date(2026, 7, 20, 23, 0, 0, 0, time.UTC)
	fp := externalFactFingerprintPrefix + "abc"

	t.Run("连续两次同指纹失败", func(t *testing.T) {
		p := newCircuitPlan()
		addCircuitFailure(p, "run-1", "res-1", fp, base)
		addCircuitFailure(p, "run-2", "res-2", fp, base.Add(time.Minute))
		gotFP, count := leadingExternalFactFailures(p)
		if count != 2 || gotFP != fp {
			t.Fatalf("leadingExternalFactFailures = (%q, %d), want (%q, 2)", gotFP, count, fp)
		}
	})

	t.Run("不同指纹打断连续计数", func(t *testing.T) {
		p := newCircuitPlan()
		addCircuitFailure(p, "run-1", "res-1", fp, base)
		addCircuitFailure(p, "run-2", "res-2", externalFactFingerprintPrefix+"other", base.Add(time.Minute))
		_, count := leadingExternalFactFailures(p)
		if count != 1 {
			t.Fatalf("count = %d, want 1 (only the latest fingerprint run counts)", count)
		}
	})

	t.Run("PASS 结果复位", func(t *testing.T) {
		p := newCircuitPlan()
		addCircuitFailure(p, "run-1", "res-1", fp, base)
		p.AcceptanceRuns["run-2"] = model.AcceptanceRun{
			ID: "run-2", PlanID: p.ID, SpecID: p.CurrentAcceptanceSpecID,
			SpecRevision: 1, TargetGraphDigest: p.CurrentGraphDigest, ResultID: "res-2",
		}
		p.AcceptanceResults["res-2"] = model.AcceptanceResult{
			ID: "res-2", RunID: "run-2", PlanID: p.ID,
			Verdict: model.AcceptanceVerdictPass, CreatedAt: base.Add(time.Minute),
		}
		if _, count := leadingExternalFactFailures(p); count != 0 {
			t.Fatalf("count = %d, want 0 after a pass", count)
		}
	})

	t.Run("Spec 变化后旧失败不计入", func(t *testing.T) {
		p := newCircuitPlan()
		addCircuitFailure(p, "run-1", "res-1", fp, base)
		addCircuitFailure(p, "run-2", "res-2", fp, base.Add(time.Minute))
		// 验收标准修订 → 旧 run 的 SpecRevision 不再匹配当前 epoch。
		p.CurrentAcceptanceSpecRevision = 2
		if _, count := leadingExternalFactFailures(p); count != 0 {
			t.Fatalf("count = %d, want 0 after spec epoch change", count)
		}
	})

	t.Run("图变化不复位 epoch", func(t *testing.T) {
		// 2026-07-21 事故：每次 ensure_acceptance_run 都改变 GraphDigest，
		// 若 digest 参与 epoch，验收重试循环里熔断在构造上永远无法触发。
		p := newCircuitPlan()
		addCircuitFailure(p, "run-1", "res-1", fp, base)
		p.CurrentGraphDigest = "digest-b" // 新 run 发布导致 digest 前进
		p.AcceptanceRuns["run-2"] = model.AcceptanceRun{
			ID: "run-2", PlanID: p.ID, SpecID: p.CurrentAcceptanceSpecID,
			SpecRevision: p.CurrentAcceptanceSpecRevision, TargetGraphDigest: "digest-b",
			ResultID: "res-2",
		}
		p.AcceptanceResults["res-2"] = model.AcceptanceResult{
			ID: "res-2", RunID: "run-2", PlanID: p.ID,
			Verdict: model.AcceptanceVerdictFail, FailureFingerprint: fp,
			CreatedAt: base.Add(time.Minute),
		}
		if _, count := leadingExternalFactFailures(p); count != 2 {
			t.Fatalf("count = %d, want 2 (digest 变化不复位 epoch)", count)
		}
	})

	t.Run("硬约束指纹同样计入", func(t *testing.T) {
		p := newCircuitPlan()
		addCircuitFailure(p, "run-1", "res-1", hardConstraintFingerprintPrefix+"xyz", base)
		addCircuitFailure(p, "run-2", "res-2", hardConstraintFingerprintPrefix+"xyz", base.Add(time.Minute))
		gotFP, count := leadingExternalFactFailures(p)
		if count != 2 || gotFP != hardConstraintFingerprintPrefix+"xyz" {
			t.Fatalf("leadingExternalFactFailures = (%q, %d), want hardc 指纹连续计数 2", gotFP, count)
		}
	})

	t.Run("非控制面指纹不计入", func(t *testing.T) {
		p := newCircuitPlan()
		addCircuitFailure(p, "run-1", "res-1", "submitter-fingerprint", base)
		if _, count := leadingExternalFactFailures(p); count != 0 {
			t.Fatalf("count = %d, want 0 for submitter-provided fingerprint", count)
		}
	})
}
