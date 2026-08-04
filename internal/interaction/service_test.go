package interaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testChoiceInput(id, sessionID string) CreateRequest {
	return CreateRequest{
		ID:            id,
		SessionID:     sessionID,
		Kind:          KindChoice,
		Purpose:       "plan_pause",
		Prompt:        "计划已暂停，请选择下一步",
		AllowFreeText: true,
		Origin:        Origin{Component: "scheduler", AgentID: "scheduler", TaskID: "controller-1"},
		Subject:       Subject{Kind: "plan", ID: "plan-1", Version: 7, Digest: "graph-abc"},
		Resolution:    ResolutionSpec{Handler: "plan_resume", TaskID: "controller-1"},
		Metadata: map[string]string{
			"execution_state_version": "7",
			"gate":                    "plan",
		},
		Options: []Option{
			{ID: "continue", Label: "继续", ActionRef: "plan:continue"},
			{ID: "converge", Label: "收敛", Description: "停止扩展并汇总", ActionRef: "plan:converge"},
			{ID: "guidance", Label: "补充指导", RequiresText: true, ActionRef: "plan:guidance"},
			{ID: "terminate", Label: "终止", ActionRef: "plan:terminate"},
		},
	}
}

func newTestService(t *testing.T) (*Service, Request) {
	t.Helper()
	fixed := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	service := NewService(NewMemoryStore(), WithClock(func() time.Time { return fixed }))
	request, err := service.Create(context.Background(), testChoiceInput("ix_test", "session-a"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return service, request
}

func TestServiceCreateDeepCopiesServerFields(t *testing.T) {
	fixed := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	input := testChoiceInput("ix_copy", "session-a")
	service := NewService(nil, WithClock(func() time.Time { return fixed }))
	created, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Version != 1 || created.State != StatePending {
		t.Fatalf("初始状态 = version %d, state %s", created.Version, created.State)
	}
	if !created.CreatedAt.Equal(fixed) || !created.UpdatedAt.Equal(fixed) {
		t.Fatalf("创建时间未使用注入时钟: %+v", created)
	}

	input.Options[0].ActionRef = "tampered-input"
	input.Metadata["gate"] = "tampered-input"
	created.Options[1].ActionRef = "tampered-output"
	created.Metadata["execution_state_version"] = "tampered-output"

	stored, err := service.Get(context.Background(), "ix_copy")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := stored.Options[0].ActionRef; got != "plan:continue" {
		t.Fatalf("Store ActionRef 被输入引用污染: %q", got)
	}
	if got := stored.Options[1].ActionRef; got != "plan:converge" {
		t.Fatalf("Store ActionRef 被输出引用污染: %q", got)
	}
	if got := stored.Metadata["gate"]; got != "plan" {
		t.Fatalf("Store Metadata 被输入引用污染: %q", got)
	}
	if got := stored.Metadata["execution_state_version"]; got != "7" {
		t.Fatalf("Store Metadata 被输出引用污染: %q", got)
	}
}

func TestCreateValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CreateRequest)
		want   error
	}{
		{name: "prompt empty", mutate: func(in *CreateRequest) { in.Prompt = " " }, want: ErrInvalidRequest},
		{name: "purpose unstable", mutate: func(in *CreateRequest) { in.Purpose = "Plan Pause" }, want: ErrInvalidRequest},
		{name: "handler missing", mutate: func(in *CreateRequest) { in.Resolution.Handler = "" }, want: ErrInvalidRequest},
		{name: "uppercase option id", mutate: func(in *CreateRequest) { in.Options[0].ID = "Continue" }, want: ErrInvalidOption},
		{name: "duplicate option id", mutate: func(in *CreateRequest) { in.Options[1].ID = in.Options[0].ID }, want: ErrInvalidOption},
		{name: "requires text not allowed", mutate: func(in *CreateRequest) { in.AllowFreeText = false }, want: ErrInvalidOption},
		{name: "choice options empty", mutate: func(in *CreateRequest) { in.Options = nil }, want: ErrInvalidRequest},
		{name: "text with options", mutate: func(in *CreateRequest) { in.Kind = KindText }, want: ErrInvalidRequest},
		{name: "unknown kind", mutate: func(in *CreateRequest) { in.Kind = "unknown" }, want: ErrInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testChoiceInput("ix_validation", "session-a")
			test.mutate(&input)
			_, err := NewService(nil).Create(context.Background(), input)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, test.want)
			}
		})
	}
}

