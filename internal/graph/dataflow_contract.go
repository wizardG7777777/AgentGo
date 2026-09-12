package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
)

const (
	DataflowSchema        = "agentgo.graph/v7"
	AgentTaskKind         = "agentTask"
	AgentTaskResultSchema = "agentgo.agent-task-result/v2"
	CompletionSchema      = "agentgo.graph-completion/v1"
)

// DataflowDefinition 仅描述当前已知工作；没有 root、next、条件出口和结束节点。
type DataflowDefinition struct {
	Schema      string                       `json:"schema"`
	GraphID     string                       `json:"graph_id"`
	SessionID   string                       `json:"session_id"`
	RunID       string                       `json:"run_id"`
	Revision    int64                        `json:"revision"`
	Objective   string                       `json:"objective"`
	Constraints []string                     `json:"constraints,omitempty"`
	Inputs      map[string]DataflowInputSpec `json:"inputs,omitempty"`
	Nodes       []AgentTaskNode              `json:"nodes"`
}

type DataflowInputSpec struct {
	Schema map[string]any `json:"schema"`
}

// AgentTaskNode 是一次工作实例；执行重试使用 Attempt，返工须建立新的 NodeID。
type AgentTaskNode struct {
	NodeID       string                         `json:"node_id"`
	Kind         string                         `json:"kind"`
	Title        string                         `json:"title"`
	Objective    string                         `json:"objective"`
	Inputs       map[string]DataflowInputSource `json:"inputs,omitempty"`
	ResultSchema map[string]any                 `json:"result_schema"`
	Execution    AgentTaskExecutionSpec         `json:"execution"`
	Labels       map[string]string              `json:"labels,omitempty"`
}

type AgentTaskExecutionSpec struct {
	RouteRef string   `json:"route_ref"`
	Tools    []string `json:"tools"`
	Model    string   `json:"model,omitempty"`
}

// DataflowInputSource 只能绑定一个来源；输出字段按 JSON Pointer 选取。
type DataflowInputSource struct {
	Kind    string `json:"kind"` // graph_input 或 node_result
	NodeID  string `json:"node_id,omitempty"`
	Port    string `json:"port,omitempty"`
	Version int64  `json:"version,omitempty"`
	Pointer string `json:"pointer,omitempty"`
}

// AgentTaskResult 是完整结果权威。框架身份及候选不得取自模型结果字段。
type AgentTaskResult struct {
	PlainText    bool            `json:"plain_text,omitempty"`
	Evidence     []EvidenceEntry `json:"evidence,omitempty"`
	Schema       string          `json:"schema"`
	Ref          string          `json:"ref"`
	RunID        string          `json:"run_id"`
	GraphID      string          `json:"graph_id"`
	NodeID       string          `json:"node_id"`
	TaskID       string          `json:"task_id"`
	ActivationID string          `json:"activation_id"`
	AttemptID    string          `json:"attempt_id"`
	Value        map[string]any  `json:"value"`
	CandidateRef string          `json:"candidate_ref,omitempty"`
	EvidenceRefs []string        `json:"evidence_refs,omitempty"`
}

func decodeDataflowJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("数据流契约解码失败: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("数据流契约包含多余 JSON 内容")
	}
	return nil
}

func DecodeDataflowDefinition(data []byte) (DataflowDefinition, error) {
	var def DataflowDefinition
	if err := decodeDataflowJSON(data, &def); err != nil {
		return def, err
	}
	return def, ValidateDataflowDefinition(def)
}

func dataflowIdentity(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 512 {
		return fmt.Errorf("%s 身份为空、过长或含首尾空白", name)
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return fmt.Errorf("%s 身份含控制字符", name)
		}
	}
	return nil
}

