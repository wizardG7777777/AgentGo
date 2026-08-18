package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"agentgo/internal/ui"
)

const (
	graphNodeBoxHeight = 5
	graphNodeMinWidth  = 24
	graphNodeMaxWidth  = 32
	graphNodeGap       = 3
)

type graphTopologyRender struct {
	lines        []string
	selectedLine int
}

type graphTopologyLayout struct {
	layers          []graphTopologyLayer
	nodeDepth       map[string]int
	backEdges       map[int]bool
	historicalEdges map[int]bool
}

type graphTopologyLayer struct {
	depth        int
	nodes        []graphTopologyNode
	hiddenBefore int
	hiddenAfter  int
}

type graphTopologyNode struct {
	node   GraphNodeInfo
	index  int
	x      int
	width  int
	center int
}

type graphCanvasCell struct {
	links uint8
	arrow bool
	kind  graphEdgeKind
}

type graphEdgeKind uint8

const (
	graphEdgePending graphEdgeKind = iota
	graphEdgeTraversed
	graphEdgeCurrent
)

const (
	graphLinkUp uint8 = 1 << iota
	graphLinkDown
	graphLinkLeft
	graphLinkRight
)

var (
	graphLightGlyphs = [16]string{
		"", "│", "│", "│", "─", "┘", "┐", "┤",
		"─", "└", "┌", "├", "─", "┴", "┬", "┼",
	}
	graphDoubleGlyphs = [16]string{
		"", "║", "║", "║", "═", "╝", "╗", "╣",
		"═", "╚", "╔", "╠", "═", "╩", "╦", "╬",
	}
	graphHeavyGlyphs = [16]string{
		"", "┃", "┃", "┃", "━", "┛", "┓", "┫",
		"━", "┗", "┏", "┣", "━", "┻", "┳", "╋",
	}
)

func renderGraphTopology(t Theme, w int, graph GraphInfo, selectedNode int) graphTopologyRender {
	layout := buildGraphTopologyLayout(graph, w, selectedNode)
	result := graphTopologyRender{selectedLine: -1}
	if len(layout.layers) == 0 {
		return result
	}

	visible := make(map[string]graphTopologyNode, len(graph.Nodes))
	for _, layer := range layout.layers {
		for _, node := range layer.nodes {
			visible[node.node.NodeID] = node
		}
	}

	for layerIndex, layer := range layout.layers {
		if layerIndex > 0 {
			connector, labels := renderGraphConnectorBand(
				t, w, graph, layout, visible, layout.layers[layerIndex-1].depth)
			result.lines = append(result.lines, connector...)
			result.lines = append(result.lines, labels...)
		}

		if layer.hiddenBefore > 0 || layer.hiddenAfter > 0 {
			label := fmt.Sprintf("layer %d", layer.depth)
			if layer.hiddenBefore > 0 {
				label += fmt.Sprintf(" · %d hidden left", layer.hiddenBefore)
			}
			if layer.hiddenAfter > 0 {
				label += fmt.Sprintf(" · %d hidden right", layer.hiddenAfter)
			}
			result.lines = append(result.lines, truncateDisplay(t.SidebarDim.Render("  "+label), w))
		}

		rowStart := len(result.lines)
		result.lines = append(result.lines, renderGraphTopologyLayer(t, w, graph, layer, selectedNode)...)
		for _, node := range layer.nodes {
			if node.index == selectedNode {
				result.selectedLine = rowStart + graphNodeBoxHeight/2
				break
			}
		}
	}

	special := renderGraphSpecialEdges(t, w, graph, layout)
	if len(special) > 0 {
		result.lines = append(result.lines, "")
		result.lines = append(result.lines, special...)
	}
	if result.selectedLine < 0 {
		result.selectedLine = 0
	}
	return result
}

func cropGraphTopology(t Theme, w int, lines []string, selectedLine, limit int) []string {
	if limit <= 0 || len(lines) == 0 {
		return nil
	}
	if len(lines) <= limit {
		return lines
	}
	if selectedLine < 0 || selectedLine >= len(lines) {
		selectedLine = 0
	}
	start := maxInt(0, selectedLine-graphNodeBoxHeight/2)
	reserveTop := 0
	if start > 0 {
		reserveTop = 1
	}
	contentLimit := maxInt(1, limit-reserveTop)
	end := minInt(len(lines), start+contentLimit)
	reserveBottom := 0
	if end < len(lines) && contentLimit > graphNodeBoxHeight {
		reserveBottom = 1
		end = maxInt(start, end-reserveBottom)
	}
	result := make([]string, 0, limit)
	if reserveTop > 0 {
		result = append(result, truncateDisplay(t.MsgLog.Render("  ↑ topology continues"), w))
	}
	result = append(result, lines[start:end]...)
	if reserveBottom > 0 {
		result = append(result, truncateDisplay(t.MsgLog.Render("  ↓ topology continues"), w))
	}
	return result
}

