package ui

import (
	"context"
	"sync"
	"time"

	"agentgo/internal/interaction"
	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/output"
	"agentgo/internal/scheduler"
	"agentgo/internal/trace"
)

// DefaultPollInterval 是 PollInterval 未设置（<=0）时的默认轮询间隔。
const DefaultPollInterval = 500 * time.Millisecond

const (
	recentOutputLimit = 200
	recentLogLimit    = 500
	recentTraceLimit  = 1000
)

// Deps 是 Hub 的全部外部依赖，以注入函数 / 通道的形式给出。
// bootstrap 负责把真实系统组件装配进来；测试用 fake 即可。
//
// 三个事件源通道是强制排干路径（headless 保证）：Hub 即使没有任何订阅者
// 也会持续消费它们，上游生产者永不因前端缺失而阻塞。
// 注入函数除通道外均可为 nil——nil 时对应能力不可用，Controller 方法返回
// ErrNotAssembled 错误（无 error 返回值的方法静默忽略），绝不 panic。
type Deps struct {
	// ── 事件源（Hub 常驻排干）──

	// OutputCh 是代理用户可见输出通道。
	OutputCh <-chan output.Event
	// Interactions 是结构化人机交互的权威服务。Hub 只维护其前端安全投影。
	// 运行时任务跨 Session 连续，因此 Hub 展示整个进程中所有 pending 请求；
	// Request.SessionID 仅保留创建时的审计归属，不能用于隐藏活动控制点。
	Interactions *interaction.Service
	// StatusCh 是系统日志行通道（tui 中的 SystemMsgCh）。
	StatusCh <-chan string

	// ── 快照轮询（可选；nil 时对应字段保持零值）──

	// PollAgents 返回当前全部代理卡片。
	PollAgents func() []AgentCard
	// PollBoard 返回当前任务看板。
	PollBoard func() []BoardTask
	// PollInterval 是快照轮询间隔；<=0 时使用 DefaultPollInterval。
	PollInterval time.Duration
	// ModeGet 返回当前调度模式（"plan" / "immediate"）。
	ModeGet func() string
	// ExecModeGet 返回当前执行权限模式（"normal" / "strict" / "readonly" / "yolo"）。
	ExecModeGet func() string
	// TopoModeGet 返回当前编排拓扑模式（"team" / "solo"）。
	TopoModeGet func() string
	// SessionGet 返回当前 Session 信息。
	SessionGet func() SessionInfo
	// ResultGet 返回当前 Session 最近一次用户可见任务结果。Hub 仍会在收到
	// KindResult 时立即更新自己的快照；此 getter 用于启动恢复、页面晚订阅与
	// Session 切换后的周期自愈。
	ResultGet func() *ResultItem

	// ── 控制面注入（Controller 实现依赖；nil 时返回 ErrNotAssembled）──

	// RecordUserInput 在投递用户输入事件前调用，对应 tui sendUserText 中的
	// SessionMgr.RecordFirstInput + IncrementTaskCount 元数据记录。
	RecordUserInput func(text string)
	// SendUserEvent 把用户输入事件投递到调度事件通道；由装配方实现
	// tui sendUserText 的 5 秒发送超时语义。
	SendUserEvent func(ev model.Event) error
	// CancelTaskByPrefix 是受守卫的任务取消入口（前缀解析 + plan 守卫由
	// 装配方完成），返回实际取消的完整任务 ID。
	CancelTaskByPrefix func(idPrefix string) (taskID string, err error)
	// CancelLatestRequest 是请求树取消入口（Esc 取消的后端）：取消最新
	// 创建的一棵活跃请求树，返回一行中文摘要；无活跃请求树时返回
	// ErrNoActiveRequest。
	CancelLatestRequest func() (summary string, err error)
	// SteerSend 投递一条已构造好的 steer 邮件。消息构造（From=user /
	// MsgTypeSteer / PriorityHigh）是协议，保留在 ui 层；注入方只负责传输。
	SteerSend func(msg mailbox.Message) error
	// ModeSet 切换调度模式的 gate 轴。Controller.SetMode 负责
	// bool→scheduler.Mode（= modes.GateMode）的映射
	// （true=plan，false=immediate），与三轴 modes.Store 的 gate 轴语义一致。
	ModeSet func(mode scheduler.Mode)
	// ExecModeSet 切换 exec 轴。Controller.SetExecMode 已用
	// modes.ParseExecMode 解析并传入规范化小写串（"normal" 等），
	// 装配方只需写入 store，无需再解析。
	ExecModeSet func(mode string) error
	// TopoModeSet 切换 topo 轴。Controller.SetTopoMode 已用
	// modes.ParseTopoMode 解析并传入规范化小写串（"team" / "solo"）。
	TopoModeSet func(mode string) error
	// SessionNew 创建并切换到新 Session，返回新 Session ID。
	SessionNew func() (string, error)
	// SessionSwitch 切换到指定 Session；changed=false 表示同 Session no-op。
	SessionSwitch func(id string) (changed bool, err error)
	// SessionList 返回全部 Session 信息。
	SessionList func() ([]SessionInfo, error)
	// ApprovePlanReview 批准一个处于 plan_review 挂起的 Plan（gate=plan）。
	// idPrefix 为 Plan ID 前缀；空串在恰好一个待批准时默认选中。返回一行
	// 中文摘要；前缀解析 / 歧义处理 / 保留 controller 预发布与 Plan 恢复
	// 全部由装配方完成。
	ApprovePlanReview func(idPrefix string) (summary string, err error)
	// RejectPlanReview 拒绝并终止一个处于 plan_review 挂起的 Plan。
	// 前缀语义同 ApprovePlanReview。
	RejectPlanReview func(idPrefix string) (summary string, err error)
	// PendingPlanReviews 返回全部等待批准的计划条目（/plan 列表）。
	PendingPlanReviews func() ([]PlanReviewItem, error)
	// ResolveInteraction 由 bootstrap 路由并执行服务器端 ActionRef；Hub 不
	// 解释业务动作，只转交客户端的 option_id / text 与 expected_version。
	ResolveInteraction func(context.Context, interaction.ResolveInput) (interaction.Request, error)
	// QuitFn 是退出入口（对应 tui 的 /quit → CancelFn）。
	QuitFn func()
}

