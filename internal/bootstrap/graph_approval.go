package bootstrap

// 本文件是 V6 Graph approval 桥（C5c）：把 internal/graph 的 ApprovalGateway
// 接到活系统的 Interaction 服务。三件东西：
//   - graphApprovalGateway：graph.ApprovalGateway 的 Interaction 实现——
//     approval 节点的审批请求落成 purpose=graph_approval 的授权型
//     Interaction（批准/拒绝两选项），requestID 由 (graph_id, activation_id)
//     确定性派生（幂等键），决议经 Service 终态回调异步回填
//     Runtime.OnApprovalDecided；
//   - wireGraphApprovalBridge：Bootstrap 装配点（注入网关 + 注册终态回调）；
//   - rearmPendingGraphApprovals：重启后为重挂起的 approval 节点补登记
//     Interaction（Interaction 是内存服务不跨重启，而 execution.requestID
//     已 durable，Runtime 恢复路径不会重复 RequestApproval）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"agentgo/internal/graph"
	"agentgo/internal/interaction"
)

const (
	// purposeGraphApproval 是图审批请求的 Interaction 业务用途标识。
	purposeGraphApproval interaction.Purpose = "graph_approval"
	// graphApprovalHandler 是图审批请求的 Resolution.Handler（服务端零
	// effect——批准/拒绝事实由本桥经终态回调回填 Graph Runtime，
	// resolveInteraction 只锁定回答并 Complete）。
	graphApprovalHandler = "graph_approval"

	// 图审批的两个稳定选项 ID。
	graphApprovalOptionApprove = "approve"
	graphApprovalOptionReject  = "reject"
)

// graphApprovalGateway 实现 graph.ApprovalGateway：approval 节点的审批请求
// 落成 Interaction，等待用户裁决。决议不回经本方法返回——由终态回调
// （onInteractionResolved）异步回填 Runtime.OnApprovalDecided。
//
// 锁纪律（graph.ApprovalGateway 接口契约）：RequestApproval 在 Runtime 锁内
// 被同步调用，只触碰 Interaction 服务（独立锁域），绝不回调 Runtime；
// 决议回调在 Service 迁移调用方的 goroutine 触发，一律起新 goroutine 再调
// Runtime（rt.mu 与 Service 迁移路径不构成锁序环）。
type graphApprovalGateway struct {
	interactions *interaction.Service
	runtime      *graph.Runtime // 决议回填目标；RequestApproval 内不得触碰

	mu sync.Mutex
	// byActivation 是 (graphID \x00 activationID) → requestID 的进程内索引，
	// 仅作快路径；miss 时凭确定性 requestID + service.Get 兜底去重（覆盖
	// 崩溃窗口后的补发与重启后的 rearm）。
	byActivation map[string]string
}

func newGraphApprovalGateway(interactions *interaction.Service, rt *graph.Runtime) *graphApprovalGateway {
	return &graphApprovalGateway{
		interactions: interactions,
		runtime:      rt,
		byActivation: make(map[string]string),
	}
}

// graphApprovalRequestID 由 (graph_id, activation_id) 确定性派生请求 ID
// （哈希避免 graph/node ID 字符集与请求 ID 稳定标识字符集的耦合）。
// 确定性是跨崩溃/重启幂等的根基：同一 activation 永远得到同一 requestID。
func graphApprovalRequestID(graphID, activationID string) string {
	sum := sha256.Sum256([]byte(graphID + "\x00" + activationID))
	return "gxa_" + hex.EncodeToString(sum[:])[:24]
}

