package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 输入边界与数量上限常量（校验链阶段 1 与阶段 4 使用）。
const (
	MaxDocumentBytes = 1 << 20 // 单张图 JSON 大小上限 1 MiB
	MaxDepth         = 32      // JSON 最大嵌套深度
	MaxNodes         = 512     // 单图节点数上限
	MaxNextPerNode   = 32      // 单节点 next 转移数上限
	MaxIDLength      = 128     // graph_id 与节点 ID 长度上限
)

// idCharset 是节点 ID 的合法字符集（字母、数字与 . _ : -）。
// 节点 ID 刻意不含 "@"（activation_id 的分隔符）与 "/"（子图 ID 的分段符）。
var idCharset = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// graphIDSegmentCharset 是 graph_id 单个分段的合法字符集：在节点 ID 字符集
// 之上多允许 "@"——subgraph 物化子图的末段是 activation_id 形态 <nodeID>@<n>。
var graphIDSegmentCharset = regexp.MustCompile(`^[A-Za-z0-9._:@-]+$`)

// validateGraphID 校验 graph_id：总长度受限，按 "/" 分段，每段非空、合法、
// 不得为 "." / ".."（路径穿越防护，store.graphDir 另有防御性复查）。
// 顶层图通常无 "/" 段；物化子图 ID 形如 <父图ID>/<activationID>。
func validateGraphID(graphID string) error {
	if graphID == "" {
		return fmt.Errorf("graph_id 不能为空")
	}
	if len(graphID) > MaxIDLength {
		return fmt.Errorf("graph_id 长度 %d 超过上限 %d", len(graphID), MaxIDLength)
	}
	for _, seg := range strings.Split(graphID, "/") {
		if seg == "" {
			return fmt.Errorf("graph_id %q 含空分段（不允许连续或首尾的 \"/\"）", graphID)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("graph_id %q 含非法分段 %q", graphID, seg)
		}
		if !graphIDSegmentCharset.MatchString(seg) {
			return fmt.Errorf("graph_id %q 的分段 %q 含非法字符（仅允许字母、数字与 . _ : - @）", graphID, seg)
		}
	}
	return nil
}

// ValidateGraphID exposes the canonical Graph identity validation to control
// planes that must reserve Graph-scoped resources before the full document is
// submitted (for example provision_agent_team).
func ValidateGraphID(graphID string) error { return validateGraphID(graphID) }

// ValidationError 是带校验阶段与 JSON 路径的结构化校验错误。
type ValidationError struct {
	Stage string // 校验阶段名，如 "重复键"
	Path  string // 出错位置的 JSON 路径，如 nodes.verify.next[0]
	Msg   string // 具体原因（含路径展示）
}

// Error 渲染为「校验[阶段]: 消息」。
func (e *ValidationError) Error() string {
	return fmt.Sprintf("校验[%s]: %s", e.Stage, e.Msg)
}

// newErr 构造校验错误；format 中应自然包含路径展示。
func newErr(stage, path, format string, args ...any) *ValidationError {
	return &ValidationError{Stage: stage, Path: path, Msg: fmt.Sprintf(format, args...)}
}

// ParseAndValidate 解析并校验一份 JSON GraphDocument。
//
// 校验链按固定阶段顺序执行，任一阶段失败即返回 *ValidationError：
//  1. 输入边界：大小上限与嵌套深度（流式扫描）；
//  2. JSON 语法 + 类型化解码（DisallowUnknownFields，未知核心字段在此拒绝；
//     extensions 内的 RawMessage 不受限）；
//  3. 重复 object key 检测（encoding/json 不查重，独立流式走查并报出路径）；
//  4. 基本字段：schema 恰为 "agentgo.graph/v1" 或 "agentgo.graph/v2"、
//     graph_id 非空且字符集合法、
//     revision/state_version 非负、节点数/单节点 next 数/ID 长度上限；
//  5. root：唯一、非空、指向存在的节点；
//  6. 转移：next.to 引用存在、when 仅两种形态、operator 枚举、activation 仅 "new"；
//     authoring 期另校验节点类型可产生的事件（如 join 不透传上游 event）；
//  7. 可达性：所有节点必须能从 root 到达（允许回边，不做无环校验）；
//  8. 节点语义：kind 枚举、end 无 next、非 end 必有 next、task.title 非空；
//  9. 能力形状：isolation 仅允许 "workspace"、tools 无空项、
//     executor 形状（type 仅 "agent"、agent_id 非空）。遗留 capability.budget
//     与 task.output_schema 由阶段 2 的 DisallowUnknownFields 按未知字段拒绝——
//     两个占位都没有 Runtime 消费者，删除以避免虚假契约；
//  10. 初始字段所有权：图必须 pending、state_version=0；节点必须
//     inactive 且 executor/execution 为 null。
func ParseAndValidate(data []byte) (*GraphDocument, error) {
	// 阶段 1：输入边界（大小 + 流式深度扫描）
	if len(data) == 0 {
		return nil, newErr("输入边界", "", "输入为空")
	}
	if len(data) > MaxDocumentBytes {
		return nil, newErr("输入边界", "", "大小 %d 字节超过上限 %d 字节", len(data), MaxDocumentBytes)
	}
	if depth := maxJSONDepth(data); depth > MaxDepth {
		return nil, newErr("输入边界", "", "嵌套深度 %d 超过上限 %d", depth, MaxDepth)
	}

	// 阶段 2：JSON 语法 + 类型化解码（拒绝未知核心字段）
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc GraphDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, classifyDecodeError(err)
	}
	if dec.More() {
		return nil, newErr("JSON语法", "", "文档结束后存在多余内容")
	}

	// 阶段 3：重复 object key 检测（报出路径）
	if path, key, dup := findDuplicateKey(data); dup {
		if path == "" {
			return nil, newErr("重复键", path, "顶层的 %q 重复出现", key)
		}
		return nil, newErr("重复键", path, "%s 的 %q 重复出现", path, key)
	}

	// 阶段 4–9：与 JSON 形态无关的语义校验链（抽出供 GraphStore 复用）
	if err := validateSemantics(&doc); err != nil {
		return nil, err
	}
	if err := validateAuthoringSemantics(&doc); err != nil {
		return nil, err
	}

	// 阶段 10：初始提交的字段所有权。Scheduler 只能提交定义面；运行面必须
	// 是唯一的初始零状态，不能借整图 JSON 伪造 running/completed、认领者或
	// activation。Store.Recover 不走本入口，而是 validateRuntimeState 校验已
	// 持久化的真实运行状态。
	if err := validateInitialOwnership(&doc); err != nil {
		return nil, err
	}

	return &doc, nil
}

