// Package intervention 把 L4 durable LoopInterventionRequested 可靠投递给 L5。
// Handler 必须以 CommandID 幂等。物化协调决策与确认消费刻意分成两个阶段：
// EnsureTask 只确保 L5 coordination wake 存在，绝不提前 ack；只有该 wake 的
// durable TaskOutcome 已形成后，terminal adapter 才能调用 Ack。
package intervention

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
)

type Store interface {
	PendingInterventionsForTask(taskID string) ([]loopcontract.LoopInterventionRequested, error)
	AckIntervention(taskID string, ack loopstore.InterventionAck) error
}

type Handler interface {
	HandleLoopIntervention(context.Context, loopcontract.LoopInterventionRequested) (decisionRef string, err error)
}

type HandlerFunc func(context.Context, loopcontract.LoopInterventionRequested) (string, error)

func (f HandlerFunc) HandleLoopIntervention(ctx context.Context, command loopcontract.LoopInterventionRequested) (string, error) {
	return f(ctx, command)
}

type Pump struct {
	Store    Store
	Handler  Handler
	Consumer string
	Now      func() time.Time
}

// EnsuredDecision 是一次已幂等物化但尚未 ack 的 L5 协调决策。
type EnsuredDecision struct {
	CommandID    string
	SourceTaskID string
	DecisionRef  string
}

// EnsureTask 只处理指定 source Task 的 pending commands，禁止因一条 terminal
// event 全局 Drain 而抢跑其它 Task 尚未完成的 TaskOutcome/Graph settlement。
// 返回值只表示 Handler 已确保协调决策存在；本方法绝不 AckIntervention。
func (p *Pump) EnsureTask(ctx context.Context, taskID string) ([]EnsuredDecision, error) {
	if p == nil || p.Store == nil || p.Handler == nil || strings.TrimSpace(p.Consumer) == "" {
		return nil, fmt.Errorf("intervention pump 未完整装配")
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("intervention source task_id 不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	commands, err := p.Store.PendingInterventionsForTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("读取 task %s pending LoopIntervention: %w", taskID, err)
	}
	ensured := make([]EnsuredDecision, 0, len(commands))
	var errs []error
	for _, command := range commands {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := command.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("command %s 无效: %w", command.CommandID, err))
			continue
		}
		decisionRef, handleErr := p.Handler.HandleLoopIntervention(ctx, command)
		if handleErr != nil {
			errs = append(errs, fmt.Errorf("处理 command %s: %w", command.CommandID, handleErr))
			continue
		}
		decisionRef = strings.TrimSpace(decisionRef)
		if decisionRef == "" {
			errs = append(errs, fmt.Errorf("处理 command %s 未返回 decision_ref", command.CommandID))
			continue
		}
		ensured = append(ensured, EnsuredDecision{
			CommandID: command.CommandID, SourceTaskID: command.TaskID, DecisionRef: decisionRef,
		})
	}
	return ensured, errors.Join(errs...)
}

// Ack 只允许在 decisionRef 已有 durable 事实（生产桥使用 coordination wake
// 的 TaskOutcome ref）后调用。Ack 写失败时 command 保持 pending，禁止把它
// 计作已消费。
func (p *Pump) Ack(ctx context.Context, taskID, commandID, decisionRef string) error {
	if p == nil || p.Store == nil || strings.TrimSpace(p.Consumer) == "" {
		return fmt.Errorf("intervention pump 未完整装配")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	taskID = strings.TrimSpace(taskID)
	commandID = strings.TrimSpace(commandID)
	decisionRef = strings.TrimSpace(decisionRef)
	if taskID == "" || commandID == "" || decisionRef == "" {
		return fmt.Errorf("intervention ack 缺少 task_id/command_id/decision_ref")
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	ack := loopstore.InterventionAck{
		Schema: loopstore.InterventionAckSchemaV1, CommandID: commandID,
		Consumer: p.Consumer, DecisionRef: decisionRef, AckedAt: now().UTC(),
	}
	if err := p.Store.AckIntervention(taskID, ack); err != nil {
		return fmt.Errorf("ack command %s: %w", commandID, err)
	}
	return nil
}
