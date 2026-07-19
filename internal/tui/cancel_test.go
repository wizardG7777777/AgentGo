package tui

import (
	"errors"
	"strings"
	"testing"
)

// D2：/cancel 不再自行 ScanAll + 转换状态，而是委托 Controller.CancelTask
// （Hub 装配的受守卫入口）；成功时保持原有"已取消任务 <短ID>"UX。
func TestCancelTask_DelegatesToControllerAndRendersSuccess(t *testing.T) {
	deps := testDeps()
	var gotPrefix string
	fakeOf(deps).cancelFn = func(idPrefix string) (string, error) {
		gotPrefix = idPrefix
		return "abcd1234-0000-4000-8000-000000000000", nil
	}
	m := newAppModel(deps)
	m.cancelTask("abcd")

	if gotPrefix != "abcd" {
		t.Fatalf("CancelTask 收到的前缀 = %q, want abcd", gotPrefix)
	}
	if got := lastMessageText(&m); !strings.Contains(got, "已取消任务 abcd1234") {
		t.Fatalf("成功消息应为 [cancel] 已取消任务 <短ID>: %q", got)
	}
}

// D2：Controller.CancelTask 返回的错误（未找到/歧义/plan 守卫拒绝）原样渲染给用户。
func TestCancelTask_RendersErrorsVerbatim(t *testing.T) {
	cases := map[string]string{
		"not-found": "未找到以 zzzz 开头的任务",
		"ambiguous": "找到 2 个匹配的任务，请使用更长的任务 ID 区分:\n  aaaa0000-x\n  aaaa1111-y",
		"guard":     "cancel_task 被拒绝：任务 x 由 Plan p 的控制器托管，外部调用方不能取消",
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			deps := testDeps()
			fakeOf(deps).cancelFn = func(string) (string, error) { return "", errors.New(msg) }
			m := newAppModel(deps)
			m.cancelTask("aaaa")

			if got := lastMessageText(&m); !strings.Contains(got, msg) {
				t.Fatalf("错误未原样渲染: got %q, want contains %q", got, msg)
			}
		})
	}
}

// D2：未装配 Controller 时命令报错而不是静默无操作。
func TestCancelTask_NilController(t *testing.T) {
	m := newAppModel(Deps{})
	m.cancelTask("abcd")
	if got := lastMessageText(&m); !strings.Contains(got, "未初始化") {
		t.Fatalf("缺少 Controller 时应报错: %q", got)
	}
}

// F8：shortID 对短 ID 不得 panic，原样返回；长 ID 取前 8 字符。
func TestShortID(t *testing.T) {
	if got := shortID("abc1"); got != "abc1" {
		t.Errorf("shortID(4 字符) = %q, want 原样返回", got)
	}
	if got := shortID(""); got != "" {
		t.Errorf("shortID(空) = %q, want 空", got)
	}
	if got := shortID("abcd1234-0000"); got != "abcd1234" {
		t.Errorf("shortID(长 ID) = %q, want abcd1234", got)
	}
}
