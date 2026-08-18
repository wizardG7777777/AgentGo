package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
)

func TestController_SendUserTextOrderAndEvent(t *testing.T) {
	var calls []string
	var gotEvent model.Event
	h := NewHub(Deps{
		RecordUserInput: func(text string) {
			calls = append(calls, "record:"+text)
		},
		SendUserEvent: func(ev model.Event) error {
			calls = append(calls, "send")
			gotEvent = ev
			return nil
		},
	})

	if err := h.SendUserText(context.Background(), "你好"); err != nil {
		t.Fatalf("SendUserText 返回错误：%v", err)
	}
	// 顺序必须为先记录元数据、后投递事件（tui sendUserText 语义）
	if len(calls) != 2 || calls[0] != "record:你好" || calls[1] != "send" {
		t.Fatalf("调用序列 = %v，期望 [record:你好 send]", calls)
	}
	if gotEvent.Type != model.EventUserInput {
		t.Fatalf("事件 Type = %v，期望 EventUserInput", gotEvent.Type)
	}
	if gotEvent.Payload["text"] != "你好" {
		t.Fatalf("Payload = %v", gotEvent.Payload)
	}
}

func TestController_SendUserTextRejectsSlashCommands(t *testing.T) {
	for _, input := range []string{"/help", "  /command args"} {
		t.Run(input, func(t *testing.T) {
			recorded := false
			sent := false
			h := NewHub(Deps{
				RecordUserInput: func(string) { recorded = true },
				SendUserEvent: func(model.Event) error {
					sent = true
					return nil
				},
			})

			err := h.SendUserText(context.Background(), input)
			if !errors.Is(err, ErrSlashCommandInput) {
				t.Fatalf("err = %v，期望 ErrSlashCommandInput", err)
			}
			if recorded || sent {
				t.Fatalf("斜杠命令触达普通输入通道：recorded=%v sent=%v", recorded, sent)
			}
		})
	}
}

