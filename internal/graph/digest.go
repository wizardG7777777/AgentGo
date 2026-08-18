package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ComputeDigest 计算图的执行语义摘要（sha256，hex 编码）。
//
// 只覆盖会改变执行语义的字段：schema、graph_id、revision，以及每个节点的
// kind / task / capability / next / wait / tool / subgraph。status / executor /
// execution / state_version / metadata / extensions 一律不进入摘要——状态刷新
// 不得改变 digest，定义变化必须改变 digest。subgraph 的内联子图递归规范化。
//
// 序列化前先规范化：空值与缺省归一、Condition.Value 解码重编码（消除字面量
// 格式差异）、map key 排序由 encoding/json 保证；同一语义文档的 digest 与
// JSON key 顺序及 nodes 表遍历顺序无关。
func ComputeDigest(doc *GraphDocument) string {
	if doc == nil {
		return ""
	}
	canonical := canonicalDoc{
		Schema:   doc.Schema,
		GraphID:  doc.GraphID,
		Revision: doc.Revision,
		Nodes:    make(map[string]canonicalNode, len(doc.Nodes)),
	}
	for id, node := range doc.Nodes {
		canonical.Nodes[id] = canonicalizeNode(node)
	}
	return hashCanonical(canonical)
}

// hashCanonical 对只含 JSON 可序列化字段的内部规范结构求 sha256。调用方
// 必须先决定哪些字节属于权威语义；编码失败仅可能来自编程错误，返回空串让
// 上层完整性校验 fail-closed。
func hashCanonical(value any) string {
	buf, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

type activationSequenceDigestEntry struct {
	NodeID string `json:"node_id"`
	Seq    int    `json:"seq"`
}

// snapshotIntegrityInput 是 snapshot v2 的完整性权威：不仅覆盖定义摘要，
// 还覆盖完整运行态 Doc、边选择、activation Result/Evidence 与序号表。
type snapshotIntegrityInput struct {
	Domain            string                          `json:"domain"`
	Version           int                             `json:"version"`
	GraphID           string                          `json:"graph_id"`
	Seq               int64                           `json:"seq"`
	Revision          int64                           `json:"revision"`
	StateVersion      int64                           `json:"state_version"`
	Digest            string                          `json:"digest"`
	ChainDigest       string                          `json:"chain_digest"`
	Doc               *GraphDocument                  `json:"doc"`
	Transitions       []TransitionRecord              `json:"transitions"`
	ActivationResults []ActivationResult              `json:"activation_results"`
	ActivationSeq     []activationSequenceDigestEntry `json:"activation_seq"`
}

func computeSnapshotIntegrityDigest(snap *snapshotFile) string {
	if snap == nil {
		return ""
	}
	seq := make([]activationSequenceDigestEntry, 0, len(snap.ActivationSeq))
	for nodeID, n := range snap.ActivationSeq {
		seq = append(seq, activationSequenceDigestEntry{NodeID: nodeID, Seq: n})
	}
	sort.Slice(seq, func(i, j int) bool { return seq[i].NodeID < seq[j].NodeID })
	transitions := append([]TransitionRecord(nil), snap.Transitions...)
	sort.Slice(transitions, func(i, j int) bool {
		a, b := transitions[i], transitions[j]
		if a.SourceActivationID != b.SourceActivationID {
			return a.SourceActivationID < b.SourceActivationID
		}
		if a.TransitionID != b.TransitionID {
			return a.TransitionID < b.TransitionID
		}
		return a.TargetNodeID < b.TargetNodeID
	})
	results := append([]ActivationResult(nil), snap.ActivationResults...)
	sort.Slice(results, func(i, j int) bool { return results[i].Ref < results[j].Ref })
	return hashCanonical(snapshotIntegrityInput{
		Domain:            "agentgo.graph.snapshot/v2",
		Version:           snap.Version,
		GraphID:           snap.GraphID,
		Seq:               snap.Seq,
		Revision:          snap.Revision,
		StateVersion:      snap.StateVersion,
		Digest:            snap.Digest,
		ChainDigest:       snap.ChainDigest,
		Doc:               snap.Doc,
		Transitions:       transitions,
		ActivationResults: results,
		ActivationSeq:     seq,
	})
}

// canonicalDoc 是 digest 的规范化输入：仅执行语义字段。
type canonicalDoc struct {
	Schema   string                   `json:"schema"`
	GraphID  string                   `json:"graph_id"`
	Revision int64                    `json:"revision"`
	Nodes    map[string]canonicalNode `json:"nodes"`
}

// canonicalNode 只保留定义字段；status/executor/execution/metadata/extensions 被丢弃。
type canonicalNode struct {
	Kind       NodeKind              `json:"kind"`
	Task       *NodeTask             `json:"task,omitempty"`
	Capability *canonicalCapability  `json:"capability,omitempty"`
	Next       []canonicalTransition `json:"next"`
	Wait       *canonicalWait        `json:"wait,omitempty"`
	Tool       *canonicalTool        `json:"tool,omitempty"`
	Subgraph   *canonicalSubgraph    `json:"subgraph,omitempty"`
}

// canonicalWait 与 WaitSpec 同形（event 必填序列化，timeout_sec 空值归一）。
type canonicalWait struct {
	Event      string `json:"event"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

// canonicalTool 与 ToolSpec 同形；Args 的 map key 序由 encoding/json 归一。
type canonicalTool struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// canonicalSubgraph 与 SubgraphSpec 同形，节点递归规范化。
type canonicalSubgraph struct {
	Root  string                   `json:"root"`
	Nodes map[string]canonicalNode `json:"nodes"`
}

// canonicalCapability 与 Capability 同形，omitempty 把空值与缺省归一。
type canonicalCapability struct {
	Tools     []string `json:"tools,omitempty"`
	Model     string   `json:"model,omitempty"`
	Isolation string   `json:"isolation,omitempty"`
}

// canonicalTransition 与 Transition 同形，Value 经解码重编码规范化。
type canonicalTransition struct {
	To          string              `json:"to"`
	Activation  string              `json:"activation,omitempty"`
	TargetInput string              `json:"target_input,omitempty"`
	When        *canonicalCondition `json:"when,omitempty"`
}

// canonicalCondition 的 omitempty 保证事件形态与条件形态序列化互不污染。
type canonicalCondition struct {
	Event    string          `json:"event,omitempty"`
	Path     string          `json:"path,omitempty"`
	Operator string          `json:"operator,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

// canonicalizeNode 把节点归一到 digest 输入形态。
func canonicalizeNode(node Node) canonicalNode {
	cn := canonicalNode{Kind: node.Kind}

	// 全空的 task 与缺省等价，归一为 nil
	if node.Task != nil && (node.Task.Title != "" || node.Task.Description != "" ||
		len(node.Task.RequiredInputs) > 0 || len(node.Task.RequiredEvidence) > 0) {
		task := *node.Task
		task.RequiredInputs = append([]string(nil), node.Task.RequiredInputs...)
		task.RequiredEvidence = append([]EvidenceRequirement(nil), node.Task.RequiredEvidence...)
		cn.Task = &task
	}

	// 全空的 capability 与缺省等价，归一为 nil
	if c := node.Capability; c != nil &&
		(len(c.Tools) > 0 || c.Model != "" || c.Isolation != "") {
		cc := &canonicalCapability{
			Model:     c.Model,
			Isolation: c.Isolation,
		}
		if len(c.Tools) > 0 {
			cc.Tools = append([]string(nil), c.Tools...)
		}
		cn.Capability = cc
	}

	// nil 与空切片归一为 []（next 是必序列化字段）
	cn.Next = make([]canonicalTransition, 0, len(node.Next))
	for _, tr := range node.Next {
		ct := canonicalTransition{To: tr.To, Activation: tr.Activation, TargetInput: tr.TargetInput}
		if tr.When != nil {
			ct.When = &canonicalCondition{
				Event:    tr.When.Event,
				Path:     tr.When.Path,
				Operator: tr.When.Operator,
				Value:    normalizeRaw(tr.When.Value),
			}
		}
		cn.Next = append(cn.Next, ct)
	}

	// 类型专属规格按原样纳入（event/name 等已在校验链保证非空）
	if node.Wait != nil {
		cn.Wait = &canonicalWait{Event: node.Wait.Event, TimeoutSec: node.Wait.TimeoutSec}
	}
	if node.Tool != nil {
		cn.Tool = &canonicalTool{Name: node.Tool.Name, Args: node.Tool.Args}
	}
	if node.Subgraph != nil {
		cs := &canonicalSubgraph{
			Root:  node.Subgraph.Root,
			Nodes: make(map[string]canonicalNode, len(node.Subgraph.Nodes)),
		}
		for id, sub := range node.Subgraph.Nodes {
			cs.Nodes[id] = canonicalizeNode(sub)
		}
		cn.Subgraph = cs
	}
	return cn
}

// normalizeRaw 解码重编码一个 JSON 字面量，消除空白与数字格式差异；
// 无法解析时原样返回（未校验文档的兜底，map key 排序仍由重编码保证）。
func normalizeRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return buf
}