// Observer 是 Hub 的观测面接口：前端（TUI / Web GUI）经它订阅 Update 流
// 并读取当前快照。控制面见 control.go 的 Controller；两者都由 Hub 实现，
// 前端只依赖这两个接口，不感知 Hub 具体类型。
//
// 前端初始状态由 Subscribe 的首条 KindSnapshotSync 更新提供（订阅建立即
// 下发，见 Subscribe 文档），因此本接口不需要额外的同步初始化方法。
type Observer interface {
	// Subscribe 注册订阅者，返回只读更新通道与幂等取消函数；
	// 首条更新必为 KindSnapshotSync 全量快照。
	Subscribe(buf int) (<-chan Update, func())
	// Snapshot 返回最新快照（只读，调用方不得修改）。
	Snapshot() Snapshot
}

// 编译期断言：Hub 必须实现 Observer。
var _ Observer = (*Hub)(nil)

// Hub 是 UI 中枢：单 mux goroutine 消费事件源并维护最新快照，
// 向任意数量的订阅者扇出 Update；同时实现 Controller 控制面。
type Hub struct {
	deps Deps

	mu        sync.RWMutex
	subs      map[int]chan Update // 订阅者表
	nextSubID int                 // 订阅 ID 自增
	snapshot  Snapshot            // 最新快照（整体替换，绝不原地修改）
	feed      FeedSnapshot        // 有界实时窗口；在 Snapshot/Subscribe 时复制

	// Session 级 token 累加器：由 EmitTraceEvent 逐条累加 token_stats 事件
	// （每次 LLM 调用恰好一条，载本轮消耗）。独立于 snapshot 存放，
	// refreshSnapshot 整体替换快照时不会被抹掉；ad-hoc 团队销毁后其消耗
	// 仍累计在此，避免"对存活 agent 求和"导致的消耗隐形。
	sessionPromptTokens     int64
	sessionCompletionTokens int64
	sessionCallCount        int
}

// NewHub 创建 Hub。Run 尚未启动前即可 Subscribe / 调用 Controller。
func NewHub(deps Deps) *Hub {
	return &Hub{
		deps: deps,
		subs: make(map[int]chan Update),
	}
}