// RequestApproval 实现 graph.ApprovalGateway：以 (GraphID, ActivationID) 为
// 幂等键创建 purpose=graph_approval 的授权型 Interaction，返回 requestID。
// 同一 activation 的补发（崩溃窗口后 ResumeGraph / 重启后 rearm）经三级去重：
// 进程内索引 → service.Get 命中既有 → Create 撞 ErrDuplicateID 复用。
func (g *graphApprovalGateway) RequestApproval(spec graph.ApprovalSpec) (string, error) {
	key := graphActivationKey(spec.GraphID, spec.ActivationID)
	g.mu.Lock()
	if id, ok := g.byActivation[key]; ok {
		g.mu.Unlock()
		return id, nil
	}
	g.mu.Unlock()

	id := graphApprovalRequestID(spec.GraphID, spec.ActivationID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	existing, err := g.interactions.Get(ctx, id)
	if err == nil {
		g.mu.Lock()
		g.byActivation[key] = existing.ID
		g.mu.Unlock()
		return existing.ID, nil
	}
	if !errors.Is(err, interaction.ErrNotFound) {
		return "", fmt.Errorf("查询既有图审批请求失败: %w", err)
	}

	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = fmt.Sprintf("图 %s 节点 %s 的审批", spec.GraphID, spec.NodeID)
	}
	prompt := fmt.Sprintf("%s\n\n图 %s · 节点 %s · activation %s", title, spec.GraphID, spec.NodeID, spec.ActivationID)
	if desc := strings.TrimSpace(spec.Description); desc != "" {
		prompt += "\n\n" + desc
	}
	_, err = g.interactions.Create(ctx, interaction.CreateRequest{
		ID:      id,
		Kind:    interaction.KindAuthorization,
		Purpose: purposeGraphApproval,
		Prompt:  prompt,
		Options: []interaction.Option{
			{ID: graphApprovalOptionApprove, Label: "批准", Description: "同意该节点推进", ActionRef: "graph.approval.approve"},
			{ID: graphApprovalOptionReject, Label: "拒绝", Description: "驳回（可在文本中附理由）", ActionRef: "graph.approval.reject"},
		},
		AllowFreeText: true,
		Origin:        interaction.Origin{Component: "graph"},
		Subject:       interaction.Subject{Kind: "graph_approval", ID: spec.ActivationID},
		Resolution:    interaction.ResolutionSpec{Handler: graphApprovalHandler, TargetID: spec.ActivationID},
		Metadata: map[string]string{
			"graph_id":      spec.GraphID,
			"node_id":       spec.NodeID,
			"activation_id": spec.ActivationID,
		},
	})
	if err != nil && !errors.Is(err, interaction.ErrDuplicateID) {
		return "", fmt.Errorf("创建图审批请求（图 %s 节点 %s activation %s）失败: %w",
			spec.GraphID, spec.NodeID, spec.ActivationID, err)
	}
	// ErrDuplicateID = 并发/补发撞单：同 ID 请求已存在，复用即可。
	g.mu.Lock()
	g.byActivation[key] = id
	g.mu.Unlock()
	return id, nil
}

// onInteractionResolved 是 Interaction 终态回调（经 Service.SetOnResolved
// 注册）：把 purpose=graph_approval 请求的终态映射为图审批裁决并异步回填。
// resolved 按选中 option 定批准/拒绝；cancelled/expired/failed/interrupted
// 一律映射为 rejected（text 载明原因）。身份取自请求 Metadata——进程内索引
// 丢失（重启）后依然可回填。重复/迟到裁决由 Runtime 侧守卫幂等忽略。
func (g *graphApprovalGateway) onInteractionResolved(request interaction.Request) {
	if request.Purpose != purposeGraphApproval {
		return
	}
	graphID := request.Metadata["graph_id"]
	nodeID := request.Metadata["node_id"]
	activationID := request.Metadata["activation_id"]
	if graphID == "" || nodeID == "" || activationID == "" {
		log.Printf("[graph] WARNING: graph_approval 请求 %s 缺图身份元数据，无法回填", request.ID)
		return
	}

	approved := false
	var text string
	if request.State == interaction.StateResolved {
		text = "批准"
		if option, ok := request.SelectedOption(); ok {
			approved = option.ID == graphApprovalOptionApprove
			if !approved {
				text = "拒绝"
			}
		}
		if request.Response != nil && strings.TrimSpace(request.Response.Text) != "" {
			text = strings.TrimSpace(request.Response.Text)
		}
	} else {
		// cancelled/expired/failed/interrupted：审批没有有效裁决，按拒绝处理并载明原因。
		reason := strings.TrimSpace(request.StatusReason)
		if reason == "" {
			reason = "（无原因）"
		}
		text = fmt.Sprintf("审批请求已%s：%s", graphApprovalStateText(request.State), reason)
	}

	go func() {
		if err := g.runtime.OnApprovalDecided(graphID, nodeID, activationID, approved, text); err != nil {
			log.Printf("[graph] WARNING: 回填图审批裁决失败（图 %s 节点 %s activation %s）: %v",
				graphID, nodeID, activationID, err)
		}
	}()
}