func TestTextInteraction(t *testing.T) {
	service := NewService(nil)
	request, err := service.Create(context.Background(), CreateRequest{
		ID:            "ix_text",
		Kind:          KindText,
		Purpose:       "clarification",
		Prompt:        "请补充目标目录",
		AllowFreeText: true,
		Resolution:    ResolutionSpec{Handler: "scheduler_continuation"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("空文本 error = %v, want ErrInvalidRequest", err)
	}
	resolved, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, Text: "src/service", RespondedBy: "user",
	})
	if err != nil {
		t.Fatalf("BeginResolve: %v", err)
	}
	if resolved.Response == nil || resolved.Response.Text != "src/service" || resolved.Response.OptionID != "" {
		t.Fatalf("Response = %+v", resolved.Response)
	}
}

func TestBeginResolveTwoPhaseAndServerAction(t *testing.T) {
	service, request := newTestService(t)
	resolving, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "converge", RespondedBy: "local-user",
	})
	if err != nil {
		t.Fatalf("BeginResolve: %v", err)
	}
	if resolving.State != StateResolving || resolving.Version != 2 {
		t.Fatalf("BeginResolve = state %s version %d", resolving.State, resolving.Version)
	}
	if resolving.Response == nil || resolving.Response.OptionID != "converge" {
		t.Fatalf("Response = %+v", resolving.Response)
	}
	selected, ok := resolving.SelectedOption()
	if !ok || selected.ActionRef != "plan:converge" {
		t.Fatalf("SelectedOption = %+v, %v", selected, ok)
	}

	completed, err := service.Complete(context.Background(), request.ID, resolving.Version)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if completed.State != StateResolved || completed.Version != 3 {
		t.Fatalf("Complete = state %s version %d", completed.State, completed.Version)
	}
	// 使用旧 expectedVersion 重试相同 Complete 仍幂等返回当前事实。
	again, err := service.Complete(context.Background(), request.ID, resolving.Version)
	if err != nil || again.Version != completed.Version {
		t.Fatalf("幂等 Complete = %+v, %v", again, err)
	}
}

func TestRequiresTextAndFreeTextPolicy(t *testing.T) {
	service, request := newTestService(t)
	_, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "guidance",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("缺少必填指导文本 error = %v", err)
	}
	got, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "guidance", Text: "只检查核心包",
	})
	if err != nil {
		t.Fatalf("带文本 BeginResolve: %v", err)
	}
	if got.Response == nil || got.Response.Text != "只检查核心包" {
		t.Fatalf("Response = %+v", got.Response)
	}

	input := testChoiceInput("ix_no_text", "session-a")
	input.AllowFreeText = false
	input.Options = input.Options[:2]
	service2 := NewService(nil)
	request2, err := service2.Create(context.Background(), input)
	if err != nil {
		t.Fatalf("Create no-text: %v", err)
	}
	_, err = service2.BeginResolve(context.Background(), ResolveInput{
		RequestID: request2.ID, ExpectedVersion: request2.Version, OptionID: "continue", Text: "伪造文本",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("禁止自由文本 error = %v", err)
	}
}

func TestCASAndResponseIdempotency(t *testing.T) {
	service, request := newTestService(t)
	input := ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "continue", RespondedBy: "user",
	}
	first, err := service.BeginResolve(context.Background(), input)
	if err != nil {
		t.Fatalf("first BeginResolve: %v", err)
	}
	retry, err := service.BeginResolve(context.Background(), input)
	if err != nil || retry.Version != first.Version || retry.State != StateResolving {
		t.Fatalf("相同回答重试 = %+v, %v", retry, err)
	}
	_, err = service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "terminate", RespondedBy: "user",
	})
	if !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("不同回答 error = %v, want ErrAlreadyAnswered", err)
	}

	service2, request2 := newTestService(t)
	_, err = service2.BeginResolve(context.Background(), ResolveInput{
		RequestID: request2.ID, ExpectedVersion: 99, OptionID: "continue", RespondedBy: "user",
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("错误版本 error = %v, want ErrVersionConflict", err)
	}
}

func TestConcurrentFirstLegalResponseWins(t *testing.T) {
	service, request := newTestService(t)
	const goroutines = 64
	start := make(chan struct{})
	type result struct {
		option string
		req    Request
		err    error
	}
	results := make(chan result, goroutines)
	var wg sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		optionID := "continue"
		if index%2 == 1 {
			optionID = "terminate"
		}
		wg.Add(1)
		go func(option string) {
			defer wg.Done()
			<-start
			got, err := service.BeginResolve(context.Background(), ResolveInput{
				RequestID: request.ID, ExpectedVersion: request.Version,
				OptionID: option, RespondedBy: "user",
			})
			results <- result{option: option, req: got, err: err}
		}(optionID)
	}
	close(start)
	wg.Wait()
	close(results)

	final, err := service.Get(context.Background(), request.ID)
	if err != nil {
		t.Fatalf("Get final: %v", err)
	}
	if final.State != StateResolving || final.Version != 2 || final.Response == nil {
		t.Fatalf("final = %+v", final)
	}
	for result := range results {
		if result.option == final.Response.OptionID {
			if result.err != nil || result.req.Response == nil || result.req.Response.OptionID != result.option {
				t.Fatalf("获胜回答应幂等成功: %+v", result)
			}
			continue
		}
		if !errors.Is(result.err, ErrAlreadyAnswered) {
			t.Fatalf("落败回答 error = %v, want ErrAlreadyAnswered", result.err)
		}
	}
}