// Run 启动 Hub 的主循环，阻塞直到 ctx 取消。
//
// 循环职责：
//  1. 常驻排干 OutputCh / StatusCh，并订阅 Interaction——即使前端为零也照常
//     消费（headless 保证），更新仅用于维护内部状态后直接丢弃；
//  2. 按 PollInterval 刷新快照并广播 KindAgentsChanged；
//  3. ctx.Done 时退出。
//
// 某个事件源通道被关闭后，该源被永久移除（置 nil），避免对关闭通道忙轮询。
func (h *Hub) Run(ctx context.Context) {
	outCh := h.deps.OutputCh
	statusCh := h.deps.StatusCh
	var interactionCh <-chan interaction.Event
	var cancelInteractions func()
	if h.deps.Interactions != nil {
		interactionCh, cancelInteractions = h.deps.Interactions.Subscribe(64)
		defer cancelInteractions()
	}

	interval := h.deps.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}

	// 启动即刷新一次快照，保证首批订阅者拿到的是当前状态而非零值。
	h.refreshSnapshot()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-outCh:
			if !ok {
				outCh = nil
				continue
			}
			if ev.Kind == output.KindResult {
				h.setLastResult(&ResultItem{AgentID: ev.AgentID, Text: ev.Text})
			}
			now := time.Now()
			h.recordOutput(ev, now)
			h.broadcast(Update{Kind: outputUpdateKind(ev.Kind), Output: ev, At: now})
		case line, ok := <-statusCh:
			if !ok {
				statusCh = nil
				continue
			}
			now := time.Now()
			h.recordLog(line, now)
			h.broadcast(Update{Kind: KindLogLine, LogLine: line, At: now})
		case _, ok := <-interactionCh:
			if !ok {
				interactionCh = nil
				continue
			}
			h.refreshInteractions()
		case <-ticker.C:
			h.refreshSnapshot()
			snap := h.Snapshot()
			h.broadcast(Update{
				Kind:                    KindAgentsChanged,
				Agents:                  snap.Agents,
				Tasks:                   snap.Tasks,
				SessionPromptTokens:     snap.SessionPromptTokens,
				SessionCompletionTokens: snap.SessionCompletionTokens,
				SessionCallCount:        snap.SessionCallCount,
				At:                      time.Now(),
			})
		}
	}
}

// Subscribe 注册一个订阅者，返回只读更新通道与取消函数。
//
// 语义保证：
//   - 订阅者收到的第一条更新必定是 KindSnapshotSync 全量快照（通道容量
//     下限为 1，注册时快照在锁内入队，保证"第一条"不被其他广播抢位）；
//   - buf<=0 时按 1 处理；
//   - 通道满时采用 drop-oldest 策略：丢弃最旧一条再入队，慢前端绝不
//     阻塞 hub；极端并发下仍入队失败则放弃该条更新；
//   - 取消函数是幂等的：仅从订阅表注销，不关闭通道（关闭会与广播的
//     非阻塞发送产生 send-on-closed 竞态）。取消后订阅方应自行停止读取，
//     通道随订阅方释放被 GC 回收。
func (h *Hub) Subscribe(buf int) (<-chan Update, func()) {
	if buf < 1 {
		buf = 1
	}
	ch := make(chan Update, buf)

	h.mu.Lock()
	id := h.nextSubID
	h.nextSubID++
	h.subs[id] = ch
	// 首条必为全量快照：通道新建且容量>=1，锁内发送绝不阻塞。
	ch <- Update{Kind: KindSnapshotSync, Snapshot: h.snapshotWithFeedLocked(), At: time.Now()}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, id)
			h.mu.Unlock()
		})
	}
	return ch, cancel
}

// Snapshot 返回最新快照。返回值只读，前端不得修改（见 Snapshot 文档）。
func (h *Hub) Snapshot() Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snapshotWithFeedLocked()
}

func (h *Hub) snapshotWithFeedLocked() Snapshot {
	snap := h.snapshot
	snap.Feed = cloneFeedSnapshot(h.feed)
	snap.SessionPromptTokens = h.sessionPromptTokens
	snap.SessionCompletionTokens = h.sessionCompletionTokens
	snap.SessionCallCount = h.sessionCallCount
	return snap
}

// recordTokenStats 累加一条 token_stats 事件的本轮消耗到 session 级计数器。
// token_stats 每次 LLM 调用恰好 emit 一条（agent.go 主循环），因此不存在
// 重复计数；llm_call_end 虽也载本轮 token，但不纳入（会与 token_stats 双计）。
func (h *Hub) recordTokenStats(ev trace.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionPromptTokens += int64(ev.PromptTokens)
	h.sessionCompletionTokens += int64(ev.CompletionTokens)
	h.sessionCallCount++
}

func cloneFeedSnapshot(feed FeedSnapshot) FeedSnapshot {
	return FeedSnapshot{
		Outputs: append([]FeedOutput(nil), feed.Outputs...),
		Logs:    append([]LogItem(nil), feed.Logs...),
		Traces:  append([]TraceEvent(nil), feed.Traces...),
	}
}

func feedOutputFromEvent(ev output.Event, at time.Time) FeedOutput {
	kind := "text"
	switch ev.Kind {
	case output.KindResult:
		kind = "result"
	case output.KindStream:
		kind = "stream"
	}
	return FeedOutput{
		Kind: kind, AgentID: ev.AgentID, TaskID: ev.TaskID, StreamID: ev.StreamID,
		Loop: ev.Loop, Text: ev.Text, Done: ev.Done, Error: ev.Error, At: at,
	}
}