func buildGraphTopologyLayout(graph GraphInfo, w, selectedNode int) graphTopologyLayout {
	layout := graphTopologyLayout{
		nodeDepth:       make(map[string]int, len(graph.Nodes)),
		backEdges:       make(map[int]bool),
		historicalEdges: make(map[int]bool),
	}
	if len(graph.Nodes) == 0 {
		return layout
	}

	nodes := make(map[string]GraphNodeInfo, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.NodeID] = node
	}
	root := graph.Root
	if _, ok := nodes[root]; !ok {
		root = graph.Nodes[0].NodeID
	}

	adjacency := make(map[string][]int, len(graph.Nodes))
	for index, edge := range graph.Edges {
		if edge.When == "historical" {
			layout.historicalEdges[index] = true
			continue
		}
		if _, fromOK := nodes[edge.From]; !fromOK {
			continue
		}
		if _, toOK := nodes[edge.To]; !toOK {
			continue
		}
		adjacency[edge.From] = append(adjacency[edge.From], index)
	}

	state := make(map[string]uint8, len(graph.Nodes))
	var visit func(string)
	visit = func(nodeID string) {
		state[nodeID] = 1
		for _, edgeIndex := range adjacency[nodeID] {
			target := graph.Edges[edgeIndex].To
			switch state[target] {
			case 0:
				visit(target)
			case 1:
				layout.backEdges[edgeIndex] = true
			}
		}
		state[nodeID] = 2
	}
	visit(root)
	for _, node := range graph.Nodes {
		if state[node.NodeID] == 0 {
			visit(node.NodeID)
		}
	}

	reachable := make(map[string]bool, len(graph.Nodes))
	queue := []string{root}
	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if reachable[nodeID] {
			continue
		}
		reachable[nodeID] = true
		for _, edgeIndex := range adjacency[nodeID] {
			if layout.backEdges[edgeIndex] {
				continue
			}
			queue = append(queue, graph.Edges[edgeIndex].To)
		}
	}

	indegree := make(map[string]int, len(graph.Nodes))
	for _, node := range graph.Nodes {
		indegree[node.NodeID] = 0
		layout.nodeDepth[node.NodeID] = -1
	}
	for index, edge := range graph.Edges {
		if layout.backEdges[index] || layout.historicalEdges[index] {
			continue
		}
		if _, fromOK := nodes[edge.From]; !fromOK {
			continue
		}
		if _, toOK := nodes[edge.To]; !toOK {
			continue
		}
		indegree[edge.To]++
	}
	topoQueue := make([]string, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if indegree[node.NodeID] == 0 {
			topoQueue = append(topoQueue, node.NodeID)
		}
	}
	layout.nodeDepth[root] = 0
	for len(topoQueue) > 0 {
		nodeID := topoQueue[0]
		topoQueue = topoQueue[1:]
		for _, edgeIndex := range adjacency[nodeID] {
			if layout.backEdges[edgeIndex] {
				continue
			}
			target := graph.Edges[edgeIndex].To
			if layout.nodeDepth[nodeID] >= 0 && reachable[nodeID] {
				candidate := layout.nodeDepth[nodeID] + 1
				if candidate > layout.nodeDepth[target] {
					layout.nodeDepth[target] = candidate
				}
			}
			indegree[target]--
			if indegree[target] == 0 {
				topoQueue = append(topoQueue, target)
			}
		}
	}

	maxDepth := 0
	for nodeID, depth := range layout.nodeDepth {
		if reachable[nodeID] && depth > maxDepth {
			maxDepth = depth
		}
	}
	for _, node := range graph.Nodes {
		if layout.nodeDepth[node.NodeID] < 0 {
			layout.nodeDepth[node.NodeID] = maxDepth + 1
		}
	}

	layerNodes := make(map[int][]graphTopologyNode)
	for index, node := range graph.Nodes {
		depth := layout.nodeDepth[node.NodeID]
		layerNodes[depth] = append(layerNodes[depth], graphTopologyNode{node: node, index: index})
	}
	depths := make([]int, 0, len(layerNodes))
	for depth := range layerNodes {
		depths = append(depths, depth)
	}
	sort.Ints(depths)
	for _, depth := range depths {
		nodesAtDepth := layerNodes[depth]
		visibleNodes, before, after := visibleGraphLayerNodes(nodesAtDepth, w, selectedNode)
		positionGraphLayerNodes(visibleNodes, w)
		layout.layers = append(layout.layers, graphTopologyLayer{
			depth: depth, nodes: visibleNodes, hiddenBefore: before, hiddenAfter: after,
		})
	}
	return layout
}

