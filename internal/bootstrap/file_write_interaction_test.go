package bootstrap

import (
	"context"
	"strings"
	"testing"

	"agentgo/internal/interaction"
	"agentgo/internal/tools"
)

// file_write handler 与 shell_command 同为"服务端零 effect"路由：
// resolveInteraction 只锁定回答并 Complete，不触碰任何领域状态；
// 写文件副作用由等待中的工具 wrapper 持有。
func TestResolveInteraction_FileWriteHandlerZeroEffect(t *testing.T) {
	system := &System{
		Interactions: interaction.NewService(interaction.NewMemoryStore()),
		StatusCh:     make(chan string, 16),
	}
	ctx := context.Background()
	created, err := system.Interactions.Create(ctx, interaction.CreateRequest{
		Kind:    interaction.KindAuthorization,
		Purpose: tools.PurposeFileWrite,
		Prompt:  "Agent worker-1 请求写入文件（strict 执行模式，需人工批准）：\n工具: write_file\n路径: /repo/a.go",
		Options: []interaction.Option{
			{ID: "allow_once", Label: "仅允许本次", ActionRef: "allow_once"},
			{ID: "deny", Label: "拒绝", ActionRef: "deny"},
		},
		Origin:     interaction.Origin{Component: "file_write", AgentID: "worker-1", TaskID: "task-1"},
		Subject:    interaction.Subject{Kind: "file_write", ID: "/repo/a.go", TaskID: "task-1", Digest: "digest-1"},
		Resolution: interaction.ResolutionSpec{Handler: tools.ResolutionHandlerFileWrite, TargetID: "digest-1", AgentID: "worker-1", TaskID: "task-1"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resolved, err := system.resolveInteraction(ctx, interaction.ResolveInput{
		RequestID: created.ID, ExpectedVersion: created.Version, OptionID: "allow_once",
	})
	if err != nil {
		t.Fatalf("file_write handler 应零 effect 完成: %v", err)
	}
	if resolved.State != interaction.StateResolved {
		t.Fatalf("state = %s, want resolved", resolved.State)
	}
	// 零 effect 断言：请求只是被完成，不存在其他领域写入面可观测；
	// 同一回答幂等重试仍返回 resolved。
	again, err := system.resolveInteraction(ctx, interaction.ResolveInput{
		RequestID: created.ID, ExpectedVersion: resolved.Version, OptionID: "allow_once",
	})
	if err != nil || again.State != interaction.StateResolved {
		t.Fatalf("幂等重试: state=%s err=%v", again.State, err)
	}
}

// 未知 handler 仍然报错并 Release 回 pending（不静默吞掉路由缺口）。
func TestResolveInteraction_UnknownHandlerFails(t *testing.T) {
	system := &System{
		Interactions: interaction.NewService(interaction.NewMemoryStore()),
		StatusCh:     make(chan string, 16),
	}
	ctx := context.Background()
	created, err := system.Interactions.Create(ctx, interaction.CreateRequest{
		Kind:    interaction.KindAuthorization,
		Purpose: "probe_purpose",
		Prompt:  "未知 handler 探测",
		Options: []interaction.Option{
			{ID: "allow_once", Label: "仅允许本次", ActionRef: "allow_once"},
		},
		Resolution: interaction.ResolutionSpec{Handler: "no_such_handler"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = system.resolveInteraction(ctx, interaction.ResolveInput{
		RequestID: created.ID, ExpectedVersion: created.Version, OptionID: "allow_once",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown interaction handler") {
		t.Fatalf("未知 handler 应报错: %v", err)
	}
	// 可恢复错误走 Release：请求回到 pending，可被后续合法路径重新处理。
	current, getErr := system.Interactions.Get(ctx, created.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if current.State != interaction.StatePending {
		t.Fatalf("未知 handler 失败后应 Release 回 pending，state = %s", current.State)
	}
}
