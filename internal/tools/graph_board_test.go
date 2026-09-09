package tools

import (
	"agentgo/internal/graph"
	"sync"
)

// fakeGraphBoard 实现 graph.TaskBoard，记录收到的发布 spec（含幂等去重）。
type fakeGraphBoard struct {
	mu    sync.Mutex
	specs []graph.TaskSpec
	seq   int
}

func (b *fakeGraphBoard) PublishGraphTask(spec graph.TaskSpec) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.specs = append(b.specs, spec)
	return "task-" + spec.ActivationID, nil
}

func (b *fakeGraphBoard) LookupGraphTask(graphID, activationID, _ string) (graph.GraphTaskSnapshot, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, spec := range b.specs {
		if spec.GraphID == graphID && spec.ActivationID == activationID {
			return graph.GraphTaskSnapshot{TaskID: "task-" + spec.ActivationID, NodeKind: spec.NodeKind}, true, nil
		}
	}
	return graph.GraphTaskSnapshot{}, false, nil
}

func (b *fakeGraphBoard) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.specs)
}

func (b *fakeGraphBoard) last() graph.TaskSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.specs[len(b.specs)-1]
}