func TestReleaseReopensRequest(t *testing.T) {
	service, request := newTestService(t)
	first, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "continue",
	})
	if err != nil {
		t.Fatalf("BeginResolve: %v", err)
	}
	released, err := service.Release(context.Background(), request.ID, first.Version, "Plan 版本已变化，请重新选择")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if released.State != StatePending || released.Version != 3 || released.Response != nil {
		t.Fatalf("Release = %+v", released)
	}
	if released.StatusReason == "" {
		t.Fatal("Release 应保留重新开放原因")
	}
	second, err := service.BeginResolve(context.Background(), ResolveInput{
		RequestID: request.ID, ExpectedVersion: released.Version, OptionID: "terminate",
	})
	if err != nil || second.Response == nil || second.Response.OptionID != "terminate" {
		t.Fatalf("重新回答 = %+v, %v", second, err)
	}
}

func TestTerminalTransitions(t *testing.T) {
	tests := []struct {
		name  string
		state State
		apply func(*Service, Request) (Request, error)
		want  error
	}{
		{name: "cancel", state: StateCancelled, apply: func(s *Service, r Request) (Request, error) {
			return s.Cancel(context.Background(), r.ID, r.Version, "task cancelled")
		}, want: ErrCancelled},
		{name: "expire", state: StateExpired, apply: func(s *Service, r Request) (Request, error) {
			return s.Expire(context.Background(), r.ID, r.Version, "timeout")
		}, want: ErrExpired},
		{name: "fail", state: StateFailed, apply: func(s *Service, r Request) (Request, error) {
			return s.Fail(context.Background(), r.ID, r.Version, "handler failed")
		}, want: ErrFailed},
		{name: "interrupt", state: StateInterrupted, apply: func(s *Service, r Request) (Request, error) {
			return s.Interrupt(context.Background(), r.ID, r.Version, "shutdown")
		}, want: ErrInterrupted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, request := newTestService(t)
			got, err := test.apply(service, request)
			if err != nil {
				t.Fatalf("transition: %v", err)
			}
			if got.State != test.state || got.Version != request.Version+1 {
				t.Fatalf("state/version = %s/%d", got.State, got.Version)
			}
			awaited, awaitErr := service.Await(context.Background(), request.ID)
			if awaited.State != test.state || !errors.Is(awaitErr, test.want) {
				t.Fatalf("Await = state %s, err %v", awaited.State, awaitErr)
			}
			_, resolveErr := service.BeginResolve(context.Background(), ResolveInput{
				RequestID: request.ID, ExpectedVersion: got.Version, OptionID: "continue",
			})
			if !errors.Is(resolveErr, test.want) {
				t.Fatalf("BeginResolve terminal err = %v, want %v", resolveErr, test.want)
			}
		})
	}
}