// validateInitialOwnership 强制 V6 §6-9 的初始写入边界。GraphDocument 的
// JSON 为了展示完整对象仍显式携带 status/executor/execution，但 Scheduler
// 只能提供 pending/inactive/null 这些初始值；state_version 必须从 0 开始。
// 内联 subgraph 也是未来将被物化的新图，递归执行同一节点运行面约束。
func validateInitialOwnership(doc *GraphDocument) error {
	if !doc.Status.IsValid() {
		return newErr("初始状态", "status", "非法图状态 %q", doc.Status)
	}
	if doc.Status != GraphPending {
		return newErr("字段所有权", "status", "Scheduler 提交的新图 status 必须为 %q，实际为 %q", GraphPending, doc.Status)
	}
	if doc.StateVersion != 0 {
		return newErr("字段所有权", "state_version", "Scheduler 不得写运行版本：新图 state_version 必须为 0，实际为 %d", doc.StateVersion)
	}
	return validateInitialNodes(doc.Nodes, "nodes")
}

func validateInitialNodes(nodes map[string]Node, prefix string) error {
	for _, id := range sortedNodeKeys(nodes) {
		node := nodes[id]
		path := prefix + "." + id
		if !node.Status.IsValid() {
			return newErr("初始状态", path+".status", "非法节点状态 %q", node.Status)
		}
		if node.Status != NodeInactive {
			return newErr("字段所有权", path+".status", "Scheduler 不得写节点运行状态：新节点 %q 的 status 必须为 %q，实际为 %q", id, NodeInactive, node.Status)
		}
		if node.Executor != nil {
			return newErr("字段所有权", path+".executor", "Scheduler 不得写节点 executor：新节点 %q 必须为 null", id)
		}
		if node.Execution != nil {
			return newErr("字段所有权", path+".execution", "Scheduler 不得写节点 execution：新节点 %q 必须为 null", id)
		}
		if node.Subgraph != nil {
			if err := validateInitialNodes(node.Subgraph.Nodes, path+".subgraph.nodes"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateSemantics 在内存 GraphDocument 上执行 ParseAndValidate 的阶段 4–9
// （基本字段/root/转移/可达性/节点语义/类型专属规格/能力形状，与 JSON 形态无关）。
// GraphStore 在变更后的候选副本上复用同一校验链，保证「进程内对象」与
// 「JSON 输入」接受完全一致的定义面约束。
func validateSemantics(doc *GraphDocument) error {
	return validateSemanticsDepth(doc, 1)
}

// validateSemanticsDepth 同 validateSemantics，但携带 subgraph 嵌套深度
// （顶层图为 1；内联子图递归校验时 depth+1，超过 MaxSubgraphDepth 拒绝）。
func validateSemanticsDepth(doc *GraphDocument, depth int) error {
	// 阶段 4：基本字段与数量上限
	if err := validateBasics(doc); err != nil {
		return err
	}

	// 阶段 5：root 唯一、非空、指向存在的节点
	if doc.Root == "" {
		return newErr("root", "root", "root 不能为空：每张图有且只有一个 root")
	}
	if _, ok := doc.Nodes[doc.Root]; !ok {
		return newErr("root", "root", "root 指向不存在的节点 %q", doc.Root)
	}

	// 阶段 6：转移引用与条件形态
	if err := validateTransitions(doc); err != nil {
		return err
	}

	// 阶段 7：从 root 的可达性（允许回边，不查环）
	if err := validateReachability(doc); err != nil {
		return err
	}

	// 阶段 8：节点类型与 next 规则
	if err := validateNodeSemantics(doc); err != nil {
		return err
	}

	// 阶段 8.5：类型专属规格（wait/tool/subgraph 的必备与错配检查；内联子图递归）
	if err := validateNodesKindSpecs(doc.Nodes, depth); err != nil {
		return err
	}

	// 阶段 9：能力与 executor 结构形状
	return validateCapabilityShape(doc)
}

// validateRuntimeState 校验运行状态字段的枚举合法性（图与节点 status）。
// 与 ParseAndValidate 阶段 10 不同，这里不做「新图仅 pending/running」
// 限制——GraphStore 中的图会随运行推进到任意合法状态。
func validateRuntimeState(doc *GraphDocument) error {
	if !doc.Status.IsValid() {
		return newErr("初始状态", "status", "非法图状态 %q", doc.Status)
	}
	for _, id := range sortedNodeIDs(doc) {
		node := doc.Nodes[id]
		if st := node.Status; !st.IsValid() {
			return newErr("初始状态", "nodes."+id+".status", "非法节点状态 %q", st)
		}
		if node.Execution == nil || node.Execution.Settlement == nil {
			continue
		}
		settlement := node.Execution.Settlement
		path := "nodes." + id + ".execution.settlement"
		switch settlement.Status {
		case NodeCompleted, NodeFailed, NodeBlocked, NodeCancelled:
		default:
			return newErr("运行状态", path+".status", "节点 %q 的结算状态 %q 非法", id, settlement.Status)
		}
		if node.Status != settlement.Status {
			return newErr("运行状态", path+".status", "节点 %q 当前状态 %q 与结算状态 %q 不一致", id, node.Status, settlement.Status)
		}
		switch settlement.Continuation {
		case SettlementContinueTransitions, SettlementContinueGraphComplete, SettlementContinueGraphFail, SettlementContinueNone:
		default:
			return newErr("运行状态", path+".continuation", "节点 %q 的结算续跑动作 %q 非法", id, settlement.Continuation)
		}
		if settlement.ResultRef == "" {
			var result map[string]any
			if len(settlement.Result) == 0 || json.Unmarshal(settlement.Result, &result) != nil || result == nil {
				return newErr("运行状态", path+".result", "节点 %q 的结算必须携带 ResultRef，或为旧记录携带合法 JSON 对象 Result", id)
			}
		} else if strings.TrimSpace(settlement.ResultRef) != settlement.ResultRef {
			return newErr("运行状态", path+".result_ref", "节点 %q 的结算 ResultRef 不得携带首尾空白", id)
		}
		if settlement.Continuation == SettlementContinueGraphFail && strings.TrimSpace(settlement.Reason) == "" {
			return newErr("运行状态", path+".reason", "节点 %q 的 graph_fail 结算必须携带原因", id)
		}
	}
	return nil
}

// validateBasics 实现阶段 4：schema、graph_id、版本号与数量/长度上限。
// schema 只接受 SchemaV1 / SchemaV2 两个封闭值；v2 文档的追加约束（事件
// 词表、输出契约声明）在 authoring 阶段按版本分流，见 validateAuthoringNodes。
func validateBasics(doc *GraphDocument) error {
	if doc.Schema != SchemaV1 && doc.Schema != SchemaV2 {
		return newErr("基本字段", "schema", "schema 必须恰为 %q 或 %q，实际为 %q", SchemaV1, SchemaV2, doc.Schema)
	}
	if err := validateGraphID(doc.GraphID); err != nil {
		return newErr("基本字段", "graph_id", "%s", err.Error())
	}
	if doc.Revision < 0 {
		return newErr("基本字段", "revision", "revision 必须非负，实际为 %d", doc.Revision)
	}
	if doc.StateVersion < 0 {
		return newErr("基本字段", "state_version", "state_version 必须非负，实际为 %d", doc.StateVersion)
	}
	if len(doc.Nodes) == 0 {
		return newErr("基本字段", "nodes", "nodes 不能为空：每张图至少一个节点")
	}
	if len(doc.Nodes) > MaxNodes {
		return newErr("基本字段", "nodes", "节点数 %d 超过上限 %d", len(doc.Nodes), MaxNodes)
	}
	for _, id := range sortedNodeIDs(doc) {
		if id == "" {
			return newErr("基本字段", "nodes", "节点 ID 不能为空")
		}
		if len(id) > MaxIDLength {
			return newErr("基本字段", "nodes."+id, "节点 ID 长度 %d 超过上限 %d", len(id), MaxIDLength)
		}
		if !idCharset.MatchString(id) {
			return newErr("基本字段", "nodes."+id, "节点 ID %q 含非法字符（仅允许字母、数字与 . _ : -）", id)
		}
		if n := len(doc.Nodes[id].Next); n > MaxNextPerNode {
			return newErr("基本字段", "nodes."+id+".next", "单节点 next 数 %d 超过上限 %d", n, MaxNextPerNode)
		}
	}
	return nil
}

// validateTransitions 实现阶段 6：next.to 引用存在、activation 枚举、when 形态。
func validateTransitions(doc *GraphDocument) error {
	for _, id := range sortedNodeIDs(doc) {
		node := doc.Nodes[id]
		for i, tr := range node.Next {
			path := fmt.Sprintf("nodes.%s.next[%d]", id, i)
			if tr.To == "" {
				return newErr("转移", path, "%s 的 to 不能为空", path)
			}
			if _, ok := doc.Nodes[tr.To]; !ok {
				return newErr("转移", path+".to", "%s 的 to 指向不存在的节点 %q", path, tr.To)
			}
			if tr.Activation != "" && tr.Activation != ActivationNew {
				return newErr("转移", path+".activation", "%s 的 activation 仅允许 %q，实际为 %q", path, ActivationNew, tr.Activation)
			}
			if tr.TargetInput != "" {
				if len(tr.TargetInput) > MaxIDLength || !idCharset.MatchString(tr.TargetInput) {
					return newErr("转移", path+".target_input", "%s 的 target_input %q 非法（仅允许字母、数字与 . _ : -，长度 ≤ %d）", path, tr.TargetInput, MaxIDLength)
				}
				target := doc.Nodes[tr.To]
				if target.Kind != KindJoin && target.Kind != KindAcceptance {
					return newErr("转移", path+".target_input", "%s 仅在目标为 join / acceptance 时可声明 target_input，目标 %q 类型为 %s", path, tr.To, target.Kind)
				}
			}
			if tr.When != nil {
				if err := validateCondition(path+".when", tr.When); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateAuthoringSemantics 校验只适用于新提交/patch 的定义约束。它与
// validateSemantics 分开，避免升级后新增的 authoring 规则让已经 durable 的
// 历史 Graph 无法 Recover；旧图仍按其冻结契约恢复，新定义则 fail-closed。
// schema v2 文档在此追加终态契约 v2 的边条件规则（v1 文档不受限）。
func validateAuthoringSemantics(doc *GraphDocument) error {
	return validateAuthoringNodes(doc.Nodes, "nodes", doc.Schema == SchemaV2)
}

func validateAuthoringNodes(nodes map[string]Node, prefix string, v2 bool) error {
	type inboundEdge struct {
		source string
		index  int
		input  string
	}
	inbound := make(map[string][]inboundEdge)
	for _, sourceID := range sortedNodeKeys(nodes) {
		for i, tr := range nodes[sourceID].Next {
			inbound[tr.To] = append(inbound[tr.To], inboundEdge{source: sourceID, index: i, input: tr.TargetInput})
		}
	}
	for _, id := range sortedNodeKeys(nodes) {
		node := nodes[id]
		path := prefix + "." + id
		edges := inbound[id]
		if node.Kind != KindJoin && node.Kind != KindAcceptance && len(edges) > 1 {
			edge := edges[1]
			return newErr("转移", fmt.Sprintf("%s.%s.next[%d]", prefix, edge.source, edge.index),
				"%s 是非 barrier 节点，但存在 %d 条静态入边；普通节点的 Input 在首次激活时冻结，当前仅允许单一生产边。条件分支请保持各自后续/各自 end，等待 generation/correlation token 后再开放 OR mux", path, len(edges))
		}
		if node.Kind == KindJoin || node.Kind == KindAcceptance {
			required := []string(nil)
			if node.Task != nil {
				required = node.Task.RequiredInputs
			}
			if len(required) == 0 && len(edges) > 1 {
				for _, edge := range edges {
					if edge.input == "" {
						return newErr("转移", fmt.Sprintf("%s.%s.next[%d].target_input", prefix, edge.source, edge.index),
							"%s 有多条直接入边；每条入边都必须用唯一 target_input 声明端口（不同端口构成 AND barrier），也可在目标 task.required_inputs 显式列出必需端口", path)
					}
				}
			}
			if len(required) > 0 {
				allowed := make(map[string]struct{}, len(required))
				for _, input := range required {
					allowed[input] = struct{}{}
				}
				supplied := make(map[string]inboundEdge, len(edges))
				for _, edge := range edges {
					if edge.input == "" {
						return newErr("转移", fmt.Sprintf("%s.%s.next[%d].target_input", prefix, edge.source, edge.index),
							"指向 %s 的入边必须声明 target_input；目标要求端口 %v", path, required)
					}
					if _, ok := allowed[edge.input]; !ok {
						return newErr("转移", fmt.Sprintf("%s.%s.next[%d].target_input", prefix, edge.source, edge.index),
							"指向 %s 的入边端口 %q 未列入目标 task.required_inputs %v；条件分支须保持各自后续/各自 end，OR mux 待 generation/correlation token 后再开放", path, edge.input, required)
					}
					if previous, exists := supplied[edge.input]; exists {
						return newErr("转移", fmt.Sprintf("%s.%s.next[%d].target_input", prefix, edge.source, edge.index),
							"指向 %s 的端口 %q 已由 %s.%s.next[%d] 声明；输入端口是单赋值，条件分支须保持各自后续/各自 end，OR mux 待 generation/correlation token 后再开放", path, edge.input, prefix, previous.source, previous.index)
					}
					supplied[edge.input] = edge
				}
				for _, input := range required {
					if _, ok := supplied[input]; !ok {
						return newErr("节点", path+".task.required_inputs", "%s 要求输入端口 %q，但没有任何入边写入该端口", path, input)
					}
				}
			}
			if len(required) == 0 {
				seenPort := make(map[string]inboundEdge, len(edges))
				for _, edge := range edges {
					if edge.input == "" {
						continue
					}
					if previous, exists := seenPort[edge.input]; exists {
						return newErr("转移", fmt.Sprintf("%s.%s.next[%d].target_input", prefix, edge.source, edge.index),
							"指向 %s 的端口 %q 已由 %s.%s.next[%d] 声明；输入端口是单赋值，条件分支须保持各自后续/各自 end，OR mux 待 generation/correlation token 后再开放", path, edge.input, prefix, previous.source, previous.index)
					}
					seenPort[edge.input] = edge
				}
			}
		}
		if node.Kind == KindJoin {
			for i, tr := range node.Next {
				if tr.When == nil || tr.When.Event == "" || tr.When.Event == EventCompleted || tr.When.Event == EventAlways {
					continue
				}
				edgePath := fmt.Sprintf("%s.next[%d].when.event", path, i)
				return newErr("转移", edgePath,
					"%s=%q 永远无法匹配：join 节点不透传上游 event，成功汇合只产生终态事件 %q；请改用 event=%q/always，或用 path 条件检查按目标输入端口归并的结果",
					edgePath, tr.When.Event, EventCompleted, EventCompleted)
			}
		}
		if node.Kind == KindAcceptance {
			for i, tr := range node.Next {
				if err := validateAcceptanceOutgoingTransition(fmt.Sprintf("%s.next[%d]", path, i), tr); err != nil {
					return err
				}
			}
		}
		if v2 && (node.Kind == KindAgent || node.Kind == KindController) {
			for i, tr := range node.Next {
				if err := validateV2WorkerOutgoingTransition(fmt.Sprintf("%s.next[%d]", path, i), id, node, tr); err != nil {
					return err
				}
			}
		}
		if node.Subgraph != nil {
			if err := validateAuthoringNodes(node.Subgraph.Nodes, path+".subgraph.nodes", v2); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateAcceptanceOutgoingTransition 禁止 acceptance 用无条件、always 或
// completed 边绕过 verdict。completed 的业务结论只允许精确比较
// $.verdict；Runtime 自身产生的 failed/blocked 终态可以用同名事件边
// 收敛。二者分属业务结论和基础设施失败，不共用事件词。
func validateAcceptanceOutgoingTransition(path string, tr Transition) error {
	if tr.When == nil {
		return newErr("转移", path+".when", "%s 的 acceptance 出边必须显式按 $.verdict 路由，或处理 Runtime failed/blocked 事件，不能无条件放行", path)
	}
	when := tr.When
	if when.Event != "" {
		switch when.Event {
		case EventFailed, EventBlocked:
			return nil
		default:
			return newErr("转移", path+".when.event", "%s 的 acceptance 事件边只允许 Runtime 产生的 failed/blocked；业务结论必须走 $.verdict eq", path)
		}
	}
	if when.Path != "$.verdict" || when.Operator != OpEq {
		return newErr("转移", path+".when", "%s 的 acceptance 路径边必须用 $.verdict eq <pass|fixable|failed>，不得使用 in 或按其它字段放行", path)
	}
	var verdict string
	if err := json.Unmarshal(when.Value, &verdict); err != nil {
		return newErr("转移", path+".when.value", "%s 的 acceptance verdict eq 值必须是字符串", path)
	}
	if !isValidAcceptanceVerdict(verdict) {
		return newErr("转移", path+".when.value", "%s 引用了非法 acceptance verdict %q（仅 pass/fixable/failed）", path, verdict)
	}
	return nil
}

// v2WorkerSystemEvents 是 schema v2 文档中 agent/controller 节点出边允许的
// 系统事件集（终态契约 v2 §4）：节点 status 的镜像加无条件 always，用于
// 不需要业务语义的边（如失败兜底）。业务结论不再用事件名表达，一律走
// result 数据字段 + path 条件。
var v2WorkerSystemEvents = map[string]struct{}{
	EventCompleted: {},
	EventFailed:    {},
	EventBlocked:   {},
	EventAlways:    {},
}

// validateV2WorkerOutgoingTransition 实现终态契约 v2 §4 对 agent/controller
// 出边的两条追加约束（仅 schema v2 文档适用，v1 文档逐字节沿用旧规则）：
//   - 事件词表：when.event 只允许系统事件 completed/failed/blocked/always；
//     ready/pass/fixable/approved/rejected/timeout 一律拒绝；
//   - 输出契约：path 条件引用的根字段名（"$." 后第一段）必须以子串形式
//     出现在该节点 task.description 中——刻意简单的 v1 版声明约定，拒绝
//     「边引用了生产者没承诺的字段」。when 为 nil 或纯系统事件边不受此限。
func validateV2WorkerOutgoingTransition(path, id string, node Node, tr Transition) error {
	if tr.When == nil {
		return nil
	}
	if tr.When.Event != "" {
		if _, ok := v2WorkerSystemEvents[tr.When.Event]; !ok {
			return newErr("事件词表", path+".when.event",
				"%s 的 event %q 不是 schema v2 系统事件：%s 节点出边只允许 completed/failed/blocked/always；业务路由请改用 result 数据字段 + path 条件",
				path, tr.When.Event, node.Kind)
		}
		return nil
	}
	field := pathRootField(tr.When.Path)
	desc := ""
	if node.Task != nil {
		desc = node.Task.Description
	}
	if !strings.Contains(desc, field) {
		return newErr("输出契约", path+".when.path",
			"节点 %s 的出边 %s 引用了 result 字段 %q，但其 task.description 未声明该字段——请在描述中写明输出契约（本节点 result 必须包含哪些字段及合法取值）",
			id, path, field)
	}
	return nil
}

// pathRootField 提取 "$.a.b" 形态路径的根字段名（"$." 后第一段）。
func pathRootField(path string) string {
	rest := strings.TrimPrefix(path, "$.")
	if i := strings.IndexByte(rest, '.'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// validateCondition 校验 when 只允许两种形态：事件形态或条件形态。
func validateCondition(path string, c *Condition) error {
	hasEvent := c.Event != ""
	hasPath := c.Path != ""
	switch {
	case hasEvent && hasPath:
		return newErr("转移", path, "%s 只能是事件形态或条件形态之一，event 与 path 不得同时出现", path)
	case hasEvent:
		if c.Operator != "" || len(c.Value) > 0 {
			return newErr("转移", path, "%s 为事件形态，不得携带 operator/value", path)
		}
		if !IsValidEventName(c.Event) {
			return newErr("转移", path, "%s 的 event %q 未知（可选: ready/completed/fixable/failed/blocked/pass/approved/rejected/timeout/always）", path, c.Event)
		}
	case hasPath:
		if !strings.HasPrefix(c.Path, "$.") {
			return newErr("转移", path, "%s 的 path 必须以 \"$.\" 开头（如 $.verdict），实际为 %q", path, c.Path)
		}
		switch c.Operator {
		case OpEq, OpNe:
			if len(c.Value) == 0 {
				return newErr("转移", path, "%s 的 operator %q 需要 value", path, c.Operator)
			}
			if !isJSONScalar(c.Value) {
				return newErr("转移", path, "%s 的 operator %q 的 value 必须是 JSON 标量", path, c.Operator)
			}
		case OpIn:
			if len(c.Value) == 0 {
				return newErr("转移", path, "%s 的 operator %q 需要 value", path, c.Operator)
			}
			if !isJSONStringList(c.Value) {
				return newErr("转移", path, "%s 的 operator %q 的 value 必须是字符串数组", path, c.Operator)
			}
		case OpExists:
			if len(c.Value) > 0 {
				return newErr("转移", path, "%s 的 operator %q 只判定路径存在性，不得携带 value", path, c.Operator)
			}
		default:
			return newErr("转移", path, "%s 的 operator %q 未知（可选: eq/ne/in/exists）", path, c.Operator)
		}
	default:
		return newErr("转移", path, "%s 为空：必须是 {event:...} 或 {path,operator,value} 形态之一", path)
	}
	return nil
}

// validateReachability 实现阶段 7：BFS，所有节点必须能从 root 到达。
// 允许回边（visited 集合天然免疫环），不做无环校验。
func validateReachability(doc *GraphDocument) error {
	visited := map[string]bool{doc.Root: true}
	queue := []string{doc.Root}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, tr := range doc.Nodes[id].Next {
			if !visited[tr.To] {
				visited[tr.To] = true
				queue = append(queue, tr.To)
			}
		}
	}
	for _, id := range sortedNodeIDs(doc) {
		if !visited[id] {
			return newErr("可达性", "nodes."+id, "节点 %q 无法从 root %q 到达（允许回边，但所有节点必须可达）", id, doc.Root)
		}
	}
	return nil
}

// validateNodeSemantics 实现阶段 8：kind 枚举、end 无 next、非 end 必有 next、task.title 非空。
func validateNodeSemantics(doc *GraphDocument) error {
	for _, id := range sortedNodeIDs(doc) {
		node := doc.Nodes[id]
		path := "nodes." + id
		if !node.Kind.IsValid() {
			return newErr("节点", path+".kind", "未知节点类型 %q（首批支持 controller/agent/tool/router/join/approval/wait_event/acceptance/subgraph/end）", node.Kind)
		}
		if node.Kind == KindEnd {
			if len(node.Next) > 0 {
				return newErr("节点", path+".next", "end 节点 %q 的 next 必须为空", id)
			}
		} else if len(node.Next) == 0 {
			return newErr("节点", path+".next", "非 end 节点 %q（%s）的 next 不能为空", id, node.Kind)
		}
		requiresTask := node.Kind == KindController || node.Kind == KindAgent || node.Kind == KindAcceptance
		if requiresTask && node.Task == nil {
			return newErr("节点", path+".task", "节点 %q（%s）必须携带 task 与非空 title", id, node.Kind)
		}
		if node.Task != nil {
			if strings.TrimSpace(node.Task.Title) == "" {
				return newErr("节点", path+".task.title", "节点 %q 的 task.title 不能为空", id)
			}
			// 占位符防线（2026-08-19 SWE 实测：title="t"/description="d" 的
			// 占位图通过校验并被真实派发执行，explorer 收到乱码任务）。只拦
			// 「单个 ASCII 字母/数字」这种铁定无信息的占位符——单字中文
			// （如「源」「迁」）与一切正常标题不受影响；阈值宁可漏过也不
			// 误伤，避免防御机制本身成为新的错误源。
			if requiresTask && isPlaceholderToken(node.Task.Title) {
				return newErr("节点", path+".task.title", "节点 %q（%s）的 task.title=%q 疑似占位符，请填写有意义的任务标题", id, node.Kind, node.Task.Title)
			}
			if requiresTask && node.Task.Description != "" && isPlaceholderToken(node.Task.Description) {
				return newErr("节点", path+".task.description", "节点 %q（%s）的 task.description=%q 疑似占位符，请写明该节点要做什么", id, node.Kind, node.Task.Description)
			}
			if node.Kind == KindAcceptance && strings.TrimSpace(node.Task.Description) == "" {
				return newErr("节点", path+".task.description", "acceptance 节点 %q 的 task.description 必须写明可执行的验收判据", id)
			}
			if len(node.Task.RequiredInputs) > 0 && node.Kind != KindJoin && node.Kind != KindAcceptance {
				return newErr("节点", path+".task.required_inputs", "required_inputs 仅允许 join / acceptance 节点声明，节点 %q 类型为 %s", id, node.Kind)
			}
			seen := make(map[string]struct{}, len(node.Task.RequiredInputs))
			for i, input := range node.Task.RequiredInputs {
				reqPath := fmt.Sprintf("%s.task.required_inputs[%d]", path, i)
				if strings.TrimSpace(input) == "" {
					return newErr("节点", reqPath, "%s 不能为空", reqPath)
				}
				if len(input) > MaxIDLength || !idCharset.MatchString(input) {
					return newErr("节点", reqPath, "%s 的输入端口 %q 非法（仅允许字母、数字与 . _ : -，长度 ≤ %d）", reqPath, input, MaxIDLength)
				}
				if _, dup := seen[input]; dup {
					return newErr("节点", reqPath, "%s 重复声明输入端口 %q", reqPath, input)
				}
				seen[input] = struct{}{}
			}
			if len(node.Task.RequiredEvidence) > 0 && node.Kind != KindAcceptance {
				return newErr("节点", path+".task.required_evidence", "required_evidence 仅允许 acceptance 节点声明，节点 %q 类型为 %s", id, node.Kind)
			}
			evidenceSeen := make(map[string]struct{}, len(node.Task.RequiredEvidence))
			for i, req := range node.Task.RequiredEvidence {
				reqPath := fmt.Sprintf("%s.task.required_evidence[%d]", path, i)
				if _, ok := seen[req.Input]; !ok {
					return newErr("节点", reqPath+".input", "%s.input=%q 必须引用 required_inputs 中已声明的端口", reqPath, req.Input)
				}
				if strings.TrimSpace(req.Kind) == "" {
					return newErr("节点", reqPath+".kind", "%s.kind 不能为空", reqPath)
				}
				key := req.Input + "\x00" + req.Kind
				if _, dup := evidenceSeen[key]; dup {
					return newErr("节点", reqPath, "%s 重复声明 input=%q kind=%q", reqPath, req.Input, req.Kind)
				}
				evidenceSeen[key] = struct{}{}
			}
		}
	}
	return nil
}

// isPlaceholderToken 报告 s 去空白后是否为单个 ASCII 字母/数字（占位符特征）。
// 只匹配 ASCII——单字中文（「源」「迁」）携带真实语义，不误伤。
func isPlaceholderToken(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 1 {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// validateNodesKindSpecs 实现阶段 8.5：类型专属规格校验。
//   - wait_event 必须携带 wait（event 非空；timeout_sec 不得为负）；
//   - tool 必须携带 tool（name 非空）；
//   - subgraph 必须携带 subgraph，且内联子图递归走完整语义校验
//     （深度 +1，超过 MaxSubgraphDepth 拒绝）；
//   - 三类字段出现在不匹配的节点类型上一律拒绝。
func validateNodesKindSpecs(nodes map[string]Node, depth int) error {
	for _, id := range sortedNodeKeys(nodes) {
		node := nodes[id]
		path := "nodes." + id
		if node.Kind != KindWaitEvent && node.Wait != nil {
			return newErr("节点", path+".wait", "节点 %q（%s）不得携带 wait 字段（仅 wait_event 可用）", id, node.Kind)
		}
		if node.Kind != KindTool && node.Tool != nil {
			return newErr("节点", path+".tool", "节点 %q（%s）不得携带 tool 字段（仅 tool 可用）", id, node.Kind)
		}
		if node.Kind != KindSubgraph && node.Subgraph != nil {
			return newErr("节点", path+".subgraph", "节点 %q（%s）不得携带 subgraph 字段（仅 subgraph 可用）", id, node.Kind)
		}
		switch node.Kind {
		case KindWaitEvent:
			if node.Wait == nil {
				return newErr("节点", path, "wait_event 节点 %q 必须携带 wait 字段", id)
			}
			if strings.TrimSpace(node.Wait.Event) == "" {
				return newErr("节点", path+".wait.event", "wait_event 节点 %q 的 wait.event 不能为空", id)
			}
			if node.Wait.TimeoutSec < 0 {
				return newErr("节点", path+".wait.timeout_sec", "wait_event 节点 %q 的 wait.timeout_sec 必须为正数，实际为 %d", id, node.Wait.TimeoutSec)
			}
		case KindTool:
			if node.Tool == nil {
				return newErr("节点", path, "tool 节点 %q 必须携带 tool 字段", id)
			}
			if strings.TrimSpace(node.Tool.Name) == "" {
				return newErr("节点", path+".tool.name", "tool 节点 %q 的 tool.name 不能为空", id)
			}
		case KindSubgraph:
			if node.Subgraph == nil {
				return newErr("节点", path, "subgraph 节点 %q 必须携带 subgraph 字段", id)
			}
			if depth+1 > MaxSubgraphDepth {
				return newErr("节点", path+".subgraph", "subgraph 节点 %q 的嵌套深度 %d 超过上限 %d", id, depth+1, MaxSubgraphDepth)
			}
			// 内联子图递归走完整语义校验（root/引用/可达性/节点语义等）。
			// graph_id 用占位值——物化时才分配真实 ID（<父图ID>/<activationID>）。
			sub := &GraphDocument{
				Schema:  SchemaV1,
				GraphID: "inline",
				Status:  GraphPending,
				Root:    node.Subgraph.Root,
				Nodes:   node.Subgraph.Nodes,
			}
			if err := validateSemanticsDepth(sub, depth+1); err != nil {
				return newErr("节点", path+".subgraph", "subgraph 节点 %q 的内联子图非法: %s", id, err.Error())
			}
		}
	}
	return nil
}

// validateCapabilityShape 实现阶段 9：capability 与 executor 的结构形状。
func validateCapabilityShape(doc *GraphDocument) error {
	for _, id := range sortedNodeIDs(doc) {
		node := doc.Nodes[id]
		path := "nodes." + id
		if cap := node.Capability; cap != nil {
			if cap.Isolation != "" && cap.Isolation != IsolationWorkspace {
				return newErr("能力", path+".capability.isolation", "capability.isolation 仅允许 %q，实际为 %q", IsolationWorkspace, cap.Isolation)
			}
			for i, tool := range cap.Tools {
				if strings.TrimSpace(tool) == "" {
					return newErr("能力", fmt.Sprintf("%s.capability.tools[%d]", path, i), "capability.tools 不得含空字符串")
				}
			}
			// controller 是纯控制面（读上游结果、裁决、路由），不持有业务
			// 工具——声明了也无法兑现（租约层强制置空），图提交期 fail-closed
			//（2026-08-19 SWE 实测：scheduler 借 controller 节点自修业务代码）。
			if node.Kind == KindController && len(cap.Tools) > 0 {
				return newErr("能力", path+".capability.tools", "controller 节点 %q 不得声明 capability.tools（纯控制面无业务工具），实际为 %v", id, cap.Tools)
			}
		}
		if ex := node.Executor; ex != nil {
			if ex.Type != ExecutorTypeAgent {
				return newErr("能力", path+".executor.type", "executor.type 仅允许 %q，实际为 %q", ExecutorTypeAgent, ex.Type)
			}
			if strings.TrimSpace(ex.AgentID) == "" {
				return newErr("能力", path+".executor.agent_id", "executor.agent_id 不能为空")
			}
		}
	}
	return nil
}

// ============================================================
// 流式扫描与工具函数
// ============================================================

// maxJSONDepth 流式扫描 JSON 的最大嵌套深度；遇到语法错误即返回已测得的深度
// （语法问题统一由阶段 2 报告，此处只回答「深度是否超限」）。
func maxJSONDepth(data []byte) int {
	dec := json.NewDecoder(bytes.NewReader(data))
	depth, maxDepth := 0, 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return maxDepth
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth > maxDepth {
					maxDepth = depth
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// findDuplicateKey 流式走查原始 JSON，报告首个重复 object key 的位置。
// encoding/json 的 Decode 对重复 key 静默取后者，必须独立检测。
// 调用前文档已通过阶段 2 解码，不会再遇到语法错误；若仍遇到无法处理的 token
// （如 extensions 内溢出 float64 的数字），停止扫描并视为无重复。
func findDuplicateKey(data []byte) (path string, key string, found bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	type frame struct {
		isObj     bool
		expectKey bool // 对象层下一个 token 应为 key
		seen      map[string]struct{}
		pending   string // 对象层最近读到的 key（等待其值）
		idx       int    // 数组层下一个元素下标
	}
	var stack []frame
	var segs []string // 与 stack 并行：stack[i] 容器在父容器中的段名（root 无段名，len(segs)=len(stack)-1）

	for {
		tok, err := dec.Token()
		if err != nil {
			return "", "", false
		}

		// 对象层期待 key 的位置：string token 是 key，其余（空对象的 }）落入容器边界处理
		if len(stack) > 0 && stack[len(stack)-1].isObj && stack[len(stack)-1].expectKey {
			if k, ok := tok.(string); ok {
				top := &stack[len(stack)-1]
				if _, dup := top.seen[k]; dup {
					return renderPath(segs), k, true
				}
				top.seen[k] = struct{}{}
				top.pending = k
				top.expectKey = false
				continue
			}
		}

		switch delim := tok.(type) {
		case json.Delim:
			switch delim {
			case '{', '[':
				if len(stack) > 0 {
					top := &stack[len(stack)-1]
					if top.isObj {
						segs = append(segs, top.pending)
					} else {
						segs = append(segs, strconv.Itoa(top.idx))
						top.idx++
					}
				}
				if delim == '{' {
					stack = append(stack, frame{isObj: true, expectKey: true, seen: make(map[string]struct{})})
				} else {
					stack = append(stack, frame{})
				}
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 {
					segs = segs[:len(segs)-1]
					if top := &stack[len(stack)-1]; top.isObj {
						top.expectKey = true // 父对象的该值已完整消费
					}
				}
			}
		default:
			// 标量值：父对象回到期待 key 状态；父数组推进下标
			if len(stack) > 0 {
				if top := &stack[len(stack)-1]; top.isObj {
					top.expectKey = true
				} else {
					top.idx++
				}
			}
		}
	}
}

// renderPath 把段名序列渲染为 JSON 路径（数组段渲染为 [i]）。
func renderPath(segs []string) string {
	var b strings.Builder
	for i, s := range segs {
		if isArrayIndexSeg(s) {
			b.WriteString("[")
			b.WriteString(s)
			b.WriteString("]")
			continue
		}
		if i > 0 {
			b.WriteString(".")
		}
		b.WriteString(s)
	}
	return b.String()
}

// isArrayIndexSeg 报告段名是否为数组下标（纯十进制数字）。
func isArrayIndexSeg(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// classifyDecodeError 把阶段 2 的解码错误归类为带阶段的 ValidationError。
func classifyDecodeError(err error) *ValidationError {
	var synErr *json.SyntaxError
	if errors.As(err, &synErr) {
		return newErr("JSON语法", "", "偏移 %d 附近: %s", synErr.Offset, synErr.Error())
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return newErr("类型", typeErr.Field, "字段 %s 应为 %s，实际输入为 %s", typeErr.Field, typeErr.Type, typeErr.Value)
	}
	if errors.Is(err, io.EOF) {
		return newErr("JSON语法", "", "文档不完整")
	}
	const unknownFieldPrefix = "json: unknown field "
	if msg := err.Error(); strings.HasPrefix(msg, unknownFieldPrefix) {
		name := strings.Trim(strings.TrimPrefix(msg, unknownFieldPrefix), `"`)
		return newErr("未知字段", name, "未知核心字段 %q（扩展能力请放入节点的 metadata 或 extensions）", name)
	}
	return newErr("JSON语法", "", "%s", err.Error())
}

// isJSONScalar 报告 RawMessage 是否为 JSON 标量（null / bool / string / number）。
func isJSONScalar(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch v.(type) {
	case nil, bool, string, float64:
		return true
	}
	return false
}

// isJSONStringList 报告 RawMessage 是否为字符串数组（允许空数组）。
func isJSONStringList(raw json.RawMessage) bool {
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false
	}
	if arr == nil {
		return false // null 或非数组
	}
	for _, item := range arr {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

// sortedNodeIDs 返回排序后的节点 ID，保证校验错误位置确定。
func sortedNodeIDs(doc *GraphDocument) []string {
	return sortedNodeKeys(doc.Nodes)
}

// sortedNodeKeys 返回排序后的节点表键（内联子图无 GraphDocument 外壳时使用）。
func sortedNodeKeys(nodes map[string]Node) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