func (h *Hub) recordOutput(ev output.Event, at time.Time) {
	item := feedOutputFromEvent(ev, at)
	h.mu.Lock()
	defer h.mu.Unlock()
	if item.Kind == "stream" && item.StreamID != "" {
		for i := len(h.feed.Outputs) - 1; i >= 0; i-- {
			if h.feed.Outputs[i].StreamID == item.StreamID {
				h.feed.Outputs[i] = item
				return
			}
		}
	}
	h.feed.Outputs = append(h.feed.Outputs, item)
	if len(h.feed.Outputs) > recentOutputLimit {
		h.feed.Outputs = append([]FeedOutput(nil), h.feed.Outputs[len(h.feed.Outputs)-recentOutputLimit:]...)
	}
}

func (h *Hub) recordLog(line string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.feed.Logs = append(h.feed.Logs, LogItem{Text: line, At: at})
	if len(h.feed.Logs) > recentLogLimit {
		h.feed.Logs = append([]LogItem(nil), h.feed.Logs[len(h.feed.Logs)-recentLogLimit:]...)
	}
}

func (h *Hub) recordTrace(event TraceEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.feed.Traces = append(h.feed.Traces, event)
	if len(h.feed.Traces) > recentTraceLimit {
		h.feed.Traces = append([]TraceEvent(nil), h.feed.Traces[len(h.feed.Traces)-recentTraceLimit:]...)
	}
}

// refreshSnapshot 调用各轮询函数重建快照并整体替换。
// 轮询函数与 Interaction Store 读取均在锁外调用（可能较慢）。
func (h *Hub) refreshSnapshot() {
	var snap Snapshot
	if h.deps.PollAgents != nil {
		snap.Agents = h.deps.PollAgents()
	}
	if h.deps.PollBoard != nil {
		snap.Tasks = h.deps.PollBoard()
	}
	if h.deps.ModeGet != nil {
		snap.Mode = h.deps.ModeGet()
	}
	if h.deps.ExecModeGet != nil {
		snap.ExecMode = h.deps.ExecModeGet()
	}
	if h.deps.TopoModeGet != nil {
		snap.TopoMode = h.deps.TopoModeGet()
	}
	if h.deps.SessionGet != nil {
		snap.Session = h.deps.SessionGet()
	}
	if h.deps.ResultGet != nil {
		snap.LastResult = cloneResultItem(h.deps.ResultGet())
	} else {
		// 没有权威 getter 的轻量装配（常见于包级测试）仍保留从 OutputCh
		// 实时观察到的结果，不能被下一次轮询刷新抹掉。
		h.mu.RLock()
		snap.LastResult = cloneResultItem(h.snapshot.LastResult)
		h.mu.RUnlock()
	}
	if h.deps.Interactions != nil {
		requests, err := h.deps.Interactions.ListPending(context.Background(), "")
		if err == nil {
			snap.PendingInteractions = interactionItemsFromRequests(requests)
		}
	}

	h.mu.Lock()
	h.snapshot = snap
	h.mu.Unlock()
}

func (h *Hub) setLastResult(result *ResultItem) {
	h.mu.Lock()
	snap := h.snapshot
	snap.LastResult = cloneResultItem(result)
	h.snapshot = snap
	h.mu.Unlock()
}

func cloneResultItem(result *ResultItem) *ResultItem {
	if result == nil {
		return nil
	}
	cp := *result
	return &cp
}

// refreshInteractions 用完整 pending 列表替换快照并广播。这样即使订阅者
// 因背压丢失一次边沿事件，下一次任何 Interaction 变更仍会自愈。
func (h *Hub) refreshInteractions() {
	if h.deps.Interactions == nil {
		return
	}
	requests, err := h.deps.Interactions.ListPending(context.Background(), "")
	if err != nil {
		return
	}
	items := interactionItemsFromRequests(requests)
	h.mu.Lock()
	snap := h.snapshot
	snap.PendingInteractions = items
	h.snapshot = snap
	h.mu.Unlock()
	h.broadcast(Update{Kind: KindInteractionsChanged, Interactions: items, At: time.Now()})
}

// broadcast 把一条更新扇出给全部订阅者。
// 订阅者列表在锁内拷贝，发送在锁外进行且全部非阻塞（drop-oldest），
// 任何慢前端都不会阻塞 hub。
func (h *Hub) broadcast(u Update) {
	h.mu.RLock()
	subs := make([]chan Update, 0, len(h.subs))
	for _, ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- u:
		default:
			// 通道满：丢弃最旧一条，再尝试入队；仍失败（多广播者并发
			// 竞争）则放弃本条，绝不阻塞。
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- u:
			default:
			}
		}
	}
}

// outputUpdateKind 把 output.Kind 映射为对应的 UpdateKind。
func outputUpdateKind(k output.Kind) UpdateKind {
	if k == output.KindResult {
		return KindOutputResult
	}
	if k == output.KindStream {
		return KindOutputStream
	}
	return KindOutputText
}