func ValidateDataflowDefinition(def DataflowDefinition) error {
	if def.Schema != DataflowSchema {
		return fmt.Errorf("拒绝旧图契约 %q，仅接受 %s", def.Schema, DataflowSchema)
	}
	for _, item := range []struct{ name, value string }{{"graph_id", def.GraphID}, {"session_id", def.SessionID}, {"run_id", def.RunID}} {
		if err := dataflowIdentity(item.name, item.value); err != nil {
			return err
		}
	}
	if def.Revision < 1 || strings.TrimSpace(def.Objective) == "" {
		return fmt.Errorf("图必须声明正 revision 与目标")
	}
	for _, port := range sortedDataflowKeys(def.Inputs) {
		if err := dataflowIdentity("图输入", port); err != nil {
			return err
		}
		if err := validateDataflowValueSchema(def.Inputs[port].Schema); err != nil {
			return fmt.Errorf("图输入 %s: %w", port, err)
		}
	}
	nodes := make(map[string]AgentTaskNode, len(def.Nodes))
	for _, node := range def.Nodes {
		if err := dataflowIdentity("node_id", node.NodeID); err != nil {
			return err
		}
		if _, exists := nodes[node.NodeID]; exists {
			return fmt.Errorf("重复 node_id=%s", node.NodeID)
		}
		if node.Kind != AgentTaskKind {
			return fmt.Errorf("节点 %s 类型 %q 已退役，仅接受 agentTask", node.NodeID, node.Kind)
		}
		if strings.TrimSpace(node.Title) == "" || strings.TrimSpace(node.Objective) == "" {
			return fmt.Errorf("节点 %s 缺少确定任务", node.NodeID)
		}
		if err := dataflowIdentity("route_ref", node.Execution.RouteRef); err != nil {
			return fmt.Errorf("节点 %s: %w", node.NodeID, err)
		}
		seenTools := map[string]bool{}
		for _, name := range node.Execution.Tools {
			if err := dataflowIdentity("tool", name); err != nil {
				return err
			}
			if seenTools[name] {
				return fmt.Errorf("节点 %s 重复工具 %s", node.NodeID, name)
			}
			seenTools[name] = true
		}
		if err := validateDataflowValueSchema(node.ResultSchema); err != nil {
			return fmt.Errorf("节点 %s result_schema: %w", node.NodeID, err)
		}
		if node.ResultSchema["type"] != "object" {
			return fmt.Errorf("节点 %s 的结果必须是 object", node.NodeID)
		}
		nodes[node.NodeID] = node
	}
	for _, id := range sortedDataflowKeys(nodes) {
		for _, slot := range sortedDataflowKeys(nodes[id].Inputs) {
			if err := dataflowIdentity("输入槽", slot); err != nil {
				return err
			}
			src := nodes[id].Inputs[slot]
			switch src.Kind {
			case "node_result", "node_outcome":
				if src.Port != "" || src.Version != 0 {
					return fmt.Errorf("节点 %s 输入 %s 混合来源字段", id, slot)
				}
				if _, ok := nodes[src.NodeID]; !ok {
					return fmt.Errorf("节点 %s 输入 %s 引用不存在节点 %q", id, slot, src.NodeID)
				}
			case "graph_input":
				if src.NodeID != "" || src.Version < 1 {
					return fmt.Errorf("节点 %s 图输入必须绑定明确版本", id)
				}
				if _, ok := def.Inputs[src.Port]; !ok {
					return fmt.Errorf("节点 %s 引用未声明图输入 %q", id, src.Port)
				}
			default:
				return fmt.Errorf("节点 %s 未知输入来源 %q", id, src.Kind)
			}
			if _, err := dataflowPointerTokens(src.Pointer); err != nil {
				return fmt.Errorf("节点 %s 输入 %s: %w", id, slot, err)
			}
		}
	}
	// 每个 revision 无环；通过追加新工作实例表达任意多轮迭代。
	colors := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if colors[id] == 1 {
			return fmt.Errorf("数据依赖成环，涉及节点 %s；返工请追加新实例", id)
		}
		if colors[id] == 2 {
			return nil
		}
		colors[id] = 1
		for _, slot := range sortedDataflowKeys(nodes[id].Inputs) {
			src := nodes[id].Inputs[slot]
			if src.Kind == "node_result" || src.Kind == "node_outcome" {
				if err := visit(src.NodeID); err != nil {
					return err
				}
			}
		}
		colors[id] = 2
		return nil
	}
	for _, id := range sortedDataflowKeys(nodes) {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func sortedDataflowKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dataflowDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (def DataflowDefinition) Digest() (string, error) {
	if err := ValidateDataflowDefinition(def); err != nil {
		return "", err
	}
	return dataflowDigest(def)
}

func cloneDataflow[T any](value T) (T, error) {
	var out T
	raw, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	err = decodeDataflowJSON(raw, &out)
	return out, err
}

func ValidateGraphID(id string) error {
	if err := dataflowIdentity("graph_id", id); err != nil {
		return err
	}
	if strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return fmt.Errorf("图 ID 不允许路径分隔符")
	}
	return nil
}
