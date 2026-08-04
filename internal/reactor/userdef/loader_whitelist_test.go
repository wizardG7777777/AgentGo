package userdef

import (
	"strings"
	"testing"
)

// D4：白名单倒挂修复的校验矩阵——
// 有真实发射点的活 Kind 必须能通过 `on:` 校验；
// 预留的死 Kind（shell_timeout_pending/resolved）必须报明确的"预留"错误；
// C6c 删除的 Plan 时代 Kind（plan_/replan_）必须报 unknown kind。
func TestLoad_WhitelistMatrix(t *testing.T) {
	dir := t.TempDir()
	writePrompt(t, dir, "p.md", "stub ${event.task.id}")

	// 有真实发射点、必须可订阅的 Kind
	validKinds := []string{
		"shell_executed", // D4 修复后由 run_shell 工具发射
		"task_published", // D4 修复后由 PublishTask 发射
		"task_blocked",   // 无可用路由超时后由 Store 系统终态发射
	}
	for _, kind := range validKinds {
		t.Run("valid/"+kind, func(t *testing.T) {
			yamlData := []byte(`
reactors:
  - on: ` + kind + `
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
			if _, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}}); err != nil {
				t.Fatalf("on: %s 应通过校验, got: %v", kind, err)
			}
		})
	}

	// 预留给未来内置 TimeoutHandler 的死 Kind：报错必须指明"预留"，
	// 不能混同为普通 unknown kind。
	reservedKinds := []string{
		"shell_timeout_pending",
		"shell_timeout_resolved",
	}
	for _, kind := range reservedKinds {
		t.Run("reserved/"+kind, func(t *testing.T) {
			yamlData := []byte(`
reactors:
  - on: ` + kind + `
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
			_, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}})
			if err == nil {
				t.Fatalf("on: %s 应校验失败（无发射点的预留 Kind）", kind)
			}
			if !strings.Contains(err.Error(), "reserved") || !strings.Contains(err.Error(), kind) {
				t.Fatalf("错误信息应含 reserved 与 kind 名, got: %v", err)
			}
		})
	}

	// C6c 删除的 Plan 时代 Kind：发射点与常量均已移除，订阅必须报 unknown kind。
	deletedKinds := []string{
		"replan_requested", "replan_coalesced", "replan_decided",
		"plan_revision_changed", "plan_paused", "plan_terminal", "acceptance_completed",
	}
	for _, kind := range deletedKinds {
		t.Run("deleted/"+kind, func(t *testing.T) {
			yamlData := []byte(`
reactors:
  - on: ` + kind + `
    publish_task:
      kind: explorer
      description: { file: ./p.md }
`)
			_, err := Load(yamlData, dir, dir, Deps{Store: &fakeStore{}})
			if err == nil {
				t.Fatalf("on: %s 应校验失败（C6c 已删除的 Plan 时代 Kind）", kind)
			}
			if !strings.Contains(err.Error(), "unknown event kind") {
				t.Fatalf("错误信息应为 unknown event kind, got: %v", err)
			}
		})
	}
}
