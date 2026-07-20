package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agentgo/internal/ui"
)

// /plan 三形态：列表 / approve / reject 都走 Controller 的 plan_review
// 入口（Hub → bootstrap 的 approvePlanReview/rejectPlanReview），
// TUI 只负责参数解析与结果渲染。

func TestPlanList_RendersPendingReviews(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).planReviews = []ui.PlanReviewItem{
		{PlanID: "aaaa0000-1111", SubmittedAt: time.Now(), Excerpt: "# 计划\n1. 写 x.go"},
	}
	m := newAppModel(deps)

	m.handleCommand("/plan")

	got := lastMessageText(&m)
	if !strings.Contains(got, "aaaa0000") || !strings.Contains(got, "写 x.go") {
		t.Fatalf("列表未渲染 Plan 前缀与计划摘要: %q", got)
	}
	if !strings.Contains(got, "/plan approve") {
		t.Fatalf("列表应附带操作提示: %q", got)
	}
}

func TestPlanList_EmptyRendersHint(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	m.handleCommand("/plan")

	if got := lastMessageText(&m); !strings.Contains(got, "没有等待批准的计划") {
		t.Fatalf("空列表提示 = %q", got)
	}
}

func TestPlanApprove_GoesThroughController(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).approveFn = func(prefix string) (string, error) {
		return "已批准 Plan aaaa0000 的执行计划", nil
	}
	m := newAppModel(deps)

	m.handleCommand("/plan approve aaaa0000")

	f := fakeOf(deps)
	if len(f.approveCalls) != 1 || f.approveCalls[0] != "aaaa0000" {
		t.Fatalf("Controller.ApprovePlan 调用 = %+v", f.approveCalls)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "已批准") {
		t.Fatalf("成功消息未渲染摘要: %q", got)
	}
}

// 无前缀时传空串——"恰好一个待批准时默认选中"由 Hub 装配方判定。
func TestPlanApprove_NoPrefixPassesEmpty(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).approveFn = func(prefix string) (string, error) {
		return "已批准", nil
	}
	m := newAppModel(deps)

	m.handleCommand("/plan approve")

	f := fakeOf(deps)
	if len(f.approveCalls) != 1 || f.approveCalls[0] != "" {
		t.Fatalf("无前缀 approve 应传空串: %+v", f.approveCalls)
	}
}

func TestPlanReject_RendersError(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).rejectFn = func(prefix string) (string, error) {
		return "", errors.New("当前没有等待批准的计划")
	}
	m := newAppModel(deps)

	m.handleCommand("/plan reject")

	if got := lastMessageText(&m); !strings.Contains(got, "没有等待批准的计划") {
		t.Fatalf("失败消息未透出错误: %q", got)
	}
}

func TestPlanReject_GoesThroughController(t *testing.T) {
	deps := testDeps()
	fakeOf(deps).rejectFn = func(prefix string) (string, error) {
		return "已拒绝 Plan aaaa0000 的执行计划", nil
	}
	m := newAppModel(deps)

	m.handleCommand("/plan reject aaaa")

	f := fakeOf(deps)
	if len(f.rejectCalls) != 1 || f.rejectCalls[0] != "aaaa" {
		t.Fatalf("Controller.RejectPlan 调用 = %+v", f.rejectCalls)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "已拒绝") {
		t.Fatalf("成功消息未渲染摘要: %q", got)
	}
}

func TestPlan_UnknownSubcommandShowsUsage(t *testing.T) {
	deps := testDeps()
	m := newAppModel(deps)

	m.handleCommand("/plan frobnicate")

	if got := lastMessageText(&m); !strings.Contains(got, "用法") {
		t.Fatalf("未知子命令应输出用法: %q", got)
	}
}

func TestPlan_NoController(t *testing.T) {
	m := newAppModel(Deps{}) // Controller 未注入
	m.handleCommand("/plan")
	if got := lastMessageText(&m); !strings.Contains(got, "未初始化") {
		t.Fatalf("缺少 Controller 时应报错: %q", got)
	}
}