func TestController_SendUserTextNotAssembled(t *testing.T) {
	// 全部缺失
	h := NewHub(Deps{})
	if err := h.SendUserText(context.Background(), "x"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
	// 只有 RecordUserInput：SendUserEvent 缺失时报错且不得调用 record
	recorded := false
	h = NewHub(Deps{RecordUserInput: func(string) { recorded = true }})
	if err := h.SendUserText(context.Background(), "x"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
	if recorded {
		t.Fatal("SendUserEvent 未装配时不应调用 RecordUserInput")
	}
	// ctx 已取消：直接返回 ctx 错误
	h = NewHub(Deps{
		RecordUserInput: func(string) {},
		SendUserEvent:   func(model.Event) error { return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.SendUserText(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v，期望 context.Canceled", err)
	}
}

func TestController_SteerAgentMessage(t *testing.T) {
	var got mailbox.Message
	h := NewHub(Deps{
		SteerSend: func(msg mailbox.Message) error {
			got = msg
			return nil
		},
	})

	before := time.Now()
	if err := h.SteerAgent("worker-1", "先跑测试再提交"); err != nil {
		t.Fatalf("SteerAgent 返回错误：%v", err)
	}
	if got.From != "user" {
		t.Fatalf("From = %q，期望 user", got.From)
	}
	if got.To != "worker-1" {
		t.Fatalf("To = %q", got.To)
	}
	if got.Content != "先跑测试再提交" || got.Summary != "先跑测试再提交" {
		t.Fatalf("Content/Summary = %q / %q", got.Content, got.Summary)
	}
	if got.Type != mailbox.MsgTypeSteer {
		t.Fatalf("Type = %q，期望 %q", got.Type, mailbox.MsgTypeSteer)
	}
	if got.Priority != mailbox.PriorityHigh {
		t.Fatalf("Priority = %q，期望 %q", got.Priority, mailbox.PriorityHigh)
	}
	if got.SentAt.Before(before) || got.SentAt.After(time.Now()) {
		t.Fatalf("SentAt = %v，不在调用时间窗口内", got.SentAt)
	}
}

func TestController_SteerAgentNotAssembled(t *testing.T) {
	h := NewHub(Deps{})
	if err := h.SteerAgent("a", "m"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}

func TestController_SetExecMode(t *testing.T) {
	var got []string
	h := NewHub(Deps{
		ExecModeSet: func(mode string) error { got = append(got, mode); return nil },
	})
	// 合法值（含大小写容错）解析后传规范化小写串。
	if err := h.SetExecMode("Readonly"); err != nil {
		t.Fatalf("SetExecMode(readonly) 返回错误：%v", err)
	}
	if len(got) != 1 || got[0] != "readonly" {
		t.Fatalf("ExecModeSet 收到 %v，期望 [readonly]", got)
	}

	// 非法值返回中文错误，且不触达注入函数。
	err := h.SetExecMode("nope")
	if err == nil || !strings.Contains(err.Error(), "未知执行权限模式") {
		t.Fatalf("err = %v，期望含 未知执行权限模式", err)
	}
	if len(got) != 1 {
		t.Fatalf("非法值不应触达 ExecModeSet，收到 %v", got)
	}

	// 未装配返回 ErrNotAssembled。
	if err := NewHub(Deps{}).SetExecMode("normal"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}

func TestController_SetTopoMode(t *testing.T) {
	var got []string
	h := NewHub(Deps{
		TopoModeSet: func(mode string) error { got = append(got, mode); return nil },
	})
	if err := h.SetTopoMode("Solo"); err != nil {
		t.Fatalf("SetTopoMode(solo) 返回错误：%v", err)
	}
	if len(got) != 1 || got[0] != "solo" {
		t.Fatalf("TopoModeSet 收到 %v，期望 [solo]", got)
	}

	err := h.SetTopoMode("nope")
	if err == nil || !strings.Contains(err.Error(), "未知编排拓扑模式") {
		t.Fatalf("err = %v，期望含 未知编排拓扑模式", err)
	}
	if len(got) != 1 {
		t.Fatalf("非法值不应触达 TopoModeSet，收到 %v", got)
	}

	if err := NewHub(Deps{}).SetTopoMode("team"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}

func TestSnapshot_TwoModeAxes(t *testing.T) {
	h := NewHub(Deps{
		ExecModeGet: func() string { return "readonly" },
		TopoModeGet: func() string { return "solo" },
	})
	h.refreshSnapshot()
	snap := h.Snapshot()
	if snap.ExecMode != "readonly" || snap.TopoMode != "solo" {
		t.Fatalf("快照两轴 = (%q, %q)，期望 (readonly, solo)",
			snap.ExecMode, snap.TopoMode)
	}

	// Getter 未装配时对应字段保持零值。
	h = NewHub(Deps{})
	h.refreshSnapshot()
	snap = h.Snapshot()
	if snap.ExecMode != "" || snap.TopoMode != "" {
		t.Fatalf("未装配的轴应为零值，实际 (%q, %q)", snap.ExecMode, snap.TopoMode)
	}
}

func TestController_CancelTask(t *testing.T) {
	var gotPrefix string
	h := NewHub(Deps{
		CancelTaskByPrefix: func(prefix string) (string, error) {
			gotPrefix = prefix
			return "task-12345678", nil
		},
	})
	id, err := h.CancelTask("task-12")
	if err != nil || id != "task-12345678" || gotPrefix != "task-12" {
		t.Fatalf("id=%q err=%v prefix=%q", id, err, gotPrefix)
	}

	// 错误透传
	wantErr := errors.New("未找到匹配任务")
	h = NewHub(Deps{CancelTaskByPrefix: func(string) (string, error) { return "", wantErr }})
	if _, err := h.CancelTask("zzz"); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v，期望透传 %v", err, wantErr)
	}

	// 未装配
	if _, err := NewHub(Deps{}).CancelTask("x"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}

func TestController_CancelLatestRequest(t *testing.T) {
	called := false
	h := NewHub(Deps{
		CancelLatestRequest: func() (string, error) {
			called = true
			return "已取消请求「写月度报表…」：共取消 2 个任务", nil
		},
	})
	summary, err := h.CancelLatestRequest()
	if err != nil || !called || summary != "已取消请求「写月度报表…」：共取消 2 个任务" {
		t.Fatalf("summary=%q err=%v called=%v", summary, err, called)
	}

	// ErrNoActiveRequest 原样透传，调用方可 errors.Is 判定
	h = NewHub(Deps{CancelLatestRequest: func() (string, error) { return "", ErrNoActiveRequest }})
	if _, err := h.CancelLatestRequest(); !errors.Is(err, ErrNoActiveRequest) {
		t.Fatalf("err = %v，期望透传 ErrNoActiveRequest", err)
	}

	// 未装配
	if _, err := NewHub(Deps{}).CancelLatestRequest(); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}

func TestController_Sessions(t *testing.T) {
	var switched string
	h := NewHub(Deps{
		SessionNew:      func() (string, error) { return "sess-new", nil },
		SessionNewForce: func() (string, error) { return "sess-force", nil },
		SessionSwitch:   func(id string) (bool, error) { switched = id; return true, nil },
		SessionList: func() ([]SessionInfo, error) {
			return []SessionInfo{{ID: "sess-1"}, {ID: "sess-2"}}, nil
		},
	})

	id, err := h.NewSession()
	if err != nil || id != "sess-new" {
		t.Fatalf("NewSession = %q, %v", id, err)
	}
	id, err = h.NewSessionForce()
	if err != nil || id != "sess-force" {
		t.Fatalf("NewSessionForce = %q, %v", id, err)
	}
	if changed, err := h.SwitchSession("sess-1"); err != nil || !changed || switched != "sess-1" {
		t.Fatalf("SwitchSession changed=%v err=%v switched=%q", changed, err, switched)
	}
	list, err := h.ListSessions()
	if err != nil || len(list) != 2 || list[0].ID != "sess-1" {
		t.Fatalf("ListSessions = %+v, %v", list, err)
	}

	// 未装配路径
	h = NewHub(Deps{})
	if _, err := h.NewSession(); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("NewSession err = %v", err)
	}
	if _, err := h.NewSessionForce(); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("NewSessionForce err = %v", err)
	}
	if _, err := h.SwitchSession("x"); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("SwitchSession err = %v", err)
	}
	if _, err := h.ListSessions(); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("ListSessions err = %v", err)
	}
}

func TestController_RequestQuit(t *testing.T) {
	quit := false
	h := NewHub(Deps{QuitFn: func() { quit = true }})
	h.RequestQuit()
	if !quit {
		t.Fatal("QuitFn 未被调用")
	}
	// 未装配时静默忽略，不得 panic
	NewHub(Deps{}).RequestQuit()
}

func TestController_EmitGraphEvent(t *testing.T) {
	var gotGraph, gotEvent string
	var gotData map[string]any
	h := NewHub(Deps{EmitGraphEvent: func(graphID, event string, data map[string]any) error {
		gotGraph, gotEvent, gotData = graphID, event, data
		return nil
	}})
	if err := h.EmitGraphEvent("g-1", "deploy.done", map[string]any{"ok": true}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if gotGraph != "g-1" || gotEvent != "deploy.done" || gotData["ok"] != true {
		t.Fatalf("委托入参错误: %q %q %v", gotGraph, gotEvent, gotData)
	}
	// 未装配返回 ErrNotAssembled。
	if err := NewHub(Deps{}).EmitGraphEvent("g-1", "e", nil); !errors.Is(err, ErrNotAssembled) {
		t.Fatalf("err = %v，期望 ErrNotAssembled", err)
	}
}