func visibleGraphLayerNodes(nodes []graphTopologyNode, w, selectedNode int) ([]graphTopologyNode, int, int) {
	if len(nodes) == 0 {
		return nil, 0, 0
	}
	maxVisible := maxInt(1, (maxInt(1, w-2)+graphNodeGap)/(graphNodeMinWidth+graphNodeGap))
	if len(nodes) <= maxVisible {
		return append([]graphTopologyNode(nil), nodes...), 0, 0
	}
	selected := -1
	for index, node := range nodes {
		if node.index == selectedNode {
			selected = index
			break
		}
	}
	if selected < 0 {
		selected = 0
	}
	start, end := selectionWindow(len(nodes), selected, maxVisible)
	return append([]graphTopologyNode(nil), nodes[start:end]...), start, len(nodes) - end
}

func positionGraphLayerNodes(nodes []graphTopologyNode, w int) {
	if len(nodes) == 0 {
		return
	}
	available := maxInt(1, w-2)
	boxWidth := (available - graphNodeGap*(len(nodes)-1)) / len(nodes)
	boxWidth = minInt(graphNodeMaxWidth, maxInt(10, boxWidth))
	total := boxWidth*len(nodes) + graphNodeGap*(len(nodes)-1)
	x := maxInt(0, (w-total)/2)
	for index := range nodes {
		nodes[index].x = x
		nodes[index].width = boxWidth
		nodes[index].center = x + boxWidth/2
		x += boxWidth + graphNodeGap
	}
}

func renderGraphTopologyLayer(t Theme, w int, graph GraphInfo, layer graphTopologyLayer, selectedNode int) []string {
	boxes := make([][]string, len(layer.nodes))
	for index, layoutNode := range layer.nodes {
		boxes[index] = renderGraphTopologyNode(
			t, graph, layoutNode.node, layoutNode.width, layoutNode.index == selectedNode)
	}
	rows := make([]string, graphNodeBoxHeight)
	for lineIndex := range rows {
		cursor := 0
		var line strings.Builder
		for nodeIndex, layoutNode := range layer.nodes {
			if layoutNode.x > cursor {
				line.WriteString(strings.Repeat(" ", layoutNode.x-cursor))
			}
			line.WriteString(boxes[nodeIndex][lineIndex])
			cursor = layoutNode.x + layoutNode.width
		}
		rows[lineIndex] = truncateDisplay(line.String(), w)
	}
	return rows
}

func renderGraphTopologyNode(t Theme, graph GraphInfo, node GraphNodeInfo, width int, selected bool) []string {
	width = maxInt(10, width)
	inner := maxInt(1, width-2)
	border := graphNodeBorderStyle(node.Status, selected)
	topLeft, topRight, bottomLeft, bottomRight, horizontal, vertical := "┌", "┐", "└", "┘", "─", "│"
	if selected {
		topLeft, topRight, bottomLeft, bottomRight, horizontal, vertical = "╔", "╗", "╚", "╝", "═", "║"
	}
	top := border.Render(topLeft + strings.Repeat(horizontal, inner) + topRight)
	bottom := border.Render(bottomLeft + strings.Repeat(horizontal, inner) + bottomRight)

	icon, statusStyle := nodeStatusVisual(t, node.Status)
	role := ""
	if node.NodeID == graph.Root || node.Root {
		role = "START "
	} else if node.Kind == "end" {
		role = "END "
	}
	selection := ""
	if selected {
		selection = "▸ "
	}
	titlePrefix := selection + role
	titleAvailable := maxInt(1, inner-cellWidth(titlePrefix)-cellWidth(icon)-1)
	title := graphSingleLine(node.Title)
	line1 := titlePrefix + statusStyle.Render(icon) + " " + t.CardTitle.Render(truncateDisplay(title, titleAvailable))
	line1 = padDisplay(truncateDisplay(line1, inner), inner)

	line2 := t.CardBody.Render(truncateOrPadDisplay(graphSingleLine(node.NodeID+" · "+node.Kind), inner))
	execution := node.AgentID
	if execution == "" {
		execution = node.ActivationID
	}
	if execution == "" && node.WaitEvent != "" {
		execution = "waits " + node.WaitEvent
	}
	if execution == "" && node.RequestID != "" {
		execution = "approval"
	}
	if execution == "" && node.ChildGraphID != "" {
		execution = "subgraph " + node.ChildGraphID
	}
	state := statusStyle.Render(node.Status)
	if execution != "" {
		state += t.CardBody.Render(" · " + graphSingleLine(execution))
	}
	line3 := padDisplay(truncateDisplay(state, inner), inner)

	return []string{
		top,
		border.Render(vertical) + line1 + border.Render(vertical),
		border.Render(vertical) + line2 + border.Render(vertical),
		border.Render(vertical) + line3 + border.Render(vertical),
		bottom,
	}
}

