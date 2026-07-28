package agent

import (
	"errors"
	"reflect"
	"testing"

	"agentgo/internal/llm"
	"agentgo/internal/output"
)

func TestPublishCompletedTurnKeepsPublicTextAndToolNames(t *testing.T) {
	var events []output.Event
	a := &Agent{
		ID: "worker-1",
		StreamOutput: func(event output.Event) {
			events = append(events, event)
		},
	}
	a.publishCompletedTurn("turn-1", "task-1", 3, ExecuteResult{
		AssistantContent: "准备读取文件",
		ToolCalled:       true,
		ToolCalls: []llm.ToolCall{{
			ID: "call-1", Name: "read_file", Arguments: map[string]any{"path": "secret.txt"},
		}},
	}, nil, "流式备用文本")

	if len(events) != 1 {
		t.Fatalf("每次执行必须恰好发布一个完成轮次，实际为 %d", len(events))
	}
	got := events[0]
	if got.Kind != output.KindTurn || got.StreamID != "turn-1" || got.Loop != 3 || !got.Done {
		t.Fatalf("完成轮次元数据错误: %+v", got)
	}
	if got.Text != "准备读取文件" || !reflect.DeepEqual(got.ToolCalls, []string{"read_file"}) {
		t.Fatalf("公开正文或工具名错误: %+v", got)
	}
	if got.Text == "secret.txt" {
		t.Fatalf("轮次输出不应复制工具参数: %+v", got)
	}
}

func TestPublishCompletedTurnFallsBackToStreamOnFailure(t *testing.T) {
	var got output.Event
	a := &Agent{ID: "scheduler-1", StreamOutput: func(event output.Event) { got = event }}
	a.publishCompletedTurn("turn-2", "task-2", 4, ExecuteResult{}, errors.New("连接中断"), "已生成的部分正文")

	if got.Kind != output.KindTurn || got.Text != "已生成的部分正文" || got.Error != "连接中断" {
		t.Fatalf("失败轮次未保留部分公开正文: %+v", got)
	}
}

func TestPublishCompletedTurnUsesNaturalOutputForLegacyExecutor(t *testing.T) {
	var got output.Event
	a := &Agent{ID: "worker-1", StreamOutput: func(event output.Event) { got = event }}
	a.publishCompletedTurn("turn-3", "task-3", 1, ExecuteResult{
		Output: "自然完成正文",
	}, nil, "")

	if got.Text != "自然完成正文" {
		t.Fatalf("兼容 executor 的自然正文未保存: %+v", got)
	}
}
