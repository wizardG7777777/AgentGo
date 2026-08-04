package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agentgo/internal/interaction"
	"agentgo/internal/shell"
	"agentgo/internal/tools"
)

// resolveInteraction is the single trusted response router used by TUI/Web.
// BeginResolve provides first-writer-wins; the owning effect handler then
// commits before Complete makes the answer terminal.
//
// V6 C6a 已删除 Plan review/pause 的 Interaction 通道（plan_control handler
// 与暂停事实的 reconcile 循环）；剩余 handler 全部为服务端零
// effect：shell_command / file_write / agent_question / graph_approval。
func (s *System) resolveInteraction(ctx context.Context, input interaction.ResolveInput) (interaction.Request, error) {
	if s == nil || s.Interactions == nil {
		return interaction.Request{}, fmt.Errorf("interaction service is not initialized")
	}
	locked, err := s.Interactions.BeginResolve(ctx, input)
	if err != nil {
		return interaction.Request{}, err
	}
	if locked.State == interaction.StateResolved {
		return locked, nil
	}
	if locked.State != interaction.StateResolving {
		return interaction.Request{}, fmt.Errorf("%w: state=%s", interaction.ErrInvalidTransition, locked.State)
	}

	var effectErr error
	switch locked.Resolution.Handler {
	case shell.ResolutionHandlerShellCommand:
		// The waiting Shell adapter owns the captured command/filter effect. The
		// control plane only locks the answer and marks it ready for Await.
		effectErr = nil
	case tools.ResolutionHandlerFileWrite:
		// 与 shell_command 相同：服务端零 effect。strict 模式下写文件副作用由
		// 等待中的工具 wrapper（tools.FileWriteApprover）持有并在 Await 返回后
		// 复核绑定；控制面只锁定回答并 Complete。
		effectErr = nil
	case tools.ResolutionHandlerAgentResponse:
		// Ordinary Agent questions have no privileged control-plane effect. The
		// waiting tool receives the validated answer after Complete. Shell
		// authorization keeps its dedicated trusted handler.
		effectErr = nil
	case graphApprovalHandler:
		// 与 shell_command 相同：服务端零 effect。图审批的批准/拒绝事实由
		// graph approval 桥（graph_approval.go）经 Service 终态回调异步回填
		// Runtime.OnApprovalDecided；控制面只锁定回答并 Complete。
		effectErr = nil
	default:
		effectErr = fmt.Errorf("unknown interaction handler %q", locked.Resolution.Handler)
	}
	if effectErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		released, releaseErr := s.Interactions.Release(cleanupCtx, locked.ID, locked.Version, effectErr.Error())
		if releaseErr != nil {
			return interaction.Request{}, errors.Join(effectErr, releaseErr)
		}
		return released, effectErr
	}
	// The effect is already committed. Do not let a disconnected HTTP client or
	// closing TUI strand the request in resolving and leave a Shell waiter stuck.
	completeCtx, cancelComplete := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelComplete()
	completed, err := s.Interactions.Complete(completeCtx, locked.ID, locked.Version)
	if err != nil {
		return interaction.Request{}, err
	}
	return completed, nil
}

func (s *System) interruptPendingInteractions(reason string) {
	if s == nil || s.Interactions == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	requests, err := s.Interactions.List(ctx, interaction.Filter{States: []interaction.State{interaction.StatePending}})
	if err != nil {
		return
	}
	for _, request := range requests {
		_, _ = s.Interactions.Interrupt(ctx, request.ID, request.Version, reason)
	}
}
