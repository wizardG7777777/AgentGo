package contextruntime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/policycatalog"
	"github.com/google/uuid"
)

// OutputRecord 同时保存完整结果和失败展示，不将半截文本当作可执行响应。
type OutputRecord struct {
	ToolCalls   []string     `json:"tool_calls,omitempty"`
	Schema      string       `json:"schema"`
	Identity    llm.Identity `json:"identity"`
	Result      *llm.Result  `json:"result,omitempty"`
	Text        string       `json:"text"`
	Reasoning   string       `json:"reasoning,omitempty"`
	Status      string       `json:"status"`
	Error       string       `json:"error,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	CompletedAt time.Time    `json:"completed_at,omitempty"`
}
type OutputEvent struct {
	EventCursor string         `json:"eventCursor"`
	Kind        string         `json:"kind"`
	Identity    llm.Identity   `json:"identity"`
	Delta       *llm.Event     `json:"delta,omitempty"`
	Record      *OutputRecord  `json:"record,omitempty"`
	Snapshot    []OutputRecord `json:"snapshot,omitempty"`
}
type WatchOptions struct {
	SessionID   string
	AgentID     string
	EventCursor string
	Buffer      int
}
type outputCursor struct {
	Epoch    string
	Scope    string
	Sequence uint64
}
type outputSubscriber struct {
	options WatchOptions
	channel chan OutputEvent
}
type storedOutputEvent struct {
	sequence uint64
	event    OutputEvent
	bytes    int
}
type OutputService struct {
	mu       sync.Mutex
	epoch    string
	sequence uint64
	records  map[string]OutputRecord
	order    []string
	events   []storedOutputEvent
	bytes    int
	subs     map[uint64]outputSubscriber
	nextSub  uint64
	persist  func(OutputRecord) error
}

func NewOutputService(persist func(OutputRecord) error) *OutputService {
	return &OutputService{epoch: uuid.NewString(), records: map[string]OutputRecord{}, subs: map[uint64]outputSubscriber{}, persist: persist}
}
func outputMatches(o WatchOptions, id llm.Identity) bool {
	return (o.SessionID == "" || o.SessionID == id.SessionID) && (o.AgentID == "" || o.AgentID == id.AgentID)
}
func outputScope(o WatchOptions) string {
	raw, _ := json.Marshal([]string{o.SessionID, o.AgentID})
	return string(raw)
}
func (s *OutputService) cursor(o WatchOptions) string {
	raw, _ := json.Marshal(outputCursor{Epoch: s.epoch, Scope: outputScope(o), Sequence: s.sequence})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func cloneOutput[T any](v T) T {
	raw, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(raw, &out)
	return out
}
func (s *OutputService) snapshot(o WatchOptions) []OutputRecord {
	var out []OutputRecord
	for _, id := range s.order {
		r := s.records[id]
		if outputMatches(o, r.Identity) {
			v := cloneOutput(r)
			v.Result = nil
			out = append(out, v)
		}
	}
	return out
}

func (s *OutputService) WatchModelOutput(o WatchOptions) (<-chan OutputEvent, func(), error) {
	if o.Buffer < 1 {
		o.Buffer = policycatalog.OutputDefaultSubscriberBuffer
	}
	if o.Buffer > policycatalog.OutputReplayMaxEvents {
		o.Buffer = policycatalog.OutputReplayMaxEvents
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan OutputEvent, o.Buffer)
	reset := true
	if o.EventCursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(o.EventCursor)
		var c outputCursor
		if err != nil || json.Unmarshal(raw, &c) != nil {
			return nil, nil, fmt.Errorf("流式事件游标无效")
		}
		if c.Scope != outputScope(o) {
			return nil, nil, fmt.Errorf("流式事件游标订阅范围不匹配")
		}
		if c.Epoch == s.epoch && c.Sequence <= s.sequence && (len(s.events) == 0 && c.Sequence == s.sequence || len(s.events) > 0 && c.Sequence+1 >= s.events[0].sequence) {
			var replay []OutputEvent
			for _, e := range s.events {
				if e.sequence > c.Sequence && outputMatches(o, e.event.Identity) {
					v := cloneOutput(e.event)
					raw, _ := json.Marshal(outputCursor{Epoch: s.epoch, Scope: outputScope(o), Sequence: e.sequence})
					v.EventCursor = base64.RawURLEncoding.EncodeToString(raw)
					replay = append(replay, v)
				}
			}
			if len(replay) <= cap(ch) {
				for _, e := range replay {
					ch <- e
				}
				reset = false
			}
		}
	}
	if reset {
		kind := "snapshot"
		if o.EventCursor != "" {
			kind = "resync"
		}
		ch <- OutputEvent{Kind: kind, EventCursor: s.cursor(o), Snapshot: s.snapshot(o)}
	}
	s.nextSub++
	id := s.nextSub
	s.subs[id] = outputSubscriber{options: o, channel: ch}
	var once sync.Once
	cancel := func() { once.Do(func() { s.mu.Lock(); defer s.mu.Unlock(); delete(s.subs, id); close(ch) }) }
	return ch, cancel, nil
}

func (s *OutputService) publish(e OutputEvent) {
	if e.Record != nil {
		v := cloneOutput(*e.Record)
		v.Result = nil
		e.Record = &v
	}
	s.sequence++
	raw, _ := json.Marshal(e)
	s.events = append(s.events, storedOutputEvent{sequence: s.sequence, event: cloneOutput(e), bytes: len(raw)})
	s.bytes += len(raw)
	for len(s.events) > policycatalog.OutputReplayMaxEvents || s.bytes > policycatalog.OutputReplayMaxBytes {
		s.bytes -= s.events[0].bytes
		s.events = s.events[1:]
	}
	for _, sub := range s.subs {
		if !outputMatches(sub.options, e.Identity) {
			continue
		}
		v := cloneOutput(e)
		v.EventCursor = s.cursor(sub.options)
		select {
		case sub.channel <- v:
		default:
			for len(sub.channel) > 0 {
				<-sub.channel
			}
			sub.channel <- OutputEvent{Kind: "resync", EventCursor: s.cursor(sub.options), Snapshot: s.snapshot(sub.options)}
		}
	}
}
func (s *OutputService) Start(id llm.Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[id.InvocationID]; ok {
		return
	}
	r := OutputRecord{Schema: "agentgo.model-output/v1", Identity: id, Status: "streaming", StartedAt: time.Now()}
	s.records[id.InvocationID] = r
	s.order = append(s.order, id.InvocationID)
	s.publish(OutputEvent{Kind: "started", Identity: id, Record: &r})
}
func (s *OutputService) Accept(e llm.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[e.InvocationID]
	if !ok || r.Status != "streaming" {
		return
	}
	switch e.Kind {
	case "text_delta":
		r.Text += e.Delta
	case "reasoning_delta":
		r.Reasoning += e.Delta
	}
	s.records[e.InvocationID] = r
	s.publish(OutputEvent{Kind: "delta", Identity: r.Identity, Delta: &e})
}
func (s *OutputService) Finish(id llm.Identity, result llm.Result, callErr error) error {
	s.mu.Lock()
	r, ok := s.records[id.InvocationID]
	if !ok || r.Status != "streaming" {
		s.mu.Unlock()
		return fmt.Errorf("模型输出终态重复或缺少开始事件")
	}
	r.Status = "settling"
	s.records[id.InvocationID] = r
	s.mu.Unlock()
	r.CompletedAt = time.Now()
	r.Status = "completed"
	if callErr != nil {
		r.Status = "failed"
		r.Error = callErr.Error()
	} else {
		r.Result = &result
		for _, call := range result.ToolCalls() {
			r.ToolCalls = append(r.ToolCalls, call.Name)
		}
		r.Text = result.Content()
		r.Reasoning = result.Reasoning()
	}
	var saveErr error
	if s.persist != nil {
		saveErr = s.persist(r)
		if saveErr != nil {
			r.Status = "failed"
			r.Error = "模型输出持久化失败: " + saveErr.Error()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[id.InvocationID] = r
	s.publish(OutputEvent{Kind: "finished", Identity: id, Record: &r})
	for len(s.order) > policycatalog.OutputRecentRecords {
		old := s.order[0]
		if s.records[old].Status == "streaming" || s.records[old].Status == "settling" {
			break
		}
		s.order = s.order[1:]
		delete(s.records, old)
	}
	return saveErr
}

// Restore 只恢复新格式的完整记录，事件游标在新进程中重新开始。
func (s *OutputService) Restore(records []OutputRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		if r.Schema != "agentgo.model-output/v1" || r.Status == "streaming" {
			return fmt.Errorf("拒绝旧格式或未完成输出记录")
		}
		if _, ok := s.records[r.Identity.InvocationID]; ok {
			continue
		}
		s.records[r.Identity.InvocationID] = cloneOutput(r)
		s.order = append(s.order, r.Identity.InvocationID)
	}
	return nil
}

func (s *OutputService) Snapshot(options WatchOptions) []OutputRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(options)
}

func (s *OutputService) HasRecorder() bool { return s != nil && s.persist != nil }
