package ui

import (
	"agentgo/internal/contextruntime"
	"agentgo/internal/output"
)

// ModelOutputTurns 是 UI 的纯展示投影，不产生游标或持久化记录。
func ModelOutputTurns(records []contextruntime.OutputRecord) []AgentTurn {
	out := make([]AgentTurn, 0, len(records))
	for _, r := range records {
		out = append(out, AgentTurn{ID: r.Identity.InvocationID, SessionID: r.Identity.SessionID, AgentID: r.Identity.AgentID, TaskID: r.Identity.TaskID, Loop: r.Identity.Loop, Text: r.Text, Reasoning: r.Reasoning, Status: r.Status, Error: r.Error, StartedAt: r.StartedAt, CompletedAt: r.CompletedAt, ToolCalls: r.ToolCalls})
	}
	return out
}

// ModelOutputView 只保存当前消费者的渲染状态，唯一数据来源是 L2 订阅。
type ModelOutputView struct{ Records []contextruntime.OutputRecord }

func (v *ModelOutputView) Apply(e contextruntime.OutputEvent) (output.Event, bool) {
	if e.Kind == "snapshot" || e.Kind == "resync" {
		v.Records = e.Snapshot
		return output.Event{}, false
	}
	index := -1
	for i := range v.Records {
		if v.Records[i].Identity.InvocationID == e.Identity.InvocationID {
			index = i
			break
		}
	}
	if e.Record != nil {
		if index < 0 {
			v.Records = append(v.Records, *e.Record)
			index = len(v.Records) - 1
		} else {
			v.Records[index] = *e.Record
		}
	}
	if index < 0 {
		return output.Event{}, false
	}
	r := &v.Records[index]
	if e.Delta != nil {
		switch e.Delta.Kind {
		case "text_delta":
			r.Text += e.Delta.Delta
		case "reasoning_delta":
			r.Reasoning += e.Delta.Delta
		}
	}
	kind := output.KindStream
	if e.Kind == "finished" {
		kind = output.KindTurn
	}
	return output.Event{Kind: kind, StreamID: r.Identity.InvocationID, SessionID: r.Identity.SessionID, AgentID: r.Identity.AgentID, TaskID: r.Identity.TaskID, Loop: r.Identity.Loop, Text: r.Text, Reasoning: r.Reasoning, Done: e.Kind == "finished", Error: r.Error, ToolCalls: r.ToolCalls}, true
}