func graphNodeBorderStyle(status string, selected bool) lipgloss.Style {
	color := lipgloss.Color("237")
	switch status {
	case "running":
		color = lipgloss.Color("82")
	case "ready":
		color = lipgloss.Color("39")
	case "waiting", "blocked":
		color = lipgloss.Color("214")
	case "completed":
		color = lipgloss.Color("35")
	case "failed":
		color = lipgloss.Color("196")
	}
	if selected {
		color = lipgloss.Color("87")
	}
	return lipgloss.NewStyle().Foreground(color)
}

func renderGraphConnectorBand(
	t Theme,
	w int,
	graph GraphInfo,
	layout graphTopologyLayout,
	visible map[string]graphTopologyNode,
	boundaryDepth int,
) ([]string, []string) {
	canvas := make([][]graphCanvasCell, 3)
	for row := range canvas {
		canvas[row] = make([]graphCanvasCell, maxInt(1, w))
	}
	var labels []ui.GraphEdgeView
	for edgeIndex, edge := range graph.Edges {
		if layout.backEdges[edgeIndex] || layout.historicalEdges[edgeIndex] {
			continue
		}
		fromDepth, fromOK := layout.nodeDepth[edge.From]
		toDepth, toOK := layout.nodeDepth[edge.To]
		if !fromOK || !toOK || fromDepth > boundaryDepth || toDepth <= boundaryDepth {
			continue
		}
		from, fromVisible := visible[edge.From]
		to, toVisible := visible[edge.To]
		if !fromVisible || !toVisible {
			continue
		}
		kind := graphEdgeVisualKind(edge)
		drawGraphConnector(canvas, from.center, to.center, kind)
		if edge.When != "" && edge.When != "always" && toDepth == boundaryDepth+1 {
			labels = append(labels, edge)
		}
	}

	lines := make([]string, len(canvas))
	for row := range canvas {
		lines[row] = renderGraphCanvasRow(t, canvas[row])
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, renderGraphEdgeLabels(t, w, labels, false)
}

func drawGraphConnector(canvas [][]graphCanvasCell, fromX, toX int, kind graphEdgeKind) {
	if len(canvas) < 3 || len(canvas[0]) == 0 {
		return
	}
	fromX = minInt(len(canvas[0])-1, maxInt(0, fromX))
	toX = minInt(len(canvas[0])-1, maxInt(0, toX))
	mergeGraphCell(&canvas[0][fromX], graphLinkUp|graphLinkDown, false, kind)
	if fromX == toX {
		mergeGraphCell(&canvas[1][fromX], graphLinkUp|graphLinkDown, false, kind)
	} else if fromX < toX {
		mergeGraphCell(&canvas[1][fromX], graphLinkUp|graphLinkRight, false, kind)
		for x := fromX + 1; x < toX; x++ {
			mergeGraphCell(&canvas[1][x], graphLinkLeft|graphLinkRight, false, kind)
		}
		mergeGraphCell(&canvas[1][toX], graphLinkLeft|graphLinkDown, false, kind)
	} else {
		mergeGraphCell(&canvas[1][fromX], graphLinkUp|graphLinkLeft, false, kind)
		for x := toX + 1; x < fromX; x++ {
			mergeGraphCell(&canvas[1][x], graphLinkLeft|graphLinkRight, false, kind)
		}
		mergeGraphCell(&canvas[1][toX], graphLinkRight|graphLinkDown, false, kind)
	}
	mergeGraphCell(&canvas[2][toX], graphLinkUp, true, kind)
}

func mergeGraphCell(cell *graphCanvasCell, links uint8, arrow bool, kind graphEdgeKind) {
	cell.links |= links
	cell.arrow = cell.arrow || arrow
	if kind > cell.kind {
		cell.kind = kind
	}
}

func renderGraphCanvasRow(t Theme, row []graphCanvasCell) string {
	last := -1
	for index, cell := range row {
		if cell.links != 0 || cell.arrow {
			last = index
		}
	}
	if last < 0 {
		return ""
	}
	var line strings.Builder
	for index := 0; index <= last; index++ {
		cell := row[index]
		if cell.links == 0 && !cell.arrow {
			line.WriteByte(' ')
			continue
		}
		glyph := graphConnectorGlyph(cell)
		line.WriteString(graphEdgeStyle(t, cell.kind).Render(glyph))
	}
	return line.String()
}

func graphConnectorGlyph(cell graphCanvasCell) string {
	if cell.arrow {
		return "▼"
	}
	switch cell.kind {
	case graphEdgeCurrent:
		if glyph := graphHeavyGlyphs[cell.links]; glyph != "" {
			return glyph
		}
	case graphEdgeTraversed:
		if glyph := graphDoubleGlyphs[cell.links]; glyph != "" {
			return glyph
		}
	}
	if glyph := graphLightGlyphs[cell.links]; glyph != "" {
		return glyph
	}
	return "┼"
}

func graphEdgeVisualKind(edge ui.GraphEdgeView) graphEdgeKind {
	if edge.Current {
		return graphEdgeCurrent
	}
	if edge.Traversed {
		return graphEdgeTraversed
	}
	return graphEdgePending
}

func graphEdgeStyle(t Theme, kind graphEdgeKind) lipgloss.Style {
	switch kind {
	case graphEdgeCurrent:
		return t.MdH2
	case graphEdgeTraversed:
		return t.TaskCompleted
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	}
}

func renderGraphEdgeLabels(t Theme, w int, edges []ui.GraphEdgeView, back bool) []string {
	if len(edges) == 0 {
		return nil
	}
	parts := make([]string, 0, len(edges))
	for _, edge := range edges {
		arrow := "→"
		if back {
			arrow = "↩"
		}
		condition := ""
		if edge.When != "" && edge.When != "always" {
			condition = " [" + graphSingleLine(edge.When) + "]"
		}
		label := fmt.Sprintf("%s%s %s %s", edge.From, condition, arrow, edge.To)
		parts = append(parts, graphEdgeStyle(t, graphEdgeVisualKind(edge)).Render(label))
	}
	return wrapGraphLabels(parts, w)
}

func renderGraphSpecialEdges(t Theme, w int, graph GraphInfo, layout graphTopologyLayout) []string {
	var backEdges []ui.GraphEdgeView
	var historical []ui.GraphEdgeView
	for index, edge := range graph.Edges {
		if layout.historicalEdges[index] {
			historical = append(historical, edge)
			continue
		}
		if layout.backEdges[index] {
			backEdges = append(backEdges, edge)
		}
	}
	lines := renderGraphEdgeLabels(t, w, backEdges, true)
	if len(historical) == 0 {
		return lines
	}
	parts := make([]string, 0, len(historical))
	for _, edge := range historical {
		parts = append(parts, t.StateInteraction.Render(fmt.Sprintf(
			"history %s → %s", graphSingleLine(edge.From), graphSingleLine(edge.To))))
	}
	return append(lines, wrapGraphLabels(parts, w)...)
}

func wrapGraphLabels(parts []string, w int) []string {
	if len(parts) == 0 || w <= 0 {
		return nil
	}
	var lines []string
	current := ""
	for _, part := range parts {
		part = truncateDisplay(part, w)
		candidate := part
		if current != "" {
			candidate = current + "   " + part
		}
		if current != "" && cellWidth(candidate) > w {
			lines = append(lines, current)
			current = part
		} else {
			current = candidate
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func graphSingleLine(value string) string {
	value = sanitizeTerminalText(value)
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\t", " ")
	return strings.Join(strings.Fields(value), " ")
}

func graphCurrentEdgeProgress(graph GraphInfo) (traversed, total int) {
	for _, edge := range graph.Edges {
		if edge.When == "historical" {
			continue
		}
		total++
		if edge.Traversed {
			traversed++
		}
	}
	return traversed, total
}

func graphEndNodeSummary(graph GraphInfo) string {
	var ends []string
	for _, node := range graph.Nodes {
		if node.Kind == "end" {
			ends = append(ends, node.NodeID)
		}
	}
	if len(ends) == 0 {
		outgoing := make(map[string]bool, len(graph.Edges))
		for _, edge := range graph.Edges {
			outgoing[edge.From] = true
		}
		for _, node := range graph.Nodes {
			if !outgoing[node.NodeID] {
				ends = append(ends, node.NodeID)
			}
		}
	}
	if len(ends) == 0 {
		return "—"
	}
	if len(ends) > 3 {
		return strings.Join(ends[:3], ",") + fmt.Sprintf(" +%d", len(ends)-3)
	}
	return strings.Join(ends, ",")
}
