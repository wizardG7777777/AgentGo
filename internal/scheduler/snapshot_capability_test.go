package scheduler

// snapshot_capability_test.go 验证 board snapshot 任务条目对 per-node 节点能力
// （Task.Capability）的投影：Scheduler 重规划时能看到自己此前设过什么。

import (
	"strings"
	"testing"

	"agentgo/internal/config"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestBuildBoardJSON_TaskCapabilityProjection(t *testing.T) {
	ch := make(chan model.Event, 64)
	s := store.NewMemoryTaskStore(ch, 100, 2, 300)
	cfg := &config.Config{Agents: []config.AgentKind{{Kind: "worker", Replicas: 1}}}

	capped := &model.Task{Description: "capped node",
		Capability: &model.NodeCapability{Tools: []string{"read_file", "web_fetch"}, Model: "deepseek-r1"}}
	if err := s.PublishTask(capped); err != nil {
		t.Fatalf("publish capped: %v", err)
	}
	plain := &model.Task{Description: "plain node"}
	if err := s.PublishTask(plain); err != nil {
		t.Fatalf("publish plain: %v", err)
	}

	out := BuildBoardJSON(s, cfg, testModeSnap(), model.Event{Type: model.EventTickerWakeup}, SnapshotSources{})

	// 带 capability 的任务：tools 全量直放（量小，不做有界摘录）+ model 覆盖。
	if !strings.Contains(out, `"capability"`) {
		t.Fatalf("board snapshot 应含 capability 段，got: %s", out)
	}
	if !strings.Contains(out, `"read_file"`) || !strings.Contains(out, `"web_fetch"`) ||
		!strings.Contains(out, `"deepseek-r1"`) {
		t.Errorf("capability 投影内容不完整，got: %s", out)
	}

	// 对照：无 capability 的任务条目不得输出 capability 字段（omitempty）。
	// 全板只有 1 个任务声明了能力，capability 键应恰好出现 1 次。
	if n := strings.Count(out, `"capability"`); n != 1 {
		t.Errorf("capability 键出现 %d 次，want 1（仅声明任务）", n)
	}
}
