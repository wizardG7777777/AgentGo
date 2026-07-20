package interaction

import (
	"sync"
	"time"
)

// EventKind 是 Interaction 观测事件类别。
type EventKind string

const (
	EventCreated      EventKind = "interaction_created"
	EventStateChanged EventKind = "interaction_state_changed"
)

// Event 供 UI Hub、审计与运行时适配器投影状态。Request 始终是深拷贝；
// 订阅者不得把 Event 当作修改 Store 的入口。
type Event struct {
	Kind          EventKind `json:"kind"`
	Request       Request   `json:"request"`
	PreviousState State     `json:"previous_state,omitempty"`
	At            time.Time `json:"at"`
}

type subscriber struct {
	id int
	ch chan Event
}

// Subscribe 注册非阻塞事件订阅。buf<=0 时按 1 处理；慢订阅者采用
// drop-oldest，永远不阻塞 Interaction 状态转换。返回的取消函数幂等且不会
// 关闭通道，避免与并发广播产生 send-on-closed 竞态。
//
// Subscribe 只提供增量事件。UI 应先 Subscribe，再调用 ListPending 获取
// 快照；二者重叠的同 ID/version 事件应按版本幂等合并。
func (s *Service) Subscribe(buf int) (<-chan Event, func()) {
	if buf < 1 {
		buf = 1
	}
	ch := make(chan Event, buf)

	s.eventMu.Lock()
	id := s.nextSubscriberID
	s.nextSubscriberID++
	s.subscribers[id] = ch
	s.eventMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.eventMu.Lock()
			delete(s.subscribers, id)
			s.eventMu.Unlock()
		})
	}
	return ch, cancel
}

func (s *Service) publish(event Event) {
	event.Request = CloneRequest(event.Request)
	s.eventMu.RLock()
	subs := make([]subscriber, 0, len(s.subscribers))
	for id, ch := range s.subscribers {
		subs = append(subs, subscriber{id: id, ch: ch})
	}
	s.eventMu.RUnlock()

	for _, sub := range subs {
		copyForSubscriber := cloneEvent(event)
		select {
		case sub.ch <- copyForSubscriber:
		default:
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- copyForSubscriber:
			default:
			}
		}
	}
	s.notifyWaiters(event.Request)
}

func cloneEvent(in Event) Event {
	out := in
	out.Request = CloneRequest(in.Request)
	return out
}
