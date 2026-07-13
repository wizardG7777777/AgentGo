package userdef

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"agentgo/internal/trace"
)

type capturedReplanRequest struct {
	event      trace.Event
	reasonCode string
	urgency    string
	detail     string
}

type fakeReplanRequester struct {
	mu       sync.Mutex
	calls    []capturedReplanRequest
	response string
	err      error
}

func (f *fakeReplanRequester) RequestReplanFromEvent(
	ev trace.Event,
	reasonCode, urgency, detail string,
) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, capturedReplanRequest{
		event:      ev,
		reasonCode: reasonCode,
		urgency:    urgency,
		detail:     detail,
	})
	return f.response, f.err
}

func (f *fakeReplanRequester) snapshot() []capturedReplanRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedReplanRequest(nil), f.calls...)
}

func TestRequestReplan_RuntimePassesRawEventAndLiteralConfig(t *testing.T) {
	requester := &fakeReplanRequester{response: "queued"}
	rs, err := Load([]byte(`
reactors:
  - name: replan-on-failure
    on: task_failed
    request_replan:
      reason_code: terminal_task_failed
      urgency: high
      detail: 'literal ${event.task.id}'
`), "", "", Deps{ReplanRequester: requester})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("len=%d want 1", len(rs))
	}
	if rs[0].Name() != "replan-on-failure" {
		t.Fatalf("Name=%q", rs[0].Name())
	}
	if rs[0].IsSync() {
		t.Fatal("request_replan user reactor must be async")
	}
	if rs[0].Priority() != 500 {
		t.Fatalf("Priority=%d want 500", rs[0].Priority())
	}
	if got := rs[0].Subscribe(); len(got) != 1 || got[0] != trace.KindTaskFailed {
		t.Fatalf("Subscribe=%v want [task_failed]", got)
	}

	ev := trace.Event{
		Kind:    trace.KindTaskFailed,
		TaskID:  "task-1",
		AgentID: "worker-2",
		Reason:  "tests failed",
		Args:    map[string]any{"attempt": 2},
		Transition: &trace.Transition{
			PrevStatus: "processing",
			NewStatus:  "failed",
			Cause:      "non_recoverable_error",
			RetryCount: 2,
		},
	}
	if err := rs[0].Run(ev); err != nil {
		t.Fatalf("Run: %v", err)
	}

	calls := requester.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls=%d want 1", len(calls))
	}
	got := calls[0]
	if !reflect.DeepEqual(got.event, ev) {
		t.Fatalf("event changed before requester call:\n got: %#v\nwant: %#v", got.event, ev)
	}
	if got.reasonCode != "terminal_task_failed" || got.urgency != "high" {
		t.Fatalf("reason/urgency=%q/%q", got.reasonCode, got.urgency)
	}
	if got.detail != `literal ${event.task.id}` {
		t.Fatalf("detail=%q; request_replan config must not be template-rendered", got.detail)
	}
}

func TestRequestReplan_WhenFiltersBeforeRequester(t *testing.T) {
	requester := &fakeReplanRequester{}
	rs, err := Load([]byte(`
reactors:
  - on: task_failed
    when: ${event.task.retry_count} >= 3
    request_replan:
      reason_code: retries_exhausted
      urgency: normal
`), "", "", Deps{ReplanRequester: requester})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, AttemptNo: 2}); err != nil {
		t.Fatalf("Run when=false: %v", err)
	}
	if got := len(requester.snapshot()); got != 0 {
		t.Fatalf("when=false requester calls=%d want 0", got)
	}

	if err := rs[0].Run(trace.Event{
		Kind:       trace.KindTaskFailed,
		Transition: &trace.Transition{RetryCount: 3},
	}); err != nil {
		t.Fatalf("Run when=true: %v", err)
	}
	if got := len(requester.snapshot()); got != 1 {
		t.Fatalf("when=true requester calls=%d want 1", got)
	}
}

func TestRequestReplan_RequesterErrorPropagates(t *testing.T) {
	requester := &fakeReplanRequester{err: errors.New("coordinator unavailable")}
	rs, err := Load([]byte(`
reactors:
  - name: replan-error
    on: task_failed
    request_replan:
      reason_code: task_failed
      urgency: high
`), "", "", Deps{ReplanRequester: requester})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = rs[0].Run(trace.Event{Kind: trace.KindTaskFailed, TaskID: "T"})
	if err == nil || !strings.Contains(err.Error(), "coordinator unavailable") {
		t.Fatalf("Run error=%v", err)
	}
	if !strings.Contains(err.Error(), "request_replan[replan-error]") {
		t.Fatalf("Run error should identify reactor, got %v", err)
	}
}

func TestRequestReplan_LoaderValidation(t *testing.T) {
	requester := &fakeReplanRequester{}
	tests := []struct {
		name    string
		action  string
		deps    Deps
		wantErr string
	}{
		{
			name:    "missing reason code",
			action:  "urgency: normal",
			deps:    Deps{ReplanRequester: requester},
			wantErr: "reason_code",
		},
		{
			name:    "missing urgency",
			action:  "reason_code: task_failed",
			deps:    Deps{ReplanRequester: requester},
			wantErr: "urgency",
		},
		{
			name:    "invalid urgency",
			action:  "reason_code: task_failed\n      urgency: critical",
			deps:    Deps{ReplanRequester: requester},
			wantErr: "normal/high",
		},
		{
			name:    "missing requester dependency",
			action:  "reason_code: task_failed\n      urgency: normal",
			deps:    Deps{},
			wantErr: "Deps.ReplanRequester",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yamlData := []byte("reactors:\n" +
				"  - on: task_failed\n" +
				"    request_replan:\n" +
				"      " + tt.action + "\n")
			_, err := Load(yamlData, "", "", tt.deps)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error=%v want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRequestReplan_RejectsAuthorityFieldsFromYAML(t *testing.T) {
	forbiddenFields := []string{
		"plan_id",
		"observed_revision",
		"observed_state_version",
		"idempotency_key",
	}
	for _, field := range forbiddenFields {
		t.Run(field, func(t *testing.T) {
			yamlData := []byte("reactors:\n" +
				"  - on: task_failed\n" +
				"    request_replan:\n" +
				"      reason_code: task_failed\n" +
				"      urgency: high\n" +
				"      " + field + ": forged\n")
			_, err := Load(yamlData, "", "", Deps{ReplanRequester: &fakeReplanRequester{}})
			if err == nil {
				t.Fatalf("Load accepted forbidden request_replan field %q", field)
			}
			if !strings.Contains(err.Error(), "unknown field") || !strings.Contains(err.Error(), field) {
				t.Fatalf("Load error=%v should identify forbidden field %q", err, field)
			}
		})
	}
}

func TestRequestReplan_IsMutuallyExclusiveWithOtherActions(t *testing.T) {
	_, err := Load([]byte(`
reactors:
  - on: task_failed
    request_replan:
      reason_code: task_failed
      urgency: normal
    call: send_message
    args:
      to: scheduler-1
      content: duplicate control path
`), "", "", Deps{
		ReplanRequester: &fakeReplanRequester{},
		Mailbox:         &fakeMailbox{},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one action") {
		t.Fatalf("Load error=%v; request_replan must be mutually exclusive", err)
	}
}
