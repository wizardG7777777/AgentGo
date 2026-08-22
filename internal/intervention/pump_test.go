package intervention

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentgo/internal/loopcontract"
	"agentgo/internal/loopstore"
	"agentgo/internal/runcontract"
)

type storeFake struct {
	commands []loopcontract.LoopInterventionRequested
	acks     []loopstore.InterventionAck
	ackErr   error
}

func (s *storeFake) PendingInterventionsForTask(taskID string) ([]loopcontract.LoopInterventionRequested, error) {
	var out []loopcontract.LoopInterventionRequested
	for _, command := range s.commands {
		if command.TaskID == taskID {
			out = append(out, command)
		}
	}
	return out, nil
}

func (s *storeFake) AckIntervention(_ string, ack loopstore.InterventionAck) error {
	if s.ackErr != nil {
		return s.ackErr
	}
	s.acks = append(s.acks, ack)
	return nil
}

func TestPumpEnsureTaskDoesNotAck(t *testing.T) {
	commands := []loopcontract.LoopInterventionRequested{validCommand("cmd-1"), validCommand("cmd-2")}
	store := &storeFake{commands: commands}
	pump := &Pump{
		Store: store, Consumer: "graph-runtime/v1", Now: func() time.Time { return time.Unix(100, 0) },
		Handler: HandlerFunc(func(_ context.Context, command loopcontract.LoopInterventionRequested) (string, error) {
			if command.CommandID == "cmd-2" {
				return "", errors.New("暂时不可路由")
			}
			return "graph-change:" + command.CommandID, nil
		}),
	}
	ensured, err := pump.EnsureTask(context.Background(), "task-1")
	if len(ensured) != 1 || err == nil || ensured[0].CommandID != "cmd-1" || len(store.acks) != 0 {
		t.Fatalf("Pump ensure/ack 边界错误: ensured=%+v err=%v acks=%+v", ensured, err, store.acks)
	}
}

func TestPumpAckIsExplicitAndDurable(t *testing.T) {
	store := &storeFake{commands: []loopcontract.LoopInterventionRequested{validCommand("cmd-1")}}
	pump := &Pump{Store: store, Consumer: "graph-runtime/v1", Now: func() time.Time { return time.Unix(200, 0) }}
	if err := pump.Ack(context.Background(), "task-1", "cmd-1", "outcome:decision-1"); err != nil {
		t.Fatal(err)
	}
	if len(store.acks) != 1 || store.acks[0].DecisionRef != "outcome:decision-1" {
		t.Fatalf("显式 Ack 未写入预期 durable ref: %+v", store.acks)
	}
	store.ackErr = errors.New("fsync failed")
	if err := pump.Ack(context.Background(), "task-1", "cmd-1", "outcome:decision-1"); err == nil {
		t.Fatal("Ack fsync 失败必须向上返回")
	}
}

func TestPumpEnsureTaskNeverHandlesOtherTask(t *testing.T) {
	first := validCommand("cmd-1")
	second := validCommand("cmd-2")
	second.TaskID = "task-2"
	seen := make([]string, 0, 1)
	pump := &Pump{Store: &storeFake{commands: []loopcontract.LoopInterventionRequested{first, second}}, Consumer: "test/v1",
		Handler: HandlerFunc(func(_ context.Context, command loopcontract.LoopInterventionRequested) (string, error) {
			seen = append(seen, command.CommandID)
			return "decision:" + command.CommandID, nil
		})}
	if _, err := pump.EnsureTask(context.Background(), "task-1"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "cmd-1" {
		t.Fatalf("定向 ensure 抢跑了其它 Task: %v", seen)
	}
}

func validCommand(id string) loopcontract.LoopInterventionRequested {
	return loopcontract.LoopInterventionRequested{
		Schema: loopcontract.InterventionSchemaV1, CommandID: id,
		RunID: "run-1", GraphID: "graph-1", NodeID: "work", ActivationID: "work@1",
		TaskID: "task-1", AttemptID: "attempt-1",
		Contract:   loopcontract.ProgressContractRef{ContractID: "progress:test/v1", PolicyRef: "test/v1", ContractDigest: "sha256:abcd"},
		ReasonCode: loopcontract.InterventionNoProgressStalled,
		BudgetUsed: runcontract.BudgetUsage{}, BudgetRemaining: runcontract.BudgetLimit{ModelCalls: 1},
		CheckpointRef: "checkpoint-1", RequestedAt: time.Unix(100, 0).UTC(),
	}
}
