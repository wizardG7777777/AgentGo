package ui

import (
	"errors"
	"testing"
	"time"
)

// plan_review 三入口（/plan 命令的后端）：Hub 只做 nil 守卫与透传，
// 前缀解析 / Plan 状态变更由 bootstrap 装配方完成。

func TestController_ApprovePlan(t *testing.T) {
	var gotPrefix string
	h := NewHub(Deps{
		ApprovePlanReview: func(prefix string) (string, error) {
			gotPrefix = prefix
			return "已批准 Plan aaaa0000 的执行计划", nil
		},
	})
	summary, err := h.ApprovePlan("aaaa")
	if err != nil || gotPrefix != "aaaa" || summary == "" {
		t.Fatalf("summary=%q err=%v prefix=%q", summary, err, gotPrefix)
	}

	// 错误透传（如歧义前缀 / 无待批准）。
	wantErr := errors.New("当前没有等待批准的计划")
	h = NewHub(Deps{ApprovePlanReview: func(string) (string, error) { return "", wantErr }})
	if _, err := h.ApprovePlan(""); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v，期望透传 %v", err, wantErr)
	}

	// 未装配
	if _, err := NewHub(Deps{}).ApprovePlan(""); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}

func TestController_RejectPlan(t *testing.T) {
	var gotPrefix string
	h := NewHub(Deps{
		RejectPlanReview: func(prefix string) (string, error) {
			gotPrefix = prefix
			return "已拒绝 Plan aaaa0000 的执行计划", nil
		},
	})
	summary, err := h.RejectPlan("")
	if err != nil || gotPrefix != "" || summary == "" {
		t.Fatalf("summary=%q err=%v prefix=%q", summary, err, gotPrefix)
	}

	// 未装配
	if _, err := NewHub(Deps{}).RejectPlan("x"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}

func TestController_PendingPlanReviews(t *testing.T) {
	want := []PlanReviewItem{
		{PlanID: "aaaa0000-1111", SubmittedAt: time.Now(), Excerpt: "# 计划"},
	}
	h := NewHub(Deps{
		PendingPlanReviews: func() ([]PlanReviewItem, error) { return want, nil },
	})
	items, err := h.PendingPlanReviews()
	if err != nil || len(items) != 1 || items[0].PlanID != "aaaa0000-1111" {
		t.Fatalf("items=%+v err=%v", items, err)
	}

	// 未装配
	if _, err := NewHub(Deps{}).PendingPlanReviews(); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}
