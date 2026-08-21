package graph

// 本文件实现终态契约 v2（docs/design/graph-terminal-contract-v2.md §5）的
// 输出契约机械派生与钉入载体：
//
//   - 派生：deriveOutputContract 从任务型节点（agent/controller/acceptance）
//     冻结出边的 path 条件机械反推逐字段契约——同一字段的全部 eq 值与 in
//     元素并入值域（合并优先级 eq > in > exists > ne，与首击诊断
//     outletResultExample 一致）；算子值域语义与 outletFieldDomains 共用
//     conditionDomainText 一处实现，不抄第二份；
//   - 注入：RenderOutputContract 把契约渲染为 <output-contract> 定界块，
//     由 Runtime 随 TaskSpec 交给任务桥追加到任务描述尾部；无 path 条件
//     出边（纯系统事件边/无条件边）返回空串，调用方不注入；
//   - 钉入：ExtractOutputContract 供 TaskMemory 严格按定界标记解析任务
//     描述中的契约块并逐行落入 Constraints（v1 图与非图任务无此块自然
//     跳过），不做子串猜测。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 输出契约定界标记。任务桥注入与 TaskMemory 钉入共用同一对常量；
// 解析严格按标记定界，不做子串猜测。
const (
	OutputContractBegin = "<output-contract>"
	OutputContractEnd   = "</output-contract>"
)

// outputContractField 是单字段的派生输出契约：Path 为 result 内字段路径
// （保留 $. 前缀）；Values 非空表示字段必须存在且取值落在值域内（eq/in
// 合并，渲染后文本、去重、保持首见序）；无值域时按 exists / ne 降级。
type outputContractField struct {
	Path     string
	Values   []string
	Exists   bool
	NeValues []string
}

// deriveOutputContract 从出边 path 条件机械派生逐字段输出契约（终态契约
// v2 §5），是「出边 path 条件 → 字段值域」的唯一派生实现。合并规则：
// 同一字段的全部 eq 值与 in 元素按出边首见序并入值域（eq 先于 in）；
// 该字段无 eq/in 值域时降级 exists，再次 ne。事件边与无条件边不参与
// 派生。返回按字段路径排序，保证渲染与钉入逐字节稳定。
func deriveOutputContract(next []Transition) []outputContractField {
	byPath := make(map[string]*outputContractField)
	field := func(path string) *outputContractField {
		f, ok := byPath[path]
		if !ok {
			f = &outputContractField{Path: path}
			byPath[path] = f
		}
		return f
	}
	appendUnique := func(dst *[]string, values ...string) {
		for _, v := range values {
			dup := false
			for _, existing := range *dst {
				if existing == v {
					dup = true
					break
				}
			}
			if !dup {
				*dst = append(*dst, v)
			}
		}
	}
	// 两遍扫描实现 eq > in 优先级：先收全部 eq 值，再收 in 元素。
	for _, tr := range next {
		if tr.When == nil || tr.When.Path == "" || tr.When.Operator != OpEq {
			continue
		}
		f := field(tr.When.Path)
		appendUnique(&f.Values, contractValueText(tr.When.Value))
	}
	for _, tr := range next {
		when := tr.When
		if when == nil || when.Path == "" {
			continue
		}
		f := field(when.Path)
		switch when.Operator {
		case OpIn:
			appendUnique(&f.Values, contractInValues(when.Value)...)
		case OpExists:
			f.Exists = true
		case OpNe:
			appendUnique(&f.NeValues, contractValueText(when.Value))
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]outputContractField, 0, len(paths))
	for _, path := range paths {
		out = append(out, *byPath[path])
	}
	return out
}

// RenderOutputContract 把派生输出契约渲染为 <output-contract> 定界块
// （终态契约 v2 §5 注入形态，块内按字段逐行列出）。无 path 条件出边
// （纯系统事件边/无条件边）返回空串——调用方不注入。
func RenderOutputContract(next []Transition) string {
	fields := deriveOutputContract(next)
	if len(fields) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(OutputContractBegin)
	b.WriteString("\n本节点 result 字段契约（终态契约 v2，自出边 path 条件机械派生，逐字段列出）：")
	for _, f := range fields {
		b.WriteString("\n" + f.domainText())
	}
	b.WriteString("\nsummary 由系统参数承载，不在 result 内重复；禁止提交 event。")
	b.WriteString("\n" + OutputContractEnd)
	return b.String()
}

// domainText 渲染单字段契约行。值域形态取设计 §5 示例（coverage ∈
// {gap, ok}）：JSON 字符串去引号、其余标量保留原文；exists 语义与
// conditionDomainText 逐字一致（共用同一实现），ne 取同一「必须不等于
// …（或字段缺失）」语义的值域集合形态。
func (f outputContractField) domainText() string {
	path := strings.TrimPrefix(f.Path, "$.")
	switch {
	case len(f.Values) > 0:
		return fmt.Sprintf("%s ∈ {%s}", path, strings.Join(f.Values, ", "))
	case f.Exists:
		return path + " " + conditionDomainText(&Condition{Operator: OpExists})
	default:
		return fmt.Sprintf("%s 必须不等于 {%s}（或字段缺失）", path, strings.Join(f.NeValues, ", "))
	}
}

// contractValueText 渲染契约值域中的单个值：JSON 字符串去引号（设计示例
// coverage ∈ {gap, ok} 的形态），其余标量保留 JSON 原文。
func contractValueText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// contractInValues 解析 in 条件值（建图校验已限定为字符串列表）；解析
// 失败整体返回 nil——契约是给 Agent 的提示，求值权威仍是 evalCondition。
func contractInValues(raw json.RawMessage) []string {
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// ExtractOutputContract 严格按定界标记解析任务描述中的 <output-contract>
// 块，返回块内逐行内容（去首尾空白、跳过空行）；无 Begin 标记或块不完整
// （有 Begin 无 End）返回 nil——不做子串猜测。供 TaskMemory 钉入
// （终态契约 v2 §5）。
func ExtractOutputContract(description string) []string {
	begin := strings.Index(description, OutputContractBegin)
	if begin < 0 {
		return nil
	}
	rest := description[begin+len(OutputContractBegin):]
	end := strings.Index(rest, OutputContractEnd)
	if end < 0 {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(rest[:end], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
