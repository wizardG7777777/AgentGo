package tui

import (
	"sort"
	"strconv"
	"strings"

	"agentgo/internal/ui"
)

// nodeActivation 是节点一次 activation 的历史条目。回边重进（work@1 失败后
// work@2 重跑）会为同一逻辑节点产生多条；每条对应一次独立的任务执行。
type nodeActivation struct {
	ActivationID string
	TaskID       string
	Status       string // BoardTask 状态；当前 activation 可能尚无任务，留空
}

// activationsForNode 从快照 Tasks 按 GraphID+NodeID 归组节点的全部
// activation，按 activation 序号（<nodeID>@<n> 的 n，解析失败退字典序）
// 升序。节点当前 ActivationID/TaskID 即便不在 Tasks 里（任务尚未发布或
// 已被看板淘汰）也并入，保证「当前 activation」永远可选。
func activationsForNode(tasks []ui.BoardTask, graph GraphInfo, node GraphNodeInfo) []nodeActivation {
	byActivation := make(map[string]nodeActivation)
	order := make([]string, 0, 4)
	for _, task := range tasks {
		if task.GraphID != graph.GraphID || task.NodeID != node.NodeID || task.ActivationID == "" {
			continue
		}
		if _, seen := byActivation[task.ActivationID]; seen {
			continue
		}
		byActivation[task.ActivationID] = nodeActivation{
			ActivationID: task.ActivationID,
			TaskID:       task.ID,
			Status:       task.Status,
		}
		order = append(order, task.ActivationID)
	}
	if node.ActivationID != "" {
		if _, seen := byActivation[node.ActivationID]; !seen {
			byActivation[node.ActivationID] = nodeActivation{
				ActivationID: node.ActivationID,
				TaskID:       node.TaskID,
			}
			order = append(order, node.ActivationID)
		}
	}

	acts := make([]nodeActivation, 0, len(order))
	for _, id := range order {
		acts = append(acts, byActivation[id])
	}
	sort.SliceStable(acts, func(i, j int) bool {
		seqI, okI := activationSeq(acts[i].ActivationID)
		seqJ, okJ := activationSeq(acts[j].ActivationID)
		if okI && okJ {
			return seqI < seqJ
		}
		return acts[i].ActivationID < acts[j].ActivationID
	})
	return acts
}

// activationSeq 解析 activation ID（<nodeID>@<n>）的序号 n；无法解析时
// ok=false（调用方退字典序）。
func activationSeq(activationID string) (int, bool) {
	at := strings.LastIndex(activationID, "@")
	if at < 0 || at+1 >= len(activationID) {
		return 0, false
	}
	n, err := strconv.Atoi(activationID[at+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// resolveActivationIndex 把 selected（AppModel.selectedActivation）解析为
// acts 中的有效下标：负值（跟随当前）或越界时回退到节点当前 activation。
func resolveActivationIndex(acts []nodeActivation, node GraphNodeInfo, selected int) int {
	if selected >= 0 && selected < len(acts) {
		return selected
	}
	for index, act := range acts {
		if act.ActivationID == node.ActivationID {
			return index
		}
	}
	return len(acts) - 1
}

// activationAdjustedNode 把节点视图切换到选中的 activation：TaskID /
// ActivationID 指向选中项（轮次/输出/trace 过滤随之切到该次运行）；
// 浏览旧 activation 时状态用任务终态标注（投影里的节点状态只属于当前
// activation）。
func activationAdjustedNode(node GraphNodeInfo, acts []nodeActivation, index int) GraphNodeInfo {
	if index < 0 || index >= len(acts) {
		return node
	}
	act := acts[index]
	adjusted := node
	adjusted.ActivationID = act.ActivationID
	if act.TaskID != "" {
		adjusted.TaskID = act.TaskID
	}
	if act.ActivationID != node.ActivationID && act.Status != "" {
		adjusted.Status = act.Status
	}
	return adjusted
}

// selectedActivationView 解析选中节点当前应展示的 activation 视图：返回
// 按选中 activation 调整后的节点、完整历史列表与选中项下标。无选中图/
// 节点时 ok=false。
func (m *AppModel) selectedActivationView() (node GraphNodeInfo, acts []nodeActivation, index int, ok bool) {
	graph := m.selectedGraphView()
	selected := m.selectedNodeView()
	if graph == nil || selected == nil {
		return GraphNodeInfo{}, nil, 0, false
	}
	acts = activationsForNode(m.tasks, *graph, *selected)
	index = resolveActivationIndex(acts, *selected, m.selectedActivation)
	return activationAdjustedNode(*selected, acts, index), acts, index, true
}

// moveSelectedActivation 在节点详情的 activation 历史中前后移动（←→）。
// 切换后滚动归零——新选中的 activation 有自己的轮次历史。
func (m *AppModel) moveSelectedActivation(delta int) {
	_, acts, index, ok := m.selectedActivationView()
	if !ok || len(acts) < 2 {
		return
	}
	next := index + delta
	if next < 0 {
		next = 0
	}
	if next >= len(acts) {
		next = len(acts) - 1
	}
	if next == index && m.selectedActivation >= 0 {
		return
	}
	m.selectedActivation = next
	m.nodeDetailScroll = 0
}
