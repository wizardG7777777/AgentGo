package bootstrap

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/model"
	"agentgo/internal/store"
)

// D2：TUI /cancel 的前缀解析闭包——歧义报错且不取消任何任务。
func TestCancelTaskByPrefix_AmbiguousListsCandidatesAndCancelsNothing(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	idA := "abcd1234-0000-4000-8000-00000000000a"
	idB := "abcd1234-1111-4000-8000-00000000000b"
	for _, id := range []string{idA, idB} {
		if err := s.PublishTask(&model.Task{ID: id, Description: "t"}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := cancelTaskByPrefix(context.Background(), s, nil, "abcd")
	if err == nil {
		t.Fatal("歧义前缀应报错")
	}
	if !strings.Contains(err.Error(), idA) || !strings.Contains(err.Error(), idB) {
		t.Fatalf("歧义错误应列出候选 ID: %v", err)
	}
	for _, id := range []string{idA, idB} {
		got, getErr := s.GetTask(id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != model.TaskStatusPending {
			t.Fatalf("歧义时不应取消任何任务: %s -> %s", id, got.Status)
		}
	}
}

// D2：零匹配报"未找到"。
func TestCancelTaskByPrefix_NotFound(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	if err := s.PublishTask(&model.Task{ID: "abcd1234-0000-4000-8000-00000000000a", Description: "t"}); err != nil {
		t.Fatal(err)
	}
	_, err := cancelTaskByPrefix(context.Background(), s, nil, "zzzz9999")
	if err == nil || !strings.Contains(err.Error(), "未找到") {
		t.Fatalf("应报未找到: %v", err)
	}
}

// D2：短于 4 字符的前缀直接报错，不做猜测（任务保持 pending）。
func TestCancelTaskByPrefix_ShortPrefixRejected(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	id := "abcd1234-0000-4000-8000-00000000000a"
	if err := s.PublishTask(&model.Task{ID: id, Description: "t"}); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"", "a", "ab", "abc"} {
		if _, err := cancelTaskByPrefix(context.Background(), s, nil, prefix); err == nil ||
			!strings.Contains(err.Error(), "前缀过短") {
			t.Fatalf("前缀 %q 应报过短: %v", prefix, err)
		}
	}
	got, getErr := s.GetTask(id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("短前缀不应取消任务: %s", got.Status)
	}
}

// D2：恰好一个匹配时经 GuardedCancel 取消，来源记为 "user"。
func TestCancelTaskByPrefix_SingleMatchCancelsWithUserSource(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	reg := store.NewTaskCancelRegistry()
	s.SetCancelRegistry(reg)
	id := "abcd1234-0000-4000-8000-00000000000a"
	if err := s.PublishTask(&model.Task{ID: id, Description: "t"}); err != nil {
		t.Fatal(err)
	}
	reg.GetOrCreate(context.Background(), id)

	gotID, err := cancelTaskByPrefix(context.Background(), s, nil, "abcd1234")
	if err != nil {
		t.Fatalf("唯一匹配应取消成功: %v", err)
	}
	if gotID != id {
		t.Fatalf("返回任务 ID = %q, want %q", gotID, id)
	}
	got, getErr := s.GetTask(id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusCancelled {
		t.Fatalf("状态 = %s, want cancelled", got.Status)
	}
	if src := reg.Source(id); src != "user" {
		t.Fatalf("取消来源 = %q, want user", src)
	}
}

// D2：唯一匹配但任务归属 Plan 时，守卫拒绝（与 LLM cancel_task 同路径）。
func TestCancelTaskByPrefix_PlanOwnedTaskRefused(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	id := "abcd1234-0000-4000-8000-00000000000a"
	if err := s.PublishTask(&model.Task{ID: id, Description: "t", PlanID: "plan-x"}); err != nil {
		t.Fatal(err)
	}

	_, err := cancelTaskByPrefix(context.Background(), s, nil, "abcd1234")
	if err == nil || !strings.Contains(err.Error(), "cancel_task 被拒绝") {
		t.Fatalf("Plan 托管任务应被守卫拒绝: %v", err)
	}
	got, getErr := s.GetTask(id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != model.TaskStatusPending {
		t.Fatalf("被拒绝后任务状态不应改变: %s", got.Status)
	}
}
