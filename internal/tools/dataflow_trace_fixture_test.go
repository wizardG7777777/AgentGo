package tools
import "agentgo/internal/trace"
type captureGraphTraceDispatcher struct{events []trace.Event}
func(d *captureGraphTraceDispatcher)Dispatch(e trace.Event){d.events=append(d.events,e)}
func(d *captureGraphTraceDispatcher)ofKind(kind trace.EventKind)[]trace.Event{var out []trace.Event;for _,e:=range d.events{if e.Kind==kind{out=append(out,e)}};return out}