func TestListPendingFiltersSession(t *testing.T) {
	service := NewService(nil)
	for index, sessionID := range []string{"session-a", "session-b", "session-a"} {
		input := testChoiceInput(fmt.Sprintf("ix_list_%d", index), sessionID)
		if _, err := service.Create(context.Background(), input); err != nil {
			t.Fatalf("Create %d: %v", index, err)
		}
	}
	request, err := service.Get(context.Background(), "ix_list_2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cancel(context.Background(), request.ID, request.Version, "cancelled"); err != nil {
		t.Fatal(err)
	}
	pending, err := service.ListPending(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "ix_list_0" {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestExpireDue(t *testing.T) {
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	service := NewService(nil, WithClock(func() time.Time { return base }))
	for index, expiry := range []time.Time{base.Add(time.Minute), base.Add(3 * time.Minute), base.Add(time.Minute)} {
		input := testChoiceInput(fmt.Sprintf("ix_due_%d", index), "session-a")
		input.ExpiresAt = expiry
		if _, err := service.Create(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	count, err := service.ExpireDue(context.Background(), "session-a", base.Add(2*time.Minute))
	if err != nil || count != 2 {
		t.Fatalf("ExpireDue = %d, %v", count, err)
	}
	pending, err := service.ListPending(context.Background(), "session-a")
	if err != nil || len(pending) != 1 || pending[0].ID != "ix_due_1" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

func TestGeneratedID(t *testing.T) {
	service := NewService(nil, WithIDGenerator(func() (string, error) { return "ix_generated", nil }))
	input := testChoiceInput("", "session-a")
	request, err := service.Create(context.Background(), input)
	if err != nil || request.ID != "ix_generated" {
		t.Fatalf("Create ID = %q, %v", request.ID, err)
	}
}

func TestCurrentSessionIDReadsProviderAtRequestTime(t *testing.T) {
	current := "session-a"
	service := NewService(nil, WithSessionIDProvider(func() string { return current }))
	if got := service.CurrentSessionID(); got != "session-a" {
		t.Fatalf("CurrentSessionID = %q", got)
	}
	current = "session-b"
	if got := service.CurrentSessionID(); got != "session-b" {
		t.Fatalf("CurrentSessionID after switch = %q", got)
	}
	var nilService *Service
	if got := nilService.CurrentSessionID(); got != "" {
		t.Fatalf("nil service CurrentSessionID = %q", got)
	}
}

// TestServiceOnResolved 终态回调挂点（C5c）：resolved/cancelled 等终态各触发
// 一次（深拷贝、含最终状态）；resolving→pending（Release）等非终态不触发；
// 未注册回调时迁移照常（nil 安全）；重复 Complete 幂等不重复触发。
func TestServiceOnResolved(t *testing.T) {
	service, request := newTestService(t)

	var mu sync.Mutex
	var fired []Request
	service.SetOnResolved(func(r Request) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, r)
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(fired)
	}

	ctx := context.Background()
	locked, err := service.BeginResolve(ctx, ResolveInput{RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "continue"})
	if err != nil {
		t.Fatalf("BeginResolve: %v", err)
	}
	if count() != 0 {
		t.Fatalf("resolving 不是终态，不应触发回调，实际 %d 次", count())
	}
	completed, err := service.Complete(ctx, locked.ID, locked.Version)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if count() != 1 {
		t.Fatalf("resolved 应触发 1 次回调，实际 %d", count())
	}
	mu.Lock()
	got := fired[0]
	mu.Unlock()
	if got.State != StateResolved || got.ID != request.ID || got.Response == nil || got.Response.OptionID != "continue" {
		t.Errorf("回调应收到 resolved 终态深拷贝（含 Response）: %+v", got)
	}
	// 深拷贝隔离：改动回调收到的副本不污染 Store。
	got.Response.OptionID = "tampered"
	latest, err := service.Get(ctx, request.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if latest.Response == nil || latest.Response.OptionID != "continue" {
		t.Errorf("回调副本不应污染 Store: %+v", latest.Response)
	}
	// 重复 Complete 幂等返回，不重复触发。
	if _, err := service.Complete(ctx, locked.ID, locked.Version); err != nil {
		t.Fatalf("重复 Complete 应幂等: %v", err)
	}
	if count() != 1 {
		t.Errorf("幂等 Complete 不应重复触发回调，实际 %d 次", count())
	}
	_ = completed

	// cancelled 终态同样触发（另一个请求）。
	other, err := service.Create(ctx, testChoiceInput("ix_cancel", "session-a"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.Cancel(ctx, other.ID, other.Version, "测试取消"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if count() != 2 {
		t.Fatalf("cancelled 应触发回调，实际共 %d 次", count())
	}
	mu.Lock()
	if fired[1].State != StateCancelled || fired[1].StatusReason != "测试取消" {
		t.Errorf("cancelled 回调应载明状态与原因: %+v", fired[1])
	}
	mu.Unlock()

	// Release（resolving→pending）不是终态，不触发。
	third, err := service.Create(ctx, testChoiceInput("ix_release", "session-a"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	locked3, err := service.BeginResolve(ctx, ResolveInput{RequestID: third.ID, ExpectedVersion: third.Version, OptionID: "continue"})
	if err != nil {
		t.Fatalf("BeginResolve: %v", err)
	}
	if _, err := service.Release(ctx, locked3.ID, locked3.Version, "可恢复失败"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if count() != 2 {
		t.Errorf("Release 回 pending 不应触发回调，实际共 %d 次", count())
	}

	// SetOnResolved(nil) 清除后不再触发；nil 安全（未注册时迁移照常）。
	service.SetOnResolved(nil)
	if _, err := service.Fail(ctx, third.ID, 3, "不可恢复"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if count() != 2 {
		t.Errorf("清除回调后不应再触发，实际共 %d 次", count())
	}
}

// TestServiceOnResolvedNilSafe 未注册回调时全部终态迁移照常工作。
func TestServiceOnResolvedNilSafe(t *testing.T) {
	service, request := newTestService(t)
	ctx := context.Background()
	locked, err := service.BeginResolve(ctx, ResolveInput{RequestID: request.ID, ExpectedVersion: request.Version, OptionID: "continue"})
	if err != nil {
		t.Fatalf("BeginResolve: %v", err)
	}
	if _, err := service.Complete(ctx, locked.ID, locked.Version); err != nil {
		t.Fatalf("未注册回调时 Complete 应照常成功: %v", err)
	}
}