// graphApprovalStateText 给非 resolved 终态一个中文短语（仅用于裁决文本）。
func graphApprovalStateText(state interaction.State) string {
	switch state {
	case interaction.StateCancelled:
		return "取消"
	case interaction.StateExpired:
		return "过期"
	case interaction.StateFailed:
		return "失败"
	case interaction.StateInterrupted:
		return "中断"
	}
	return string(state)
}

// wireGraphApprovalBridge 装配 approval 桥（C5c）：网关注入 Runtime，并在
// Interaction 服务注册终态回调把决议回填 Runtime.OnApprovalDecided。
// 返回的网关供 rearmPendingGraphApprovals 使用；任一为 nil 时不装配（返回 nil）。
func wireGraphApprovalBridge(interactions *interaction.Service, rt *graph.Runtime) *graphApprovalGateway {
	if interactions == nil || rt == nil {
		return nil
	}
	gw := newGraphApprovalGateway(interactions, rt)
	rt.SetApprovalGateway(gw)
	interactions.SetOnResolved(gw.onInteractionResolved)
	return gw
}

// rearmPendingGraphApprovals 在重启恢复后为 waiting 且已记录 requestID 的
// approval 节点补登记 Interaction：Interaction 是内存服务（不跨重启），而
// execution.requestID 已 durable 令 Runtime 恢复路径不再 RequestApproval，
// 不补登记审批请求就会在重启后凭空消失。确定性 requestID + Get 去重保证
// 幂等；既有请求已到终态（决议在崩溃窗口丢失）时立即补回填一次。
// 单图/单节点失败只记 WARNING，不阻断启动。
func rearmPendingGraphApprovals(gs *graph.Store, gw *graphApprovalGateway) {
	if gs == nil || gw == nil {
		return
	}
	rearmed := 0
	for _, sum := range gs.List() {
		if sum.Status.IsTerminal() {
			continue
		}
		doc, ok := gs.Get(sum.GraphID)
		if !ok {
			continue
		}
		for nodeID, n := range doc.Nodes {
			if n.Kind != graph.KindApproval || n.Status != graph.NodeWaiting ||
				n.Execution == nil || n.Execution.RequestID == "" {
				continue
			}
			spec := graph.ApprovalSpec{GraphID: sum.GraphID, NodeID: nodeID, ActivationID: n.Execution.ActivationID}
			if n.Task != nil {
				spec.Title = n.Task.Title
				spec.Description = n.Task.Description
			}
			requestID, err := gw.RequestApproval(spec)
			if err != nil {
				log.Printf("[启动] WARNING: 补登记图审批请求（图 %s 节点 %s）失败: %v", sum.GraphID, nodeID, err)
				continue
			}
			rearmed++
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			request, getErr := gw.interactions.Get(ctx, requestID)
			cancel()
			if getErr == nil && request.State.IsTerminal() {
				gw.onInteractionResolved(request) // 崩溃窗口丢失的决议补回填
			}
		}
	}
	if rearmed > 0 {
		log.Printf("[启动] 已补登记 %d 个图审批请求", rearmed)
	}
}
